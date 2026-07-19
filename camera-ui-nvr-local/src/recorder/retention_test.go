package recorder

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// openRetentionStores opens a fresh SQLite database in a temp dir and
// returns everything RunRetentionOnce needs.
func openRetentionStores(t *testing.T) (*store.DB, *store.SegmentStore, *store.EventStore) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, store.NewSegmentStore(db), store.NewEventStore(db)
}

// addRetentionSegment writes a real file (containing exactly sizeBytes
// bytes, so disk-cap tests can control usage precisely) for cameraID/role
// covering [startMs, endMs), indexes it into segStore, and returns the
// resulting store.Segment (ID populated). dir is the directory the file is
// created in — callers pass a per-test t.TempDir() so file removal can be
// asserted via os.Stat afterward.
func addRetentionSegment(t *testing.T, segStore *store.SegmentStore, dir, cameraID, role string, startMs, endMs int64, sizeBytes int) store.Segment {
	t.Helper()

	path := filepath.Join(dir, fmt.Sprintf("%s-%s-%d.mp4", cameraID, role, startMs))
	if err := os.WriteFile(path, make([]byte, sizeBytes), 0o644); err != nil {
		t.Fatalf("write segment file %s: %v", path, err)
	}

	seg := store.Segment{CameraID: cameraID, Role: role, Path: path, StartMs: startMs, EndMs: endMs, HasVideo: true, Codec: "h264"}
	id, err := segStore.Add(seg)
	if err != nil {
		t.Fatalf("Add segment: %v", err)
	}
	seg.ID = id
	return seg
}

// addRetentionEvent upserts a fully-ended DetectionEvent for cameraID
// covering [startMs, endMs], and, if thumbPath != "", writes a real file at
// thumbPath and sets the row's thumb_ref column to it directly (white-box:
// no production code populates thumb_ref yet — see store.DeletedEvent's doc
// comment).
func addRetentionEvent(t *testing.T, db *store.DB, eventStore *store.EventStore, id, cameraID string, startMs, endMs int64, thumbPath string) {
	t.Helper()

	ev := store.DetectionEvent{
		ID:         id,
		CameraID:   cameraID,
		State:      "ended",
		StartTime:  startMs,
		EndTime:    endMs,
		LastUpdate: endMs,
		Types:      []string{"motion"},
		Triggers: []sdk.EventTrigger{
			{Type: sdk.EventTriggerMotion, Score: 0.9, FirstSeen: startMs, LastSeen: endMs},
		},
	}
	if err := eventStore.Upsert([]store.DetectionEvent{ev}); err != nil {
		t.Fatalf("Upsert event %s: %v", id, err)
	}

	if thumbPath == "" {
		return
	}
	if err := os.WriteFile(thumbPath, []byte("jpeg"), 0o644); err != nil {
		t.Fatalf("write thumbnail %s: %v", thumbPath, err)
	}
	stmt, _, err := db.Conn().Prepare(`UPDATE events SET thumb_ref = ? WHERE id = ?`)
	if err != nil {
		t.Fatalf("prepare set thumb_ref: %v", err)
	}
	defer stmt.Close()
	if err := stmt.BindText(1, thumbPath); err != nil {
		t.Fatal(err)
	}
	if err := stmt.BindText(2, id); err != nil {
		t.Fatal(err)
	}
	if err := stmt.Exec(); err != nil {
		t.Fatalf("set thumb_ref: %v", err)
	}
}

