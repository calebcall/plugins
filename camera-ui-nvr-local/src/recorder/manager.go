// Package recorder implements RecorderManager, the registry of cameras this
// NVR instance is assigned to record and each one's per-camera recording
// config. This task (Task 6) is registry + config only — no ffmpeg/segment
// writing yet (that's Task 7). RecorderManager replaces the Task-2
// noRecorders stub in the parent package so getManagedCameraIds returns real
// data.
//
// Testability (see the task brief): sdk.CameraDevice's only constructor,
// newCameraDeviceProxy, is unexported, so no test outside package sdk can
// build a real *sdk.CameraDevice. RecorderManager therefore never takes
// *sdk.CameraDevice in its exported API — it takes the ManagedCamera
// interface below, which a real device satisfies via the sdkManagedCamera
// adapter defined in the parent package's plugin.go. Tests in this package
// use small fakes that implement ManagedCamera/CameraStorage directly.
package recorder

import (
	"sort"
	"sync"

	sdk "github.com/cameraui/sdk/go"
)

// RecordingMode selects when a managed camera's video is written to disk.
type RecordingMode string

const (
	RecordingModeOff        RecordingMode = "off"
	RecordingModeContinuous RecordingMode = "continuous"
	RecordingModeEvents     RecordingMode = "events"
)

// Per-camera recording config storage keys (declared via recordingConfigSchema
// on each camera's own DeviceStorage — see CameraStorage).
const (
	keyRecordingMode = "recordingMode"
	keyRetentionDays = "retentionDays"
	keyPreRollS      = "preRollS"
	keyPostRollS     = "postRollS"
	keyRoles         = "roles"
)

// Defaults applied when a camera has no stored value for a given key.
const (
	defaultRetentionDays = 7
	defaultPreRollS      = 5
	defaultPostRollS     = 10
)

// defaultRoles is the stream source role recorded when a camera has no
// stored "roles" value yet.
//
// DEFERRED: Task 7 (ffmpeg recording) is the first actual consumer of this
// field and may revisit the default once it knows which source role feeds
// the recorder pipeline; high-resolution is a reasonable NVR-quality
// default in the meantime.
var defaultRoles = []string{string(sdk.CameraRoleHighRes)}

// CameraStorage is the subset of *sdk.DeviceStorage RecorderManager needs:
// enough to declare the recording-config schema and read values back.
// *sdk.DeviceStorage satisfies this directly (see storage.go: GetValue,
// DefineSchemas) — the parent package's sdkManagedCamera adapter is the only
// place a real *sdk.CameraDevice's Storage() is bridged to this interface.
type CameraStorage interface {
	GetValue(key string, defaultValue ...any) any
	DefineSchemas(schemas []sdk.JsonSchema)
}

// ManagedCamera is the minimal camera shape RecorderManager needs: enough to
// identify a camera and read its recording config. Kept intentionally
// narrow (YAGNI) — add methods here only when a concrete need shows up in a
// later task.
type ManagedCamera interface {
	ID() string
	Name() string
	Storage() CameraStorage
}

// RecordingConfig is one camera's resolved recording settings.
//
// There is deliberately no per-camera disk-quota field here: the retention
// disk cap (Task 9, retention.go) is instance-wide, matching the frontend
// contract's single top-level StorageStats.nvrQuotaGB — see retention.go's
// package doc comment for why. That value lives on the plugin's own
// instance-level storage (plugin.go), not any camera's RecordingConfig.
type RecordingConfig struct {
	Mode          RecordingMode
	RetentionDays int
	PreRollS      int
	PostRollS     int
	Roles         []string
}

// RecorderEntry is a managed camera's identity plus its resolved recording
// config — RecorderManager's registry entry. It carries no ffmpeg/recording
// process state of its own; that runtime (see recorder.go's Recorder type,
// Task 7) is a separate, independently-constructed object driven by whatever
// orchestrates RecorderManager (a later task), not hung off this struct.
//
// Named "RecorderEntry" rather than "Recorder" specifically to avoid
// colliding with that Task 7 runtime type in this same package — Task 6
// originally named this struct "Recorder" and said later tasks would "hang
// per-camera runtime state off this struct"; Task 7 instead needed the bare
// name "Recorder" for its own exported constructor/Start/Stop/State API
// (kept exact because Tasks 8-10 depend on it), so this registry entry was
// renamed instead of the other way around.
type RecorderEntry struct {
	CameraID string
	Name     string
	Config   RecordingConfig
}

