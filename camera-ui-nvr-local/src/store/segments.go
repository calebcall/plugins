package store

import (
	"fmt"
)

// Segment is one indexed recorded video segment: a single file on disk
// covering [StartMs, EndMs) for one camera/role (e.g. "main"/"sub"),
// produced by the recorder and consumed by playback/export/retention.
//
// Referenced (Task 8) distinguishes permanently-retained segments from
// events-mode "spool" segments that haven't (yet, or ever) been claimed by
// a detection event: see MarkReferenced and UnreferencedOlderThan/
// DeleteByIDs, and recorder/event_mode.go which drives all three.
// Continuous-mode recording always inserts segments with Referenced=true;
// nothing in this store ever flips it back to false.
type Segment struct {
	ID         int64
	CameraID   string
	Role       string
	Path       string
	StartMs    int64
	EndMs      int64
	HasVideo   bool
	HasAudio   bool
	Codec      string
	Referenced bool
}

// SegmentStore is the typed API over the segments table (see schema.sql):
// indexing recorded segments and querying them by time range or by day, and
// pruning rows past a retention cutoff.
//
// Every method locks db (s.db.Lock()/Unlock(), the *DB.Conn()-guarding lock
// documented on the DB type in db.go) around its conn access, rather than a
// SegmentStore-private mutex: the recorder (Task 7) runs one goroutine per
// recorded role, each indexing finished segments into the same SegmentStore
// concurrently, RPC read handlers (later tasks) query it from yet another
// goroutine while recording is ongoing, and — since Task 9 — the retention
// ticker deletes from it concurrently with EventStore/VectorBackend calls
// touching the very same underlying connection. A private mutex here would
// only ever have serialized SegmentStore's own methods against each other,
// not against those other stores sharing the same conn — exactly the gap
// the Task 9 review found and fixed by moving to one connection-level lock.
type SegmentStore struct {
	db *DB
}

// NewSegmentStore returns a SegmentStore backed by db.
func NewSegmentStore(db *DB) *SegmentStore {
	return &SegmentStore{db: db}
}

// Add inserts seg as a new row and returns its assigned id.
func (s *SegmentStore) Add(seg Segment) (int64, error) {
	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`
		INSERT INTO segments (camera_id, role, path, start_ms, end_ms, has_video, has_audio, codec, referenced)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare insert segment: %w", err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, seg.CameraID); err != nil {
		return 0, err
	}
	if err := stmt.BindText(2, seg.Role); err != nil {
		return 0, err
	}
	if err := stmt.BindText(3, seg.Path); err != nil {
		return 0, err
	}
	if err := stmt.BindInt64(4, seg.StartMs); err != nil {
		return 0, err
	}
	if err := stmt.BindInt64(5, seg.EndMs); err != nil {
		return 0, err
	}
	if err := stmt.BindBool(6, seg.HasVideo); err != nil {
		return 0, err
	}
	if err := stmt.BindBool(7, seg.HasAudio); err != nil {
		return 0, err
	}
	if err := stmt.BindText(8, seg.Codec); err != nil {
		return 0, err
	}
	if err := stmt.BindBool(9, seg.Referenced); err != nil {
		return 0, err
	}

	if err := stmt.Exec(); err != nil {
		return 0, fmt.Errorf("store: insert segment: %w", err)
	}

	return s.db.Conn().LastInsertRowID(), nil
}

// InRange returns the segments for cameraID/role that overlap the window
// [startMs, endMs], ordered by start_ms ascending. The overlap test is
// inclusive on both ends (end_ms >= startMs AND start_ms <= endMs) so a
// segment merely touching a window boundary is still included.
func (s *SegmentStore) InRange(cameraID, role string, startMs, endMs int64) ([]Segment, error) {
	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`
		SELECT id, camera_id, role, path, start_ms, end_ms, has_video, has_audio, codec, referenced
		FROM segments
		WHERE camera_id = ? AND role = ? AND end_ms >= ? AND start_ms <= ?
		ORDER BY start_ms ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: prepare segments in range: %w", err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, cameraID); err != nil {
		return nil, err
	}
	if err := stmt.BindText(2, role); err != nil {
		return nil, err
	}
	if err := stmt.BindInt64(3, startMs); err != nil {
		return nil, err
	}
	if err := stmt.BindInt64(4, endMs); err != nil {
		return nil, err
	}

	var segs []Segment
	for stmt.Step() {
		segs = append(segs, Segment{
			ID:         stmt.ColumnInt64(0),
			CameraID:   stmt.ColumnText(1),
			Role:       stmt.ColumnText(2),
			Path:       stmt.ColumnText(3),
			StartMs:    stmt.ColumnInt64(4),
			EndMs:      stmt.ColumnInt64(5),
			HasVideo:   stmt.ColumnBool(6),
			HasAudio:   stmt.ColumnBool(7),
			Codec:      stmt.ColumnText(8),
			Referenced: stmt.ColumnBool(9),
		})
	}
	if err := stmt.Err(); err != nil {
		return nil, fmt.Errorf("store: scan segments in range: %w", err)
	}
	return segs, nil
}

