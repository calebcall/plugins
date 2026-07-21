// retention.go implements Task 9's retention garbage collection: a
// background housekeeping pass, run periodically across every camera this
// RecorderManager manages, that deletes recorded segments (and their files)
// once they fall outside retention — by age (per-camera RetentionDays)
// and/or an instance-wide disk cap (nvrQuotaGB, oldest-first across every
// managed camera) — cascading to the events/thumbnails/vector rows tied to
// whatever was removed.
//
// # Quota scope (Task 9 review fix)
//
// The disk cap is deliberately instance-wide, not per-camera: the frontend
// contract (docs/superpowers/specs/2026-07-19-nvr-frontend-contract.d.ts,
// StorageStats) has exactly one top-level nvrQuotaGB/nvrUsedGB for the whole
// NVR, and getStorageStats() takes no camera argument — CameraStorageStats
// has no quota field of its own. An earlier version of this file stored
// nvrQuotaGB per-camera (in the same per-camera DeviceStorage RecordingConfig
// reads from) and enforced it independently for each one, which would let N
// cameras each consume up to the "cap" for N times the intended total usage,
// and can't be surfaced by a future getStorageStats() the way the contract
// shape implies. ConfigureRetention now takes a quotaGB func() float64
// instead — read from the plugin's own instance-level storage (plugin.go),
// not any camera's — and enforceInstanceQuota (below) computes total usage
// and deletes oldest-first across every managed camera against that single
// cap.
//
// This is deliberately distinct from Task 8's event-mode spool sweep
// (event_mode.go's sweepEventSpool): that runs per-Recorder, every
// watchSegments tick, and only ever touches unreferenced events-mode
// "spool" segments nothing has claimed yet. This file runs per
// RecorderManager (across every managed camera, referenced or not) on a
// much coarser interval, and is the only place segments/events are deleted
// purely because they've aged out or a camera's disk quota is exceeded.
// Neither piece calls into the other.
//
// # Design
//
//   - RunRetentionOnce(nowMs) is the single, synchronous, one-shot GC pass
//     the task brief requires: for every entriesSnapshot() camera, run the
//     age cutoff (if RetentionDays > 0 — always true today, since
//     defaultRetentionDays is 7 and the schema's Minimum is 1, but a
//     defensive check costs nothing) and then, if NvrQuotaGB > 0, the
//     disk-cap sweep. Both funnel through deleteSegmentsAndCascadeOlderThan,
//     which is where SegmentStore.DeleteOlderThan's returned paths actually
//     get os.Remove'd and where the newly-freed time range's events (and
//     their thumbnail files / vector rows) get cascaded away too.
//   - StartRetention/StopRetention wrap RunRetentionOnce in a background
//     ticker, following the exact cancel-then-Wait leak-safety shape
//     Recorder.Start/Stop (recorder.go) already established: Stop cancels a
//     context and blocks on a done channel the ticker goroutine closes on
//     exit, so it can never return while that goroutine is still running.
//   - The ticker itself is abstracted behind the small `ticker` interface
//     below (gc.newTicker), not called via time.NewTicker directly, so tests
//     can drive ticks deterministically (send to a channel they control)
//     instead of depending on real elapsed wall-clock time — the same
//     "injectable" spirit as Recorder's r.sleep/r.nowFn (recorder.go), just
//     shaped for a periodic tick source rather than a single delay/clock
//     read.
package recorder

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// msPerDay converts RecordingConfig.RetentionDays into milliseconds for the
// age cutoff computed in RunRetentionOnce.
const msPerDay = 24 * 60 * 60 * 1000

// bytesPerGB converts the instance-wide nvrQuotaGB setting into bytes for
// enforceInstanceQuota. Decimal (1e9), matching how disk quotas are
// conventionally advertised (GB, not GiB) — this task has no other
// convention to match, and the exact boundary doesn't matter for correctness
// (the cap is enforced by comparing this same constant on both the "are we
// over" check and the "how much do we need to free" loop).
const bytesPerGB = 1_000_000_000

// defaultRetentionInterval is the ticker period StartRetention uses when
// called with interval <= 0.
//
// Was time.Hour; changed to 10 minutes (production bug fix — see below).
// At sustained high-ingest rates (observed ~1.5GB/min across a multi-camera
// instance), an hourly pass lets disk usage overshoot a configured
// nvrQuotaGB cap by up to ~90GB before the next pass claws it back — and if
// the process restarts anywhere in that hour (StartRetention previously had
// no immediate run either — see StartRetention's own doc comment), the
// window resets and GC can go long stretches without ever firing. 10
// minutes bounds the same overshoot to ~1.5GB/min * 10min ≈ 15GB, a much
// tighter tolerance around the cap, without re-scanning every camera's full
// segment/event set often enough to matter for cost.
const defaultRetentionInterval = 10 * time.Minute

