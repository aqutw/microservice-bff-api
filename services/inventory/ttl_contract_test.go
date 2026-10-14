package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReservationTTLConfigurationProducesSafeDeadlines(t *testing.T) {
	for _, tc := range []struct {
		input   string
		seconds int64
	}{{"", 900}, {"1", 1}, {"7", 7}, {"900", 900}, {"0", 900}, {"-1", 900}, {"bad", 900}, {"9223372036854775807", 900}, {"99999999999999999999999999", 900}} {
		t.Run(tc.input, func(t *testing.T) {
			setupInventoryTestDB(t)
			t.Setenv("RESERVATION_TTL_SECONDS", tc.input)
			before := time.Now().UnixMilli()
			w := httptest.NewRecorder()
			reservations(w, httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"ttl","items":[{"sku":"mug","quantity":1}]}`)))
			after := time.Now().UnixMilli()
			if w.Code != 201 {
				t.Fatalf("reserve=%d %s", w.Code, w.Body)
			}
			var expiry int64
			if err := db.QueryRow("SELECT expires_at FROM reservations WHERE order_id='ttl'").Scan(&expiry); err != nil {
				t.Fatal(err)
			}
			if expiry < before+tc.seconds*1000 || expiry > after+tc.seconds*1000 {
				t.Fatalf("deadline=%d expected within %d..%d", expiry, before+tc.seconds*1000, after+tc.seconds*1000)
			}
		})
	}
}