// CoveringSegment returns the single indexed segment for cameraID that
// covers atMs (start_ms <= atMs AND end_ms >= atMs) — the recorded footage a
// detection event at that moment falls inside, if any. When more than one
// role's segment covers the same moment (e.g. both a high- and
// low-resolution stream are recorded), the higher-resolution role is
// preferred, matching the recorder's own default recording role (see
// recorder/manager.go's defaultRoles). ok is false, with no error, when
// nothing covers atMs — an expected condition (an event timestamped before
// recording started, or one that landed inside the still-open segment
// ffmpeg hasn't finalized/indexed yet — see Recorder.watchSegments' "skip
// the newest file" doc comment), not an error condition: callers (Task 11's
// thumbnail generation) must treat ok==false as "skip gracefully", never as
// a failure.
func (s *SegmentStore) CoveringSegment(cameraID string, atMs int64) (Segment, bool, error) {
	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`
		SELECT id, camera_id, role, path, start_ms, end_ms, has_video, has_audio, codec, referenced
		FROM segments
		WHERE camera_id = ? AND start_ms <= ? AND end_ms >= ?
		ORDER BY CASE role
			WHEN 'high-resolution' THEN 0
			WHEN 'mid-resolution' THEN 1
			WHEN 'low-resolution' THEN 2
			WHEN 'snapshot' THEN 3
			ELSE 4
		END, start_ms DESC
		LIMIT 1`)
	if err != nil {
		return Segment{}, false, fmt.Errorf("store: prepare covering segment: %w", err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, cameraID); err != nil {
		return Segment{}, false, err
	}
	if err := stmt.BindInt64(2, atMs); err != nil {
		return Segment{}, false, err
	}
	if err := stmt.BindInt64(3, atMs); err != nil {
		return Segment{}, false, err
	}

	if !stmt.Step() {
		if err := stmt.Err(); err != nil {
			return Segment{}, false, fmt.Errorf("store: scan covering segment: %w", err)
		}
		return Segment{}, false, nil
	}

	seg := Segment{
		ID:         stmt.ColumnInt64(0),
		CameraID:   stmt.ColumnText(1),
		Role:       stmt.ColumnText(2),
		Path:       stmt.ColumnText(3),
		StartMs:    stmt.ColumnInt64(4),
		EndMs:      stmt.ColumnInt64(5),
		HasVideo:   stmt.ColumnBool(6),
		HasAudio:   stmt.ColumnBool(7),
		Codec:      stmt.ColumnText(8),
		Referenced: stmt.ColumnBool(9),
	}
	return seg, true, nil
}

// CoveringSegmentForRole returns the single indexed segment for
// cameraID/role that covers atMs (start_ms <= atMs AND end_ms >= atMs),
// picking the most recently started one when more than one row happens to
// match (mirrors CoveringSegment's own tie-break). Unlike CoveringSegment
// (used by the Task-11 thumbnail generator, which doesn't care which
// role's footage it draws from) this is scoped to exactly one role — the
// scrub/preview-frame RPCs (src/media/scrub.go) need the specific role the
// frontend asked for (sourceRole, defaulting to "high-resolution"), not
// whichever role happens to rank highest. ok is false, with no error, when
// nothing covers atMs for that role — an expected condition (before
// recording started, or a still-open segment not yet finalized/indexed),
// not an error.
func (s *SegmentStore) CoveringSegmentForRole(cameraID, role string, atMs int64) (Segment, bool, error) {
	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`
		SELECT id, camera_id, role, path, start_ms, end_ms, has_video, has_audio, codec, referenced
		FROM segments
		WHERE camera_id = ? AND role = ? AND start_ms <= ? AND end_ms >= ?
		ORDER BY start_ms DESC
		LIMIT 1`)
	if err != nil {
		return Segment{}, false, fmt.Errorf("store: prepare covering segment for role: %w", err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, cameraID); err != nil {
		return Segment{}, false, err
	}
	if err := stmt.BindText(2, role); err != nil {
		return Segment{}, false, err
	}
	if err := stmt.BindInt64(3, atMs); err != nil {
		return Segment{}, false, err
	}
	if err := stmt.BindInt64(4, atMs); err != nil {
		return Segment{}, false, err
	}

	if !stmt.Step() {
		if err := stmt.Err(); err != nil {
			return Segment{}, false, fmt.Errorf("store: scan covering segment for role: %w", err)
		}
		return Segment{}, false, nil
	}

	seg := Segment{
		ID:         stmt.ColumnInt64(0),
		CameraID:   stmt.ColumnText(1),
		Role:       stmt.ColumnText(2),
		Path:       stmt.ColumnText(3),
		StartMs:    stmt.ColumnInt64(4),
		EndMs:      stmt.ColumnInt64(5),
		HasVideo:   stmt.ColumnBool(6),
		HasAudio:   stmt.ColumnBool(7),
		Codec:      stmt.ColumnText(8),
		Referenced: stmt.ColumnBool(9),
	}
	return seg, true, nil
}

