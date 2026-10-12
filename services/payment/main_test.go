package main

import (
	"database/sql"
	"encoding/json"
	_ "modernc.org/sqlite"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

func TestPaymentConfirmIsIdempotent(t *testing.T) {
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
	if secondResponse.Code != 200 {
		t.Fatalf("second confirm = %d, want 200", secondResponse.Code)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit_outbox WHERE aggregate_id = ?", paymentID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("outbox events = %d, want 2", count)
	}
}

func TestCreateReplayIsAtomic(t *testing.T) {
	setupPaymentTestDB(t)
	var first string
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/", strings.NewReader(`{"order_id":"replay","amount_cents":650,"currency":"TWD","channel":"web"}`))
		r.Header.Set("Idempotency-Key", "replay")
		w := httptest.NewRecorder()
		payments(w, r)
		if w.Code != 201 {
			t.Fatalf("status=%d %s", w.Code, w.Body)
		}
		if i == 0 {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Fatal("replay changed response")
		}
	}
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM audit_outbox").Scan(&count)
	if count != 1 {
		t.Fatalf("events=%d", count)
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(strings.Replace(`{"order_id":"replay","amount_cents":650,"currency":"TWD","channel":"web"}`, "replay", "other", 1)))
	r.Header.Set("Idempotency-Key", "replay")
	w := httptest.NewRecorder()
	payments(w, r)
	if w.Code != 409 {
		t.Fatalf("conflict=%d", w.Code)
	}
}

func TestConcurrentConfirmationEmitsOnePaidEvent(t *testing.T) {
	setupPaymentTestDB(t)
	created := httptest.NewRecorder()
	payments(created, httptest.NewRequest("POST", "/v1/payments", strings.NewReader(`{"order_id":"race","amount_cents":650}`)))
	var payment payment
	_ = json.Unmarshal(created.Body.Bytes(), &payment)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := httptest.NewRecorder()
			paymentAction(response, httptest.NewRequest("POST", "/v1/payments/"+payment.ID+"/confirm", nil))
			if response.Code != 200 {
				t.Errorf("status=%d", response.Code)
			}
		}()
	}
	wg.Wait()
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM audit_outbox WHERE event_type='payment.confirmed'").Scan(&count)
	if count != 1 {
		t.Fatalf("paid events=%d", count)
	}
}