// RecorderManager tracks which cameras this NVR instance is assigned to
// (via the Hub camera lifecycle: ConfigureCameras/OnCameraAdded/
// OnCameraReleased) and each one's recording config. It holds no
// ffmpeg/recording process state yet — that's Task 7.
type RecorderManager struct {
	mu        sync.RWMutex
	recorders map[string]*RecorderEntry

	// gc holds the retention garbage-collection dependencies (SQLite stores,
	// vector backends) and background-ticker state — see retention.go. Left
	// nil by NewRecorderManager; every pre-Task-9 caller (including every
	// existing test in this package) never touches retention, so
	// RunRetentionOnce/StartRetention/StopRetention must all treat a nil gc
	// as "not configured, nothing to do" rather than panicking. Set once via
	// ConfigureRetention (production wiring lives in plugin.go).
	gc *retentionGC
}

// NewRecorderManager returns an empty manager. Recording config lives on
// each camera's own DeviceStorage (via ManagedCamera.Storage()), not in this
// plugin's SQLite database, so there is no store dependency to inject here.
func NewRecorderManager() *RecorderManager {
	return &RecorderManager{recorders: make(map[string]*RecorderEntry)}
}

// Configure replaces the full set of managed cameras. Intended for the Hub
// ConfigureCameras callback, which the SDK calls once at startup with every
// camera currently assigned to this plugin.
func (m *RecorderManager) Configure(cameras []ManagedCamera) error {
	next := make(map[string]*RecorderEntry, len(cameras))
	for _, cam := range cameras {
		next[cam.ID()] = newRecorder(cam)
	}

	m.mu.Lock()
	m.recorders = next
	m.mu.Unlock()
	return nil
}

// Add registers (or re-registers, re-reading its config) a single camera.
// Intended for the Hub OnCameraAdded callback.
func (m *RecorderManager) Add(cam ManagedCamera) error {
	r := newRecorder(cam)

	m.mu.Lock()
	if m.recorders == nil {
		m.recorders = make(map[string]*RecorderEntry)
	}
	m.recorders[cam.ID()] = r
	m.mu.Unlock()
	return nil
}

// Remove unregisters a camera. Intended for the Hub OnCameraReleased
// callback. Removing an unknown ID is a no-op, not an error.
func (m *RecorderManager) Remove(cameraID string) error {
	m.mu.Lock()
	delete(m.recorders, cameraID)
	m.mu.Unlock()
	return nil
}

// Camera returns the registered Recorder for id, if any.
func (m *RecorderManager) Camera(id string) (*RecorderEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.recorders[id]
	return r, ok
}

// ManagedCameraIDs returns the IDs of registered cameras whose recording
// mode is not "off" — i.e. the cameras this instance is actually supposed to
// be recording, as opposed to every camera merely assigned to the Hub role.
// Always non-nil, sorted for stable output.
func (m *RecorderManager) ManagedCameraIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ids := make([]string, 0, len(m.recorders))
	for id, r := range m.recorders {
		if r.Config.Mode != RecordingModeOff {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// entriesSnapshot returns a value-copy of every registered RecorderEntry —
// regardless of Config.Mode, unlike ManagedCameraIDs, since retention (Task
// 9) must still clean up a camera's old footage even if its mode was since
// switched to "off" — for a caller (retention.go) that needs to iterate the
// full managed set without holding m.mu itself or racing a concurrent
// Configure/Add/Remove. Copying each *RecorderEntry by value (rather than
// handing out the live pointers stored in m.recorders) means a retention
// pass sees a consistent snapshot even if the registry changes while it
// runs. Sorted by CameraID for stable/deterministic iteration order (tests).
func (m *RecorderManager) entriesSnapshot() []RecorderEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]RecorderEntry, 0, len(m.recorders))
	for _, r := range m.recorders {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CameraID < out[j].CameraID })
	return out
}

func newRecorder(cam ManagedCamera) *RecorderEntry {
	return &RecorderEntry{
		CameraID: cam.ID(),
		Name:     cam.Name(),
		Config:   readRecordingConfig(cam.Storage()),
	}
}

