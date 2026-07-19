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
// Instance ID (findings, task 2 — revised after review)
//
// There is no public SDK accessor for a plugin's own instance/runtime ID —
// neither on CoreManager (GetFFmpegPath, GetServerAddresses,
// GetCloudServerID, GetPluginsByInterface, ConnectToPlugin — none return the
// caller's own ID) nor on PluginAPI/BasePlugin/Logger (Logger stores an
// unexported pluginID field with no getter) — nor is there any SDK/core
// accessor for the core's own settings.instanceId. This was initially taken
// to mean GetInstanceId should mirror sdk.Run's os.Getenv("PLUGIN_ID"), but
// review of the compiled @camera.ui/nvr frontend showed that's the wrong
// contract: getInstanceId() is polled by the frontend purely as a
// cache-invalidation change-token — when the returned value CHANGES from
// what it last saw, the client flushes its NVR event cache. It is never
// compared to the core's instanceId. PLUGIN_ID is the plugin's constant
// package id (e.g. "@calebcall/camera-ui-nvr-local") and never changes
// across restarts, so it could never drive that flush — it was simply the
// wrong value for what this method is actually for.
//
// GetInstanceId (rpc_recording.go) instead returns a UUID generated once and
// persisted in this plugin's own DeviceStorage (p.store, backed by
// p.Storage in production) under instanceIDStorageKey. That's stable across
// restarts and unique per install, changing only if the plugin's storage is
// wiped — exactly the change-token semantics the frontend's cache consumer
// needs, achieved entirely from this plugin's own state with no core/SDK
// change required.
//
// Correction (second review pass): the first cut of this fix persisted via
// p.store.SetValue(instanceIDStorageKey, id) without ever declaring a schema
// for that key. sdk.DeviceStorage.SetValue (storage.go) silently no-ops when
// no schema exists for the key —
//
//	schema := ds.findSchemaByKey(key)
//	if schema == nil {
//	    ds.mu.Unlock()
//	    return nil
//	}
//
// — so the "persisted" UUID was never actually written, and every call
// regenerated a fresh one (worse than the original PLUGIN_ID bug: the
// frontend's change-token would flip on every poll instead of never).
// NVRPlugin now implements sdk.StorageSchemaProvider (StorageSchema, below)
// to declare a schema for instanceIDStorageKey. Ordering is confirmed safe
// from run.go: the host constructs the plugin, then — *before* registering
// any RPC handler — calls StorageSchema() and DefineSchemas() on the result:
//
//	plugin = constructor(logger, api, pluginStorage)
//	if schemaProvider, ok := plugin.(StorageSchemaProvider); ok {
//	    schemas := schemaProvider.StorageSchema()
//	    if len(schemas) > 0 {
//	        pluginStorage.DefineSchemas(schemas)
//	    }
//	}
//	cleanupRPC, err = client.RegisterHandler(namespaces.PluginChildRPC, plugin)
//
// so the schema is always registered before the first getInstanceId RPC
// call could possibly arrive.
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

	// store backs GetInstanceId's persistent UUID. Set to the plugin's real
	// sdk.DeviceStorage (the same value as BasePlugin.Storage) in NewPlugin;
	// tests substitute an in-memory fake.
	store instanceIDStore
}

// Compile-time assertions that NVRPlugin implements the optional SDK
// interfaces it relies on.
var _ sdk.StorageSchemaProvider = (*NVRPlugin)(nil)

// RPCMethods restricts this plugin's RPC surface to the wire names listed
// here (see the casing/allow-list findings above). Extend this list as later
// tasks add RPC-visible methods; every entry must be the camelCase wire name,
// not the Go method name.
func (p *NVRPlugin) RPCMethods() []string {
	return []string{"getManagedCameraIds", "getInstanceId"}
}

// StorageSchema declares the plugin-level storage schema. sdk.Run calls this
// (via sdk.StorageSchemaProvider) right after construction and feeds the
// result into DeviceStorage.DefineSchemas — before RPC handlers are
// registered — so every key here is writable via SetValue from the first RPC
// call onward (see the "Correction" note above for why this matters).
//
// instanceIDStorageKey is hidden (internal bookkeeping, not a user-facing
// setting) and stored (Store: true) so GetInstanceId's generated UUID
// actually persists across restarts.
func (p *NVRPlugin) StorageSchema() []sdk.JsonSchema {
	storeTrue := true
	return []sdk.JsonSchema{
		{
			Type:   sdk.JsonSchemaTypeString,
			Key:    instanceIDStorageKey,
			Title:  "Instance ID",
			Hidden: true,
			Store:  &storeTrue,
		},
	}
}

func NewPlugin(logger *sdk.Logger, api *sdk.PluginAPI, storage *sdk.DeviceStorage) sdk.Plugin {
	p := &NVRPlugin{BasePlugin: sdk.NewBasePlugin(logger, api, storage), recorders: noRecorders{}, store: storage}

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
