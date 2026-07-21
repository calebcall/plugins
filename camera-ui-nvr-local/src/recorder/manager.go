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
//
// SourceRoles enumerates the role strings this camera's actual stream
// sources offer, in source order (e.g. ["high-resolution", "low-resolution"]
// for a typical two-stream camera). It exists so startRecorder (see
// resolveRoles) can narrow a camera's configured/default recording roles
// down to ones the camera actually has, and fall back to the camera's real
// roles when the configured one doesn't exist on it — robustness against the
// stored/default role ("high-resolution") not matching every camera source's
// naming. An empty return means "camera reports no sources at all" (rather
// than "record nothing"): resolveRoles treats that as best-effort and keeps
// the configured/default roles unchanged.
type ManagedCamera interface {
	ID() string
	Name() string
	Storage() CameraStorage
	StreamURL(role string) (string, error)
	SourceRoles() []string
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

	// SourceRoles is cam.SourceRoles(), captured at newRecorder time for the
	// same reason StreamURL is: startRecorder needs it (via resolveRoles) to
	// narrow/fall-back Config.Roles to what this camera's sources actually
	// offer, well after the original ManagedCamera has gone out of scope.
	SourceRoles []string
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

// activeOutputDirsProvider is an optional capability a RecorderHandle may
// implement (checked via a type assertion in RecorderManager.
// ActiveOutputDirs, below, so a RecorderHandle that doesn't — e.g. every
// fake used by this package's own orchestration tests — simply contributes
// nothing, rather than being required to implement a method it has no
// meaningful answer for). The real *Recorder satisfies it directly
// (recorder.go's ActiveOutputDirs); production's recorderHandleWithRegistry
// wrapper (plugin.go) forwards to it.
type activeOutputDirsProvider interface {
	ActiveOutputDirs() []string
}

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

	// camLocksMu/camLocks serialize, per camera ID, the "stop the currently
	// active handle, then (unless mode is off) start a fresh one" sequence
	// (see startOrRestartRecorder) — the review fix for a concurrent-Add
	// leak: without a per-ID lock, two overlapping calls for the SAME
	// camera ID could each successfully Start a handle, with the loser's
	// handle silently overwritten (and never Stopped) in m.active once the
	// winner's write ran after it — a goroutine/ffmpeg leak invisible to
	// StopAll/Shutdown, since the leaked handle is no longer reachable from
	// m.active at all.
	//
	// Deliberately a SEPARATE lock from m.mu, keyed per camera ID: the
	// whole point is to hold exclusivity across the blocking
	// handle.Start/Stop calls (which may wait out real ffmpeg/supervision
	// goroutines) without ever holding m.mu itself across that wait — m.mu
	// is still only ever held for short, independent map reads/writes
	// inside startRecorder/stopRecorder. camLocks entries are created
	// lazily and never removed (bounded by the number of distinct camera
	// IDs ever seen across this manager's lifetime, which for a single NVR
	// instance is small).
	camLocksMu sync.Mutex
	camLocks   map[string]*sync.Mutex

	// log reports recorder lifecycle events (started/stopped/restarted,
	// StartAll summaries, start failures) — see logf/warnf. nil (the zero
	// value, matching every test that doesn't call SetLogger) silently
	// skips every log call rather than panicking, the same nil-logger
	// tolerance recorder.go's Recorder.logf already established.
	log *sdk.Logger

	// stateNotify, if set (via SetStateNotifier), is invoked every time a
	// managed camera's live Recorder actually starts or stops — i.e. from
	// the exact same two call sites as the "recorder: started/stopped
	// camera ..." logf lines below, not from Config.Mode alone. nil (the
	// zero value, matching every test that doesn't call SetStateNotifier)
	// silently skips notification, the same optional-hook convention log
	// already established. Production wiring (plugin.go's NewPlugin) sets
	// this to a closure that fans the transition out to this plugin's
	// OnRecordingState subscribers (and, as a minimal SystemEvent
	// producer, its OnSystemEvent subscribers too).
	stateNotify func(cameraID string, recording bool)
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
//
// Once recording has been launched (StartAll has run), this also
// starts/restarts/stops that camera's live Recorder to match its
// (re-)resolved config — see syncRecording. Before StartAll, this only
// updates the registry; StartAll picks up whatever's registered when it
// eventually runs.
//
// KNOWN LIMITATION (documented, not fixed, per review): re-adding an
// already-known camera ID DOES re-read its config from scratch and restart
// its Recorder to match — so Add is technically capable of being the
// mechanism a live per-camera settings edit (recordingMode/roles/pre-post
// roll) flows through. But nothing in this plugin actually calls Add for
// that reason today: the SDK's sdk.Plugin interface has no "camera config
// changed" hook at all (only ConfigureCameras once at startup, and
// OnCameraAdded/OnCameraReleased for assignment changes — see sdk.Plugin's
// own doc comment, "the host calls these methods... as the user adds or
// removes cameras", nothing about editing an already-assigned one). So in
// production, editing an already-adopted camera's recording settings has NO
// effect on its already-running Recorder until the plugin (or camera.ui
// itself) restarts and ConfigureCameras runs again with the new values. A
// later task could close this gap with a periodic reconcile pass (re-read
// every managed camera's stored config on a timer and call Add for any that
// changed) — out of scope here.
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
// was already mode "off"). Takes the same per-camera-ID lock
// startOrRestartRecorder does, so a Remove can never race a concurrent
// Add/restart for the same camera into the handle-leak this file's review
// fix closed (see camLocks's doc comment).
//
// notifyState (when stopRecorder reports it actually stopped something) is
// called AFTER lock.Unlock() — same lock-scope reasoning as
// startOrRestartRecorder: subscriber fan-out must never run while camLock is
// held. See notifyState's doc comment.
func (m *RecorderManager) Remove(cameraID string) error {
	m.mu.Lock()
	delete(m.recorders, cameraID)
	m.mu.Unlock()

	lock := m.camLock(cameraID)
	lock.Lock()
	stopped := m.stopRecorder(cameraID)
	lock.Unlock()

	if stopped {
		m.notifyState(cameraID, false)
	}
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
//
// See SetLogger for wiring this manager's lifecycle logging, and Add's own
// doc comment for the known limitation that a config change to an
// already-adopted camera has no live effect (no SDK hook fires for it) —
// neither of those is a ConfigureRecording concern, called out here only so
// a reader wiring this up (plugin.go) sees both nearby.
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

// SetLogger wires the *sdk.Logger RecorderManager uses to report recorder
// lifecycle events: started/stopped/restarted, a StartAll summary, and a
// warning when a camera's Recorder fails to start. Optional — a manager
// with no logger set (the zero value, matching every test that predates
// this) silently skips every log call via logf/warnf instead of panicking,
// mirroring Recorder's own nil-logger tolerance (recorder.go). Safe to call
// at any time; production wiring (plugin.go) calls it once, alongside
// ConfigureRecording.
func (m *RecorderManager) SetLogger(log *sdk.Logger) {
	m.mu.Lock()
	m.log = log
	m.mu.Unlock()
}

// SetStateNotifier wires fn as this manager's recording-lifecycle
// notification hook — see stateNotify's doc comment. Safe to call at any
// time; production wiring (plugin.go) calls it once, alongside SetLogger.
func (m *RecorderManager) SetStateNotifier(fn func(cameraID string, recording bool)) {
	m.mu.Lock()
	m.stateNotify = fn
	m.mu.Unlock()
}

// notifyState invokes the configured stateNotify hook, if any, for
// cameraID's start/stop transition. Deliberately reads the hook under m.mu
// and then calls it OUTSIDE the lock: fn (production: a closure over the
// parent plugin's subscriber registries, plus a synchronous
// SystemEventStore.Insert) must never be called while m.mu is held, since a
// subscriber callback or a concurrent RPC-goroutine register/unregister
// could otherwise deadlock against this manager's own lock — this mirrors
// logf/warnf's identical "copy the field under RLock, call it unlocked"
// shape.
//
// Just as importantly, EVERY caller of notifyState (startOrRestartRecorder,
// Remove) must itself call this only AFTER releasing camLock(cameraID), not
// while still holding it — subscriber fan-out and the DB write it can
// trigger are unbounded work with no business running inside a lock whose
// only job is to serialize one camera's stop→start sequence, and a
// subscriber callback that re-enters RecorderManager for the SAME camera ID
// (e.g. calling Remove from inside an OnRecordingState callback) would
// self-deadlock against camLock's non-reentrant sync.Mutex if it were still
// held here. See TestOnRecordingState_CallbackDoesNotDeadlockReenteringSameCameraLock
// (rpc_subscriptions_test.go, package main) for the regression test.
func (m *RecorderManager) notifyState(cameraID string, recording bool) {
	m.mu.RLock()
	fn := m.stateNotify
	m.mu.RUnlock()
	if fn != nil {
		fn(cameraID, recording)
	}
}

// IsActive reports whether cameraID currently has a live, started
// RecorderHandle tracked in m.active — i.e. whether it is actually
// recording right now, as opposed to merely being configured with a
// non-off RecordingMode (see rpc_recording.go's own IsRecording
// approximation, which intentionally uses the cheaper Config.Mode check
// instead — this method is for OnRecordingState's "current state on
// subscribe" emit, which needs the real answer).
func (m *RecorderManager) IsActive(cameraID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.active[cameraID]
	return ok
}

// ActiveOutputDirs returns the union of every currently-active
// RecorderHandle's own ActiveOutputDirs() (handles that don't implement
// activeOutputDirsProvider — every fake in this package's own tests, plus
// any future RecorderHandle that never wires it up — simply contribute
// nothing, not an error). Called fresh by RunRetentionOnce on every
// retention pass (never cached), so it always reflects whichever
// directories are being written to RIGHT NOW: retention's orphan sweep
// (recorder/retention.go, sweepOrphanFiles) excludes every one of them
// entirely, regardless of a file's mtime — the defense against a
// stalled-but-still-open segment (e.g. an RTSP source hang while ffmpeg
// still holds the file open) being misclassified as orphaned by the
// mtime>grace heuristic alone. Always non-nil.
func (m *RecorderManager) ActiveOutputDirs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	dirs := make([]string, 0, len(m.active))
	for _, handle := range m.active {
		if provider, ok := handle.(activeOutputDirsProvider); ok {
			dirs = append(dirs, provider.ActiveOutputDirs()...)
		}
	}
	return dirs
}

func (m *RecorderManager) logf(format string, args ...any) {
	m.mu.RLock()
	log := m.log
	m.mu.RUnlock()
	if log == nil {
		return
	}
	log.Log(fmt.Sprintf(format, args...))
}

func (m *RecorderManager) warnf(format string, args ...any) {
	m.mu.RLock()
	log := m.log
	m.mu.RUnlock()
	if log == nil {
		return
	}
	log.Warn(fmt.Sprintf(format, args...))
}

// camLock returns the per-camera-ID mutex startOrRestartRecorder/Remove hold
// across their stop/start sequence for cameraID, creating it on first use.
// See camLocks's doc comment for why this is a separate lock from m.mu.
func (m *RecorderManager) camLock(cameraID string) *sync.Mutex {
	m.camLocksMu.Lock()
	defer m.camLocksMu.Unlock()
	if m.camLocks == nil {
		m.camLocks = make(map[string]*sync.Mutex)
	}
	l, ok := m.camLocks[cameraID]
	if !ok {
		l = &sync.Mutex{}
		m.camLocks[cameraID] = l
	}
	return l
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

	started := 0
	attempted := 0
	var errs []error
	for _, entry := range m.entriesSnapshot() {
		if entry.Config.Mode == RecordingModeOff {
			continue
		}
		attempted++
		// Takes the per-camera-ID lock (via startOrRestartRecorder, which
		// no-ops the "stop" half since nothing is active for a camera yet
		// at initial launch) rather than calling startRecorder directly, so
		// a concurrent Add for the same camera ID racing this very first
		// pass can't double-start it either — the same exclusion Add/Remove
		// rely on for a config-change restart.
		if err := m.startOrRestartRecorder(entry); err != nil {
			errs = append(errs, fmt.Errorf("camera %s: %w", entry.CameraID, err))
			continue
		}
		started++
	}
	m.logf("recorder: StartAll: started %d of %d managed camera(s)", started, attempted)
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
// already-managed one (Add's own doc comment, including its documented
// limitation that nothing currently triggers this for a live settings edit)
// take effect immediately once something does call Add.
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

	return m.startOrRestartRecorder(entry)
}

// startOrRestartRecorder serializes, per camera ID (via camLock), the
// sequence "stop any currently active handle for this camera, then (unless
// mode is off) build and start a fresh one from entry's current config".
//
// This per-ID lock is the review fix for a handle leak: without it, two
// concurrent callers for the SAME camera ID (e.g. two overlapping Add
// calls, or Add racing StartAll's own initial pass for a camera Add already
// registered before StartAll ran) could each independently read "nothing
// active yet"/"stop then start", and each successfully Start a handle —
// with the loser's handle silently overwritten (and never Stopped) in
// m.active once the winner's write ran after it. Holding camLock(cameraID)
// for the whole stop→start→record sequence means only one such sequence can
// ever be in flight for a given camera ID at a time, so m.active always
// ends up holding exactly the most recently started handle, and every
// handle that was ever started is either that one or was Stopped by the
// next sequence that replaced it.
//
// Deliberately does NOT hold m.mu itself across this — see camLocks's doc
// comment — so a concurrent call for a DIFFERENT camera ID, or an unrelated
// read (ManagedCameraIDs, Camera, entriesSnapshot), is never blocked
// waiting on this one's potentially-slow handle.Start/Stop.
//
// notifyState (Task SUBS's producer hook) is deliberately called AFTER
// lock.Unlock(), not while camLock is still held — see notifyState's own
// doc comment for why holding a per-camera lock across subscriber fan-out
// (unbounded work, including a synchronous DB write) is a lock-scope bug
// and a self-deadlock risk. stopRecorder/startRecorder below only report
// WHETHER a transition happened (via their bool returns); this function is
// solely responsible for turning those into notifyState calls, once the
// lock is safely released.
func (m *RecorderManager) startOrRestartRecorder(entry RecorderEntry) error {
	lock := m.camLock(entry.CameraID)
	lock.Lock()

	stopped := m.stopRecorder(entry.CameraID)

	var started bool
	var err error
	if entry.Config.Mode != RecordingModeOff {
		if stopped {
			m.logf("recorder: restarting camera %s (config changed)", entry.CameraID)
		}
		started, err = m.startRecorder(entry)
	}

	lock.Unlock()

	if stopped {
		m.notifyState(entry.CameraID, false)
	}
	if started {
		m.notifyState(entry.CameraID, true)
	}
	return err
}

// startRecorder builds a RecorderConfig from entry (plus this manager's
// configured dataDir/segmentSeconds), constructs a RecorderHandle via the
// configured factory, starts it under the manager's root context, and — only
// once Start has actually succeeded — records it in m.active so a later
// stopRecorder/StopAll can find and stop it. A no-op, returning (false, nil),
// if recording hasn't been configured or the manager's root context doesn't
// exist yet (StartAll hasn't run) — callers (StartAll, syncRecording, both
// via startOrRestartRecorder) only reach this once both are true, but this
// guard keeps startRecorder safe to call on its own too.
//
// Returns (started, err): started is true only when a handle was actually
// built and Start succeeded — the caller (startOrRestartRecorder) uses this
// to decide whether to notifyState(cameraID, true) once camLock is released,
// rather than this method calling notifyState itself while still holding
// it. See notifyState's doc comment for why that split matters.
//
// Callers must hold camLock(entry.CameraID) — see startOrRestartRecorder.
func (m *RecorderManager) startRecorder(entry RecorderEntry) (bool, error) {
	m.mu.RLock()
	factory := m.recorderFactory
	dataDir := m.dataDir
	segmentSeconds := m.segmentSeconds
	ctx := m.rootCtx
	m.mu.RUnlock()
	if factory == nil || ctx == nil {
		return false, nil
	}

	roles := resolveRoles(entry.Config.Roles, entry.SourceRoles)

	cfg := RecorderConfig{
		CameraID:       entry.CameraID,
		StreamURL:      entry.StreamURL,
		Roles:          roles,
		SegmentSeconds: segmentSeconds,
		DataDir:        dataDir,
		Mode:           entry.Config.Mode,
		PreRollS:       entry.Config.PreRollS,
		PostRollS:      entry.Config.PostRollS,
	}

	handle := factory(cfg)
	if err := handle.Start(ctx); err != nil {
		m.warnf("recorder: camera %s: start failed: %v", entry.CameraID, err)
		return false, fmt.Errorf("start recorder: %w", err)
	}

	m.mu.Lock()
	if m.active == nil {
		m.active = make(map[string]RecorderHandle)
	}
	m.active[entry.CameraID] = handle
	m.mu.Unlock()

	m.logf("recorder: started camera %s (mode=%s roles=%v)", entry.CameraID, entry.Config.Mode, roles)
	return true, nil
}

// resolveRoles narrows configured (entry.Config.Roles — the camera's
// stored/default recording roles) down to the ones actually offered by
// available (entry.SourceRoles — the camera's real stream sources),
// preserving configured's order, so the recorder is never pointed at a role
// string the camera doesn't have. This is the robustness fix layered on top
// of readRecordingConfig's own empty->defaultRoles fallback: even a
// correctly-resolved "high-resolution" default is wrong for a camera whose
// sources are named something else entirely.
//
//   - available empty (camera reports no sources at all — a ManagedCamera
//     that can't yet answer this, or a genuinely sourceless camera): returns
//     configured unchanged. There's nothing more useful to narrow against,
//     and the existing "no roles / stream error" logging in the recorder
//     start path is the safety net for this case.
//   - intersection non-empty: returns just the configured roles that
//     available actually offers, in configured's order — e.g. configured
//     ["high-resolution"] against available ["high-resolution",
//     "low-resolution"] returns ["high-resolution"], not every role the
//     camera has.
//   - intersection empty (configured names a role this camera doesn't have
//     — e.g. a non-amcrest source named "main-hd" instead of
//     "high-resolution"): falls back to available itself, so recording still
//     happens using whatever roles the camera actually offers instead of
//     silently recording nothing.
func resolveRoles(configured, available []string) []string {
	if len(available) == 0 {
		return configured
	}

	availableSet := make(map[string]struct{}, len(available))
	for _, role := range available {
		availableSet[role] = struct{}{}
	}

	var intersection []string
	for _, role := range configured {
		if _, ok := availableSet[role]; ok {
			intersection = append(intersection, role)
		}
	}
	if len(intersection) == 0 {
		return available
	}
	return intersection
}

// stopRecorder stops and deregisters cameraID's currently active Recorder,
// if any, and reports whether there was one to stop (so
// startOrRestartRecorder can tell a genuine restart from a first start for
// its log line, AND — since this method deliberately does NOT call
// notifyState itself, see below — whether its caller owes a
// notifyState(cameraID, false) once camLock is released). A no-op —
// returning false — for a camera with none (never started, already
// stopped, or mode "off").
//
// Deliberately does not call notifyState: every caller (startOrRestartRecorder,
// Remove) holds camLock(cameraID) across this call, and notifyState fans out
// to subscriber callbacks (plus a synchronous SystemEventStore.Insert) —
// unbounded work that must never run while camLock is held (see notifyState's
// doc comment for the self-deadlock risk this avoids: a callback re-entering
// RecorderManager for the SAME camera ID, e.g. calling Remove from inside an
// OnRecordingState subscriber, would otherwise deadlock against this
// non-reentrant per-camera lock). Callers are responsible for calling
// notifyState(cameraID, false) themselves, after releasing camLock, when this
// returns true.
//
// Callers must hold camLock(cameraID) — see startOrRestartRecorder; Remove
// (the other caller) takes it directly itself.
func (m *RecorderManager) stopRecorder(cameraID string) bool {
	m.mu.Lock()
	handle, ok := m.active[cameraID]
	if ok {
		delete(m.active, cameraID)
	}
	m.mu.Unlock()

	if !ok {
		return false
	}
	_ = handle.Stop()
	m.logf("recorder: stopped camera %s", cameraID)
	return true
}

// Camera returns the registered Recorder for id, if any.
func (m *RecorderManager) Camera(id string) (*RecorderEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.recorders[id]
	return r, ok
}

// CameraName returns cameraID's display name — RecorderEntry.Name, itself
// captured from ManagedCamera.Name() at the most recent Configure/Add call
// for this camera — or ("", false) if this manager has no entry for it at
// all (never assigned, or already Removed). Satisfies the parent package's
// cameraNamer interface (events_ingest.go), which detectionEventIngester's
// notify uses to title push notifications with a camera's human-readable
// name (e.g. "Sideyard — Person") instead of falling back to its bare ID.
func (m *RecorderManager) CameraName(cameraID string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.recorders[cameraID]
	if !ok {
		return "", false
	}
	return r.Name, true
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
		CameraID:    cam.ID(),
		Name:        cam.Name(),
		Config:      readRecordingConfig(cam.Storage()),
		StreamURL:   cam.StreamURL,
		SourceRoles: cam.SourceRoles(),
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
			Type:         sdk.JsonSchemaTypeArray,
			Key:          keyRoles,
			Title:        "Recorded Stream Roles",
			Hidden:       true,
			Store:        &storeTrue,
			DefaultValue: defaultRoles,
			Items:        &sdk.JsonSchema{Type: sdk.JsonSchemaTypeString},
		},
	}
}

