package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOrderPublicTransitionMatrix(t *testing.T) {
	for _, from := range []string{"pending_payment", "settling", "paid", "shipped", "cancelling", "refund_pending", "cancelled"} {
		for _, target := range []string{"pending_payment", "paid", "cancelled", "shipped"} {
			t.Run(from+"/"+target, func(t *testing.T) {
				job := contractJob(t, "awaiting_payment")
				job.Result.Order.Status = from
				job.Result.State = from
				raw, _ := json.Marshal(job.Result)
				if _, err := db.Exec("UPDATE orders SET status=? WHERE id='o1'", from); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("UPDATE checkouts SET result_json=?,lease_until=? WHERE id=?", string(raw), time.Now().Add(time.Minute).UnixMilli(), job.ID); err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				orderAction(w, ownedRequest("POST", "/v1/orders/o1", strings.NewReader(`{"status":"`+target+`"}`)))
				wantCode, wantStatus, wantAudit := 409, from, 0
				if target == from && target != "paid" {
					wantCode = 200
				}
				if target == "cancelled" && (from == "pending_payment" || from == "cancelling") {
					wantCode, wantStatus = 202, "cancelling"
				}
				if from == "paid" && target == "shipped" {
					wantCode, wantStatus, wantAudit = 200, "shipped", 1
				}
				var status string
				var events int
				if err := db.QueryRow("SELECT status FROM orders WHERE id='o1'").Scan(&status); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow("SELECT count(*) FROM audit_outbox").Scan(&events); err != nil {
					t.Fatal(err)
				}
				if w.Code != wantCode || status != wantStatus || events != wantAudit {
					t.Fatalf("HTTP=%d status=%s audit=%d wanted %d %s %d", w.Code, status, events, wantCode, wantStatus, wantAudit)
				}
			})
		}
	}
}

func TestRepeatedCancellationDuringRefundRemainsPending(t *testing.T) {
	job := contractJob(t, "refund_pending")
	if _, err := db.Exec("UPDATE orders SET status='refund_pending' WHERE id='o1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE checkouts SET cancel_requested=1 WHERE id=?", job.ID); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	orderAction(w, ownedRequest("POST", "/v1/orders/o1", strings.NewReader(`{"status":"cancelled"}`)))
	if w.Code != 202 {
		t.Fatalf("refund retry=%d %s", w.Code, w.Body)
	}
	assertContractUnchanged(t, "refund_pending", "refund_pending")
}
