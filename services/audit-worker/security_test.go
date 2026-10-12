package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"poc/security"
	"strings"
	"sync"
	"testing"
)

func TestAuthenticatedConcurrentReadsAndDeliveries(t *testing.T) {
	setupAuditWorkerTestDB(t)
	key := bytes.Repeat([]byte{3}, 32)
	receiver := &security.Auth{Name: "audit-worker", Incoming: map[string][]byte{"order": key, "desktop-bff": key}}
	if err := receiver.UseReplayStore(db); err != nil {
		t.Fatal(err)
	}
	handler := receiver.Middleware(http.HandlerFunc(events))
	var group sync.WaitGroup
	for index := 0; index < 40; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			caller, method, body, expected := "desktop-bff", "GET", "", 200
			if index%2 == 0 {
				caller = "order"
				method = "POST"
				expected = 202
				body = fmt.Sprintf(`{"event_id":"evt_order_%d","event_type":"order.created","service":"order","aggregate_id":"order","occurred_at":"2026-10-12T00:00:00Z"}`, index)
			}
			request := httptest.NewRequest(method, "/v1/audit/events", strings.NewReader(body))
			request.Header.Set("X-Actor-ID", "usr_auditor")
			request.Header.Set("X-User-Scopes", "audit:read")
			sender := &security.Auth{Name: caller, Outgoing: map[string][]byte{"audit-worker": key}}
			if err := sender.Sign(request, "audit-worker"); err != nil {
				t.Error(err)
				return
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != expected {
				t.Errorf("%s status=%d", method, response.Code)
			}
		}(index)
	}
	group.Wait()
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM audit_events").Scan(&count)
	if count != 20 {
		t.Fatalf("events=%d", count)
	}
}