// eventExists reports whether id is still present in eventStore for
// cameraID.
func eventExists(t *testing.T, eventStore *store.EventStore, cameraID, id string) bool {
	t.Helper()
	result, err := eventStore.Query([]string{cameraID}, store.GetEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for _, ev := range result.Events {
		if ev.ID == id {
			return true
		}
	}
	return false
}

// vectorExists reports whether id has a row in backend, by querying for it
// with its own embedding and checking whether the top match is itself
// (VectorBackend has no direct "Get" — Query is the only read path).
func vectorExists(t *testing.T, backend store.VectorBackend, id string, embedding []float32) bool {
	t.Helper()
	matches, err := backend.Query(embedding, 10)
	if err != nil {
		t.Fatalf("Query vector: %v", err)
	}
	for _, m := range matches {
		if m.ID == id {
			return true
		}
	}
	return false
}

// newRetentionCamera returns a ManagedCamera whose recording config has the
// given retentionDays/nvrQuotaGB stored explicitly (bypassing the
// RecordingModeOff default — mode is set to continuous so it's a plausible
// managed camera, though RunRetentionOnce processes every entriesSnapshot
// camera regardless of mode).
func newRetentionCamera(id string, retentionDays int, nvrQuotaGB float64) *fakeCamera {
	cam := newFakeCamera(id, id, RecordingModeContinuous)
	cam.storage.set(keyRetentionDays, float64(retentionDays))
	cam.storage.set(keyNvrQuotaGB, nvrQuotaGB)
	return cam
}

// ---------------------------------------------------------------------------
// Age GC
// ---------------------------------------------------------------------------

// TestRunRetentionOnce_AgeGC_DeletesOnlyExpiredAndCascades proves
// RunRetentionOnce, given two cameras with different retentionDays: deletes
// only the segment rows+files past each camera's own cutoff, leaves
// within-retention rows+files untouched, and cascades an expired segment's
// camera to its fully-ended, now-expired events — removing their rows,
// thumbnail files, and vector rows — while leaving a still-within-retention
// event (and a still-active, never-ended event) alone.
func TestRunRetentionOnce_AgeGC_DeletesOnlyExpiredAndCascades(t *testing.T) {
	dir := t.TempDir()
	db, segStore, eventStore := openRetentionStores(t)

	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()
	cam1Cutoff := now - 1*msPerDay // cam1: retentionDays=1

	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{
		newRetentionCamera("cam1", 1, 0),
		newRetentionCamera("cam2", 10, 0),
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	m.ConfigureRetention(segStore, eventStore, db.ClipVectors, db.FaceVectors)

	// cam1: one segment safely past its 1-day cutoff, one well within it.
	oldSeg := addRetentionSegment(t, segStore, dir, "cam1", "main", cam1Cutoff-5000, cam1Cutoff-1000, 100)
	withinSeg := addRetentionSegment(t, segStore, dir, "cam1", "main", now-1000, now, 100)

	// cam2: a segment that's past cam1's cutoff but still within cam2's own
	// 10-day cutoff — proves retentionDays is applied per-camera, not
	// globally.
	cam2Seg := addRetentionSegment(t, segStore, dir, "cam2", "main", cam1Cutoff-5000, cam1Cutoff-1000, 100)

	// cam1 events: one fully ended before the cutoff (expired, with a
	// thumbnail + vector row to prove the cascade), one fully ended after it
	// (must survive), one never-ended/active event started long ago (must
	// survive regardless of age).
	expiredThumb := filepath.Join(dir, "expired-thumb.jpg")
	addRetentionEvent(t, db, eventStore, "expired-event", "cam1", cam1Cutoff-6000, cam1Cutoff-2000, expiredThumb)
	addRetentionEvent(t, db, eventStore, "recent-event", "cam1", now-2000, now-1000, "")
	activeEmbedding := []float32{0, 0, 1, 0}
	expiredEmbedding := []float32{1, 0, 0, 0}
	if err := db.ClipVectors.Upsert("expired-event", expiredEmbedding); err != nil {
		t.Fatalf("seed clip vector: %v", err)
	}
	if err := db.ClipVectors.Upsert("recent-event", activeEmbedding); err != nil {
		t.Fatalf("seed clip vector: %v", err)
	}
	// Active/never-ended event: EndTime left at 0 via a direct Upsert (not
	// addRetentionEvent, which always sets EndTime).
	if err := eventStore.Upsert([]store.DetectionEvent{{
		ID: "active-event", CameraID: "cam1", State: "active", StartTime: cam1Cutoff - 999999,
		Types: []string{"motion"},
	}}); err != nil {
		t.Fatalf("Upsert active event: %v", err)
	}

	if err := m.RunRetentionOnce(now); err != nil {
		t.Fatalf("RunRetentionOnce: %v", err)
	}

	// Segment expectations.
	if _, err := os.Stat(oldSeg.Path); !os.IsNotExist(err) {
		t.Errorf("expected expired cam1 segment file %s to be removed, stat err=%v", oldSeg.Path, err)
	}
	if _, err := os.Stat(withinSeg.Path); err != nil {
		t.Errorf("expected within-retention cam1 segment file %s to survive, stat err=%v", withinSeg.Path, err)
	}
	if _, err := os.Stat(cam2Seg.Path); err != nil {
		t.Errorf("expected cam2's equally-old-by-cam1-standards segment to survive its own 10-day retention, stat err=%v", cam2Seg.Path)
	}

	remainingCam1, err := segStore.AllByCamera("cam1")
	if err != nil {
		t.Fatal(err)
	}
	if len(remainingCam1) != 1 || remainingCam1[0].ID != withinSeg.ID {
		t.Errorf("expected only withinSeg to remain for cam1, got %+v", remainingCam1)
	}

	// Event expectations.
	if eventExists(t, eventStore, "cam1", "expired-event") {
		t.Errorf("expected expired-event to be deleted")
	}
	if !eventExists(t, eventStore, "cam1", "recent-event") {
		t.Errorf("expected recent-event to survive")
	}
	if !eventExists(t, eventStore, "cam1", "active-event") {
		t.Errorf("expected never-ended active-event to survive regardless of age")
	}

	// Cascade expectations: thumbnail file + vector row.
	if _, err := os.Stat(expiredThumb); !os.IsNotExist(err) {
		t.Errorf("expected expired event's thumbnail file to be removed, stat err=%v", err)
	}
	if vectorExists(t, db.ClipVectors, "expired-event", expiredEmbedding) {
		t.Errorf("expected expired-event's clip vector row to be deleted")
	}
	if !vectorExists(t, db.ClipVectors, "recent-event", activeEmbedding) {
		t.Errorf("expected recent-event's clip vector row to survive")
	}
}

// TestRunRetentionOnce_MissingSegmentFileToleratesGracefully proves a
// segment row whose file is already gone before RunRetentionOnce runs
// doesn't cause the pass to error out, and the row is still removed.
func TestRunRetentionOnce_MissingSegmentFileToleratesGracefully(t *testing.T) {
	dir := t.TempDir()
	_, segStore, eventStore := openRetentionStores(t)

	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()
	cutoff := now - 1*msPerDay

	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 1, 0)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore)

	seg := addRetentionSegment(t, segStore, dir, "cam1", "main", cutoff-5000, cutoff-1000, 100)
	if err := os.Remove(seg.Path); err != nil {
		t.Fatalf("pre-remove segment file: %v", err)
	}

	if err := m.RunRetentionOnce(now); err != nil {
		t.Fatalf("expected no error for an already-missing file, got: %v", err)
	}

	remaining, err := segStore.AllByCamera("cam1")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected the segment row to be removed despite its missing file, got %+v", remaining)
	}
}

