package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCommitChecksDeadlineAndOrderWithoutWaitingForSweeper(t *testing.T) {
	setupInventoryTestDB(t)
	created := httptest.NewRecorder()
	reservations(created, httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"owner","items":[{"sku":"mug","quantity":1}]}`)))
	var hold reservation
	if err := json.Unmarshal(created.Body.Bytes(), &hold); err != nil {
		t.Fatal(err)
	}
	commit := func(orderID string) int {
		w := httptest.NewRecorder()
		reservationAction(w, httptest.NewRequest("POST", "/v1/reservations/"+hold.ID+"/commit", strings.NewReader(`{"order_id":"`+orderID+`"}`)))
		return w.Code
	}
	if code := commit("other"); code != 409 {
		t.Fatalf("wrong owner=%d", code)
	}
	if _, err := db.Exec("UPDATE reservations SET expires_at=? WHERE id=?", time.Now().Add(-time.Second).UnixMilli(), hold.ID); err != nil {
		t.Fatal(err)
	}
	if code := commit("owner"); code != 409 {
		t.Fatalf("expired=%d", code)
	}
	releaseExpiredReservations()
	if code := commit("owner"); code != 409 {
		t.Fatalf("released=%d", code)
	}
	var available, committed int
	db.QueryRow("SELECT available FROM stock WHERE sku='mug'").Scan(&available)
	db.QueryRow("SELECT count(*) FROM audit_outbox WHERE event_type='inventory.committed'").Scan(&committed)
	if available != 20 || committed != 0 {
		t.Fatalf("stock=%d commits=%d", available, committed)
	}
}

func TestCommittedHoldSurvivesDeadlineAndReplay(t *testing.T) {
	setupInventoryTestDB(t)
	created := httptest.NewRecorder()
	reservations(created, httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"owner","items":[{"sku":"mug","quantity":1}]}`)))
	var hold reservation
	json.Unmarshal(created.Body.Bytes(), &hold)
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		reservationAction(w, httptest.NewRequest("POST", "/v1/reservations/"+hold.ID+"/commit", strings.NewReader(`{"order_id":"owner"}`)))
		if w.Code != 200 {
			t.Fatalf("commit=%d %s", w.Code, w.Body)
		}
		db.Exec("UPDATE reservations SET expires_at=? WHERE id=?", time.Now().Add(-time.Hour).UnixMilli(), hold.ID)
		releaseExpiredReservations()
	}
	var available, committed int
	db.QueryRow("SELECT available FROM stock WHERE sku='mug'").Scan(&available)
	db.QueryRow("SELECT count(*) FROM audit_outbox WHERE event_type='inventory.committed'").Scan(&committed)
	if available != 19 || committed != 1 {
		t.Fatalf("stock=%d commits=%d", available, committed)
	}
}
