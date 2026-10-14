package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
)

func previousOrderSchema(t *testing.T) {
	t.Helper()
	setupOrderTestDB(t)
	var auditSchema string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE name='audit_outbox'").Scan(&auditSchema); err != nil {
		t.Fatal(err)
	}
	db.Close()
	legacy, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "previous.db"))
	if err != nil {
		t.Fatal(err)
	}
	legacy.SetMaxOpenConns(1)
	db = legacy
	t.Cleanup(func() { legacy.Close() })
	if _, err = db.Exec(`CREATE TABLE orders(id TEXT PRIMARY KEY,user_id TEXT NOT NULL,items_json TEXT NOT NULL,total_cents INTEGER NOT NULL,currency TEXT NOT NULL,status TEXT NOT NULL,created_at DATETIME NOT NULL);
CREATE TABLE checkouts(id TEXT PRIMARY KEY,request_json TEXT NOT NULL,stage TEXT NOT NULL,result_json TEXT NOT NULL,actor_id TEXT NOT NULL,correlation_id TEXT NOT NULL,error_status INTEGER NOT NULL DEFAULT 0,error_message TEXT NOT NULL DEFAULT '',attempts INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL DEFAULT 0,lease_until INTEGER NOT NULL DEFAULT 0);`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(auditSchema); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, status, stage string }{{"unsettled-paid", "paid", "awaiting_payment"}, {"waiting", "pending_payment", "awaiting_payment"}, {"shipped", "shipped", "awaiting_payment"}, {"cancelled", "cancelled", "failed"}, {"settled-paid", "paid", "paid"}, {"refund", "refund_pending", "refund_pending"}} {
		input, _ := json.Marshal(checkoutRequest{UserID: "legacy-owner", TotalCents: 650, Currency: "TWD", Channel: "web"})
		result, _ := json.Marshal(checkoutResult{CheckoutID: tc.id, State: tc.stage, Order: order{ID: tc.id, UserID: "legacy-owner", Status: tc.status}, Inventory: json.RawMessage(`{"reservation_id":"old","status":"reserved"}`), Payment: json.RawMessage(`{"id":"old-pay","status":"pending","checkout_url":"https://pay.test/legacy"}`)})
		if _, err = db.Exec("INSERT INTO orders VALUES(?,'legacy-owner','[]',650,'TWD',?,CURRENT_TIMESTAMP)", tc.id, tc.status); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec("INSERT INTO checkouts(id,request_json,stage,result_json,actor_id,correlation_id,error_status) VALUES(?,?,?,?,'legacy-owner','legacy',409)", tc.id, string(input), tc.stage, string(result)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPreviousOrderSchemaUpgradePreservesMixedWorkflowsAndOwners(t *testing.T) {
	previousOrderSchema(t)
	for i := 0; i < 2; i++ {
		if err := initDB(); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]string{"unsettled-paid": "settling", "waiting": "pending_payment", "shipped": "shipped", "cancelled": "cancelled", "settled-paid": "paid", "refund": "refund_pending"} {
		var owner, status, reservation string
		if err := db.QueryRow("SELECT user_id,status,reservation_id FROM orders WHERE id=?", id).Scan(&owner, &status, &reservation); err != nil {
			t.Fatal(err)
		}
		if owner != "legacy-owner" || status != want || reservation != "" {
			t.Fatalf("%s owner=%s status=%s reservation=%s", id, owner, status, reservation)
		}
		var linked string
		var cancel bool
		if err := db.QueryRow("SELECT order_id,cancel_requested FROM checkouts WHERE id=?", id).Scan(&linked, &cancel); err != nil {
			t.Fatal(err)
		}
		if linked != id || cancel {
			t.Fatalf("%s linked=%s cancel=%v", id, linked, cancel)
		}
		job, err := loadCheckout(id)
		if err != nil {
			t.Fatal(err)
		}
		var payment map[string]any
		if err = json.Unmarshal(job.Result.Payment, &payment); err != nil {
			t.Fatal(err)
		}
		if payment["checkout_url"] != "https://pay.test/legacy" {
			t.Fatal("migration changed payment client data")
		}
		if id == "unsettled-paid" && job.Result.State != "settling" {
			t.Fatal("legacy unsafe paid snapshot not quarantined")
		}
	}
	var events int
	if err := db.QueryRow("SELECT count(*) FROM audit_outbox WHERE event_type='order.settlement_recovering'").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("migration audit=%d", events)
	}
}

func TestUpgradeFailureCannotPartiallyQuarantineOrders(t *testing.T) {
	previousOrderSchema(t)
	if _, err := db.Exec(`CREATE TRIGGER block_upgrade BEFORE INSERT ON audit_outbox BEGIN SELECT RAISE(ABORT,'upgrade audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := initDB(); err == nil {
		t.Fatal("upgrade ignored audit failure")
	}
	var status, state string
	if err := db.QueryRow("SELECT status FROM orders WHERE id='unsettled-paid'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT json_extract(result_json,'$.state') FROM checkouts WHERE id='unsettled-paid'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if status != "paid" || state != "awaiting_payment" {
		t.Fatalf("partial upgrade order=%s snapshot=%s", status, state)
	}
	if _, err := db.Exec("DROP TRIGGER block_upgrade"); err != nil {
		t.Fatal(err)
	}
	if err := initDB(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT status FROM orders WHERE id='unsettled-paid'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "settling" {
		t.Fatal(status)
	}
}
