// manager_orch_test.go tests Task ORCH's orchestration additions to
// RecorderManager: ConfigureRecording/StartAll/StopAll and Add/Remove's new
// start/restart/stop side effects. Every test here uses fakeRecorderFactory/
// fakeRecorderHandle below instead of a real *Recorder — proving the
// orchestration logic itself (which camera gets a Recorder, when, with what
// config) without spawning ffmpeg, per the task's testability constraint.
package recorder

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// fakeRecorderHandle is an injectable stand-in for a live *Recorder,
// recording how many times Start/Stop were called (and letting a test force
// Start to fail) so orchestration tests can assert on lifecycle calls
// without a real ffmpeg process.
type fakeRecorderHandle struct {
	mu       sync.Mutex
	cfg      RecorderConfig
	started  int
	stopped  int
	startErr error
}

func (h *fakeRecorderHandle) Start(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started++
	return h.startErr
}

func (h *fakeRecorderHandle) Stop() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped++
	return nil
}

func (h *fakeRecorderHandle) snapshot() (started, stopped int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started, h.stopped
}

// fakeRecorderFactory is an injectable RecorderFactory that records every
// RecorderConfig it was called with and keeps the most recently created
// fakeRecorderHandle per camera ID, so a test can find "the handle currently
// backing this camera" without RecorderManager exposing its internal active
// map.
type fakeRecorderFactory struct {
	mu      sync.Mutex
	calls   []RecorderConfig
	handles map[string]*fakeRecorderHandle
}

func newFakeRecorderFactory() *fakeRecorderFactory {
	return &fakeRecorderFactory{handles: make(map[string]*fakeRecorderHandle)}
}

func (f *fakeRecorderFactory) factory() RecorderFactory {
	return func(cfg RecorderConfig) RecorderHandle {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, cfg)
		h := &fakeRecorderHandle{cfg: cfg}
		f.handles[cfg.CameraID] = h
		return h
	}
}

func (f *fakeRecorderFactory) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRecorderFactory) handleFor(cameraID string) *fakeRecorderHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handles[cameraID]
}

func (f *fakeRecorderFactory) configFor(cameraID string) (RecorderConfig, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Last matching call wins, mirroring handleFor's "most recent" contract.
	var cfg RecorderConfig
	found := false
	for _, c := range f.calls {
		if c.CameraID == cameraID {
			cfg = c
			found = true
		}
	}
	return cfg, found
}

// ---------------------------------------------------------------------------
// StartAll
// ---------------------------------------------------------------------------

func TestStartAll_StartsAndBuildsConfigForManagedCamerasOnly(t *testing.T) {
	m := NewRecorderManager()
	continuous := newFakeCamera("cam-1", "Front Door", RecordingModeContinuous)
	off := newFakeCamera("cam-2", "Garage", RecordingModeOff)
	if err := m.Configure([]ManagedCamera{continuous, off}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())

	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	if got := factory.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 recorder built (only the non-off camera), got %d", got)
	}
	handle := factory.handleFor("cam-1")
	if handle == nil {
		t.Fatalf("expected a recorder handle for cam-1")
	}
	if started, _ := handle.snapshot(); started != 1 {
		t.Fatalf("expected cam-1's handle Start called once, got %d", started)
	}
	if factory.handleFor("cam-2") != nil {
		t.Fatalf("expected no recorder built for the off-mode camera")
	}
}