// recordingConfigSchema declares the recordingMode/retentionDays/preRollS/
// postRollS/roles fields on a camera's own storage scope. Called on every
// read (readRecordingConfig), same idempotent pattern as reolink's
// ensureStorageSchemas: DeviceStorage.DefineSchemas overwrites the schema
// list of the *caller's own* per-plugin-per-camera storage scope (each
// plugin attached to a camera gets its own DeviceStorage instance — see
// StorageController.createCameraStorage — so this never touches another
// plugin's schema for the same camera), and re-declaring is cheap and safe.
func recordingConfigSchema() []sdk.JsonSchema {
	storeTrue := true
	return []sdk.JsonSchema{
		{
			Type:         sdk.JsonSchemaTypeString,
			Key:          keyRecordingMode,
			Title:        "Recording Mode",
			Description:  "off: never record. continuous: always record. events: record only around detection events.",
			Enum:         []string{string(RecordingModeOff), string(RecordingModeContinuous), string(RecordingModeEvents)},
			DefaultValue: string(RecordingModeOff),
			Store:        &storeTrue,
		},
		{
			Type:         sdk.JsonSchemaTypeNumber,
			Key:          keyRetentionDays,
			Title:        "Retention (days)",
			Description:  "How long recorded segments are kept before being deleted.",
			DefaultValue: float64(defaultRetentionDays),
			Minimum:      sdk.Float64(1),
			Store:        &storeTrue,
		},
		{
			Type:         sdk.JsonSchemaTypeNumber,
			Key:          keyPreRollS,
			Title:        "Pre-roll (seconds)",
			Description:  "Seconds of buffered video kept before an event-triggered recording starts.",
			DefaultValue: float64(defaultPreRollS),
			Minimum:      sdk.Float64(0),
			Store:        &storeTrue,
		},
		{
			Type:         sdk.JsonSchemaTypeNumber,
			Key:          keyPostRollS,
			Title:        "Post-roll (seconds)",
			Description:  "Seconds of video kept after an event-triggered recording ends.",
			DefaultValue: float64(defaultPostRollS),
			Minimum:      sdk.Float64(0),
			Store:        &storeTrue,
		},
		{
			Type:   sdk.JsonSchemaTypeArray,
			Key:    keyRoles,
			Title:  "Recorded Stream Roles",
			Hidden: true,
			Store:  &storeTrue,
			Items:  &sdk.JsonSchema{Type: sdk.JsonSchemaTypeString},
		},
	}
}

// readRecordingConfig declares the recording config schema on storage (so
// defaults exist and edits can persist) and resolves the current
// RecordingConfig from it. Unset values resolve to the defaults above;
// an invalid/corrupt stored recordingMode (e.g. hand-edited storage.json,
// or a value from a future schema version this build doesn't know) falls
// back to RecordingModeOff rather than recording unexpectedly or panicking.
func readRecordingConfig(storage CameraStorage) RecordingConfig {
	storage.DefineSchemas(recordingConfigSchema())

	mode, _ := storage.GetValue(keyRecordingMode, string(RecordingModeOff)).(string)
	recordingMode := RecordingMode(mode)
	switch recordingMode {
	case RecordingModeOff, RecordingModeContinuous, RecordingModeEvents:
	default:
		recordingMode = RecordingModeOff
	}

	return RecordingConfig{
		Mode:          recordingMode,
		RetentionDays: intValue(storage.GetValue(keyRetentionDays, defaultRetentionDays), defaultRetentionDays),
		PreRollS:      intValue(storage.GetValue(keyPreRollS, defaultPreRollS), defaultPreRollS),
		PostRollS:     intValue(storage.GetValue(keyPostRollS, defaultPostRollS), defaultPostRollS),
		Roles:         stringSliceValue(storage.GetValue(keyRoles, defaultRoles)),
	}
}

// intValue coerces a GetValue result (which may be int, one of the narrower
// integer types msgpack decodes onto the wire, or float64 from a JSON/schema
// default) into an int, falling back to fallback for any other/missing type.
func intValue(v any, fallback int) int {
	switch t := v.(type) {
	case int:
		return t
	case int32:
		return int(t)
	case int64:
		return int(t)
	case float32:
		return int(t)
	case float64:
		return int(t)
	default:
		return fallback
	}
}

// stringSliceValue coerces a GetValue result into a []string. Storage may
// hand back either a genuine []string (fallback default, or a fake in
// tests) or a []any of strings (msgpack/JSON-decoded array), depending on
// how the value reached storage.
func stringSliceValue(v any) []string {
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
