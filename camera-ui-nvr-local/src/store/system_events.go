package store

import (
	"fmt"
	"strings"

	"github.com/ncruces/go-sqlite3"
)

// SystemEvent is a single instance-level (as opposed to per-detection)
// notable occurrence — recorder start/stop, disk-critical, retention runs,
// and similar — mirroring the frontend's SystemEvent (see
// docs/superpowers/specs/2026-07-19-nvr-frontend-contract.d.ts in the
// camera.ui repo). Duration is a pointer so "not provided" (an instantaneous
// event) can be told apart from a genuine zero-length duration, matching
// GetEventsOptions' own pointer-field convention (events.go) for optional
// wire fields.
//
// GAP: nothing in this plugin currently calls Insert. The system_events
// table (schema.sql) has existed since the initial schema, but no code path
// — recorder start/stop, retention GC, disk-critical detection, etc. — has
// been wired to populate it yet. GetSystemEvents (rpc_events.go) can only
// ever return an empty result until a later task adds that producer side;
// this store's Query is otherwise fully functional and unit-tested against
// rows inserted directly, so wiring a producer later needs no further store
// changes.
type SystemEvent struct {
	ID        string `msgpack:"id" json:"id"`
	Type      string `msgpack:"type" json:"type"`
	Severity  string `msgpack:"severity" json:"severity"`
	CameraID  string `msgpack:"cameraId,omitempty" json:"cameraId,omitempty"`
	Timestamp int64  `msgpack:"timestamp" json:"timestamp"`
	Duration  *int64 `msgpack:"duration,omitempty" json:"duration,omitempty"`
	Message   string `msgpack:"message" json:"message"`
}

// GetSystemEventsOptions mirrors the frontend's GetSystemEventsOptions.
type GetSystemEventsOptions struct {
	StartMs *int64 `msgpack:"startMs,omitempty" json:"startMs,omitempty"`
	EndMs   *int64 `msgpack:"endMs,omitempty" json:"endMs,omitempty"`
	Limit   *int64 `msgpack:"limit,omitempty" json:"limit,omitempty"`
}

// GetSystemEventsResult mirrors the frontend's GetSystemEventsResult.
type GetSystemEventsResult struct {
	Events  []SystemEvent `msgpack:"events" json:"events"`
	HasMore bool          `msgpack:"hasMore" json:"hasMore"`
}

// defaultSystemEventsLimit is the page size Query uses when opts.Limit is
// unset, matching defaultEventsLimit's role for EventStore.Query.
const defaultSystemEventsLimit = 100

// SystemEventStore is the typed API over the system_events table
// (schema.sql). Same single connection-level locking convention as
// EventStore/SegmentStore — see the DB type doc comment in db.go for why a
// per-store mutex would not be sufficient.
type SystemEventStore struct {
	db *DB
}

// NewSystemEventStore returns a SystemEventStore backed by db.
func NewSystemEventStore(db *DB) *SystemEventStore {
	return &SystemEventStore{db: db}
}

// Insert upserts each event by id. Nothing in this plugin calls this yet —
// see the package doc comment on SystemEvent for the producer-side gap this
// leaves getSystemEvents with.
func (s *SystemEventStore) Insert(events []SystemEvent) error {
	if len(events) == 0 {
		return nil
	}

	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`
		INSERT INTO system_events (id, camera_id, ts_ms, type, severity, message, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			camera_id = excluded.camera_id,
			ts_ms = excluded.ts_ms,
			type = excluded.type,
			severity = excluded.severity,
			message = excluded.message,
			duration_ms = excluded.duration_ms`)
	if err != nil {
		return fmt.Errorf("store: prepare insert system event: %w", err)
	}
	defer stmt.Close()

	for _, ev := range events {
		if err := bindSystemEvent(stmt, ev); err != nil {
			return err
		}
		if err := stmt.Exec(); err != nil {
			return fmt.Errorf("store: insert system event %s: %w", ev.ID, err)
		}
		if err := stmt.Reset(); err != nil {
			return fmt.Errorf("store: reset insert system event statement: %w", err)
		}
	}
	return nil
}

