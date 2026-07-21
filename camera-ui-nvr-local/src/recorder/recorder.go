// recorder.go implements the actual single-camera continuous ffmpeg
// recorder (Task 7): pulling one RTSP role, writing segmented fMP4 files to
// disk via ffmpeg, supervising that process (restart-with-backoff on exit
// while the caller's context is still alive), and indexing each finished
// segment into a *store.SegmentStore. This is the runtime engine; the
// registry/config bookkeeping (which cameras are recorded, at what
// mode/retention) is RecorderEntry/RecorderManager in manager.go — a later
// task wires the two together (one *Recorder per continuously-recorded
// camera, constructed from its RecorderEntry.Config).
//
// Event-triggered recording and retention garbage collection are explicitly
// out of scope here (the next two tasks) — this file only ever runs ffmpeg
// continuously for as long as Start's context is alive.
package recorder

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// Backoff/poll tuning. Exposed as package-level defaults (not exported
// constants) so tests can override the Recorder-instance fields
// (pollInterval, sleep) without touching global state.
const (
	defaultSegmentPollInterval = 1 * time.Second
	defaultBackoffInitial      = 1 * time.Second
	defaultBackoffMax          = 30 * time.Second
	// stableRunResetThreshold: an ffmpeg run that lasted at least this long
	// before exiting is treated as "was healthy, then had a one-off hiccup"
	// rather than "crash-looping" — the backoff resets to its initial value
	// instead of continuing to grow, so a recorder that's been recording
	// fine for hours doesn't creep toward defaultBackoffMax on one transient
	// disconnect.
	stableRunResetThreshold = 10 * time.Second
	// stderrTailSize bounds how much of a failed ffmpeg process's stderr is
	// kept and surfaced in logs — enough to carry a meaningful failure
	// reason (auth failure, connection refused, codec/RTSP negotiation
	// errors) without accumulating without bound across a long healthy run.
	stderrTailSize = 8 * 1024
)

// RecordingState is the wire shape polled by the frontend's
// onRecordingState (docs/superpowers/specs/2026-07-19-nvr-frontend-contract.d.ts:
// RecordingState { cameraId, state: 'recording'|'stopped', timestamp }).
type RecordingState struct {
	CameraID    string `msgpack:"cameraId" json:"cameraId"`
	State       string `msgpack:"state" json:"state"`
	TimestampMs int64  `msgpack:"timestamp" json:"timestamp"`
}

// Recording state values, matching the frontend contract's string union
// exactly.
const (
	StateRecording = "recording"
	StateStopped   = "stopped"
)

// RecorderConfig is everything one Recorder instance needs to continuously
// record a single camera. StreamURL is a function rather than a plain
// string because the actual RTSP URL for a role (credentials embedded, etc.)
// may need to be resolved fresh on every (re)connect attempt — e.g. a token
// that expires, or a URL the caller looks up from the live *sdk.CameraDevice
// rather than caching once at construction time.
type RecorderConfig struct {
	CameraID       string
	StreamURL      func(role string) (string, error)
	Roles          []string
	SegmentSeconds int
	DataDir        string

	// Mode, PreRollS, and PostRollS drive event-mode recording (Task 8, see
	// event_mode.go): everything below is a no-op when Mode is anything
	// other than RecordingModeEvents (its zero value included), which is
	// also why every pre-Task-8 caller of RecorderConfig (this field's zero
	// value) keeps behaving exactly like continuous mode.
	//
	// Mode selects, at the moment each segment is finalized, whether it
	// starts out permanently retained (continuous, indexed
	// Referenced=true) or as an events-mode "spool" segment (indexed
	// Referenced=false, only promoted by MarkEvent, otherwise swept once
	// older than PreRollS).
	Mode      RecordingMode
	PreRollS  int
	PostRollS int
}

