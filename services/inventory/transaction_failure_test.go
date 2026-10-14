package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInventoryAuditFailureRollsBackStockAndReservation(t *testing.T) {
	for _, action := range []string{"reserve", "commit", "release"} {
		t.Run(action, func(t *testing.T) {
			setupInventoryTestDB(t)
			var hold reservation
			create := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				reservations(w, httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"atomic","items":[{"sku":"mug","quantity":1}]}`)))
				return w
			}
			if action != "reserve" {
				w := create()
				if w.Code != 201 {
					t.Fatal(w.Code)
				}
				if err := json.Unmarshal(w.Body.Bytes(), &hold); err != nil {
					t.Fatal(err)
				}
			}
			mutate := func() *httptest.ResponseRecorder {
				if action == "reserve" {
					return create()
				}
				w := httptest.NewRecorder()
				reservationAction(w, httptest.NewRequest("POST", "/v1/reservations/"+hold.ID+"/"+action, strings.NewReader(`{"order_id":"atomic"}`)))
				return w
			}
			if _, err := db.Exec(`CREATE TRIGGER injected_failure BEFORE INSERT ON audit_outbox BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`); err != nil {
				t.Fatal(err)
			}
			if w := mutate(); w.Code != 500 {
				t.Fatalf("failed mutation=%d %s", w.Code, w.Body)
			}
			var available, holds, events int
			if err := db.QueryRow("SELECT available FROM stock WHERE sku='mug'").Scan(&available); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT count(*) FROM reservations").Scan(&holds); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT count(*) FROM audit_outbox").Scan(&events); err != nil {
				t.Fatal(err)
			}
			wantStock, wantCount := 19, 1
			if action == "reserve" {
				wantStock, wantCount = 20, 0
			}
			if available != wantStock || holds != wantCount || events != wantCount {
				t.Fatalf("partial mutation stock=%d holds=%d audit=%d", available, holds, events)
			}
			if action != "reserve" {
				var status string
				if err := db.QueryRow("SELECT status FROM reservations WHERE id=?", hold.ID).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if status != "reserved" {
					t.Fatal(status)
				}
			}
			if _, err := db.Exec("DROP TRIGGER injected_failure"); err != nil {
				t.Fatal(err)
			}
			w := mutate()
			wantCode := 200
			if action == "reserve" {
				wantCode = 201
			}
			if w.Code != wantCode {
				t.Fatal(w.Code)
			}
			if err := db.QueryRow("SELECT available FROM stock WHERE sku='mug'").Scan(&available); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT count(*) FROM audit_outbox").Scan(&events); err != nil {
				t.Fatal(err)
			}
			wantStock, wantCount = 19, 2
			if action == "release" {
				wantStock = 20
			}
			if action == "reserve" {
				wantCount = 1
			}
			if available != wantStock || events != wantCount {
				t.Fatalf("retry stock=%d audit=%d", available, events)
			}
		})
	}
}
