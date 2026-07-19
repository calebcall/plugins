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
	"context"
	"errors"
	"fmt"
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
// identify a camera, read its recording config, and (Task ORCH) resolve the
// RTSP/go2rtc URL for one of its stream roles so a live Recorder can be
// started against it. Kept intentionally narrow (YAGNI) — add methods here
// only when a concrete need shows up in a later task.
//
// StreamURL is the one addition this task makes: it is deliberately a
// method on ManagedCamera rather than a bare string, mirroring
// RecorderConfig.StreamURL's own doc comment (recorder.go) — the URL may
// need re-resolving on every (re)connect attempt, not just once. A real
// *sdk.CameraDevice cannot be constructed in tests (newCameraDeviceProxy is
// unexported), which is exactly why this stays on the interface instead of
// RecorderManager reaching for *sdk.CameraDevice directly: the parent
// package's sdkManagedCamera adapter (plugin.go) is the only place a real
// device is bridged to it; tests use fakeCamera.
type ManagedCamera interface {
	ID() string
	Name() string
	Storage() CameraStorage
	StreamURL(role string) (string, error)
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

	// StreamURL resolves cam.StreamURL for the camera this entry was built
	// from (see newRecorder) — carried on the entry itself, rather than
	// requiring callers to keep the original ManagedCamera around, so
	// StartAll/syncRecording (below) can build a RecorderConfig purely from
	// the registry, well after ConfigureCameras/Add's ManagedCamera
	// argument has gone out of scope.
	StreamURL func(role string) (string, error)
}

// RecorderHandle is the lifecycle surface RecorderManager needs from a live
// per-camera recording process: Start begins recording (idempotent, like
// *recorder.Recorder.Start), Stop cancels it and blocks until it has fully
// stopped. *Recorder satisfies this directly (its Start/Stop signatures
// already match) — no adapter needed for tests that exercise a real
// Recorder, and production wiring (plugin.go) wraps one only to also keep
// its own event-mode recorder registry in sync.
//
// RecorderManager depends on this interface — never *Recorder directly —
// specifically so orchestration (StartAll/StopAll/syncRecording, below) can
// be unit-tested with an injected fake RecorderFactory instead of spawning
// real ffmpeg processes.
type RecorderHandle interface {
	Start(ctx context.Context) error
	Stop() error
}

// RecorderFactory constructs a RecorderHandle for cfg. Production wiring
// (plugin.go's NewPlugin, via ConfigureRecording) supplies one that builds a
// real *Recorder (backed by the plugin's SegmentStore/FFmpeg) and registers
// it in the plugin's own recorderRegistry so detection-event ingestion's
// MarkEvent reaches it; tests inject a fake that records the RecorderConfig
// it was called with and a fake handle whose Start/Stop calls can be
// asserted on directly.
type RecorderFactory func(RecorderConfig) RecorderHandle

