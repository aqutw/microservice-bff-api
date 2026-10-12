package main

import (
	"database/sql"
	"encoding/json"
	"io"
	_ "modernc.org/sqlite"
	"net/http"
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
	request := ownedRequest("POST", "/v1/orders", strings.NewReader(`{"user_id":"test-user","items":[{"sku":"mug","quantity":1}],"total_cents":650,"currency":"TWD"}`))
	request.Header.Set("X-Actor-ID", "test-user")
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
	statusRequest := ownedRequest("POST", "/v1/orders/"+orderID, strings.NewReader(`{"status":"paid"}`))
	statusRequest.Header.Set("X-Actor-ID", "test-user")
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

func TestRepeatedOrderStatusDoesNotDuplicateAudit(t *testing.T) {
	setupOrderTestDB(t)
	created := httptest.NewRecorder()
	orders(created, ownedRequest("POST", "/v1/orders", strings.NewReader(`{"user_id":"user","items":[{"sku":"mug","quantity":1}],"total_cents":650}`)))
	var result order
	_ = json.Unmarshal(created.Body.Bytes(), &result)
	for i := 0; i < 2; i++ {
		response := httptest.NewRecorder()
		orderAction(response, ownedRequest("POST", "/v1/orders/"+result.ID, strings.NewReader(`{"status":"paid"}`)))
		if response.Code != 200 {
			t.Fatalf("status=%d", response.Code)
		}
	}
	invalid := httptest.NewRecorder()
	orderAction(invalid, ownedRequest("POST", "/v1/orders/"+result.ID, strings.NewReader(`{"status":"pending_payment"}`)))
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM audit_outbox WHERE aggregate_id=?", result.ID).Scan(&count)
	if count != 2 || invalid.Code != 409 {
		t.Fatalf("events=%d invalid transition=%d", count, invalid.Code)
	}
}

func ownedRequest(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("X-Actor-ID", "user")
	return r
}
