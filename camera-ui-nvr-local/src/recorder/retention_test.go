package recorder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// given retentionDays stored explicitly (bypassing the RecordingModeOff
// default — mode is set to continuous so it's a plausible managed camera,
// though RunRetentionOnce processes every entriesSnapshot camera regardless
// of mode). There is no per-camera quota parameter: the disk cap is
// instance-wide (see retention.go's package doc comment) — tests that need
// one pass a quotaGB getter to ConfigureRetention directly (fixedQuota,
// below), not through camera config.
func newRetentionCamera(id string, retentionDays int) *fakeCamera {
	cam := newFakeCamera(id, id, RecordingModeContinuous)
	cam.storage.set(keyRetentionDays, float64(retentionDays))
	return cam
}

// fixedQuota returns a quotaGB getter (ConfigureRetention's third argument)
// that always reports gb — the test equivalent of a fixed, never-changing
// instance-wide disk cap setting.
func fixedQuota(gb float64) func() float64 {
	return func() float64 { return gb }
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
		newRetentionCamera("cam1", 1),
		newRetentionCamera("cam2", 10),
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	m.ConfigureRetention(segStore, eventStore, "", nil, db.ClipVectors, db.FaceVectors)

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
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 1)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore, "", nil)

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
// Orphan file sweep (Task 9 review fix: enforceInstanceQuota only ever
// counted/evicted INDEXED segments — a crashed/restarted recorder can leave
// a finished .mp4 on disk with no segments-table row at all, invisible to
// both the disk-cap computation and every row-driven delete path, so actual
// disk usage silently exceeds the tracked/enforced total).
// ---------------------------------------------------------------------------

// writeOrphanFile writes a *.mp4 file directly under
// "<recordingsDir>/recordings/<cameraID>" (mirroring Recorder.outDir's own
// "<DataDir>/recordings/<cameraId>/..." layout, recorder.go) with NO
// corresponding segments-table row, then backdates its mtime to atMs via
// os.Chtimes — so sweepOrphanFiles' grace-period check has a deterministic,
// test-controlled age to compare against nowMs, independent of the real
// wall-clock time the test happens to run at.
func writeOrphanFile(t *testing.T, recordingsDir, cameraID, name string, atMs int64) string {
	t.Helper()

	camDir := filepath.Join(recordingsDir, "recordings", cameraID)
	if err := os.MkdirAll(camDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", camDir, err)
	}
	path := filepath.Join(camDir, name)
	if err := os.WriteFile(path, make([]byte, 100), 0o644); err != nil {
		t.Fatalf("write orphan file %s: %v", path, err)
	}
	at := time.UnixMilli(atMs)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	return path
}

