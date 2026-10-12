package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func checkoutForTest(key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest("POST", "/v1/checkouts", strings.NewReader(`{"user_id":"user","items":[{"sku":"mug","quantity":1}],"total_cents":650,"currency":"TWD","channel":"web"}`))
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	checkouts(response, request)
	return response
}
func jobForTest(t *testing.T) checkoutJob {
	t.Helper()
	var id string
	if err := db.QueryRow("SELECT id FROM checkouts LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	job, err := loadCheckout(id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestCheckoutReplayAndPayloadConflict(t *testing.T) {
	setupOrderTestDB(t)
	var reserve, pay int
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reserve++
		write(w, 201, map[string]string{"reservation_id": "r1", "status": "reserved"})
	}))
	defer inventory.Close()
	payment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pay++
		write(w, 201, map[string]string{"id": "p1", "status": "pending"})
	}))
	defer payment.Close()
	t.Setenv("INVENTORY_API_URL", inventory.URL)
	t.Setenv("PAYMENT_API_URL", payment.URL)
	if response := checkoutForTest("same-key"); response.Code != 201 {
		t.Fatalf("first=%d %s", response.Code, response.Body)
	}
	if response := checkoutForTest("same-key"); response.Code != 201 {
		t.Fatalf("replay=%d", response.Code)
	}
	conflict := httptest.NewRequest("POST", "/v1/checkouts", strings.NewReader(`{"user_id":"user","items":[{"sku":"mug","quantity":2}],"total_cents":1300,"currency":"TWD","channel":"web"}`))
	conflict.Header.Set("Idempotency-Key", "same-key")
	response := httptest.NewRecorder()
	checkouts(response, conflict)
	var orders, events int
	_ = db.QueryRow("SELECT COUNT(*) FROM orders").Scan(&orders)
	_ = db.QueryRow("SELECT COUNT(*) FROM audit_outbox").Scan(&events)
	if response.Code != 409 || orders != 1 || events != 1 || reserve != 1 || pay != 1 {
		t.Fatalf("conflict=%d orders=%d events=%d reserve=%d pay=%d", response.Code, orders, events, reserve, pay)
	}
}

func TestLostReservationResponseRecoversAfterDatabaseReopen(t *testing.T) {
	setupOrderTestDB(t)
	var calls, effects int
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			effects++
			connection, _, _ := w.(http.Hijacker).Hijack()
			connection.Close()
			return
		}
		if r.Header.Get("Idempotency-Key") == "" {
			t.Error("missing replay key")
		}
		write(w, 201, map[string]string{"reservation_id": "r1", "status": "reserved"})
	}))
	defer inventory.Close()
	payment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write(w, 201, map[string]string{"id": "p1", "status": "pending"})
	}))
	defer payment.Close()
	t.Setenv("INVENTORY_API_URL", inventory.URL)
	t.Setenv("PAYMENT_API_URL", payment.URL)
	response := checkoutForTest("lost-response")
	if response.Code != 202 {
		t.Fatalf("status=%d", response.Code)
	}
	job := jobForTest(t)
	// Close and reopen the same file to prove recovery does not depend on memory.
	var path string
	var seq int
	var name string
	if err := db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.SetMaxOpenConns(1)
	db = reopened
	defer reopened.Close()
	_, _ = db.Exec("UPDATE checkouts SET next_attempt=0,lease_until=0 WHERE id=?", job.ID)
	recoverCheckouts()
	job = jobForTest(t)
	if job.Stage != "awaiting_payment" || effects != 1 || calls != 2 {
		t.Fatalf("stage=%s effects=%d calls=%d", job.Stage, effects, calls)
	}
}

func TestCompensationFailureRemainsDurableAndRetries(t *testing.T) {
	setupOrderTestDB(t)
	var releases int
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/release") {
			releases++
			if releases == 1 {
				write(w, 503, map[string]string{"error": "unavailable"})
				return
			}
			write(w, 200, map[string]string{"status": "released"})
			return
		}
		write(w, 201, map[string]string{"reservation_id": "r1", "status": "reserved"})
	}))
	defer inventory.Close()
	payment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write(w, 422, map[string]string{"error": "payment_rejected"})
	}))
	defer payment.Close()
	t.Setenv("INVENTORY_API_URL", inventory.URL)
	t.Setenv("PAYMENT_API_URL", payment.URL)
	if response := checkoutForTest("compensate"); response.Code != 202 {
		t.Fatalf("status=%d", response.Code)
	}
	job := jobForTest(t)
	if job.Stage != "compensate" {
		t.Fatalf("stage=%s", job.Stage)
	}
	_, _ = db.Exec("UPDATE checkouts SET next_attempt=0 WHERE id=?", job.ID)
	recoverCheckouts()
	job = jobForTest(t)
	var status string
	_ = db.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&status)
	if job.Stage != "failed" || status != "cancelled" || releases != 2 {
		t.Fatalf("stage=%s order=%s releases=%d", job.Stage, status, releases)
	}
	response := httptest.NewRecorder()
	writeCheckout(response, job.ID)
	if response.Code != 422 {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestConcurrentCheckoutKeyCreatesOneWorkflow(t *testing.T) {
	setupOrderTestDB(t)
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write(w, 201, map[string]string{"reservation_id": "r1", "status": "reserved"})
	}))
	defer inventory.Close()
	payment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write(w, 201, map[string]string{"id": "p1", "status": "pending"})
	}))
	defer payment.Close()
	t.Setenv("INVENTORY_API_URL", inventory.URL)
	t.Setenv("PAYMENT_API_URL", payment.URL)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := checkoutForTest("concurrent")
			if response.Code != 201 && response.Code != 202 {
				t.Errorf("status=%d", response.Code)
			}
		}()
	}
	wg.Wait()
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM orders").Scan(&count)
	if count != 1 {
		t.Fatalf("orders=%d", count)
	}
}
