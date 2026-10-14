package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type checkoutRequest struct {
	UserID     string `json:"user_id"`
	Items      []Item `json:"items"`
	TotalCents int64  `json:"total_cents"`
	Currency   string `json:"currency"`
	Channel    string `json:"channel"`
}
type checkoutResult struct {
	CheckoutID string          `json:"checkout_id"`
	State      string          `json:"state"`
	Order      order           `json:"order"`
	Inventory  json.RawMessage `json:"inventory,omitempty"`
	Payment    json.RawMessage `json:"payment,omitempty"`
}
type checkoutJob struct {
	ID, Stage, Actor, Correlation string
	Input                         checkoutRequest
	Result                        checkoutResult
	ErrorStatus                   int
	ErrorMessage                  string
	Attempts                      int
	Lease                         int64
	CancelRequested               bool
}
type upstreamFailure struct {
	status  int
	message string
}

func (e upstreamFailure) Error() string { return e.message }

func initCheckouts() error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS checkouts (
 id TEXT PRIMARY KEY, request_json TEXT NOT NULL, stage TEXT NOT NULL,
 result_json TEXT NOT NULL, actor_id TEXT NOT NULL, correlation_id TEXT NOT NULL,
 error_status INTEGER NOT NULL DEFAULT 0, error_message TEXT NOT NULL DEFAULT '',
 attempts INTEGER NOT NULL DEFAULT 0, next_attempt INTEGER NOT NULL DEFAULT 0,
 lease_until INTEGER NOT NULL DEFAULT 0);
 CREATE INDEX IF NOT EXISTS checkouts_recovery ON checkouts(stage,next_attempt,lease_until);`)
	if err != nil {
		return err
	}
	if _, err = db.Exec("ALTER TABLE checkouts ADD COLUMN order_id TEXT NOT NULL DEFAULT ''"); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	if _, err = db.Exec("ALTER TABLE checkouts ADD COLUMN cancel_requested INTEGER NOT NULL DEFAULT 0"); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	_, err = db.Exec(`UPDATE checkouts SET order_id=json_extract(result_json,'$.order.id') WHERE order_id='';
