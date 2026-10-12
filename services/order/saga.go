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
	return err
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
	digest := sha256.Sum256([]byte(input.Channel + ":" + key))
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
	inserted, err := tx.Exec("INSERT INTO checkouts(id,request_json,stage,result_json,actor_id,correlation_id) VALUES(?,?,'reserve',?,?,?) ON CONFLICT(id) DO NOTHING", id, string(raw), string(snapshot), r.Header.Get("X-Actor-ID"), r.Header.Get("X-Correlation-ID"))
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
	writeCheckout(w, strings.TrimPrefix(r.URL.Path, "/v1/checkouts/"))
}
func loadCheckout(id string) (checkoutJob, error) {
	job := checkoutJob{ID: id}
	var input, result string
	err := db.QueryRow("SELECT request_json,stage,result_json,actor_id,correlation_id,error_status,error_message,attempts FROM checkouts WHERE id=?", id).Scan(&input, &job.Stage, &result, &job.Actor, &job.Correlation, &job.ErrorStatus, &job.ErrorMessage, &job.Attempts)
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
		write(w, job.ErrorStatus, map[string]any{"error": job.ErrorMessage, "checkout_id": id})
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
	_, err = db.Exec("UPDATE checkouts SET stage=?,result_json=?,error_status=?,error_message=?,attempts=0,next_attempt=0 WHERE id=?", stage, string(raw), job.ErrorStatus, job.ErrorMessage, job.ID)
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
	req.Header.Set("Idempotency-Key", job.ID+":"+job.Stage)
	req.Header.Set("X-Actor-ID", job.Actor)
	req.Header.Set("X-Correlation-ID", job.Correlation)
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
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
	claim, err := db.Exec("UPDATE checkouts SET lease_until=? WHERE id=? AND stage IN ('reserve','payment','compensate','awaiting_payment') AND lease_until<=? AND next_attempt<=?", now+60000, id, now, now)
	if err != nil {
		log.Printf("checkout claim: %v", err)
		return
	}
	changed, _ := claim.RowsAffected()
	if changed != 1 {
		return
	}
	defer func() {
		if _, err := db.Exec("UPDATE checkouts SET lease_until=0 WHERE id=?", id); err != nil {
			log.Printf("checkout unlock: %v", err)
		}
	}()
	job, err := loadCheckout(id)
	if err != nil {
		return
	}
	for step := 0; step < 3; step++ {
		switch job.Stage {
		case "reserve":
			job.Result.Inventory, err = callCheckout(job, env("INVENTORY_API_URL", "http://localhost:8082"), "/v1/reservations", map[string]any{"order_id": job.Result.Order.ID, "items": job.Input.Items})
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
				err = checkpoint(&job, "payment")
			}
		case "payment":
			job.Result.Payment, err = callCheckout(job, env("PAYMENT_API_URL", "http://localhost:8083"), "/v1/payments", map[string]any{"order_id": job.Result.Order.ID, "amount_cents": job.Input.TotalCents, "currency": job.Input.Currency, "channel": job.Input.Channel})
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
				err = checkpoint(&job, "awaiting_payment")
				if err == nil {
					return
				}
			}
		case "compensate":
			err = compensateCheckout(&job)
		case "awaiting_payment":
			err = reconcilePayment(&job)
			if err == nil {
				return
			}
		default:
			return
		}
		if err != nil {
			var failure upstreamFailure
			if job.Stage != "compensate" && job.Stage != "awaiting_payment" && errors.As(err, &failure) && failure.status >= 400 && failure.status < 500 {
				job.ErrorStatus = failure.status
				job.ErrorMessage = failure.message
				// An explicit rejection has no committed side effect. Unknown outcomes
				// keep the current stage and replay its idempotent request instead.
				if checkpoint(&job, "compensate") == nil {
					continue
				}
			}
			delay := time.Second * time.Duration(1<<min(job.Attempts, 5))
			_, updateErr := db.Exec("UPDATE checkouts SET attempts=attempts+1,next_attempt=? WHERE id=?", time.Now().Add(delay).UnixMilli(), id)
			log.Printf("checkout %s stage %s will retry: %v (checkpoint: %v)", id, job.Stage, err, updateErr)
			return
		}
	}
}
func compensateCheckout(job *checkoutJob) error {
	if len(job.Result.Inventory) > 0 {
		var reservation struct {
			ID string `json:"reservation_id"`
		}
		if err := json.Unmarshal(job.Result.Inventory, &reservation); err != nil {
			return err
		}
		_, err := callCheckout(*job, env("INVENTORY_API_URL", "http://localhost:8082"), "/v1/reservations/"+reservation.ID+"/release", nil)
		if err != nil {
			var failure upstreamFailure
			if !errors.As(err, &failure) || failure.status != 409 {
				return err
			}
			// A prior successful release can lose its response; verify it was released.
			response, getErr := (&http.Client{Timeout: 3 * time.Second}).Get(env("INVENTORY_API_URL", "http://localhost:8082") + "/v1/reservations/" + reservation.ID)
			if getErr != nil {
				return getErr
			}
			var existing struct {
				Status string `json:"status"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&existing)
			response.Body.Close()
			if response.StatusCode != 200 || decodeErr != nil || existing.Status != "released" {
				return err
			}
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed, err := tx.Exec("UPDATE orders SET status='cancelled' WHERE id=? AND status='pending_payment'", job.Result.Order.ID)
	if err != nil {
		return err
	}
	count, _ := changed.RowsAffected()
	if count == 1 {
		r, _ := http.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("X-Actor-ID", job.Actor)
		r.Header.Set("X-Correlation-ID", job.Correlation)
		if err = recordAuditTx(tx, r, "order.status_changed", "order", job.Result.Order.ID, "update_status", "success", map[string]any{"status": "pending_payment"}, map[string]any{"status": "cancelled"}); err != nil {
			return err
		}
	}
	job.Result.State = "failed"
	job.Result.Order.Status = "cancelled"
	raw, _ := json.Marshal(job.Result)
	if _, err = tx.Exec("UPDATE checkouts SET stage='failed',result_json=? WHERE id=?", string(raw), job.ID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	job.Stage = "failed"
	return nil
}
func recoverCheckouts() {
	rows, err := db.Query("SELECT id FROM checkouts WHERE stage IN ('reserve','payment','compensate','awaiting_payment') AND next_attempt<=? AND lease_until<=? ORDER BY next_attempt LIMIT 20", time.Now().UnixMilli(), time.Now().UnixMilli())
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

// A persisted awaiting-payment workflow remains a recovery job until Order
// observes Payment's durable paid state. No client retry is required.
func reconcilePayment(job *checkoutJob) error {
	var reference struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(job.Result.Payment, &reference); err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get(env("PAYMENT_API_URL", "http://localhost:8083") + "/v1/payments/" + reference.ID)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("payment_lookup_status_%d", response.StatusCode)
	}
	var payment struct {
		Status   string `json:"status"`
		OrderID  string `json:"order_id"`
		Amount   int64  `json:"amount_cents"`
		Currency string `json:"currency"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&payment); err != nil {
		return err
	}
	if payment.Status != "paid" {
		_, err = db.Exec("UPDATE checkouts SET next_attempt=? WHERE id=?", time.Now().Add(5*time.Second).UnixMilli(), job.ID)
		return err
	}
	if payment.OrderID != job.Result.Order.ID || payment.Amount != job.Input.TotalCents || payment.Currency != job.Input.Currency {
		return fmt.Errorf("payment_order_mismatch")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed, err := tx.Exec("UPDATE orders SET status='paid' WHERE id=? AND status='pending_payment'", job.Result.Order.ID)
	if err != nil {
		return err
	}
	count, err := changed.RowsAffected()
	if err != nil {
		return err
	}
	var current string
	if err = tx.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&current); err != nil {
		return err
	}
	if current != "paid" && current != "shipped" {
		return fmt.Errorf("order_cannot_be_paid_%s", current)
	}
	if count == 1 {
		request, _ := http.NewRequest(http.MethodPost, "/", nil)
		request.Header.Set("X-Actor-ID", job.Actor)
		request.Header.Set("X-Correlation-ID", job.Correlation)
		if err = recordAuditTx(tx, request, "order.status_changed", "order", job.Result.Order.ID, "update_status", "success", map[string]any{"status": "pending_payment"}, map[string]any{"status": "paid"}); err != nil {
			return err
		}
	}
	job.Result.State = "paid"
	job.Result.Order.Status = current
	var snapshot map[string]any
	if err = json.Unmarshal(job.Result.Payment, &snapshot); err != nil {
		return err
	}
	snapshot["status"] = "paid"
	job.Result.Payment, _ = json.Marshal(snapshot)
	raw, _ := json.Marshal(job.Result)
	_, err = tx.Exec("UPDATE checkouts SET stage='paid',result_json=?,attempts=0,next_attempt=0 WHERE id=?", string(raw), job.ID)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	job.Stage = "paid"
	return nil
}
