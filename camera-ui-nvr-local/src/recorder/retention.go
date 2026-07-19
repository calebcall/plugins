// retention.go implements Task 9's retention garbage collection: a
// background housekeeping pass, run periodically across every camera this
// RecorderManager manages, that deletes recorded segments (and their files)
// once they fall outside the camera's configured retention window — by age
// (RetentionDays) and/or an optional disk cap (NvrQuotaGB, oldest-first) —
// cascading to the events/thumbnails/vector rows tied to whatever was
// removed.
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
	"os"
	"sync"
	"time"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// msPerDay converts RecordingConfig.RetentionDays into milliseconds for the
// age cutoff computed in ageGC.
const msPerDay = 24 * 60 * 60 * 1000

// bytesPerGB converts RecordingConfig.NvrQuotaGB into bytes for enforceQuota.
// Decimal (1e9), matching how disk quotas are conventionally advertised
// (GB, not GiB) — this task has no other convention to match, and the exact
// boundary doesn't matter for correctness (the cap is enforced by comparing
// this same constant on both the "are we over" check and the "how much do
// we need to free" loop).
const bytesPerGB = 1_000_000_000

// defaultRetentionInterval is the ticker period StartRetention uses when
// called with interval <= 0. An hour is frequent enough that a camera's
// disk usage or age-eligible footage never drifts far past its configured
// limit, without re-scanning every camera's full segment/event set needlessly
// often.
const defaultRetentionInterval = time.Hour

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

	mu      sync.Mutex
	cancel  func()
	done    chan struct{}
	running bool
}

// ConfigureRetention wires the SQLite-backed stores RunRetentionOnce/
// StartRetention need to actually delete anything: segStore and eventStore
// back the age/disk-cap GC itself, and vectors (typically db.ClipVectors,
// db.FaceVectors — both satisfy store.VectorBackend) are every vector
// backend whose row for a deleted event's ID should be removed alongside it
// (VectorBackend.Delete is documented as a no-op, not an error, for an id
// that was never stored, so passing backends that happen to hold nothing
// for a given event is harmless). Passing no vectors at all is valid — there
// is simply nothing to cascade to.
//
// Safe to call once, before RunRetentionOnce/StartRetention are ever
// invoked; calling it again replaces the previous configuration (and, if a
// ticker was running under the old one, orphans it — callers should
// StopRetention first if reconfiguring a live manager, though production
// wiring (plugin.go) only ever calls this once at startup).
func (m *RecorderManager) ConfigureRetention(segStore *store.SegmentStore, eventStore *store.EventStore, vectors ...store.VectorBackend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gc = &retentionGC{
		segStore:   segStore,
		eventStore: eventStore,
		vectors:    vectors,
		newTicker:  newRealTicker,
	}
}