// bindSystemEvent binds ev's columns onto stmt (already prepared by Insert)
// in the same column order as its INSERT statement.
func bindSystemEvent(stmt *sqlite3.Stmt, ev SystemEvent) error {
	if err := stmt.BindText(1, ev.ID); err != nil {
		return err
	}
	if err := stmt.BindText(2, ev.CameraID); err != nil {
		return err
	}
	if err := stmt.BindInt64(3, ev.Timestamp); err != nil {
		return err
	}
	if err := stmt.BindText(4, ev.Type); err != nil {
		return err
	}
	if err := stmt.BindText(5, ev.Severity); err != nil {
		return err
	}
	if err := stmt.BindText(6, ev.Message); err != nil {
		return err
	}
	if ev.Duration != nil {
		return stmt.BindInt64(7, *ev.Duration)
	}
	return stmt.BindNull(7)
}

// Query returns system events for cameraIDs (every camera if empty,
// mirroring EventStore.Query's getEvents-vs-getCameraEvents convention),
// optionally windowed by opts.StartMs/EndMs, newest-first, paginated by
// opts.Limit (defaultSystemEventsLimit when unset) with HasMore reporting
// whether more rows exist past the returned page.
func (s *SystemEventStore) Query(cameraIDs []string, opts GetSystemEventsOptions) (GetSystemEventsResult, error) {
	query, args, limit := buildSystemEventsQuery(cameraIDs, opts)

	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(query)
	if err != nil {
		return GetSystemEventsResult{}, fmt.Errorf("store: prepare query system events: %w", err)
	}
	defer stmt.Close()

	for i, arg := range args {
		idx := i + 1
		switch v := arg.(type) {
		case string:
			err = stmt.BindText(idx, v)
		case int64:
			err = stmt.BindInt64(idx, v)
		default:
			err = fmt.Errorf("store: unsupported bind arg type %T", v)
		}
		if err != nil {
			return GetSystemEventsResult{}, err
		}
	}

	var rows []SystemEvent
	for stmt.Step() {
		ev := SystemEvent{
			ID:        stmt.ColumnText(0),
			CameraID:  stmt.ColumnText(1),
			Timestamp: stmt.ColumnInt64(2),
			Type:      stmt.ColumnText(3),
			Severity:  stmt.ColumnText(4),
			Message:   stmt.ColumnText(5),
		}
		if stmt.ColumnType(6) != sqlite3.NULL {
			d := stmt.ColumnInt64(6)
			ev.Duration = &d
		}
		rows = append(rows, ev)
	}
	if err := stmt.Err(); err != nil {
		return GetSystemEventsResult{}, fmt.Errorf("store: scan system events: %w", err)
	}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	if rows == nil {
		rows = []SystemEvent{}
	}

	return GetSystemEventsResult{Events: rows, HasMore: hasMore}, nil
}

// buildSystemEventsQuery constructs the SQL text, positional bind args, and
// effective page size for Query, mirroring buildEventsQuery's shape
// (events.go) minus the Go-side post-filter pass system events don't need
// (SystemEvent has no nested structure to filter on).
func buildSystemEventsQuery(cameraIDs []string, opts GetSystemEventsOptions) (string, []any, int) {
	var clauses []string
	var args []any

	if len(cameraIDs) > 0 {
		placeholders := make([]string, len(cameraIDs))
		for i, id := range cameraIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		clauses = append(clauses, fmt.Sprintf("camera_id IN (%s)", strings.Join(placeholders, ",")))
	}
	if opts.StartMs != nil {
		clauses = append(clauses, "ts_ms >= ?")
		args = append(args, *opts.StartMs)
	}
	if opts.EndMs != nil {
		clauses = append(clauses, "ts_ms <= ?")
		args = append(args, *opts.EndMs)
	}

	query := "SELECT id, camera_id, ts_ms, type, severity, message, duration_ms FROM system_events"
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY ts_ms DESC"

	limit := defaultSystemEventsLimit
	if opts.Limit != nil && *opts.Limit > 0 {
		limit = int(*opts.Limit)
	}
	query += " LIMIT ?"
	args = append(args, int64(limit+1))

	return query, args, limit
}
