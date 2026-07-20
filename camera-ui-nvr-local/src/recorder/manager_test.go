package recorder

import (
	"reflect"
	"testing"

	sdk "github.com/cameraui/sdk/go"
)

// fakeCameraStorage is an in-memory stand-in for a camera's
// *sdk.DeviceStorage. It mirrors the one behavior RecorderManager depends
// on: DefineSchemas seeds a key's default value the first time it's
// declared, without clobbering a value already present (matching
// sdk.DeviceStorage.DefineSchemas's merge-with-existing-values behavior in
// storage.go).
type fakeCameraStorage struct {
	values map[string]any
}

func newFakeCameraStorage() *fakeCameraStorage {
	return &fakeCameraStorage{values: make(map[string]any)}
}

func (f *fakeCameraStorage) DefineSchemas(schemas []sdk.JsonSchema) {
	for _, schema := range schemas {
		if _, exists := f.values[schema.Key]; exists || schema.DefaultValue == nil {
			continue
		}
		f.values[schema.Key] = schema.DefaultValue
	}
}

func (f *fakeCameraStorage) GetValue(key string, defaultValue ...any) any {
	if v, ok := f.values[key]; ok {
		return v
	}
	if len(defaultValue) > 0 {
		return defaultValue[0]
	}
	return nil
}

func (f *fakeCameraStorage) set(key string, value any) { f.values[key] = value }

// fakeCamera implements ManagedCamera for tests. Real *sdk.CameraDevice
// cannot be constructed outside package sdk (newCameraDeviceProxy is
// unexported), which is exactly why RecorderManager depends on
// ManagedCamera instead.
type fakeCamera struct {
	id      string
	name    string
	storage *fakeCameraStorage

	// streamURL backs StreamURL below; defaults (in newFakeCamera) to a
	// fixed, deterministic URL per role so tests that don't care about the
	// actual value (most of them) don't need to set it themselves.
	streamURL func(role string) (string, error)

	// sourceRoles backs SourceRoles below. Left nil by newFakeCamera — a
	// camera reporting no sources at all — so every pre-existing test
	// (which doesn't set this) keeps exercising the "no sources: keep
	// configured/default roles as-is" fallback rather than any
	// intersection/fallback-to-camera-roles behavior it isn't testing.
	// Tests exercising SourceRoles set this field directly.
	sourceRoles []string
}

func (f *fakeCamera) ID() string             { return f.id }
func (f *fakeCamera) Name() string           { return f.name }
func (f *fakeCamera) Storage() CameraStorage { return f.storage }
func (f *fakeCamera) StreamURL(role string) (string, error) {
	return f.streamURL(role)
}
func (f *fakeCamera) SourceRoles() []string { return f.sourceRoles }

func newFakeCamera(id, name string, mode RecordingMode) *fakeCamera {
	storage := newFakeCameraStorage()
	if mode != "" {
		storage.set(keyRecordingMode, string(mode))
	}
	return &fakeCamera{
		id:      id,
		name:    name,
		storage: storage,
		streamURL: func(role string) (string, error) {
			return "rtsp://" + id + "/" + role, nil
		},
	}
}

