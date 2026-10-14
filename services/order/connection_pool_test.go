package main

import (
	"database/sql"
	"sync"
	"testing"
)

func TestConcurrentRecoveryWithProductionSQLiteConnections(t *testing.T) {
	f := setupSettlementFixture(t, "")
	job := jobForTest(t)
	var sequence int
	var name, path string
	if err := db.QueryRow("PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	pool, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(8)
	db = pool
	t.Cleanup(func() { pool.Close() })
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < 24; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); <-start; processCheckout(job.ID) }()
	}
	close(start)
	workers.Wait()
	finished, err := loadCheckout(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var status string
	var events int
	if err := db.QueryRow("SELECT status FROM orders WHERE id=?", job.Result.Order.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM audit_outbox WHERE event_type='order.status_changed'").Scan(&events); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if finished.Stage != "paid" || status != "paid" || f.commits != 1 || events != 1 {
		t.Fatalf("stage=%s order=%s commits=%d audit=%d", finished.Stage, status, f.commits, events)
	}
	if db.Stats().OpenConnections < 2 {
		t.Fatal("test did not use concurrent database connections")
	}
}