func TestStartAll_BuildsConfigFromCameraRecordingConfigAndDefaults(t *testing.T) {
	m := NewRecorderManager()
	cam := newFakeCamera("cam-1", "Front Door", RecordingModeEvents)
	cam.storage.set(keyPreRollS, float64(7))
	cam.storage.set(keyPostRollS, float64(20))
	cam.storage.set(keyRoles, []string{"low-resolution"})
	if err := m.Configure([]ManagedCamera{cam}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data/nvr", 0, factory.factory())

	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	cfg, ok := factory.configFor("cam-1")
	if !ok {
		t.Fatalf("expected a RecorderConfig built for cam-1")
	}
	if cfg.DataDir != "/data/nvr" {
		t.Errorf("expected DataDir %q, got %q", "/data/nvr", cfg.DataDir)
	}
	if cfg.SegmentSeconds != defaultSegmentSeconds {
		t.Errorf("expected default SegmentSeconds %d, got %d", defaultSegmentSeconds, cfg.SegmentSeconds)
	}
	if cfg.Mode != RecordingModeEvents {
		t.Errorf("expected Mode events, got %q", cfg.Mode)
	}
	if cfg.PreRollS != 7 || cfg.PostRollS != 20 {
		t.Errorf("expected PreRollS=7 PostRollS=20, got PreRollS=%d PostRollS=%d", cfg.PreRollS, cfg.PostRollS)
	}
	if len(cfg.Roles) != 1 || cfg.Roles[0] != "low-resolution" {
		t.Errorf("expected Roles [low-resolution], got %v", cfg.Roles)
	}
	if cfg.StreamURL == nil {
		t.Fatalf("expected a non-nil StreamURL closure")
	}
	url, err := cfg.StreamURL("low-resolution")
	if err != nil || url != "rtsp://cam-1/low-resolution" {
		t.Errorf("expected StreamURL to call through to the camera's StreamURL, got (%q, %v)", url, err)
	}
}

func TestStartAll_ExplicitSegmentSecondsOverridesDefault(t *testing.T) {
	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newFakeCamera("cam-1", "A", RecordingModeContinuous)}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 30, factory.factory())

	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	cfg, ok := factory.configFor("cam-1")
	if !ok || cfg.SegmentSeconds != 30 {
		t.Fatalf("expected explicit SegmentSeconds 30, got %+v (found=%v)", cfg, ok)
	}
}

func TestStartAll_NoOpWhenRecordingNotConfigured(t *testing.T) {
	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newFakeCamera("cam-1", "A", RecordingModeContinuous)}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if err := m.StartAll(); err != nil {
		t.Fatalf("expected StartAll to no-op cleanly when ConfigureRecording was never called, got %v", err)
	}
}

func TestStartAll_SecondCallIsNoop(t *testing.T) {
	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newFakeCamera("cam-1", "A", RecordingModeContinuous)}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())

	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll (1st): %v", err)
	}
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll (2nd): %v", err)
	}

	if got := factory.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 recorder built across both StartAll calls, got %d", got)
	}
}