// CoversRange reports whether at least one indexed segment for cameraID —
// across every recorded role, not just one — overlaps [startMs, endMs]
// (inclusive on both ends, the same overlap test InRange/CoveringSegment
// use: end_ms >= startMs AND start_ms <= endMs). startMs == endMs is a
// valid, useful call (a point-in-time check, equivalent to CoveringSegment
// but without CoveringSegment's role-priority tie-break, which doesn't
// matter here since only existence is reported).
//
// This backs event ingestion's has_recording computation
// (events_ingest.go): CoveringSegment/CoveringSegmentForRole answer "what
// footage covers this instant/role", the question thumbnail generation and
// scrub/playback need; this answers the coarser "was ANY of this event's
// [start,end] window actually recorded at all", which is what
// has_recording is for. ok is always nil-error, true/false — no coverage
// is an expected, common outcome (recording not yet started, mode "off",
// or an events-mode spool segment not yet promoted), never treated as a
// failure.
func (s *SegmentStore) CoversRange(cameraID string, startMs, endMs int64) (bool, error) {
	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`
		SELECT 1 FROM segments
		WHERE camera_id = ? AND end_ms >= ? AND start_ms <= ?
		LIMIT 1`)
	if err != nil {
		return false, fmt.Errorf("store: prepare covers range: %w", err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, cameraID); err != nil {
		return false, err
	}
	if err := stmt.BindInt64(2, startMs); err != nil {
		return false, err
	}
	if err := stmt.BindInt64(3, endMs); err != nil {
		return false, err
	}

	if !stmt.Step() {
		if err := stmt.Err(); err != nil {
			return false, fmt.Errorf("store: scan covers range: %w", err)
		}
		return false, nil
	}
	return true, nil
}