// commandRunner abstracts "run this ffmpeg invocation until it exits or ctx
// is canceled" so Recorder's supervision/backoff logic can be unit-tested
// with an injected fake instead of spawning real ffmpeg processes and
// depending on real process timing. stderr receives everything the process
// writes to its standard error stream (ffmpeg logs its own diagnostics
// there) so a failing run's actual error can be surfaced, not just its exit
// status. execCommandRunner (the default, production implementation) shells
// out for real.
type commandRunner interface {
	Run(ctx context.Context, name string, args []string, stderr io.Writer) error
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args []string, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = stderr
	return cmd.Run()
}

// tailBuffer is an io.Writer that keeps only the most recent maxSize bytes
// written to it, discarding everything older — used to capture a bounded
// tail of a supervised ffmpeg process's stderr (see stderrTailSize) without
// letting a long-running process accumulate unbounded memory. Safe for
// concurrent use: written to from the OS-pipe-reading goroutine os/exec runs
// internally while cmd.Run() is in flight, and read from (String) by the
// caller after the process exits.
type tailBuffer struct {
	mu      sync.Mutex
	buf     []byte
	maxSize int
}

func newTailBuffer(maxSize int) *tailBuffer {
	return &tailBuffer{maxSize: maxSize}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.buf = append(t.buf, p...)
	if len(t.buf) > t.maxSize {
		t.buf = t.buf[len(t.buf)-t.maxSize:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// Recorder continuously records one camera's configured roles: one
// supervised ffmpeg process per role, each writing segmented fMP4 files that
// get indexed into SegmentStore as they're finished. Safe for concurrent use
// of Start/Stop/State from multiple goroutines.
type Recorder struct {
	cfg      RecorderConfig
	segStore *store.SegmentStore
	ff       *FFmpeg
	log      *sdk.Logger

	// runner, pollInterval, and sleep are overridden by tests to avoid
	// spawning real processes / real wall-clock delays; production code
	// never touches them after construction.
	runner       commandRunner
	pollInterval time.Duration
	sleep        func(ctx context.Context, d time.Duration) bool

	// nowFn is the injected clock (Task 8 fix) used by every event-mode
	// retention decision (event_mode.go: MarkEvent, promoteIfCovered,
	// sweepEventSpool) instead of calling time.Now() directly, so tests can
	// drive "now" deterministically — essential for proving a protected
	// window stays open across a long-active event, and that post-roll
	// segments finalized after the terminal message get promoted, without
	// actually sleeping. Defaults to the real wall clock in NewRecorder;
	// production code never overrides it.
	nowFn func() int64

	// events tracks each currently-relevant detection event's protected
	// retention window (event_mode.go), and retentionMu serializes every
	// promotion pass (MarkEvent, promoteIfCovered) against sweepEventSpool's
	// own read-decide-delete sequence, so the two can never interleave and
	// delete a segment a concurrent promotion was in the middle of
	// retaining (the TOCTOU a plain per-call SegmentStore lock alone
	// couldn't prevent, since it only ever locked one round-trip at a
	// time).
	events      eventWindowSet
	retentionMu sync.Mutex

	// activeDirsMu/activeDirs track the on-disk directory (or directories,
	// one per currently-running role) THIS Recorder's ffmpeg process(es) are
	// writing segments into right now — runOnce registers its role's outDir
	// before starting ffmpeg and deregisters it once ffmpeg exits (whether
	// cleanly or not; see the defer in runOnce). ActiveOutputDirs exposes a
	// snapshot of this set so retention's orphan sweep
	// (recorder/retention.go, sweepOrphanFiles) can exclude it entirely: a
	// file in an active output directory is either mid-write or, in the
	// stalled-RTSP-source case that motivated this, sitting with a stale
	// mtime while ffmpeg still holds its fd open — the mtime>grace heuristic
	// alone can't tell that apart from a genuine orphan, but "is this
	// directory something a live recorder still owns" can. A separate small
	// mutex (not r.mu) since this is read far more often (every retention
	// pass) than r.mu's own state, and updated from runOnce's own goroutine,
	// not Start/Stop's.
	activeDirsMu sync.Mutex
	activeDirs   map[string]int

	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
	state   RecordingState
	wg      sync.WaitGroup
}

// NewRecorder returns a Recorder for cfg, ready to Start. segStore is where
// finished segments are indexed; ff resolves the ffmpeg binary;
// log may be nil (as in unit tests) — every log call below guards for that,
// matching the pattern established by detectionEventIngester
// (events_ingest.go) since neither has anywhere else to report an error
// (ffmpeg supervision runs entirely in background goroutines with no caller
// to return an error to).
func NewRecorder(cfg RecorderConfig, segStore *store.SegmentStore, ff *FFmpeg, log *sdk.Logger) *Recorder {
	return &Recorder{
		cfg:          cfg,
		segStore:     segStore,
		ff:           ff,
		log:          log,
		runner:       execCommandRunner{},
		pollInterval: defaultSegmentPollInterval,
		sleep:        ctxSleep,
		nowFn:        func() int64 { return time.Now().UnixMilli() },
		state:        RecordingState{CameraID: cfg.CameraID, State: StateStopped},
	}
}

// ctxSleep blocks for d, or until ctx is canceled, whichever comes first. It
// reports whether the sleep completed normally (true) or was cut short by
// ctx (false) — a false return means the caller's supervision loop should
// stop rather than proceeding to another restart attempt.
func ctxSleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Start begins supervising one ffmpeg process per configured role. It is
// idempotent: calling Start again while already running is a no-op (returns
// nil without spawning a second set of supervisors). Recording continues
// until ctx is canceled or Stop is called; on an unexpected ffmpeg exit
// while ctx is still alive, the process is restarted with backoff (see
// superviseRole).
//
// Every wg.Add call for this Start happens inside the same r.mu critical
// section that sets running = true, and before r.mu is unlocked. This
// matters: a concurrent Stop() can only observe running == true after
// acquiring r.mu, which (by mutex ordering) can only happen after this
// critical section — and therefore every Add — has already completed. That
// ordering is what makes r.wg.Add always happen-before any r.wg.Wait() Stop
// might go on to call, satisfying sync.WaitGroup's documented contract
// ("calls with a positive delta... must happen before a Wait"). Doing the
// Adds after unlocking (e.g. one per loop iteration, as an earlier version
// of this method did) leaves a window where a concurrent Stop() can call
// Wait() before any Add() has run, which either lets Stop() return before
// the goroutine it should have waited for even starts, or trips
// sync.WaitGroup's own "Add called concurrently with Wait" misuse panic.
func (r *Recorder) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.running = true
	r.setStateLocked(StateRecording)
	roles := r.cfg.Roles
	r.wg.Add(len(roles))
	r.mu.Unlock()

	if len(roles) == 0 {
		r.logf("recorder: %s: Start called with no configured roles; nothing to record", r.cfg.CameraID)
	}

	for _, role := range roles {
		go func(role string) {
			defer r.wg.Done()
			r.superviseRole(runCtx, role)
		}(role)
	}
	return nil
}

// Stop cancels every role's supervised ffmpeg process and waits for their
// goroutines to finish (each one performs a final segment-index sweep before
// returning — see runOnce). Idempotent: calling Stop when not running is a
// no-op.
func (r *Recorder) Stop() error {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return nil
	}
	cancel := r.cancel
	r.mu.Unlock()

	cancel()
	r.wg.Wait()

	r.mu.Lock()
	r.running = false
	r.cancel = nil
	r.setStateLocked(StateStopped)
	r.mu.Unlock()
	return nil
}

// State returns the recorder's current wire-shape recording state.
func (r *Recorder) State() RecordingState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// setStateLocked updates r.state to the given state value, stamped with the
// current time. Callers must hold r.mu.
func (r *Recorder) setStateLocked(state string) {
	r.state = RecordingState{
		CameraID:    r.cfg.CameraID,
		State:       state,
		TimestampMs: time.Now().UnixMilli(),
	}
}

// superviseRole runs one role's ffmpeg process for as long as ctx is alive,
// restarting it with exponential backoff (capped at defaultBackoffMax) on
// every exit that isn't caused by ctx itself being canceled. A run that
// lasted at least stableRunResetThreshold resets the backoff back to
// defaultBackoffInitial, so an intermittent disconnect after hours of good
// recording doesn't inherit a large backoff from an earlier, unrelated
// crash loop.
func (r *Recorder) superviseRole(ctx context.Context, role string) {
	backoff := defaultBackoffInitial

	for ctx.Err() == nil {
		started := time.Now()

		if err := r.runOnce(ctx, role); err != nil {
			r.logf("recorder: %s/%s: ffmpeg exited: %v", r.cfg.CameraID, role, err)
		}

		if ctx.Err() != nil {
			return
		}

		if time.Since(started) >= stableRunResetThreshold {
			backoff = defaultBackoffInitial
		}

		r.logf("recorder: %s/%s: restarting in %s", r.cfg.CameraID, role, backoff)
		if !r.sleep(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// nextBackoff doubles cur, capped at defaultBackoffMax.
func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > defaultBackoffMax {
		return defaultBackoffMax
	}
	return next
}

// runOnce resolves role's stream URL, ensures this run's output directory
// exists, runs ffmpeg (via r.runner, blocking until it exits or ctx is
// canceled) while a concurrent watcher indexes finished segments as they
// appear, and performs one final indexing sweep once ffmpeg has exited
// (whether that's because ctx was canceled or the process died on its own)
// so no completed segment is left un-indexed just because this particular
// attempt ended.
func (r *Recorder) runOnce(ctx context.Context, role string) error {
	url, err := r.cfg.StreamURL(role)
	if err != nil {
		return fmt.Errorf("resolve stream url: %w", err)
	}

	outDir := r.outDir(role, time.Now())
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create output dir %s: %w", outDir, err)
	}

	// Registered for the entire lifetime of this ffmpeg run (deregistered
	// via the deferred call below once it exits, however it exits) — see
	// activeDirs' own doc comment on why retention's orphan sweep needs to
	// know this directory is currently owned by a live recorder, not just
	// that it was recently written to.
	r.registerActiveDir(outDir)
	defer r.deregisterActiveDir(outDir)

	args := r.ff.segmentArgs(url, outDir, r.cfg.SegmentSeconds, role)

	// The watcher gets its own context, independent of ctx: it must still
	// run its final sweep (indexing whatever ffmpeg finished writing) even
	// when ctx is the one that ended the run, so a Stop()/cancellation never
	// drops the last, already-finalized segment.
	watchCtx, stopWatch := context.WithCancel(context.Background())
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		r.watchSegments(watchCtx, outDir, role)
	}()

	stderrTail := newTailBuffer(stderrTailSize)
	runErr := r.runner.Run(ctx, r.ff.Path(), args, stderrTail)

	stopWatch()
	<-watchDone

	if runErr != nil {
		if tail := strings.TrimSpace(stderrTail.String()); tail != "" {
			runErr = fmt.Errorf("%w\nffmpeg stderr (tail):\n%s", runErr, tail)
		}
	}

	return runErr
}

// outDir returns the on-disk directory one role's segments for the given
// moment are written to:
// <DataDir>/recordings/<cameraId>/<YYYY-MM-DD>/<HH>/<role>/ (UTC, so the
// boundary a long-running recording rolls over at is unambiguous
// regardless of host timezone). Segments only roll into a new directory
// when ffmpeg is (re)started — a single ffmpeg process that runs across an
// hour/day boundary keeps writing into the directory chosen when it started;
// see the Concerns section of the task report for why that's an accepted
// limitation of this task's scope rather than something worked around here.
func (r *Recorder) outDir(role string, at time.Time) string {
	at = at.UTC()
	return filepath.Join(r.cfg.DataDir, "recordings", r.cfg.CameraID, at.Format("2006-01-02"), at.Format("15"), role)
}

// registerActiveDir/deregisterActiveDir mark dir as currently owned by a
// live ffmpeg run (registerActiveDir, called once per runOnce before
// starting ffmpeg) or no longer so (deregisterActiveDir, deferred in
// runOnce so it always runs once that ffmpeg process exits, regardless of
// why). A plain refcount (rather than a bool/set membership) so two
// concurrent runs that happen to resolve to the same directory — not
// expected given role is part of outDir's path, but cheap to make safe
// anyway — never have the first one's deregister prematurely evict a
// directory the second is still actively using.
func (r *Recorder) registerActiveDir(dir string) {
	r.activeDirsMu.Lock()
	defer r.activeDirsMu.Unlock()
	if r.activeDirs == nil {
		r.activeDirs = make(map[string]int)
	}
	r.activeDirs[dir]++
}

func (r *Recorder) deregisterActiveDir(dir string) {
	r.activeDirsMu.Lock()
	defer r.activeDirsMu.Unlock()
	if r.activeDirs[dir] <= 1 {
		delete(r.activeDirs, dir)
		return
	}
	r.activeDirs[dir]--
}

// ActiveOutputDirs returns a snapshot of every directory this Recorder is
// currently writing segments into — one per role with a live ffmpeg run in
// progress right now, empty when none are (not yet started, stopped, or
// between a crashed run and its backoff-delayed restart). Satisfies
// RecorderManager's (unexported) activeOutputDirsProvider interface
// (manager.go), which RunRetentionOnce uses to build the exclusion set
// retention's orphan sweep (recorder/retention.go, sweepOrphanFiles) never
// deletes from, regardless of a file's mtime.
func (r *Recorder) ActiveOutputDirs() []string {
	r.activeDirsMu.Lock()
	defer r.activeDirsMu.Unlock()
	if len(r.activeDirs) == 0 {
		return nil
	}
	dirs := make([]string, 0, len(r.activeDirs))
	for dir := range r.activeDirs {
		dirs = append(dirs, dir)
	}
	return dirs
}

// watchSegments polls outDir every r.pollInterval, indexing every *.mp4 file
// except the most recently created one (which ffmpeg is presumably still
// writing to) into r.segStore as soon as it's noticed. processedUpTo tracks
// only the single lexically-highest filename already fully indexed (rather
// than an ever-growing set of every path ever seen), so both memory and the
// per-tick work in sweepSegments stay bounded by how many *new* segments
// appeared since the last tick — not by how long this run has accumulated
// segments in outDir (a long healthy recording keeps writing into the same
// directory; see outDir's doc comment on why directory rollover only
// happens when ffmpeg restarts). When ctx is done (the ffmpeg process this
// watcher was paired with has exited — see runOnce), it performs one last
// sweep that indexes every remaining file, including the one that was
// previously being skipped, since ffmpeg is no longer writing to anything
// in this directory.
func (r *Recorder) watchSegments(ctx context.Context, outDir, role string) {
	var processedUpTo string
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.sweepSegments(outDir, role, &processedUpTo, true)
			return
		case <-ticker.C:
			r.sweepSegments(outDir, role, &processedUpTo, false)
			// The events-mode janitor rides the same tick: see
			// sweepEventSpool's doc comment (event_mode.go) for why it
			// only ever does anything in RecordingModeEvents, and only to
			// spool segments already older than the camera's configured
			// pre-roll AND not covered by any still-open protected event
			// window.
			r.sweepEventSpool()
		}
	}
}

