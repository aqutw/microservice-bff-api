package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResourceOwnershipRejectsOtherUserWithoutWrites(t *testing.T) {
	setupOrderTestDB(t)
	_, err := db.Exec(`INSERT INTO orders VALUES('owned','alice','[]',650,'TWD','pending_payment',CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "POST"} {
		req := ownedRequest(method, "/v1/orders/owned", strings.NewReader(`{"status":"paid"}`))
		req.Header.Set("X-Actor-ID", "bob")
		response := httptest.NewRecorder()
		orderAction(response, req)
		if response.Code != 404 {
			t.Fatalf("%s: %d", method, response.Code)
		}
	}
	var status string
	var events int
	_ = db.QueryRow("SELECT status FROM orders WHERE id='owned'").Scan(&status)
	_ = db.QueryRow("SELECT COUNT(*) FROM audit_outbox").Scan(&events)
	if status != "pending_payment" || events != 0 {
		t.Fatalf("status=%s events=%d", status, events)
	}
	req := ownedRequest("POST", "/v1/checkouts", strings.NewReader(`{"user_id":"alice","items":[{"sku":"mug","quantity":1}],"total_cents":650,"channel":"web"}`))
	response := httptest.NewRecorder()
	checkouts(response, req)
	if response.Code != 403 {
		t.Fatalf("spoofed owner=%d", response.Code)
	}
}

func TestCheckoutKeysAreNamespacedByOwnerAndLegacyRowsStayPrivate(t *testing.T) {
	setupOrderTestDB(t)
	// Stop before any outbound request; inspect the durable identifiers.
	for _, user := range []string{"alice", "bob"} {
		req := ownedRequest("POST", "/v1/checkouts", strings.NewReader(`{"user_id":"`+user+`","items":[{"sku":"mug","quantity":1}],"total_cents":650,"channel":"web"}`))
		req.Header.Set("X-Actor-ID", user)
		req.Header.Set("Idempotency-Key", "same-key")
		response := httptest.NewRecorder()
		checkouts(response, req)
	}
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM checkouts").Scan(&count)
	if count != 2 {
		t.Fatalf("distinct workflows=%d", count)
	}
	var id string
	_ = db.QueryRow("SELECT id FROM checkouts WHERE json_extract(request_json,'$.user_id')='alice'").Scan(&id)
	req := ownedRequest("GET", "/v1/checkouts/"+id, nil)
	req.Header.Set("X-Actor-ID", "bob")
	response := httptest.NewRecorder()
	checkoutStatus(response, req)
	if response.Code != 404 {
		t.Fatalf("foreign checkout=%d", response.Code)
	}
}
