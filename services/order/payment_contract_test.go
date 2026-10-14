package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func contractJob(t *testing.T, stage string) checkoutJob {
	t.Helper()
	setupOrderTestDB(t)
	job := checkoutJob{ID: "contract", Stage: stage, Actor: "user", Correlation: "contract",
		Input: checkoutRequest{UserID: "user", TotalCents: 650, Currency: "TWD", Channel: "app", Items: []Item{{SKU: "mug", Quantity: 1}}},
		Result: checkoutResult{CheckoutID: "contract", State: stage,
			Order:     order{ID: "o1", UserID: "user", TotalCents: 650, Currency: "TWD", Status: "pending_payment"},
			Inventory: json.RawMessage(`{"reservation_id":"r1","order_id":"o1","status":"reserved"}`),
			Payment:   json.RawMessage(`{"id":"p1","status":"pending","client_token":"token","checkout_url":"https://pay.test/p1"}`)}}
	input, _ := json.Marshal(job.Input)
	result, _ := json.Marshal(job.Result)
	if _, err := db.Exec("INSERT INTO orders(id,user_id,items_json,total_cents,currency,status,created_at) VALUES('o1','user','[]',650,'TWD','pending_payment',CURRENT_TIMESTAMP)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO checkouts(id,order_id,request_json,stage,result_json,actor_id,correlation_id) VALUES(?,?,?,?,?,?,?)", job.ID, "o1", string(input), stage, string(result), job.Actor, job.Correlation); err != nil {
		t.Fatal(err)
	}
	return job
}

func assertContractUnchanged(t *testing.T, stage, orderStatus string) {
	t.Helper()
	var gotStage, gotStatus string
	var events int
	if err := db.QueryRow("SELECT stage FROM checkouts WHERE id='contract'").Scan(&gotStage); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT status FROM orders WHERE id='o1'").Scan(&gotStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM audit_outbox").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if gotStage != stage || gotStatus != orderStatus || events != 0 {
		t.Fatalf("stage=%s order=%s events=%d", gotStage, gotStatus, events)
	}
}

func TestPaymentLookupRejectsUnrelatedOrIncompletePaidResponses(t *testing.T) {
	cases := []struct {
		name     string
		response any
	}{
		{"wrong payment", map[string]any{"id": "other", "order_id": "o1", "status": "paid", "amount_cents": 650, "currency": "TWD"}},
		{"wrong order", map[string]any{"id": "p1", "order_id": "other", "status": "paid", "amount_cents": 650, "currency": "TWD"}},
		{"wrong amount", map[string]any{"id": "p1", "order_id": "o1", "status": "paid", "amount_cents": 651, "currency": "TWD"}},
		{"wrong currency", map[string]any{"id": "p1", "order_id": "o1", "status": "paid", "amount_cents": 650, "currency": "USD"}},
		{"missing amount", map[string]any{"id": "p1", "order_id": "o1", "status": "paid", "currency": "TWD"}},
		{"missing status", map[string]any{"id": "p1", "order_id": "o1", "amount_cents": 650, "currency": "TWD"}},
		{"unknown status", map[string]any{"id": "p1", "order_id": "o1", "status": "unknown", "amount_cents": 650, "currency": "TWD"}},
		{"null", nil}, {"array", []string{"paid"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := contractJob(t, "awaiting_payment")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { write(w, 200, tc.response) }))
			defer server.Close()
			t.Setenv("PAYMENT_API_URL", server.URL)
			if err := reconcilePayment(&job); err == nil {
				t.Fatal("invalid payment advanced settlement")
			}
			assertContractUnchanged(t, "awaiting_payment", "pending_payment")
		})
	}
}

func TestMissingSettlementReferencesCannotPublishPaid(t *testing.T) {
	for _, reference := range []string{"payment", "inventory"} {
		t.Run(reference, func(t *testing.T) {
			job := contractJob(t, "awaiting_payment")
			if reference == "payment" {
				job.Result.Payment = nil
			} else {
				job.Result.Inventory = nil
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				write(w, 200, paymentView{ID: "p1", OrderID: "o1", Status: "paid", Amount: 650, Currency: "TWD"})
			}))
			defer server.Close()
			t.Setenv("PAYMENT_API_URL", server.URL)
			if err := reconcilePayment(&job); err == nil {
				t.Fatal("missing reference advanced settlement")
			}
			if reference == "payment" && calls.Load() != 0 {
				t.Fatal("missing payment reference reached upstream")
			}
			assertContractUnchanged(t, "awaiting_payment", "pending_payment")
		})
	}
}