// TestRunRetentionOnce_OrphanSweep_RemovesOnlyOldUnindexedFiles is the FIX A
// regression/RED-proof test: seeds a recordings tree with (a) a tracked
// file that DOES have a matching segments-table row, (b) an old, unindexed
// (orphaned) file well past orphanGrace, and (c) a very recent unindexed
// file still within orphanGrace — and proves RunRetentionOnce removes only
// (b), leaving (a) and (c) untouched.
func TestRunRetentionOnce_OrphanSweep_RemovesOnlyOldUnindexedFiles(t *testing.T) {
	recordingsDir := t.TempDir()
	db, segStore, eventStore := openRetentionStores(t)

	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()

	m := NewRecorderManager()
	// retentionDays large enough that age GC never fires here — this test
	// exercises only the orphan sweep.
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 365)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore, recordingsDir, nil, db.ClipVectors, db.FaceVectors)

	// (a) tracked: a real file with a matching segment row — must survive
	// regardless of age or mtime.
	trackedPath := filepath.Join(recordingsDir, "recordings", "cam1", "tracked.mp4")
	if err := os.MkdirAll(filepath.Dir(trackedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trackedPath, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := segStore.Add(store.Segment{CameraID: "cam1", Role: "main", Path: trackedPath, StartMs: now - 5000, EndMs: now - 4000, HasVideo: true, Codec: "h264"}); err != nil {
		t.Fatalf("Add tracked segment: %v", err)
	}

	// (b) old orphan: no segment row, mtime well past orphanGrace — must be
	// removed.
	oldOrphan := writeOrphanFile(t, recordingsDir, "cam1", "orphan-old.mp4", now-int64(30*time.Minute/time.Millisecond))

	// (c) recent orphan: no segment row, mtime within orphanGrace (written
	// "just now" relative to nowMs) — must survive this pass (might still
	// be being written or awaiting finalization/indexing).
	recentOrphan := writeOrphanFile(t, recordingsDir, "cam1", "orphan-recent.mp4", now-int64(1*time.Minute/time.Millisecond))

	if err := m.RunRetentionOnce(now); err != nil {
		t.Fatalf("RunRetentionOnce: %v", err)
	}

	if _, err := os.Stat(trackedPath); err != nil {
		t.Errorf("expected tracked (indexed) file to survive, stat err=%v", err)
	}
	if _, err := os.Stat(oldOrphan); !os.IsNotExist(err) {
		t.Errorf("expected old orphan file %s to be removed, stat err=%v", oldOrphan, err)
	}
	if _, err := os.Stat(recentOrphan); err != nil {
		t.Errorf("expected recent orphan file %s (within grace) to survive, stat err=%v", recentOrphan, err)
	}
}

// TestRunRetentionOnce_OrphanSweep_LogsRemovalSummary proves an orphan-sweep
// pass that actually removes something reports one summary line (count +
// GB freed) via gc.logf, the same low-noise-logging convention the age/
// disk-cap GC passes already established.
func TestRunRetentionOnce_OrphanSweep_LogsRemovalSummary(t *testing.T) {
	recordingsDir := t.TempDir()
	_, segStore, eventStore := openRetentionStores(t)

	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()

	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 365)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore, recordingsDir, nil)

	var logs []string
	m.gc.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	writeOrphanFile(t, recordingsDir, "cam1", "orphan-old.mp4", now-int64(30*time.Minute/time.Millisecond))

	if err := m.RunRetentionOnce(now); err != nil {
		t.Fatalf("RunRetentionOnce: %v", err)
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "orphan sweep") && strings.Contains(l, "1") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a log line mentioning the orphan-sweep removal count, got %v", logs)
	}
}

// TestRunRetentionOnce_OrphanSweep_NoOpWhenRecordingsDirUnset proves a
// manager configured with recordingsDir == "" (e.g. db failed to open in
// NewPlugin, or a test/config that never set one) never touches disk for
// the orphan sweep at all — RunRetentionOnce must not error or panic.
func TestRunRetentionOnce_OrphanSweep_NoOpWhenRecordingsDirUnset(t *testing.T) {
	_, segStore, eventStore := openRetentionStores(t)
	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()

	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 365)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore, "", nil)

	if err := m.RunRetentionOnce(now); err != nil {
		t.Fatalf("expected nil error with recordingsDir unset, got %v", err)
	}
}

// TestRunRetentionOnce_OrphanSweep_TOCTOU_FreshlyIndexedFileSurvives is the
// CRITICAL data-loss regression test a review caught in this sweep's first
// cut: knownPaths (segStore.AllPaths()) is a point-in-time snapshot taken
// once, before the filesystem walk — a segment can be finalized and indexed
// (the crash-then-restart-then-reindex case the grace period exists to
// protect) AFTER that snapshot but BEFORE the walk visits its file, while
// its mtime is already old enough to clear orphanGrace (it was written
// before the crash). Using gc.afterOrphanSnapshot (a test-only hook, see its
// doc comment) to insert the segment row for candidate.mp4's path exactly
// in that window — after the snapshot, before the walk — simulates this
// race deterministically. The fix (a fresh, unconditional
// segStore.HasPath(path) recheck immediately before the delete decision,
// never trusting the stale snapshot) must see the row and leave the file
// alone.
func TestRunRetentionOnce_OrphanSweep_TOCTOU_FreshlyIndexedFileSurvives(t *testing.T) {
	recordingsDir := t.TempDir()
	_, segStore, eventStore := openRetentionStores(t)

	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()

	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 365)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore, recordingsDir, nil)

	// Absent from the AllPaths() snapshot at the moment it's taken (no
	// segment row exists yet) — mtime is old enough to clear orphanGrace, the
	// exact shape of a segment written before a crash and only indexed once
	// the recorder restarts and catches up.
	candidate := writeOrphanFile(t, recordingsDir, "cam1", "candidate.mp4", now-int64(30*time.Minute/time.Millisecond))

	m.gc.afterOrphanSnapshot = func() {
		if _, err := segStore.Add(store.Segment{
			CameraID: "cam1", Role: "main", Path: candidate,
			StartMs: now - 40000, EndMs: now - 30000, HasVideo: true, Codec: "h264",
		}); err != nil {
			t.Fatalf("simulate concurrent index of candidate.mp4: %v", err)
		}
	}

	if err := m.RunRetentionOnce(now); err != nil {
		t.Fatalf("RunRetentionOnce: %v", err)
	}

	if _, err := os.Stat(candidate); err != nil {
		t.Errorf("expected the freshly-indexed file to survive (TOCTOU fix), stat err=%v", err)
	}
}

