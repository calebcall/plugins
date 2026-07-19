package main

import "os"

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

// GetInstanceId returns the plugin instance ID the host assigned this
// process. Registered as the RPC method "getInstanceId".
//
// See the "Instance ID" findings at the top of plugin.go: the SDK exposes no
// public accessor for this value, so this reads the same PLUGIN_ID
// environment variable sdk.Run itself reads to build the RPC namespace the
// frontend calls into, guaranteeing the value matches what the core already
// uses to address this plugin.
func (p *NVRPlugin) GetInstanceId() (string, error) {
	return os.Getenv("PLUGIN_ID"), nil
}