// sweepSegments lists outDir's *.mp4 files (oldest first — files are named
// by creation-epoch-second and ffmpeg creates them strictly in order, so a
// lexical sort matches creation order for as long as every name has the same
// digit count, i.e. until the year 2286), skips every file at or before
// *processedUpTo (already indexed by an earlier tick — sort.SearchStrings
// finds that boundary in the already-sorted list without rescanning it), and
// indexes the rest, skipping the newest file unless final is true (see
// watchSegments). Each indexed segment's end time is derived from the
// immediately following file's own filename-encoded start time
// (parseEpochStartMs), not ffprobe — see finalizeSegment/segmentTimeRange;
// this is exactly why the newest file is left alone until a further-newer
// one appears (or the run ends): only then is its end time actually known.
// *processedUpTo only advances past a file once it has been successfully
// indexed; on the first failure in a tick the sweep stops rather than
// skipping ahead, so a failed file and everything after it are retried
// together, in order, on the next tick instead of ever being silently
// dropped.
func (r *Recorder) sweepSegments(outDir, role string, processedUpTo *string, final bool) {
	files, err := filepath.Glob(filepath.Join(outDir, "*.mp4"))
	if err != nil || len(files) == 0 {
		return
	}
	sort.Strings(files)

	start := 0
	if *processedUpTo != "" {
		start = sort.SearchStrings(files, *processedUpTo) + 1
	}

	limit := len(files)
	if !final {
		limit-- // leave the newest file alone; ffmpeg is still writing it
	}

	for i := start; i < limit; i++ {
		path := files[i]

		var nextStartMs *int64
		if i+1 < len(files) {
			if ms, ok := parseEpochStartMs(files[i+1]); ok {
				nextStartMs = &ms
			}
		}

		seg, err := finalizeSegment(r.ff, r.segStore, r.cfg.CameraID, role, path, r.initiallyReferenced(), nextStartMs, r.cfg.SegmentSeconds)
		if err != nil {
			r.logf("recorder: %s/%s: index segment %s: %v", r.cfg.CameraID, role, path, err)
			return
		}
		*processedUpTo = path

		// Critical 1 fix (Task 8 follow-up): a post-roll segment doesn't
		// exist yet at the moment MarkEvent's own promotion pass runs for
		// an event's terminal message — it's only finalized here, up to
		// PostRollS seconds later. Checking every newly finalized
		// events-mode segment against the currently open protected windows
		// immediately is what actually retains it; see promoteIfCovered.
		r.promoteIfCovered(seg)
	}
}

