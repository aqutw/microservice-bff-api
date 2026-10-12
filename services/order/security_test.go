package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"poc/security"
	"testing"
)

func TestReplayNonceSurvivesAuthenticationRestart(t *testing.T) {
	setupOrderTestDB(t)
	key := bytes.Repeat([]byte{7}, 32)
	sender := &security.Auth{Name: "desktop-bff", Outgoing: map[string][]byte{"order": key}}
	request := httptest.NewRequest("GET", "/v1/orders/unknown", nil)
	request.Header.Set("X-Actor-ID", "usr_alice")
	if err := sender.Sign(request, "order"); err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	for attempt := 0; attempt < 2; attempt++ {
		receiver := &security.Auth{Name: "order", Incoming: map[string][]byte{"desktop-bff": key}}
		if err := receiver.UseReplayStore(db); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		receiver.Middleware(handler).ServeHTTP(response, request.Clone(request.Context()))
		expected := 204
		if attempt == 1 {
			expected = 401
		}
		if response.Code != expected {
			t.Fatalf("attempt=%d status=%d", attempt, response.Code)
		}
	}
}

func TestServiceCredentialDenialRetainsCheckoutForRecovery(t *testing.T) {
	setupOrderTestDB(t)
	denied := true
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if denied {
			write(w, 401, map[string]string{"error": "service_unauthorized"})
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
	if result := checkoutForTest("service-auth-recovery"); result.Code != 202 {
		t.Fatal(result.Code)
	}
	job := jobForTest(t)
	if job.Stage != "reserve" || job.Result.Order.Status != "pending_payment" {
		t.Fatalf("unexpected compensation: %s", job.Stage)
	}
	denied = false
	_, _ = db.Exec("UPDATE checkouts SET next_attempt=0 WHERE id=?", job.ID)
	processCheckout(job.ID)
	job = jobForTest(t)
	if job.Stage != "awaiting_payment" {
		t.Fatalf("recovered stage=%s", job.Stage)
	}
}