// orphanGrace is how recently a *.mp4 file under
// "<recordingsDir>/recordings" must have been modified for
// sweepOrphanFiles to leave it alone even though it has no matching
// segments-table row. A recorder writes a segment file, then finalizes and
// indexes it into SegmentStore some short time later (see recorder.go's
// watchSegments/sweepSegments) — a file that's simply mid-write, or
// finished writing but not indexed yet, has no row for exactly that
// ordinary reason and must never be mistaken for an orphan. 5 minutes is
// comfortably more than 2x defaultSegmentSeconds (60s, manager.go) plus any
// realistic indexing lag, while still being short enough that a genuinely
// orphaned file (left behind by a crashed/restarted recorder that never
// got to finalize/index it — this file's Task 9 review bug: such files sit
// on disk, uncounted and undeleted, forever) is reclaimed within one
// retention pass of aging out of that window.
const orphanGrace = 5 * time.Minute

// ticker abstracts the periodic tick source StartRetention's background loop
// waits on. Production uses realTicker (wrapping time.Ticker); tests inject
// a fake whose C() channel they control directly, so a tick can be fired
// deterministically without waiting on real elapsed time — see
// TestStartRetention_TickTriggersRunRetentionOnce.
type ticker interface {
	C() <-chan time.Time
	Stop()
}

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

func newRealTicker(d time.Duration) ticker { return realTicker{t: time.NewTicker(d)} }

// retentionGC holds the storage dependencies and background-ticker state
// that power RecorderManager's retention garbage collection. A
// RecorderManager holds at most one of these (m.gc, manager.go), allocated
// by ConfigureRetention; every method below is reached only through the
// RecorderManager methods further down, which all treat a nil m.gc as
// "retention not configured" rather than dereferencing it.
type retentionGC struct {
	segStore   *store.SegmentStore
	eventStore *store.EventStore
	vectors    []store.VectorBackend

	// recordingsDir is the resolved base directory new recordings are
	// written under (plugin.go's p.recordingsDir — the SAME directory
	// RecorderManager.ConfigureRecording's dataDir and Recorder.outDir's
	// "<DataDir>/recordings/..." are rooted at), used by sweepOrphanFiles to
	// find "<recordingsDir>/recordings" and walk it for *.mp4 files with no
	// segments-table row. Empty (the zero value — every test that doesn't
	// care about the orphan sweep, and any production build where db failed
	// to open) makes sweepOrphanFiles an unconditional no-op rather than
	// walking an unintended directory (e.g. the working directory) or
	// erroring.
	recordingsDir string

	// quotaGB, if non-nil, returns the current instance-wide disk cap in
	// gigabytes (0/negative means uncapped) — called fresh on every
	// RunRetentionOnce pass rather than captured once, so a config change
	// (the user editing the plugin's own nvrQuotaGB storage value, see
	// plugin.go) takes effect on the next pass without restarting. Reading
	// this is the caller's (plugin.go's) responsibility — retentionGC has no
	// storage dependency of its own for it, only this getter, since the
	// value now lives on the plugin's own instance-level storage rather than
	// any per-camera one (see this file's package doc comment).
	quotaGB func() float64

	// newTicker constructs the tick source StartRetention's background loop
	// reads from. Defaults to newRealTicker in ConfigureRetention; tests in
	// this package override it directly (white-box, same pattern as
	// Recorder's r.runner/r.sleep) to inject a fake ticker they drive by
	// hand.
	newTicker func(time.Duration) ticker

	// afterTick, if set, is called at the end of every ticker-driven
	// RunRetentionOnce invocation, after it returns — a test-only
	// synchronization hook (nil in production) so a test can deterministically
	// wait for one ticker-triggered pass to finish (e.g. by closing a channel
	// from it) instead of sleeping and hoping the background goroutine has
	// gotten far enough. See TestStartRetention_TickTriggersRunRetentionOnce.
	afterTick func()

	// afterImmediate, if set, is called once, right after StartRetention's
	// immediate startup RunRetentionOnce call returns (before the ticker
	// loop is ever entered) — the same test-only synchronization purpose as
	// afterTick, kept as a separate hook so a test can wait specifically for
	// the startup pass without it being confused for (or racing) a
	// tick-driven one. nil in production. See
	// TestStartRetention_RunsImmediatelyAtStartup.
	afterImmediate func()

	// afterOrphanSnapshot, if set, is called once per sweepOrphanFiles call,
	// right after its knownPaths snapshot (segStore.AllPaths()) is taken but
	// before the recordings-tree walk begins — a test-only synchronization
	// hook (nil in production, same convention as afterTick/afterImmediate
	// above) that lets a test deterministically simulate the exact TOCTOU
	// race sweepOrphanFiles' fresh HasPath recheck exists to close: insert a
	// segment row for a candidate file's path from here, after it's already
	// missing from the snapshot, and prove the recheck still protects it.
	// See TestRunRetentionOnce_OrphanSweep_TOCTOU_FreshlyIndexedFileSurvives.
	afterOrphanSnapshot func()

	// logf/warnf report retention GC activity: one summary line per
	// RunRetentionOnce pass (usage vs quota, over/under cap) plus one line
	// per eviction batch (segments/bytes removed, age-retention or
	// disk-cap), and a warning when a scheduled pass returns an error.
	// ConfigureRetention defaults both to the owning RecorderManager's own
	// logf/warnf (manager.go) — method values that re-read m.log fresh on
	// every call, so logging works correctly even though production wiring
	// calls SetLogger *after* ConfigureRetention (see plugin.go). nil (the
	// zero value) silently drops the log line, the same tolerance
	// RecorderManager.logf/warnf already have for a manager with no logger
	// set. Tests in this package override these fields directly — the same
	// white-box pattern already used for newTicker/afterTick — to capture
	// calls instead of writing through a real *sdk.Logger (which has no
	// injectable writer to intercept).
	logf  func(format string, args ...any)
	warnf func(format string, args ...any)

	mu      sync.Mutex
	cancel  func()
	done    chan struct{}
	running bool
}

