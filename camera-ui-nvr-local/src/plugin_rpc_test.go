package main

import "testing"

// newTestPlugin constructs an NVRPlugin suitable for unit-testing RPC
// methods that don't touch the live sdk.PluginAPI (getManagedCameraIds,
// getInstanceId). BasePlugin's Logger/API/Storage are left nil deliberately
// — a live *sdk.PluginAPI requires a connected NATS client, which unit tests
// don't have. Methods that need those fields must be exercised separately;
// see the "cannot be unit-tested" note on TestGetInstanceId below.
func newTestPlugin(t *testing.T) *NVRPlugin {
	t.Helper()
	return &NVRPlugin{recorders: noRecorders{}}
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

// TestGetInstanceId_ReadsPluginIDEnv is the smallest test possible for this
// method given the SDK's actual surface (see the "Instance ID" findings at
// the top of plugin.go): there is no SDK type to fake or inject here — the
// value comes from the PLUGIN_ID environment variable, the same one sdk.Run
// reads before the plugin constructor runs. This test only proves
// GetInstanceId reads that variable faithfully; it does not (and cannot,
// without a live host process) prove the value matches what a real host
// assigns.
func TestGetInstanceId_ReadsPluginIDEnv(t *testing.T) {
	t.Setenv("PLUGIN_ID", "test-instance-123")

	p := newTestPlugin(t)
	id, err := p.GetInstanceId()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "test-instance-123" {
		t.Fatalf("expected instance id %q, got %q", "test-instance-123", id)
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