func TestConfigure_OnlyNonOffCamerasAreManaged(t *testing.T) {
	m := NewRecorderManager()
	continuous := newFakeCamera("cam-1", "Front Door", RecordingModeContinuous)
	off := newFakeCamera("cam-2", "Garage", RecordingModeOff)

	if err := m.Configure([]ManagedCamera{continuous, off}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	ids := m.ManagedCameraIDs()
	if len(ids) != 1 || ids[0] != "cam-1" {
		t.Fatalf("expected only cam-1 managed, got %v", ids)
	}
}

func TestConfigure_ReplacesPreviousSet(t *testing.T) {
	m := NewRecorderManager()
	if err := m.Configure([]ManagedCamera{newFakeCamera("cam-1", "A", RecordingModeContinuous)}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := m.Configure([]ManagedCamera{newFakeCamera("cam-2", "B", RecordingModeEvents)}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	ids := m.ManagedCameraIDs()
	if len(ids) != 1 || ids[0] != "cam-2" {
		t.Fatalf("expected only cam-2 managed after re-Configure, got %v", ids)
	}
	if _, ok := m.Camera("cam-1"); ok {
		t.Fatalf("expected cam-1 to be gone after re-Configure")
	}
}

func TestAddRemove_UpdatesManagedSet(t *testing.T) {
	m := NewRecorderManager()
	cam := newFakeCamera("cam-1", "Front Door", RecordingModeEvents)

	if err := m.Add(cam); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if ids := m.ManagedCameraIDs(); len(ids) != 1 || ids[0] != "cam-1" {
		t.Fatalf("expected cam-1 managed after Add, got %v", ids)
	}

	if err := m.Remove("cam-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if ids := m.ManagedCameraIDs(); len(ids) != 0 {
		t.Fatalf("expected no managed cameras after Remove, got %v", ids)
	}
}

func TestRemove_UnknownCameraIsNoop(t *testing.T) {
	m := NewRecorderManager()
	if err := m.Remove("does-not-exist"); err != nil {
		t.Fatalf("expected Remove of unknown camera to be a no-op, got error: %v", err)
	}
}

func TestManagedCameraIDs_NeverNilOnZeroValueManager(t *testing.T) {
	m := NewRecorderManager()
	ids := m.ManagedCameraIDs()
	if ids == nil {
		t.Fatalf("expected a non-nil empty slice, got nil")
	}
	if len(ids) != 0 {
		t.Fatalf("expected 0 managed cameras, got %d", len(ids))
	}
}

func TestManagedCameraIDs_SortedForStableOutput(t *testing.T) {
	m := NewRecorderManager()
	_ = m.Configure([]ManagedCamera{
		newFakeCamera("cam-z", "Z", RecordingModeContinuous),
		newFakeCamera("cam-a", "A", RecordingModeContinuous),
		newFakeCamera("cam-m", "M", RecordingModeEvents),
	})

	ids := m.ManagedCameraIDs()
	want := []string{"cam-a", "cam-m", "cam-z"}
	if len(ids) != len(want) {
		t.Fatalf("expected %v, got %v", want, ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("expected sorted %v, got %v", want, ids)
		}
	}
}

func TestCamera_ReturnsConfiguredRecorder(t *testing.T) {
	m := NewRecorderManager()
	cam := newFakeCamera("cam-1", "Front Door", RecordingModeContinuous)
	if err := m.Add(cam); err != nil {
		t.Fatalf("Add: %v", err)
	}

	r, ok := m.Camera("cam-1")
	if !ok {
		t.Fatalf("expected cam-1 to be found")
	}
	if r.CameraID != "cam-1" || r.Name != "Front Door" || r.Config.Mode != RecordingModeContinuous {
		t.Fatalf("unexpected recorder: %+v", r)
	}

	if _, ok := m.Camera("missing"); ok {
		t.Fatalf("expected an unregistered camera id to be absent")
	}
}

func TestReadRecordingConfig_DefaultsAppliedWhenUnset(t *testing.T) {
	storage := newFakeCameraStorage() // nothing stored yet

	cfg := readRecordingConfig(storage)

	if cfg.Mode != RecordingModeOff {
		t.Fatalf("expected default mode %q, got %q", RecordingModeOff, cfg.Mode)
	}
	if cfg.RetentionDays != defaultRetentionDays {
		t.Fatalf("expected default retentionDays %d, got %d", defaultRetentionDays, cfg.RetentionDays)
	}
	if cfg.PreRollS != defaultPreRollS {
		t.Fatalf("expected default preRollS %d, got %d", defaultPreRollS, cfg.PreRollS)
	}
	if cfg.PostRollS != defaultPostRollS {
		t.Fatalf("expected default postRollS %d, got %d", defaultPostRollS, cfg.PostRollS)
	}
	if len(cfg.Roles) != 1 || cfg.Roles[0] != string(sdk.CameraRoleHighRes) {
		t.Fatalf("expected default roles %v, got %v", defaultRoles, cfg.Roles)
	}
}

func TestReadRecordingConfig_StoredValuesOverrideDefaults(t *testing.T) {
	storage := newFakeCameraStorage()
	storage.set(keyRecordingMode, string(RecordingModeContinuous))
	storage.set(keyRetentionDays, float64(30))
	storage.set(keyPreRollS, float64(3))
	storage.set(keyPostRollS, float64(15))
	storage.set(keyRoles, []string{"low-resolution"})

	cfg := readRecordingConfig(storage)

	if cfg.Mode != RecordingModeContinuous {
		t.Fatalf("expected mode continuous, got %q", cfg.Mode)
	}
	if cfg.RetentionDays != 30 || cfg.PreRollS != 3 || cfg.PostRollS != 15 {
		t.Fatalf("expected stored numeric overrides, got %+v", cfg)
	}
	if len(cfg.Roles) != 1 || cfg.Roles[0] != "low-resolution" {
		t.Fatalf("expected stored roles override, got %v", cfg.Roles)
	}
}

func TestReadRecordingConfig_InvalidModeFallsBackToOff(t *testing.T) {
	storage := newFakeCameraStorage()
	storage.set(keyRecordingMode, "not-a-real-mode")

	cfg := readRecordingConfig(storage)

	if cfg.Mode != RecordingModeOff {
		t.Fatalf("expected an invalid stored mode to fall back to %q, got %q", RecordingModeOff, cfg.Mode)
	}
}

// TestReadRecordingConfig_EmptyStoredRolesFallsBackToDefault is the
// regression test for the production bug: DefineSchemas seeded keyRoles with
// no DefaultValue, so a camera that was ever loaded before this fix has an
// explicitly-stored EMPTY roles value (not "unset") on disk. A stored value —
// even an empty one — beats stringSliceValue's variadic default, so
// readRecordingConfig used to return Roles=[] for every such camera, and the
// recorder started with "no configured roles; nothing to record". Storing an
// empty []string here (as opposed to just never calling storage.set at all,
// which TestReadRecordingConfig_DefaultsAppliedWhenUnset already covers) is
// what reproduces the actual on-disk state that broke production.
func TestReadRecordingConfig_EmptyStoredRolesFallsBackToDefault(t *testing.T) {
	storage := newFakeCameraStorage()
	storage.set(keyRoles, []string{})

	cfg := readRecordingConfig(storage)

	if len(cfg.Roles) != 1 || cfg.Roles[0] != string(sdk.CameraRoleHighRes) {
		t.Fatalf("expected a stored-empty roles value to fall back to defaultRoles %v, got %v", defaultRoles, cfg.Roles)
	}
}

// TestRecordingConfigSchema_RolesHasDefaultValue guards against the
// production bug recurring for any camera that hasn't stored a roles value
// yet: keyRoles' schema entry must declare DefaultValue so
// DeviceStorage.DefineSchemas seeds it with defaultRoles up front, the same
// way every other recordingConfigSchema entry already does.
func TestRecordingConfigSchema_RolesHasDefaultValue(t *testing.T) {
	for _, s := range recordingConfigSchema() {
		if s.Key != keyRoles {
			continue
		}
		got, ok := s.DefaultValue.([]string)
		if !ok {
			t.Fatalf("expected keyRoles schema DefaultValue to be a []string, got %T (%v)", s.DefaultValue, s.DefaultValue)
		}
		if !reflect.DeepEqual(got, defaultRoles) {
			t.Fatalf("expected keyRoles schema DefaultValue %v, got %v", defaultRoles, got)
		}
		return
	}
	t.Fatalf("expected a schema entry for key %q", keyRoles)
}

// TestResolveRoles exercises resolveRoles directly (the intersect/fallback
// logic startRecorder applies to a camera's configured/default roles against
// its actual SourceRoles) without needing the full manager start path.
func TestResolveRoles(t *testing.T) {
	cases := []struct {
		name                 string
		configured, available, want []string
	}{
		{
			name:       "no sources at all keeps configured/default roles as-is",
			configured: []string{"high-resolution"},
			available:  nil,
			want:       []string{"high-resolution"},
		},
		{
			name:       "configured role offered by the camera is preserved",
			configured: []string{"low-resolution"},
			available:  []string{"high-resolution", "low-resolution"},
			want:       []string{"low-resolution"},
		},
		{
			name:       "configured role not offered falls back to the camera's actual roles",
			configured: []string{"high-resolution"},
			available:  []string{"main-hd"},
			want:       []string{"main-hd"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveRoles(c.configured, c.available)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("resolveRoles(%v, %v) = %v, want %v", c.configured, c.available, got, c.want)
			}
		})
	}
}