// defaultSegmentSeconds is the ffmpeg segment duration (RecorderConfig.
// SegmentSeconds) ConfigureRecording applies when its caller doesn't specify
// one (segmentSeconds <= 0). A minute is short enough that a given segment's
// worst-case finalization lag (see recorder.go's postRollWindowMs, the Task
// 8 events-mode edge this task also fixes) stays small relative to typical
// pre/post-roll settings, while still being long enough that continuous
// recording doesn't churn through an excessive number of small files.
const defaultSegmentSeconds = 60

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

	// recorderFactory, dataDir, and segmentSeconds are the dependencies
	// StartAll/syncRecording (Task ORCH, below) need to actually build and
	// start a live Recorder for a managed camera. Set once via
	// ConfigureRecording; a nil recorderFactory means "recording not
	// configured", the same "not configured, nothing to do" convention gc
	// above already established for retention.
	recorderFactory RecorderFactory
	dataDir         string
	segmentSeconds  int

	// launched, rootCtx/rootCancel, and active track this manager's live
	// orchestration state: launched flips true exactly once, on the first
	// StartAll call, and back to false on StopAll (so a later StartAll —
	// not expected in production, where Shutdown ends the process — starts
	// fresh rather than silently no-op'ing forever). rootCtx is the parent
	// context every started RecorderHandle.Start is given; rootCancel is
	// released by StopAll after every active handle has already been
	// stopped individually (a backstop, not the primary shutdown path —
	// each handle's own Stop() is what actually blocks until its recording
	// goroutines exit). active maps a managed camera's ID to its currently
	// running RecorderHandle, if any; a camera can be registered
	// (m.recorders) without being active (mode "off", or recording not yet
	// launched).
	launched   bool
	rootCtx    context.Context
	rootCancel context.CancelFunc
	active     map[string]RecorderHandle
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
// Intended for the Hub OnCameraAdded callback — and, since re-adding an
// already-known camera ID re-reads its config from scratch, also the
// mechanism a live per-camera settings edit (recordingMode/roles/pre-post
// roll) flows through: there is no separate SDK "config changed" hook (see
// sdk.Plugin), so whatever notices a stored value changed is expected to
// call Add again for that camera.
//
// Once recording has been launched (StartAll has run), this also
// starts/restarts/stops that camera's live Recorder to match its
// (re-)resolved config — see syncRecording. Before StartAll, this only
// updates the registry; StartAll picks up whatever's registered when it
// eventually runs.
func (m *RecorderManager) Add(cam ManagedCamera) error {
	r := newRecorder(cam)

	m.mu.Lock()
	if m.recorders == nil {
		m.recorders = make(map[string]*RecorderEntry)
	}
	m.recorders[cam.ID()] = r
	m.mu.Unlock()

	return m.syncRecording(*r)
}

// Remove unregisters a camera. Intended for the Hub OnCameraReleased
// callback. Removing an unknown ID is a no-op, not an error. Also stops and
// deregisters that camera's live Recorder, if one is currently active —
// a no-op if recording was never launched or the camera had none (e.g. it
// was already mode "off").
func (m *RecorderManager) Remove(cameraID string) error {
	m.mu.Lock()
	delete(m.recorders, cameraID)
	m.mu.Unlock()

	m.stopRecorder(cameraID)
	return nil
}

// ConfigureRecording wires the dependencies StartAll/syncRecording need to
// actually build and start a live Recorder for a managed camera: dataDir is
// the RecorderConfig.DataDir every built config uses (the plugin's own
// storage directory — recordings/ lives under it, see recorder.go's outDir),
// segmentSeconds is the RecorderConfig.SegmentSeconds every built config
// uses (defaultSegmentSeconds when <= 0), and factory builds the actual
// RecorderHandle for a given RecorderConfig (production: a real *Recorder,
// wrapped to also register into the plugin's recorderRegistry — see
// plugin.go; tests: a fake).
//
// Safe to call once, before StartAll is ever invoked; calling it again
// replaces the previous configuration (production wiring only ever calls
// this once, at startup, mirroring ConfigureRetention's own contract).
func (m *RecorderManager) ConfigureRecording(dataDir string, segmentSeconds int, factory RecorderFactory) {
	if segmentSeconds <= 0 {
		segmentSeconds = defaultSegmentSeconds
	}
	m.mu.Lock()
	m.dataDir = dataDir
	m.segmentSeconds = segmentSeconds
	m.recorderFactory = factory
	m.mu.Unlock()
}