// ConfigureRetention wires the dependencies RunRetentionOnce/StartRetention
// need to actually delete anything: segStore and eventStore back the
// age/disk-cap GC itself, quotaGB (may be nil, meaning "no instance-wide
// cap — age-based retention only") returns the current instance-wide disk
// cap in gigabytes on every call (see retentionGC.quotaGB's doc comment for
// why it's a getter, not a captured value), and vectors (typically
// db.ClipVectors, db.FaceVectors — both satisfy store.VectorBackend) are
// every vector backend whose row for a deleted event's ID should be removed
// alongside it (VectorBackend.Delete is documented as a no-op, not an
// error, for an id that was never stored, so passing backends that happen
// to hold nothing for a given event is harmless). Passing no vectors at all
// is valid — there is simply nothing to cascade to.
//
// recordingsDir is the resolved recordings base directory (plugin.go's
// p.recordingsDir) sweepOrphanFiles walks "<recordingsDir>/recordings"
// under — passed alongside segStore/eventStore (the other on-disk-truth
// dependencies this GC pass needs) rather than as a trailing option, since
// every real caller has one available at the same point it has the stores.
// "" (production: only when store.Open already failed and this is never
// reached; tests that don't exercise the orphan sweep) disables the sweep
// entirely — see recordingsDir's own doc comment on retentionGC.
//
// Safe to call once, before RunRetentionOnce/StartRetention are ever
// invoked; calling it again replaces the previous configuration (and, if a
// ticker was running under the old one, orphans it — callers should
// StopRetention first if reconfiguring a live manager, though production
// wiring (plugin.go) only ever calls this once at startup).
func (m *RecorderManager) ConfigureRetention(segStore *store.SegmentStore, eventStore *store.EventStore, recordingsDir string, quotaGB func() float64, vectors ...store.VectorBackend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gc = &retentionGC{
		segStore:      segStore,
		eventStore:    eventStore,
		recordingsDir: recordingsDir,
		quotaGB:       quotaGB,
		vectors:       vectors,
		newTicker:     newRealTicker,
		logf:          m.logf,
		warnf:         m.warnf,
	}
}

// logPass reports one retention log line via gc.logf, if set — a thin
// nil-guard so every call site below doesn't have to repeat it. See
// retentionGC.logf's doc comment.
func (gc *retentionGC) logPass(format string, args ...any) {
	if gc.logf != nil {
		gc.logf(format, args...)
	}
}

// warnPass reports one retention warning via gc.warnf, if set — the warning
// counterpart to logPass. See retentionGC.warnf's doc comment.
func (gc *retentionGC) warnPass(format string, args ...any) {
	if gc.warnf != nil {
		gc.warnf(format, args...)
	}
}