// TestRunRetentionOnce_OrphanSweep_SkipsFilesUnderActiveRecorderOutputDir
// proves a file under a directory RecorderManager.ActiveOutputDirs()
// reports as currently owned by a live recorder is never swept, even when
// its mtime is old enough to otherwise clear orphanGrace — the defense
// against a stalled-but-still-open segment (e.g. an RTSP source hang lasting
// longer than the grace period while ffmpeg still holds the file's fd open)
// being misclassified as an orphan by the mtime heuristic alone.
func TestRunRetentionOnce_OrphanSweep_SkipsFilesUnderActiveRecorderOutputDir(t *testing.T) {
	recordingsDir := t.TempDir()
	_, segStore, eventStore := openRetentionStores(t)

	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()

	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 365)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore, recordingsDir, nil)

	activeDir := filepath.Join(recordingsDir, "recordings", "cam1")
	m.ConfigureRecording(recordingsDir, 0, func(cfg RecorderConfig) RecorderHandle {
		return fakeActiveDirHandle{dirs: []string{activeDir}}
	})
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	defer m.StopAll()

	stalled := writeOrphanFile(t, recordingsDir, "cam1", "stalled.mp4", now-int64(30*time.Minute/time.Millisecond))

	if err := m.RunRetentionOnce(now); err != nil {
		t.Fatalf("RunRetentionOnce: %v", err)
	}

	if _, err := os.Stat(stalled); err != nil {
		t.Errorf("expected the file under the active recorder output dir to survive despite being past grace, stat err=%v", err)
	}
}

// fakeActiveDirHandle is a RecorderHandle that also reports a fixed set of
// "currently active" output directories, simulating a live *recorder.
// Recorder for TestRunRetentionOnce_OrphanSweep_SkipsFilesUnderActiveRecorderOutputDir
// without spawning a real ffmpeg process.
type fakeActiveDirHandle struct{ dirs []string }

func (f fakeActiveDirHandle) Start(context.Context) error { return nil }
func (f fakeActiveDirHandle) Stop() error                 { return nil }
func (f fakeActiveDirHandle) ActiveOutputDirs() []string  { return f.dirs }

// ---------------------------------------------------------------------------
// Disk-cap GC
// ---------------------------------------------------------------------------

