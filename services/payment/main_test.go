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

func setupPaymentTestDB(t *testing.T) {
	t.Helper()
	previous := db
	testDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "payment.db"))
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

func TestPaymentConfirmIsNotIdempotentlyRepeated(t *testing.T) {
	setupPaymentTestDB(t)
	request := httptest.NewRequest("POST", "/v1/payments", strings.NewReader(`{"order_id":"ord-test","amount_cents":650,"currency":"TWD","channel":"web"}`))
	response := httptest.NewRecorder()
	payments(response, request)
	if response.Code != 201 {
		t.Fatalf("create status = %d, want 201", response.Code)
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	paymentID := created["id"].(string)
	confirmRequest := httptest.NewRequest("POST", "/v1/payments/"+paymentID+"/confirm", strings.NewReader(`{}`))
	confirmResponse := httptest.NewRecorder()
	paymentAction(confirmResponse, confirmRequest)
	if confirmResponse.Code != 200 {
		t.Fatalf("first confirm = %d, want 200", confirmResponse.Code)
	}
	secondResponse := httptest.NewRecorder()
	paymentAction(secondResponse, httptest.NewRequest("POST", "/v1/payments/"+paymentID+"/confirm", strings.NewReader(`{}`)))
	if secondResponse.Code != 409 {
		t.Fatalf("second confirm = %d, want 409", secondResponse.Code)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit_outbox WHERE aggregate_id = ?", paymentID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("outbox events = %d, want 2", count)
	}
}
