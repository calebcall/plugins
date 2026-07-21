package main

import (
	"errors"
	"sync"
	"testing"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// fakeEventStore is an in-memory stand-in for *store.EventStore, recording
// every Upsert call so detectionEventIngester.handle can be tested without a
// real SQLite-backed EventStore or a live sdk.CameraDevice (which can't be
// constructed outside package sdk — see attachDetectionIngestion's DEFERRED
// note in plugin.go). Guarded by mu because handle (the method under test)
// is documented as callable from concurrent host-driven goroutines with no
// single-goroutine guarantee — the real *store.EventStore.Upsert already
// locks internally (see store/events.go), so this fake must too, or a
// concurrent-handle-calls test (-race) flags the fake itself rather than
// anything production code does.
type fakeEventStore struct {
	mu       sync.Mutex
	upserted []store.DetectionEvent
	err      error
}

func (f *fakeEventStore) Upsert(events []store.DetectionEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.upserted = append(f.upserted, events...)
	return nil
}

// TestDetectionEventIngester_Handle_UpsertsTheEvent proves handle — the
// exact callback shape sdk.CameraDevice.OnDetectionEvent expects — forwards
// a synthetic detection event into the store unchanged.
func TestDetectionEventIngester_Handle_UpsertsTheEvent(t *testing.T) {
	fake := &fakeEventStore{}
	ingester := newDetectionEventIngester(fake, nil, nil, nil, nil, nil, nil)

	event := sdk.DetectionEvent{
		ID:        "evt-1",
		CameraID:  "cam1",
		State:     sdk.DetectionEventStateActive,
		StartTime: 1000,
		Types:     []string{"motion"},
	}

	ingester.handle(sdk.DetectionEventStart, event)

	if len(fake.upserted) != 1 {
		t.Fatalf("expected exactly 1 upsert, got %d", len(fake.upserted))
	}
	if fake.upserted[0].ID != "evt-1" || fake.upserted[0].CameraID != "cam1" {
		t.Fatalf("expected the synthetic event to be forwarded unchanged, got %+v", fake.upserted[0])
	}
}

// TestDetectionEventIngester_Handle_ReplacesOnUpdate proves successive
// lifecycle messages for the same event id (start, then end) both flow
// through handle as separate Upsert calls — EventStore's ON CONFLICT(id) DO
// UPDATE (not this handler) is what turns them into a single row, but the
// handler must not skip or coalesce them itself.
func TestDetectionEventIngester_Handle_ReplacesOnUpdate(t *testing.T) {
	fake := &fakeEventStore{}
	ingester := newDetectionEventIngester(fake, nil, nil, nil, nil, nil, nil)

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateActive, StartTime: 1000,
	})
	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded, StartTime: 1000, EndTime: 5000,
	})

	if len(fake.upserted) != 2 {
		t.Fatalf("expected handle to call Upsert once per message (2 total), got %d", len(fake.upserted))
	}
	if fake.upserted[1].State != sdk.DetectionEventStateEnded {
		t.Fatalf("expected the second call to carry the ended state, got %+v", fake.upserted[1])
	}
}

// TestDetectionEventIngester_Handle_LogsAndSwallowsStoreErrors proves a
// failing Upsert doesn't panic or propagate — OnDetectionEvent's callback
// signature (func(DetectionEventType, DetectionEvent), no error return) has
// nowhere for an error to go, so handle must swallow it (after logging,
// exercised here only via a nil logger to prove it doesn't panic when
// there's nowhere to log to either).
func TestDetectionEventIngester_Handle_LogsAndSwallowsStoreErrors(t *testing.T) {
	fake := &fakeEventStore{err: errors.New("boom")}
	ingester := newDetectionEventIngester(fake, nil, nil, nil, nil, nil, nil)

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{ID: "evt-1", CameraID: "cam1"})
}

// ---------------------------------------------------------------------------
// Task 8 wiring: handle -> eventRecorder.MarkEvent
// ---------------------------------------------------------------------------

// spyRecorder is an eventRecorder test double recording every MarkEvent call
// it receives, so a test can assert both that it was called and with which
// (eventID, startMs, endMs).
type spyRecorder struct {
	calls []struct {
		eventID string
		startMs int64
		endMs   int64
	}
}

func (s *spyRecorder) MarkEvent(eventID string, startMs, endMs int64) {
	s.calls = append(s.calls, struct {
		eventID string
		startMs int64
		endMs   int64
	}{eventID, startMs, endMs})
}

