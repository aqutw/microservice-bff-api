package main

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func productionInventoryPool(t *testing.T) {
	t.Helper()
	setupInventoryTestDB(t)
	var sequence int
	var name, path string
	if err := db.QueryRow("PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	db.Close()
	pool, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(8)
	db = pool
	t.Cleanup(func() { pool.Close() })
}

func TestReservationsWithProductionSQLiteConnectionsNeverOversell(t *testing.T) {
	productionInventoryPool(t)
	start := make(chan struct{})
	var workers sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			w := httptest.NewRecorder()
			reservations(w, httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"pool","items":[{"sku":"mug","quantity":1}]}`)))
			if w.Code == 201 {
				mu.Lock()
				success++
				mu.Unlock()
			} else if w.Code != 409 {
				t.Errorf("reserve=%d %s", w.Code, w.Body)
			}
		}()
	}
	close(start)
	workers.Wait()
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
	if available != 0 || success != 20 || holds != 20 || events != 20 {
		t.Fatalf("stock=%d success=%d holds=%d audit=%d", available, success, holds, events)
	}
	if db.Stats().OpenConnections < 2 {
		t.Fatal("test did not use concurrent database connections")
	}
}

func TestCommitDeadlineIsCheckedAfterWaitingForWriterLock(t *testing.T) {
	productionInventoryPool(t)
	created := httptest.NewRecorder()
	reservations(created, httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"deadline","items":[{"sku":"mug","quantity":1}]}`)))
	var hold reservation
	if err := json.Unmarshal(created.Body.Bytes(), &hold); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	if _, err := db.Exec("UPDATE reservations SET expires_at=? WHERE id=?", deadline.UnixMilli(), hold.ID); err != nil {
		t.Fatal(err)
	}
	writer, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err = writer.Exec("UPDATE stock SET available=available WHERE sku='mug'"); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		reservationAction(w, httptest.NewRequest("POST", "/v1/reservations/"+hold.ID+"/commit", strings.NewReader(`{"order_id":"deadline"}`)))
		done <- w.Code
	}()
	for db.Stats().InUse < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if db.Stats().InUse < 2 {
		t.Fatal("commit did not contend for writer before deadline")
	}
	<-time.After(time.Until(deadline) + 30*time.Millisecond)
	if err = writer.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 409 {
			t.Fatalf("expired commit after writer wait=%d", code)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("commit did not finish")
	}
	releaseExpiredReservations()
	var available int
	if err = db.QueryRow("SELECT available FROM stock WHERE sku='mug'").Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 20 {
		t.Fatalf("expired stock not restored: %d", available)
	}
}
