package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestRecoverySchedulesBoundedBackoffWithoutLosingStage(t *testing.T) {
	for _, attempts := range []int{0, 1, 4, 5, 30} {
		t.Run(fmt.Sprint(attempts), func(t *testing.T) {
			job := contractJob(t, "settling")
			if _, err := db.Exec("UPDATE orders SET status='settling' WHERE id='o1'"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE checkouts SET attempts=? WHERE id=?", attempts, job.ID); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { write(w, 503, map[string]string{"error": "unavailable"}) }))
			defer server.Close()
			t.Setenv("INVENTORY_API_URL", server.URL)
			before := time.Now().UnixMilli()
			processCheckout(job.ID)
			after := time.Now().UnixMilli()
			var stage string
			var next, lease int64
			var count int
			if err := db.QueryRow("SELECT stage,attempts,next_attempt,lease_until FROM checkouts WHERE id=?", job.ID).Scan(&stage, &count, &next, &lease); err != nil {
				t.Fatal(err)
			}
			wantDelay := map[int]int64{0: 1000, 1: 2000, 4: 16000, 5: 32000, 30: 32000}[attempts]
			if stage != "settling" || count != attempts+1 || lease != 0 || next < before+wantDelay || next > after+wantDelay {
				t.Fatalf("stage=%s attempts=%d lease=%d scheduled=%d bounds=%d..%d", stage, count, lease, next, before+wantDelay, after+wantDelay)
			}
			// A caller cannot bypass the scheduled retry by invoking the worker again.
			processCheckout(job.ID)
			var unchanged int
			if err := db.QueryRow("SELECT attempts FROM checkouts WHERE id=?", job.ID).Scan(&unchanged); err != nil {
				t.Fatal(err)
			}
			if unchanged != count {
				t.Fatal("worker ignored next_attempt")
			}
		})
	}
}

func TestOperationReplayKeysSurviveStageChangesAndStayDistinct(t *testing.T) {
	job := contractJob(t, "reserve")
	keys := map[string]string{}
	effects := map[string]int{}
	var mu sync.Mutex
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := r.Header.Get("Idempotency-Key")
		if old, ok := keys[key]; ok {
			if old != r.URL.Path {
				t.Errorf("key reused across operations: %s and %s", old, r.URL.Path)
			}
		} else {
			keys[key] = r.URL.Path
			effects[r.URL.Path]++
		}
		write(w, 200, map[string]string{"status": "replayed"})
	})
	inventory, payment := httptest.NewServer(handler), httptest.NewServer(handler)
	defer inventory.Close()
	defer payment.Close()
	t.Setenv("INVENTORY_API_URL", inventory.URL)
	t.Setenv("PAYMENT_API_URL", payment.URL)
	for _, stage := range []string{"reserve", "payment", "compensate", "settling", "refund_pending"} {
		job.Stage = stage
		for _, path := range []string{"/v1/reservations", "/v1/payments", "/v1/reservations/r1/commit", "/v1/reservations/r1/release", "/v1/payments/p1/cancel", "/v1/payments/p1/refund"} {
			base := inventory.URL
			if path == "/v1/payments" || path == "/v1/payments/p1/cancel" || path == "/v1/payments/p1/refund" {
				base = payment.URL
			}
			if _, err := callCheckout(job, base, path, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for path, count := range effects {
		if count != 1 {
			t.Fatalf("%s effects=%d", path, count)
		}
	}
	if len(effects) != 6 || keys[job.ID+":reserve"] != "/v1/reservations" || keys[job.ID+":payment"] != "/v1/payments" {
		t.Fatalf("legacy create replay keys changed: %v", keys)
	}
}
