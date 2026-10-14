package main

import "testing"

func TestFinalizationRollsBackDatabaseAndAuditFailures(t *testing.T) {
	for _, outcome := range []string{"paid", "refunded"} {
		for _, failure := range []string{"audit", "snapshot"} {
			t.Run(outcome+"/"+failure, func(t *testing.T) {
				mode := ""
				if outcome == "refunded" {
					mode = "expired"
				}
				f := setupSettlementFixture(t, mode)
				terminal := "paid"
				if outcome == "refunded" {
					terminal = "failed"
				}
				trigger := `CREATE TRIGGER injected_failure BEFORE INSERT ON audit_outbox WHEN NEW.event_type='order.status_changed' BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`
				if failure == "snapshot" {
					trigger = `CREATE TRIGGER injected_failure BEFORE UPDATE OF stage ON checkouts WHEN NEW.stage='` + terminal + `' BEGIN SELECT RAISE(ABORT,'injected snapshot failure'); END`
				}
				if _, err := db.Exec(trigger); err != nil {
					t.Fatal(err)
				}
				job := recoverSettlementForTest(t)
				want := "settling"
				if outcome == "refunded" {
					want = "refund_pending"
				}
				var status string
				var events int
				if err := db.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow("SELECT count(*) FROM audit_outbox WHERE event_type='order.status_changed'").Scan(&events); err != nil {
					t.Fatal(err)
				}
				if status != want || job.Stage != want || job.Result.State != want || events != 0 {
					t.Fatalf("partial commit: order=%s stage=%s snapshot=%s audit=%d", status, job.Stage, job.Result.State, events)
				}
				if _, err := db.Exec("DROP TRIGGER injected_failure"); err != nil {
					t.Fatal(err)
				}
				job = recoverSettlementForTest(t)
				if err := db.QueryRow("SELECT count(*) FROM audit_outbox WHERE event_type='order.status_changed'").Scan(&events); err != nil {
					t.Fatal(err)
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if job.Stage != terminal || events != 1 {
					t.Fatalf("recovery stage=%s audit=%d", job.Stage, events)
				}
				if outcome == "paid" && f.commits != 1 {
					t.Fatalf("commits=%d", f.commits)
				}
				if outcome == "refunded" && f.refunds != 1 {
					t.Fatalf("refunds=%d", f.refunds)
				}
			})
		}
	}
}