CREATE UNIQUE INDEX IF NOT EXISTS checkouts_order ON checkouts(order_id) WHERE order_id<>'';`)
	if err != nil {
		return err
	}
	return recoverLegacySettlements()
}

func checkouts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		write(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	var input checkoutRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.UserID == "" || len(input.Items) == 0 || input.TotalCents < 0 || (input.Channel != "web" && input.Channel != "app") {
		write(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	if input.UserID != r.Header.Get("X-Actor-ID") {
		write(w, 403, map[string]string{"error": "owner_mismatch"})
		return
	}
	for _, item := range input.Items {
		if item.SKU == "" || item.Quantity < 1 {
			write(w, 400, map[string]string{"error": "invalid_items"})
			return
		}
	}
	if input.Currency == "" {
		input.Currency = "TWD"
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 200 {
		write(w, 400, map[string]string{"error": "invalid_idempotency_key"})
		return
	}
	if key == "" {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			write(w, 500, map[string]string{"error": "key_generation_failed"})
			return
		}
		key = hex.EncodeToString(random[:])
	}
	namespace, _ := json.Marshal([]string{input.UserID, input.Channel, key})
	digest := sha256.Sum256(namespace)
	id := "chk_" + hex.EncodeToString(digest[:])
	raw, _ := json.Marshal(input)
	tx, err := db.Begin()
	if err != nil {
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// Insert first to acquire SQLite's writer lock before checking the request.
	now := time.Now().UTC()
	result := checkoutResult{CheckoutID: id, State: "processing", Order: order{ID: "ord_" + hex.EncodeToString(digest[:]), UserID: input.UserID, Items: input.Items, TotalCents: input.TotalCents, Currency: input.Currency, Status: "pending_payment", CreatedAt: now.Format(time.RFC3339Nano)}}
	snapshot, _ := json.Marshal(result)
	inserted, err := tx.Exec("INSERT INTO checkouts(id,request_json,stage,result_json,actor_id,correlation_id,order_id) VALUES(?,?,'reserve',?,?,?,?) ON CONFLICT(id) DO NOTHING", id, string(raw), string(snapshot), r.Header.Get("X-Actor-ID"), r.Header.Get("X-Correlation-ID"), result.Order.ID)
	if err != nil {
		_ = tx.Rollback()
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	var stored string
	if err = tx.QueryRow("SELECT request_json FROM checkouts WHERE id=?", id).Scan(&stored); err != nil {
		_ = tx.Rollback()
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if stored != string(raw) {
		_ = tx.Rollback()
		write(w, 409, map[string]string{"error": "idempotency_conflict"})
		return
	}
	changed, _ := inserted.RowsAffected()
	if changed == 1 {
		items, _ := json.Marshal(input.Items)
		_, err = tx.Exec("INSERT INTO orders(id,user_id,items_json,total_cents,currency,status,created_at) VALUES(?,?,?,?,?,'pending_payment',?)", result.Order.ID, input.UserID, string(items), input.TotalCents, input.Currency, now)
		if err == nil {
			err = recordAuditTx(tx, r, "order.created", "order", result.Order.ID, "create", "success", nil, map[string]any{"status": "pending_payment", "total_cents": input.TotalCents, "currency": input.Currency})
		}
	}
	if err != nil {
		_ = tx.Rollback()
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err = tx.Commit(); err != nil {
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	processCheckout(id)
	writeCheckout(w, id)
}

func checkoutStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		write(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/checkouts/")
	job, err := loadCheckout(id)
	if err != nil || job.Input.UserID != r.Header.Get("X-Actor-ID") {
		write(w, 404, map[string]string{"error": "checkout_not_found"})
		return
	}
	writeCheckout(w, id)
}
func loadCheckout(id string) (checkoutJob, error) {
	job := checkoutJob{ID: id}
	var input, result string
	err := db.QueryRow("SELECT request_json,stage,result_json,actor_id,correlation_id,error_status,error_message,attempts,lease_until,cancel_requested FROM checkouts WHERE id=?", id).Scan(&input, &job.Stage, &result, &job.Actor, &job.Correlation, &job.ErrorStatus, &job.ErrorMessage, &job.Attempts, &job.Lease, &job.CancelRequested)
	if err != nil {
		return job, err
	}
	if err = json.Unmarshal([]byte(input), &job.Input); err != nil {
		return job, err
	}
	err = json.Unmarshal([]byte(result), &job.Result)
	return job, err
}
func writeCheckout(w http.ResponseWriter, id string) {
	job, err := loadCheckout(id)
	if err == sql.ErrNoRows {
		write(w, 404, map[string]string{"error": "checkout_not_found"})
		return
	}
	if err != nil {
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if job.Stage == "failed" {
		status := job.ErrorStatus
		if status < 400 || status > 599 {
			status = 500
		}
		write(w, status, map[string]any{"error": job.ErrorMessage, "checkout_id": id})
		return
	}
	if job.CancelRequested && job.Stage != "paid" && job.Stage != "refund_pending" {
		job.Result.State = "cancelling"
		job.Result.Order.Status = "cancelling"
		write(w, 202, job.Result)
		return
	}
	if job.Stage == "awaiting_payment" || job.Stage == "paid" {
		write(w, 201, job.Result)
		return
	}
	write(w, 202, job.Result)
}
func checkpoint(job *checkoutJob, stage string) error {
	raw, err := json.Marshal(job.Result)
	if err != nil {
		return err
	}
	changed, err := db.Exec("UPDATE checkouts SET stage=?,result_json=?,error_status=?,error_message=?,attempts=0,next_attempt=0 WHERE id=? AND lease_until=?", stage, string(raw), job.ErrorStatus, job.ErrorMessage, job.ID, job.Lease)
	if err == nil {
		n, _ := changed.RowsAffected()
		if n != 1 {
			return fmt.Errorf("checkout_lease_lost")
		}
	}
	if err == nil {
		job.Stage = stage
	}
	return err
}
func callCheckout(job checkoutJob, base, path string, payload any) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", checkoutOperationKey(job, path))
	req.Header.Set("X-Actor-ID", job.Actor)
	req.Header.Set("X-Correlation-ID", job.Correlation)
	target := "inventory"
	if base == env("PAYMENT_API_URL", "https://localhost:8083") {
		target = "payment"
	}
	response, err := serviceAuth.Do(&http.Client{Timeout: 3 * time.Second}, target, req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	if err != nil {
		return nil, err
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid_upstream_response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &failure)
		return nil, upstreamFailure{response.StatusCode, failure.Error}
	}
	return body, nil
}

func processCheckout(id string) {
	now := time.Now().UnixMilli()
	lease := now + 60000
	claim, err := db.Exec("UPDATE checkouts SET lease_until=? WHERE id=? AND stage IN ('reserve','payment','compensate','awaiting_payment','settling','refund_pending') AND lease_until<=? AND next_attempt<=?", lease, id, now, now)
	if err != nil {
		log.Printf("checkout claim: %v", err)
		return
	}
	changed, _ := claim.RowsAffected()
	if changed != 1 {
		return
	}
	defer func() {
		if _, err := db.Exec("UPDATE checkouts SET lease_until=0 WHERE id=? AND lease_until=?", id, lease); err != nil {
			log.Printf("checkout unlock: %v", err)
		}
	}()
	job, err := loadCheckout(id)
	if err != nil {
		return
	}
	job.Lease = lease
	for step := 0; step < 4; step++ {
		if job.Stage == "awaiting_payment" && job.CancelRequested {
			job.ErrorStatus, job.ErrorMessage = 409, "order_cancelled"
			if checkpoint(&job, "compensate") != nil {
				return
			}
			continue
		}
		switch job.Stage {
		case "reserve":
			job.Result.Inventory, err = callCheckout(job, env("INVENTORY_API_URL", "https://localhost:8082"), "/v1/reservations", map[string]any{"order_id": job.Result.Order.ID, "items": job.Input.Items})
			if err == nil {
				var reservation struct {
					ID string `json:"reservation_id"`
				}
				err = json.Unmarshal(job.Result.Inventory, &reservation)
				if err == nil && reservation.ID == "" {
					err = fmt.Errorf("invalid_reservation_response")
				}
			}
			if err == nil {
				_, err = db.Exec("UPDATE orders SET reservation_id=? WHERE id=?", reservationID(job.Result), job.Result.Order.ID)
			}
			if err == nil {
				stage := "payment"
				if cancellationRequested(&job) {
					stage = "compensate"
				}
				err = checkpoint(&job, stage)
			}
		case "payment":
			job.Result.Payment, err = callCheckout(job, env("PAYMENT_API_URL", "https://localhost:8083"), "/v1/payments", map[string]any{"order_id": job.Result.Order.ID, "amount_cents": job.Input.TotalCents, "currency": job.Input.Currency, "channel": job.Input.Channel})
			if err == nil {
				var payment struct {
					ID string `json:"id"`
				}
				err = json.Unmarshal(job.Result.Payment, &payment)
				if err == nil && payment.ID == "" {
					err = fmt.Errorf("invalid_payment_response")
				}
			}
			if err == nil {
				job.Result.State = "awaiting_payment"
				stage := "awaiting_payment"
				if cancellationRequested(&job) {
					stage = "compensate"
				}
				err = checkpoint(&job, stage)
				if err == nil && stage == "awaiting_payment" {
					return
				}
			}
		case "compensate":
			err = compensateCheckout(&job)
		case "awaiting_payment":
			err = reconcilePayment(&job)
			if err == nil && job.Stage == "awaiting_payment" {
				return
			}
		case "settling":
			err = settleInventory(&job)
		case "refund_pending":
			err = refundCheckout(&job)
		default:
			return
		}
		if err != nil {
			var failure upstreamFailure
			if (job.Stage == "reserve" || job.Stage == "payment") && errors.As(err, &failure) && (failure.status == 400 || failure.status == 409 || failure.status == 422) {
				job.ErrorStatus = failure.status
				job.ErrorMessage = failure.message
				// An explicit rejection has no committed side effect. Unknown outcomes
				// keep the current stage and replay its idempotent request instead.
				if checkpoint(&job, "compensate") == nil {
					continue
				}
			}
			delay := time.Second * time.Duration(1<<min(job.Attempts, 5))
			_, updateErr := db.Exec("UPDATE checkouts SET attempts=attempts+1,next_attempt=? WHERE id=? AND lease_until=?", time.Now().Add(delay).UnixMilli(), id, lease)
			log.Printf("checkout %s stage %s will retry: %v (checkpoint: %v)", id, job.Stage, err, updateErr)
			return
		}
	}
}

// reservationID extracts the reservation reference from a checkout result.
func reservationID(result checkoutResult) string {
	var reservation struct {
		ID string `json:"reservation_id"`
	}
	_ = json.Unmarshal(result.Inventory, &reservation)
	return reservation.ID
}

// releaseInventoryReservation releases a hold and tolerates a lost response by
// re-reading the reservation when the upstream reports it is no longer active.
func releaseInventoryReservation(job checkoutJob, id string) error {
	if id == "" {
		return nil
	}
	body, err := callCheckout(job, env("INVENTORY_API_URL", "https://localhost:8082"), "/v1/reservations/"+id+"/release", nil)
	if err != nil {
		var failure upstreamFailure
		if !errors.As(err, &failure) || failure.status != 409 {
			return err
		}
		status, getErr := inventoryReservationStatus(job, id)
		if getErr != nil {
			return getErr
		}
		if status != "released" && status != "expired" {
			return err
		}
		return nil
	}
	var released struct {
		ID      string `json:"reservation_id"`
		OrderID string `json:"order_id"`
		Status  string `json:"status"`
	}
	if json.Unmarshal(body, &released) != nil || released.ID != id || released.OrderID != job.Result.Order.ID || released.Status != "released" {
		return fmt.Errorf("invalid_reservation_release_response")
	}
	return nil
}

// commitInventoryReservation turns a hold into a permanent sale. It is
// idempotent so a lost response can be replayed until the reservation is
// observed as committed.
func commitInventoryReservation(job checkoutJob, id string) error {
	if id == "" {
		return fmt.Errorf("reservation_reference_missing")
	}
	body, err := callCheckout(job, env("INVENTORY_API_URL", "https://localhost:8082"), "/v1/reservations/"+id+"/commit", map[string]string{"order_id": job.Result.Order.ID})
	if err != nil {
		var failure upstreamFailure
		if !errors.As(err, &failure) || failure.status != 409 {
			return err
		}
		status, getErr := inventoryReservationStatus(job, id)
		if getErr != nil {
			return getErr
		}
		if status != "committed" {
			return err
		}
		return nil
	}
	var committed struct {
		ID      string `json:"reservation_id"`
		OrderID string `json:"order_id"`
		Status  string `json:"status"`
	}
	if json.Unmarshal(body, &committed) != nil || committed.ID != id || committed.OrderID != job.Result.Order.ID || committed.Status != "committed" {
		return fmt.Errorf("invalid_reservation_commit_response")
	}
	return nil
}

func inventoryReservationStatus(job checkoutJob, id string) (string, error) {
	lookup, _ := http.NewRequest(http.MethodGet, env("INVENTORY_API_URL", "https://localhost:8082")+"/v1/reservations/"+id, nil)
	lookup.Header.Set("X-Actor-ID", job.Actor)
	lookup.Header.Set("X-Correlation-ID", job.Correlation)
	response, err := serviceAuth.Do(&http.Client{Timeout: 3 * time.Second}, "inventory", lookup)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("reservation_lookup_status_%d", response.StatusCode)
	}
	var existing struct {
		ID      string `json:"reservation_id"`
		Status  string `json:"status"`
		OrderID string `json:"order_id"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&existing); err != nil {
		return "", err
	}
	if existing.ID != id || existing.OrderID != job.Result.Order.ID {
		return "", fmt.Errorf("reservation_order_mismatch")
	}
	return existing.Status, nil
}

