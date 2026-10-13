package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func checkoutForTest(key string) *httptest.ResponseRecorder {
	request := ownedRequest("POST", "/v1/checkouts", strings.NewReader(`{"user_id":"user","items":[{"sku":"mug","quantity":1}],"total_cents":650,"currency":"TWD","channel":"web"}`))
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
		if r.Method == http.MethodPost {
			pay++
		}
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
	conflict := ownedRequest("POST", "/v1/checkouts", strings.NewReader(`{"user_id":"user","items":[{"sku":"mug","quantity":2}],"total_cents":1300,"currency":"TWD","channel":"web"}`))
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

func TestInventoryRejectionCancelsOrder(t *testing.T) {
	setupOrderTestDB(t)
	var paymentCalled atomic.Int32
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write(w, 409, map[string]string{"error": "insufficient_stock"})
	}))
	defer inventory.Close()
	payment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paymentCalled.Add(1)
		write(w, 500, map[string]string{"error": "unexpected"})
	}))
	defer payment.Close()
	t.Setenv("INVENTORY_API_URL", inventory.URL)
	t.Setenv("PAYMENT_API_URL", payment.URL)
	if response := checkoutForTest("insufficient"); response.Code != 409 {
		t.Fatalf("status=%d %s", response.Code, response.Body)
	}
	job := jobForTest(t)
	var status string
	_ = db.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&status)
	if status != "cancelled" || job.Stage != "failed" || job.ErrorStatus != 409 || paymentCalled.Load() != 0 {
		t.Fatalf("order=%s stage=%s error=%d payment=%d", status, job.Stage, job.ErrorStatus, paymentCalled.Load())
	}
}

func TestPaidOrderCommitsReservation(t *testing.T) {
	setupOrderTestDB(t)
	var mu sync.Mutex
	state, committed := "reserved", 0
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/commit") {
			state, committed = "committed", committed+1
			write(w, 200, map[string]string{"reservation_id": "r1", "status": "committed"})
			return
		}
		if r.Method == http.MethodGet {
			write(w, 200, map[string]string{"reservation_id": "r1", "status": state})
			return
		}
		write(w, 201, map[string]string{"reservation_id": "r1", "status": "reserved"})
	}))
	defer inventory.Close()
	payment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			job := jobForTest(t)
			write(w, 200, map[string]any{"id": "p1", "status": "paid", "order_id": job.Result.Order.ID, "amount_cents": 650, "currency": "TWD"})
			return
		}
		write(w, 201, map[string]string{"id": "p1", "status": "pending"})
	}))
	defer payment.Close()
	t.Setenv("INVENTORY_API_URL", inventory.URL)
	t.Setenv("PAYMENT_API_URL", payment.URL)
	if response := checkoutForTest("paid-commit"); response.Code != 201 {
		t.Fatalf("status=%d", response.Code)
	}
	job := jobForTest(t)
	_, _ = db.Exec("UPDATE checkouts SET next_attempt=0,lease_until=0 WHERE id=?", job.ID)
	recoverCheckouts()
	job = jobForTest(t)
	var status, reservation string
	_ = db.QueryRow("SELECT status,reservation_id FROM orders WHERE id=?", job.Result.Order.ID).Scan(&status, &reservation)
	mu.Lock()
	commits, finalState := committed, state
	mu.Unlock()
	if status != "paid" || job.Stage != "paid" || reservation != "r1" || commits != 1 || finalState != "committed" {
		t.Fatalf("order=%s stage=%s reservation=%s commits=%d state=%s", status, job.Stage, reservation, commits, finalState)
	}
}

func TestDirectOrderCancelReleasesReservation(t *testing.T) {
	setupOrderTestDB(t)
	var mu sync.Mutex
	state, releases := "reserved", 0
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/release") {
			state, releases = "released", releases+1
			write(w, 200, map[string]string{"reservation_id": "r1", "status": "released"})
			return
		}
		if r.Method == http.MethodGet {
			write(w, 200, map[string]string{"reservation_id": "r1", "status": state})
			return
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
	if response := checkoutForTest("direct-cancel"); response.Code != 201 {
		t.Fatalf("status=%d", response.Code)
	}
	job := jobForTest(t)
	var reservation string
	_ = db.QueryRow("SELECT reservation_id FROM orders WHERE id=?", job.Result.Order.ID).Scan(&reservation)
	if reservation != "r1" {
		t.Fatalf("reservation_id=%q", reservation)
	}
	response := httptest.NewRecorder()
	orderAction(response, ownedRequest("POST", "/v1/orders/"+job.Result.Order.ID, strings.NewReader(`{"status":"cancelled"}`)))
	if response.Code != 200 {
		t.Fatalf("cancel=%d %s", response.Code, response.Body)
	}
	var status string
	_ = db.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&status)
	mu.Lock()
	released := releases
	mu.Unlock()
	if status != "cancelled" || released != 1 {
		t.Fatalf("order=%s releases=%d", status, released)
	}
}

func TestPaidPaymentReconcilesWithoutClientRetry(t *testing.T) {
	setupOrderTestDB(t)
	var paid atomic.Bool
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write(w, 201, map[string]string{"reservation_id": "r1", "status": "reserved"})
	}))
	defer inventory.Close()
	payment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			job := jobForTest(t)
			state := "pending"
			if paid.Load() {
				state = "paid"
			}
			write(w, 200, map[string]any{"id": "p1", "status": state, "order_id": job.Result.Order.ID, "amount_cents": 650, "currency": "TWD"})
			return
		}
		write(w, 201, map[string]string{"id": "p1", "status": "pending", "checkout_url": "demo"})
	}))
	defer payment.Close()
	t.Setenv("INVENTORY_API_URL", inventory.URL)
	t.Setenv("PAYMENT_API_URL", payment.URL)
	if response := checkoutForTest("paid-recovery"); response.Code != 201 {
		t.Fatalf("status=%d", response.Code)
	}
	job := jobForTest(t)
	// The payment commits while the caller cannot update Order.
	paid.Store(true)
	_, _ = db.Exec("UPDATE orders SET status='pending_payment' WHERE id=?", job.Result.Order.ID)
	_, _ = db.Exec("UPDATE checkouts SET stage='awaiting_payment',next_attempt=0 WHERE id=?", job.ID)
	recoverCheckouts()
	recoverCheckouts()
	job = jobForTest(t)
	var status string
	var count int
	_ = db.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&status)
	_ = db.QueryRow("SELECT COUNT(*) FROM audit_outbox WHERE event_type='order.status_changed'").Scan(&count)
	if status != "paid" || job.Stage != "paid" || count != 1 {
		t.Fatalf("order=%s stage=%s events=%d", status, job.Stage, count)
	}
}