// postRollWindowMs returns the millisecond post-roll figure event_mode.go's
// eventWindow.window/isOpen should use when deciding how long a window
// stays protected — r.cfg.PostRollS's own value, padded with one extra
// SegmentSeconds when SegmentSeconds >= PostRollS (both converted to ms
// first).
//
// Why the padding: a segment isn't finalized (indexed into segStore, and
// therefore visible to promoteIfCovered) until ffmpeg rotates to the next
// one — which can be up to SegmentSeconds after the last moment it covers.
// When the configured segment length is at or beyond the post-roll window
// itself, the segment carrying the event's actual post-roll footage can
// still be mid-write at the instant isOpen would otherwise report the
// window closed, so promoteIfCovered's "skip if !w.open" guard permanently
// orphans it (it never gets another look once the window is gone). Padding
// the effective post-roll by one more SegmentSeconds keeps the window open
// long enough for that final segment to finish and be checked. Left
// unpadded (this method returns exactly r.cfg.PostRollS in ms) when
// SegmentSeconds is comfortably smaller than PostRollS, since in that case
// the segment finalizes well within the post-roll window already and
// widening it further would just over-retain unrelated footage for no
// reason.
func (r *Recorder) postRollWindowMs() int64 {
	postRollMs := int64(r.cfg.PostRollS) * 1000
	segMs := int64(r.cfg.SegmentSeconds) * 1000
	if segMs >= postRollMs {
		postRollMs += segMs
	}
	return postRollMs
}