// fakeRecorderLookup is an eventRecorderLookup test double: recorders holds
// the (possibly empty) set of camera IDs with a registered eventRecorder.
type fakeRecorderLookup struct {
	recorders map[string]eventRecorder
}

func (f *fakeRecorderLookup) RecorderFor(cameraID string) (eventRecorder, bool) {
	rec, ok := f.recorders[cameraID]
	return rec, ok
}

// TestDetectionEventIngester_Handle_CallsMarkEventOnRegisteredRecorder proves
// handle looks up the event's camera in the recorder lookup and, when found,
// calls MarkEvent with exactly the event's ID/StartTime/EndTime — the window
// recorder.Recorder.MarkEvent (event_mode.go) then expands by the camera's
// pre/post roll itself.
func TestDetectionEventIngester_Handle_CallsMarkEventOnRegisteredRecorder(t *testing.T) {
	spy := &spyRecorder{}
	lookup := &fakeRecorderLookup{recorders: map[string]eventRecorder{"cam1": spy}}
	ingester := newDetectionEventIngester(&fakeEventStore{}, lookup, nil, nil, nil, nil, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000, EndTime: 6000,
	})

	if len(spy.calls) != 1 {
		t.Fatalf("expected exactly 1 MarkEvent call, got %d", len(spy.calls))
	}
	if spy.calls[0].eventID != "evt-1" || spy.calls[0].startMs != 1000 || spy.calls[0].endMs != 6000 {
		t.Fatalf("expected MarkEvent(evt-1, 1000, 6000), got MarkEvent(%s, %d, %d)", spy.calls[0].eventID, spy.calls[0].startMs, spy.calls[0].endMs)
	}
}

// TestDetectionEventIngester_Handle_StartThenEndLifecycle_CallsMarkEventForBoth
// is the multi-message lifecycle case that hid the original design's bugs
// (see event_mode.go's package doc): a "start" message reports EndTime==0
// (sdk.DetectionEvent's omitempty zero value), and only a later "end"
// message reports the real EndTime. This proves handle calls MarkEvent for
// BOTH messages — not just the terminal one — with the same eventID both
// times (so recorder.Recorder can tell they're updates to the same
// protected window, not two different events) and the exact EndTime each
// message carried (0, then 5000).
func TestDetectionEventIngester_Handle_StartThenEndLifecycle_CallsMarkEventForBoth(t *testing.T) {
	spy := &spyRecorder{}
	lookup := &fakeRecorderLookup{recorders: map[string]eventRecorder{"cam1": spy}}
	ingester := newDetectionEventIngester(&fakeEventStore{}, lookup, nil, nil, nil, nil, nil)

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateActive, StartTime: 1000,
	})
	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded, StartTime: 1000, EndTime: 5000,
	})

	if len(spy.calls) != 2 {
		t.Fatalf("expected 2 MarkEvent calls (one per lifecycle message), got %d", len(spy.calls))
	}
	if spy.calls[0].eventID != "evt-1" || spy.calls[0].startMs != 1000 || spy.calls[0].endMs != 0 {
		t.Fatalf("expected the start message's call to be MarkEvent(evt-1, 1000, 0), got MarkEvent(%s, %d, %d)", spy.calls[0].eventID, spy.calls[0].startMs, spy.calls[0].endMs)
	}
	if spy.calls[1].eventID != "evt-1" || spy.calls[1].startMs != 1000 || spy.calls[1].endMs != 5000 {
		t.Fatalf("expected the end message's call to be MarkEvent(evt-1, 1000, 5000), got MarkEvent(%s, %d, %d)", spy.calls[1].eventID, spy.calls[1].startMs, spy.calls[1].endMs)
	}
}

// TestDetectionEventIngester_Handle_SkipsMarkEventWhenNoRecorderRegistered
// proves handle does not call MarkEvent (and does not panic) for a camera
// with no registered recorder — e.g. one this instance isn't recording, or
// isn't in events mode — even though a lookup is configured.
func TestDetectionEventIngester_Handle_SkipsMarkEventWhenNoRecorderRegistered(t *testing.T) {
	lookup := &fakeRecorderLookup{recorders: map[string]eventRecorder{}}
	ingester := newDetectionEventIngester(&fakeEventStore{}, lookup, nil, nil, nil, nil, nil)

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam-unregistered", StartTime: 1000,
	})
	// No assertion beyond "did not panic": there is no spy to have been
	// called, by construction of this test's empty lookup.
}