// Days returns the distinct calendar days, as "YYYY-MM-DD" strings sorted
// ascending, on which cameraID has at least one recorded segment starting
// within the given year/month. Days are derived from start_ms interpreted
// as a UTC Unix epoch millisecond timestamp (SQLite's date()/strftime()
// functions operate in UTC by default; no other convention is established
// elsewhere in this package, so UTC is used here).
func (s *SegmentStore) Days(cameraID string, year, month int) ([]string, error) {
	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`
		SELECT DISTINCT date(start_ms / 1000, 'unixepoch') AS day
		FROM segments
		WHERE camera_id = ?
		  AND strftime('%Y', start_ms / 1000, 'unixepoch') = ?
		  AND strftime('%m', start_ms / 1000, 'unixepoch') = ?
		ORDER BY day ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: prepare segment days: %w", err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, cameraID); err != nil {
		return nil, err
	}
	if err := stmt.BindText(2, fmt.Sprintf("%04d", year)); err != nil {
		return nil, err
	}
	if err := stmt.BindText(3, fmt.Sprintf("%02d", month)); err != nil {
		return nil, err
	}

	var days []string
	for stmt.Step() {
		days = append(days, stmt.ColumnText(0))
	}
	if err := stmt.Err(); err != nil {
		return nil, fmt.Errorf("store: scan segment days: %w", err)
	}
	return days, nil
}

// DeleteOlderThan removes every segment row for cameraID whose end_ms falls
// strictly before cutoffMs, and returns the file paths of the rows removed.
// This only deletes the SQLite index rows; deleting the underlying files at
// those paths is the caller's (retention task's) responsibility.
func (s *SegmentStore) DeleteOlderThan(cameraID string, cutoffMs int64) ([]string, error) {
	s.db.Lock()
	defer s.db.Unlock()

	conn := s.db.Conn()

	if err := conn.Exec("BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("store: begin delete older than: %w", err)
	}

	paths, err := s.pathsOlderThan(cameraID, cutoffMs)
	if err != nil {
		_ = conn.Exec("ROLLBACK")
		return nil, err
	}

	stmt, _, err := conn.Prepare(`DELETE FROM segments WHERE camera_id = ? AND end_ms < ?`)
	if err != nil {
		_ = conn.Exec("ROLLBACK")
		return nil, fmt.Errorf("store: prepare delete older than: %w", err)
	}
	bindErr := func() error {
		defer stmt.Close()
		if err := stmt.BindText(1, cameraID); err != nil {
			return err
		}
		if err := stmt.BindInt64(2, cutoffMs); err != nil {
			return err
		}
		return stmt.Exec()
	}()
	if bindErr != nil {
		_ = conn.Exec("ROLLBACK")
		return nil, fmt.Errorf("store: delete older than: %w", bindErr)
	}

	if err := conn.Exec("COMMIT"); err != nil {
		return nil, fmt.Errorf("store: commit delete older than: %w", err)
	}
	return paths, nil
}

// AllByCamera returns every segment row for cameraID, across every role,
// ordered by start_ms ascending (oldest first). Unlike InRange (scoped to a
// single role and time window) this is a full per-camera listing — used by
// the retention task's disk-cap garbage collection (recorder/retention.go)
// to compute total on-disk usage and identify the oldest segments to delete
// first when a camera's configured quota is exceeded.
func (s *SegmentStore) AllByCamera(cameraID string) ([]Segment, error) {
	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`
		SELECT id, camera_id, role, path, start_ms, end_ms, has_video, has_audio, codec, referenced
		FROM segments
		WHERE camera_id = ?
		ORDER BY start_ms ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: prepare all by camera: %w", err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, cameraID); err != nil {
		return nil, err
	}

	var segs []Segment
	for stmt.Step() {
		segs = append(segs, Segment{
			ID:         stmt.ColumnInt64(0),
			CameraID:   stmt.ColumnText(1),
			Role:       stmt.ColumnText(2),
			Path:       stmt.ColumnText(3),
			StartMs:    stmt.ColumnInt64(4),
			EndMs:      stmt.ColumnInt64(5),
			HasVideo:   stmt.ColumnBool(6),
			HasAudio:   stmt.ColumnBool(7),
			Codec:      stmt.ColumnText(8),
			Referenced: stmt.ColumnBool(9),
		})
	}
	if err := stmt.Err(); err != nil {
		return nil, fmt.Errorf("store: scan all by camera: %w", err)
	}
	return segs, nil
}