// initiallyReferenced reports the Referenced value a newly finalized segment
// should be indexed with: false (an events-mode "spool" segment, subject to
// sweepEventSpool until/unless MarkEvent promotes it) only when this
// recorder is in RecordingModeEvents; true — permanently retained from the
// moment it's indexed — for every other mode (continuous, and the zero
// value, so existing callers that never set Mode are unaffected).
func (r *Recorder) initiallyReferenced() bool {
	return r.cfg.Mode != RecordingModeEvents
}

// logf logs through r.log if one was provided (see NewRecorder's doc
// comment on why log may be nil).
func (r *Recorder) logf(format string, args ...any) {
	if r.log == nil {
		return
	}
	r.log.Log(fmt.Sprintf(format, args...))
}

// finalizeSegment derives path's (a segment file ffmpeg has finished
// writing) start/end window (segmentTimeRange) and best-effort audio/codec
// info (FFmpeg.probeCodecInfo), and indexes it into segStore as a
// store.Segment for cameraID/role, with Referenced set to referenced (see
// initiallyReferenced: false for an events-mode spool segment, true
// otherwise). Returns the indexed segment (with its assigned ID) on success.
//
// Deliberately ffprobe-free (see ffmpeg.go's FFmpeg doc comment for why:
// node-av, the core's bundled toolchain, ships no ffprobe binary at all).
// HasVideo is always true — every segment comes from stream-copying a
// camera's video RTSP role — and HasAudio/Codec come from
// probeCodecInfo's best-effort parse of `ffmpeg -i path`'s stderr, which
// never blocks indexing on a parse miss (see that method's doc comment).
func finalizeSegment(ff *FFmpeg, segStore *store.SegmentStore, cameraID, role, path string, referenced bool, nextStartMs *int64, segmentSeconds int) (store.Segment, error) {
	startMs, endMs, err := segmentTimeRange(path, nextStartMs, segmentSeconds)
	if err != nil {
		return store.Segment{}, err
	}

	hasAudio, codec := ff.probeCodecInfo(path)

	seg := store.Segment{
		CameraID:   cameraID,
		Role:       role,
		Path:       path,
		StartMs:    startMs,
		EndMs:      endMs,
		HasVideo:   true,
		HasAudio:   hasAudio,
		Codec:      codec,
		Referenced: referenced,
	}

	id, err := segStore.Add(seg)
	if err != nil {
		return store.Segment{}, fmt.Errorf("recorder: index segment %s: %w", path, err)
	}
	seg.ID = id
	return seg, nil
}

