package main

import (
	"fmt"
	"sync"
	"unicode"

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
// camera's mode to decide whether calling it is safe). eventID keys the
// recorder's per-event protected window (event_mode.go's eventWindowSet) —
// without it, repeated calls for the same event across its start/update/end
// lifecycle messages couldn't be told apart from calls for different
// events. Tests substitute a spy to assert MarkEvent is invoked with the
// right (eventID, startMs, endMs).
type eventRecorder interface {
	MarkEvent(eventID string, startMs, endMs int64)
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

// eventThumbnailer is the minimal interface detectionEventIngester needs to
// trigger Task 11's thumbnail generation: *media.Generator satisfies it
// directly. GenerateAsync is fire-and-forget — see that method's doc
// comment for why a slow, hung, or failing ffmpeg process must never block
// or fail event ingestion.
type eventThumbnailer interface {
	GenerateAsync(event store.DetectionEvent)
}

// recordingCoverageChecker is the minimal interface detectionEventIngester
// needs to compute a DetectionEvent's has_recording flag (see
// resolveHasRecording): does any indexed recorded segment for the event's
// camera overlap [startMs, endMs]? *store.SegmentStore satisfies this
// directly via CoversRange. Tests substitute a fake so this can be
// exercised without a real SQLite-backed SegmentStore.
type recordingCoverageChecker interface {
	CoversRange(cameraID string, startMs, endMs int64) (bool, error)
}

// eventNotifier is the minimal interface detectionEventIngester needs to
// publish a push notification for a terminal object-detection event: the
// FIX-C gap this closes is that this plugin declares
// PluginInterface.Notifier + PluginCapability.PublishNotifications
// (contract.ts) but never actually calls Publish, so the mobile app never
// receives an event notification the closed NVR did send. *sdk.
// NotificationManager (api.NotificationManager, plugin_api.go) satisfies
// this directly. Tests substitute a fake so notify can be exercised without
// a real host connection.
type eventNotifier interface {
	Publish(n *sdk.Notification) error
}

// cameraNamer resolves a camera ID to its human-readable display name, for
// titling push notifications with (e.g.) "Sideyard" rather than a bare
// camera ID. *recorder.RecorderManager satisfies this directly
// (RecorderManager.CameraName, manager.go) via whatever
// ManagedCamera.Name() reported at Configure/Add time. ok is false when
// this manager has no entry for the camera at all (e.g. it was never
// assigned/managed) — notify falls back to the bare camera ID in that case
// rather than failing to notify.
type cameraNamer interface {
	CameraName(cameraID string) (string, bool)
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
	thumbs    eventThumbnailer
	coverage  recordingCoverageChecker
	logger    *sdk.Logger

	// notifier publishes exactly one push notification per terminal
	// object-detection event (see notify) — nil (tests, or a host that
	// never wired api.NotificationManager) skips notification entirely,
	// the same optional-dependency convention thumbs/coverage already
	// established.
	notifier eventNotifier

	// cameraNames resolves event.CameraID to a display name for the
	// notification title (see notify). nil falls back to the bare camera
	// ID rather than failing to notify.
	cameraNames cameraNamer

	// notified dedups notify's Publish call to exactly once per event ID —
	// see notifiedEvents' own doc comment for why this can't simply reuse
	// acc's per-event accumulator (that entry is evicted on the very same
	// terminal message notify itself reacts to).
	notified notifiedEvents

	// acc accumulates each event's detections/attributes/types across its
	// lifecycle messages (see events_ingest_merge.go) so a sparse terminal
	// 'end' or plain 'update' message — observed to arrive with
	// Segments:[] and no score, even for events whose segment-* messages
	// carried real detections — never clobbers what an earlier message in
	// the same lifecycle already reported. Always non-nil (initialized by
	// newDetectionEventIngester); not a constructor parameter because it is
	// purely internal bookkeeping, not a dependency any caller/test needs
	// to substitute.
	acc *detectionAccumulator
}

// newDetectionEventIngester returns a detectionEventIngester that upserts
// into store, for a camera with a registered recorder calls MarkEvent via
// recorders, (Task 11) dispatches thumbnail generation via thumbs, and
// (has_recording linkage) recomputes each event's HasRecording flag via
// coverage before upserting — see resolveHasRecording. logger may be nil (as
// in unit tests); recorders, thumbs, and coverage may also be nil — handle
// treats a nil recorders identically to RecorderFor returning ok=false
// (skips MarkEvent), a nil thumbs skips thumbnail generation entirely, and a
// nil coverage skips the has_recording recompute (the event's own
// HasRecording value, whatever the producer sent, is upserted unchanged) —
// so existing callers/tests that don't care about that wiring don't need to
// supply one. Errors are only logged, never surfaced, because
// OnDetectionEvent's callback signature (see camera_device.go) has no error
// return for a failed handler to report through.
func newDetectionEventIngester(store eventUpserter, recorders eventRecorderLookup, thumbs eventThumbnailer, coverage recordingCoverageChecker, notifier eventNotifier, cameraNames cameraNamer, logger *sdk.Logger) *detectionEventIngester {
	return &detectionEventIngester{store: store, recorders: recorders, thumbs: thumbs, coverage: coverage, notifier: notifier, cameraNames: cameraNames, logger: logger, acc: &detectionAccumulator{}}
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
// lifecycle message (start, update(s), end, segment-*) carries the event's
// current StartTime/EndTime, and MarkEvent is called on EVERY one of them —
// not just a particular eventType — because EndTime is 0 (sdk.DetectionEvent's
// omitempty zero value) until the terminal message, and *recorder.Recorder
// needs every intermediate call too, to keep the event's protected window
// open and rolling forward while it's still active (see event_mode.go's
// package doc for why calling MarkEvent only once, on the terminal message,
// is exactly the bug this now avoids).
func (i *detectionEventIngester) handle(eventType sdk.DetectionEventType, event sdk.DetectionEvent) {
	if i.logger != nil {
		dets := ""
		for _, s := range event.Segments {
			for _, d := range s.Detections {
				dets += fmt.Sprintf("[%s=%.2f]", d.Label, d.Score)
			}
			for _, a := range s.Attributes {
				dets += fmt.Sprintf("{%s:%s=%.2f}", a.Type, a.Label, a.Confidence)
			}
		}
		trigs := ""
		for _, t := range event.Triggers {
			trigs += fmt.Sprintf("(%s=%.2f)", t.Type, t.Score)
		}
		i.logger.Debug(fmt.Sprintf("nvr-local: ingest type=%s id=%s state=%s types=%v segs=%d dets=%s trigs=%s", eventType, event.ID, event.State, event.Types, len(event.Segments), dets, trigs))
	}

	// merged carries this message's own StartTime/EndTime/State/etc.
	// unchanged, but its Types/Segments/Thumbnail are the accumulated
	// union across every lifecycle message seen for this event so far
	// (see events_ingest_merge.go's doc comment for why: a later sparse
	// message must not erase detections an earlier one already reported).
	// It — not the raw event above — is what gets stored, has_recording-
	// resolved, MarkEvent'd, and thumbnail-generated from.
	merged := i.acc.merge(event)

	merged.HasRecording = i.resolveHasRecording(merged)

	if err := i.store.Upsert([]store.DetectionEvent{merged}); err != nil && i.logger != nil {
		i.logger.Error("nvr-local: upsert detection event failed:", err)
	}
	i.markEvent(merged)
	i.generateThumbnail(merged)
	i.notify(merged)
}

// resolveHasRecording recomputes event.HasRecording from the recorded
// segment index rather than trusting whatever value the detection-event
// producer set on the wire — every event otherwise persists with
// has_recording=0 regardless of whether footage actually exists behind it
// (the bug this fixes: events never linked to their playable clips).
//
// The window checked is [event.StartTime, endMs], where endMs is
// event.EndTime once the event has ended (EndTime > 0 — its terminal 'end'
// lifecycle message) or event.StartTime itself for every earlier message
// (start/update/segment-*), so:
//
//   - an event's very first ('start') message already reports
//     has_recording=true when continuous recording already covers its
//     start time (the common case) — this is CoversRange with startMs ==
//     endMs, a point-in-time check equivalent to CoveringSegment.
//   - the terminal ('end') message re-evaluates over the event's FULL
//     [start,end] window, which is what actually matters for "is there a
//     playable clip behind this event" once its real duration is known —
//     this is also what recovers an event whose covering segment wasn't
//     finalized/indexed yet at the moment of an earlier message
//     (finalization lag; see recorder.go's postRollWindowMs for the same
//     class of lag elsewhere in this plugin). Every intermediate 'update'
//     message recomputes the same way as 'start', so a change in coverage
//     (recording starting mid-event) is picked up before the event ends
//     too, not just at the two lifecycle extremes.
//
// A nil i.coverage (callers that don't care about this wiring, matching
// every other optional dependency on detectionEventIngester) or a failed
// query leaves event.HasRecording exactly as the producer sent it, rather
// than forcing it false.
//
// Before any of that, resolveHasRecording also checks whether the event's
// camera is actively being recorded at all right now (i.recorders,
// RecorderFor's ok=false/true — see recorderRegistry.RecorderFor:
// registered exactly while a *recorder.Recorder is running for a camera
// whose configured RecordingMode isn't "off"). If it is, has_recording is
// true unconditionally: continuous/events-mode recording means footage
// exists (or is actively being written) for this window even when the
// specific segment covering the event's exact timestamps hasn't been
// finalized/indexed into SegmentStore yet — the observed real-world case
// (~half of events) where CoversRange alone finds nothing at end-time
// because the segment rolls over on its own ~60s cadence, independent of
// any individual event's lifecycle. This is checked in addition to, not
// instead of, the CoversRange path below, so an event on a camera this
// plugin isn't actively recording still gets credited for a segment that
// happens to cover it (e.g. one recorded by a different process/session).
func (i *detectionEventIngester) resolveHasRecording(event sdk.DetectionEvent) bool {
	if i.recorders != nil {
		if _, ok := i.recorders.RecorderFor(event.CameraID); ok {
			return true
		}
	}

	if i.coverage == nil {
		return event.HasRecording
	}

	endMs := event.EndTime
	if endMs <= 0 {
		endMs = event.StartTime
	}

	covered, err := i.coverage.CoversRange(event.CameraID, event.StartTime, endMs)
	if err != nil {
		if i.logger != nil {
			i.logger.Error("nvr-local: has_recording coverage check failed:", err)
		}
		return event.HasRecording
	}
	return covered
}

// markEvent calls MarkEvent(event.ID, event.StartTime, event.EndTime) on the
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
	rec.MarkEvent(event.ID, event.StartTime, event.EndTime)
}

// generateThumbnail dispatches GenerateAsync(event) on i.thumbs, if one was
// supplied. A no-op when i.thumbs is nil (see newDetectionEventIngester's
// doc comment) — every lifecycle message calls this the same way markEvent
// is called on every one, since Generator itself (media.Generator, via its
// per-event done-map) is what decides whether a given message is worth
// acting on, not this ingester.
func (i *detectionEventIngester) generateThumbnail(event sdk.DetectionEvent) {
	if i.thumbs == nil {
		return
	}
	i.thumbs.GenerateAsync(event)
}

// notify publishes exactly one push notification (via i.notifier) for
// event, once — FIX C's task brief: only on the event's terminal lifecycle
// message (isTerminal: state "ended" or a nonzero EndTime — every earlier
// start/update/segment-* message returns here without notifying) AND only
// for an object-detection event (store.EventHasDetections — the same rule
// eventHasDetections' own doc comment ties to the frontend's client-side
// display filter, so a motion-only or audio-only event never notifies
// either, avoiding notification spam for the common case). i.notified
// (keyed by event.ID) guards against publishing twice for the same event
// even if its terminal message somehow arrives more than once.
//
// A no-op when i.notifier is nil (tests, or a host build that never wired
// api.NotificationManager) — checked first, before doing any of the
// (cheap, but pointless if nothing will be published) terminal/detection
// checks below.
//
// Publish errors are logged, never surfaced: like markEvent/
// generateThumbnail, OnDetectionEvent's callback signature has no error
// return for a failed notification to propagate through, and a
// notification failure must never fail or block event ingestion itself.
func (i *detectionEventIngester) notify(event sdk.DetectionEvent) {
	if i.notifier == nil {
		return
	}
	if !isTerminalEvent(event) {
		return
	}
	if !store.EventHasDetections(event) {
		return
	}
	if !i.notified.markFirst(event.ID) {
		return
	}

	cameraTitle := event.CameraID
	if i.cameraNames != nil {
		if name, ok := i.cameraNames.CameraName(event.CameraID); ok && name != "" {
			cameraTitle = name
		}
	}

	n := &sdk.Notification{
		Title:     fmt.Sprintf("%s — %s", cameraTitle, titleCaseLabel(store.PrimaryLabel(event))),
		Severity:  sdk.SeverityInfo,
		Thumbnail: event.Thumbnail,
		Data: map[string]string{
			"cameraId": event.CameraID,
			"eventId":  event.ID,
		},
	}
	if err := i.notifier.Publish(n); err != nil && i.logger != nil {
		i.logger.Error("nvr-local: publish notification failed:", err)
	}
}

// isTerminalEvent reports whether event has reached its terminal lifecycle
// message — mirrors detectionAccumulator.merge's own identical check
// (events_ingest_merge.go) for when an event's accumulator entry is
// evicted, since both need the exact same "this is the last message for
// this event" definition: State == DetectionEventStateEnded, or a nonzero
// EndTime (sdk.DetectionEvent.EndTime is 0/omitempty until that message).
func isTerminalEvent(event sdk.DetectionEvent) bool {
	return event.State == sdk.DetectionEventStateEnded || event.EndTime > 0
}

// titleCaseLabel upper-cases label's first rune (leaving the rest
// unchanged) for display in a notification title — e.g. "person" ->
// "Person", "doorbell" -> "Doorbell". A no-op for an empty label.
func titleCaseLabel(label string) string {
	if label == "" {
		return label
	}
	r := []rune(label)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// notifiedEvents tracks which event IDs have already had exactly one push
// notification published for them (notify's dedup), bounded the same way
// detectionAccumulator is (evict the oldest entry once past
// detectionAccumulatorCap) so a pathological producer generating unbounded
// distinct event IDs can't leak this map's memory unboundedly. Guarded by
// mu for the same no-single-goroutine-guarantee reason detectionAccumulator
// is (events_ingest_merge.go). Zero value is ready to use.
type notifiedEvents struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
}

// markFirst reports whether id has NOT been recorded as notified before —
// true on the first call for a given id (and records it as seen), false on
// every subsequent call for the same id. notify calls this exactly once per
// terminal message, right before actually publishing, so a duplicate
// terminal message for the same event never results in a second Publish.
func (n *notifiedEvents) markFirst(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.seen == nil {
		n.seen = make(map[string]struct{})
	}
	if _, ok := n.seen[id]; ok {
		return false
	}
	n.seen[id] = struct{}{}
	n.order = append(n.order, id)
	for len(n.order) > detectionAccumulatorCap {
		oldest := n.order[0]
		n.order = n.order[1:]
		delete(n.seen, oldest)
	}
	return true
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
