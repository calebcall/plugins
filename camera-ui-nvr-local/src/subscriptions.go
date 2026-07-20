// subscriptions.go holds the thread-safe subscriber registries backing
// OnRecordingState/OnSystemEvent (rpc_subscriptions.go, Task SUBS): plain
// in-process fan-out from register() to emit(), with no dependency on
// github.com/cameraui/rpc/go at all. The RPC framework's own callback-
// publish mechanism (handleCallbackRequestGo,
// github.com/cameraui/rpc/go@v1.0.6/handler_callback.go) synthesizes a
// func(T) per subscription that publishes each invocation to the client
// over its callback subject — these registries are simply what
// OnRecordingState/OnSystemEvent hand that synthesized func to, and what
// this plugin's own recorder-lifecycle producer (onRecorderStateChange)
// fans out through.
//
// Both types' zero value is immediately usable (maps are created lazily in
// register()), matching this package's existing recorderRegistry
// convention (plugin.go).
package main

import (
	"sync"
	"time"
)

// nowMs is time.Now().UnixMilli(), factored out purely so
// onRecorderStateChange/OnRecordingState (rpc_subscriptions.go) have one
// obvious call site to point at rather than repeating the UnixMilli()
// incantation inline.
func nowMs() int64 { return time.Now().UnixMilli() }

// recordingStateSubscribers is a thread-safe, per-camera-ID registry of
// OnRecordingState callback subscriptions. Accessed from both the RPC
// goroutine handling OnRecordingState/its returned cleanup and the recorder
// goroutines that call emit via onRecorderStateChange — every method here
// holds mu for its own critical section only, and emit copies the callback
// slice out before invoking any of them, so a subscriber callback that
// itself calls back into this registry (e.g. a test's own cleanup) can
// never deadlock against emit's lock.
type recordingStateSubscribers struct {
	mu   sync.Mutex
	subs map[string]map[int]func(RecordingState)
	next int
}

// register adds cb as a subscriber for cameraID and returns a cleanup that
// removes exactly this subscription (not every subscription for cameraID —
// each registration gets its own integer id under the per-camera map, the
// same "distinguish same-shape subscribers" problem a plain
// map[string][]func would not solve, since a slice has no stable per-entry
// handle to remove by identity).
func (r *recordingStateSubscribers) register(cameraID string, cb func(RecordingState)) func() {
	r.mu.Lock()
	if r.subs == nil {
		r.subs = make(map[string]map[int]func(RecordingState))
	}
	if r.subs[cameraID] == nil {
		r.subs[cameraID] = make(map[int]func(RecordingState))
	}
	r.next++
	id := r.next
	r.subs[cameraID][id] = cb
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if m, ok := r.subs[cameraID]; ok {
			delete(m, id)
			if len(m) == 0 {
				delete(r.subs, cameraID)
			}
		}
	}
}

// emit fans state out to every currently-registered subscriber for
// state.CameraID. A no-op for a camera with no subscribers.
func (r *recordingStateSubscribers) emit(state RecordingState) {
	r.mu.Lock()
	byID := r.subs[state.CameraID]
	cbs := make([]func(RecordingState), 0, len(byID))
	for _, cb := range byID {
		cbs = append(cbs, cb)
	}
	r.mu.Unlock()

	for _, cb := range cbs {
		cb(state)
	}
}

// systemEventSubscribers is the OnSystemEvent equivalent of
// recordingStateSubscribers, minus the per-camera keying: every subscriber
// receives every SystemEvent, matching the frontend contract's
// onSystemEvent(callback) signature (no cameraID filter parameter).
type systemEventSubscribers struct {
	mu   sync.Mutex
	subs map[int]func(SystemEvent)
	next int
}

// register adds cb as a subscriber and returns a cleanup that removes
// exactly this subscription.
func (r *systemEventSubscribers) register(cb func(SystemEvent)) func() {
	r.mu.Lock()
	if r.subs == nil {
		r.subs = make(map[int]func(SystemEvent))
	}
	r.next++
	id := r.next
	r.subs[id] = cb
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.subs, id)
	}
}

// emit fans ev out to every currently-registered subscriber.
func (r *systemEventSubscribers) emit(ev SystemEvent) {
	r.mu.Lock()
	cbs := make([]func(SystemEvent), 0, len(r.subs))
	for _, cb := range r.subs {
		cbs = append(cbs, cb)
	}
	r.mu.Unlock()

	for _, cb := range cbs {
		cb(ev)
	}
}
