package main

import (
	"sync"
	"testing"
	"time"
)

func waitGate(t *testing.T, gate <-chan struct{}) {
	t.Helper()
	select {
	case <-gate:
	case <-time.After(10 * time.Second):
		t.Fatal("workflow did not reach gate")
	}
}

func TestCancellationAndSettlementCompeteWhileUpstreamIsInFlight(t *testing.T) {
	for _, winner := range []string{"cancel", "settle"} {
		t.Run(winner, func(t *testing.T) {
			f := setupSettlementFixture(t, "")
			entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(resume) }) }
			t.Cleanup(release)
			f.mu.Lock()
			if winner == "cancel" {
				f.paymentEntered, f.paymentResume = entered, resume
			} else {
				f.commitEntered, f.commitResume = entered, resume
			}
			f.mu.Unlock()
			job := jobForTest(t)
			go func() { defer close(done); processCheckout(job.ID) }()
			waitGate(t, entered)
			response := cancelForTest(t)
			want := 202
			if winner == "settle" {
				want = 409
			}
			if response.Code != want {
				t.Fatalf("cancel while %s in flight=%d %s", winner, response.Code, response.Body)
			}
			release()
			waitGate(t, done)
			job = recoverSettlementForTest(t)
			f.mu.Lock()
			defer f.mu.Unlock()
			if winner == "cancel" {
				if job.Stage != "failed" || f.inventory != "released" || f.payment != "refunded" || f.refunds != 1 || f.commits != 0 {
					t.Fatalf("stage=%s inventory=%s payment=%s refunds=%d commits=%d", job.Stage, f.inventory, f.payment, f.refunds, f.commits)
				}
			} else if job.Stage != "paid" || f.inventory != "committed" || f.refunds != 0 || f.commits != 1 {
				t.Fatalf("stage=%s inventory=%s refunds=%d commits=%d", job.Stage, f.inventory, f.refunds, f.commits)
			}
		})
	}
}
