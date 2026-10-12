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
	"strconv"
	"strings"
	"time"
)

type Item struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}
type createOrderRequest struct {
	UserID     string `json:"user_id"`
	Items      []Item `json:"items"`
	TotalCents int64  `json:"total_cents"`
	Currency   string `json:"currency"`
}
type order struct {
	ID         string `json:"id"`
	UserID     string `json:"user_id"`
	Items      []Item `json:"items"`
	TotalCents int64  `json:"total_cents"`
	Currency   string `json:"currency"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
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

func main() {
	path := env("DB_PATH", "./data/order.db")
	if dir := strings.TrimSuffix(path, "/"+base(path)); dir != "" && dir != path {
		_ = os.MkdirAll(dir, 0755)
	}
	var err error
	db, err = sql.Open("sqlite", path)
	if err != nil {
		log.Fatal(err)
	}
	if err = initDB(); err != nil {
		log.Fatal(err)
	}
	go auditRelay(env("AUDIT_WORKER_URL", "http://localhost:8084"))
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", health)
	mux.HandleFunc("/v1/audit/events", auditEvents)
	mux.HandleFunc("/v1/orders", orders)
	mux.HandleFunc("/v1/orders/", orderAction)
	addr := ":" + env("PORT", "8081")
	log.Printf("order listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, logging(mux)))
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
	_, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS orders (id TEXT PRIMARY KEY,user_id TEXT NOT NULL,items_json TEXT NOT NULL,total_cents INTEGER NOT NULL,currency TEXT NOT NULL,status TEXT NOT NULL,created_at DATETIME NOT NULL); CREATE TABLE IF NOT EXISTS audit_outbox (event_id TEXT PRIMARY KEY,event_type TEXT NOT NULL,service TEXT NOT NULL,aggregate_type TEXT NOT NULL,aggregate_id TEXT NOT NULL,actor_id TEXT NOT NULL,correlation_id TEXT NOT NULL,action TEXT NOT NULL,status TEXT NOT NULL,before_json TEXT,after_json TEXT,occurred_at DATETIME NOT NULL,published_at TEXT,attempts INTEGER NOT NULL DEFAULT 0,last_error TEXT);`)
	return err
}
func write(w http.ResponseWriter, s int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(s)
	_ = json.NewEncoder(w).Encode(v)
}
func health(w http.ResponseWriter, r *http.Request) {
	write(w, 200, map[string]string{"status": "ok", "service": "order"})
}
func orders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		write(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req createOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" || len(req.Items) == 0 || req.TotalCents < 0 {
		write(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	if req.Currency == "" {
		req.Currency = "TWD"
	}
	raw, _ := json.Marshal(req.Items)
	id := "ord_" + time.Now().UTC().Format("20060102150405.000000000")
	tx, err := db.Begin()
	if err != nil {
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	_, err = tx.Exec("INSERT INTO orders(id,user_id,items_json,total_cents,currency,status,created_at) VALUES(?,?,?,?,?,?,?)", id, req.UserID, string(raw), req.TotalCents, req.Currency, "pending_payment", time.Now().UTC())
	if err != nil {
		_ = tx.Rollback()
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err = recordAuditTx(tx, r, "order.created", "order", id, "create", "success", nil, map[string]any{"status": "pending_payment", "total_cents": req.TotalCents, "currency": req.Currency}); err != nil {
		_ = tx.Rollback()
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err = tx.Commit(); err != nil {
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	write(w, 201, order{ID: id, UserID: req.UserID, Items: req.Items, TotalCents: req.TotalCents, Currency: req.Currency, Status: "pending_payment", CreatedAt: time.Now().UTC().Format(time.RFC3339)})
}
func orderAction(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/orders/")
	if id == "" {
		write(w, 404, map[string]string{"error": "not_found"})
		return
	}
	var user, raw, currency, status, created string
	var total int64
	err := db.QueryRow("SELECT user_id,items_json,total_cents,currency,status,created_at FROM orders WHERE id=?", id).Scan(&user, &raw, &total, &currency, &status, &created)
	if err == sql.ErrNoRows {
		write(w, 404, map[string]string{"error": "order_not_found"})
		return
	}
	if err != nil {
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if r.Method == http.MethodGet {
		var items []Item
		_ = json.Unmarshal([]byte(raw), &items)
		write(w, 200, order{ID: id, UserID: user, Items: items, TotalCents: total, Currency: currency, Status: status, CreatedAt: created})
		return
	}
	if r.Method != http.MethodPost {
		write(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Status == "" {
		write(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	allowed := map[string]bool{"pending_payment": true, "paid": true, "cancelled": true, "shipped": true}
	if !allowed[body.Status] {
		write(w, 400, map[string]string{"error": "invalid_status"})
		return
	}
	tx, err := db.Begin()
	if err != nil {
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if _, err = tx.Exec("UPDATE orders SET status=? WHERE id=?", body.Status, id); err != nil {
		_ = tx.Rollback()
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err = recordAuditTx(tx, r, "order.status_changed", "order", id, "update_status", "success", map[string]any{"status": status}, map[string]any{"status": body.Status}); err != nil {
		_ = tx.Rollback()
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err = tx.Commit(); err != nil {
		write(w, 500, map[string]string{"error": err.Error()})
		return
	}
	write(w, 200, map[string]string{"order_id": id, "status": body.Status})
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
	_, err = tx.Exec("INSERT INTO audit_outbox(event_id,event_type,service,aggregate_type,aggregate_id,actor_id,correlation_id,action,status,before_json,after_json,occurred_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", "evt_order_"+time.Now().UTC().Format("20060102150405.000000000"), eventType, "order", aggregateType, aggregateID, actorID, correlationID, action, status, string(beforeJSON), string(afterJSON), time.Now().UTC().Format(time.RFC3339Nano))
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
	write(w, 200, map[string]any{"service": "order", "events": events, "count": len(events)})
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
			response, postErr := client.Do(request)
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
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
