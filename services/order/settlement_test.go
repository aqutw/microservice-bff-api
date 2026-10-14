package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type settlementFixture struct {
	mu                                         sync.Mutex
	orderID, inventory, payment, commitFailure string
	commits, refunds                           int
	keys                                       []string
	offline                                    bool
	paymentEntered, paymentResume              chan struct{}
	commitEntered, commitResume                chan struct{}
}

func setupSettlementFixture(t *testing.T, failure string) *settlementFixture {
	t.Helper()
	setupOrderTestDB(t)
	f := &settlementFixture{inventory: "reserved", payment: "pending", commitFailure: failure}
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/v1/reservations" {
			var input struct {
				OrderID string `json:"order_id"`
			}
			json.NewDecoder(r.Body).Decode(&input)
			f.orderID = input.OrderID
		} else if strings.HasSuffix(r.URL.Path, "/commit") {
			if f.offline {
				write(w, 503, map[string]string{"error": "unavailable"})
				return
			}
			var input struct {
				OrderID string `json:"order_id"`
			}
			json.NewDecoder(r.Body).Decode(&input)
			if input.OrderID != f.orderID {
				write(w, 409, map[string]string{"error": "reservation_order_mismatch"})
				return
			}
			f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
			failure := f.commitFailure
			f.commitFailure = ""
			if failure == "unavailable" {
				write(w, 503, map[string]string{"error": "unavailable"})
				return
			}
			if failure == "expired" {
				f.inventory = "released"
			}
			if f.inventory == "released" {
				write(w, 409, map[string]string{"error": "reservation_not_active"})
				return
			}
			if f.inventory != "committed" {
				f.inventory = "committed"
				f.commits++
			}
			if f.commitEntered != nil {
				entered, resume := f.commitEntered, f.commitResume
				f.commitEntered = nil
				close(entered)
				f.mu.Unlock()
				<-resume
				f.mu.Lock()
			}
			if failure == "lost" {
				f.offline = true
				connection, _, _ := w.(http.Hijacker).Hijack()
				connection.Close()
				return
			}
		} else if strings.HasSuffix(r.URL.Path, "/release") {
			if f.inventory == "committed" {
				write(w, 409, map[string]string{"error": "reservation_not_active"})
				return
			}
			f.inventory = "released"
		}
		write(w, 200, map[string]string{"reservation_id": "r1", "order_id": f.orderID, "status": f.inventory})
	}))
	t.Cleanup(inventory.Close)
	payment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodGet && f.paymentEntered != nil {
			entered, resume := f.paymentEntered, f.paymentResume
			f.paymentEntered = nil
			close(entered)
			f.mu.Unlock()
			<-resume
			f.mu.Lock()
		}
		if strings.HasSuffix(r.URL.Path, "/refund") {
			if f.payment != "refunded" {
				f.payment = "refunded"
				f.refunds++
			}
		}
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			if f.payment == "paid" {
				write(w, 409, map[string]string{"error": "payment_transition_conflict"})
				return
			}
			f.payment = "cancelled"
		}
		status := 200
		if r.URL.Path == "/v1/payments" {
			status = 201
		}
		write(w, status, map[string]any{"id": "p1", "order_id": f.orderID, "status": f.payment, "amount_cents": 650, "currency": "TWD"})
	}))
	t.Cleanup(payment.Close)
	t.Setenv("INVENTORY_API_URL", inventory.URL)
	t.Setenv("PAYMENT_API_URL", payment.URL)
	if w := checkoutForTest(t.Name()); w.Code != 201 {
		t.Fatalf("checkout=%d %s", w.Code, w.Body)
	}
	f.mu.Lock()
	f.payment = "paid"
	f.mu.Unlock()
	return f
}

func recoverSettlementForTest(t *testing.T) checkoutJob {
	t.Helper()
	job := jobForTest(t)
	if _, err := db.Exec("UPDATE checkouts SET next_attempt=0,lease_until=0 WHERE id=?", job.ID); err != nil {
		t.Fatal(err)
	}
	recoverCheckouts()
	return jobForTest(t)
}

func TestSettlementRetriesWithoutPublishingPaidEarly(t *testing.T) {
	f := setupSettlementFixture(t, "unavailable")
	job := recoverSettlementForTest(t)
	var status string
	db.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&status)
	if status != "settling" || job.Stage != "settling" || job.Result.State != "settling" {
		t.Fatalf("order=%s checkout=%s", status, job.Stage)
	}
	job = recoverSettlementForTest(t)
	db.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&status)
	var inventory struct {
		Status string `json:"status"`
	}
	json.Unmarshal(job.Result.Inventory, &inventory)
	f.mu.Lock()
	defer f.mu.Unlock()
	if status != "paid" || job.Stage != "paid" || inventory.Status != "committed" || f.commits != 1 || len(f.keys) != 2 || f.keys[0] != f.keys[1] {
		t.Fatalf("order=%s stage=%s inventory=%s commits=%d keys=%v", status, job.Stage, inventory.Status, f.commits, f.keys)
	}
}

