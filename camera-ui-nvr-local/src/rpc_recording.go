package main

import (
	"fmt"

	"github.com/google/uuid"
)

// managedCameraSource is the minimal interface the RPC layer needs from the
// recorder registry. Implemented for real by the recorder manager (Task 6);
// stubbed by noRecorders until then.
type managedCameraSource interface {
	// ManagedCameraIDs returns the IDs of cameras this instance is actively
	// recording. Must return a non-nil (possibly empty) slice.
	ManagedCameraIDs() []string
}

// noRecorders is the zero-value stand-in for the recorder registry until
// Task 6 lands. It manages no cameras.
type noRecorders struct{}

func (noRecorders) ManagedCameraIDs() []string { return []string{} }

// GetManagedCameraIds returns the camera IDs this NVR instance is actively
// recording. Registered as the RPC method "getManagedCameraIds" — see the
// casing findings at the top of plugin.go for how the Go method name maps to
// that wire name.
func (p *NVRPlugin) GetManagedCameraIds() ([]string, error) {
	return p.recorders.ManagedCameraIDs(), nil
}

// instanceIDStore is the minimal storage interface GetInstanceId needs to
// read and persist its generated instance id. sdk.DeviceStorage satisfies
// this exactly (see GetValue/SetValue in storage.go) — p.Storage is used in
// production; tests substitute an in-memory fake.
type instanceIDStore interface {
	GetValue(key string, defaultValue ...any) any
	SetValue(key string, value any) error
}

// instanceIDStorageKey is the plugin storage key the persistent instance id
// is kept under.
const instanceIDStorageKey = "instanceId"

// GetInstanceId returns a persistent, per-plugin-install UUID. Registered as
// the RPC method "getInstanceId".
//
// Revised finding (task 2 review): the compiled frontend uses getInstanceId()
// purely as a cache-invalidation change-token — it polls the value and, when
// it CHANGES from whatever it last saw, flushes the NVR event cache. It is
// never compared against the core's own settings.instanceId, and (per the
// findings at the top of plugin.go) no SDK/core accessor for that core value
// exists to plugin authors anyway. The original implementation returned
// os.Getenv("PLUGIN_ID") — the plugin's constant package id
// (e.g. "@calebcall/camera-ui-nvr-local") — which never changes across
// restarts and therefore could never drive the intended cache flush; it was
// the wrong value for the contract this method actually serves.
//
// The correct value is a UUID generated once and persisted in this plugin's
// own DeviceStorage (p.Storage), keyed under instanceIDStorageKey: stable
// across restarts and unique per install, changing only if the plugin's
// storage is wiped — exactly the semantics the frontend's cache-invalidation
// consumer needs, achievable entirely from this plugin's own state with no
// core/SDK change required.
//
// This only actually persists because NVRPlugin.StorageSchema (plugin.go)
// declares a schema for instanceIDStorageKey with Store: true.
// sdk.DeviceStorage.SetValue silently no-ops for any key with no declared
// schema — see the "Correction" note in plugin.go's doc comment for the bug
// this caused before StorageSchema existed, and why run.go's registration
// order guarantees the schema is in place before any RPC call reaches here.
func (p *NVRPlugin) GetInstanceId() (string, error) {
	if existing, ok := p.store.GetValue(instanceIDStorageKey, "").(string); ok && existing != "" {
		return existing, nil
	}

	id := uuid.NewString()
	if err := p.store.SetValue(instanceIDStorageKey, id); err != nil {
		return "", fmt.Errorf("persist instance id: %w", err)
	}
	return id, nil
}