// RunRetentionOnce performs a single garbage-collection pass:
//
//  1. Age GC, per camera: for every camera this manager tracks
//     (entriesSnapshot — every registered camera, regardless of current
//     Config.Mode; see its doc comment for why), if RetentionDays > 0,
//     deletes segment rows (+ files) whose end_ms falls before
//     nowMs - RetentionDays*24h, cascading to that camera's fully-ended
//     events (+ thumbnail files/vector rows) older than the same cutoff.
//  2. Disk-cap GC, instance-wide: once every camera's age pass has run, if
//     gc.quotaGB() > 0, computes total on-disk usage across every managed
//     camera and, if it still exceeds that single cap, deletes the oldest
//     remaining segments — regardless of which camera they belong to — (+
//     the same cascade) until it no longer does (enforceInstanceQuota). See
//     this file's package doc comment for why this is instance-wide rather
//     than per-camera.
//
// A no-op returning nil when retention hasn't been configured
// (ConfigureRetention never called) — every pre-Task-9 caller/test that
// never touches retention is unaffected. Every error encountered is
// collected (via errors.Join) rather than aborting the whole pass early, so
// one camera's I/O error (e.g. a permission problem removing one file)
// doesn't prevent every other managed camera's GC from running.
func (m *RecorderManager) RunRetentionOnce(nowMs int64) error {
	m.mu.RLock()
	gc := m.gc
	m.mu.RUnlock()
	if gc == nil {
		return nil
	}

	entries := m.entriesSnapshot()

	var errs []error
	cameraIDs := make([]string, 0, len(entries))
	var ageRemovedSegs int
	var ageRemovedBytes int64
	for _, entry := range entries {
		cameraIDs = append(cameraIDs, entry.CameraID)

		if entry.Config.RetentionDays > 0 {
			cutoffMs := nowMs - int64(entry.Config.RetentionDays)*msPerDay
			n, b, err := gc.deleteSegmentsAndCascadeOlderThan(entry.CameraID, cutoffMs)
			ageRemovedSegs += n
			ageRemovedBytes += b
			if err != nil {
				errs = append(errs, fmt.Errorf("retention: camera %s: age gc: %w", entry.CameraID, err))
			}
		}
	}
	if ageRemovedSegs > 0 {
		gc.logPass("retention: age gc evicted %d segment(s) (%.2f GB) past their camera's retention window", ageRemovedSegs, float64(ageRemovedBytes)/bytesPerGB)
	}

	// Orphan sweep, instance-wide: runs after age GC (whose file removals
	// don't affect it) and before the disk-cap GC below, so bytes an orphan
	// file's removal frees are already reflected in enforceInstanceQuota's
	// own fresh fileSize() reads of whatever's left on disk — an orphan file
	// counts toward "how much are we actually using" the same as an indexed
	// one, so freeing it first, not after, is what makes the quota's own
	// pass see accurate usage. See sweepOrphanFiles' doc comment for what
	// counts as an orphan and why the grace period exists.
	orphanFiles, orphanBytes, orphanErr := gc.sweepOrphanFiles(nowMs, m.ActiveOutputDirs())
	if orphanErr != nil {
		errs = append(errs, fmt.Errorf("retention: orphan sweep: %w", orphanErr))
	}
	if orphanFiles > 0 {
		gc.logPass("retention: orphan sweep removed %d untracked segment file(s) (%.2f GB) with no matching segment row", orphanFiles, float64(orphanBytes)/bytesPerGB)
	}

	if gc.quotaGB != nil {
		if quota := gc.quotaGB(); quota > 0 {
			if err := gc.enforceInstanceQuota(cameraIDs, quota); err != nil {
				errs = append(errs, fmt.Errorf("retention: instance quota gc: %w", err))
			}
		}
	}

	return errors.Join(errs...)
}

