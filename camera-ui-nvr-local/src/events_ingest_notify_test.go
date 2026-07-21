package main

import (
	"errors"
	"sync"
	"testing"

	sdk "github.com/cameraui/sdk/go"
)

// fakeNotifier is an eventNotifier test double recording every Publish call
// it receives, so notify (events_ingest.go) can be tested without a real
// *sdk.NotificationManager/host connection. Guarded by mu for the same
// no-single-goroutine-guarantee reason every other ingester dependency's
// fake in this package is (handle has no documented single-goroutine
// contract).
type fakeNotifier struct {
	mu           sync.Mutex
	published    []*sdk.Notification
	publishErr   error
	publishCalls int
}

func (f *fakeNotifier) Publish(n *sdk.Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishCalls++
	if f.publishErr != nil {
		return f.publishErr
	}
	f.published = append(f.published, n)
	return nil
}

// fakeCameraNames is a cameraNamer test double: names maps a camera ID to
// its display name; a camera ID absent from the map reports ok=false, the
// same "no entry for this camera" contract RecorderManager.CameraName has.
type fakeCameraNames struct {
	names map[string]string
}

func (f *fakeCameraNames) CameraName(cameraID string) (string, bool) {
	name, ok := f.names[cameraID]
	return name, ok
}

// TestDetectionEventIngester_Notify_ObjectEventTerminalMessagePublishesOnce
// is the FIX C core proof: a person-detection event's terminal ('end')
// message publishes exactly one notification, titled with the resolved
// camera name and the primary (object) label, and carrying cameraId/eventId
// in Data.
func TestDetectionEventIngester_Notify_ObjectEventTerminalMessagePublishesOnce(t *testing.T) {
	notifier := &fakeNotifier{}
	names := &fakeCameraNames{names: map[string]string{"cam1": "Sideyard"}}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, names, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000,
		Types: []string{"motion", "person"},
		Segments: []sdk.EventSegment{
			{Detections: []sdk.EventDetection{{Label: "person", Score: 0.8}}},
		},
	})

	if notifier.publishCalls != 1 {
		t.Fatalf("expected exactly 1 Publish call, got %d", notifier.publishCalls)
	}
	n := notifier.published[0]
	if n.Title != "Sideyard — Person" {
		t.Errorf("expected title %q, got %q", "Sideyard — Person", n.Title)
	}
	if n.Severity != sdk.SeverityInfo {
		t.Errorf("expected severity info, got %q", n.Severity)
	}
	if n.Data["cameraId"] != "cam1" || n.Data["eventId"] != "evt-1" {
		t.Errorf("expected Data to carry cameraId=cam1 eventId=evt-1, got %+v", n.Data)
	}
}

// TestDetectionEventIngester_Notify_MotionOnlyEventNeverPublishes proves a
// motion-only event's terminal message publishes NO notification at all —
// the anti-spam gate FIX C's brief requires (only object-detection events
// notify).
func TestDetectionEventIngester_Notify_MotionOnlyEventNeverPublishes(t *testing.T) {
	notifier := &fakeNotifier{}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, nil, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000, Types: []string{"motion"},
	})

	if notifier.publishCalls != 0 {
		t.Fatalf("expected motion-only event to never publish, got %d calls", notifier.publishCalls)
	}
}

// TestDetectionEventIngester_Notify_AudioOnlyEventNeverPublishes proves an
// audio-only event's terminal message also publishes nothing — the same
// non-object-detection gate as motion-only.
func TestDetectionEventIngester_Notify_AudioOnlyEventNeverPublishes(t *testing.T) {
	notifier := &fakeNotifier{}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, nil, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000, Types: []string{"audio"},
	})

	if notifier.publishCalls != 0 {
		t.Fatalf("expected audio-only event to never publish, got %d calls", notifier.publishCalls)
	}
}

// TestDetectionEventIngester_Notify_NonTerminalMessagesNeverPublish proves
// an object-detection event's start/update messages (not yet terminal) do
// NOT publish — only the terminal message does.
func TestDetectionEventIngester_Notify_NonTerminalMessagesNeverPublish(t *testing.T) {
	notifier := &fakeNotifier{}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, nil, nil)

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateActive,
		StartTime: 1000, Types: []string{"person"},
	})
	ingester.handle(sdk.DetectionEventUpdate, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateActive,
		StartTime: 1000, Types: []string{"person"},
	})

	if notifier.publishCalls != 0 {
		t.Fatalf("expected non-terminal messages to never publish, got %d calls", notifier.publishCalls)
	}
}

// TestDetectionEventIngester_Notify_DuplicateTerminalMessageDoesNotDoublePublish
// proves a second terminal message arriving for an event id already
// notified (e.g. a duplicate/retried 'end' delivery) does not publish a
// second time.
func TestDetectionEventIngester_Notify_DuplicateTerminalMessageDoesNotDoublePublish(t *testing.T) {
	notifier := &fakeNotifier{}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, nil, nil)

	end := sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000, Types: []string{"person"},
	}
	ingester.handle(sdk.DetectionEventEnd, end)
	ingester.handle(sdk.DetectionEventEnd, end)

	if notifier.publishCalls != 1 {
		t.Fatalf("expected exactly 1 Publish call across two terminal messages for the same event id, got %d", notifier.publishCalls)
	}
}

// TestDetectionEventIngester_Notify_NilNotifierNeverPublishesOrPanics proves
// a nil notifier (the default for tests, or a host build that never wired
// api.NotificationManager) is a safe no-op rather than a nil-pointer panic.
func TestDetectionEventIngester_Notify_NilNotifierNeverPublishesOrPanics(t *testing.T) {
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, nil, nil, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000, Types: []string{"person"},
	})
	// No assertion beyond "did not panic" — there is no notifier to have
	// been called.
}

// TestDetectionEventIngester_Notify_UnresolvedCameraNameFallsBackToID proves
// a nil cameraNamer, or one with no entry for the event's camera, falls back
// to the bare camera ID in the notification title rather than failing to
// notify.
func TestDetectionEventIngester_Notify_UnresolvedCameraNameFallsBackToID(t *testing.T) {
	notifier := &fakeNotifier{}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, nil, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000, Types: []string{"person"},
	})

	if notifier.publishCalls != 1 {
		t.Fatalf("expected exactly 1 Publish call, got %d", notifier.publishCalls)
	}
	if notifier.published[0].Title != "cam1 — Person" {
		t.Errorf("expected title %q, got %q", "cam1 — Person", notifier.published[0].Title)
	}
}

// TestDetectionEventIngester_Notify_PublishErrorIsSwallowed proves a failing
// Publish call is logged (exercised here with a nil logger, to prove it
// doesn't panic when there's nowhere to log to either) rather than
// propagated — OnDetectionEvent's callback has no error return for it to go
// through.
func TestDetectionEventIngester_Notify_PublishErrorIsSwallowed(t *testing.T) {
	notifier := &fakeNotifier{publishErr: errors.New("boom")}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, nil, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000, Types: []string{"person"},
	})
	// No panic, and the earlier "swallowed, not surfaced" contract is
	// implicitly proven by this test simply completing.
}
