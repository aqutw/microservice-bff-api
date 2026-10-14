package main

import (
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpgradePreviousInventorySchemaPreservesStockAndHolds(t *testing.T) {
	previous := db
	legacy, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	db = legacy
	t.Cleanup(func() { legacy.Close(); db = previous })
	_, err = db.Exec(`CREATE TABLE stock (sku TEXT PRIMARY KEY, available INTEGER NOT NULL);
CREATE TABLE reservations (id TEXT PRIMARY KEY, order_id TEXT NOT NULL, status TEXT NOT NULL, items_json TEXT NOT NULL, created_at DATETIME DEFAULT CURRENT_TIMESTAMP);
INSERT INTO stock VALUES ('mug', 19);
INSERT INTO reservations(id,order_id,status,items_json) VALUES ('legacy','old-order','reserved','[{"sku":"mug","quantity":1}]');`)
	if err != nil {
		t.Fatal(err)
	}
	// Startup and subsequent restarts must both work with the same database.
	for i := 0; i < 2; i++ {
		if err = initDB(); err != nil {
			t.Fatalf("startup %d: %v", i, err)
		}
	}
	releaseExpiredReservations()
	var available, expires, indexes int
	var status string
	if err = db.QueryRow("SELECT available FROM stock WHERE sku='mug'").Scan(&available); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT status,expires_at FROM reservations WHERE id='legacy'").Scan(&status, &expires); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='reservations_expiry'").Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	// Legacy holds may already belong to paid orders; expiry requires an explicit migration of those associations.
	if available != 19 || status != "reserved" || expires != 0 || indexes != 1 {
		t.Fatalf("stock=%d status=%s expires=%d indexes=%d", available, status, expires, indexes)
	}
}

func TestLegacyZeroDeadlineHoldCanCommitWithoutRestoringSoldStock(t *testing.T) {
	setupInventoryTestDB(t)
	if _, err := db.Exec(`UPDATE stock SET available=19 WHERE sku='mug'; INSERT INTO reservations(id,order_id,status,items_json,expires_at) VALUES('legacy','legacy-order','reserved','[{"sku":"mug","quantity":1}]',0)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		reservationAction(w, httptest.NewRequest("POST", "/v1/reservations/legacy/commit", strings.NewReader(`{"order_id":"legacy-order"}`)))
		if w.Code != 200 {
			t.Fatalf("legacy commit=%d %s", w.Code, w.Body)
		}
	}
	releaseExpiredReservations()
	var stock, events int
	var status string
	if err := db.QueryRow("SELECT available FROM stock WHERE sku='mug'").Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT status FROM reservations WHERE id='legacy'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM audit_outbox WHERE aggregate_id='legacy'").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if stock != 19 || status != "committed" || events != 1 {
		t.Fatalf("stock=%d status=%s audit=%d", stock, status, events)
	}
}
