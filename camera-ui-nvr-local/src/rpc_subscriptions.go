// rpc_subscriptions.go implements OnRecordingState/OnSystemEvent (Task
// SUBS): the two callback-subscription RPC methods the closed frontend's
// CameraTimeline calls but this plugin never implemented — RPCMethods()
// (plugin.go) never listed them, so rpc.ExtractMethods never subscribed
// them on the wire, and the frontend's RPC proxy left them undefined,
// throwing `t.onRecordingState is not a function` /
// `e.onSystemEvent is not a function` when the timeline tried to call
// them.
//
// Handler signature (confirmed by reading handleCallbackRequestGo,
// github.com/cameraui/rpc/go@v1.0.6/handler_callback.go, lines ~14-90): a
// callback subscription request is recognized by the presence of a
// func(T) (or func(T) error) parameter on the handler method — found by
// scanning fn.Type().In(i) for the first reflect.Func kind. The framework
// then reflect.MakeFunc's its own wrapper of that exact type, substitutes
// it in for the real parameter, and calls the handler. The handler's
// result is passed through processResults (handler.go): for a two-value
// return whose second value is an error, the FIRST value becomes
// handlerResult. handleCallbackRequestGo then does exactly this:
//
//	if handlerResult != nil {
//	    if fn, ok := handlerResult.(func()); ok {
//	        handlerCleanup = fn
//	    }
//	}
//
// — a plain type assertion against `func()`. A handler returning
// (func() error, error) would fail that assertion silently (handlerCleanup
// stays nil, and the client's eventual unsubscribe would never call
// through to this plugin's own cleanup), so the return type here is
// exactly (func(), error), not (func() error, error).
package main

import (
	"fmt"
)

// OnRecordingState registers cb to receive every subsequent recording
// lifecycle transition (recording/stopped) for cameraID, as
// RecorderManager's live Recorder for that camera actually starts/stops
// (wired via onRecorderStateChange below, through
// recorder.RecorderManager.SetStateNotifier in NewPlugin/plugin.go — NOT
// merely the camera's configured RecordingMode). Before returning, cb is
// invoked once immediately with the camera's CURRENT state (p.recorder.
// IsActive), so a client subscribing to an already-running (or
// already-stopped) camera initializes its UI without waiting for the next
// transition — the CameraTimeline behavior this task's brief calls out
// ("Also immediately emit the current state for that camera").
//
// Returns a cleanup that unregisters cb from p.recordingStateSubs; see this
// file's package doc comment for why the return type must be exactly
// (func(), error).
func (p *NVRPlugin) OnRecordingState(cameraID string, cb func(RecordingState)) (func(), error) {
	p.logRPC("onRecordingState", cameraID)

	cleanup := p.recordingStateSubs.register(cameraID, cb)

	state := "stopped"
	if p.recorder.IsActive(cameraID) {
		state = "recording"
	}
	cb(RecordingState{CameraID: cameraID, State: state, Timestamp: nowMs()})

	return cleanup, nil
}

// OnSystemEvent registers cb to receive every subsequent SystemEvent this
// plugin emits. Today the only producer is onRecorderStateChange (below) —
// a recorder start/stop also emits a matching SystemEvent, filling in part
// of the "nothing calls SystemEventStore.Insert yet" gap that
// store.SystemEvent's own doc comment flags — but this method's own
// contract is independent of that: it exists, and works, regardless of how
// many (or how few) producers exist. No current-state replay on subscribe
// here, unlike OnRecordingState: SystemEvent has no single "current state"
// to echo back (getSystemEvents, rpc_events.go, already covers the
// historical/backlog read path).
//
// Returns a cleanup that unregisters cb from p.systemEventSubs; see this
// file's package doc comment for why the return type must be exactly
// (func(), error).
func (p *NVRPlugin) OnSystemEvent(cb func(SystemEvent)) (func(), error) {
	p.logRPC("onSystemEvent")

	cleanup := p.systemEventSubs.register(cb)
	return cleanup, nil
}

// onRecorderStateChange is RecorderManager's stateNotify hook (wired in
// NewPlugin via p.recorder.SetStateNotifier(p.onRecorderStateChange)):
// called from the recorder's own goroutines whenever a managed camera's
// live Recorder actually starts or stops. Translates that raw transition
// into a RecordingState and fans it out to every OnRecordingState
// subscriber for cameraID, and — as the minimal, low-risk SystemEvent
// producer this task's brief calls out as optional — also persists (when
// p.systemEvents is non-nil; nil in unit tests that construct NVRPlugin
// directly, and whenever store.Open failed in NewPlugin — see p.systemEvents'
// own doc comment) and fans out a matching SystemEvent.
//
// A persistence failure is logged (when p.Logger is non-nil), not
// propagated: this is a notification hook with no caller in a position to
// receive or act on an error (RecorderManager.notifyState never returns
// one), the same tolerance p.Logger's other fire-and-forget call sites in
// this package already have.
//
// Called by RecorderManager AFTER it has released camLock(cameraID) — see
// recorder.RecorderManager.notifyState's doc comment — so this method, and
// everything it does (subscriber fan-out, the SystemEventStore.Insert
// below), never runs while that per-camera lock is held. It is therefore
// safe for a subscriber callback registered via OnRecordingState/
// OnSystemEvent to call back into RecorderManager for the SAME camera ID
// (e.g. Remove) without deadlocking.
func (p *NVRPlugin) onRecorderStateChange(cameraID string, recording bool) {
	state := "stopped"
	eventType := "recorder_stopped"
	if recording {
		state = "recording"
		eventType = "recorder_started"
	}
	ts := nowMs()

	p.recordingStateSubs.emit(RecordingState{CameraID: cameraID, State: state, Timestamp: ts})

	ev := SystemEvent{
		ID:        fmt.Sprintf("%s-%s-%d", eventType, cameraID, ts),
		Type:      eventType,
		Severity:  "info",
		CameraID:  cameraID,
		Timestamp: ts,
		Message:   fmt.Sprintf("Recording %s for camera %s", state, cameraID),
	}
	// Persist first, emit live only on success. Otherwise a failed insert
	// would still reach every live onSystemEvent subscriber while
	// getSystemEvents' persisted backlog never gets the event at all,
	// silently diverging the live feed from the historical record.
	// p.systemEvents == nil (no store configured — unit tests that
	// construct NVRPlugin directly, or a failed store.Open in NewPlugin)
	// is deliberately NOT treated as a persistence failure: there is no
	// backlog for the live feed to diverge from in that case, so it still
	// emits.
	persisted := true
	if p.systemEvents != nil {
		if err := p.systemEvents.Insert([]SystemEvent{ev}); err != nil {
			persisted = false
			if p.Logger != nil {
				p.Logger.Warn("nvr-local: insert system event failed:", err)
			}
		}
	}
	if persisted {
		p.systemEventSubs.emit(ev)
	}
}
