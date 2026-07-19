// event_mode.go implements Task 8's events recording mode: MarkEvent, the
// promotion side (a detection event's protected window promotes every
// finalized segment overlapping it out of the events-mode "spool" into
// permanent retention), and sweepEventSpool, the discard side (a periodic,
// event-aware janitor that deletes spool segments nothing protects, once
// they're old enough that a fresh event's pre-roll can no longer need
// them).
//
// # Design (revised after review — see the three bugs below)
//
// The original version of this file called MarkEvent(startMs, endMs) once,
// synchronously, per DetectionEvent lifecycle message, computing a single
// fixed [start-preRoll, end+postRoll] window from whatever
// StartTime/EndTime that one message carried. Review found this
// structurally wrong:
//
//   - CRITICAL 1: sdk.DetectionEvent.EndTime is 0 (omitempty) until the
//     event's terminal message. Every non-terminal call therefore computed
//     endMs=0, i.e. a window ending at time 0 — a no-op in practice. The
//     one call that DID carry a real EndTime (the terminal message) ran
//     immediately, before the post-roll segments in [endMs, endMs+postRoll]
//     had even been recorded yet (they appear over the following
//     PostRollS seconds) — so post-roll was never actually retained.
//   - CRITICAL 2: the janitor's cutoff was `now - preRoll`, evaluated on
//     every tick regardless of whether an event was in flight. A pre-roll
//     segment is, by construction, already older than preRoll relative to
//     "now" for as long as the event that needs it hasn't ended yet (that's
//     the whole point of pre-roll) — so it could be swept before the one
//     effective (terminal) MarkEvent call ever ran.
//   - IMPORTANT (TOCTOU): promotion was InRange-then-MarkReferenced, two
//     independently-locked SegmentStore round-trips; a concurrent janitor
//     sweep could delete a borderline segment in the gap between them.
//
// The fix, below, replaces the one-shot call with three cooperating pieces:
//
//  1. eventWindowSet/eventWindow: a per-event protected window, keyed by
//     event ID, updated on EVERY lifecycle message (MarkEvent), not just
//     the terminal one. While the event is active (EndTime==0 so far), the
//     window's end keeps rolling forward to "now" every time it's touched —
//     see eventWindow.window — so it always covers "everything recorded so
//     far, plus"; once a message reports EndTime>0, the window freezes at
//     endTime+postRoll permanently (eventWindow.isOpen).
//  2. Promotion is continuous, from two call sites, not one: MarkEvent
//     itself (promoteOpenWindows, covering segments already indexed when a
//     window is added/updated — this is what protects pre-roll, which by
//     definition was recorded before the event even started) and
//     recorder.go's sweepSegments, via promoteIfCovered, called for every
//     segment right after it's finalized (this is what protects post-roll,
//     recorded after the terminal message already ran). sweepEventSpool
//     also re-runs a promotion pass every tick as a third, defense-in-depth
//     path.
//  3. sweepEventSpool is now event-aware: before deleting anything, it
//     computes the current set of open protected windows and never deletes
//     a segment overlapping any of them, regardless of the segment's own
//     age. Only a segment protected by no open window AND older than
//     PreRollS is a deletion candidate.
//  4. Every one of the above (promoteOpenWindows, promoteIfCovered, and
//     sweepEventSpool's own decide-then-delete) runs under the same
//     r.retentionMu, so a promotion and a sweep can never interleave —
//     closing the TOCTOU by construction rather than by narrowing the
//     window further.
//  5. r.nowFn (an injected clock, see recorder.go) replaces every direct
//     time.Now() call in this file, so tests can drive "now" deterministically.
package recorder

