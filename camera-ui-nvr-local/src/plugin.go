// RPC dispatch mechanism (findings, task 2)
//
// Source read: github.com/cameraui/rpc/go@v1.0.6/handler.go,
// github.com/cameraui/sdk/go@v1.1.11/run.go, .../storage.go.
//
//  1. Method-name casing is automatic and unconditional. sdk.Run registers the
//     plugin struct itself under the child-RPC namespace:
//
//       cleanupRPC, err = client.RegisterHandler(namespaces.PluginChildRPC, plugin)
//
//     RegisterHandler calls rpc.ExtractMethods(handler), which walks the
//     method set of the handler's type and, for every exported Go method,
//     lowercases just the first rune to produce the wire name:
//
//       wireName := toCamelCase(m.Name) // GetManagedCameraIds -> getManagedCameraIds
//
//     (toCamelCase: "runes[0] = unicode.ToLower(runes[0]); return string(runes)").
//     Each wire name is subscribed as its own NATS subject
//     "rpc.<namespace>.<wireName>", so `GetManagedCameraIds` on this struct is
//     exactly what answers "plugin.<id>.child.rpc.getManagedCameraIds". There
//     is no explicit map and no struct tag involved in this step — the
//     mapping is purely mechanical, off the Go method name.
//
//  2. RPCMethods() is a real, load-bearing allow-list, not just documentation.
//     ExtractMethods checks whether the handler implements:
//
//       type RPCMethodAllowlist interface { RPCMethods() []string }
//
//     If it does, only the *wire names* returned by RPCMethods() are
//     registered as subjects — every other exported method on the struct
//     stays callable in-process (e.g. from other Go code in this plugin) but
//     is never subscribed on the wire, so the frontend/host cannot reach it
//     over RPC. Comment straight from rpc/go's handler.go:
//
//       "RPCMethodAllowlist lets a struct handler restrict which of its
//       exported methods are reachable over RPC. When a handler implements
//       it, only the returned wire names (camelCase, e.g. "getValue") are
//       registered; every other exported method — including RPCMethods
//       itself — stays callable in-process but is not exposed on the wire.
//       Handlers that do not implement it expose all exported methods (the
//       default)."
//
//     sdk.DeviceStorage uses exactly this pattern (storage.go:74-83) to
//     narrow its RPC surface to the config API while keeping lifecycle
//     methods like Save/DefineSchemas Go-callable but wire-invisible. This
//     plugin follows the same convention below: RPCMethods() lists the wire
//     names ("getManagedCameraIds", "getInstanceId", ...), extended as later
//     tasks add RPC-visible methods. Names in the allow-list are the
//     lowercase-first-letter wire names, not the Go method names.
//
// Instance ID (findings, task 2)
//
// There is no public SDK accessor for a plugin's own instance/runtime ID —
// neither on CoreManager (GetFFmpegPath, GetServerAddresses,
// GetCloudServerID, GetPluginsByInterface, ConnectToPlugin — none return the
// caller's own ID) nor on PluginAPI/BasePlugin/Logger (Logger stores an
// unexported pluginID field with no getter). The only place the ID exists at
// all is the PLUGIN_ID environment variable that sdk.Run reads before the
// plugin constructor even runs:
//
//	pluginID := os.Getenv("PLUGIN_ID")
//	namespaces := getPluginNamespaces(pluginID)
//	...
//	cleanupRPC, err = client.RegisterHandler(namespaces.PluginChildRPC, plugin)
//
// That same pluginID is what builds namespaces.PluginChildRPC — the exact
// subject prefix ("plugin.<id>.child.rpc") the frontend calls into. So
// GetInstanceId (rpc_recording.go) reads os.Getenv("PLUGIN_ID") directly:
// it is guaranteed to be the identical value the core already addresses this
// plugin by, since it's the same value used to construct that address.
// (The SDK's own internal helper, manager_download.go's remotePluginID(),
// reads the same env var for the same reason, though it is unexported and
// gated to remote mode only.)
package main

import sdk "github.com/cameraui/sdk/go"

// NVRPlugin is the minimal boot skeleton for the local NVR hub plugin.
// Storage, recording, and playback are implemented in later tasks.
type NVRPlugin struct {
	sdk.BasePlugin

	// recorders supplies the set of camera IDs this instance is actively
	// recording. Stubbed with noRecorders until Task 6 introduces the real
	// recorder registry.
	recorders managedCameraSource
}

// RPCMethods restricts this plugin's RPC surface to the wire names listed
// here (see the casing/allow-list findings above). Extend this list as later
// tasks add RPC-visible methods; every entry must be the camelCase wire name,
// not the Go method name.
func (p *NVRPlugin) RPCMethods() []string {
	return []string{"getManagedCameraIds", "getInstanceId"}
}

func NewPlugin(logger *sdk.Logger, api *sdk.PluginAPI, storage *sdk.DeviceStorage) sdk.Plugin {
	p := &NVRPlugin{BasePlugin: sdk.NewBasePlugin(logger, api, storage), recorders: noRecorders{}}

	api.On(string(sdk.APIEventFinishLaunching), func(...any) { p.Logger.Log("nvr-local: finished launching") })
	api.On(string(sdk.APIEventShutdown), func(...any) { p.Logger.Log("nvr-local: shutdown") })

	return p
}

// ConfigureCameras, OnCameraAdded and OnCameraReleased satisfy sdk.Plugin.
// This is a Hub-role plugin with no managed cameras of its own, so these are
// no-ops for now; camera-aware recording logic lands in a later task.
func (p *NVRPlugin) ConfigureCameras(cameras []*sdk.CameraDevice) error { return nil }

func (p *NVRPlugin) OnCameraAdded(camera *sdk.CameraDevice) error { return nil }

func (p *NVRPlugin) OnCameraReleased(cameraID string) error { return nil }
