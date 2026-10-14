package main

import (
	"fmt"
	"net/http"
)

func cancellationRequested(job *checkoutJob) bool {
	// A cancellation can arrive while an upstream creation request is in flight.
	// Preserve the original stage/key until its unknown outcome has been recovered.
	var requested bool
	if db.QueryRow("SELECT cancel_requested FROM checkouts WHERE id=?", job.ID).Scan(&requested) == nil && requested {
		job.CancelRequested = true
		job.ErrorStatus, job.ErrorMessage = 409, "order_cancelled"
	}
	return job.CancelRequested
}

func queueCancellationRefund(job *checkoutJob) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err = tx.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&status); err != nil {
		return err
	}
	if status != "cancelling" && status != "pending_payment" && status != "refund_pending" {
		return fmt.Errorf("order_cannot_refund_%s", status)
	}
	if err = orderTransitionTx(tx, *job, status, "refund_pending", false); err != nil {
		return err
	}
	job.Result.State, job.Result.Order.Status = "refund_pending", "refund_pending"
	job.Result.Payment, err = snapshotStatus(job.Result.Payment, "paid")
	if err != nil {
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

func requestCancellation(w http.ResponseWriter, r *http.Request, orderID, checkoutID string) {
	tx, err := db.Begin()
	if err != nil {
		write(w, 500, map[string]string{"error": "cancellation_unavailable"})
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE orders SET status='cancelling' WHERE id=? AND status='pending_payment'", orderID); err != nil {
		write(w, 500, map[string]string{"error": "cancellation_unavailable"})
		return
	}
	var status string
	if err = tx.QueryRow("SELECT status FROM orders WHERE id=?", orderID).Scan(&status); err != nil {
		write(w, 500, map[string]string{"error": "cancellation_unavailable"})
		return
	}
	if status == "cancelled" {
		write(w, 200, map[string]string{"order_id": orderID, "status": status, "checkout_id": checkoutID})
		return
	}
	if status == "refund_pending" {
		var requested bool
		if tx.QueryRow("SELECT cancel_requested FROM checkouts WHERE id=?", checkoutID).Scan(&requested) == nil && requested {
			write(w, 202, map[string]string{"order_id": orderID, "status": status, "checkout_id": checkoutID})
			return
		}
	}
	if status != "cancelling" {
		write(w, 409, map[string]string{"error": "settlement_in_progress_or_completed", "status": status})
		return
	}
	if _, err = tx.Exec("UPDATE checkouts SET cancel_requested=1,error_status=409,error_message='order_cancelled',next_attempt=0 WHERE id=?", checkoutID); err != nil {
		write(w, 500, map[string]string{"error": "cancellation_unavailable"})
		return
	}
	if err = tx.Commit(); err != nil {
		write(w, 500, map[string]string{"error": "cancellation_unavailable"})
		return
	}
	processCheckout(checkoutID)
	if err = db.QueryRow("SELECT status FROM orders WHERE id=?", orderID).Scan(&status); err != nil {
		write(w, 500, map[string]string{"error": "cancellation_unavailable"})
		return
	}
	code := 202
	if status == "cancelled" {
		code = 200
	}
	write(w, code, map[string]string{"order_id": orderID, "status": status, "checkout_id": checkoutID})
}