// TestDetectionEventIngester_Handle_SkipsMarkEventWhenLookupNil proves handle
// tolerates a nil eventRecorderLookup (the default for any caller that
// doesn't care about event-mode wiring, matching newDetectionEventIngester's
// doc comment) without panicking.
func TestDetectionEventIngester_Handle_SkipsMarkEventWhenLookupNil(t *testing.T) {
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, nil, nil, nil)
	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{ID: "evt-1", CameraID: "cam1", StartTime: 1000})
}

// ---------------------------------------------------------------------------
// has_recording linkage: handle -> recordingCoverageChecker.CoversRange
// ---------------------------------------------------------------------------

// fakeCoverageChecker is a recordingCoverageChecker test double: covered
// reports the fixed answer every CoversRange call should return (or
// coverageErr, if set, to exercise the error-swallowing path), and calls
// records every (cameraID, startMs, endMs) triple it was invoked with so
// tests can assert exactly which window handle checked.
type fakeCoverageChecker struct {
	covered     bool
	coverageErr error
	calls       []struct {
		cameraID string
		startMs  int64
		endMs    int64
	}
}

func (f *fakeCoverageChecker) CoversRange(cameraID string, startMs, endMs int64) (bool, error) {
	f.calls = append(f.calls, struct {
		cameraID string
		startMs  int64
		endMs    int64
	}{cameraID, startMs, endMs})
	if f.coverageErr != nil {
		return false, f.coverageErr
	}
	return f.covered, nil
}

// TestDetectionEventIngester_Handle_SetsHasRecordingWhenCovered proves a
// 'start' message (EndTime == 0) whose StartTime is covered by an indexed
// segment is upserted with HasRecording=true, and that the coverage check
// ran as a point-in-time query (startMs == endMs == event.StartTime).
func TestDetectionEventIngester_Handle_SetsHasRecordingWhenCovered(t *testing.T) {
	fake := &fakeEventStore{}
	coverage := &fakeCoverageChecker{covered: true}
	ingester := newDetectionEventIngester(fake, nil, nil, coverage, nil, nil, nil)

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000,
	})

	if len(fake.upserted) != 1 || !fake.upserted[0].HasRecording {
		t.Fatalf("expected the upserted event to have HasRecording=true, got %+v", fake.upserted)
	}
	if len(coverage.calls) != 1 || coverage.calls[0].cameraID != "cam1" || coverage.calls[0].startMs != 1000 || coverage.calls[0].endMs != 1000 {
		t.Fatalf("expected a point-in-time CoversRange(cam1, 1000, 1000) call, got %+v", coverage.calls)
	}
}

// TestDetectionEventIngester_Handle_LeavesHasRecordingFalseWhenNotCovered
// proves an event with no covering segment is upserted with
// HasRecording=false.
func TestDetectionEventIngester_Handle_LeavesHasRecordingFalseWhenNotCovered(t *testing.T) {
	fake := &fakeEventStore{}
	coverage := &fakeCoverageChecker{covered: false}
	ingester := newDetectionEventIngester(fake, nil, nil, coverage, nil, nil, nil)

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000,
	})

	if len(fake.upserted) != 1 || fake.upserted[0].HasRecording {
		t.Fatalf("expected the upserted event to have HasRecording=false, got %+v", fake.upserted)
	}
}

// TestDetectionEventIngester_Handle_RecomputesHasRecordingOnEndMessage
// proves the terminal 'end' message re-evaluates has_recording over the
// event's FULL [start,end] window (not just its start instant): the 'start'
// message finds no coverage yet (segment not finalized/indexed), but by the
// time the 'end' message arrives coverage exists, so the final row is
// HasRecording=true — and the second CoversRange call is over
// [1000, 5000], not a repeat of the first [1000, 1000] point check.
func TestDetectionEventIngester_Handle_RecomputesHasRecordingOnEndMessage(t *testing.T) {
	fake := &fakeEventStore{}
	coverage := &fakeCoverageChecker{covered: false}
	ingester := newDetectionEventIngester(fake, nil, nil, coverage, nil, nil, nil)

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000,
	})

	// Recording catches up between the start and end messages.
	coverage.covered = true

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000, EndTime: 5000,
	})

	if len(fake.upserted) != 2 {
		t.Fatalf("expected 2 upserts (one per lifecycle message), got %d", len(fake.upserted))
	}
	if fake.upserted[0].HasRecording {
		t.Fatalf("expected the start message's row to have HasRecording=false, got true")
	}
	if !fake.upserted[1].HasRecording {
		t.Fatalf("expected the end message's row to have HasRecording=true, got false")
	}

	if len(coverage.calls) != 2 {
		t.Fatalf("expected 2 CoversRange calls, got %d", len(coverage.calls))
	}
	if coverage.calls[1].startMs != 1000 || coverage.calls[1].endMs != 5000 {
		t.Fatalf("expected the end message's coverage check to span [1000, 5000], got [%d, %d]", coverage.calls[1].startMs, coverage.calls[1].endMs)
	}
}