func TestExpiredSettlementRefundsBeforeCompletingCancellation(t *testing.T) {
	f := setupSettlementFixture(t, "expired")
	job := recoverSettlementForTest(t)
	var status string
	db.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&status)
	f.mu.Lock()
	defer f.mu.Unlock()
	if status != "cancelled" || job.Stage != "failed" || job.ErrorStatus != 409 || f.refunds != 1 || f.payment != "refunded" || f.commits != 0 {
		t.Fatalf("order=%s stage=%s error=%d payment=%s refunds=%d commits=%d", status, job.Stage, job.ErrorStatus, f.payment, f.refunds, f.commits)
	}
}

func TestCommittedReservationRecoversLostResponseAfterReopen(t *testing.T) {
	f := setupSettlementFixture(t, "lost")
	job := recoverSettlementForTest(t)
	if job.Stage != "settling" {
		t.Fatal(job.Stage)
	}
	var seq int
	var name, path string
	if err := db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	db.Close()
	reopened, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.SetMaxOpenConns(1)
	db = reopened
	t.Cleanup(func() { reopened.Close() })
	f.mu.Lock()
	f.offline = false
	f.mu.Unlock()
	job = recoverSettlementForTest(t)
	var events int
	db.QueryRow("SELECT count(*) FROM audit_outbox WHERE event_type='order.status_changed'").Scan(&events)
	f.mu.Lock()
	defer f.mu.Unlock()
	if job.Stage != "paid" || f.commits != 1 || events != 1 {
		t.Fatalf("stage=%s commits=%d events=%d", job.Stage, f.commits, events)
	}
}

func TestPaidStatusRequiresOwnedSettlementWorkflow(t *testing.T) {
	setupSettlementFixture(t, "")
	job := jobForTest(t)
	w := httptest.NewRecorder()
	orderAction(w, ownedRequest("POST", "/v1/orders/"+job.Result.Order.ID, strings.NewReader(`{"status":"paid"}`)))
	if w.Code != 409 {
		t.Fatalf("raw paid=%d", w.Code)
	}
	w = httptest.NewRecorder()
	r := ownedRequest("POST", "/v1/orders/"+job.Result.Order.ID+"/settlement", nil)
	r.Header.Set("X-Actor-ID", "other")
	orderAction(w, r)
	if w.Code != 404 {
		t.Fatalf("other owner=%d", w.Code)
	}
	w = httptest.NewRecorder()
	orderAction(w, ownedRequest("POST", "/v1/orders/"+job.Result.Order.ID+"/settlement", nil))
	if w.Code != 200 {
		t.Fatalf("settlement=%d %s", w.Code, w.Body)
	}
}

func TestUpgradePreviouslyPaidWorkflowVerifiesInventoryBeforeServingPaid(t *testing.T) {
	f := setupSettlementFixture(t, "")
	job := jobForTest(t)
	if _, err := db.Exec("UPDATE orders SET status='paid' WHERE id=?", job.Result.Order.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := initDB(); err != nil {
			t.Fatal(err)
		}
	}
	var status string
	db.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&status)
	job = jobForTest(t)
	if status != "settling" || job.Result.State != "settling" {
		t.Fatalf("legacy order=%s snapshot=%s", status, job.Result.State)
	}
	job = recoverSettlementForTest(t)
	var migrations int
	db.QueryRow("SELECT count(*) FROM audit_outbox WHERE event_type='order.settlement_recovering'").Scan(&migrations)
	f.mu.Lock()
	defer f.mu.Unlock()
	if job.Stage != "paid" || f.inventory != "committed" || migrations != 1 {
		t.Fatalf("stage=%s inventory=%s migrations=%d", job.Stage, f.inventory, migrations)
	}
}

func TestLostLeaseCannotPublishPaidOrDuplicateAudit(t *testing.T) {
	setupSettlementFixture(t, "")
	old := jobForTest(t)
	if err := reconcilePayment(&old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE checkouts SET lease_until=42 WHERE id=?", old.ID); err != nil {
		t.Fatal(err)
	}
	if err := settleInventory(&old); err == nil {
		t.Fatal("stale worker published paid")
	}
	var status string
	db.QueryRow("SELECT status FROM orders WHERE id=?", old.Result.Order.ID).Scan(&status)
	if status != "settling" {
		t.Fatalf("stale worker changed order=%s", status)
	}
	job := recoverSettlementForTest(t)
	var events int
	db.QueryRow("SELECT count(*) FROM audit_outbox WHERE event_type='order.status_changed'").Scan(&events)
	if job.Stage != "paid" || events != 1 {
		t.Fatalf("stage=%s paid events=%d", job.Stage, events)
	}
}
