package store

import (
	"fmt"
	"sync"
)

// Segment is one indexed recorded video segment: a single file on disk
// covering [StartMs, EndMs) for one camera/role (e.g. "main"/"sub"),
// produced by the recorder and consumed by playback/export/retention.
//
// Referenced (Task 8) distinguishes permanently-retained segments from
// events-mode "spool" segments that haven't (yet, or ever) been claimed by
// a detection event: see MarkReferenced and DeleteUnreferencedOlderThan,
// and recorder/event_mode.go which drives both. Continuous-mode recording
// always inserts segments with Referenced=true; nothing in this store ever
// flips it back to false.
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
// mu serializes every method against the shared *DB.Conn(): go-sqlite3's
// *sqlite3.Conn is documented as "not safe for concurrent use by multiple
// goroutines" (conn.go), but SegmentStore itself is: the recorder (Task 7)
// runs one goroutine per recorded role, each indexing finished segments into
// the same SegmentStore concurrently, and RPC read handlers (later tasks)
// query it from yet another goroutine while recording is ongoing. Without
// this lock those goroutines would race directly on the underlying
// connection's internal state, not just risk an app-level SQLITE_BUSY.
type SegmentStore struct {
	mu sync.Mutex
	db *DB
}

// NewSegmentStore returns a SegmentStore backed by db.
func NewSegmentStore(db *DB) *SegmentStore {
	return &SegmentStore{db: db}
}

// Add inserts seg as a new row and returns its assigned id.
func (s *SegmentStore) Add(seg Segment) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

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
	s.mu.Lock()
	defer s.mu.Unlock()

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

// Days returns the distinct calendar days, as "YYYY-MM-DD" strings sorted
// ascending, on which cameraID has at least one recorded segment starting
// within the given year/month. Days are derived from start_ms interpreted
// as a UTC Unix epoch millisecond timestamp (SQLite's date()/strftime()
// functions operate in UTC by default; no other convention is established
// elsewhere in this package, so UTC is used here).
func (s *SegmentStore) Days(cameraID string, year, month int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

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
	s.mu.Lock()
	defer s.mu.Unlock()

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

// MarkReferenced flips referenced = 1 for every segment id given, promoting
// them out of the events-mode "spool" so DeleteUnreferencedOlderThan's
// janitor sweep never removes them. Called by recorder.Recorder.MarkEvent
// once it has resolved which segments cover a detection event's
// [start-preRoll, end+postRoll] window (recorder/event_mode.go). A no-op,
// not an error, when ids is empty (MarkEvent's usual case when a role has no
// segments in the event's window yet).
func (s *SegmentStore) MarkReferenced(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

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

// DeleteUnreferencedOlderThan removes every segment row for cameraID that is
// still unreferenced (referenced = 0 — an events-mode spool segment never
// claimed by MarkEvent) and whose end_ms falls strictly before cutoffMs,
// returning the file paths of the rows removed so the caller can delete the
// underlying files too (this method only touches the DB rows, matching
// DeleteOlderThan's contract).
//
// This is the events-mode janitor's mechanism (recorder/event_mode.go): the
// recorder calls it on a timer with cutoffMs = now - preRollS, so a spool
// segment is only ever discarded once it's older than the camera's
// configured pre-roll — guaranteeing a fresh detection event can always
// still find, and MarkReferenced promote, the pre-roll segments it needs.
// Referenced segments are never touched here regardless of age; pruning
// those by age/retention is DeleteOlderThan's (a later retention-GC task's)
// separate job.
func (s *SegmentStore) DeleteUnreferencedOlderThan(cameraID string, cutoffMs int64) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	conn := s.db.Conn()

	if err := conn.Exec("BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("store: begin delete unreferenced older than: %w", err)
	}

	paths, err := s.unreferencedPathsOlderThan(cameraID, cutoffMs)
	if err != nil {
		_ = conn.Exec("ROLLBACK")
		return nil, err
	}

	stmt, _, err := conn.Prepare(`DELETE FROM segments WHERE camera_id = ? AND referenced = 0 AND end_ms < ?`)
	if err != nil {
		_ = conn.Exec("ROLLBACK")
		return nil, fmt.Errorf("store: prepare delete unreferenced older than: %w", err)
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
		return nil, fmt.Errorf("store: delete unreferenced older than: %w", bindErr)
	}

	if err := conn.Exec("COMMIT"); err != nil {
		return nil, fmt.Errorf("store: commit delete unreferenced older than: %w", err)
	}
	return paths, nil
}

// unreferencedPathsOlderThan returns the paths of the segments matching the
// same predicate used by DeleteUnreferencedOlderThan's DELETE, so the two
// stay in sync. Internal helper only called from DeleteUnreferencedOlderThan,
// which already holds s.mu — it must not lock again itself (sync.Mutex isn't
// reentrant).
func (s *SegmentStore) unreferencedPathsOlderThan(cameraID string, cutoffMs int64) ([]string, error) {
	stmt, _, err := s.db.Conn().Prepare(`
		SELECT path FROM segments WHERE camera_id = ? AND referenced = 0 AND end_ms < ?`)
	if err != nil {
		return nil, fmt.Errorf("store: prepare select unreferenced older than: %w", err)
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
		return nil, fmt.Errorf("store: scan select unreferenced older than: %w", err)
	}
	return paths, nil
}

// pathsOlderThan returns the paths of the segments matching the same
// predicate used by DeleteOlderThan's DELETE, so the two stay in sync.
// Internal helper only called from DeleteOlderThan, which already holds
// s.mu — it must not lock again itself (sync.Mutex isn't reentrant).
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
