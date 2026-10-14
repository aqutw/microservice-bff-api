package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Creation keys retain their original names so pre-upgrade unknown outcomes replay safely.
func checkoutOperationKey(job checkoutJob, path string) string {
	operation := strings.TrimPrefix(path, "/v1/")
	if path == "/v1/reservations" {
		operation = "reserve"
	}
	if path == "/v1/payments" {
		operation = "payment"
	}
	if strings.HasSuffix(path, "/commit") {
		operation = "inventory_commit"
	}
	if strings.HasSuffix(path, "/release") {
		operation = "compensate"
	}
	if strings.HasSuffix(path, "/cancel") {
		operation = "payment_cancel"
	}
	if strings.HasSuffix(path, "/refund") {
		operation = "payment_refund"
	}
	return job.ID + ":" + operation
}

type paymentView struct {
	ID       string `json:"id"`
	OrderID  string `json:"order_id"`
	Status   string `json:"status"`
	Amount   int64  `json:"amount_cents"`
	Currency string `json:"currency"`
}

func paymentID(job checkoutJob) string {
	var reference struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(job.Result.Payment, &reference)
	return reference.ID
}

func lookupPayment(job checkoutJob) (paymentView, error) {
	var payment paymentView
	id := paymentID(job)
	if id == "" {
		return payment, fmt.Errorf("payment_reference_missing")
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(env("PAYMENT_API_URL", "https://localhost:8083"), "/")+"/v1/payments/"+id, nil)
	if err != nil {
		return payment, err
	}
	req.Header.Set("X-Actor-ID", job.Actor)
	req.Header.Set("X-Correlation-ID", job.Correlation)
	response, err := serviceAuth.Do(&http.Client{Timeout: 3 * time.Second}, "payment", req)
	if err != nil {
		return payment, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return payment, fmt.Errorf("payment_lookup_status_%d", response.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&payment); err != nil {
		return payment, err
	}
	if payment.ID != id || payment.OrderID != job.Result.Order.ID || payment.Amount != job.Input.TotalCents || payment.Currency != job.Input.Currency {
		return payment, fmt.Errorf("payment_order_mismatch")
	}
	return payment, nil
}

func snapshotStatus(raw json.RawMessage, status string) (json.RawMessage, error) {
	var snapshot map[string]any
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, fmt.Errorf("snapshot_missing")
	}
	snapshot["status"] = status
	return json.Marshal(snapshot)
}

// Every worker write is fenced by its claimed lease. Business data, snapshots and audit share a transaction.
func saveJobTx(tx *sql.Tx, job *checkoutJob, stage string) error {
	raw, err := json.Marshal(job.Result)
	if err != nil {
		return err
	}
	result, err := tx.Exec("UPDATE checkouts SET stage=?,result_json=?,error_status=?,error_message=?,attempts=0,next_attempt=0 WHERE id=? AND lease_until=?", stage, string(raw), job.ErrorStatus, job.ErrorMessage, job.ID, job.Lease)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("checkout_lease_lost")
	}
	return nil
}

func orderTransitionTx(tx *sql.Tx, job checkoutJob, from, to string, audit bool) error {
	result, err := tx.Exec("UPDATE orders SET status=? WHERE id=? AND status=?", to, job.Result.Order.ID, from)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		var current string
		if err = tx.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&current); err != nil {
			return err
		}
		if current != to {
			return fmt.Errorf("order_transition_conflict_%s", current)
		}
		return nil
	}
	if !audit {
		return nil
	}
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Actor-ID", job.Actor)
	r.Header.Set("X-Correlation-ID", job.Correlation)
	return recordAuditTx(tx, r, "order.status_changed", "order", job.Result.Order.ID, "update_status", "success", map[string]any{"status": from}, map[string]any{"status": to})
}