// ---------------------------------------------------------------------------
// Disk-cap GC
// ---------------------------------------------------------------------------

// TestRunRetentionOnce_DiskCapGC_DeletesOldestFirstUntilUnderCap proves the
// disk-cap sweep, given usage over nvrQuotaGB, deletes the oldest segments
// first (fewest possible) until total usage is back under the cap, and
// leaves the newest segments (and anything already within age-based
// retention) alone.
func TestRunRetentionOnce_DiskCapGC_DeletesOldestFirstUntilUnderCap(t *testing.T) {
	dir := t.TempDir()
	db, segStore, eventStore := openRetentionStores(t)

	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()

	m := NewRecorderManager()
	// retentionDays large enough that age GC never fires in this test —
	// only the quota sweep should delete anything. quotaGB chosen so 5
	// segments of 1000 bytes each (5000 bytes total) exceed it, and
	// removing exactly the 2 oldest (2000 bytes) brings it back under.
	quotaGB := 3500.0 / bytesPerGB
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 365, quotaGB)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore, db.ClipVectors, db.FaceVectors)

	var segs []store.Segment
	for i := 0; i < 5; i++ {
		start := now - int64(5-i)*10000
		end := start + 500
		segs = append(segs, addRetentionSegment(t, segStore, dir, "cam1", "main", start, end, 1000))
	}

	if err := m.RunRetentionOnce(now); err != nil {
		t.Fatalf("RunRetentionOnce: %v", err)
	}

	for i, seg := range segs {
		_, err := os.Stat(seg.Path)
		if i < 2 {
			if !os.IsNotExist(err) {
				t.Errorf("expected oldest segment %d (%s) to be removed, stat err=%v", i, seg.Path, err)
			}
		} else {
			if err != nil {
				t.Errorf("expected newer segment %d (%s) to survive, stat err=%v", i, seg.Path, err)
			}
		}
	}

	remaining, err := segStore.AllByCamera("cam1")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 3 {
		t.Fatalf("expected 3 segments to remain under the cap, got %d: %+v", len(remaining), remaining)
	}
	for _, seg := range remaining {
		if seg.ID == segs[0].ID || seg.ID == segs[1].ID {
			t.Errorf("expected the two oldest segments to be gone, found id %d still present", seg.ID)
		}
	}
}

