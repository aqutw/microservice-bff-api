package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	_ "modernc.org/sqlite"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func setupInventoryTestDB(t *testing.T) {
	t.Helper()
	previous := db
	testDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	testDB.SetMaxOpenConns(1)
	db = testDB
	if err = initDB(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testDB.Close(); db = previous })
}

func TestDuplicateSKUsCannotOversell(t *testing.T) {
	setupInventoryTestDB(t)
	response := httptest.NewRecorder()
	reservations(response, httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"duplicate","items":[{"sku":"mug","quantity":15},{"sku":"mug","quantity":15}]}`)))
	if response.Code != 409 {
		t.Fatalf("status = %d, want 409", response.Code)
	}
	var available, events int
	_ = db.QueryRow("SELECT available FROM stock WHERE sku='mug'").Scan(&available)
	_ = db.QueryRow("SELECT COUNT(*) FROM audit_outbox").Scan(&events)
	if available != 20 || events != 0 {
		t.Fatalf("available=%d events=%d", available, events)
	}
}

func TestDuplicateSKUsAreMergedAndDeductedOnce(t *testing.T) {
	setupInventoryTestDB(t)
	response := httptest.NewRecorder()
	reservations(response, httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"merged","items":[{"sku":"mug","quantity":2},{"sku":"mug","quantity":3}]}`)))
	var created reservation
	_ = json.Unmarshal(response.Body.Bytes(), &created)
	if response.Code != 201 || len(created.Items) != 1 || created.Items[0].Quantity != 5 {
		t.Fatalf("response: %s", response.Body)
	}
	var available int
	_ = db.QueryRow("SELECT available FROM stock WHERE sku='mug'").Scan(&available)
	if available != 15 {
		t.Fatalf("available=%d", available)
	}
}

func TestFailedMultiSKUReservationRollsBack(t *testing.T) {
	setupInventoryTestDB(t)
	response := httptest.NewRecorder()
	reservations(response, httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"rollback","items":[{"sku":"mug","quantity":1},{"sku":"tea-leaves","quantity":31}]}`)))
	var available int
	_ = db.QueryRow("SELECT available FROM stock WHERE sku='mug'").Scan(&available)
	if response.Code != 409 || available != 20 {
		t.Fatalf("status=%d available=%d", response.Code, available)
	}
}

func TestConcurrentReservationsNeverOversell(t *testing.T) {
	setupInventoryTestDB(t)
	var wg sync.WaitGroup
	codes := make(chan int, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := httptest.NewRecorder()
			reservations(response, httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"concurrent","items":[{"sku":"mug","quantity":1}]}`)))
			codes <- response.Code
		}()
	}
	wg.Wait()
	close(codes)
	success := 0
	for code := range codes {
		if code == 201 {
			success++
		} else if code != 409 {
			t.Fatalf("status=%d", code)
		}
	}
	var available int
	_ = db.QueryRow("SELECT available FROM stock WHERE sku='mug'").Scan(&available)
	if success != 20 || available != 0 {
		t.Fatalf("success=%d available=%d", success, available)
	}
}

func TestConcurrentReservationTransitionsHaveOneWinner(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%v", mixed), func(t *testing.T) {
			setupInventoryTestDB(t)
			createdResponse := httptest.NewRecorder()
			reservations(createdResponse, httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"race","items":[{"sku":"mug","quantity":1}]}`)))
			var created reservation
			_ = json.Unmarshal(createdResponse.Body.Bytes(), &created)
			var wg sync.WaitGroup
			codes := make(chan int, 32)
			for i := 0; i < 32; i++ {
				action := "release"
				if mixed && i%2 == 0 {
					action = "commit"
				}
				wg.Add(1)
				go func(action string) {
					defer wg.Done()
					response := httptest.NewRecorder()
					reservationAction(response, httptest.NewRequest("POST", "/v1/reservations/"+created.ID+"/"+action, nil))
					codes <- response.Code
				}(action)
			}
			wg.Wait()
			close(codes)
			success := 0
			for code := range codes {
				if code == 200 {
					success++
				} else if code != 409 {
					t.Fatalf("status=%d", code)
				}
			}
			var available, events int
			var status string
			_ = db.QueryRow("SELECT available FROM stock WHERE sku='mug'").Scan(&available)
			_ = db.QueryRow("SELECT status FROM reservations WHERE id=?", created.ID).Scan(&status)
			_ = db.QueryRow("SELECT COUNT(*) FROM audit_outbox WHERE aggregate_id=?", created.ID).Scan(&events)
			want := 19
			if status == "released" {
				want = 20
			}
			if success != 1 || available != want || events != 2 {
				t.Fatalf("success=%d available=%d status=%s events=%d", success, available, status, events)
			}
		})
	}
}

func TestInventoryReserveReleaseAndOutbox(t *testing.T) {
	setupInventoryTestDB(t)
	request := httptest.NewRequest("POST", "/v1/reservations", strings.NewReader(`{"order_id":"ord-test","items":[{"sku":"mug","quantity":2}]}`))
	request.Header.Set("X-Correlation-ID", "corr-inventory-test")
	response := httptest.NewRecorder()
	reservations(response, request)
	if response.Code != 201 {
		t.Fatalf("reserve status = %d, want 201", response.Code)
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	reservationID := created["reservation_id"].(string)
	var available int
	if err := db.QueryRow("SELECT available FROM stock WHERE sku = 'mug'").Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 18 {
		t.Fatalf("available after reserve = %d, want 18", available)
	}
	releaseRequest := httptest.NewRequest("POST", "/v1/reservations/"+reservationID+"/release", nil)
	releaseResponse := httptest.NewRecorder()
	reservationAction(releaseResponse, releaseRequest)
	if releaseResponse.Code != 200 {
		t.Fatalf("release status = %d, want 200", releaseResponse.Code)
	}
	if err := db.QueryRow("SELECT available FROM stock WHERE sku = 'mug'").Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 20 {
		t.Fatalf("available after release = %d, want 20", available)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit_outbox WHERE aggregate_id = ?", reservationID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("outbox events = %d, want 2", count)
	}
}

func TestCreateReplayIsAtomic(t *testing.T) {
	setupInventoryTestDB(t)
	var first string
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/", strings.NewReader(`{"order_id":"replay","items":[{"sku":"mug","quantity":1}]}`))
		r.Header.Set("Idempotency-Key", "replay")
		w := httptest.NewRecorder()
		reservations(w, r)
		if w.Code != 201 {
			t.Fatalf("status=%d %s", w.Code, w.Body)
		}
		if i == 0 {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Fatal("replay changed response")
		}
	}
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM audit_outbox").Scan(&count)
	if count != 1 {
		t.Fatalf("events=%d", count)
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(strings.Replace(`{"order_id":"replay","items":[{"sku":"mug","quantity":1}]}`, "replay", "other", 1)))
	r.Header.Set("Idempotency-Key", "replay")
	w := httptest.NewRecorder()
	reservations(w, r)
	if w.Code != 409 {
		t.Fatalf("conflict=%d", w.Code)
	}
}