// parseEpochStartMs parses path's basename as the plain-integer Unix epoch
// second ffmpeg's -strftime 1 "%s.mp4" segment naming produces (see
// FFmpeg.segmentArgs), returning it in milliseconds. ok is false for any
// basename that isn't a positive plain integer (e.g. a hand-picked fixture
// filename in a test).
func parseEpochStartMs(path string) (ms int64, ok bool) {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	epochSeconds, err := strconv.ParseInt(base, 10, 64)
	if err != nil || epochSeconds <= 0 {
		return 0, false
	}
	return epochSeconds * 1000, true
}

// segmentTimeRange derives a segment file's [startMs, endMs) window without
// ffprobe. ffmpeg's segment muxer names each file after the Unix epoch
// second it was opened at (parseEpochStartMs), so the primary path reads
// that directly as the exact start time. endMs is the immediately following
// segment's own start time when the caller has one (nextStartMs —
// sweepSegments always supplies this once a segment is superseded by a
// newer one, since the two are sequential: the moment ffmpeg stopped
// writing this file is exactly the moment it started the next one);
// otherwise (the last segment seen so far, still possibly being written, or
// the very last file in a final sweep) it's estimated as
// startMs + segmentSeconds.
//
// Any file whose basename isn't a plain integer (e.g. a hand-picked fixture
// file in a test, or a segment produced by some other naming scheme) falls
// back to the same nextStartMs-or-estimate logic for endMs, using the
// file's mtime as the estimate's basis instead of a parsed start, and
// derives startMs as endMs minus the segmentSeconds estimate.
func segmentTimeRange(path string, nextStartMs *int64, segmentSeconds int) (startMs, endMs int64, err error) {
	estimatedDurationMs := int64(segmentSeconds) * 1000

	if ms, ok := parseEpochStartMs(path); ok {
		startMs = ms
		if nextStartMs != nil {
			return startMs, *nextStartMs, nil
		}
		return startMs, startMs + estimatedDurationMs, nil
	}

	info, statErr := os.Stat(path)
	if statErr != nil {
		return 0, 0, fmt.Errorf("recorder: stat %s: %w", path, statErr)
	}
	endMs = info.ModTime().UnixMilli()
	if nextStartMs != nil {
		endMs = *nextStartMs
	}
	startMs = endMs - estimatedDurationMs
	return startMs, endMs, nil
}