// TestRunRetentionOnce_DiskCapGC_NoOpWhenUnderQuota proves a camera whose
// usage is already under its configured nvrQuotaGB is left untouched by the
// quota sweep.
func TestRunRetentionOnce_DiskCapGC_NoOpWhenUnderQuota(t *testing.T) {
	dir := t.TempDir()
	db, segStore, eventStore := openRetentionStores(t)
	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()

	m := NewRecorderManager()
	quotaGB := 10000.0 / bytesPerGB // well above the 1000 bytes seeded below
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 365, quotaGB)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore, db.ClipVectors, db.FaceVectors)

	seg := addRetentionSegment(t, segStore, dir, "cam1", "main", now-5000, now-4500, 1000)

	if err := m.RunRetentionOnce(now); err != nil {
		t.Fatalf("RunRetentionOnce: %v", err)
	}

	if _, err := os.Stat(seg.Path); err != nil {
		t.Errorf("expected under-quota segment to survive, stat err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// Background ticker
// ---------------------------------------------------------------------------

// fakeTicker is a ticker whose channel/Stop are entirely test-controlled, so
// StartRetention's background loop can be driven deterministically (send a
// value to ch to fire a tick) without depending on any real elapsed
// wall-clock time.
type fakeTicker struct {
	ch        chan time.Time
	stopCalls chan struct{}
}

func newFakeTicker() *fakeTicker {
	return &fakeTicker{ch: make(chan time.Time), stopCalls: make(chan struct{}, 1)}
}
func (f *fakeTicker) C() <-chan time.Time { return f.ch }
func (f *fakeTicker) Stop()               { f.stopCalls <- struct{}{} }

// TestStartRetention_TickTriggersRunRetentionOnce proves StartRetention's
// background loop calls RunRetentionOnce, sourced from the injected clock,
// each time the (fake, test-driven) ticker fires — synchronized via the
// afterTick test hook rather than a sleep, so the assertion runs only once
// the tick's RunRetentionOnce call has actually completed.
func TestStartRetention_TickTriggersRunRetentionOnce(t *testing.T) {
	dir := t.TempDir()
	db, segStore, eventStore := openRetentionStores(t)

	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 1, 0)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore, db.ClipVectors, db.FaceVectors)

	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()
	cutoff := now - 1*msPerDay
	seg := addRetentionSegment(t, segStore, dir, "cam1", "main", cutoff-5000, cutoff-1000, 100)

	ft := newFakeTicker()
	tickDone := make(chan struct{})
	m.gc.newTicker = func(time.Duration) ticker { return ft }
	m.gc.afterTick = func() { close(tickDone) }

	if err := m.StartRetention(time.Hour, func() int64 { return now }); err != nil {
		t.Fatalf("StartRetention: %v", err)
	}
	defer m.StopRetention()

	select {
	case ft.ch <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out sending fake tick")
	}

	select {
	case <-tickDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ticker-driven RunRetentionOnce to complete")
	}

	if _, err := os.Stat(seg.Path); !os.IsNotExist(err) {
		t.Errorf("expected ticker-driven RunRetentionOnce to have removed the expired segment file, stat err=%v", err)
	}
}

// TestStartRetention_NoOpWhenNotConfigured proves StartRetention/
// StopRetention on a manager that never called ConfigureRetention are safe
// no-ops rather than a nil-pointer panic.
func TestStartRetention_NoOpWhenNotConfigured(t *testing.T) {
	m := NewRecorderManager()
	if err := m.StartRetention(time.Millisecond, nil); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	m.StopRetention() // must not panic
}

// TestStartRetention_SecondStartIsNoop proves calling StartRetention again
// while already running doesn't spawn a second ticker goroutine (which
// would otherwise leak past a single StopRetention call).
func TestStartRetention_SecondStartIsNoop(t *testing.T) {
	_, segStore, eventStore := openRetentionStores(t)
	m := NewRecorderManager()
	m.ConfigureRetention(segStore, eventStore)

	ft1 := newFakeTicker()
	m.gc.newTicker = func(time.Duration) ticker { return ft1 }
	if err := m.StartRetention(time.Hour, nil); err != nil {
		t.Fatal(err)
	}

	// Second Start must not replace the running ticker/goroutine.
	ft2 := newFakeTicker()
	m.gc.newTicker = func(time.Duration) ticker { return ft2 }
	if err := m.StartRetention(time.Hour, nil); err != nil {
		t.Fatal(err)
	}

	m.StopRetention()

	select {
	case <-ft1.stopCalls:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the original (first-started) ticker to have been stopped")
	}
}

// TestStopRetention_CancelsCleanlyWithoutLeaking proves StopRetention blocks
// until the ticker goroutine has actually exited (it stops the fake ticker
// before returning), and that calling it twice — or on a manager that was
// never started — is a safe no-op, proving no goroutine is left running
// past the first Stop.
func TestStopRetention_CancelsCleanlyWithoutLeaking(t *testing.T) {
	_, segStore, eventStore := openRetentionStores(t)
	m := NewRecorderManager()
	m.ConfigureRetention(segStore, eventStore)

	ft := newFakeTicker()
	m.gc.newTicker = func(time.Duration) ticker { return ft }
	if err := m.StartRetention(time.Hour, nil); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		m.StopRetention()
		close(done)
	}()

	select {
	case <-ft.stopCalls:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the ticker to be Stop()ped")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StopRetention did not return promptly after cancellation")
	}

	// Idempotent: a second Stop (goroutine already gone) and Stop on a
	// never-started manager must not hang or panic.
	m.StopRetention()

	m2 := NewRecorderManager()
	m2.StopRetention()
}
