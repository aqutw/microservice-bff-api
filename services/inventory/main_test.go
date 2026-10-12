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

func setupInventoryTestDB(t *testing.T) {
	t.Helper()
	previous := db
	testDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "inventory.db"))
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

func TestInventoryReserveReleaseAndOutbox(t *testing.T) {
	setupInventoryTestDB(t)
	request := httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"ord-test","items":[{"sku":"mug","quantity":2}]}`))
	request.Header.Set("X-Correlation-ID", "corr-inventory-test")
	response := httptest.NewRecorder()
	reservations(response, request)
	if response.Code != 201 {
		t.Fatalf("reserve status = %d, want 201", response.Code)
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	reservationID := created["reservation_id"].(string)
	var available int
	if err := db.QueryRow("SELECT available FROM stock WHERE sku = 'mug'").Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 18 {
		t.Fatalf("available after reserve = %d, want 18", available)
	}
	releaseRequest := httptest.NewRequest("POST", "/v1/reservations/"+reservationID+"/release", nil)
	releaseResponse := httptest.NewRecorder()
	reservationAction(releaseResponse, releaseRequest)
	if releaseResponse.Code != 200 {
		t.Fatalf("release status = %d, want 200", releaseResponse.Code)
	}
	if err := db.QueryRow("SELECT available FROM stock WHERE sku = 'mug'").Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 20 {
		t.Fatalf("available after release = %d, want 20", available)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit_outbox WHERE aggregate_id = ?", reservationID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("outbox events = %d, want 2", count)
	}
}
