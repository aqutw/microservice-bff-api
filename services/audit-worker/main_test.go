package main

import (
	"database/sql"
	_ "modernc.org/sqlite"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func setupAuditWorkerTestDB(t *testing.T) {
	t.Helper()
	previous := db
	testDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	testDB.SetMaxOpenConns(1)
	db = testDB
	if err = initDB(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testDB.Close(); db = previous })
}

func TestAuditEventDeliveryIsIdempotent(t *testing.T) {
	setupAuditWorkerTestDB(t)
	payload := `{"event_id":"evt-1","event_type":"order.created","service":"order","aggregate_type":"order","aggregate_id":"ord-1","actor_id":"tester","correlation_id":"corr-1","action":"create","status":"success","occurred_at":"2026-10-12T10:00:00Z"}`
	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		events(response, httptest.NewRequest("POST", "/v1/audit/events", strings.NewReader(payload)))
		if response.Code != 202 {
			t.Fatalf("delivery attempt %d = %d, want 202", attempt+1, response.Code)
		}
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE event_id = 'evt-1'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("stored events = %d, want 1", count)
	}
}