func compensateCheckout(job *checkoutJob) error {
	// Resolve Payment before closing the workflow: confirmation may have won a cancellation race.
	if paymentID(*job) != "" {
		payment, err := lookupPayment(*job)
		if err != nil {
			return err
		}
		switch payment.Status {
		case "paid":
			return queueCancellationRefund(job)
		case "pending":
			body, callErr := callCheckout(*job, env("PAYMENT_API_URL", "https://localhost:8083"), "/v1/payments/"+payment.ID+"/cancel", nil)
			if callErr != nil {
				return callErr
			}
			var result paymentView
			if json.Unmarshal(body, &result) != nil || result.ID != payment.ID || result.OrderID != job.Result.Order.ID || result.Status != "cancelled" || result.Amount != payment.Amount || result.Currency != payment.Currency {
				return fmt.Errorf("invalid_payment_cancel_response")
			}
			payment.Status = "cancelled"
		case "cancelled", "refunded":
		default:
			return fmt.Errorf("unknown_payment_status_%s", payment.Status)
		}
		job.Result.Payment, err = snapshotStatus(job.Result.Payment, payment.Status)
		if err != nil {
			return err
		}
	}
	if err := releaseInventoryReservation(*job, reservationID(job.Result)); err != nil {
		return err
	}
	return completeCancellation(job)
}
func recoverCheckouts() {
	rows, err := db.Query("SELECT id FROM checkouts WHERE stage IN ('reserve','payment','compensate','awaiting_payment','settling','refund_pending') AND next_attempt<=? AND lease_until<=? ORDER BY next_attempt LIMIT 20", time.Now().UnixMilli(), time.Now().UnixMilli())
	if err != nil {
		log.Printf("checkout recovery: %v", err)
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		processCheckout(id)
	}
}
func checkoutRecovery() {
	recoverCheckouts()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		recoverCheckouts()
	}
}
