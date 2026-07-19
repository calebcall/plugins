package recorder

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// newEventsModeRecorder returns a Recorder configured for RecordingModeEvents
// against a fresh segStore, with the given pre/post roll (seconds). It never
// runs ffmpeg — every test in this file drives MarkEvent/promoteIfCovered/
// sweepEventSpool directly against segments seeded straight into segStore,
// simulating "a sequence of finalized segments" (the brief's rolling
// buffer) without any real recording. Its clock (r.nowFn) defaults to the
// real wall clock; tests that need deterministic time override it directly.
func newEventsModeRecorder(t *testing.T, segStore *store.SegmentStore, preRollS, postRollS int) *Recorder {
	t.Helper()
	cfg := RecorderConfig{
		CameraID:  "cam1",
		Roles:     []string{"high"},
		DataDir:   t.TempDir(),
		Mode:      RecordingModeEvents,
		PreRollS:  preRollS,
		PostRollS: postRollS,
	}
	return NewRecorder(cfg, segStore, &FFmpeg{}, nil)
}

// fixedClock returns a nowFn that always reports ms, for tests that need to
// drive Recorder.nowFn deterministically rather than depending on real
// elapsed wall-clock time.
func fixedClock(ms int64) func() int64 {
	return func() int64 { return ms }
}