// RunRetentionOnce performs a single garbage-collection pass across every
// camera this manager tracks (entriesSnapshot — every registered camera,
// regardless of current Config.Mode; see its doc comment for why). For each
// one: if RetentionDays > 0, deletes segment rows (+ files) whose end_ms
// falls before nowMs - RetentionDays*24h, cascading to that camera's
// fully-ended events (+ thumbnail files/vector rows) older than the same
// cutoff (ageGC). Then, if NvrQuotaGB > 0, and only if the camera's current
// on-disk usage still exceeds that cap after the age pass, deletes the
// oldest remaining segments (+ the same cascade) until it no longer does
// (enforceQuota).
//
// A no-op returning nil when retention hasn't been configured
// (ConfigureRetention never called) — every pre-Task-9 caller/test that
// never touches retention is unaffected. Every per-camera error
// encountered is collected (via errors.Join) rather than aborting the whole
// pass early, so one camera's I/O error (e.g. a permission problem removing
// one file) doesn't prevent every other managed camera's GC from running.
func (m *RecorderManager) RunRetentionOnce(nowMs int64) error {
	m.mu.RLock()
	gc := m.gc
	m.mu.RUnlock()
	if gc == nil {
		return nil
	}

	var errs []error
	for _, entry := range m.entriesSnapshot() {
		cfg := entry.Config

		if cfg.RetentionDays > 0 {
			cutoffMs := nowMs - int64(cfg.RetentionDays)*msPerDay
			if err := gc.deleteSegmentsAndCascadeOlderThan(entry.CameraID, cutoffMs); err != nil {
				errs = append(errs, fmt.Errorf("retention: camera %s: age gc: %w", entry.CameraID, err))
			}
		}

		if cfg.NvrQuotaGB > 0 {
			if err := gc.enforceQuota(entry.CameraID, cfg.NvrQuotaGB); err != nil {
				errs = append(errs, fmt.Errorf("retention: camera %s: quota gc: %w", entry.CameraID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// StartRetention begins a background ticker that calls RunRetentionOnce
// every interval (defaultRetentionInterval if interval <= 0), sourcing each
// call's nowMs from clockNow (time.Now().UnixMilli if clockNow is nil).
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
		t := newTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C():
				_ = m.RunRetentionOnce(clockNow())
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
func (gc *retentionGC) deleteSegmentsAndCascadeOlderThan(cameraID string, cutoffMs int64) error {
	var errs []error

	if gc.segStore != nil {
		paths, err := gc.segStore.DeleteOlderThan(cameraID, cutoffMs)
		if err != nil {
			errs = append(errs, fmt.Errorf("delete segments: %w", err))
		} else {
			errs = append(errs, removeFiles(paths)...)
		}
	}

	if gc.eventStore != nil {
		deleted, err := gc.eventStore.DeleteOlderThan(cameraID, cutoffMs)
		if err != nil {
			errs = append(errs, fmt.Errorf("delete events: %w", err))
		} else {
			errs = append(errs, gc.cascadeDeletedEvents(deleted)...)
		}
	}

	return errors.Join(errs...)
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

// enforceQuota lists cameraID's segments oldest-first (SegmentStore.
// AllByCamera), sums their on-disk size (fileSize — 0 for an already-missing
// file, the same missing-file tolerance removeFile below applies on the
// delete side), and — only if that total exceeds quotaGB converted to bytes
// — walks the oldest-first list accumulating how much would be freed by
// deleting each one in turn, until the running total drops back under the
// cap. The EndMs of the last segment that decision includes becomes a
// cutoff, and the actual deletion (rows, files, and the event/thumbnail/
// vector cascade) is performed by a single
// deleteSegmentsAndCascadeOlderThan(cameraID, cutoff+1) call — reusing
// exactly the same deletion path ageGC uses, rather than deleting segments
// one at a time, so the disk-cap sweep gets the identical cascade behavior
// for free. cutoff+1 (not cutoff) so the boundary segment itself — whose
// EndMs == cutoff — is included (DeleteOlderThan's predicate is end_ms <
// cutoffMs, strict).
func (gc *retentionGC) enforceQuota(cameraID string, quotaGB float64) error {
	if gc.segStore == nil {
		return nil
	}

	segs, err := gc.segStore.AllByCamera(cameraID)
	if err != nil {
		return fmt.Errorf("list segments for quota: %w", err)
	}

	quotaBytes := int64(quotaGB * bytesPerGB)
	sizes := make([]int64, len(segs))
	var total int64
	for i, seg := range segs {
		sizes[i] = fileSize(seg.Path)
		total += sizes[i]
	}
	if total <= quotaBytes {
		return nil
	}

	var cutoffMs int64 = -1
	for i, seg := range segs {
		if total <= quotaBytes {
			break
		}
		total -= sizes[i]
		if seg.EndMs > cutoffMs {
			cutoffMs = seg.EndMs
		}
	}
	if cutoffMs < 0 {
		// Every segment is already accounted for (quotaBytes itself was
		// negative, or segs was empty) — nothing to delete.
		return nil
	}

	return gc.deleteSegmentsAndCascadeOlderThan(cameraID, cutoffMs+1)
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
