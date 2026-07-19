package store

import (
	"fmt"
	"sync"
	"testing"

	sdk "github.com/cameraui/sdk/go"
)

// TestConcurrentStoreAccess_NoRace is the Task 9 review's required
// reproduction: EventStore.Upsert, EventStore.DeleteOlderThan, and a
// SegmentStore write, run concurrently from three separate goroutines
// against the same *DB. Before the DB-level lock (db.go's DB.mu, via
// Lock/Unlock) existed, SegmentStore had its own private mutex and
// EventStore had none at all — so nothing prevented these three goroutines
// from calling into the shared *sqlite3.Conn (a pure-Go/WASM build, not
// safe for concurrent use by multiple goroutines) at the same time. That
// reproduced as a real `panic: slice bounds out of range` inside the
// sqlite WASM VM under `go test -race`, not just a hypothetical data race.
//
// Run with `go test ./src/store/... -race -count=3` (per the review): with
// the DB-level lock in place, this passes cleanly and reproducibly. See
// this task's report for the RED confirmation (temporarily no-op'ing
// DB.Lock/Unlock reproduces the race/panic; restoring them fixes it).
func TestConcurrentStoreAccess_NoRace(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	events := NewEventStore(db)
	segments := NewSegmentStore(db)

	// Seed one already-ended, already-expired event so every
	// DeleteOlderThan call below has a real row to find/delete, not just a
	// no-op scan.
	seed := DetectionEvent{
		ID: "seed", CameraID: "cam1", State: "ended",
		StartTime: 1000, EndTime: 2000, Types: []string{"motion"},
		Triggers: []sdk.EventTrigger{{Type: sdk.EventTriggerMotion, Score: 0.5, FirstSeen: 1000, LastSeen: 2000}},
	}
	if err := events.Upsert([]DetectionEvent{seed}); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	const iterations = 50
	var wg sync.WaitGroup
	wg.Add(3)

	// Goroutine 1: EventStore.Upsert, repeatedly, with fresh IDs.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			ev := DetectionEvent{
				ID:        fmt.Sprintf("upsert-%d", i),
				CameraID:  "cam1",
				State:     "active",
				StartTime: int64(1_000_000 + i),
				Types:     []string{"motion"},
			}
			if err := events.Upsert([]DetectionEvent{ev}); err != nil {
				t.Errorf("Upsert: %v", err)
				return
			}
		}
	}()

	// Goroutine 2: EventStore.DeleteOlderThan, repeatedly — racing directly
	// against goroutine 1's Upserts and goroutine 3's segment writes on the
	// same shared connection.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := events.DeleteOlderThan("cam1", int64(500_000)); err != nil {
				t.Errorf("DeleteOlderThan: %v", err)
				return
			}
		}
	}()

	// Goroutine 3: SegmentStore.Add, repeatedly — a different store,
	// entirely unguarded by EventStore's own (now-shared) lock unless the
	// lock genuinely lives on the connection rather than per-store.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			seg := Segment{
				CameraID: "cam1",
				Role:     "main",
				Path:     fmt.Sprintf("/rec/seg-%d.mp4", i),
				StartMs:  int64(i * 1000),
				EndMs:    int64(i*1000 + 500),
			}
			if _, err := segments.Add(seg); err != nil {
				t.Errorf("SegmentStore.Add: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}
