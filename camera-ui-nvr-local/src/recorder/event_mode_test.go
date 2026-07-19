package recorder

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// newEventsModeRecorder returns a Recorder configured for RecordingModeEvents
// against a fresh segStore, with the given pre/post roll (seconds). It never
// runs ffmpeg — every test in this file drives MarkEvent/sweepEventSpool
// directly against segments seeded straight into segStore, simulating "a
// sequence of finalized segments" (the brief's rolling buffer) without any
// real recording.
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
// MarkEvent: promotion
// ---------------------------------------------------------------------------

// TestRecorder_MarkEvent_PromotesOnlySegmentsCoveringWindow proves MarkEvent,
// given a rolling buffer of finalized (but not yet referenced) segments,
// promotes exactly the ones overlapping [start-preRoll, end+postRoll] and
// leaves every other segment untouched (still unreferenced).
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
	r.MarkEvent(19000, 21000)

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
	r.MarkEvent(eventAtMs, eventAtMs)

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

	r.MarkEvent(0, 0)

	if !segmentReferenced(t, segStore, seg) {
		t.Errorf("continuous-mode segment must remain referenced after MarkEvent")
	}

	// A Recorder with Mode unset (zero value) and no segStore at all must
	// not panic either — this is the "no-op" half of MarkEvent's contract
	// taken to its limit.
	bare := NewRecorder(RecorderConfig{CameraID: "cam2"}, nil, &FFmpeg{}, nil)
	bare.MarkEvent(1000, 2000)
}

// ---------------------------------------------------------------------------
// sweepEventSpool: discard
// ---------------------------------------------------------------------------

// TestRecorder_SweepEventSpool_DeletesOnlyStaleUnreferencedSegments proves
// the janitor: an unreferenced segment older than PreRollS is deleted (row
// and file); an unreferenced segment still within the PreRollS freshness
// window is left alone (so a not-yet-arrived event can still claim it); and
// a referenced segment is never deleted regardless of age.
func TestRecorder_SweepEventSpool_DeletesOnlyStaleUnreferencedSegments(t *testing.T) {
	segStore := newTestSegmentStore(t)
	dir := t.TempDir()

	const preRollS = 5
	r := newEventsModeRecorder(t, segStore, preRollS, 0)

	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	nowMs := now.UnixMilli()

	stale := seedSegment(t, dir, segStore, nowMs-20_000, nowMs-19_000, false)       // unreferenced, well past preRoll
	fresh := seedSegment(t, dir, segStore, nowMs-3_000, nowMs-2_000, false)         // unreferenced, still within preRoll
	staleButKept := seedSegment(t, dir, segStore, nowMs-20_000, nowMs-19_000, true) // referenced, same age as stale

	r.sweepEventSpool(now)

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

	old := seedSegment(t, dir, segStore, 0, 1000, false)

	r.sweepEventSpool(time.Now().Add(time.Hour))

	if !segmentExists(t, segStore, old) {
		t.Errorf("sweepEventSpool must not delete anything outside events mode")
	}
}