// StartAll starts a Recorder for every currently registered camera whose
// recording mode is not "off" (see ManagedCameraIDs — off-mode cameras get
// no Recorder). Intended for the APIEventFinishLaunching handler, called
// once ConfigureCameras/Configure has populated the registry and
// ConfigureRecording has wired the factory/dataDir/segmentSeconds this needs
// to build one.
//
// A no-op, returning nil, when recording hasn't been configured
// (ConfigureRecording never called — mirrors StartRetention's own
// "not configured" no-op) or StartAll has already been called once
// (launched — calling it again doesn't start a second set of Recorders for
// every camera; new cameras still start normally via Add/syncRecording).
// Every per-camera start error is collected (errors.Join) rather than
// aborting the whole pass, so one camera's failure to start doesn't prevent
// every other managed camera from recording.
func (m *RecorderManager) StartAll() error {
	m.mu.Lock()
	if m.launched || m.recorderFactory == nil {
		m.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.rootCtx = ctx
	m.rootCancel = cancel
	m.launched = true
	m.mu.Unlock()

	var errs []error
	for _, entry := range m.entriesSnapshot() {
		if entry.Config.Mode == RecordingModeOff {
			continue
		}
		if err := m.startRecorder(entry); err != nil {
			errs = append(errs, fmt.Errorf("camera %s: %w", entry.CameraID, err))
		}
	}
	return errors.Join(errs...)
}

// StopAll stops every currently active Recorder — blocking until each one's
// Stop has returned, so a caller (APIEventShutdown) never leaves a recording
// goroutine running past this call — and marks the manager as no longer
// launched. Safe to call when nothing was ever started (StartAll never
// called, or every managed camera is mode "off"); idempotent.
func (m *RecorderManager) StopAll() {
	m.mu.Lock()
	active := m.active
	m.active = nil
	cancel := m.rootCancel
	m.launched = false
	m.rootCtx = nil
	m.rootCancel = nil
	m.mu.Unlock()

	for _, handle := range active {
		_ = handle.Stop()
	}
	if cancel != nil {
		cancel()
	}
}

// syncRecording brings entry's live Recorder in line with its current
// Config.Mode: stops and deregisters any existing Recorder for entry.
// CameraID, then — unless the resolved mode is "off" — starts a fresh one
// from entry's (possibly just-changed) config. Called from Add for every
// registration, so both a genuinely new camera and a config change to an
// already-managed one (Add's own doc comment) take effect immediately.
//
// A no-op before StartAll has ever run (m.launched false): there is nothing
// to stop yet, and starting anything before ConfigureRecording's
// dataDir/segmentSeconds and StartAll's root context exist would be
// premature — StartAll's own pass over the registry picks up whatever was
// registered by the time it runs.
func (m *RecorderManager) syncRecording(entry RecorderEntry) error {
	m.mu.RLock()
	launched := m.launched
	m.mu.RUnlock()
	if !launched {
		return nil
	}

	m.stopRecorder(entry.CameraID)
	if entry.Config.Mode == RecordingModeOff {
		return nil
	}
	return m.startRecorder(entry)
}

// startRecorder builds a RecorderConfig from entry (plus this manager's
// configured dataDir/segmentSeconds), constructs a RecorderHandle via the
// configured factory, starts it under the manager's root context, and — only
// once Start has actually succeeded — records it in m.active so a later
// stopRecorder/StopAll can find and stop it. A no-op, returning nil, if
// recording hasn't been configured or the manager's root context doesn't
// exist yet (StartAll hasn't run) — callers (StartAll, syncRecording) only
// reach this once both are true, but this guard keeps startRecorder safe to
// call on its own too.
func (m *RecorderManager) startRecorder(entry RecorderEntry) error {
	m.mu.RLock()
	factory := m.recorderFactory
	dataDir := m.dataDir
	segmentSeconds := m.segmentSeconds
	ctx := m.rootCtx
	m.mu.RUnlock()
	if factory == nil || ctx == nil {
		return nil
	}

	cfg := RecorderConfig{
		CameraID:       entry.CameraID,
		StreamURL:      entry.StreamURL,
		Roles:          entry.Config.Roles,
		SegmentSeconds: segmentSeconds,
		DataDir:        dataDir,
		Mode:           entry.Config.Mode,
		PreRollS:       entry.Config.PreRollS,
		PostRollS:      entry.Config.PostRollS,
	}

	handle := factory(cfg)
	if err := handle.Start(ctx); err != nil {
		return fmt.Errorf("start recorder: %w", err)
	}

	m.mu.Lock()
	if m.active == nil {
		m.active = make(map[string]RecorderHandle)
	}
	m.active[entry.CameraID] = handle
	m.mu.Unlock()
	return nil
}

// stopRecorder stops and deregisters cameraID's currently active Recorder,
// if any. A no-op for a camera with none (never started, already stopped,
// or mode "off").
func (m *RecorderManager) stopRecorder(cameraID string) {
	m.mu.Lock()
	handle, ok := m.active[cameraID]
	if ok {
		delete(m.active, cameraID)
	}
	m.mu.Unlock()

	if ok {
		_ = handle.Stop()
	}
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
		CameraID:  cam.ID(),
		Name:      cam.Name(),
		Config:    readRecordingConfig(cam.Storage()),
		StreamURL: cam.StreamURL,
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
