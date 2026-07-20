package main

import (
	"errors"
	"testing"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// fakeEventStore is an in-memory stand-in for *store.EventStore, recording
// every Upsert call so detectionEventIngester.handle can be tested without a
// real SQLite-backed EventStore or a live sdk.CameraDevice (which can't be
// constructed outside package sdk — see attachDetectionIngestion's DEFERRED
// note in plugin.go).
type fakeEventStore struct {
	upserted []store.DetectionEvent
	err      error
}

func (f *fakeEventStore) Upsert(events []store.DetectionEvent) error {
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
	ingester := newDetectionEventIngester(fake, nil, nil, nil)

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
	ingester := newDetectionEventIngester(fake, nil, nil, nil)

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
	ingester := newDetectionEventIngester(fake, nil, nil, nil)

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
	ingester := newDetectionEventIngester(&fakeEventStore{}, lookup, nil, nil)

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
	ingester := newDetectionEventIngester(&fakeEventStore{}, lookup, nil, nil)

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
	ingester := newDetectionEventIngester(&fakeEventStore{}, lookup, nil, nil)

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
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil)
	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{ID: "evt-1", CameraID: "cam1", StartTime: 1000})
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