import (
	"os"
	"sync"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// eventWindow is one detection event's protected retention window, tracked
// for as long as it might still need pre/post-roll segments protected from
// sweepEventSpool. startMs is the event's own StartTime, never itself
// expanded by pre-roll — window/isOpen below add PreRollS/PostRollS (in ms)
// on top when computing an actual range, since those are per-recorder
// config, not per-event state.
type eventWindow struct {
	startMs int64
	// rawEnd is the event's own EndTime as most recently reported by any
	// lifecycle message; 0 means the event is still active
	// (sdk.DetectionEvent.EndTime's omitempty zero value — see this file's
	// package doc for why that fact is exactly what broke the original
	// design). Frozen, permanently, the first time a message reports
	// EndTime > 0.
	rawEnd int64
}

// window returns this event's current promotion/protection range, given now
// (ms) and the recorder's configured pre/post-roll (ms, already multiplied
// up from the RecorderConfig seconds fields by the caller). While the event
// is still active (rawEnd == 0), the range's end rolls forward to now — an
// ongoing event's eventual post-roll can't be known yet, so treating the
// window as extending "at least to now" is what keeps every segment
// recorded during the event protected without needing to guess when it
// will end. Once a terminal message has set rawEnd, the range's end is
// fixed at rawEnd+postRollMs forever after.
func (w *eventWindow) window(now, preRollMs, postRollMs int64) (start, end int64) {
	start = w.startMs - preRollMs
	effectiveEnd := w.rawEnd
	if effectiveEnd == 0 {
		effectiveEnd = now
	}
	end = effectiveEnd + postRollMs
	return start, end
}

// isOpen reports whether this window must still be treated as protected at
// now: unconditionally true while the event is active (rawEnd == 0, so its
// eventual end+postRoll can't yet be known to have passed), otherwise true
// only until now reaches the frozen rawEnd+postRollMs.
func (w *eventWindow) isOpen(now, postRollMs int64) bool {
	if w.rawEnd == 0 {
		return true
	}
	return now < w.rawEnd+postRollMs
}

// windowRange is one eventWindow's range as of a particular now, i.e. the
// result of calling window/isOpen on it once, bundled together so callers
// that took a snapshot (eventWindowSet.snapshot) don't recompute "now" or
// re-read the source eventWindow (which could have been concurrently
// upserted again by then) inconsistently between the two.
type windowRange struct {
	eventID    string
	start, end int64
	open       bool
}

// overlaps reports whether seg overlaps this range, using the same
// inclusive-both-ends test SegmentStore.InRange itself uses (end_ms >=
// start AND start_ms <= end), so "protected by this window" and "would
// have been found by InRange(start, end)" always agree.
func (w windowRange) overlaps(seg store.Segment) bool {
	return seg.EndMs >= w.start && seg.StartMs <= w.end
}

// eventWindowSet tracks every currently-relevant detection event's
// eventWindow, keyed by event ID, guarded by its own mutex. A Recorder in
// RecordingModeEvents holds exactly one (see Recorder.events); every method
// on it is safe for concurrent use, but see Recorder.retentionMu for why
// the *sequence* of "snapshot, promote, maybe retire, maybe delete" a
// caller builds on top of these methods needs a coarser lock than this
// struct's own — this mutex alone only ever protects one call at a time,
// not a caller's multi-step decision built from several calls.
type eventWindowSet struct {
	mu   sync.Mutex
	byID map[string]*eventWindow
}

// upsert records a DetectionEvent lifecycle message for eventID: creates
// the window on first sight (using startMs), and — whenever endMs > 0 (a
// terminal message) — freezes its rawEnd. A non-terminal message (endMs ==
// 0) for an already-known event is intentionally a no-op beyond that:
// there's nothing new to record, since window() already rolls the range
// forward to "now" on every read for as long as rawEnd stays 0.
func (s *eventWindowSet) upsert(eventID string, startMs, endMs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.byID[eventID]
	if !ok {
		if s.byID == nil {
			s.byID = make(map[string]*eventWindow)
		}
		w = &eventWindow{startMs: startMs}
		s.byID[eventID] = w
	}
	if endMs > 0 {
		w.rawEnd = endMs
	}
}

// retire forgets eventID's window. Called only once its range's end has
// fully elapsed (see windowRange.open) — see promoteOpenWindowsLocked.
func (s *eventWindowSet) retire(eventID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, eventID)
}