// TestRunRetentionOnce_DiskCapGC_DeletesOldestFirstAcrossCamerasUntilUnderCap
// proves the disk-cap sweep is instance-wide, not per-camera: given two
// cameras whose combined usage exceeds a single configured quotaGB, it
// deletes the globally oldest segments first — regardless of which camera
// they belong to — until total usage across both cameras is back under the
// one cap, and leaves every newer segment (on either camera) alone. Segment
// start times are interleaved across the two cameras specifically so a
// per-camera (rather than truly global) implementation would produce a
// different result here.
func TestRunRetentionOnce_DiskCapGC_DeletesOldestFirstAcrossCamerasUntilUnderCap(t *testing.T) {
	dir := t.TempDir()
	db, segStore, eventStore := openRetentionStores(t)

	m := NewRecorderManager()
	// retentionDays large enough that age GC never fires in this test — only
	// the instance-wide quota sweep should delete anything.
	if err := m.Configure([]ManagedCamera{
		newRetentionCamera("cam1", 365),
		newRetentionCamera("cam2", 365),
	}); err != nil {
		t.Fatal(err)
	}
	// 6 segments of 1000 bytes each (6000 bytes total, across both cameras)
	// against a 4500-byte cap: removing the 2 globally-oldest (2000 bytes)
	// brings total usage to 4000, back under the cap.
	quotaGB := 4500.0 / bytesPerGB
	m.ConfigureRetention(segStore, eventStore, "", fixedQuota(quotaGB), db.ClipVectors, db.FaceVectors)

	// All six segments are recent relative to "now" (well within the
	// 365-day age cutoff, so age GC never touches them) — every timestamp
	// below is now-relative, not epoch-relative, precisely so this test
	// exercises only the quota sweep.
	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()

	// Interleaved by start time across the two cameras: cam1Oldest is the
	// globally oldest, cam2Oldest the second-oldest — one segment from EACH
	// camera, not two from the same one.
	cam1Oldest := addRetentionSegment(t, segStore, dir, "cam1", "main", now-100000, now-99995, 1000)
	cam2Oldest := addRetentionSegment(t, segStore, dir, "cam2", "main", now-90000, now-89995, 1000)
	cam1Mid := addRetentionSegment(t, segStore, dir, "cam1", "main", now-60000, now-59995, 1000)
	cam2Mid := addRetentionSegment(t, segStore, dir, "cam2", "main", now-50000, now-49995, 1000)
	cam1Newest := addRetentionSegment(t, segStore, dir, "cam1", "main", now-20000, now-19995, 1000)
	cam2Newest := addRetentionSegment(t, segStore, dir, "cam2", "main", now-10000, now-9995, 1000)

	if err := m.RunRetentionOnce(now); err != nil {
		t.Fatalf("RunRetentionOnce: %v", err)
	}

	removed := []store.Segment{cam1Oldest, cam2Oldest}
	kept := []store.Segment{cam1Mid, cam2Mid, cam1Newest, cam2Newest}
	for _, seg := range removed {
		if _, err := os.Stat(seg.Path); !os.IsNotExist(err) {
			t.Errorf("expected globally-oldest segment %s (camera %s) to be removed, stat err=%v", seg.Path, seg.CameraID, err)
		}
	}
	for _, seg := range kept {
		if _, err := os.Stat(seg.Path); err != nil {
			t.Errorf("expected segment %s (camera %s) to survive, stat err=%v", seg.Path, seg.CameraID, err)
		}
	}

	remainingCam1, err := segStore.AllByCamera("cam1")
	if err != nil {
		t.Fatal(err)
	}
	if len(remainingCam1) != 2 {
		t.Errorf("expected 2 cam1 segments to remain, got %d: %+v", len(remainingCam1), remainingCam1)
	}
	remainingCam2, err := segStore.AllByCamera("cam2")
	if err != nil {
		t.Fatal(err)
	}
	if len(remainingCam2) != 2 {
		t.Errorf("expected 2 cam2 segments to remain, got %d: %+v", len(remainingCam2), remainingCam2)
	}
}

// TestRunRetentionOnce_DiskCapGC_NoOpWhenUnderQuota proves cameras whose
// combined usage is already under the configured instance-wide quotaGB are
// left untouched by the quota sweep.
func TestRunRetentionOnce_DiskCapGC_NoOpWhenUnderQuota(t *testing.T) {
	dir := t.TempDir()
	db, segStore, eventStore := openRetentionStores(t)
	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()

	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{
		newRetentionCamera("cam1", 365),
		newRetentionCamera("cam2", 365),
	}); err != nil {
		t.Fatal(err)
	}
	quotaGB := 10000.0 / bytesPerGB // well above the 2000 bytes seeded below
	m.ConfigureRetention(segStore, eventStore, "", fixedQuota(quotaGB), db.ClipVectors, db.FaceVectors)

	seg1 := addRetentionSegment(t, segStore, dir, "cam1", "main", now-5000, now-4500, 1000)
	seg2 := addRetentionSegment(t, segStore, dir, "cam2", "main", now-5000, now-4500, 1000)

	if err := m.RunRetentionOnce(now); err != nil {
		t.Fatalf("RunRetentionOnce: %v", err)
	}

	if _, err := os.Stat(seg1.Path); err != nil {
		t.Errorf("expected under-quota cam1 segment to survive, stat err=%v", err)
	}
	if _, err := os.Stat(seg2.Path); err != nil {
		t.Errorf("expected under-quota cam2 segment to survive, stat err=%v", err)
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
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 1)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore, "", nil, db.ClipVectors, db.FaceVectors)

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
	m.ConfigureRetention(segStore, eventStore, "", nil)

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
	m.ConfigureRetention(segStore, eventStore, "", nil)

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

