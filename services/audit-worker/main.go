package main

import (
	"database/sql"
	"encoding/json"
	"log"
	_ "modernc.org/sqlite"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

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
	path := env("DB_PATH", "./data/audit.db")
	if dir := strings.TrimSuffix(path, "/"+base(path)); dir != "" && dir != path {
		_ = os.MkdirAll(dir, 0755)
	}
	var err error
	db, err = sql.Open("sqlite", path)
	if err != nil {
		log.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS audit_events (event_id TEXT PRIMARY KEY,event_type TEXT NOT NULL,service TEXT NOT NULL,aggregate_type TEXT NOT NULL,aggregate_id TEXT NOT NULL,actor_id TEXT NOT NULL,correlation_id TEXT NOT NULL,action TEXT NOT NULL,status TEXT NOT NULL,before_json TEXT,after_json TEXT,occurred_at TEXT NOT NULL);`); err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", health)
	mux.HandleFunc("/v1/audit/events", events)
	addr := ":" + env("PORT", "8084")
	log.Printf("audit worker listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, logging(mux)))
}

func env(k, fallback string) string {
	if value := os.Getenv(k); value != "" {
		return value
	}
	return fallback
}
func base(path string) string {
	if index := strings.LastIndex(path, "/"); index >= 0 {
		return path[index+1:]
	}
	return path
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok", "service": "audit-worker"})
}

func events(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var event auditEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil || event.EventID == "" || event.EventType == "" || event.Service == "" || event.AggregateID == "" || event.OccurredAt == "" {
			writeJSON(w, 400, map[string]string{"error": "invalid_audit_event"})
			return
		}
		before, _ := json.Marshal(event.Before)
		after, _ := json.Marshal(event.After)
		_, err := db.Exec("INSERT OR IGNORE INTO audit_events(event_id,event_type,service,aggregate_type,aggregate_id,actor_id,correlation_id,action,status,before_json,after_json,occurred_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", event.EventID, event.EventType, event.Service, event.AggregateType, event.AggregateID, event.ActorID, event.CorrelationID, event.Action, event.Status, string(before), string(after), event.OccurredAt)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 202, map[string]any{"accepted": true, "event_id": event.EventID})
	default:
		if r.Method != http.MethodGet {
			writeJSON(w, 405, map[string]string{"error": "method_not_allowed"})
			return
		}
		q := r.URL.Query()
		limit := auditLimit(q)
		eventType, service, aggregateID, actorID, correlationID := q.Get("event_type"), q.Get("service"), q.Get("aggregate_id"), q.Get("actor_id"), q.Get("correlation_id")
		rows, err := db.Query(`SELECT event_id,event_type,service,aggregate_type,aggregate_id,actor_id,correlation_id,action,status,before_json,after_json,occurred_at FROM audit_events WHERE (? = '' OR event_type = ?) AND (? = '' OR service = ?) AND (? = '' OR aggregate_id = ?) AND (? = '' OR actor_id = ?) AND (? = '' OR correlation_id = ?) ORDER BY occurred_at DESC LIMIT ?`, eventType, eventType, service, service, aggregateID, aggregateID, actorID, actorID, correlationID, correlationID, limit)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()
		result := []auditEvent{}
		for rows.Next() {
			var event auditEvent
			var before, after string
			if err := rows.Scan(&event.EventID, &event.EventType, &event.Service, &event.AggregateType, &event.AggregateID, &event.ActorID, &event.CorrelationID, &event.Action, &event.Status, &before, &after, &event.OccurredAt); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
			event.Before, event.After = json.RawMessage(before), json.RawMessage(after)
			result = append(result, event)
		}
		if err := rows.Err(); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"service": "audit-worker", "events": result, "count": len(result)})
	}
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