// TestDetectionEventIngester_Handle_NilCoverageLeavesHasRecordingUnchanged
// proves a nil coverage checker (the default for callers that don't wire
// this up, matching every other optional dependency) leaves the event's own
// HasRecording value untouched rather than forcing it to false.
func TestDetectionEventIngester_Handle_NilCoverageLeavesHasRecordingUnchanged(t *testing.T) {
	fake := &fakeEventStore{}
	ingester := newDetectionEventIngester(fake, nil, nil, nil, nil, nil, nil)

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000, HasRecording: true,
	})

	if len(fake.upserted) != 1 || !fake.upserted[0].HasRecording {
		t.Fatalf("expected a nil coverage checker to leave HasRecording=true unchanged, got %+v", fake.upserted)
	}
}

// TestDetectionEventIngester_Handle_CoverageErrorLeavesHasRecordingUnchanged
// proves a failing CoversRange call is swallowed (logged, not propagated —
// same contract as a failing Upsert) and leaves HasRecording at whatever the
// producer sent, rather than panicking or forcing false.
func TestDetectionEventIngester_Handle_CoverageErrorLeavesHasRecordingUnchanged(t *testing.T) {
	fake := &fakeEventStore{}
	coverage := &fakeCoverageChecker{coverageErr: errors.New("boom")}
	ingester := newDetectionEventIngester(fake, nil, nil, coverage, nil, nil, nil)

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000, HasRecording: true,
	})

	if len(fake.upserted) != 1 || !fake.upserted[0].HasRecording {
		t.Fatalf("expected a CoversRange error to leave HasRecording=true unchanged, got %+v", fake.upserted)
	}
}

// TestDetectionEventIngester_Handle_ActiveRecordingSetsHasRecordingWithoutCoverage
// proves the finalize-lag fix: a camera this plugin is actively recording
// (a registered eventRecorder — see recorderRegistry.RecorderFor, only
// registered while a *recorder.Recorder is running for a non-"off"
// RecordingMode camera) gets HasRecording=true even when no segment has
// been indexed yet to cover the event's window — the real-world case where
// the covering segment isn't finalized/indexed until the next ~60s segment
// roll, well after the event itself has already ended.
func TestDetectionEventIngester_Handle_ActiveRecordingSetsHasRecordingWithoutCoverage(t *testing.T) {
	fake := &fakeEventStore{}
	coverage := &fakeCoverageChecker{covered: false}
	lookup := &fakeRecorderLookup{recorders: map[string]eventRecorder{"cam1": &spyRecorder{}}}
	ingester := newDetectionEventIngester(fake, lookup, nil, coverage, nil, nil, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded, StartTime: 1000, EndTime: 5000,
	})

	if len(fake.upserted) != 1 || !fake.upserted[0].HasRecording {
		t.Fatalf("expected an actively-recorded camera to persist has_recording=true despite no coverage, got %+v", fake.upserted)
	}
}

// TestDetectionEventIngester_Handle_NoActiveRecordingFallsBackToCoverage
// proves a camera with recordingMode=off (no registered eventRecorder) gets
// no free pass from the active-recording check: with no coverage either,
// has_recording stays false — the active-recording check is additive, not
// a replacement for CoversRange.
func TestDetectionEventIngester_Handle_NoActiveRecordingFallsBackToCoverage(t *testing.T) {
	fake := &fakeEventStore{}
	coverage := &fakeCoverageChecker{covered: false}
	lookup := &fakeRecorderLookup{recorders: map[string]eventRecorder{}}
	ingester := newDetectionEventIngester(fake, lookup, nil, coverage, nil, nil, nil)

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000,
	})

	if len(fake.upserted) != 1 || fake.upserted[0].HasRecording {
		t.Fatalf("expected has_recording=false for an unrecorded camera with no coverage, got %+v", fake.upserted)
	}
}

