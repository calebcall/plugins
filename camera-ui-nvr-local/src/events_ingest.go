package main

import (
	"sync"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// eventUpserter is the minimal interface detectionEventIngester needs from
// the event store. store.EventStore satisfies it; tests substitute a fake
// so the ingestion handler can be exercised with a synthetic event and no
// SQLite involved.
type eventUpserter interface {
	Upsert(events []store.DetectionEvent) error
}

// eventRecorder is the minimal interface detectionEventIngester needs to
// trigger Task 8's event-mode retention: *recorder.Recorder.MarkEvent
// satisfies it directly (see recorder/event_mode.go — it is itself a no-op
// outside RecordingModeEvents, so the ingester never needs to know a
// camera's mode to decide whether calling it is safe). Tests substitute a
// spy to assert MarkEvent is invoked with the right window.
type eventRecorder interface {
	MarkEvent(startMs, endMs int64)
}

// eventRecorderLookup resolves a camera ID to the eventRecorder responsible
// for it, if this plugin instance currently has one registered. A camera
// this instance isn't recording at all (recordingMode "off", or simply not
// assigned here) has none, and RecorderFor's ok=false return tells handle to
// skip MarkEvent for it entirely rather than trying to construct a
// zero-value recorder.
type eventRecorderLookup interface {
	RecorderFor(cameraID string) (eventRecorder, bool)
}

// detectionEventIngester adapts sdk.CameraDevice.OnDetectionEvent's callback
// shape into an EventStore.Upsert call, plus (Task 8) a MarkEvent call on
// the event's camera's recorder, if one is registered. One instance is
// shared across every camera this plugin attaches to (see
// NVRPlugin.attachDetectionIngestion in plugin.go); it carries no per-camera
// state of its own — the event already identifies its camera via
// DetectionEvent.CameraID.
type detectionEventIngester struct {
	store     eventUpserter
	recorders eventRecorderLookup
	logger    *sdk.Logger
}

// newDetectionEventIngester returns a detectionEventIngester that upserts
// into store and, for a camera with a registered recorder, calls MarkEvent
// via recorders. logger may be nil (as in unit tests); recorders may also be
// nil — handle treats that identically to RecorderFor returning ok=false,
// i.e. it just skips the MarkEvent step — so existing callers/tests that
// don't care about event-mode wiring at all don't need to supply a lookup.
// Errors are only logged, never surfaced, because OnDetectionEvent's
// callback signature (see camera_device.go) has no error return for a
// failed handler to report through.
func newDetectionEventIngester(store eventUpserter, recorders eventRecorderLookup, logger *sdk.Logger) *detectionEventIngester {
	return &detectionEventIngester{store: store, recorders: recorders, logger: logger}
}

// handle is the exact callback shape sdk.CameraDevice.OnDetectionEvent
// expects:
//
//	func (d *CameraDevice) OnDetectionEvent(callback func(eventType DetectionEventType, event DetectionEvent)) *Disposable
//
// (github.com/cameraui/sdk/go@v1.1.11/camera_device.go:547). It fires for
// every lifecycle message (start/update/end/segment-start/segment-
// update/segment-end); each one is upserted as-is by event.ID, so a later
// message for the same event (e.g. 'end' following 'start') replaces the
// row via EventStore.Upsert's ON CONFLICT(id) DO UPDATE rather than adding a
// duplicate. eventType itself isn't needed here — event.ID and event.State
// already carry everything Upsert needs to decide insert vs. replace.
//
// After upserting, handle also calls markEvent for the same event: every
// lifecycle message carries the event's current StartTime/EndTime, so
// MarkEvent is called repeatedly (once per message) with a window that only
// grows as EndTime advances from 0 (event still active) to its final value
// — each call promotes whatever segments are coverable so far, which is
// simpler and safer than trying to call it only once on some particular
// eventType, and is idempotent because MarkReferenced (store/segments.go) is
// itself idempotent.
func (i *detectionEventIngester) handle(eventType sdk.DetectionEventType, event sdk.DetectionEvent) {
	if err := i.store.Upsert([]store.DetectionEvent{event}); err != nil && i.logger != nil {
		i.logger.Error("nvr-local: upsert detection event failed:", err)
	}
	i.markEvent(event)
}

// markEvent calls MarkEvent(event.StartTime, event.EndTime) on the
// eventRecorder registered for event.CameraID, if any. A no-op when
// i.recorders is nil or has no recorder registered for this camera — see
// newDetectionEventIngester's doc comment for why that's not an error.
func (i *detectionEventIngester) markEvent(event sdk.DetectionEvent) {
	if i.recorders == nil {
		return
	}
	rec, ok := i.recorders.RecorderFor(event.CameraID)
	if !ok {
		return
	}
	rec.MarkEvent(event.StartTime, event.EndTime)
}

// detectionSubscriptions tracks the per-camera sdk.Disposable returned by
// CameraDevice.OnDetectionEvent, so OnCameraReleased can unsubscribe the
// right camera instead of leaking a listener on every hub-camera detach.
// Guarded by a mutex because ConfigureCameras/OnCameraAdded/OnCameraReleased
// are documented (plugin.go, sdk.Plugin) as host-driven callbacks with no
// stated single-goroutine guarantee.
type detectionSubscriptions struct {
	mu   sync.Mutex
	subs map[string]*sdk.Disposable
}

func (s *detectionSubscriptions) add(cameraID string, disposable *sdk.Disposable) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subs == nil {
		s.subs = make(map[string]*sdk.Disposable)
	}
	// A pre-existing subscription for this camera (e.g. OnCameraAdded firing
	// twice, or ConfigureCameras racing OnCameraAdded for the same id) would
	// otherwise leak: drop it before overwriting.
	if existing, ok := s.subs[cameraID]; ok {
		existing.Dispose()
	}
	s.subs[cameraID] = disposable
}

func (s *detectionSubscriptions) remove(cameraID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.subs[cameraID]; ok {
		d.Dispose()
		delete(s.subs, cameraID)
	}
}
