// rpc_subscriptions_test.go covers OnRecordingState/OnSystemEvent (Task
// SUBS): the two callback-subscription RPC methods the closed frontend's
// CameraTimeline calls but this plugin never implemented, which is why the
// browser sees `t.onRecordingState is not a function` /
// `e.onSystemEvent is not a function` — RPCMethods() never listed them, so
// they were never registered on the wire and stayed undefined on the
// frontend's RPC proxy.
//
// These tests exercise the registry/notify logic directly (register,
// unregister-stops-delivery, RPCMethods listing) rather than the live
// framework's callback-subject publish, which needs a connected NATS
// client and can't be unit-tested here — see handleCallbackRequestGo
// (github.com/cameraui/rpc/go@v1.0.6/handler_callback.go) for the framework
// side this handler signature must match: it calls the handler, then does
// `handlerResult.(func())` on the first non-error return value to find the
// cleanup to run when the client cancels the subscription — so both
// OnRecordingState and OnSystemEvent must return exactly (func(), error),
// not (func() error, error) or anything else.
package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/recorder"
)

// fakeSubscriptionHandle is a no-op recorder.RecorderHandle: these tests
// only care about RecorderManager's start/stop *notifications*
// (SetStateNotifier), not any real recording behavior, so Start/Stop do
// nothing and never fail.
type fakeSubscriptionHandle struct{}

func (fakeSubscriptionHandle) Start(ctx context.Context) error { return nil }
func (fakeSubscriptionHandle) Stop() error                     { return nil }

// --- OnRecordingState --------------------------------------------------

// TestOnRecordingState_RPCMethodsAllowsIt pins down the exact bug this task
// fixes: onRecordingState must be in the allow-list RPCMethods() returns,
// or rpc.ExtractMethods never subscribes it on the wire at all and it stays
// undefined on the frontend's proxy no matter what this file implements.
func TestOnRecordingState_RPCMethodsAllowsIt(t *testing.T) {
	p := newTestPlugin(t)
	allowed := p.RPCMethods()
	found := false
	for _, name := range allowed {
		if name == "onRecordingState" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected RPCMethods() to include %q, got %v", "onRecordingState", allowed)
	}
}

// TestOnRecordingState_EmitsCurrentStateImmediately covers the "initialize
// correctly" requirement: subscribing to a camera that isn't currently
// being actively recorded must immediately receive one "stopped" callback,
// with no recorder transition required to trigger it.
func TestOnRecordingState_EmitsCurrentStateImmediately(t *testing.T) {
	p := newTestPlugin(t)

	var mu sync.Mutex
	var got []RecordingState
	cleanup, err := p.OnRecordingState("cam-1", func(s RecordingState) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("OnRecordingState: %v", err)
	}
	defer cleanup()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 immediate callback, got %d: %+v", len(got), got)
	}
	if got[0].CameraID != "cam-1" || got[0].State != "stopped" {
		t.Fatalf("expected immediate {cam-1 stopped}, got %+v", got[0])
	}
	if got[0].Timestamp <= 0 {
		t.Fatalf("expected a non-zero timestamp, got %d", got[0].Timestamp)
	}
}