// readRecordingConfig declares the recording config schema on storage (so
// defaults exist and edits can persist) and resolves the current
// RecordingConfig from it. Unset values resolve to the defaults above;
// an invalid/corrupt stored recordingMode (e.g. hand-edited storage.json,
// or a value from a future schema version this build doesn't know) falls
// back to RecordingModeOff rather than recording unexpectedly or panicking.
//
// Roles gets its own empty->defaultRoles fallback below, in addition to
// (not instead of) recordingConfigSchema's own keyRoles DefaultValue: every
// camera that was ever loaded before this fix has an explicitly-stored EMPTY
// roles value on disk (DefineSchemas only seeds a DefaultValue for keys with
// no stored value at all, and an empty []string still counts as stored), so
// the schema DefaultValue alone only prevents this for cameras that haven't
// stored anything yet — it can't retroactively fix an already-empty stored
// value. This was the actual production bug: every managed camera started
// its Recorder with roles=[] and recorded nothing.
func readRecordingConfig(storage CameraStorage) RecordingConfig {
	storage.DefineSchemas(recordingConfigSchema())

	mode, _ := storage.GetValue(keyRecordingMode, string(RecordingModeOff)).(string)
	recordingMode := RecordingMode(mode)
	switch recordingMode {
	case RecordingModeOff, RecordingModeContinuous, RecordingModeEvents:
	default:
		recordingMode = RecordingModeOff
	}

	roles := stringSliceValue(storage.GetValue(keyRoles, defaultRoles))
	if len(roles) == 0 {
		roles = append([]string(nil), defaultRoles...)
	}

	return RecordingConfig{
		Mode:          recordingMode,
		RetentionDays: intValue(storage.GetValue(keyRetentionDays, defaultRetentionDays), defaultRetentionDays),
		PreRollS:      intValue(storage.GetValue(keyPreRollS, defaultPreRollS), defaultPreRollS),
		PostRollS:     intValue(storage.GetValue(keyPostRollS, defaultPostRollS), defaultPostRollS),
		Roles:         roles,
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
