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

	sdk "github.com/cameraui/sdk/go"
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
// map. all additionally keeps EVERY handle ever created (not just the most
// recent per camera), for the concurrency/leak tests below, which need to
// inspect every handle a camera ID ever had, not just the latest.
type fakeRecorderFactory struct {
	mu      sync.Mutex
	calls   []RecorderConfig
	handles map[string]*fakeRecorderHandle
	all     []*fakeRecorderHandle
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
		f.all = append(f.all, h)
		return h
	}
}

// allHandlesFor returns every handle ever created for cameraID, in creation
// order.
func (f *fakeRecorderFactory) allHandlesFor(cameraID string) []*fakeRecorderHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeRecorderHandle
	for _, h := range f.all {
		h.mu.Lock()
		id := h.cfg.CameraID
		h.mu.Unlock()
		if id == cameraID {
			out = append(out, h)
		}
	}
	return out
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

// TestStartAll_StoredEmptyRolesFallBackToDefaultAndCameraSources is the
// full-start-path regression test for the production bug reported live: a
// camera with recordingMode=continuous whose stored roles value is an
// explicitly-empty []string (every already-loaded camera, pre-fix — see
// TestReadRecordingConfig_EmptyStoredRolesFallsBackToDefault for the
// readRecordingConfig-level version of this same scenario) must still end up
// with a non-empty RecorderConfig.Roles once it flows through StartAll: the
// empty stored value falls back to defaultRoles ("high-resolution"), which
// this camera's SourceRoles actually offers, so it's used as-is.
func TestStartAll_StoredEmptyRolesFallBackToDefaultAndCameraSources(t *testing.T) {
	m := NewRecorderManager()
	cam := newFakeCamera("cam-1", "Front Door", RecordingModeContinuous)
	cam.storage.set(keyRoles, []string{}) // the production bug: stored-empty
	cam.sourceRoles = []string{string(sdk.CameraRoleHighRes)}
	if err := m.Configure([]ManagedCamera{cam}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	cfg, ok := factory.configFor("cam-1")
	if !ok {
		t.Fatalf("expected a RecorderConfig built for cam-1")
	}
	if len(cfg.Roles) != 1 || cfg.Roles[0] != string(sdk.CameraRoleHighRes) {
		t.Fatalf("production bug: expected Roles to fall back to defaultRoles %v, got %v", defaultRoles, cfg.Roles)
	}
}

// TestStartAll_ConfiguredRoleNotOnCamera_FallsBackToCameraSourceRoles covers
// a camera whose configured/default role ("high-resolution") isn't one its
// actual sources report — e.g. a non-amcrest source named "main-hd" instead
// — expecting the built RecorderConfig to fall back to the camera's real
// SourceRoles rather than recording nothing.
func TestStartAll_ConfiguredRoleNotOnCamera_FallsBackToCameraSourceRoles(t *testing.T) {
	m := NewRecorderManager()
	cam := newFakeCamera("cam-1", "Front Door", RecordingModeContinuous)
	cam.sourceRoles = []string{"main-hd"}
	if err := m.Configure([]ManagedCamera{cam}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	cfg, ok := factory.configFor("cam-1")
	if !ok {
		t.Fatalf("expected a RecorderConfig built for cam-1")
	}
	if len(cfg.Roles) != 1 || cfg.Roles[0] != "main-hd" {
		t.Fatalf("expected fallback to the camera's actual source roles [main-hd], got %v", cfg.Roles)
	}
}

// TestStartAll_ConfiguredRoleOnCamera_IntersectionPreserved covers a camera
// whose configured role IS one of its actual sources: the configured value
// must be used as-is, not widened to every source role the camera offers.
func TestStartAll_ConfiguredRoleOnCamera_IntersectionPreserved(t *testing.T) {
	m := NewRecorderManager()
	cam := newFakeCamera("cam-1", "Front Door", RecordingModeContinuous)
	cam.storage.set(keyRoles, []string{"low-resolution"})
	cam.sourceRoles = []string{string(sdk.CameraRoleHighRes), "low-resolution"}
	if err := m.Configure([]ManagedCamera{cam}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	cfg, ok := factory.configFor("cam-1")
	if !ok {
		t.Fatalf("expected a RecorderConfig built for cam-1")
	}
	if len(cfg.Roles) != 1 || cfg.Roles[0] != "low-resolution" {
		t.Fatalf("expected the configured role preserved as-is [low-resolution], got %v", cfg.Roles)
	}
}

// TestStartAll_NoCameraSources_KeepsConfiguredRoles covers a camera that
// reports no sources at all (SourceRoles empty) — recorder.go's own
// stream-error logging is the existing safety net for that case, so this
// path must keep the configured/default roles as a best effort rather than
// e.g. collapsing to no roles.
func TestStartAll_NoCameraSources_KeepsConfiguredRoles(t *testing.T) {
	m := NewRecorderManager()
	cam := newFakeCamera("cam-1", "Front Door", RecordingModeContinuous) // sourceRoles left nil
	if err := m.Configure([]ManagedCamera{cam}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	cfg, ok := factory.configFor("cam-1")
	if !ok {
		t.Fatalf("expected a RecorderConfig built for cam-1")
	}
	if len(cfg.Roles) != 1 || cfg.Roles[0] != string(sdk.CameraRoleHighRes) {
		t.Fatalf("expected configured/default roles kept when the camera reports no sources, got %v", cfg.Roles)
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

// TestAdd_ConcurrentSameCameraID_NoHandleLeak is the review-fix regression
// test for the concurrent-restart handle leak: syncRecording's stop-old/
// start-new sequence used to run as two independently-locked map operations
// with no exclusion between them for a given camera ID, so two overlapping
// Add calls for the SAME id could each successfully Start a handle, with
// the loser's handle silently overwritten (and never Stopped) in m.active
// once the winner's write ran after it — a goroutine/ffmpeg leak invisible
// to StopAll/Shutdown, since the leaked handle is no longer reachable from
// m.active at all.
//
// Fires a burst of concurrent Add calls (occasionally interleaved with a
// Remove) for one camera ID from multiple goroutines, then asserts the
// invariant that must hold regardless of interleaving: for every handle
// fakeRecorderFactory ever built for that camera, started == stopped + (1
// if it's still the one currently tracked in m.active, else 0) — i.e.
// nothing was ever started without eventually being stopped (or still
// legitimately running as the sole survivor).
func TestAdd_ConcurrentSameCameraID_NoHandleLeak(t *testing.T) {
	m := NewRecorderManager()
	factory := newFakeRecorderFactory()
	m.ConfigureRecording("/data", 0, factory.factory())
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	const cameraID = "cam-1"
	const n = 40

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			if i%7 == 0 {
				_ = m.Remove(cameraID)
				return
			}
			_ = m.Add(newFakeCamera(cameraID, "A", RecordingModeContinuous))
		}(i)
	}
	wg.Wait()

	m.mu.Lock()
	activeHandle, stillActive := m.active[cameraID]
	activeCount := len(m.active)
	m.mu.Unlock()

	if activeCount > 1 {
		t.Fatalf("expected at most 1 active handle tracked across all cameras, got %d", activeCount)
	}

	handles := factory.allHandlesFor(cameraID)
	if len(handles) == 0 {
		t.Fatalf("expected at least one handle to have been created for %s", cameraID)
	}

	for i, h := range handles {
		started, stopped := h.snapshot()
		wantActive := 0
		if stillActive && h == activeHandle {
			wantActive = 1
		}
		if started != stopped+wantActive {
			t.Errorf("handle %d leaked: started=%d stopped=%d stillActive=%v (want started == stopped + %d)", i, started, stopped, h == activeHandle, wantActive)
		}
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