// StartRetention begins a background ticker that calls RunRetentionOnce
// every interval (defaultRetentionInterval if interval <= 0), sourcing each
// call's nowMs from clockNow (time.Now().UnixMilli if clockNow is nil).
//
// Production bug fix: before ever waiting on the first tick, the background
// goroutine now runs one RunRetentionOnce pass immediately. Previously GC
// only ran on a tick, so quota enforcement lagged a full interval behind
// every launch/restart — and under frequent restarts (each one resetting
// the timer) could go long stretches without running at all, letting a
// configured nvrQuotaGB cap drift far over. The immediate pass still
// respects a Stop() that races it: it's guarded by the same stop-channel
// check the tick loop uses, so a Stop() called right after Start either
// skips it entirely or lets it run to completion and then exits cleanly —
// never blocking StopRetention indefinitely either way.
//
// Returns immediately; the ticker runs in its own goroutine until
// StopRetention is called. A no-op (nil error, nothing started) when
// retention hasn't been configured (ConfigureRetention) or a ticker is
// already running — calling Start twice without an intervening Stop doesn't
// spawn a second goroutine.
func (m *RecorderManager) StartRetention(interval time.Duration, clockNow func() int64) error {
	m.mu.RLock()
	gc := m.gc
	m.mu.RUnlock()
	if gc == nil {
		return nil
	}

	gc.mu.Lock()
	defer gc.mu.Unlock()
	if gc.running {
		return nil
	}

	if interval <= 0 {
		interval = defaultRetentionInterval
	}
	if clockNow == nil {
		clockNow = func() int64 { return time.Now().UnixMilli() }
	}
	newTicker := gc.newTicker
	if newTicker == nil {
		newTicker = newRealTicker
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	gc.cancel = func() { close(stop) }
	gc.done = done
	gc.running = true

	go func() {
		defer close(done)

		// t is created (and its Stop deferred) before the immediate pass
		// below, unconditionally — so StopRetention's expectation that the
		// ticker is always Stop()ped before the goroutine exits holds
		// regardless of whether the immediate pass below actually ran.
		t := newTicker(interval)
		defer t.Stop()

		select {
		case <-stop:
			return
		default:
			if err := m.RunRetentionOnce(clockNow()); err != nil {
				gc.warnPass("retention: immediate startup gc pass failed: %v", err)
			}
			if gc.afterImmediate != nil {
				gc.afterImmediate()
			}
		}

		for {
			select {
			case <-stop:
				return
			case <-t.C():
				if err := m.RunRetentionOnce(clockNow()); err != nil {
					gc.warnPass("retention: scheduled gc pass failed: %v", err)
				}
				if gc.afterTick != nil {
					gc.afterTick()
				}
			}
		}
	}()
	return nil
}

// StopRetention cancels the background ticker started by StartRetention and
// blocks until its goroutine has fully exited — the same cancel-then-Wait
// shape Recorder.Stop (recorder.go) uses, which is what makes this leak-safe:
// StopRetention cannot return while the ticker goroutine is still running,
// so a caller that calls it (e.g. on plugin shutdown) never leaves a
// goroutine behind. Idempotent: calling Stop when not running (including on
// a manager where ConfigureRetention was never called) is a no-op.
func (m *RecorderManager) StopRetention() {
	m.mu.RLock()
	gc := m.gc
	m.mu.RUnlock()
	if gc == nil {
		return
	}

	gc.mu.Lock()
	if !gc.running {
		gc.mu.Unlock()
		return
	}
	cancel := gc.cancel
	done := gc.done
	gc.mu.Unlock()

	cancel()
	<-done

	gc.mu.Lock()
	gc.running = false
	gc.cancel = nil
	gc.done = nil
	gc.mu.Unlock()
}

// deleteSegmentsAndCascadeOlderThan is the shared core both ageGC's cutoff
// and enforceQuota's computed disk-cap boundary funnel through: delete
// cameraID's segment rows ending before cutoffMs (SegmentStore.
// DeleteOlderThan — rows only), remove the files at the paths it returns,
// then do the same for cameraID's fully-ended events older than the same
// cutoff (EventStore.DeleteOlderThan) and cascade each removed event to its
// thumbnail file and vector rows (cascadeDeletedEvents). Segment/event row
// deletion always happens before file/cascade removal is attempted — like
// event_mode.go's sweepEventSpool, a file-removal error is reported (via the
// returned error) but never rolls back the already-committed row deletion,
// so a stubborn file (e.g. a permission error) doesn't leave a
// still-referenced-by-nothing row stuck in the store forever.
//
// Returns the number of segments removed and their total on-disk size in
// bytes (measured before removal), so callers (RunRetentionOnce's age loop,
// enforceInstanceQuota) can report one eviction-batch summary line instead
// of logging per file.
func (gc *retentionGC) deleteSegmentsAndCascadeOlderThan(cameraID string, cutoffMs int64) (removedSegments int, removedBytes int64, err error) {
	var errs []error

	if gc.segStore != nil {
		paths, dErr := gc.segStore.DeleteOlderThan(cameraID, cutoffMs)
		if dErr != nil {
			errs = append(errs, fmt.Errorf("delete segments: %w", dErr))
		} else {
			removedSegments = len(paths)
			for _, p := range paths {
				removedBytes += fileSize(p)
			}
			errs = append(errs, removeFiles(paths)...)
		}
	}

	if gc.eventStore != nil {
		deleted, dErr := gc.eventStore.DeleteOlderThan(cameraID, cutoffMs)
		if dErr != nil {
			errs = append(errs, fmt.Errorf("delete events: %w", dErr))
		} else {
			errs = append(errs, gc.cascadeDeletedEvents(deleted)...)
		}
	}

	return removedSegments, removedBytes, errors.Join(errs...)
}

// cascadeDeletedEvents removes each deleted event's thumbnail file (if any —
// see store.DeletedEvent's doc comment on why ThumbRef is "" for every row
// today) and its row in every configured vector backend (gc.vectors),
// keyed by event ID. Collects and returns every error encountered rather
// than stopping at the first, so one event's stubborn thumbnail file doesn't
// prevent every other deleted event in the same batch from being cascaded.
func (gc *retentionGC) cascadeDeletedEvents(events []store.DeletedEvent) []error {
	var errs []error
	for _, ev := range events {
		if ev.ThumbRef != "" {
			if err := removeFile(ev.ThumbRef); err != nil {
				errs = append(errs, fmt.Errorf("remove thumbnail for event %s: %w", ev.ID, err))
			}
		}
		for _, vb := range gc.vectors {
			if err := vb.Delete(ev.ID); err != nil {
				errs = append(errs, fmt.Errorf("delete vector for event %s: %w", ev.ID, err))
			}
		}
	}
	return errs
}

// cameraSegment pairs a segment with the camera it belongs to, so
// enforceInstanceQuota can merge every managed camera's segments into one
// list and sort it oldest-first across the whole instance, rather than per
// camera.
type cameraSegment struct {
	cameraID string
	seg      store.Segment
}

// enforceInstanceQuota lists every segment across every camera in
// cameraIDs (SegmentStore.AllByCamera, once per camera), merges them into a
// single oldest-first (by StartMs) list spanning the whole instance, sums
// their on-disk size (fileSize — 0 for an already-missing file, the same
// missing-file tolerance removeFile below applies on the delete side), and
// — only if that total exceeds quotaGB converted to bytes — walks the
// merged oldest-first list accumulating how much would be freed by deleting
// each one in turn, until the running total drops back under the single
// instance-wide cap. This is deliberately global, not per-camera: see this
// file's package doc comment for why (StorageStats' nvrQuotaGB is one
// instance-wide value, not one per camera).
//
// Each segment walked contributes to that owning camera's own cutoff (the
// highest EndMs removed for that camera) — a plain map, since different
// cameras' segments are interleaved in the merged oldest-first walk and
// each one's actual row/file/cascade deletion is still necessarily
// per-camera (SegmentStore.DeleteOlderThan and EventStore.DeleteOlderThan
// are both scoped to one cameraID). Once the walk is done, every camera
// that had at least one segment removed gets exactly one
// deleteSegmentsAndCascadeOlderThan(cameraID, cutoff+1) call — reusing the
// exact same deletion path the age GC uses, so the disk-cap sweep gets the
// identical cascade behavior for free. cutoff+1 (not cutoff) so the
// boundary segment itself — whose EndMs == cutoff — is included
// (DeleteOlderThan's predicate is end_ms < cutoffMs, strict).
func (gc *retentionGC) enforceInstanceQuota(cameraIDs []string, quotaGB float64) error {
	if gc.segStore == nil {
		return nil
	}

	var all []cameraSegment
	for _, camID := range cameraIDs {
		segs, err := gc.segStore.AllByCamera(camID)
		if err != nil {
			return fmt.Errorf("list segments for camera %s: %w", camID, err)
		}
		for _, seg := range segs {
			all = append(all, cameraSegment{cameraID: camID, seg: seg})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seg.StartMs < all[j].seg.StartMs })

	quotaBytes := int64(quotaGB * bytesPerGB)
	sizes := make([]int64, len(all))
	var total int64
	for i, cs := range all {
		sizes[i] = fileSize(cs.seg.Path)
		total += sizes[i]
	}
	usageGB := float64(total) / bytesPerGB
	if total <= quotaBytes {
		gc.logPass("retention: disk-cap gc pass: usage %.2f GB / quota %.2f GB (under cap)", usageGB, quotaGB)
		return nil
	}
	gc.logPass("retention: disk-cap gc pass: usage %.2f GB / quota %.2f GB (OVER cap, evicting oldest segments)", usageGB, quotaGB)

	cutoffs := make(map[string]int64)
	for i, cs := range all {
		if total <= quotaBytes {
			break
		}
		total -= sizes[i]
		if cs.seg.EndMs > cutoffs[cs.cameraID] {
			cutoffs[cs.cameraID] = cs.seg.EndMs
		}
	}

	var errs []error
	var evictedSegs int
	var evictedBytes int64
	for camID, cutoffMs := range cutoffs {
		n, b, err := gc.deleteSegmentsAndCascadeOlderThan(camID, cutoffMs+1)
		evictedSegs += n
		evictedBytes += b
		if err != nil {
			errs = append(errs, fmt.Errorf("camera %s: %w", camID, err))
		}
	}
	if evictedSegs > 0 {
		gc.logPass("retention: disk-cap gc evicted %d segment(s) (%.2f GB) across %d camera(s) to reclaim quota", evictedSegs, float64(evictedBytes)/bytesPerGB, len(cutoffs))
	}
	return errors.Join(errs...)
}

// sweepOrphanFiles walks "<recordingsDir>/recordings" (recordingsDir being
// gc.recordingsDir — see its doc comment) and deletes every *.mp4 file
// under it that has NO row at all in gc.segStore's segments table AND whose
// mtime is older than orphanGrace relative to nowMs — with two additional
// safety checks (both added after a review caught this method's first cut
// as an unsafe, real-data-loss race) before any actual deletion:
//
//   - activeOutputDirs (RunRetentionOnce passes RecorderManager.
//     ActiveOutputDirs(), computed fresh on every call) lists every directory
//     a currently-running Recorder is writing segments into RIGHT NOW. A
//     candidate file inside one of these is skipped unconditionally,
//     regardless of mtime: this is what protects a stalled-but-still-open
//     segment (e.g. an RTSP source hang lasting longer than orphanGrace,
//     while ffmpeg still holds the file's fd open and simply isn't writing
//     new bytes to it) from being misclassified as orphaned by the
//     mtime>grace heuristic alone.
//   - gc.segStore.HasPath is re-queried, fresh, IMMEDIATELY before deleting
//     any file that survives every check above — never decided from the
//     knownPaths snapshot taken at the top of this method. This closes a
//     TOCTOU window a review found in this method's first cut: knownPaths is
//     a point-in-time snapshot, but the walk that follows it can take long
//     enough (a large recordings tree) that a segment can be finalized and
//     indexed — by a recorder that crashed and was then restarted, the exact
//     scenario this sweep exists to clean up after — AFTER the snapshot was
//     taken but BEFORE the walk visits its file, while that file's mtime is
//     already old enough to clear orphanGrace (it was written before the
//     crash). Without this recheck, such a file would be deleted despite
//     having a perfectly valid segment row by the time this method actually
//     unlinks it — real, indexed footage lost. knownPaths remains a cheap
//     first filter (skips the common case — most files ARE indexed — without
//     a DB round-trip per file), but it is never, by itself, what decides a
//     file is safe to delete.
//
// This is the Task 9 review bug fix: enforceInstanceQuota (and the age GC
// above) only ever counts/evicts segments that ARE indexed — a recorder
// that crashes or is restarted mid-segment can leave a finished (or
// partially-written) .mp4 file on disk that never got finalized/indexed
// into SegmentStore at all, so it's invisible to both the disk-cap
// computation (actual disk usage silently exceeds the tracked/enforced
// total) and every GC path that deletes by segment row. This sweep is the
// only place such a file is ever found and removed.
//
// The grace period exists so a file that's simply mid-write right now, or
// finished writing moments ago but hasn't been finalized/indexed yet (an
// entirely normal, momentary state for the newest segment — see
// recorder.go's watchSegments "skip the newest file" doc comment for the
// same class of lag elsewhere in this plugin), is never deleted out from
// under the recorder still writing it or about to index it. Only a file
// unindexed at the fresh recheck, older than the grace window, AND outside
// every currently-active output directory is treated as a genuine orphan.
//
// Returns the count and total pre-removal size (bytes) of files actually
// removed, for RunRetentionOnce's summary log line. A missing/unreadable
// recordings directory, or gc.recordingsDir/gc.segStore left unset (""/nil
// — see their own doc comments), is not an error: there is simply nothing
// to sweep. Every other per-file error (a failed stat, a failed HasPath
// recheck, a failed removal) is collected and returned via errors.Join
// rather than aborting the walk, so one stubborn file doesn't stop every
// other orphan in the same pass from being reclaimed — but, critically, any
// such error skips (never deletes) the file it occurred on: an error from
// the fresh HasPath recheck must never be treated as "so assume it's still
// an orphan".
func (gc *retentionGC) sweepOrphanFiles(nowMs int64, activeOutputDirs []string) (removedFiles int, removedBytes int64, err error) {
	if gc.recordingsDir == "" || gc.segStore == nil {
		return 0, 0, nil
	}

	base := filepath.Join(gc.recordingsDir, "recordings")
	if _, statErr := os.Stat(base); statErr != nil {
		return 0, 0, nil
	}

	knownPaths, listErr := gc.segStore.AllPaths()
	if listErr != nil {
		return 0, 0, fmt.Errorf("list known segment paths: %w", listErr)
	}
	known := make(map[string]struct{}, len(knownPaths))
	for _, p := range knownPaths {
		known[normalizeSegmentPath(p)] = struct{}{}
	}

	// afterOrphanSnapshot, if set (test-only, nil in production — the same
	// synchronization-hook convention as afterTick/afterImmediate above), is
	// called once here, right after the knownPaths snapshot above but before
	// the walk begins — letting a test deterministically simulate the exact
	// TOCTOU race this method's fresh HasPath recheck (below) closes: insert
	// a segment row for a candidate file's path AFTER it's already missing
	// from knownPaths, and prove the recheck still catches it.
	if gc.afterOrphanSnapshot != nil {
		gc.afterOrphanSnapshot()
	}

	activeDirs := make(map[string]struct{}, len(activeOutputDirs))
	for _, d := range activeOutputDirs {
		activeDirs[normalizeSegmentPath(d)] = struct{}{}
	}

	cutoff := time.UnixMilli(nowMs).Add(-orphanGrace)

	var errs []error
	walkErr := filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A permission error (or similar) on one entry shouldn't abort
			// sweeping the rest of the tree — recorded and skipped, same
			// tolerance as every other per-file error below.
			errs = append(errs, walkErr)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".mp4") {
			return nil
		}
		if _, ok := known[normalizeSegmentPath(path)]; ok {
			return nil
		}
		if _, ok := activeDirs[normalizeSegmentPath(filepath.Dir(path))]; ok {
			// A currently-running recorder owns this directory right now —
			// never touch anything in it, regardless of mtime (see this
			// method's own doc comment on the stalled-open-segment case this
			// protects against).
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			errs = append(errs, infoErr)
			return nil
		}
		if info.ModTime().After(cutoff) {
			// Too recent to be confidently orphaned — may still be being
			// written, or finished but not yet finalized/indexed. Leave it
			// for a later pass.
			return nil
		}

		// Fresh, point-in-time recheck — the TOCTOU fix. Never decide from
		// the knownPaths snapshot above; a segment can have been finalized
		// and indexed after that snapshot was taken but before the walk
		// reached this file. An error here means "unknown whether it's still
		// indexed" and must be treated exactly like "yes, it's indexed" —
		// skip, don't delete.
		indexed, hpErr := gc.segStore.HasPath(path)
		if hpErr != nil {
			errs = append(errs, fmt.Errorf("recheck segment path %s: %w", path, hpErr))
			return nil
		}
		if indexed {
			return nil
		}

		size := info.Size()
		if rmErr := removeFile(path); rmErr != nil {
			errs = append(errs, rmErr)
			return nil
		}
		removedFiles++
		removedBytes += size
		return nil
	})
	if walkErr != nil {
		errs = append(errs, fmt.Errorf("walk recordings dir: %w", walkErr))
	}

	return removedFiles, removedBytes, errors.Join(errs...)
}