// TestOnRecordingState_RecorderStartStopNotifiesSubscriber is the
// end-to-end wiring test: a real RecorderManager (with a fake
// RecorderHandle, no ffmpeg) actually starting/stopping a camera's recorder
// must notify an OnRecordingState subscriber for that camera with
// {cameraId, state, timestamp} — the "hook those transitions" requirement.
func TestOnRecordingState_RecorderStartStopNotifiesSubscriber(t *testing.T) {
	p := newTestPlugin(t)
	p.recorder.SetStateNotifier(p.onRecorderStateChange)

	var mu sync.Mutex
	var got []RecordingState
	cleanup, err := p.OnRecordingState("cam-1", func(s RecordingState) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("OnRecordingState: %v", err)
	}
	defer cleanup()

	p.recorder.ConfigureRecording("/tmp/nvr-subs-test", 0, func(recorder.RecorderConfig) recorder.RecorderHandle {
		return fakeSubscriptionHandle{}
	})
	cam := newFakeManagedCamera("cam-1", "Front Door", "continuous")
	if err := p.recorder.Add(cam); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := p.recorder.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if err := p.recorder.Remove("cam-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// [0] = immediate "stopped" on subscribe, [1] = recorder start ->
	// "recording", [2] = Remove's stop -> "stopped".
	if len(got) != 3 {
		t.Fatalf("expected 3 callbacks (initial, start, stop), got %d: %+v", len(got), got)
	}
	if got[1].CameraID != "cam-1" || got[1].State != "recording" {
		t.Fatalf("expected recorder start to notify {cam-1 recording}, got %+v", got[1])
	}
	if got[2].CameraID != "cam-1" || got[2].State != "stopped" {
		t.Fatalf("expected recorder stop to notify {cam-1 stopped}, got %+v", got[2])
	}
}

// TestOnRecordingState_CallbackDoesNotDeadlockReenteringSameCameraLock is
// the regression test for the lock-scope review fix: notifyState (and the
// subscriber fan-out it triggers) must run AFTER RecorderManager releases
// camLock(cameraID), never while still holding it. This subscribes with a
// callback that, on seeing "recording", calls p.recorder.Remove for the
// SAME camera ID from inside the callback — i.e. from the same goroutine
// that is still unwinding out of RecorderManager's own start path. If
// notifyState (or the code that calls it) were still holding
// camLock("cam-1") at that point, Remove's own lock.Lock() call for the
// same camera ID would self-deadlock forever, since sync.Mutex is not
// reentrant and no other goroutine will ever unlock it. StartAll (which
// triggers the notification synchronously) runs in its own goroutine so a
// real deadlock hangs that goroutine instead of the test process, and a
// timeout turns that hang into a normal test failure instead of an
// unkillable `go test` run.
func TestOnRecordingState_CallbackDoesNotDeadlockReenteringSameCameraLock(t *testing.T) {
	p := newTestPlugin(t)
	p.recorder.SetStateNotifier(p.onRecorderStateChange)
	p.recorder.ConfigureRecording("/tmp/nvr-lockscope-test", 0, func(recorder.RecorderConfig) recorder.RecorderHandle {
		return fakeSubscriptionHandle{}
	})

	var mu sync.Mutex
	var removeErr error
	var removeCalled bool
	cleanup, err := p.OnRecordingState("cam-1", func(s RecordingState) {
		if s.State != "recording" {
			return
		}
		mu.Lock()
		removeCalled = true
		mu.Unlock()
		// Re-enters RecorderManager for the SAME camera ID from inside the
		// notification callback — the exact shape the lock-scope fix must
		// tolerate.
		err := p.recorder.Remove("cam-1")
		mu.Lock()
		removeErr = err
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("OnRecordingState: %v", err)
	}
	defer cleanup()

	cam := newFakeManagedCamera("cam-1", "Front Door", "continuous")
	if err := p.recorder.Add(cam); err != nil {
		t.Fatalf("Add: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.recorder.StartAll()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartAll deadlocked: notifyState (or its caller) appears to still hold camLock while invoking subscriber callbacks")
	}

	mu.Lock()
	defer mu.Unlock()
	if !removeCalled {
		t.Fatalf("expected the callback to observe a \"recording\" transition and call Remove")
	}
	if removeErr != nil {
		t.Fatalf("Remove (called from within the notify callback): %v", removeErr)
	}
	if ids := p.recorder.ManagedCameraIDs(); len(ids) != 0 {
		t.Fatalf("expected Remove (called from within the callback) to have taken effect, got %v", ids)
	}
}

// TestOnRecordingState_CleanupStopsFurtherDelivery covers the unsubscribe
// contract handleCallbackRequestGo depends on: the cleanup this handler
// returns must actually unregister the callback, not merely be inert.
func TestOnRecordingState_CleanupStopsFurtherDelivery(t *testing.T) {
	p := newTestPlugin(t)
	p.recorder.SetStateNotifier(p.onRecorderStateChange)

	var mu sync.Mutex
	calls := 0
	cleanup, err := p.OnRecordingState("cam-1", func(s RecordingState) {
		mu.Lock()
		calls++
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("OnRecordingState: %v", err)
	}

	mu.Lock()
	afterSubscribe := calls
	mu.Unlock()
	if afterSubscribe != 1 {
		t.Fatalf("expected 1 immediate callback, got %d", afterSubscribe)
	}

	cleanup()

	// A recorder transition after cleanup must not reach the unsubscribed
	// callback.
	p.onRecorderStateChange("cam-1", true)

	mu.Lock()
	defer mu.Unlock()
	if calls != afterSubscribe {
		t.Fatalf("expected cleanup to stop further delivery: calls before=%d after transition=%d", afterSubscribe, calls)
	}
}

// TestOnRecordingState_OnlyNotifiesMatchingCamera covers that a subscriber
// for one camera never receives another camera's transitions.
func TestOnRecordingState_OnlyNotifiesMatchingCamera(t *testing.T) {
	p := newTestPlugin(t)

	var mu sync.Mutex
	var got []RecordingState
	cleanup, err := p.OnRecordingState("cam-1", func(s RecordingState) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("OnRecordingState: %v", err)
	}
	defer cleanup()

	p.onRecorderStateChange("cam-2", true)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected only the initial cam-1 callback, got %d: %+v", len(got), got)
	}
}

// --- OnSystemEvent -------------------------------------------------------

func TestOnSystemEvent_RPCMethodsAllowsIt(t *testing.T) {
	p := newTestPlugin(t)
	allowed := p.RPCMethods()
	found := false
	for _, name := range allowed {
		if name == "onSystemEvent" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected RPCMethods() to include %q, got %v", "onSystemEvent", allowed)
	}
}

func TestOnSystemEvent_RegisterAndCleanup(t *testing.T) {
	p := newTestPlugin(t)

	var mu sync.Mutex
	var got []SystemEvent
	cleanup, err := p.OnSystemEvent(func(ev SystemEvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("OnSystemEvent: %v", err)
	}

	p.onRecorderStateChange("cam-1", true)

	mu.Lock()
	if len(got) != 1 {
		mu.Unlock()
		t.Fatalf("expected 1 system event after a recorder transition, got %d", len(got))
	}
	if got[0].CameraID != "cam-1" {
		mu.Unlock()
		t.Fatalf("expected system event for cam-1, got %+v", got[0])
	}
	mu.Unlock()

	cleanup()
	p.onRecorderStateChange("cam-1", false)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected cleanup to stop further delivery, got %d events", len(got))
	}
}

// --- concurrency (-race) -------------------------------------------------

// TestRecordingStateSubscribers_ConcurrentRegisterAndEmit exercises
// concurrent register/unregister/emit from multiple goroutines — the shape
// the task brief calls out (RPC goroutine registering/unregistering while
// recorder goroutines emit) — to be run with `go test -race`.
func TestRecordingStateSubscribers_ConcurrentRegisterAndEmit(t *testing.T) {
	p := newTestPlugin(t)

	var wg sync.WaitGroup
	const n = 50
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			cleanup, err := p.OnRecordingState("cam-1", func(RecordingState) {})
			if err != nil {
				t.Errorf("OnRecordingState: %v", err)
				return
			}
			cleanup()
		}()
		go func() {
			defer wg.Done()
			p.onRecorderStateChange("cam-1", true)
		}()
	}
	wg.Wait()
}

// TestSystemEventSubscribers_ConcurrentRegisterAndEmit is the
// onSystemEvent equivalent of the above.
func TestSystemEventSubscribers_ConcurrentRegisterAndEmit(t *testing.T) {
	p := newTestPlugin(t)

	var wg sync.WaitGroup
	const n = 50
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			cleanup, err := p.OnSystemEvent(func(SystemEvent) {})
			if err != nil {
				t.Errorf("OnSystemEvent: %v", err)
				return
			}
			cleanup()
		}()
		go func() {
			defer wg.Done()
			p.onRecorderStateChange("cam-1", false)
		}()
	}
	wg.Wait()
}
