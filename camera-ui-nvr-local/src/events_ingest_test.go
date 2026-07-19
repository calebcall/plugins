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
	ingester := newDetectionEventIngester(fake, nil)

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
	ingester := newDetectionEventIngester(fake, nil)

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
	ingester := newDetectionEventIngester(fake, nil)

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{ID: "evt-1", CameraID: "cam1"})
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
