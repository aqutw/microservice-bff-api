package main

import (
	"encoding/json"
	"testing"
)

func TestPaymentTransitionMatrixPreservesDeniedStatesAndAudit(t *testing.T) {
	states := []string{"pending", "paid", "cancelled", "refunded"}
	actions := []string{"confirm", "cancel", "refund"}
	valid := map[string]map[string]string{
		"pending":   {"confirm": "paid", "cancel": "cancelled"},
		"paid":      {"confirm": "paid", "refund": "refunded"},
		"cancelled": {"cancel": "cancelled"},
		"refunded":  {"refund": "refunded"},
	}
	for _, from := range states {
		for _, action := range actions {
			t.Run(from+"/"+action, func(t *testing.T) {
				setupPaymentTestDB(t)
				if _, err := db.Exec("INSERT INTO payments VALUES('p1','o1',650,'TWD','app',?,CURRENT_TIMESTAMP)", from); err != nil {
					t.Fatal(err)
				}
				response := paymentActionForTest(action)
				target, allowed := valid[from][action]
				wantCode, wantEvents := 409, 0
				if allowed {
					wantCode = 200
					if target != from {
						wantEvents = 1
					}
				} else {
					target = from
				}
				var status string
				var events int
				if err := db.QueryRow("SELECT status FROM payments WHERE id='p1'").Scan(&status); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow("SELECT count(*) FROM audit_outbox").Scan(&events); err != nil {
					t.Fatal(err)
				}
				if response.Code != wantCode || status != target || events != wantEvents {
					t.Fatalf("HTTP=%d status=%s events=%d wanted %d %s %d", response.Code, status, events, wantCode, target, wantEvents)
				}
				if allowed {
					var body payment
					if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if body.ID != "p1" || body.OrderID != "o1" || body.Status != target {
						t.Fatalf("response=%+v", body)
					}
				}
			})
		}
	}
}
