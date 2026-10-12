package main

import (
	"database/sql"
	"encoding/json"
	_ "modernc.org/sqlite"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func setupOrderTestDB(t *testing.T) {
	t.Helper()
	previous := db
	testDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "order.db"))
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

func TestOrderWritesOutboxForCreateAndStatusChange(t *testing.T) {
	setupOrderTestDB(t)
	request := httptest.NewRequest("POST", "/v1/orders", strings.NewReader(`{"user_id":"test-user","items":[{"sku":"mug","quantity":1}],"total_cents":650,"currency":"TWD"}`))
	request.Header.Set("X-Actor-ID", "tester")
	request.Header.Set("X-Correlation-ID", "corr-order-test")
	response := httptest.NewRecorder()
	orders(response, request)
	if response.Code != 201 {
		t.Fatalf("create status = %d, want 201", response.Code)
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	orderID := created["id"].(string)
	statusRequest := httptest.NewRequest("POST", "/v1/orders/"+orderID, strings.NewReader(`{"status":"paid"}`))
	statusResponse := httptest.NewRecorder()
	orderAction(statusResponse, statusRequest)
	if statusResponse.Code != 200 {
		t.Fatalf("status update = %d, want 200", statusResponse.Code)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit_outbox WHERE aggregate_id = ?", orderID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("outbox events = %d, want 2", count)
	}
}