// ---------------------------------------------------------------------------
// Production bug fix: immediate startup run, shorter default interval,
// eviction logging (see retention.go's defaultRetentionInterval,
// StartRetention, and retentionGC.logf/warnf doc comments).
// ---------------------------------------------------------------------------

// TestDefaultRetentionInterval_Is10Minutes proves the ticker period was
// tightened from an hour to 10 minutes — production bug fix: at sustained
// high-ingest rates (~1.5GB/min observed), an hourly pass let disk usage
// overshoot a configured nvrQuotaGB cap by up to ~90GB before the next pass
// clawed it back. 10 minutes bounds the same overshoot to ~15GB.
func TestDefaultRetentionInterval_Is10Minutes(t *testing.T) {
	if defaultRetentionInterval != 10*time.Minute {
		t.Fatalf("expected defaultRetentionInterval == 10m, got %v", defaultRetentionInterval)
	}
}

// TestStartRetention_RunsImmediatelyAtStartup proves StartRetention's
// background loop runs one RunRetentionOnce pass immediately at startup,
// before ever waiting on a tick — production bug fix: previously GC only
// ran on each hourly tick, so a restart anywhere in that hour reset the
// timer and could leave the quota unenforced indefinitely under frequent
// restarts. The fake ticker's channel is never sent to, so this only passes
// if the immediate pass (not a tick) removed the segment.
func TestStartRetention_RunsImmediatelyAtStartup(t *testing.T) {
	dir := t.TempDir()
	_, segStore, eventStore := openRetentionStores(t)

	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()
	cutoff := now - 1*msPerDay

	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 1)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore, "", nil)

	seg := addRetentionSegment(t, segStore, dir, "cam1", "main", cutoff-5000, cutoff-1000, 100)

	ft := newFakeTicker()
	immediateDone := make(chan struct{})
	m.gc.newTicker = func(time.Duration) ticker { return ft }
	m.gc.afterImmediate = func() { close(immediateDone) }

	if err := m.StartRetention(time.Hour, func() int64 { return now }); err != nil {
		t.Fatalf("StartRetention: %v", err)
	}
	defer m.StopRetention()

	select {
	case <-immediateDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the immediate startup gc pass to complete (zero ticks delivered)")
	}

	if _, err := os.Stat(seg.Path); !os.IsNotExist(err) {
		t.Errorf("expected the immediate startup gc pass to have removed the expired segment file, stat err=%v", err)
	}
}

// TestStartRetention_StopImmediatelyAfterStartDoesNotHangOrPanic proves
// calling StopRetention right after StartRetention — racing the immediate
// startup gc pass — neither hangs nor panics, and leaves no goroutine
// running (StopRetention blocks on the ticker goroutine's done channel).
// Run with -race to confirm no data race on the immediate-run guard.
func TestStartRetention_StopImmediatelyAfterStartDoesNotHangOrPanic(t *testing.T) {
	_, segStore, eventStore := openRetentionStores(t)
	m := NewRecorderManager()
	m.ConfigureRetention(segStore, eventStore, "", nil)

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
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StopRetention did not return promptly after racing the immediate startup gc pass")
	}
}