// normalizeSegmentPath resolves path to an absolute path for
// sweepOrphanFiles' known-paths comparison, so a segments-table row stored
// with (e.g.) a relative path still matches the absolute path
// filepath.WalkDir hands the walk callback. Falls back to path unchanged if
// it can't be resolved (e.g. an empty string) rather than erroring — worst
// case, that row simply fails to suppress a match against a real, still-
// indexed segment, which would surface as a spurious deletion to revisit if
// ever observed, not a panic or an aborted sweep.
func normalizeSegmentPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// fileSize returns path's size in bytes, or 0 if it can't be stat'd
// (already missing, or any other error) — enforceQuota treats a missing
// segment file the same way removeFile's IsNotExist tolerance does: it
// simply contributes nothing to the usage total rather than aborting the
// whole quota computation.
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// removeFiles calls removeFile for every path and collects every non-nil
// error it returns (missing-file tolerance is removeFile's own concern, so
// this never reports anything for a path that was already gone).
func removeFiles(paths []string) []error {
	var errs []error
	for _, p := range paths {
		if err := removeFile(p); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// removeFile deletes path, treating "already gone" as success rather than
// an error — the same tolerance event_mode.go's sweepEventSpool already
// applies (os.IsNotExist(err)), needed here because a retention pass racing
// something else that already cleaned up the same path (or simply retrying
// after a partially-failed previous pass) must not fail the whole GC run
// over a file that's already exactly as absent as this pass wants it to be.
// An empty path (never expected from SegmentStore/EventStore, but not worth
// a panic over) is treated as nothing to remove.
func removeFile(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
