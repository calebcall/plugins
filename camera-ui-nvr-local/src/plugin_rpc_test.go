package main

import (
	"testing"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/recorder"
)

// fakeInstanceStore is an in-memory stand-in for sdk.DeviceStorage. It
// models the one behavior that matters for GetInstanceId's persistence
// contract: sdk.DeviceStorage.SetValue silently no-ops when no schema has
// been declared for the key (storage.go: "schema := ds.findSchemaByKey(key);
// if schema == nil { return nil }"). A naive map-backed fake that always
// writes would pass even if the plugin never registers a schema — which is
// exactly the bug the second review pass caught (GetInstanceId "persisted"
// via SetValue, but with no schema declared the write was silently dropped,
// so every call actually re-generated a new UUID). declareSchema marks a key
// as schema-backed, the same way DefineSchemas does for the real store.
type fakeInstanceStore struct {
	values  map[string]any
	schemas map[string]bool
}

func newFakeInstanceStore() *fakeInstanceStore {
	return &fakeInstanceStore{values: make(map[string]any), schemas: make(map[string]bool)}
}

// declareSchema marks key as schema-backed, mirroring what
// DeviceStorage.DefineSchemas does for the real store before SetValue will
// honor writes to that key.
func (s *fakeInstanceStore) declareSchema(key string) {
	s.schemas[key] = true
}

func (s *fakeInstanceStore) GetValue(key string, defaultValue ...any) any {
	if v, ok := s.values[key]; ok {
		return v
	}
	if len(defaultValue) > 0 {
		return defaultValue[0]
	}
	return nil
}

// SetValue mirrors sdk.DeviceStorage.SetValue's schema gating: a key with no
// declared schema is a silent no-op, same as the real store.
func (s *fakeInstanceStore) SetValue(key string, value any) error {
	if !s.schemas[key] {
		return nil
	}
	s.values[key] = value
	return nil
}

// declarePluginSchemas registers store's schemas from p.StorageSchema(),
// mirroring exactly what sdk.Run does after construction (constructor ->
// StorageSchema() -> DefineSchemas() -> only then RegisterHandler). Using
// the plugin's own StorageSchema() here — rather than hardcoding
// instanceIDStorageKey — means this test setup breaks (and the tests below
// go red) if StorageSchema is ever removed or stops declaring the key,
// which is the regression this exists to catch.
func declarePluginSchemas(p *NVRPlugin, store *fakeInstanceStore) {
	for _, schema := range p.StorageSchema() {
		store.declareSchema(schema.Key)
	}
}

// newTestPlugin constructs an NVRPlugin suitable for unit-testing RPC
// methods that don't touch the live sdk.PluginAPI (getManagedCameraIds,
// getInstanceId). BasePlugin's Logger/API/Storage are left nil deliberately
// — a live *sdk.PluginAPI requires a connected NATS client, which unit tests
// don't have. GetInstanceId only needs storage, so it's wired to an
// in-memory fake (fakeInstanceStore) with its schema declared exactly as
// sdk.Run would declare it for the real store.
func newTestPlugin(t *testing.T) *NVRPlugin {
	t.Helper()
	store := newFakeInstanceStore()
	p := &NVRPlugin{recorder: recorder.NewRecorderManager(), store: store}
	declarePluginSchemas(p, store)
	return p
}

func TestGetManagedCameraIds_EmptyByDefault(t *testing.T) {
	p := newTestPlugin(t)
	ids, err := p.GetManagedCameraIds()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ids == nil {
		t.Fatalf("expected a non-nil empty slice, got nil")
	}
	if len(ids) != 0 {
		t.Fatalf("expected 0 managed cameras, got %d", len(ids))
	}
}

// fakeManagedCameraStorage/fakeManagedCamera let this test delegate through
// the real recorder.RecorderManager instead of a stub, proving
// GetManagedCameraIds (rpc_recording.go) actually reflects p.recorder's
// state rather than a hardcoded value. They implement
// recorder.CameraStorage/recorder.ManagedCamera directly — see
// src/recorder/manager_test.go for the equivalent fakes used inside package
// recorder itself; these are duplicated here (rather than exported from
// recorder) because plugin_rpc_test.go, in package main, cannot reach
// recorder's unexported test-only types.
type fakeManagedCameraStorage struct{ values map[string]any }