// TestRunRetentionOnce_AgeGC_LogsEvictionSummary proves an age-GC pass that
// actually evicts something reports one low-noise summary line via gc.logf
// (segment count + GB freed) — production bug fix: retention previously had
// no logger at all, so evictions were invisible to operators.
func TestRunRetentionOnce_AgeGC_LogsEvictionSummary(t *testing.T) {
	dir := t.TempDir()
	_, segStore, eventStore := openRetentionStores(t)

	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()
	cutoff := now - 1*msPerDay

	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 1)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore, "", nil)

	var logs []string
	m.gc.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	addRetentionSegment(t, segStore, dir, "cam1", "main", cutoff-5000, cutoff-1000, 100)

	if err := m.RunRetentionOnce(now); err != nil {
		t.Fatalf("RunRetentionOnce: %v", err)
	}

	if len(logs) == 0 {
		t.Fatal("expected at least one summary log line for an age-gc pass that evicted a segment")
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "age gc") && strings.Contains(l, "1") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a log line mentioning the age-gc eviction count, got %v", logs)
	}
}

// TestRunRetentionOnce_DiskCapGC_LogsUsageAndEvictionSummary proves a
// disk-cap pass reports a per-pass usage-vs-quota summary line, and — when
// over cap — a separate eviction-batch summary line (segments + GB freed),
// via gc.logf.
func TestRunRetentionOnce_DiskCapGC_LogsUsageAndEvictionSummary(t *testing.T) {
	dir := t.TempDir()
	db, segStore, eventStore := openRetentionStores(t)

	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 365)}); err != nil {
		t.Fatal(err)
	}
	quotaGB := 1500.0 / bytesPerGB
	m.ConfigureRetention(segStore, eventStore, "", fixedQuota(quotaGB), db.ClipVectors, db.FaceVectors)

	var logs []string
	m.gc.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()
	addRetentionSegment(t, segStore, dir, "cam1", "main", now-60000, now-59995, 1000)
	addRetentionSegment(t, segStore, dir, "cam1", "main", now-20000, now-19995, 1000)

	if err := m.RunRetentionOnce(now); err != nil {
		t.Fatalf("RunRetentionOnce: %v", err)
	}

	var sawUsage, sawEviction bool
	for _, l := range logs {
		if strings.Contains(l, "quota") {
			sawUsage = true
		}
		if strings.Contains(l, "disk-cap") && strings.Contains(l, "evicted") {
			sawEviction = true
		}
	}
	if !sawUsage {
		t.Errorf("expected a usage-vs-quota summary log line, got %v", logs)
	}
	if !sawEviction {
		t.Errorf("expected a disk-cap eviction-batch summary log line, got %v", logs)
	}
}

// TestStartRetention_WarnsWhenScheduledPassErrors proves StartRetention's
// background loop reports (via gc.warnf) an error returned from
// RunRetentionOnce instead of silently discarding it (production bug fix:
// the ticker loop previously did `_ = m.RunRetentionOnce(...)`, discarding
// any error outright).
func TestStartRetention_WarnsWhenScheduledPassErrors(t *testing.T) {
	dir := t.TempDir()
	_, segStore, eventStore := openRetentionStores(t)

	now := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC).UnixMilli()
	cutoff := now - 1*msPerDay

	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newRetentionCamera("cam1", 1)}); err != nil {
		t.Fatal(err)
	}
	m.ConfigureRetention(segStore, eventStore, "", nil)

	// Force RunRetentionOnce's age gc to fail on a file-removal error
	// (rather than an already-missing file, which is tolerated): the
	// segment's containing directory is made unwritable, so os.Remove on
	// the still-present file fails with a permission error. Restored so
	// t.TempDir()'s own cleanup can still remove it.
	addRetentionSegment(t, segStore, dir, "cam1", "main", cutoff-5000, cutoff-1000, 100)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	var warnings []string
	warnDone := make(chan struct{})
	m.gc.warnf = func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
		close(warnDone)
	}
	ft := newFakeTicker()
	m.gc.newTicker = func(time.Duration) ticker { return ft }

	if err := m.StartRetention(time.Hour, nil); err != nil {
		t.Fatalf("StartRetention: %v", err)
	}
	defer m.StopRetention()

	select {
	case <-warnDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for StartRetention to warn about the immediate pass's error")
	}

	if len(warnings) == 0 {
		t.Fatal("expected at least one warning to be logged")
	}
}