// TestDetectionEventIngester_Handle_RealSegmentStore_LinksHasRecordingEndToEnd
// is the end-to-end proof, against a real SQLite-backed SegmentStore/
// EventStore (newTestPluginWithDB, the same wiring NewPlugin uses in
// production), that ingesting a detection event whose time is covered by an
// indexed segment persists has_recording=true, an event with no covering
// segment persists has_recording=false, and the terminal 'end' message's
// full-window recompute promotes a previously-uncovered event to true once
// its post-roll segment has since been indexed — the exact finalization-lag
// scenario resolveHasRecording's doc comment describes.
func TestDetectionEventIngester_Handle_RealSegmentStore_LinksHasRecordingEndToEnd(t *testing.T) {
	p := newTestPluginWithDB(t)

	// cam1 has a segment covering [10_000, 20_000).
	if _, err := p.segments.Add(store.Segment{
		CameraID: "cam1", Role: "high-resolution", Path: "/rec/cam1.mp4",
		StartMs: 10_000, EndMs: 20_000, HasVideo: true,
	}); err != nil {
		t.Fatalf("segments.Add: %v", err)
	}

	ingester := newDetectionEventIngester(p.events, nil, nil, p.segments, nil, nil, nil)

	// Covered: cam1's start time (12_000) falls inside the indexed segment.
	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-covered", CameraID: "cam1", StartTime: 12_000,
	})
	covered, err := p.events.Query([]string{"cam1"}, store.GetEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(covered.Events) != 1 || !covered.Events[0].HasRecording {
		t.Fatalf("expected evt-covered to persist has_recording=true, got %+v", covered.Events)
	}

	// Not covered: cam2 has no indexed segments at all.
	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-uncovered", CameraID: "cam2", StartTime: 12_000,
	})
	uncovered, err := p.events.Query([]string{"cam2"}, store.GetEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(uncovered.Events) != 1 || uncovered.Events[0].HasRecording {
		t.Fatalf("expected evt-uncovered to persist has_recording=false, got %+v", uncovered.Events)
	}

	// End-message recompute: cam3's 'start' message finds nothing yet (its
	// post-roll segment isn't indexed until after the event ends), but by
	// the 'end' message the segment covering its full [start,end] window
	// has been indexed, so the recompute over the FULL window promotes it.
	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-lag", CameraID: "cam3", StartTime: 30_000,
	})
	afterStart, err := p.events.Query([]string{"cam3"}, store.GetEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(afterStart.Events) != 1 || afterStart.Events[0].HasRecording {
		t.Fatalf("expected evt-lag's start message to persist has_recording=false (not yet indexed), got %+v", afterStart.Events)
	}

	if _, err := p.segments.Add(store.Segment{
		CameraID: "cam3", Role: "high-resolution", Path: "/rec/cam3.mp4",
		StartMs: 30_000, EndMs: 40_000, HasVideo: true,
	}); err != nil {
		t.Fatalf("segments.Add: %v", err)
	}

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-lag", CameraID: "cam3", StartTime: 30_000, EndTime: 35_000,
	})
	afterEnd, err := p.events.Query([]string{"cam3"}, store.GetEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(afterEnd.Events) != 1 || !afterEnd.Events[0].HasRecording {
		t.Fatalf("expected evt-lag's end message to promote has_recording to true once its segment was indexed, got %+v", afterEnd.Events)
	}
}

// TestDetectionSubscriptions_AddThenRemove proves the add/remove bookkeeping
// attachDetectionIngestion/OnCameraReleased rely on actually disposes the
// right camera's subscription and forgets it, so a later remove of the same
// id is a no-op rather than a double-dispose.
func TestDetectionSubscriptions_AddThenRemove(t *testing.T) {
	var subs detectionSubscriptions

	disposed := false
	d := sdk.NewDisposable(func() { disposed = true })

	subs.add("cam1", d)
	if disposed {
		t.Fatalf("add must not dispose immediately")
	}

	subs.remove("cam1")
	if !disposed {
		t.Fatalf("expected remove to dispose the tracked subscription")
	}

	// Removing again (e.g. a duplicate OnCameraReleased) must not panic or
	// double-dispose.
	subs.remove("cam1")
}

// TestDetectionSubscriptions_AddReplacesExisting proves adding a second
// subscription for a camera id already tracked disposes the first one
// instead of leaking it.
func TestDetectionSubscriptions_AddReplacesExisting(t *testing.T) {
	var subs detectionSubscriptions

	firstDisposed := false
	first := sdk.NewDisposable(func() { firstDisposed = true })
	second := sdk.NewDisposable(func() {})

	subs.add("cam1", first)
	subs.add("cam1", second)

	if !firstDisposed {
		t.Fatalf("expected the first subscription to be disposed when replaced")
	}
}