func TestStartAll_CollectsPerCameraStartErrorsWithoutAbortingOthers(t *testing.T) {
	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{
		newFakeCamera("cam-1", "A", RecordingModeContinuous),
		newFakeCamera("cam-2", "B", RecordingModeContinuous),
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	wantErr := errors.New("boom")
	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, func(cfg RecorderConfig) RecorderHandle {
		h := &fakeRecorderHandle{cfg: cfg}
		if cfg.CameraID == "cam-1" {
			h.startErr = wantErr
		}
		factory.mu.Lock()
		factory.calls = append(factory.calls, cfg)
		factory.handles[cfg.CameraID] = h
		factory.mu.Unlock()
		return h
	})

	err := m.StartAll()
	if err == nil {
		t.Fatalf("expected StartAll to report cam-1's start error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("expected returned error to wrap %v, got %v", wantErr, err)
	}

	if h := factory.handleFor("cam-2"); h == nil {
		t.Fatalf("expected cam-2 to still get a recorder despite cam-1 failing")
	} else if started, _ := h.snapshot(); started != 1 {
		t.Errorf("expected cam-2's handle to have been started, got %d", started)
	}
}

// ---------------------------------------------------------------------------
// Add / Remove after launch: start, restart, stop
// ---------------------------------------------------------------------------

func TestAdd_BeforeLaunch_DoesNotStartAnything(t *testing.T) {
	m := NewRecorderManager()
	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())

	if err := m.Add(newFakeCamera("cam-1", "A", RecordingModeContinuous)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if got := factory.callCount(); got != 0 {
		t.Fatalf("expected no recorder built before StartAll, got %d calls", got)
	}

	// StartAll, once it does run, must still pick up the camera Add
	// registered earlier.
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if got := factory.callCount(); got != 1 {
		t.Fatalf("expected StartAll to start the pre-launch-added camera, got %d calls", got)
	}
}

func TestAdd_AfterLaunch_StartsRecorderForNewCamera(t *testing.T) {
	m := NewRecorderManager()
	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	if err := m.Add(newFakeCamera("cam-1", "A", RecordingModeContinuous)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	handle := factory.handleFor("cam-1")
	if handle == nil {
		t.Fatalf("expected Add (after launch) to start a recorder for the new camera")
	}
	if started, _ := handle.snapshot(); started != 1 {
		t.Fatalf("expected 1 Start call, got %d", started)
	}
}

func TestAdd_AfterLaunch_OffModeCameraNeverStarts(t *testing.T) {
	m := NewRecorderManager()
	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	if err := m.Add(newFakeCamera("cam-1", "A", RecordingModeOff)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if got := factory.callCount(); got != 0 {
		t.Fatalf("expected no recorder built for an off-mode camera, got %d", got)
	}
}

func TestAdd_AfterLaunch_ConfigChangeRestartsRecorder(t *testing.T) {
	m := NewRecorderManager()
	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	cam := newFakeCamera("cam-1", "A", RecordingModeContinuous)
	if err := m.Add(cam); err != nil {
		t.Fatalf("Add (1st): %v", err)
	}
	firstHandle := factory.handleFor("cam-1")
	if firstHandle == nil {
		t.Fatalf("expected a recorder handle after the first Add")
	}

	// Simulate a config edit (e.g. roles changed) by re-adding the same
	// camera ID with a different stored config, exactly like Add's own doc
	// comment describes as the "config change" path.
	cam.storage.set(keyRoles, []string{"low-resolution"})
	if err := m.Add(cam); err != nil {
		t.Fatalf("Add (2nd, config change): %v", err)
	}

	if stopped, _ := firstHandle.snapshot(); stopped != 1 {
		t.Errorf("expected the first handle to be stopped exactly once on restart, got %d", stopped)
	}

	secondHandle := factory.handleFor("cam-1")
	if secondHandle == firstHandle {
		t.Fatalf("expected a distinct recorder handle after the restart")
	}
	if started, _ := secondHandle.snapshot(); started != 1 {
		t.Errorf("expected the second handle's Start to have been called, got %d", started)
	}
	if got := factory.callCount(); got != 2 {
		t.Fatalf("expected exactly 2 recorders built (initial + restart), got %d", got)
	}
}

func TestRemove_AfterLaunch_StopsAndDeregistersRecorder(t *testing.T) {
	m := NewRecorderManager()
	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if err := m.Add(newFakeCamera("cam-1", "A", RecordingModeContinuous)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	handle := factory.handleFor("cam-1")

	if err := m.Remove("cam-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if stopped, _ := handle.snapshot(); stopped != 1 {
		t.Fatalf("expected Remove to stop the camera's active recorder, got %d stop calls", stopped)
	}
	if ids := m.ManagedCameraIDs(); len(ids) != 0 {
		t.Fatalf("expected cam-1 to be gone from the managed set after Remove, got %v", ids)
	}
}

func TestRemove_UnknownOrInactiveCameraIsNoop(t *testing.T) {
	m := NewRecorderManager()
	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	if err := m.Remove("does-not-exist"); err != nil {
		t.Fatalf("expected Remove of an unmanaged camera to be a no-op, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// StopAll
// ---------------------------------------------------------------------------

func TestStopAll_StopsEveryActiveRecorder(t *testing.T) {
	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{
		newFakeCamera("cam-1", "A", RecordingModeContinuous),
		newFakeCamera("cam-2", "B", RecordingModeEvents),
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	m.StopAll()

	for _, id := range []string{"cam-1", "cam-2"} {
		h := factory.handleFor(id)
		if h == nil {
			t.Fatalf("expected a recorder handle for %s", id)
		}
		if stopped, _ := h.snapshot(); stopped != 1 {
			t.Errorf("expected %s's handle to be stopped exactly once, got %d", id, stopped)
		}
	}
}

func TestStopAll_NoOpWhenNothingWasStarted(t *testing.T) {
	m := NewRecorderManager()
	// Neither Configure nor ConfigureRecording nor StartAll called at all.
	m.StopAll() // must not panic
}

func TestStopAll_ThenStartAllStartsFreshRecorders(t *testing.T) {
	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newFakeCamera("cam-1", "A", RecordingModeContinuous)}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())

	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll (1st): %v", err)
	}
	m.StopAll()
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll (2nd, after StopAll): %v", err)
	}

	if got := factory.callCount(); got != 2 {
		t.Fatalf("expected StopAll to allow a subsequent StartAll to start fresh recorders, got %d calls total", got)
	}
}