// MarkReferenced flips referenced = 1 for every segment id given, promoting
// them out of the events-mode "spool" so the janitor (UnreferencedOlderThan
// + DeleteByIDs, driven by recorder.Recorder.sweepEventSpool) never removes
// them. Called by recorder.Recorder's promotion paths (MarkEvent,
// promoteIfCovered, promoteRangeLocked) once they've resolved which
// segments cover one of a camera's currently protected detection-event
// windows (recorder/event_mode.go). A no-op, not an error, when ids is
// empty (the common case when a query found nothing new to promote).
func (s *SegmentStore) MarkReferenced(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`UPDATE segments SET referenced = 1 WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("store: prepare mark referenced: %w", err)
	}
	defer stmt.Close()

	for _, id := range ids {
		if err := stmt.BindInt64(1, id); err != nil {
			return err
		}
		if err := stmt.Exec(); err != nil {
			return fmt.Errorf("store: mark segment %d referenced: %w", id, err)
		}
		if err := stmt.Reset(); err != nil {
			return fmt.Errorf("store: reset mark referenced statement: %w", err)
		}
	}
	return nil
}

// UnreferencedOlderThan returns (read-only — no delete) every segment row
// for cameraID that is unreferenced (referenced = 0 — an events-mode spool
// segment never claimed by any promotion path) and whose end_ms falls
// strictly before cutoffMs: the events-mode janitor's candidate set, before
// recorder.Recorder.sweepEventSpool filters out anything still covered by a
// currently open protected detection-event window (recorder/event_mode.go
// — that filtering needs each candidate's own StartMs/EndMs to check
// against a set of windows that can't be expressed as a single additional
// SQL predicate here, hence the read-then-filter-then-DeleteByIDs split
// instead of one combined delete statement like DeleteOlderThan's).
func (s *SegmentStore) UnreferencedOlderThan(cameraID string, cutoffMs int64) ([]Segment, error) {
	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`
		SELECT id, camera_id, role, path, start_ms, end_ms, has_video, has_audio, codec, referenced
		FROM segments WHERE camera_id = ? AND referenced = 0 AND end_ms < ?`)
	if err != nil {
		return nil, fmt.Errorf("store: prepare unreferenced older than: %w", err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, cameraID); err != nil {
		return nil, err
	}
	if err := stmt.BindInt64(2, cutoffMs); err != nil {
		return nil, err
	}

	var segs []Segment
	for stmt.Step() {
		segs = append(segs, Segment{
			ID:         stmt.ColumnInt64(0),
			CameraID:   stmt.ColumnText(1),
			Role:       stmt.ColumnText(2),
			Path:       stmt.ColumnText(3),
			StartMs:    stmt.ColumnInt64(4),
			EndMs:      stmt.ColumnInt64(5),
			HasVideo:   stmt.ColumnBool(6),
			HasAudio:   stmt.ColumnBool(7),
			Codec:      stmt.ColumnText(8),
			Referenced: stmt.ColumnBool(9),
		})
	}
	if err := stmt.Err(); err != nil {
		return nil, fmt.Errorf("store: scan unreferenced older than: %w", err)
	}
	return segs, nil
}

// DeleteByIDs removes exactly the given segment rows, regardless of their
// referenced flag or age, and returns their file paths — the second half of
// the events-mode janitor's UnreferencedOlderThan-then-filter-then-
// DeleteByIDs sequence (recorder.Recorder.sweepEventSpool), for a caller
// that has already independently decided, via its own predicate (there:
// "unreferenced, stale, AND not covered by any open protected window"),
// exactly which rows to remove. A no-op, not an error, when ids is empty.
func (s *SegmentStore) DeleteByIDs(ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	s.db.Lock()
	defer s.db.Unlock()

	conn := s.db.Conn()

	if err := conn.Exec("BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("store: begin delete by ids: %w", err)
	}

	paths, err := s.pathsByIDs(ids)
	if err != nil {
		_ = conn.Exec("ROLLBACK")
		return nil, err
	}

	stmt, _, err := conn.Prepare(`DELETE FROM segments WHERE id = ?`)
	if err != nil {
		_ = conn.Exec("ROLLBACK")
		return nil, fmt.Errorf("store: prepare delete by ids: %w", err)
	}
	execErr := func() error {
		defer stmt.Close()
		for _, id := range ids {
			if err := stmt.BindInt64(1, id); err != nil {
				return err
			}
			if err := stmt.Exec(); err != nil {
				return err
			}
			if err := stmt.Reset(); err != nil {
				return err
			}
		}
		return nil
	}()
	if execErr != nil {
		_ = conn.Exec("ROLLBACK")
		return nil, fmt.Errorf("store: delete by ids: %w", execErr)
	}

	if err := conn.Exec("COMMIT"); err != nil {
		return nil, fmt.Errorf("store: commit delete by ids: %w", err)
	}
	return paths, nil
}

// pathsByIDs returns the paths of the segments matching ids, so DeleteByIDs
// can report what it removed. Internal helper only called from
// DeleteByIDs, which already holds the db lock (s.db.Lock()) — it must not
// lock again itself (sync.Mutex isn't reentrant).
func (s *SegmentStore) pathsByIDs(ids []int64) ([]string, error) {
	stmt, _, err := s.db.Conn().Prepare(`SELECT path FROM segments WHERE id = ?`)
	if err != nil {
		return nil, fmt.Errorf("store: prepare select by ids: %w", err)
	}
	defer stmt.Close()

	var paths []string
	for _, id := range ids {
		if err := stmt.BindInt64(1, id); err != nil {
			return nil, err
		}
		if stmt.Step() {
			paths = append(paths, stmt.ColumnText(0))
		}
		if err := stmt.Err(); err != nil {
			return nil, fmt.Errorf("store: scan select by ids: %w", err)
		}
		if err := stmt.Reset(); err != nil {
			return nil, fmt.Errorf("store: reset select by ids: %w", err)
		}
	}
	return paths, nil
}

// DistinctCameraIDs returns every camera ID that has at least one indexed
// segment row, sorted ascending. Used by the getStorageStats RPC handler
// (rpc_recording.go) to decide which cameras to build a CameraStorageStats
// entry for — driven by what has actually been recorded to disk, rather
// than by which cameras are currently assigned/managed, so a camera
// reassigned away from this Hub (or switched to recordingMode "off") still
// gets its historical usage reported instead of silently disappearing from
// the storage breakdown.
func (s *SegmentStore) DistinctCameraIDs() ([]string, error) {
	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`SELECT DISTINCT camera_id FROM segments ORDER BY camera_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: prepare distinct camera ids: %w", err)
	}
	defer stmt.Close()

	var ids []string
	for stmt.Step() {
		ids = append(ids, stmt.ColumnText(0))
	}
	if err := stmt.Err(); err != nil {
		return nil, fmt.Errorf("store: scan distinct camera ids: %w", err)
	}
	return ids, nil
}

