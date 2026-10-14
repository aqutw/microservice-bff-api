package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func cancelForTest(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	job := jobForTest(t)
	w := httptest.NewRecorder()
	orderAction(w, ownedRequest("POST", "/v1/orders/"+job.Result.Order.ID, strings.NewReader(`{"status":"cancelled"}`)))
	return w
}

func TestCancellationAfterConfirmationRefundsExactlyOnce(t *testing.T) {
	f := setupSettlementFixture(t, "")
	for i := 0; i < 2; i++ {
		if w := cancelForTest(t); w.Code != 200 {
			t.Fatalf("cancel=%d %s", w.Code, w.Body)
		}
	}
	job := jobForTest(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	if job.Stage != "failed" || job.ErrorStatus != 409 || f.payment != "refunded" || f.inventory != "released" || f.refunds != 1 || f.commits != 0 {
		t.Fatalf("stage=%s payment=%s inventory=%s refunds=%d commits=%d", job.Stage, f.payment, f.inventory, f.refunds, f.commits)
	}
}

func TestCancellationCannotReleaseSettlingInventory(t *testing.T) {
	f := setupSettlementFixture(t, "unavailable")
	job := recoverSettlementForTest(t)
	if job.Stage != "settling" {
		t.Fatal(job.Stage)
	}
	if w := cancelForTest(t); w.Code != 409 {
		t.Fatalf("cancel during settlement=%d", w.Code)
	}
	job = recoverSettlementForTest(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	if job.Stage != "paid" || f.inventory != "committed" || f.refunds != 0 {
		t.Fatalf("stage=%s inventory=%s refunds=%d", job.Stage, f.inventory, f.refunds)
	}
}

func TestCancellationRecoversUnknownCreationOutcomes(t *testing.T) {
	for _, stage := range []string{"reserve", "payment"} {
		t.Run(stage, func(t *testing.T) {
			f := setupSettlementFixture(t, "")
			f.mu.Lock()
			f.payment = "pending"
			f.mu.Unlock()
			job := jobForTest(t)
			// Upstream creation succeeded before a crash prevented its local checkpoint.
			job.Result.Payment = nil
			if stage == "reserve" {
				job.Result.Inventory = nil
			}
			raw, err := json.Marshal(job.Result)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("UPDATE checkouts SET stage=?,result_json=? WHERE id=?", stage, string(raw), job.ID); err != nil {
				t.Fatal(err)
			}
			w := cancelForTest(t)
			if w.Code != 200 && w.Code != 202 {
				t.Fatalf("cancel=%d %s", w.Code, w.Body)
			}
			job = recoverSettlementForTest(t)
			f.mu.Lock()
			defer f.mu.Unlock()
			if job.Stage != "failed" || f.inventory != "released" || (stage == "payment" && f.payment != "cancelled") || f.commits != 0 {
				t.Fatalf("stage=%s inventory=%s payment=%s", job.Stage, f.inventory, f.payment)
			}
		})
	}
}

func TestLegacyFailedCheckoutNeverWritesHTTPZero(t *testing.T) {
	setupOrderTestDB(t)
	if _, err := db.Exec(`INSERT INTO checkouts(id,request_json,stage,result_json,actor_id,correlation_id) VALUES('old','{}','failed','{}','user','old')`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	writeCheckout(w, "old")
	if w.Code != 500 {
		t.Fatalf("invalid historical error status=%d", w.Code)
	}
}
