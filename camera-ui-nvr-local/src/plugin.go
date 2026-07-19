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

import (
	"sync"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/recorder"
	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// NVRPlugin is the minimal boot skeleton for the local NVR hub plugin.
// Recording and playback are implemented in later tasks.
type NVRPlugin struct {
	sdk.BasePlugin

	// recorder tracks which cameras assigned to this Hub-role plugin are
	// actually being recorded, and each one's per-camera recording config.
	// Replaces the Task-2 noRecorders/managedCameraSource stub —
	// GetManagedCameraIds (rpc_recording.go) now delegates to
	// recorder.ManagedCameraIDs(). Populated from the Hub camera lifecycle
	// (ConfigureCameras/OnCameraAdded/OnCameraReleased, below) via the
	// sdkManagedCamera adapter. No ffmpeg/recording process state lives here
	// yet — that's Task 7.
	recorder *recorder.RecorderManager

	// store backs GetInstanceId's persistent UUID. Set to the plugin's real
	// sdk.DeviceStorage (the same value as BasePlugin.Storage) in NewPlugin;
	// tests substitute an in-memory fake.
	store instanceIDStore

	// db is this plugin's embedded SQLite database (store.Open), holding
	// events, segments, faces, and vector tables. Opened against
	// api.StoragePath in NewPlugin; nil in unit tests that construct
	// NVRPlugin directly rather than going through NewPlugin, and left nil
	// in production too if store.Open fails (logged, not fatal — see
	// NewPlugin) since sdk's pluginConstructor signature has no error return
	// for a failure here to propagate through.
	db *store.DB

	// events is the EventStore backing DetectionEvent ingestion
	// (attachDetectionIngestion, events_ingest.go) and, in a later task, the
	// getEvents/getCameraEvents RPC handlers. nil whenever db is nil.
	events *store.EventStore

	// detectionSubs tracks the per-camera sdk.Disposable returned by
	// CameraDevice.OnDetectionEvent so OnCameraReleased can unsubscribe
	// exactly the released camera (see events_ingest.go).
	detectionSubs detectionSubscriptions

	// recorders backs detectionEventIngester's eventRecorderLookup
	// (events_ingest.go), letting DetectionEvent ingestion call MarkEvent on
	// the camera's live recorder when one is registered. Zero value is
	// ready to use (an always-empty registry) — see recorderRegistry's doc
	// comment for why nothing populates it yet.
	recorders recorderRegistry
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
	p := &NVRPlugin{BasePlugin: sdk.NewBasePlugin(logger, api, storage), recorder: recorder.NewRecorderManager(), store: storage}

	// Open the embedded SQLite database against the host-provided storage
	// directory (api.StoragePath — see plugin_api.go: "absolute path to the
	// plugin's writable storage directory"). A failure here is logged, not
	// fatal: NewPlugin's signature (sdk.pluginConstructor) has no error
	// return, so the alternative would be a panic that takes the whole
	// plugin process down over what later tasks can treat as "events/
	// recording unavailable this run" — p.events stays nil and
	// attachDetectionIngestion/-Released below no-op accordingly.
	db, err := store.Open(api.StoragePath)
	if err != nil {
		logger.Error("nvr-local: open store failed:", err)
	} else {
		p.db = db
		p.events = store.NewEventStore(db)
	}

	api.On(string(sdk.APIEventFinishLaunching), func(...any) { p.Logger.Log("nvr-local: finished launching") })
	api.On(string(sdk.APIEventShutdown), func(...any) {
		p.Logger.Log("nvr-local: shutdown")
		if p.db != nil {
			if err := p.db.Close(); err != nil {
				p.Logger.Error("nvr-local: close store failed:", err)
			}
		}
	})

	return p
}

// ConfigureCameras, OnCameraAdded and OnCameraReleased satisfy sdk.Plugin.
// This is a Hub-role plugin (PluginRoleHub, contract.ts) that "attaches to
// cameras owned by other plugins" — cameras are handed to it here via the
// host's hub assignment, not because it owns them. Recording (ffmpeg,
// segment writing) is still a later task's no-op-for-now scope, but two
// pieces of camera-aware wiring are real as of this task: DetectionEvent
// ingestion (attachDetectionIngestion, unchanged since Task 1) and the
// recorder registry (p.recorder, Task 6) that backs GetManagedCameraIds.
// Every camera handed in is adapted to recorder.ManagedCamera via
// sdkManagedCamera below and registered/unregistered accordingly.
func (p *NVRPlugin) ConfigureCameras(cameras []*sdk.CameraDevice) error {
	managed := make([]recorder.ManagedCamera, 0, len(cameras))
	for _, cam := range cameras {
		p.attachDetectionIngestion(cam)
		managed = append(managed, sdkManagedCamera{dev: cam})
	}
	return p.recorder.Configure(managed)
}

func (p *NVRPlugin) OnCameraAdded(camera *sdk.CameraDevice) error {
	p.attachDetectionIngestion(camera)
	return p.recorder.Add(sdkManagedCamera{dev: camera})
}

func (p *NVRPlugin) OnCameraReleased(cameraID string) error {
	p.detectionSubs.remove(cameraID)
	p.recorders.Remove(cameraID)
	return p.recorder.Remove(cameraID)
}

// recorderRegistry maps camera IDs to the *recorder.Recorder instance
// currently recording them, backing detectionEventIngester's
// eventRecorderLookup (events_ingest.go: RecorderFor). Nothing populates it
// yet in this build: constructing and Start()ing one *recorder.Recorder per
// managed camera — tying RecorderManager's RecorderEntry/RecordingConfig
// (manager.go) to a live ffmpeg process — is explicitly deferred to a later
// task per Task 7's own report ("Wiring one *Recorder per
// continuously-recorded RecorderEntry is left to whichever later task
// orchestrates RecorderManager against real cameras"). This type is that
// later task's extension point: Set/Remove for it to call as recorders
// start/stop, RecorderFor for detectionEventIngester to query on every
// DetectionEvent. OnCameraReleased above already calls Remove so a stale
// entry doesn't outlive its camera once something starts adding them.
//
// *recorder.Recorder satisfies eventRecorder (MarkEvent(startMs, endMs
// int64)) directly — no adapter needed, unlike sdkManagedCamera above.
type recorderRegistry struct {
	mu   sync.Mutex
	recs map[string]*recorder.Recorder
}

// Set registers rec as cameraID's live recorder, replacing any previous
// entry for the same id.
func (r *recorderRegistry) Set(cameraID string, rec *recorder.Recorder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recs == nil {
		r.recs = make(map[string]*recorder.Recorder)
	}
	r.recs[cameraID] = rec
}

// Remove unregisters cameraID's recorder, if any. A no-op for an unknown id.
func (r *recorderRegistry) Remove(cameraID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.recs, cameraID)
}