func (f *fakeManagedCameraStorage) GetValue(key string, defaultValue ...any) any {
	if v, ok := f.values[key]; ok {
		return v
	}
	if len(defaultValue) > 0 {
		return defaultValue[0]
	}
	return nil
}

func (f *fakeManagedCameraStorage) DefineSchemas(schemas []sdk.JsonSchema) {
	for _, schema := range schemas {
		if _, exists := f.values[schema.Key]; exists || schema.DefaultValue == nil {
			continue
		}
		f.values[schema.Key] = schema.DefaultValue
	}
}

type fakeManagedCamera struct {
	id, name string
	storage  *fakeManagedCameraStorage
}

func newFakeManagedCamera(id, name, recordingMode string) *fakeManagedCamera {
	return &fakeManagedCamera{
		id:   id,
		name: name,
		storage: &fakeManagedCameraStorage{
			values: map[string]any{"recordingMode": recordingMode},
		},
	}
}

func (f *fakeManagedCamera) ID() string                      { return f.id }
func (f *fakeManagedCamera) Name() string                    { return f.name }
func (f *fakeManagedCamera) Storage() recorder.CameraStorage { return f.storage }
func (f *fakeManagedCamera) StreamURL(role string) (string, error) {
	return "rtsp://" + f.id + "/" + role, nil
}

// SourceRoles reports no sources: these tests only exercise
// GetManagedCameraIds delegation, not recorder-start role resolution (see
// recorder/manager_orch_test.go for that).
func (f *fakeManagedCamera) SourceRoles() []string { return nil }

func TestGetManagedCameraIds_DelegatesToRecorderManager(t *testing.T) {
	p := newTestPlugin(t)
	cam := newFakeManagedCamera("cam-1", "Front Door", "continuous")
	if err := p.recorder.Add(cam); err != nil {
		t.Fatalf("Add: %v", err)
	}
	off := newFakeManagedCamera("cam-2", "Garage", "off")
	if err := p.recorder.Add(off); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ids, err := p.GetManagedCameraIds()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 1 || ids[0] != "cam-1" {
		t.Fatalf("expected GetManagedCameraIds to reflect p.recorder's state ([cam-1]), got %v", ids)
	}
}

func TestGetManagedCameraIds_RPCMethodsAllowsIt(t *testing.T) {
	p := newTestPlugin(t)
	allowed := p.RPCMethods()
	found := false
	for _, name := range allowed {
		if name == "getManagedCameraIds" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected RPCMethods() to include %q, got %v", "getManagedCameraIds", allowed)
	}
}

// TestGetInstanceId_GeneratesPersistsAndIsStable covers the persistent-UUID
// contract end to end through a store whose schema was declared the same
// way sdk.Run declares it (via declarePluginSchemas -> p.StorageSchema()):
// with fresh/empty storage, the first call generates a non-empty value and
// actually writes it to storage under instanceIDStorageKey; a second call
// returns the identical value instead of generating a new one.
//
// This is the test that catches the missing-schema regression: verified by
// temporarily removing the declarePluginSchemas call in newTestPlugin (or
// deleting NVRPlugin.StorageSchema / its interface assertion) and
// re-running `go test ./src/... -run TestGetInstanceId_GeneratesPersistsAndIsStable -v`
// — with no schema declared, fakeInstanceStore.SetValue no-ops exactly like
// the real DeviceStorage, so the "second == first" assertion fails (a fresh
// UUID is generated on every call instead of one being persisted).
func TestGetInstanceId_GeneratesPersistsAndIsStable(t *testing.T) {
	store := newFakeInstanceStore()
	p := &NVRPlugin{recorder: recorder.NewRecorderManager(), store: store}
	declarePluginSchemas(p, store)

	first, err := p.GetInstanceId()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first == "" {
		t.Fatalf("expected a non-empty generated instance id")
	}

	stored, _ := store.values[instanceIDStorageKey].(string)
	if stored != first {
		t.Fatalf("expected generated id %q to be persisted under %q, got %q", first, instanceIDStorageKey, stored)
	}

	second, err := p.GetInstanceId()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if second != first {
		t.Fatalf("expected instance id to stay stable across calls: first=%q second=%q", first, second)
	}
}

