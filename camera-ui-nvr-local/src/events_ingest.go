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

// detectionEventIngester adapts sdk.CameraDevice.OnDetectionEvent's callback
// shape into an EventStore.Upsert call. One instance is shared across every
// camera this plugin attaches to (see NVRPlugin.attachDetectionIngestion in
// plugin.go); it carries no per-camera state of its own — the event already
// identifies its camera via DetectionEvent.CameraID.
type detectionEventIngester struct {
	store  eventUpserter
	logger *sdk.Logger
}

// newDetectionEventIngester returns a detectionEventIngester that upserts
// into store. logger may be nil (as in unit tests); errors are only logged,
// never surfaced, because OnDetectionEvent's callback signature (see
// camera_device.go) has no error return for a failed handler to report
// through.
func newDetectionEventIngester(store eventUpserter, logger *sdk.Logger) *detectionEventIngester {
	return &detectionEventIngester{store: store, logger: logger}
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
func (i *detectionEventIngester) handle(eventType sdk.DetectionEventType, event sdk.DetectionEvent) {
	if err := i.store.Upsert([]store.DetectionEvent{event}); err != nil && i.logger != nil {
		i.logger.Error("nvr-local: upsert detection event failed:", err)
	}
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
