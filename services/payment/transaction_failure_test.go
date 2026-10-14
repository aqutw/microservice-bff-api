package main

import "testing"

func TestPaymentAuditFailureRollsBackEveryMutation(t *testing.T) {
	for _, tc := range []struct{ from, action, to string }{{"pending", "confirm", "paid"}, {"pending", "cancel", "cancelled"}, {"paid", "refund", "refunded"}} {
		t.Run(tc.action, func(t *testing.T) {
			setupPaymentTestDB(t)
			if _, err := db.Exec("INSERT INTO payments VALUES('p1','o1',650,'TWD','web',?,CURRENT_TIMESTAMP)", tc.from); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TRIGGER injected_failure BEFORE INSERT ON audit_outbox BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`); err != nil {
				t.Fatal(err)
			}
			if w := paymentActionForTest(tc.action); w.Code != 500 {
				t.Fatalf("failed mutation=%d %s", w.Code, w.Body)
			}
			var status string
			var events int
			if err := db.QueryRow("SELECT status FROM payments WHERE id='p1'").Scan(&status); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT count(*) FROM audit_outbox").Scan(&events); err != nil {
				t.Fatal(err)
			}
			if status != tc.from || events != 0 {
				t.Fatalf("partial mutation: status=%s audit=%d", status, events)
			}
			if _, err := db.Exec("DROP TRIGGER injected_failure"); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if w := paymentActionForTest(tc.action); w.Code != 200 {
					t.Fatal(w.Code)
				}
			}
			if err := db.QueryRow("SELECT status FROM payments WHERE id='p1'").Scan(&status); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT count(*) FROM audit_outbox").Scan(&events); err != nil {
				t.Fatal(err)
			}
			if status != tc.to || events != 1 {
				t.Fatalf("retry status=%s audit=%d", status, events)
			}
		})
	}
}
