package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSettlementAndCompensationPreservePaymentClientData(t *testing.T) {
	for _, channel := range []string{"app", "web"} {
		for _, outcome := range []string{"paid", "refunded"} {
			t.Run(channel+"/"+outcome, func(t *testing.T) {
				job := contractJob(t, "awaiting_payment")
				job.Input.Channel = channel
				input, err := json.Marshal(job.Input)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec("UPDATE checkouts SET request_json=? WHERE id=?", string(input), job.ID); err != nil {
					t.Fatal(err)
				}
				inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					status := "committed"
					if outcome == "refunded" {
						status = "released"
						if strings.HasSuffix(r.URL.Path, "/commit") {
							write(w, 409, map[string]string{"error": "reservation_not_active"})
							return
						}
					}
					write(w, 200, map[string]string{"reservation_id": "r1", "order_id": "o1", "status": status})
				}))
				defer inventory.Close()
				t.Setenv("INVENTORY_API_URL", inventory.URL)
				payment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					status := "paid"
					if strings.HasSuffix(r.URL.Path, "/refund") {
						status = "refunded"
					}
					write(w, 200, paymentView{ID: "p1", OrderID: "o1", Status: status, Amount: 650, Currency: "TWD"})
				}))
				defer payment.Close()
				t.Setenv("PAYMENT_API_URL", payment.URL)
				processCheckout(job.ID)
				finished, err := loadCheckout(job.ID)
				if err != nil {
					t.Fatal(err)
				}
				var snapshot map[string]any
				if err = json.Unmarshal(finished.Result.Payment, &snapshot); err != nil {
					t.Fatal(err)
				}
				if snapshot["id"] != "p1" || snapshot["client_token"] != "token" || snapshot["checkout_url"] != "https://pay.test/p1" || snapshot["status"] != outcome {
					t.Fatalf("payment=%v", snapshot)
				}
				wantStage, wantOrder := "paid", "paid"
				if outcome == "refunded" {
					wantStage, wantOrder = "failed", "cancelled"
				}
				var canonical string
				if err = db.QueryRow("SELECT status FROM orders WHERE id='o1'").Scan(&canonical); err != nil {
					t.Fatal(err)
				}
				if finished.Stage != wantStage || finished.Result.Order.Status != wantOrder || canonical != wantOrder {
					t.Fatalf("stage=%s snapshot=%s canonical=%s", finished.Stage, finished.Result.Order.Status, canonical)
				}
				before, _ := json.Marshal(finished.Result)
				processCheckout(job.ID)
				replayed, err := loadCheckout(job.ID)
				if err != nil {
					t.Fatal(err)
				}
				after, _ := json.Marshal(replayed.Result)
				if string(before) != string(after) {
					t.Fatal("terminal replay changed client data")
				}
			})
		}
	}
}
