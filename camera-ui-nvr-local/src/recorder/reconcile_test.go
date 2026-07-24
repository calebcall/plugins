// reconcile_test.go covers the periodic reconciliation pass (reconcile.go):
// picking up a camera's recordingMode change with no SDK config-changed hook
// (the fix for runtime camera add/config edits needing a full plugin
// restart), without churning healthy active recorders. Uses the same
// fakeCamera/fakeRecorderFactory/fakeTicker doubles as the other manager
// orchestration tests.
package recorder

import (
	"testing"
	"time"
)

// launchedManager wires a manager with one camera at the given mode, a fake
// recorder factory, and a completed StartAll — the common setup for the
// reconcile tests below. Returns the manager, the camera (so a test can flip
// its stored mode), and the factory (so a test can inspect handles).
func launchedManager(t *testing.T, mode RecordingMode) (*RecorderManager, *fakeCamera, *fakeRecorderFactory) {
	t.Helper()
	m := NewRecorderManager()
	cam := newFakeCamera("cam-1", "Front Door", mode)
	if err := m.Configure([]ManagedCamera{cam}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	return m, cam, factory
}

// TestReconcile_StartsCameraWhoseModeFlippedFromOff is the core fix: a camera
// adopted while its mode was "off" (the default for a freshly re-added
// camera) records nothing at launch; once the user sets it to "continuous",
// Reconcile starts its recorder — no plugin restart required.
func TestReconcile_StartsCameraWhoseModeFlippedFromOff(t *testing.T) {
	m, cam, factory := launchedManager(t, RecordingModeOff)

	if factory.callCount() != 0 {
		t.Fatalf("expected no recorder for off-mode camera at launch, got %d", factory.callCount())
	}

	// User configures it in the UI: off -> continuous.
	cam.storage.set(keyRecordingMode, string(RecordingModeContinuous))
	m.Reconcile()

	h := factory.handleFor("cam-1")
	if h == nil {
		t.Fatalf("expected reconcile to build a recorder for cam-1")
	}
	if started, _ := h.snapshot(); started != 1 {
		t.Fatalf("expected cam-1 started once by reconcile, got %d", started)
	}
	if !m.IsActive("cam-1") {
		t.Fatalf("expected cam-1 active after reconcile")
	}
	if ids := m.ManagedCameraIDs(); len(ids) != 1 || ids[0] != "cam-1" {
		t.Fatalf("expected cam-1 managed after mode change, got %v", ids)
	}
}

// TestReconcile_StopsCameraWhoseModeFlippedToOff proves the reverse: turning a
// recording camera off stops its recorder on the next reconcile.
func TestReconcile_StopsCameraWhoseModeFlippedToOff(t *testing.T) {
	m, cam, factory := launchedManager(t, RecordingModeContinuous)

	h := factory.handleFor("cam-1")
	if h == nil || !m.IsActive("cam-1") {
		t.Fatalf("expected cam-1 recording after StartAll")
	}

	cam.storage.set(keyRecordingMode, string(RecordingModeOff))
	m.Reconcile()

	if m.IsActive("cam-1") {
		t.Fatalf("expected cam-1 stopped after reconcile with mode off")
	}
	if _, stopped := h.snapshot(); stopped != 1 {
		t.Fatalf("expected cam-1 handle stopped once, got %d", stopped)
	}
}

// TestReconcile_NoChurnForHealthyActiveCamera proves reconcile is a no-op for
// a camera already in the desired state — it must never restart a healthy
// ffmpeg process on every tick.
func TestReconcile_NoChurnForHealthyActiveCamera(t *testing.T) {
	m, _, factory := launchedManager(t, RecordingModeContinuous)

	m.Reconcile()
	m.Reconcile()

	h := factory.handleFor("cam-1")
	started, stopped := h.snapshot()
	if started != 1 || stopped != 0 {
		t.Fatalf("expected healthy active camera untouched (started=1 stopped=0), got started=%d stopped=%d", started, stopped)
	}
	if factory.callCount() != 1 {
		t.Fatalf("expected exactly 1 recorder ever built, got %d", factory.callCount())
	}
}

// TestReconcile_NoOpBeforeLaunch proves Reconcile does nothing before
// StartAll has run — there is nothing launched to reconcile against yet.
func TestReconcile_NoOpBeforeLaunch(t *testing.T) {
	m := NewRecorderManager()
	cam := newFakeCamera("cam-1", "Front Door", RecordingModeContinuous)
	if err := m.Configure([]ManagedCamera{cam}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())
	// No StartAll → not launched.

	m.Reconcile()

	if factory.callCount() != 0 {
		t.Fatalf("expected reconcile to be a no-op before StartAll, built %d recorders", factory.callCount())
	}
}

// TestStartReconcile_TickRunsReconcileThenStopsCleanly wires the injectable
// fake ticker, fires one tick after flipping the camera to continuous, and
// asserts the background loop ran Reconcile (camera becomes active) and then
// StopReconcile returns cleanly (the goroutine exits + the ticker is Stopped).
func TestStartReconcile_TickRunsReconcileThenStopsCleanly(t *testing.T) {
	m, cam, _ := launchedManager(t, RecordingModeOff)

	ft := newFakeTicker()
	m.reconcileNewTicker = func(time.Duration) ticker { return ft }
	m.StartReconcile(time.Minute)

	cam.storage.set(keyRecordingMode, string(RecordingModeContinuous))
	ft.ch <- time.Now() // fire one tick → Reconcile runs on the loop goroutine

	waitForCondition(t, 2*time.Second, func() bool { return m.IsActive("cam-1") })

	m.StopReconcile() // must return: goroutine exits, ticker Stopped

	select {
	case <-ft.stopCalls:
	default:
		t.Fatalf("expected StopReconcile to Stop the ticker")
	}

	// Idempotent second stop is a no-op (must not block or panic).
	m.StopReconcile()
}