// seedSegment inserts a segment row for cam1/high covering [startMs, endMs)
// with the given Referenced value, and writes a real (empty) file at its
// Path so sweepEventSpool's file-removal side can be observed. The filename
// is startMs plus a random suffix (via os.CreateTemp) rather than bare
// startMs, specifically so two segments sharing a StartMs/EndMs (as
// staleButKept and stale do, in the janitor test below, to prove age alone
// doesn't matter) never collide on the same underlying file — the filename
// otherwise carries no meaning to the code under test (unlike recorder.go's
// real segments, whose epoch-second name IS load-bearing — see
// segmentTimeRange).
func seedSegment(t *testing.T, dir string, segStore *store.SegmentStore, startMs, endMs int64, referenced bool) store.Segment {
	t.Helper()

	f, err := os.CreateTemp(dir, fmt.Sprintf("%d-*.mp4", startMs))
	if err != nil {
		t.Fatalf("seed segment file: %v", err)
	}
	path := f.Name()
	if _, err := f.WriteString("x"); err != nil {
		f.Close()
		t.Fatalf("seed segment file %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("seed segment file %s: %v", path, err)
	}

	seg := store.Segment{
		CameraID:   "cam1",
		Role:       "high",
		Path:       path,
		StartMs:    startMs,
		EndMs:      endMs,
		Referenced: referenced,
	}
	id, err := segStore.Add(seg)
	if err != nil {
		t.Fatalf("seed segment: Add: %v", err)
	}
	seg.ID = id
	return seg
}

// segmentReferenced looks up seg.ID's current Referenced value by querying
// InRange over a window guaranteed to include it, so tests can assert on
// promotion without a dedicated store accessor.
func segmentReferenced(t *testing.T, segStore *store.SegmentStore, seg store.Segment) bool {
	t.Helper()
	got, err := segStore.InRange(seg.CameraID, seg.Role, seg.StartMs, seg.EndMs)
	if err != nil {
		t.Fatalf("InRange: %v", err)
	}
	for _, s := range got {
		if s.ID == seg.ID {
			return s.Referenced
		}
	}
	t.Fatalf("segment id %d not found via InRange(%d,%d)", seg.ID, seg.StartMs, seg.EndMs)
	return false
}

// segmentExists reports whether seg.ID is still present in the store, by the
// same InRange lookup segmentReferenced uses.
func segmentExists(t *testing.T, segStore *store.SegmentStore, seg store.Segment) bool {
	t.Helper()
	got, err := segStore.InRange(seg.CameraID, seg.Role, seg.StartMs, seg.EndMs)
	if err != nil {
		t.Fatalf("InRange: %v", err)
	}
	for _, s := range got {
		if s.ID == seg.ID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// MarkEvent: promotion (single-message / already-final windows)
// ---------------------------------------------------------------------------

// TestRecorder_MarkEvent_PromotesOnlySegmentsCoveringWindow proves MarkEvent,
// given a rolling buffer of finalized (but not yet referenced) segments and
// a single already-terminal message (endMs > 0, as if it were the only
// message received for this event), promotes exactly the segments
// overlapping [start-preRoll, end+postRoll] and leaves every other segment
// untouched (still unreferenced).
func TestRecorder_MarkEvent_PromotesOnlySegmentsCoveringWindow(t *testing.T) {
	segStore := newTestSegmentStore(t)
	dir := t.TempDir()
	r := newEventsModeRecorder(t, segStore, 0, 0)

	// Five 9999ms segments, back to back with a 1ms gap between each so
	// boundary-touching is unambiguous: [0,9999], [10000,19999],
	// [20000,29999], [30000,39999], [40000,49999].
	seg0 := seedSegment(t, dir, segStore, 0, 9999, false)
	seg1 := seedSegment(t, dir, segStore, 10000, 19999, false)
	seg2 := seedSegment(t, dir, segStore, 20000, 29999, false)
	seg3 := seedSegment(t, dir, segStore, 30000, 39999, false)
	seg4 := seedSegment(t, dir, segStore, 40000, 49999, false)

	// An event spanning exactly the seg1/seg2 boundary, with zero pre/post
	// roll, overlaps only seg1 and seg2.
	r.MarkEvent("evt-1", 19000, 21000)

	if segmentReferenced(t, segStore, seg0) {
		t.Errorf("seg0 (ends before the event window) should not be promoted")
	}
	if !segmentReferenced(t, segStore, seg1) {
		t.Errorf("seg1 (overlaps the event window) should be promoted")
	}
	if !segmentReferenced(t, segStore, seg2) {
		t.Errorf("seg2 (overlaps the event window) should be promoted")
	}
	if segmentReferenced(t, segStore, seg3) {
		t.Errorf("seg3 (starts after the event window) should not be promoted")
	}
	if segmentReferenced(t, segStore, seg4) {
		t.Errorf("seg4 (starts after the event window) should not be promoted")
	}
}

// TestRecorder_MarkEvent_RetainsSegmentStartingAtPreRollEdge is the brief's
// required pre-roll edge case: an event at time T must retain the segment
// that started at exactly T-preRoll, and must NOT retain a segment that
// ended just before that boundary.
func TestRecorder_MarkEvent_RetainsSegmentStartingAtPreRollEdge(t *testing.T) {
	segStore := newTestSegmentStore(t)
	dir := t.TempDir()

	const preRollS = 12 // 12s pre-roll
	r := newEventsModeRecorder(t, segStore, preRollS, 0)

	// justBefore ends at 19999, one millisecond short of the pre-roll
	// boundary (20000) that an event at T=32000 with a 12s pre-roll opens
	// up. atEdge starts at exactly that boundary (20000 == 32000-12000).
	justBefore := seedSegment(t, dir, segStore, 10000, 19999, false)
	atEdge := seedSegment(t, dir, segStore, 20000, 29999, false)
	// current covers the event's own instant (32000) and must be retained
	// regardless of pre-roll, just to prove the window's other (right) edge
	// isn't what's being tested here.
	current := seedSegment(t, dir, segStore, 30000, 39999, false)

	const eventAtMs = 32000
	r.MarkEvent("evt-1", eventAtMs, eventAtMs)

	if segmentReferenced(t, segStore, justBefore) {
		t.Errorf("segment ending at 19999 is entirely before T-preRoll (20000) and must not be retained")
	}
	if !segmentReferenced(t, segStore, atEdge) {
		t.Errorf("segment starting exactly at T-preRoll (20000) must be retained (pre-roll edge, inclusive)")
	}
	if !segmentReferenced(t, segStore, current) {
		t.Errorf("segment covering the event's own instant must be retained")
	}
}

// TestRecorder_MarkEvent_NoOpOutsideEventsMode proves MarkEvent does nothing
// — not even querying the store — for a Recorder in continuous mode (or any
// non-events mode, including the zero value), matching "everything already
// retained" in continuous mode: finalizeSegment already indexes every
// continuous-mode segment Referenced=true, so there is nothing left to
// promote and MarkEvent must not error or panic against a Recorder with no
// segStore at all.
func TestRecorder_MarkEvent_NoOpOutsideEventsMode(t *testing.T) {
	segStore := newTestSegmentStore(t)
	dir := t.TempDir()

	cfg := RecorderConfig{
		CameraID:  "cam1",
		Roles:     []string{"high"},
		DataDir:   t.TempDir(),
		Mode:      RecordingModeContinuous,
		PreRollS:  5,
		PostRollS: 5,
	}
	r := NewRecorder(cfg, segStore, &FFmpeg{}, nil)

	// A continuous-mode segment is always indexed Referenced=true (see
	// initiallyReferenced); seed it that way to prove MarkEvent leaves it be.
	seg := seedSegment(t, dir, segStore, 0, 9999, true)

	r.MarkEvent("evt-1", 0, 0)

	if !segmentReferenced(t, segStore, seg) {
		t.Errorf("continuous-mode segment must remain referenced after MarkEvent")
	}

	// A Recorder with Mode unset (zero value) and no segStore at all must
	// not panic either — this is the "no-op" half of MarkEvent's contract
	// taken to its limit.
	bare := NewRecorder(RecorderConfig{CameraID: "cam2"}, nil, &FFmpeg{}, nil)
	bare.MarkEvent("evt-2", 1000, 2000)
}

// ---------------------------------------------------------------------------
// postRollWindowMs grace (Task ORCH's fix for the Task-8-deferred edge): when
// SegmentSeconds >= PostRollS, a tail post-roll segment might not finalize
// until after the un-padded window would already have closed. These tests
// drive promoteIfCovered directly (the call site that would otherwise skip
// such a segment via its "!w.open" guard) to prove the padded window keeps
// it eligible, and that no such padding is applied (or needed) when segments
// are short relative to the configured post-roll.
// ---------------------------------------------------------------------------

// TestRecorder_PostRollGrace_KeepsLateFinalizedSegmentEligible covers the
// case the brief calls out: SegmentSeconds (60s) at least as large as
// PostRollS (5s) means a segment finalized well after eventEnd+PostRollS
// (but still within eventEnd+PostRollS+SegmentSeconds) must still be
// promoted — the tail segment isn't orphaned just because ffmpeg hadn't
// rotated to a new file yet when the un-padded window would have closed.
func TestRecorder_PostRollGrace_KeepsLateFinalizedSegmentEligible(t *testing.T) {
	segStore := newTestSegmentStore(t)
	dir := t.TempDir()

	const preRollS = 0
	const postRollS = 5
	const segmentSeconds = 60
	cfg := RecorderConfig{
		CameraID:       "cam1",
		Roles:          []string{"high"},
		DataDir:        t.TempDir(),
		Mode:           RecordingModeEvents,
		PreRollS:       preRollS,
		PostRollS:      postRollS,
		SegmentSeconds: segmentSeconds,
	}
	r := NewRecorder(cfg, segStore, &FFmpeg{}, nil)

	const eventStart = int64(100_000)
	const eventEnd = int64(105_000) // eventStart + 5s

	clockMs := eventEnd
	r.nowFn = func() int64 { return clockMs }
	r.MarkEvent("evt-1", eventStart, eventEnd)

	// Un-padded, the window would close at eventEnd+postRollS = 110000.
	// Advance well past that (150000) but still within the padded window
	// (eventEnd + postRollS*1000 + segmentSeconds*1000 = 165000) before the
	// tail segment is finalized — simulating ffmpeg only rotating to a new
	// segment file 45s after the event ended, well within its 60s segment
	// length.
	clockMs = 150_000
	tailSeg := seedSegment(t, dir, segStore, eventEnd+1_000, eventEnd+2_000, false)
	r.promoteIfCovered(tailSeg)

	if !segmentReferenced(t, segStore, tailSeg) {
		t.Fatalf("tail post-roll segment finalized after the un-padded window would have closed (but within the SegmentSeconds grace) must still be promoted")
	}
}

// TestRecorder_PostRollGrace_NotAppliedWhenSegmentsAreShort proves the grace
// is conditional: when SegmentSeconds (2s) is comfortably smaller than
// PostRollS (5s), a segment finalized well after the window's un-padded
// close time must NOT be promoted — there is no finalization-lag problem to
// compensate for, so promoteIfCovered's ordinary "window closed" behavior
// applies unchanged.
func TestRecorder_PostRollGrace_NotAppliedWhenSegmentsAreShort(t *testing.T) {
	segStore := newTestSegmentStore(t)
	dir := t.TempDir()

	const postRollS = 5
	const segmentSeconds = 2
	cfg := RecorderConfig{
		CameraID:       "cam1",
		Roles:          []string{"high"},
		DataDir:        t.TempDir(),
		Mode:           RecordingModeEvents,
		PreRollS:       0,
		PostRollS:      postRollS,
		SegmentSeconds: segmentSeconds,
	}
	r := NewRecorder(cfg, segStore, &FFmpeg{}, nil)

	const eventStart = int64(100_000)
	const eventEnd = int64(105_000)

	clockMs := eventEnd
	r.nowFn = func() int64 { return clockMs }
	r.MarkEvent("evt-1", eventStart, eventEnd)

	// Well past eventEnd+postRollS (110000), and also well past any
	// plausible SegmentSeconds-based grace (which shouldn't apply at all
	// here since segmentSeconds < postRollS).
	clockMs = 130_000
	lateSeg := seedSegment(t, dir, segStore, eventEnd+20_000, eventEnd+21_000, false)
	r.promoteIfCovered(lateSeg)

	if segmentReferenced(t, segStore, lateSeg) {
		t.Fatalf("segment finalized long after the (un-padded) post-roll window closed must not be promoted when SegmentSeconds < PostRollS")
	}
}

// ---------------------------------------------------------------------------
// sweepEventSpool: discard (no protected windows in play)
// ---------------------------------------------------------------------------

// TestRecorder_SweepEventSpool_DeletesOnlyStaleUnreferencedSegments proves
// the janitor's age/referenced predicate in isolation (no protected windows
// exist in this test): an unreferenced segment older than PreRollS is
// deleted (row and file); an unreferenced segment still within the
// PreRollS freshness window is left alone (so a not-yet-arrived event can
// still claim it); and a referenced segment is never deleted regardless of
// age.
func TestRecorder_SweepEventSpool_DeletesOnlyStaleUnreferencedSegments(t *testing.T) {
	segStore := newTestSegmentStore(t)
	dir := t.TempDir()

	const preRollS = 5
	r := newEventsModeRecorder(t, segStore, preRollS, 0)

	const nowMs = int64(1_700_000_000_000)
	r.nowFn = fixedClock(nowMs)

	stale := seedSegment(t, dir, segStore, nowMs-20_000, nowMs-19_000, false)       // unreferenced, well past preRoll
	fresh := seedSegment(t, dir, segStore, nowMs-3_000, nowMs-2_000, false)         // unreferenced, still within preRoll
	staleButKept := seedSegment(t, dir, segStore, nowMs-20_000, nowMs-19_000, true) // referenced, same age as stale

	r.sweepEventSpool()

	if segmentExists(t, segStore, stale) {
		t.Errorf("stale unreferenced segment should have been deleted from the store")
	}
	if _, err := os.Stat(stale.Path); !os.IsNotExist(err) {
		t.Errorf("stale unreferenced segment's file should have been removed, stat err = %v", err)
	}

	if !segmentExists(t, segStore, fresh) {
		t.Errorf("unreferenced segment still within the pre-roll freshness window should not be deleted")
	}
	if _, err := os.Stat(fresh.Path); err != nil {
		t.Errorf("fresh segment's file should still exist: %v", err)
	}

	if !segmentExists(t, segStore, staleButKept) {
		t.Errorf("referenced segment should never be deleted regardless of age")
	}
	if _, err := os.Stat(staleButKept.Path); err != nil {
		t.Errorf("referenced segment's file should still exist: %v", err)
	}
}

// TestRecorder_SweepEventSpool_NoOpOutsideEventsMode proves the janitor
// never deletes anything for a continuous-mode (or zero-value-mode)
// Recorder, even if a segment happens to be flagged unreferenced (which
// production code never actually produces outside events mode, but the
// no-op must not depend on that).
func TestRecorder_SweepEventSpool_NoOpOutsideEventsMode(t *testing.T) {
	segStore := newTestSegmentStore(t)
	dir := t.TempDir()

	cfg := RecorderConfig{
		CameraID:  "cam1",
		Roles:     []string{"high"},
		DataDir:   t.TempDir(),
		Mode:      RecordingModeContinuous,
		PreRollS:  5,
		PostRollS: 5,
	}
	r := NewRecorder(cfg, segStore, &FFmpeg{}, nil)
	r.nowFn = fixedClock(time.Now().Add(time.Hour).UnixMilli())

	old := seedSegment(t, dir, segStore, 0, 1000, false)

	r.sweepEventSpool()

	if !segmentExists(t, segStore, old) {
		t.Errorf("sweepEventSpool must not delete anything outside events mode")
	}
}

// ---------------------------------------------------------------------------
// Full multi-message lifecycle, driven by the injected clock (Task 8
// follow-up fix): this is the test that would have failed against the
// original one-shot-MarkEvent design (see event_mode.go's package doc for
// the three bugs it proves are fixed).
// ---------------------------------------------------------------------------

// TestRecorder_EventLifecycle_PreRollSurvivesAndPostRollPromotedAfterEnd
// drives a single event through a "start" message (EndTime==0) and, much
// later on the injected clock, an "end" message (EndTime set) — exactly the
// sdk.DetectionEvent lifecycle shape that hid the original bugs — and
// asserts all three required properties:
//
//   - the pre-roll segment (started at eventStart-preRoll, already indexed
//     before the event even started) is retained and NOT swept, even
//     though the clock advances well past it (and well past a naive
//     `now-preRoll` cutoff) while the event is still active with no end
//     message yet (Critical 2's fix: the window has no known end while
//     rawEnd==0, so it can't be treated as stale);
//   - a segment finalized AFTER the end message, within
//     [eventEnd, eventEnd+postRoll], gets promoted via promoteIfCovered —
//     the hook recorder.go's sweepSegments calls for every newly finalized
//     segment (Critical 1's fix: post-roll segments don't exist yet at
//     MarkEvent's own promotion-pass time);
//   - a segment entirely outside [eventStart-preRoll, eventEnd+postRoll]
//     and older than preRoll IS swept, proving the janitor still discards
//     what nothing protects.
func TestRecorder_EventLifecycle_PreRollSurvivesAndPostRollPromotedAfterEnd(t *testing.T) {
	segStore := newTestSegmentStore(t)
	dir := t.TempDir()

	const preRollS = 5
	const postRollS = 5
	r := newEventsModeRecorder(t, segStore, preRollS, postRollS)

	clockMs := int64(100_000)
	r.nowFn = func() int64 { return clockMs }

	const eventStart = int64(102_000) // 2s after the clock starts

	// Pre-roll segment: already recorded before the event started, covering
	// exactly [eventStart-preRoll, eventStart-preRoll+999] = [97000, 97999].
	preRollSeg := seedSegment(t, dir, segStore, eventStart-preRollS*1000, eventStart-preRollS*1000+999, false)

	// Entirely unrelated, far-past segment: covered by no window, ever.
	unrelated := seedSegment(t, dir, segStore, 0, 500, false)

	// "start" message: EndTime == 0 (the event is active, end unknown).
	r.MarkEvent("evt-1", eventStart, 0)

	// Advance the clock well past both the pre-roll segment's own timestamp
	// and a naive `now-preRoll` cutoff, WHILE THE EVENT IS STILL ACTIVE (no
	// end message yet), and sweep repeatedly. The pre-roll segment must
	// survive every sweep because its window has no known end yet.
	clockMs += 60_000 // now = 160000; naive cutoff now-preRoll = 155000 >> 97999
	r.sweepEventSpool()
	r.sweepEventSpool() // idempotent: a second sweep must not change anything

	if !segmentExists(t, segStore, preRollSeg) {
		t.Fatalf("pre-roll segment must survive while its event is still active, however stale it looks")
	}
	if segmentExists(t, segStore, unrelated) {
		t.Fatalf("unrelated, unprotected stale segment should already have been swept")
	}

	// Terminal "end" message: EndTime freezes the window's end.
	const eventEnd = int64(161_000) // 1s after the current clock value
	clockMs = eventEnd
	r.MarkEvent("evt-1", eventStart, eventEnd)

	// Post-roll segment finalized AFTER the end message — simulating
	// recorder.go's sweepSegments indexing it some time later, within
	// [eventEnd, eventEnd+postRoll] — via the same promoteIfCovered hook
	// sweepSegments calls for every newly finalized segment.
	clockMs = eventEnd + 2_000 // 2s after end, still within the 5s post-roll
	postRollSeg := seedSegment(t, dir, segStore, eventEnd+1000, eventEnd+1999, false)
	r.promoteIfCovered(postRollSeg)

	if !segmentReferenced(t, segStore, postRollSeg) {
		t.Fatalf("post-roll segment finalized after the end message must be promoted")
	}

	// Advance well past eventEnd+postRoll and sweep again: the window
	// retires, but both already-promoted segments must remain (the janitor
	// never deletes a referenced segment, window or no window).
	clockMs = eventEnd + (postRollS+10)*1000
	r.sweepEventSpool()

	if !segmentExists(t, segStore, preRollSeg) {
		t.Fatalf("promoted pre-roll segment must not be deleted after window retirement")
	}
	if !segmentExists(t, segStore, postRollSeg) {
		t.Fatalf("promoted post-roll segment must not be deleted after window retirement")
	}
}

// ---------------------------------------------------------------------------
// Concurrency: promotion vs. sweep, under -race.
// ---------------------------------------------------------------------------

// TestRecorder_MarkEventAndSweepEventSpool_ConcurrentNoRace runs a producer
// goroutine (simulating the ingestion path: finalizing new spool segments
// and calling MarkEvent, much like recorder.go's sweepSegments +
// events_ingest.go do from separate goroutines in production) concurrently
// against a sweeper goroutine (simulating the watcher's periodic
// sweepEventSpool tick) for a short, real-time duration, and asserts only
// that nothing panics, errors, or — run under `go test -race` — races. This
// is the IMPORTANT/TOCTOU fix's regression test: promotion
// (InRange-then-MarkReferenced) and the sweep's own
// query-then-filter-then-delete now both hold r.retentionMu for their whole
// sequence, so they can never interleave.
//
// Deliberately avoids calling any *testing.T method from the background
// goroutines (t.Fatal et al. are documented as safe only from the test's
// own goroutine) — errors are instead captured via setErr and asserted
// after both goroutines have finished.
func TestRecorder_MarkEventAndSweepEventSpool_ConcurrentNoRace(t *testing.T) {
	segStore := newTestSegmentStore(t)
	dir := t.TempDir()
	r := newEventsModeRecorder(t, segStore, 2, 2)

	const testDuration = 100 * time.Millisecond
	deadline := time.Now().Add(testDuration)

	var errOnce sync.Once
	var firstErr error
	setErr := func(err error) {
		if err == nil {
			return
		}
		errOnce.Do(func() { firstErr = err })
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		i := 0
		for time.Now().Before(deadline) {
			now := r.nowFn()
			path := filepath.Join(dir, fmt.Sprintf("prod-%d.mp4", i))
			if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
				setErr(err)
				return
			}
			seg := store.Segment{CameraID: "cam1", Role: "high", Path: path, StartMs: now, EndMs: now + 900}
			if _, err := segStore.Add(seg); err != nil {
				setErr(err)
				return
			}
			r.MarkEvent(fmt.Sprintf("evt-%d", i%5), now-1000, now)
			i++
		}
	}()

	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			r.sweepEventSpool()
		}
	}()

	wg.Wait()
	if firstErr != nil {
		t.Fatalf("concurrent producer/sweeper error: %v", firstErr)
	}
}
