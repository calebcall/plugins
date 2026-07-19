// event_mode.go implements Task 8's events recording mode: MarkEvent, the
// promotion side (a detection event covering some window promotes exactly
// the finalized segments overlapping it out of the events-mode "spool" into
// permanent retention), and sweepEventSpool, the discard side (a periodic
// janitor that deletes spool segments nothing ever promoted, once they're
// old enough that a fresh event's pre-roll can no longer need them).
//
// Design: rather than writing events-mode segments to a physically separate
// spool directory and moving files into a permanent tree on promotion (the
// brief's suggested alternative), this indexes every segment into the same
// SegmentStore/on-disk layout recorder.go already uses for continuous mode,
// tagged with a `referenced` flag (store.Segment.Referenced, backed by the
// segments.referenced column added in store/db.go's migrateToV2). "Spool"
// is therefore a logical state (Referenced == false), not a location.
// MarkEvent promotes by flipping that flag (SegmentStore.MarkReferenced);
// the janitor discards by deleting rows/files still flagged false past
// their age cutoff (SegmentStore.DeleteUnreferencedOlderThan). This is the
// brief's explicitly-sanctioned "least invasive design that is fully
// unit-testable" alternative: no file-move step, no second directory tree,
// and every part of the mechanism (the SQL predicates, the promotion set,
// the age cutoff) is a plain function of SegmentStore state a test can
// assert on directly, with no ffmpeg or real timing involved.
package recorder

import (
	"os"
	"time"
)

// MarkEvent ensures every already-finalized segment covering
// [startMs-PreRollS, endMs+PostRollS] is retained: it queries r.segStore for
// each of r.cfg.Roles and promotes (MarkReferenced) every segment InRange
// finds. Segments outside that window, and every segment in any mode other
// than events, are left exactly as they were — most importantly, this is a
// no-op outside RecordingModeEvents: continuous-mode segments are already
// indexed Referenced=true by finalizeSegment (see initiallyReferenced), so
// there is nothing for MarkEvent to promote and calling it is harmless.
//
// The window's overlap test is InRange's own (end_ms >= windowStart AND
// start_ms <= windowEnd, inclusive on both ends) — in particular, a segment
// that started at exactly startMs-PreRollS (the earliest moment pre-roll
// should reach back to) is retained, not excluded by an off-by-one: its
// start_ms equals windowStart, which satisfies start_ms <= windowEnd (it's
// certainly <= a window that begins at or before it) and, being a real
// segment, its end_ms is >= its own start_ms == windowStart.
//
// Safe to call with a nil r.segStore (logs and returns) purely so a
// zero-value/partially-constructed Recorder in a test doesn't panic; every
// real Recorder is constructed via NewRecorder, which always sets segStore.
func (r *Recorder) MarkEvent(startMs, endMs int64) {
	if r.cfg.Mode != RecordingModeEvents {
		return
	}
	if r.segStore == nil {
		r.logf("recorder: %s: MarkEvent called with no segment store configured", r.cfg.CameraID)
		return
	}

	windowStart := startMs - int64(r.cfg.PreRollS)*1000
	windowEnd := endMs + int64(r.cfg.PostRollS)*1000

	for _, role := range r.cfg.Roles {
		segs, err := r.segStore.InRange(r.cfg.CameraID, role, windowStart, windowEnd)
		if err != nil {
			r.logf("recorder: %s/%s: MarkEvent: query segments in [%d,%d]: %v", r.cfg.CameraID, role, windowStart, windowEnd, err)
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
			r.logf("recorder: %s/%s: MarkEvent: mark %d segment(s) referenced: %v", r.cfg.CameraID, role, len(ids), err)
		}
	}
}

// sweepEventSpool is the events-mode janitor, called once per watchSegments
// tick (see watchSegments in recorder.go): it deletes every unreferenced
// spool segment for r.cfg.CameraID whose end_ms is older than
// r.cfg.PreRollS seconds before now, removing both the SegmentStore row
// (SegmentStore.DeleteUnreferencedOlderThan) and the underlying file on
// disk. Deliberately never touches a referenced segment, and never touches
// anything at all outside RecordingModeEvents — continuous mode has nothing
// unreferenced to sweep in the first place (see initiallyReferenced).
//
// The cutoff (now - PreRollS) is exactly what keeps pre-roll available: a
// spool segment less than PreRollS seconds old is never deleted, so an
// event firing "now" can still MarkEvent-promote the segment covering
// now-PreRollS. Only once a spool segment falls fully outside every
// possible future event's pre-roll window does the janitor consider it
// discardable.
func (r *Recorder) sweepEventSpool(now time.Time) {
	if r.cfg.Mode != RecordingModeEvents || r.segStore == nil {
		return
	}

	cutoffMs := now.UnixMilli() - int64(r.cfg.PreRollS)*1000

	paths, err := r.segStore.DeleteUnreferencedOlderThan(r.cfg.CameraID, cutoffMs)
	if err != nil {
		r.logf("recorder: %s: sweep event spool: %v", r.cfg.CameraID, err)
		return
	}

	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			r.logf("recorder: %s: sweep event spool: remove %s: %v", r.cfg.CameraID, path, err)
		}
	}
}
