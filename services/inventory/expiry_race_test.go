package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCommitReleaseAndExpiryHaveOneInventoryEffect(t *testing.T) {
	setupInventoryTestDB(t)
	if _, err := db.Exec("UPDATE stock SET available=64 WHERE sku='mug'"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 24; i++ {
		created := httptest.NewRecorder()
		reservations(created, httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"race","items":[{"sku":"mug","quantity":1}]}`)))
		if created.Code != 201 {
			t.Fatal(created.Code)
		}
		var hold reservation
		if err := json.Unmarshal(created.Body.Bytes(), &hold); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(hold.Items)
		if _, err := db.Exec("UPDATE reservations SET expires_at=? WHERE id=?", time.Now().UnixMilli()+1, hold.ID); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var group sync.WaitGroup
		for _, action := range []string{"commit", "release", "expire"} {
			group.Add(1)
			go func(action string) {
				defer group.Done()
				<-start
				if action == "expire" {
					expireReservation(hold.ID, string(raw), time.Now().UnixMilli())
					return
				}
				w := httptest.NewRecorder()
				reservationAction(w, httptest.NewRequest("POST", "/v1/reservations/"+hold.ID+"/"+action, strings.NewReader(`{"order_id":"race"}`)))
				if w.Code != 200 && w.Code != 409 {
					t.Errorf("%s=%d %s", action, w.Code, w.Body)
				}
			}(action)
		}
		close(start)
		group.Wait()
		releaseExpiredReservations()
		var status string
		var events int
		if err := db.QueryRow("SELECT status FROM reservations WHERE id=?", hold.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("SELECT count(*) FROM audit_outbox WHERE aggregate_id=?", hold.ID).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if (status != "released" && status != "committed") || events != 2 {
			t.Fatalf("status=%s audit=%d", status, events)
		}
	}
	var available, sold int
	if err := db.QueryRow("SELECT available FROM stock WHERE sku='mug'").Scan(&available); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM reservations WHERE status='committed'").Scan(&sold); err != nil {
		t.Fatal(err)
	}
	if available != 64-sold {
		t.Fatalf("stock=%d committed sales=%d", available, sold)
	}
}