// TestGetInstanceId_WithoutSchemaRegistrationDoesNotPersist pins down the
// exact bug the second review pass found: a store whose schema was never
// declared for instanceIDStorageKey (SetValue always no-ops, matching real
// sdk.DeviceStorage) must NOT be able to persist across calls — proving
// fakeInstanceStore's gating faithfully reproduces the real SDK's behavior,
// and that the fix genuinely depends on StorageSchema being registered
// rather than on some other incidental code path.
func TestGetInstanceId_WithoutSchemaRegistrationDoesNotPersist(t *testing.T) {
	store := newFakeInstanceStore() // schema deliberately NOT declared
	p := &NVRPlugin{recorder: recorder.NewRecorderManager(), store: store}

	first, err := p.GetInstanceId()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := p.GetInstanceId()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first == second {
		t.Fatalf("expected an undeclared-schema store to fail to persist (values should differ across calls), got the same value %q twice", first)
	}
}

// TestGetInstanceId_ReturnsExistingStoredValue covers the case where storage
// already holds a previously generated id (e.g. across a process restart):
// GetInstanceId must return it unchanged rather than generating a new one.
// (GetValue's read path is not schema-gated in the real SDK — only writes
// are — so this doesn't need declarePluginSchemas.)
func TestGetInstanceId_ReturnsExistingStoredValue(t *testing.T) {
	store := newFakeInstanceStore()
	store.values[instanceIDStorageKey] = "existing-uuid-value"
	p := &NVRPlugin{recorder: recorder.NewRecorderManager(), store: store}

	id, err := p.GetInstanceId()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "existing-uuid-value" {
		t.Fatalf("expected existing stored id to be returned unchanged, got %q", id)
	}
}

func TestGetInstanceId_RPCMethodsAllowsIt(t *testing.T) {
	p := newTestPlugin(t)
	allowed := p.RPCMethods()
	found := false
	for _, name := range allowed {
		if name == "getInstanceId" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected RPCMethods() to include %q, got %v", "getInstanceId", allowed)
	}
}

// TestStorageSchema_DeclaresInstanceIDKeyAsStored asserts NVRPlugin's
// StorageSchema (the thing sdk.Run feeds into DefineSchemas before any RPC
// call is possible) actually declares instanceIDStorageKey with Store: true
// — the two properties GetInstanceId's persistence depends on.
func TestStorageSchema_DeclaresInstanceIDKeyAsStored(t *testing.T) {
	p := &NVRPlugin{}
	var found bool
	for _, schema := range p.StorageSchema() {
		if schema.Key != instanceIDStorageKey {
			continue
		}
		found = true
		if schema.Store == nil || !*schema.Store {
			t.Fatalf("expected schema for %q to have Store: true, got %v", instanceIDStorageKey, schema.Store)
		}
	}
	if !found {
		t.Fatalf("expected StorageSchema() to declare a schema entry for %q", instanceIDStorageKey)
	}
}

// TestStorageSchema_DeclaresRecordingPathKeyAsStored proves NVRPlugin's
// StorageSchema also declares recordingPathStorageKey (Feature #1:
// configurable recording storage path) as a stored string field — what
// makes it both persist across restarts and actually render on the
// /settings/recordings page (SettingsRecordings.vue renders this plugin's
// whole StorageSchema via usePluginStorage), and proves declaring it didn't
// come at the cost of dropping instanceIDStorageKey (load-bearing for
// GetInstanceId's persistence, see the tests above) from the same list.
func TestStorageSchema_DeclaresRecordingPathKeyAsStored(t *testing.T) {
	p := &NVRPlugin{}
	schemas := p.StorageSchema()

	var foundRecordingPath, foundInstanceID bool
	for _, schema := range schemas {
		switch schema.Key {
		case recordingPathStorageKey:
			foundRecordingPath = true
			if schema.Type != sdk.JsonSchemaTypeString {
				t.Fatalf("expected %q to be a string schema, got %v", recordingPathStorageKey, schema.Type)
			}
			if schema.Store == nil || !*schema.Store {
				t.Fatalf("expected schema for %q to have Store: true, got %v", recordingPathStorageKey, schema.Store)
			}
		case instanceIDStorageKey:
			foundInstanceID = true
		}
	}
	if !foundRecordingPath {
		t.Fatalf("expected StorageSchema() to declare a schema entry for %q", recordingPathStorageKey)
	}
	if !foundInstanceID {
		t.Fatalf("expected StorageSchema() to still declare %q alongside the new field", instanceIDStorageKey)
	}
}