// RecorderFor implements eventRecorderLookup.
func (r *recorderRegistry) RecorderFor(cameraID string) (eventRecorder, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.recs[cameraID]
	if !ok {
		return nil, false
	}
	return rec, true
}

// sdkManagedCamera adapts a real *sdk.CameraDevice to recorder.ManagedCamera.
// This is the only place a *sdk.CameraDevice is bridged into the recorder
// package — package recorder has no dependency on *sdk.CameraDevice itself
// (see manager.go: sdk.CameraDevice's only constructor, newCameraDeviceProxy,
// is unexported, so recorder's own tests use fakes instead). *sdk.CameraDevice
// doesn't satisfy recorder.ManagedCamera on its own because its Storage()
// method returns the concrete *sdk.DeviceStorage rather than the
// recorder.CameraStorage interface; this adapter's Storage() method bridges
// that return-type mismatch (*sdk.DeviceStorage does implement
// recorder.CameraStorage's method set — GetValue/DefineSchemas — it's just
// not the interface's declared return type until wrapped here).
type sdkManagedCamera struct{ dev *sdk.CameraDevice }

func (c sdkManagedCamera) ID() string   { return c.dev.ID() }
func (c sdkManagedCamera) Name() string { return c.dev.Name() }
func (c sdkManagedCamera) Storage() recorder.CameraStorage {
	return c.dev.Storage()
}

// attachDetectionIngestion subscribes to cam's detection-event stream via
// sdk.CameraDevice.OnDetectionEvent (camera_device.go:547) and upserts every
// event into p.events through a detectionEventIngester (events_ingest.go).
// A no-op if p.events is nil (store.Open failed in NewPlugin — see there).
//
// DEFERRED live-verification: this subscription line itself — cam.
// OnDetectionEvent(ingester.handle) actually firing when a live core
// delivers a detection-event NATS message — is not covered by a unit test.
// sdk.CameraDevice's only constructor, newCameraDeviceProxy
// (camera_device.go), is unexported, so no test outside package sdk can
// build a real *sdk.CameraDevice to subscribe against; doing so needs a
// live core + NATS connection. What IS unit-tested (events_ingest_test.go)
// is detectionEventIngester.handle itself, called directly with a
// synthetic sdk.DetectionEvent and a fake eventUpserter — i.e. everything
// on this side of the OnDetectionEvent callback boundary.
func (p *NVRPlugin) attachDetectionIngestion(cam *sdk.CameraDevice) {
	if p.events == nil {
		return
	}
	ingester := newDetectionEventIngester(p.events, &p.recorders, p.Logger)
	p.detectionSubs.add(cam.ID(), cam.OnDetectionEvent(ingester.handle))
}