// snapshot returns every currently-tracked window's range as of now, each
// flagged with whether it's still open. Taking this snapshot under one lock
// acquisition (rather than the caller iterating live *eventWindow pointers)
// is what lets promoteOpenWindowsLocked below safely decide, from a single
// consistent view, both what to promote and what to retire.
func (s *eventWindowSet) snapshot(now, preRollMs, postRollMs int64) []windowRange {
	s.mu.Lock()
	defer s.mu.Unlock()

	ranges := make([]windowRange, 0, len(s.byID))
	for id, w := range s.byID {
		start, end := w.window(now, preRollMs, postRollMs)
		ranges = append(ranges, windowRange{
			eventID: id,
			start:   start,
			end:     end,
			open:    w.isOpen(now, postRollMs),
		})
	}
	return ranges
}

// MarkEvent records a DetectionEvent lifecycle message for eventID
// (startMs/endMs are the event's own StartTime/EndTime — endMs == 0 for
// every message before the terminal one, per sdk.DetectionEvent's
// omitempty) and immediately runs a promotion pass over every currently
// open protected window. This is what retains pre-roll (already indexed
// before the event, and before this window existed, so nothing else would
// ever re-scan for it) and any segment finalized so far during an active or
// just-ended event; a segment finalized AFTER this call — the post-roll
// case that broke the original design — is instead caught by
// promoteIfCovered, called from recorder.go's sweepSegments for every
// newly finalized segment for as long as this window stays open.
//
// No-op outside RecordingModeEvents (matching sweepEventSpool/
// promoteIfCovered) and when r.segStore is nil (a partially-constructed
// Recorder, so a test doesn't have to special-case it).
func (r *Recorder) MarkEvent(eventID string, startMs, endMs int64) {
	if r.cfg.Mode != RecordingModeEvents || r.segStore == nil {
		return
	}

	r.events.upsert(eventID, startMs, endMs)

	r.retentionMu.Lock()
	defer r.retentionMu.Unlock()
	r.promoteOpenWindowsLocked()
}

// promoteIfCovered immediately promotes seg if it's an events-mode spool
// segment (seg.Referenced == false) that overlaps any currently open
// protected window. Called from recorder.go's sweepSegments right after a
// segment is finalized — this is the mechanism that actually fixes
// Critical 1: a post-roll segment doesn't exist at MarkEvent's own
// promotion-pass time, so it must instead promote itself (via this hook) as
// soon as it's indexed, for as long as the event's window that covers it is
// still open.
func (r *Recorder) promoteIfCovered(seg store.Segment) {
	if r.cfg.Mode != RecordingModeEvents || r.segStore == nil || seg.Referenced {
		return
	}

	r.retentionMu.Lock()
	defer r.retentionMu.Unlock()

	now := r.nowFn()
	preRollMs := int64(r.cfg.PreRollS) * 1000
	postRollMs := r.postRollWindowMs()

	for _, w := range r.events.snapshot(now, preRollMs, postRollMs) {
		if !w.open {
			continue
		}
		if w.overlaps(seg) {
			if err := r.segStore.MarkReferenced([]int64{seg.ID}); err != nil {
				r.logf("recorder: %s/%s: promote segment %d: %v", r.cfg.CameraID, seg.Role, seg.ID, err)
			}
			return
		}
	}
}

// promoteOpenWindowsLocked runs one promotion pass over every tracked
// window (querying r.segStore for each window's current range and
// MarkReferenced-ing everything found), retires any window whose range has
// fully elapsed (see windowRange.open) — giving it this one last promotion
// pass with its final, frozen range first — and returns the ranges that
// remain open afterward, for sweepEventSpool's own use in the same
// retentionMu-locked sequence (avoiding a second, redundant snapshot/lock
// round-trip). Callers must hold r.retentionMu.
func (r *Recorder) promoteOpenWindowsLocked() []windowRange {
	now := r.nowFn()
	preRollMs := int64(r.cfg.PreRollS) * 1000
	postRollMs := r.postRollWindowMs()

	ranges := r.events.snapshot(now, preRollMs, postRollMs)

	stillOpen := make([]windowRange, 0, len(ranges))
	for _, w := range ranges {
		r.promoteRangeLocked(w.start, w.end)
		if w.open {
			stillOpen = append(stillOpen, w)
		} else {
			r.events.retire(w.eventID)
		}
	}
	return stillOpen
}

