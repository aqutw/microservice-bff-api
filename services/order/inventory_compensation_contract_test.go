package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestInventorySuccessMustDescribeTheExpectedTransition(t *testing.T) {
	for _, action := range []string{"commit", "release"} {
		for _, defect := range []string{"id", "order", "status", "missing", "null"} {
			t.Run(action+"/"+defect, func(t *testing.T) {
				stage, status, target := "settling", "settling", "committed"
				if action == "release" {
					stage, status, target = "compensate", "pending_payment", "released"
				}
				job := contractJob(t, stage)
				job.Result.Payment = nil
				if _, err := db.Exec("UPDATE orders SET status=? WHERE id='o1'", status); err != nil {
					t.Fatal(err)
				}
				body := map[string]string{"reservation_id": "r1", "order_id": "o1", "status": target}
				switch defect {
				case "id":
					body["reservation_id"] = "other"
				case "order":
					body["order_id"] = "other"
				case "status":
					body["status"] = "reserved"
				case "missing":
					delete(body, "status")
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if defect == "null" {
						write(w, 200, nil)
					} else {
						write(w, 200, body)
					}
				}))
				defer server.Close()
				t.Setenv("INVENTORY_API_URL", server.URL)
				var err error
				if action == "commit" {
					err = settleInventory(&job)
				} else {
					err = compensateCheckout(&job)
				}
				if err == nil {
					t.Fatal("invalid success completed workflow")
				}
				assertContractUnchanged(t, stage, status)
			})
		}
	}
}

func TestPaymentCompensationRejectsInvalidSuccessBodies(t *testing.T) {
	for _, action := range []string{"cancel", "refund"} {
		for _, field := range []string{"id", "order_id", "status", "amount_cents", "currency", "missing", "null"} {
			t.Run(action+"/"+field, func(t *testing.T) {
				stage, orderStatus, paymentStatus, target := "compensate", "pending_payment", "pending", "cancelled"
				if action == "refund" {
					stage, orderStatus, paymentStatus, target = "refund_pending", "refund_pending", "paid", "refunded"
				}
				job := contractJob(t, stage)
				if _, err := db.Exec("UPDATE orders SET status=? WHERE id='o1'", orderStatus); err != nil {
					t.Fatal(err)
				}
				body := map[string]any{"id": "p1", "order_id": "o1", "status": target, "amount_cents": 650, "currency": "TWD"}
				switch field {
				case "amount_cents":
					body[field] = 651
				case "missing":
					delete(body, "status")
				case "null":
				default:
					body[field] = "other"
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						write(w, 200, paymentView{ID: "p1", OrderID: "o1", Status: paymentStatus, Amount: 650, Currency: "TWD"})
						return
					}
					if !strings.HasSuffix(r.URL.Path, "/"+action) {
						t.Errorf("unexpected action %s", r.URL.Path)
					}
					if field == "null" {
						write(w, 200, nil)
					} else {
						write(w, 200, body)
					}
				}))
				defer server.Close()
				t.Setenv("PAYMENT_API_URL", server.URL)
				var releases atomic.Int32
				inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					releases.Add(1)
					write(w, 200, map[string]string{"reservation_id": "r1", "order_id": "o1", "status": "released"})
				}))
				defer inventory.Close()
				t.Setenv("INVENTORY_API_URL", inventory.URL)
				var err error
				if action == "refund" {
					err = refundCheckout(&job)
				} else {
					err = compensateCheckout(&job)
				}
				if err == nil {
					t.Fatal("invalid compensation completed workflow")
				}
				if releases.Load() != 0 {
					t.Fatal("invalid compensation released inventory")
				}
				assertContractUnchanged(t, stage, orderStatus)
			})
		}
	}
}

func TestReservationConflictLookupCannotReferToAnotherHold(t *testing.T) {
	job := contractJob(t, "settling")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			write(w, 409, map[string]string{"error": "reservation_not_active"})
			return
		}
		write(w, 200, map[string]string{"reservation_id": "other", "order_id": "o1", "status": "committed"})
	}))
	defer server.Close()
	t.Setenv("INVENTORY_API_URL", server.URL)
	if err := commitInventoryReservation(job, "r1"); err == nil {
		t.Fatal("unrelated lookup treated as committed")
	}
	// Preserve the client's original payment data even when the upstream contract is rejected.
	var payment map[string]any
	if err := json.Unmarshal(job.Result.Payment, &payment); err != nil {
		t.Fatal(err)
	}
	if payment["client_token"] != "token" {
		t.Fatal("contract rejection changed payment")
	}
}
