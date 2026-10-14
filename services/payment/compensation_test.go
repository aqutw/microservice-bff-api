package main

import (
	"net/http/httptest"
	"sync"
	"testing"
)

func paymentActionForTest(action string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	paymentAction(w, httptest.NewRequest("POST", "/v1/payments/p1/"+action, nil))
	return w
}

func TestCancelAndRefundAreIdempotentAndFinal(t *testing.T) {
	for _, paid := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "refund"}[paid], func(t *testing.T) {
			setupPaymentTestDB(t)
			status, action, final := "pending", "cancel", "cancelled"
			if paid {
				status, action, final = "paid", "refund", "refunded"
			}
			if _, err := db.Exec("INSERT INTO payments VALUES('p1','o1',650,'TWD','web',?,CURRENT_TIMESTAMP)", status); err != nil {
				t.Fatal(err)
			}
			var group sync.WaitGroup
			for i := 0; i < 16; i++ {
				group.Add(1)
				go func() {
					defer group.Done()
					if w := paymentActionForTest(action); w.Code != 200 {
						t.Errorf("replay=%d %s", w.Code, w.Body)
					}
				}()
			}
			group.Wait()
			if w := paymentActionForTest("confirm"); w.Code != 409 {
				t.Fatalf("confirmation after compensation=%d", w.Code)
			}
			var events int
			db.QueryRow("SELECT status FROM payments WHERE id='p1'").Scan(&status)
			db.QueryRow("SELECT count(*) FROM audit_outbox").Scan(&events)
			if status != final || events != 1 {
				t.Fatalf("status=%s events=%d", status, events)
			}
		})
	}
}

func TestConfirmationAndCancellationHaveOneWinner(t *testing.T) {
	setupPaymentTestDB(t)
	if _, err := db.Exec("INSERT INTO payments VALUES('p1','o1',650,'TWD','web','pending',CURRENT_TIMESTAMP)"); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for _, action := range []string{"confirm", "cancel"} {
		group.Add(1)
		go func(action string) {
			defer group.Done()
			w := paymentActionForTest(action)
			if w.Code != 200 && w.Code != 409 {
				t.Errorf("%s=%d", action, w.Code)
			}
		}(action)
	}
	group.Wait()
	var status string
	var events int
	db.QueryRow("SELECT status FROM payments WHERE id='p1'").Scan(&status)
	db.QueryRow("SELECT count(*) FROM audit_outbox").Scan(&events)
	if events != 1 || (status != "paid" && status != "cancelled") {
		t.Fatalf("status=%s events=%d", status, events)
	}
	if status == "paid" {
		if w := paymentActionForTest("refund"); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
}
