package main

import "testing"

// fakeInstanceStore is an in-memory stand-in for sdk.DeviceStorage, used to
// unit-test GetInstanceId's persistence behavior without a live host/API.
type fakeInstanceStore struct {
	values map[string]any
}

func newFakeInstanceStore() *fakeInstanceStore {
	return &fakeInstanceStore{values: make(map[string]any)}
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

func (s *fakeInstanceStore) SetValue(key string, value any) error {
	s.values[key] = value
	return nil
}

// newTestPlugin constructs an NVRPlugin suitable for unit-testing RPC
// methods that don't touch the live sdk.PluginAPI (getManagedCameraIds,
// getInstanceId). BasePlugin's Logger/API/Storage are left nil deliberately
// — a live *sdk.PluginAPI requires a connected NATS client, which unit tests
// don't have. GetInstanceId only needs storage, so it's wired to an
// in-memory fake (fakeInstanceStore) instead.
func newTestPlugin(t *testing.T) *NVRPlugin {
	t.Helper()
	return &NVRPlugin{recorders: noRecorders{}, store: newFakeInstanceStore()}
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
// contract end to end: with fresh/empty storage, the first call generates a
// non-empty value and writes it to storage under instanceIDStorageKey; a
// second call returns the identical value instead of generating a new one.
func TestGetInstanceId_GeneratesPersistsAndIsStable(t *testing.T) {
	store := newFakeInstanceStore()
	p := &NVRPlugin{recorders: noRecorders{}, store: store}

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

// TestGetInstanceId_ReturnsExistingStoredValue covers the case where storage
// already holds a previously generated id (e.g. across a process restart):
// GetInstanceId must return it unchanged rather than generating a new one.
func TestGetInstanceId_ReturnsExistingStoredValue(t *testing.T) {
	store := newFakeInstanceStore()
	store.values[instanceIDStorageKey] = "existing-uuid-value"
	p := &NVRPlugin{recorders: noRecorders{}, store: store}

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
