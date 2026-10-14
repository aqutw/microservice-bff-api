package main

import (
	"database/sql"
	"sync"
	"testing"
)

func TestCompetingPaymentsWithProductionSQLiteConnections(t *testing.T) {
	setupPaymentTestDB(t)
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
	if _, err = db.Exec("INSERT INTO payments VALUES('p1','o1',650,'TWD','web','pending',CURRENT_TIMESTAMP)"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		action := "confirm"
		if i%2 == 1 {
			action = "cancel"
		}
		workers.Add(1)
		go func(action string) {
			defer workers.Done()
			<-start
			w := paymentActionForTest(action)
			if w.Code != 200 && w.Code != 409 {
				t.Errorf("%s=%d %s", action, w.Code, w.Body)
			}
		}(action)
	}
	close(start)
	workers.Wait()
	var status string
	var events int
	if err = db.QueryRow("SELECT status FROM payments WHERE id='p1'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT count(*) FROM audit_outbox").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if (status != "paid" && status != "cancelled") || events != 1 {
		t.Fatalf("status=%s events=%d", status, events)
	}
	if db.Stats().OpenConnections < 2 {
		t.Fatal("test did not use concurrent database connections")
	}
}