// promoteRangeLocked marks referenced every segment (across every role this
// recorder is configured for) overlapping [windowStart, windowEnd]. Callers
// must hold r.retentionMu (not enforced here — this is a private helper
// only ever called from within this file, both of whose call sites already
// hold it).
func (r *Recorder) promoteRangeLocked(windowStart, windowEnd int64) {
	for _, role := range r.cfg.Roles {
		segs, err := r.segStore.InRange(r.cfg.CameraID, role, windowStart, windowEnd)
		if err != nil {
			r.logf("recorder: %s/%s: promote range [%d,%d]: query: %v", r.cfg.CameraID, role, windowStart, windowEnd, err)
			continue
		}
		if len(segs) == 0 {
			continue
		}

		ids := make([]int64, len(segs))
		for i, seg := range segs {
			ids[i] = seg.ID
		}
		if err := r.segStore.MarkReferenced(ids); err != nil {
			r.logf("recorder: %s/%s: promote range [%d,%d]: mark %d segment(s) referenced: %v", r.cfg.CameraID, role, windowStart, windowEnd, len(ids), err)
		}
	}
}

// sweepEventSpool is the events-mode janitor, called once per
// watchSegments tick (recorder.go). It first runs a promotion pass (via
// promoteOpenWindowsLocked — the "re-checked on each janitor tick"
// defense-in-depth path, on top of MarkEvent/promoteIfCovered's own calls),
// then deletes every unreferenced segment for r.cfg.CameraID that is BOTH
// older than PreRollS seconds AND not covered by any window still open
// after that promotion pass — removing the SegmentStore row and the
// underlying file. A segment covered by an open window is never a deletion
// candidate regardless of age (Critical 2's fix: a pre-roll segment for a
// long-running active event stays protected no matter how stale it looks
// relative to `now - preRoll`, because its window has no known end yet).
//
// The whole promote-then-decide-then-delete sequence runs under
// r.retentionMu, the same lock MarkEvent/promoteIfCovered take before
// touching SegmentStore's referenced flag — so a promotion and a sweep can
// never interleave (the TOCTOU fix: it's not that the window between the
// query and the delete got narrower, it's that nothing else can run during
// it at all).
//
// No-op outside RecordingModeEvents — continuous mode has nothing
// unreferenced to sweep (see initiallyReferenced), so this call is
// harmless there.
func (r *Recorder) sweepEventSpool() {
	if r.cfg.Mode != RecordingModeEvents || r.segStore == nil {
		return
	}

	r.retentionMu.Lock()
	defer r.retentionMu.Unlock()

	openWindows := r.promoteOpenWindowsLocked()

	now := r.nowFn()
	preRollMs := int64(r.cfg.PreRollS) * 1000
	cutoffMs := now - preRollMs

	candidates, err := r.segStore.UnreferencedOlderThan(r.cfg.CameraID, cutoffMs)
	if err != nil {
		r.logf("recorder: %s: sweep event spool: query candidates: %v", r.cfg.CameraID, err)
		return
	}
	if len(candidates) == 0 {
		return
	}

	var toDelete []int64
	for _, seg := range candidates {
		if segmentProtected(seg, openWindows) {
			continue
		}
		toDelete = append(toDelete, seg.ID)
	}
	if len(toDelete) == 0 {
		return
	}

	paths, err := r.segStore.DeleteByIDs(toDelete)
	if err != nil {
		r.logf("recorder: %s: sweep event spool: delete: %v", r.cfg.CameraID, err)
		return
	}

	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			r.logf("recorder: %s: sweep event spool: remove %s: %v", r.cfg.CameraID, path, err)
		}
	}
}

// segmentProtected reports whether seg overlaps any of the given (already
// open-filtered, in practice — see promoteOpenWindowsLocked's return value)
// window ranges.
func segmentProtected(seg store.Segment, windows []windowRange) bool {
	for _, w := range windows {
		if w.overlaps(seg) {
			return true
		}
	}
	return false
}
