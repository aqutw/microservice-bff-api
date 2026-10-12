package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
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
type reservationRequest struct {
	OrderID string `json:"order_id"`
	Items   []Item `json:"items"`
}
type reservation struct {
	ID      string `json:"reservation_id"`
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
	Items   []Item `json:"items"`
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
	path := getenv("DB_PATH", "./data/inventory.db")
	if err := os.MkdirAll(strings.TrimSuffix(path, "/"+filepathBase(path)), 0755); err != nil && !os.IsExist(err) {
		log.Fatal(err)
	}
	var err error
	db, err = sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	if err = initDB(); err != nil {
		log.Fatal(err)
	}
	go auditRelay(getenv("AUDIT_WORKER_URL", "http://localhost:8084"))
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", health)
	mux.HandleFunc("/v1/audit/events", auditEvents)
	mux.HandleFunc("/v1/stock/", stock)
	mux.HandleFunc("/v1/reservations", reservations)
	mux.HandleFunc("/v1/reservations/", reservationAction)
	addr := ":" + getenv("PORT", "8082")
	log.Printf("inventory listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, logging(mux)))
}

func filepathBase(path string) string {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return path
	}
	return path[i+1:]
}
func getenv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
func initDB() error {
	_, err := db.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS stock (sku TEXT PRIMARY KEY, available INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS reservations (id TEXT PRIMARY KEY, order_id TEXT NOT NULL, status TEXT NOT NULL, items_json TEXT NOT NULL, created_at DATETIME DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE IF NOT EXISTS audit_outbox (event_id TEXT PRIMARY KEY,event_type TEXT NOT NULL,service TEXT NOT NULL,aggregate_type TEXT NOT NULL,aggregate_id TEXT NOT NULL,actor_id TEXT NOT NULL,correlation_id TEXT NOT NULL,action TEXT NOT NULL,status TEXT NOT NULL,before_json TEXT,after_json TEXT,occurred_at DATETIME NOT NULL,published_at TEXT,attempts INTEGER NOT NULL DEFAULT 0,last_error TEXT);`)
	if err != nil {
		return err
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM stock").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		_, err = db.Exec("INSERT INTO stock(sku,available) VALUES ('coffee-beans',50),('tea-leaves',30),('mug',20)")
	}
	if err != nil {
		return err
	}
	return initIdempotency()
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok", "service": "inventory"})
}
func stock(w http.ResponseWriter, r *http.Request) {
	sku := strings.TrimPrefix(r.URL.Path, "/v1/stock/")
	var n int
	err := db.QueryRow("SELECT available FROM stock WHERE sku = ?", sku).Scan(&n)
	if err == sql.ErrNoRows {
		writeJSON(w, 404, map[string]string{"error": "sku_not_found"})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"sku": sku, "available": n})
}
func reservations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req reservationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OrderID == "" || len(req.Items) == 0 {
		writeJSON(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	// Merge duplicate SKUs before deduction and reject overflowing quantities.
	merged := make([]Item, 0, len(req.Items))
	positions := map[string]int{}
	for _, item := range req.Items {
		if item.SKU == "" || item.Quantity < 1 {
			writeJSON(w, 400, map[string]string{"error": "invalid_items"})
			return
		}
		if i, exists := positions[item.SKU]; exists {
			if merged[i].Quantity > int(^uint(0)>>1)-item.Quantity {
				writeJSON(w, 400, map[string]string{"error": "invalid_items"})
				return
			}
			merged[i].Quantity += item.Quantity
		} else {
			positions[item.SKU] = len(merged)
			merged = append(merged, item)
		}
	}
	req.Items = merged
	tx, key, proceed := beginMutation(w, r, req)
	if !proceed {
		return
	}
	var err error
	for _, item := range req.Items {
		result, updateErr := tx.Exec("UPDATE stock SET available = available - ? WHERE sku = ? AND available >= ?", item.Quantity, item.SKU, item.Quantity)
		if updateErr != nil {
			_ = tx.Rollback()
			writeJSON(w, 500, map[string]string{"error": updateErr.Error()})
			return
		}
		changed, rowsErr := result.RowsAffected()
		if rowsErr != nil || changed != 1 {
			_ = tx.Rollback()
			writeJSON(w, 409, map[string]string{"error": "insufficient_stock", "sku": item.SKU})
			return
		}
	}
	id := fmt.Sprintf("rsv_%d", time.Now().UnixNano())
	raw, _ := json.Marshal(req.Items)
	if _, err = tx.Exec("INSERT INTO reservations(id,order_id,status,items_json) VALUES(?,?,?,?)", id, req.OrderID, "reserved", string(raw)); err != nil {
		_ = tx.Rollback()
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err = recordAuditTx(tx, r, "inventory.reserved", "reservation", id, "reserve", "success", nil, map[string]any{"order_id": req.OrderID, "status": "reserved", "items": req.Items}); err != nil {
		_ = tx.Rollback()
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	finishMutation(w, tx, key, 201, reservation{ID: id, OrderID: req.OrderID, Status: "reserved", Items: req.Items})
}

func reservationAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/reservations/"), "/")
	id := parts[0]
	if id == "" {
		writeJSON(w, 404, map[string]string{"error": "not_found"})
		return
	}
	var status, raw string
	err := db.QueryRow("SELECT status,items_json FROM reservations WHERE id = ?", id).Scan(&status, &raw)
	if err == sql.ErrNoRows {
		writeJSON(w, 404, map[string]string{"error": "reservation_not_found"})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		var items []Item
		_ = json.Unmarshal([]byte(raw), &items)
		writeJSON(w, 200, reservation{ID: id, Status: status, Items: items})
		return
	}
	if r.Method != http.MethodPost || len(parts) != 2 {
		writeJSON(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	action := parts[1]
	target := map[string]string{"release": "released", "commit": "committed"}[action]
	if target == "" {
		writeJSON(w, 404, map[string]string{"error": "unknown_action"})
		return
	}
	if status != "reserved" {
		writeJSON(w, 409, map[string]string{"error": "reservation_not_active", "status": status})
		return
	}
	tx, err := db.Begin()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// Claim the transition before any inventory side effects. The predicate
	// remains valid even if another request changed the pre-read status.
	result, err := tx.Exec("UPDATE reservations SET status=? WHERE id=? AND status='reserved'", target, id)
	if err != nil {
		_ = tx.Rollback()
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		_ = tx.Rollback()
		writeJSON(w, 409, map[string]string{"error": "reservation_not_active"})
		return
	}
	if action == "release" {
		var items []Item
		_ = json.Unmarshal([]byte(raw), &items)
		for _, item := range items {
			if _, err = tx.Exec("UPDATE stock SET available = available + ? WHERE sku = ?", item.Quantity, item.SKU); err != nil {
				_ = tx.Rollback()
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
		}
	}
	eventType := map[string]string{"release": "inventory.released", "commit": "inventory.committed"}[action]
	if err = recordAuditTx(tx, r, eventType, "reservation", id, action, "success", map[string]any{"status": "reserved"}, map[string]any{"status": target}); err != nil {
		_ = tx.Rollback()
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"reservation_id": id, "status": target})
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
	_, err = tx.Exec("INSERT INTO audit_outbox(event_id,event_type,service,aggregate_type,aggregate_id,actor_id,correlation_id,action,status,before_json,after_json,occurred_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", "evt_inventory_"+time.Now().UTC().Format("20060102150405.000000000"), eventType, "inventory", aggregateType, aggregateID, actorID, correlationID, action, status, string(beforeJSON), string(afterJSON), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func auditEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	q := r.URL.Query()
	limit := auditLimit(q)
	eventType, aggregateID, actorID, correlationID := q.Get("event_type"), q.Get("aggregate_id"), q.Get("actor_id"), q.Get("correlation_id")
	rows, err := db.Query(`SELECT event_id,event_type,service,aggregate_type,aggregate_id,actor_id,correlation_id,action,status,before_json,after_json,occurred_at FROM audit_outbox WHERE (? = '' OR event_type = ?) AND (? = '' OR aggregate_id = ?) AND (? = '' OR actor_id = ?) AND (? = '' OR correlation_id = ?) ORDER BY occurred_at DESC LIMIT ?`, eventType, eventType, aggregateID, aggregateID, actorID, actorID, correlationID, correlationID, limit)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	events := []auditEvent{}
	for rows.Next() {
		var event auditEvent
		var before, after string
		if err := rows.Scan(&event.EventID, &event.EventType, &event.Service, &event.AggregateType, &event.AggregateID, &event.ActorID, &event.CorrelationID, &event.Action, &event.Status, &before, &after, &event.OccurredAt); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		event.Before, event.After = json.RawMessage(before), json.RawMessage(after)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"service": "inventory", "events": events, "count": len(events)})
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
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