// AllPaths returns the path of every indexed segment row, across every
// camera and role, in no particular order — the full "known to be indexed"
// set the retention task's orphan-file sweep (recorder/retention.go) diffs
// disk contents against: any *.mp4 file under the recordings tree whose
// path isn't in this list has no segments-table row at all (e.g. one
// written by a recorder that crashed/restarted before it could finalize
// and index the segment), and is therefore a candidate for that sweep to
// remove, once it's also old enough to be safely past "might still be
// written to" (see sweepOrphanFiles' grace-period doc comment).
func (s *SegmentStore) AllPaths() ([]string, error) {
	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`SELECT path FROM segments`)
	if err != nil {
		return nil, fmt.Errorf("store: prepare all paths: %w", err)
	}
	defer stmt.Close()

	var paths []string
	for stmt.Step() {
		paths = append(paths, stmt.ColumnText(0))
	}
	if err := stmt.Err(); err != nil {
		return nil, fmt.Errorf("store: scan all paths: %w", err)
	}
	return paths, nil
}

// HasPath reports whether path has at least one row in the segments table
// right now — a fresh, freshly-locked (s.db.Lock/Unlock), point-in-time
// check, deliberately NOT served from any cached/snapshotted listing.
//
// This exists for retention's orphan sweep (recorder/retention.go,
// sweepOrphanFiles) to call IMMEDIATELY before deleting a file its
// AllPaths() snapshot classified as unindexed: that snapshot is taken once,
// before a (potentially long, for a large recordings tree) filesystem walk,
// so a segment can be finalized and indexed — by a recorder that crashed
// and was then restarted, the exact case the sweep exists to clean up
// after — AFTER the snapshot but BEFORE the walk reaches its file. Deciding
// "still an orphan" from the stale snapshot alone in that window would
// delete a file that, by the time it's actually removed, has a perfectly
// valid segment row — real, indexed footage lost. HasPath closes that
// window: the sweep must re-ask the store, not its earlier snapshot, right
// before it acts.
func (s *SegmentStore) HasPath(path string) (bool, error) {
	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`SELECT 1 FROM segments WHERE path = ? LIMIT 1`)
	if err != nil {
		return false, fmt.Errorf("store: prepare has path: %w", err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, path); err != nil {
		return false, err
	}

	if !stmt.Step() {
		if err := stmt.Err(); err != nil {
			return false, fmt.Errorf("store: scan has path: %w", err)
		}
		return false, nil
	}
	return true, nil
}

// pathsOlderThan returns the paths of the segments matching the same
// predicate used by DeleteOlderThan's DELETE, so the two stay in sync.
// Internal helper only called from DeleteOlderThan, which already holds
// the db lock (s.db.Lock()) — it must not lock again itself (sync.Mutex
// isn't reentrant).
func (s *SegmentStore) pathsOlderThan(cameraID string, cutoffMs int64) ([]string, error) {
	stmt, _, err := s.db.Conn().Prepare(`
		SELECT path FROM segments WHERE camera_id = ? AND end_ms < ?`)
	if err != nil {
		return nil, fmt.Errorf("store: prepare select older than: %w", err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, cameraID); err != nil {
		return nil, err
	}
	if err := stmt.BindInt64(2, cutoffMs); err != nil {
		return nil, err
	}

	var paths []string
	for stmt.Step() {
		paths = append(paths, stmt.ColumnText(0))
	}
	if err := stmt.Err(); err != nil {
		return nil, fmt.Errorf("store: scan select older than: %w", err)
	}
	return paths, nil
}