func reconcilePayment(job *checkoutJob) error {
	payment, err := lookupPayment(*job)
	if err != nil {
		return err
	}
	if payment.Status == "pending" {
		_, err = db.Exec("UPDATE checkouts SET next_attempt=? WHERE id=? AND lease_until=?", time.Now().Add(5*time.Second).UnixMilli(), job.ID, job.Lease)
		return err
	}
	if payment.Status != "paid" {
		return fmt.Errorf("payment_not_paid_%s", payment.Status)
	}
	if reservationID(job.Result) == "" {
		return fmt.Errorf("reservation_reference_missing")
	}
	job.Result.Payment, err = snapshotStatus(job.Result.Payment, "paid")
	if err != nil {
		return err
	}
	job.Result.State = "settling"
	job.Result.Order.Status = "settling"
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = orderTransitionTx(tx, *job, "pending_payment", "settling", false); err != nil {
		return err
	}
	if err = saveJobTx(tx, job, "settling"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	job.Stage = "settling"
	return nil
}

func settleInventory(job *checkoutJob) error {
	err := commitInventoryReservation(*job, reservationID(job.Result))
	if err != nil {
		var failure upstreamFailure
		if !errors.As(err, &failure) || failure.status != 409 || failure.message != "reservation_not_active" {
			return err
		}
		job.ErrorStatus, job.ErrorMessage = 409, "reservation_expired"
		job.Result.State, job.Result.Order.Status = "refund_pending", "refund_pending"
		tx, beginErr := db.Begin()
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()
		if err = orderTransitionTx(tx, *job, "settling", "refund_pending", false); err != nil {
			return err
		}
		if err = saveJobTx(tx, job, "refund_pending"); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		job.Stage = "refund_pending"
		return nil
	}
	job.Result.Inventory, err = snapshotStatus(job.Result.Inventory, "committed")
	if err != nil {
		return err
	}
	job.Result.State, job.Result.Order.Status = "paid", "paid"
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = orderTransitionTx(tx, *job, "settling", "paid", true); err != nil {
		return err
	}
	if err = saveJobTx(tx, job, "paid"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	job.Stage = "paid"
	return nil
}

func refundCheckout(job *checkoutJob) error {
	payment, err := lookupPayment(*job)
	if err != nil {
		return err
	}
	if payment.Status != "refunded" {
		if payment.Status != "paid" {
			return fmt.Errorf("payment_cannot_refund_%s", payment.Status)
		}
		body, callErr := callCheckout(*job, env("PAYMENT_API_URL", "https://localhost:8083"), "/v1/payments/"+payment.ID+"/refund", nil)
		if callErr != nil {
			return callErr
		}
		var result paymentView
		if json.Unmarshal(body, &result) != nil || result.ID != payment.ID || result.OrderID != job.Result.Order.ID || result.Status != "refunded" {
			return fmt.Errorf("invalid_refund_response")
		}
	}
	job.Result.Payment, err = snapshotStatus(job.Result.Payment, "refunded")
	if err != nil {
		return err
	}
	if err = releaseInventoryReservation(*job, reservationID(job.Result)); err != nil {
		return err
	}
	return completeCancellation(job)
}

func completeCancellation(job *checkoutJob) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	if err = tx.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&current); err != nil {
		return err
	}
	if current != "pending_payment" && current != "refund_pending" && current != "cancelling" && current != "cancelled" {
		return fmt.Errorf("order_cannot_cancel_%s", current)
	}
	if err = orderTransitionTx(tx, *job, current, "cancelled", current != "cancelled"); err != nil {
		return err
	}
	job.Result.State, job.Result.Order.Status = "failed", "cancelled"
	if reservationID(job.Result) != "" {
		job.Result.Inventory, err = snapshotStatus(job.Result.Inventory, "released")
		if err != nil {
			return err
		}
	}
	if job.ErrorStatus < 400 || job.ErrorStatus > 599 {
		job.ErrorStatus, job.ErrorMessage = 409, "order_cancelled"
	}
	if err = saveJobTx(tx, job, "failed"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	job.Stage = "failed"
	return nil
}

// The HTTP path and recovery both advance the same persisted workflow.
func settleOrder(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		write(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	var actor, checkoutID string
	if db.QueryRow("SELECT user_id FROM orders WHERE id=?", id).Scan(&actor) != nil || actor != r.Header.Get("X-Actor-ID") {
		write(w, 404, map[string]string{"error": "order_not_found"})
		return
	}
	if err := db.QueryRow("SELECT id FROM checkouts WHERE order_id=?", id).Scan(&checkoutID); err != nil {
		write(w, 409, map[string]string{"error": "checkout_required"})
		return
	}
	if _, err := db.Exec("UPDATE checkouts SET next_attempt=0 WHERE id=? AND stage IN ('awaiting_payment','settling','refund_pending')", checkoutID); err != nil {
		write(w, 500, map[string]string{"error": "settlement_unavailable"})
		return
	}
	processCheckout(checkoutID)
	job, err := loadCheckout(checkoutID)
	if err != nil {
		write(w, 500, map[string]string{"error": "settlement_unavailable"})
		return
	}
	if job.Stage == "failed" {
		writeCheckout(w, checkoutID)
		return
	}
	status := 202
	if job.Stage == "paid" {
		status = 200
	}
	write(w, status, map[string]string{"checkout_id": checkoutID, "state": job.Result.State, "order_status": job.Result.Order.Status})
}
