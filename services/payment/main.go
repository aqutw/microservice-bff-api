package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log"
	_ "modernc.org/sqlite"
	"net/http"
	"net/url"
	"os"
	"poc/security"
	"strconv"
	"strings"
	"time"
)

type paymentRequest struct {
	OrderID     string `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	Channel     string `json:"channel"`
}
type payment struct {
	ID          string `json:"id"`
	OrderID     string `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	Channel     string `json:"channel"`
	Status      string `json:"status"`
	CheckoutURL string `json:"checkout_url,omitempty"`
	ClientToken string `json:"client_token,omitempty"`
}
type auditEvent struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	Service       string          `json:"service"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	ActorID       string          `json:"actor_id"`
	CorrelationID string          `json:"correlation_id"`
	Action        string          `json:"action"`
	Status        string          `json:"status"`
	Before        json.RawMessage `json:"before,omitempty"`
	After         json.RawMessage `json:"after,omitempty"`
	OccurredAt    string          `json:"occurred_at"`
}

var db *sql.DB
var serviceAuth *security.Auth

func main() {
	var authErr error
	serviceAuth, authErr = security.Load("payment")
	if authErr != nil {
		log.Fatal(authErr)
	}
	path := env("DB_PATH", "./data/payment.db")
	if dir := strings.TrimSuffix(path, "/"+base(path)); dir != "" && dir != path {
		_ = os.MkdirAll(dir, 0755)
	}
	var err error
	db, err = sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	if err = initDB(); err != nil {
		log.Fatal(err)
	}
	if err = serviceAuth.UseReplayStore(db); err != nil {
		log.Fatal(err)
	}
	go auditRelay(env("AUDIT_WORKER_URL", "https://localhost:8084"))
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", health)
	mux.HandleFunc("/v1/audit/events", auditEvents)
	mux.HandleFunc("/v1/payments", payments)
	mux.HandleFunc("/v1/payments/", paymentAction)
	addr := ":" + env("PORT", "8083")
	log.Printf("payment listening on %s", addr)
	log.Fatal(serviceAuth.Listen(addr, logging(mux)))
}
func env(k, f string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return f
}
func base(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
func initDB() error {
	_, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS payments (id TEXT PRIMARY KEY,order_id TEXT NOT NULL,amount_cents INTEGER NOT NULL,currency TEXT NOT NULL,channel TEXT NOT NULL,status TEXT NOT NULL,created_at DATETIME NOT NULL); CREATE TABLE IF NOT EXISTS audit_outbox (event_id TEXT PRIMARY KEY,event_type TEXT NOT NULL,service TEXT NOT NULL,aggregate_type TEXT NOT NULL,aggregate_id TEXT NOT NULL,actor_id TEXT NOT NULL,correlation_id TEXT NOT NULL,action TEXT NOT NULL,status TEXT NOT NULL,before_json TEXT,after_json TEXT,occurred_at DATETIME NOT NULL,published_at TEXT,attempts INTEGER NOT NULL DEFAULT 0,last_error TEXT);`)
	if err != nil {
		return err
	}
	return initIdempotency()
}
func write(w http.ResponseWriter, s int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(s)
	_ = json.NewEncoder(w).Encode(v)
}
func health(w http.ResponseWriter, r *http.Request) {
	write(w, 200, map[string]string{"status": "ok", "service": "payment"})
}
func payments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		write(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req paymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OrderID == "" || req.AmountCents < 0 {
		write(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	if req.Currency == "" {
		req.Currency = "TWD"
	}
	if req.Channel == "" {
		req.Channel = "web"
	}
	id := "pay_" + time.Now().UTC().Format("20060102150405.000000000")
	tx, key, proceed := beginMutation(w, r, req)
	if !proceed {
		return
	}
	var err error
	_, err = tx.Exec("INSERT INTO payments(id,order_id,amount_cents,currency,channel,status,created_at) VALUES(?,?,?,?,?,?,?)", id, req.OrderID, req.AmountCents, req.Currency, req.Channel, "pending", time.Now().UTC())
	if err != nil {
		_ = tx.Rollback()
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err = recordAuditTx(tx, r, "payment.created", "payment", id, "create", "success", nil, map[string]any{"order_id": req.OrderID, "amount_cents": req.AmountCents, "currency": req.Currency, "channel": req.Channel, "status": "pending"}); err != nil {
		_ = tx.Rollback()
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	p := payment{ID: id, OrderID: req.OrderID, AmountCents: req.AmountCents, Currency: req.Currency, Channel: req.Channel, Status: "pending"}
	if req.Channel == "app" {
		p.ClientToken = "demo_" + id
	} else {
		p.CheckoutURL = "http://localhost:3000/checkout/demo/" + id
	}
	finishMutation(w, tx, key, 201, p)
}
func paymentAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/payments/"), "/")
	id := parts[0]
	if id == "" {
		write(w, 404, map[string]string{"error": "not_found"})
		return
	}
	var p payment
	err := db.QueryRow("SELECT id,order_id,amount_cents,currency,channel,status FROM payments WHERE id=?", id).Scan(&p.ID, &p.OrderID, &p.AmountCents, &p.Currency, &p.Channel, &p.Status)
	if err == sql.ErrNoRows {
		write(w, 404, map[string]string{"error": "payment_not_found"})
		return
	}
	if err != nil {
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if caller := security.Caller(r); caller == "desktop-bff" || caller == "app-bff" {
		lookup, _ := http.NewRequest(http.MethodGet, strings.TrimRight(env("ORDER_API_URL", "https://localhost:8081"), "/")+"/v1/orders/"+p.OrderID, nil)
		lookup.Header.Set("X-Actor-ID", r.Header.Get("X-Actor-ID"))
		owner, lookupErr := serviceAuth.Do(&http.Client{Timeout: 3 * time.Second}, "order", lookup)
		if lookupErr != nil {
			write(w, 503, map[string]string{"error": "owner_lookup_unavailable"})
			return
		}
		owner.Body.Close()
		if owner.StatusCode != 200 {
			status := owner.StatusCode
			if status != 404 {
				status = 503
			}
			write(w, status, map[string]string{"error": "payment_not_found"})
			return
		}
	}
	if r.Method == http.MethodGet && len(parts) == 1 {
		write(w, 200, p)
		return
	}
	if r.Method != http.MethodPost || len(parts) != 2 || parts[1] != "confirm" {
		write(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	tx, err := db.Begin()
	if err != nil {
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	result, err := tx.Exec("UPDATE payments SET status='paid' WHERE id=? AND status='pending'", id)
	if err != nil {
		_ = tx.Rollback()
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if changed == 0 {
		err = tx.QueryRow("SELECT status FROM payments WHERE id=?", id).Scan(&p.Status)
		_ = tx.Rollback()
		if err != nil {
			write(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if p.Status == "paid" {
			write(w, 200, p)
			return
		}
		write(w, 409, map[string]string{"error": "payment_not_pending", "status": p.Status})
		return
	}
	p.Status = "paid"
	if err = recordAuditTx(tx, r, "payment.confirmed", "payment", id, "confirm", "success", map[string]any{"status": "pending"}, map[string]any{"status": "paid", "order_id": p.OrderID}); err != nil {
		_ = tx.Rollback()
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err = tx.Commit(); err != nil {
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	write(w, 200, p)
}

func recordAuditTx(tx *sql.Tx, r *http.Request, eventType, aggregateType, aggregateID, action, status string, before, after any) error {
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		return err
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		return err
	}
	actorID := r.Header.Get("X-Actor-ID")
	if actorID == "" {
		actorID = "anonymous"
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		correlationID = r.Header.Get("X-Request-ID")
	}
	if correlationID == "" {
		correlationID = "corr_" + time.Now().UTC().Format("20060102150405.000000000")
	}
	_, err = tx.Exec("INSERT INTO audit_outbox(event_id,event_type,service,aggregate_type,aggregate_id,actor_id,correlation_id,action,status,before_json,after_json,occurred_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", "evt_payment_"+time.Now().UTC().Format("20060102150405.000000000"), eventType, "payment", aggregateType, aggregateID, actorID, correlationID, action, status, string(beforeJSON), string(afterJSON), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func auditEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		write(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	q := r.URL.Query()
	limit := auditLimit(q)
	eventType, aggregateID, actorID, correlationID := q.Get("event_type"), q.Get("aggregate_id"), q.Get("actor_id"), q.Get("correlation_id")
	rows, err := db.Query(`SELECT event_id,event_type,service,aggregate_type,aggregate_id,actor_id,correlation_id,action,status,before_json,after_json,occurred_at FROM audit_outbox WHERE (? = '' OR event_type = ?) AND (? = '' OR aggregate_id = ?) AND (? = '' OR actor_id = ?) AND (? = '' OR correlation_id = ?) ORDER BY occurred_at DESC LIMIT ?`, eventType, eventType, aggregateID, aggregateID, actorID, actorID, correlationID, correlationID, limit)
	if err != nil {
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	events := []auditEvent{}
	for rows.Next() {
		var event auditEvent
		var before, after string
		if err := rows.Scan(&event.EventID, &event.EventType, &event.Service, &event.AggregateType, &event.AggregateID, &event.ActorID, &event.CorrelationID, &event.Action, &event.Status, &before, &after, &event.OccurredAt); err != nil {
			write(w, 500, map[string]string{"error": err.Error()})
			return
		}
		event.Before, event.After = json.RawMessage(before), json.RawMessage(after)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	write(w, 200, map[string]any{"service": "payment", "events": events, "count": len(events)})
}

func auditLimit(q url.Values) int {
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit < 1 {
		return 100
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func auditRelay(workerURL string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	client := &http.Client{Timeout: 2 * time.Second}
	for range ticker.C {
		rows, err := db.Query(`SELECT event_id,event_type,service,aggregate_type,aggregate_id,actor_id,correlation_id,action,status,before_json,after_json,occurred_at FROM audit_outbox WHERE published_at IS NULL ORDER BY occurred_at LIMIT 50`)
		if err != nil {
			continue
		}
		for rows.Next() {
			var event auditEvent
			var before, after string
			if err = rows.Scan(&event.EventID, &event.EventType, &event.Service, &event.AggregateType, &event.AggregateID, &event.ActorID, &event.CorrelationID, &event.Action, &event.Status, &before, &after, &event.OccurredAt); err != nil {
				continue
			}
			event.Before, event.After = json.RawMessage(before), json.RawMessage(after)
			payload, _ := json.Marshal(event)
			request, _ := http.NewRequest(http.MethodPost, strings.TrimRight(workerURL, "/")+"/v1/audit/events", bytes.NewReader(payload))
			request.Header.Set("Content-Type", "application/json")
			response, postErr := serviceAuth.Do(client, "audit-worker", request)
			if postErr == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
				response.Body.Close()
				_, _ = db.Exec("UPDATE audit_outbox SET published_at=?,last_error=NULL WHERE event_id=?", time.Now().UTC().Format(time.RFC3339Nano), event.EventID)
			} else {
				if response != nil {
					response.Body.Close()
				}
				_, _ = db.Exec("UPDATE audit_outbox SET attempts=attempts+1,last_error=? WHERE event_id=?", errorString(postErr, response), event.EventID)
			}
		}
		rows.Close()
	}
}

func errorString(err error, response *http.Response) string {
	if err != nil {
		return err.Error()
	}
	return "audit_worker_status_" + strconv.Itoa(response.StatusCode)
}
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
