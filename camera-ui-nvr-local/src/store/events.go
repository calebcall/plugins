package store

import (
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/cameraui/sdk/go"
	"github.com/ncruces/go-sqlite3"
)

// DetectionEvent is the event type EventStore stores and returns. It is a
// type alias (not a redefinition) for sdk.DetectionEvent: the frontend's
// reconstructed contract (docs/superpowers/specs/2026-07-19-nvr-frontend-contract.d.ts)
// imports its own DetectionEvent from '@camera.ui/sdk', and the Go SDK's
// sdk.DetectionEvent (camera_events.go) already carries msgpack tags for
// every one of that type's fields (id, cameraId, state, startTime, endTime,
// lastUpdate, types, triggers, segments, segmentIndex, expectedEndTime,
// thumbnail, hasRecording) — there is nothing left to transcribe, and
// redefining it would risk silent drift from what
// sdk.CameraDevice.OnDetectionEvent actually delivers.
type DetectionEvent = sdk.DetectionEvent

// GetEventsOptions mirrors the frontend's GetEventsOptions (see the d.ts
// above). Every field is optional on the wire (TS `?`); the pointer fields
// (*bool, *float64, *int64) exist specifically so "not provided" can be told
// apart from the zero value (false/0), which matters for hasRecording,
// minConfidence, and the ms-timestamp fields.
type GetEventsOptions struct {
	Types                 []string `msgpack:"types,omitempty" json:"types,omitempty"`
	Triggers              []string `msgpack:"triggers,omitempty" json:"triggers,omitempty"`
	TriggerLabels         []string `msgpack:"triggerLabels,omitempty" json:"triggerLabels,omitempty"`
	Attributes            []string `msgpack:"attributes,omitempty" json:"attributes,omitempty"`
	FilterLogicTriggers   string   `msgpack:"filterLogicTriggers,omitempty" json:"filterLogicTriggers,omitempty"`
	FilterLogicAttributes string   `msgpack:"filterLogicAttributes,omitempty" json:"filterLogicAttributes,omitempty"`
	State                 string   `msgpack:"state,omitempty" json:"state,omitempty"`
	Search                string   `msgpack:"search,omitempty" json:"search,omitempty"`
	HasDetections         *bool    `msgpack:"hasDetections,omitempty" json:"hasDetections,omitempty"`
	MinConfidence         *float64 `msgpack:"minConfidence,omitempty" json:"minConfidence,omitempty"`
	HasRecording          *bool    `msgpack:"hasRecording,omitempty" json:"hasRecording,omitempty"`
	WithRecordingInfo     *bool    `msgpack:"withRecordingInfo,omitempty" json:"withRecordingInfo,omitempty"`
	StartMs               *int64   `msgpack:"startMs,omitempty" json:"startMs,omitempty"`
	EndMs                 *int64   `msgpack:"endMs,omitempty" json:"endMs,omitempty"`
	Limit                 *int64   `msgpack:"limit,omitempty" json:"limit,omitempty"`
	Before                *int64   `msgpack:"before,omitempty" json:"before,omitempty"`
}

// GetEventsResult mirrors the frontend's GetEventsResult.
type GetEventsResult struct {
	Events  []DetectionEvent `msgpack:"events" json:"events"`
	HasMore bool             `msgpack:"hasMore" json:"hasMore"`
}

// defaultEventsLimit is the page size Query uses when opts.Limit is unset.
const defaultEventsLimit = 100

// EventStore is the typed API over the events table (see schema.sql):
// upserting DetectionEvents and querying them back out filtered, paginated,
// and ordered newest-first for the getEvents/getCameraEvents RPC methods a
// later task adds.
//
// Every exported method locks db (s.db.Lock()/Unlock()) around its conn
// access, for exactly the reason documented on the DB type in db.go:
// EventStore shares one *sqlite3.Conn with SegmentStore and every
// VectorBackend, and that connection is not safe for concurrent use from
// multiple goroutines regardless of which store is calling it — event
// ingestion (Task 5's detection-event callbacks) upserts from one goroutine
// while, since Task 9, the retention ticker deletes from another. (An
// earlier version of this file had no locking at all, which was the same
// class of bug the Task 9 review found and fixed for SegmentStore's
// then-private mutex — just further along, since EventStore never even had
// a per-store lock to begin with.)
//
// Every row's raw column holds the full DetectionEvent as JSON, so Query
// round-trips exactly what was upserted (thumbnail bytes, segments,
// triggers, everything) rather than reconstructing a lossy approximation
// from the flat indexed columns. Those flat columns (camera_id, ts_ms,
// end_ms, types, label, confidence, box, has_recording) exist purely to let
// SQL narrow down candidate rows cheaply before the full JSON is decoded and
// (for the filters that need structure SQL can't easily see into — trigger
// types/labels, attributes, free-text search, per-segment detection
// presence) filtered again in Go. See Query's doc comment for exactly which
// filters run in SQL vs. in Go.
type EventStore struct {
	db *DB
}

// NewEventStore returns an EventStore backed by db.
func NewEventStore(db *DB) *EventStore {
	return &EventStore{db: db}
}

// Upsert inserts or replaces each event by id. thumb_ref is deliberately
// left untouched by both the INSERT and the ON CONFLICT UPDATE: it is owned
// by the (later) thumbnail-persistence task, which writes it via a separate
// update once it has generated a JPEG on disk. Upserting the same event
// again here (e.g. an 'update' or 'segment-*' message for an event already
// stored from its 'start' message) must not clobber a thumb_ref set in the
// meantime.
func (s *EventStore) Upsert(events []DetectionEvent) error {
	if len(events) == 0 {
		return nil
	}

	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`
		INSERT INTO events (id, camera_id, ts_ms, end_ms, types, label, confidence, box, has_recording, raw)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			camera_id = excluded.camera_id,
			ts_ms = excluded.ts_ms,
			end_ms = excluded.end_ms,
			types = excluded.types,
			label = excluded.label,
			confidence = excluded.confidence,
			box = excluded.box,
			has_recording = excluded.has_recording,
			raw = excluded.raw`)
	if err != nil {
		return fmt.Errorf("store: prepare upsert event: %w", err)
	}
	defer stmt.Close()

	for _, ev := range events {
		if err := upsertOneEvent(stmt, ev); err != nil {
			return err
		}
		if err := stmt.Reset(); err != nil {
			return fmt.Errorf("store: reset upsert event statement: %w", err)
		}
	}
	return nil
}

// upsertOneEvent binds ev's columns onto stmt (already prepared by Upsert)
// and executes it. Split out of Upsert so the multi-event loop stays
// readable.
func upsertOneEvent(stmt *sqlite3.Stmt, ev DetectionEvent) error {
	raw, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("store: marshal event %s: %w", ev.ID, err)
	}
	typesJSON, err := json.Marshal(ev.Types)
	if err != nil {
		return fmt.Errorf("store: marshal event %s types: %w", ev.ID, err)
	}

	if err := stmt.BindText(1, ev.ID); err != nil {
		return err
	}
	if err := stmt.BindText(2, ev.CameraID); err != nil {
		return err
	}
	if err := stmt.BindInt64(3, ev.StartTime); err != nil {
		return err
	}
	if err := stmt.BindInt64(4, ev.EndTime); err != nil {
		return err
	}
	if err := stmt.BindText(5, string(typesJSON)); err != nil {
		return err
	}
	if err := stmt.BindText(6, PrimaryLabel(ev)); err != nil {
		return err
	}
	if err := stmt.BindFloat(7, bestConfidence(ev)); err != nil {
		return err
	}
	if box := bestBox(ev); box != nil {
		boxJSON, err := json.Marshal(box)
		if err != nil {
			return fmt.Errorf("store: marshal event %s box: %w", ev.ID, err)
		}
		if err := stmt.BindText(8, string(boxJSON)); err != nil {
			return err
		}
	} else {
		if err := stmt.BindNull(8); err != nil {
			return err
		}
	}
	if err := stmt.BindBool(9, ev.HasRecording); err != nil {
		return err
	}
	if err := stmt.BindText(10, string(raw)); err != nil {
		return err
	}

	if err := stmt.Exec(); err != nil {
		return fmt.Errorf("store: upsert event %s: %w", ev.ID, err)
	}
	return nil
}

// PrimaryLabel picks a best-effort single label for the event's indexed
// `label` column (used as a coarse hint for Query's own MinConfidence-style
// SQL filters and, since detectionEventIngester (events_ingest.go) reuses it
// for push-notification titles, the human-facing label a person sees for the
// event) — Query's Types/Triggers filters themselves decode the full raw
// JSON rather than relying on this column.
//
// Exported (was primaryLabel) specifically so events_ingest.go's
// notification path computes the exact same label the stored/indexed event
// carries, rather than duplicating (and risking drifting from) this
// ranking.
//
// Ranked, in order:
//
//  1. The Label of whichever segment detection (ev.Segments[].Detections)
//     has the highest Score, skipping any with an empty Label — the actual
//     detected object (person/vehicle/animal/...), when one was reported.
//  2. The first ev.Types entry that is NOT "motion", "audio", or "clip" —
//     an object-detection type name, when no per-segment detection is
//     available but Types still names the object. This is the fix for the
//     bug PrimaryLabel replaces: ev.Types is alphabetically sorted (not
//     lifecycle-ordered), so a person event carrying
//     ["clip","motion","person"] previously returned Types[0] == "clip"
//     unconditionally, never "person".
//  3. The first non-empty trigger Label (ev.Triggers) — e.g. an audio
//     classifier's own label ("doorbell"), for events with neither a
//     segment detection nor a non-motion/audio/clip type.
//  4. ev.Types[0], if Types is non-empty — the previous (buggy) behavior's
//     fallback, kept as a last resort so a motion-only event still reports
//     "motion" rather than "".
//  5. "" — no Types, no Triggers, no Segments at all.
func PrimaryLabel(ev DetectionEvent) string {
	if label := bestDetectionLabel(ev); label != "" {
		return label
	}
	for _, t := range ev.Types {
		if t != "motion" && t != "audio" && t != "clip" {
			return t
		}
	}
	for _, t := range ev.Triggers {
		if t.Label != "" {
			return t.Label
		}
	}
	if len(ev.Types) > 0 {
		return ev.Types[0]
	}
	return ""
}

// bestDetectionLabel returns the Label of the highest-Score EventDetection
// across every segment in ev.Segments, skipping any with an empty Label, or
// "" if there is no such candidate at all. Ties keep whichever candidate was
// seen first (stable, since ">" not ">=" only replaces on a strictly higher
// score) — segments/detections carry no other tie-break signal worth
// preferring one over another for this purpose.
func bestDetectionLabel(ev DetectionEvent) string {
	var bestLabel string
	var bestScore float64
	haveCandidate := false
	for _, seg := range ev.Segments {
		for _, d := range seg.Detections {
			if d.Label == "" {
				continue
			}
			if !haveCandidate || d.Score > bestScore {
				bestLabel = d.Label
				bestScore = d.Score
				haveCandidate = true
			}
		}
	}
	return bestLabel
}

// bestConfidence returns the highest confidence score across the event's
// triggers, detections, and attributes, for the indexed `confidence` column
// that Query's MinConfidence filter runs against directly in SQL.
func bestConfidence(ev DetectionEvent) float64 {
	var best float64
	for _, t := range ev.Triggers {
		if t.Score > best {
			best = t.Score
		}
	}
	for _, seg := range ev.Segments {
		for _, d := range seg.Detections {
			if d.Score > best {
				best = d.Score
			}
		}
		for _, a := range seg.Attributes {
			if a.Confidence > best {
				best = a.Confidence
			}
		}
	}
	return best
}

// bestBox returns the bounding box of the first detection that has one,
// across all of the event's segments, or nil if none do.
func bestBox(ev DetectionEvent) *sdk.BoundingBox {
	for _, seg := range ev.Segments {
		for _, d := range seg.Detections {
			if d.Box != nil {
				return d.Box
			}
		}
	}
	return nil
}

// SetThumbRef writes eventID's thumb_ref column to thumbRef — the on-disk
// path media.Generator (Task 11) persisted a generated primary JPEG
// thumbnail under, once it has actually written the file. Deliberately
// separate from Upsert (see Upsert's own doc comment: thumb_ref is owned by
// the thumbnail-persistence path, not the event-lifecycle upsert path, so a
// later lifecycle message for the same event never clobbers a thumb_ref set
// in the meantime). A no-op (not an error) when eventID doesn't match any
// row — the event may have been deleted by retention between generation
// starting and finishing.
func (s *EventStore) SetThumbRef(eventID, thumbRef string) error {
	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`UPDATE events SET thumb_ref = ? WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("store: prepare set thumb_ref: %w", err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, thumbRef); err != nil {
		return err
	}
	if err := stmt.BindText(2, eventID); err != nil {
		return err
	}
	if err := stmt.Exec(); err != nil {
		return fmt.Errorf("store: set thumb_ref for event %s: %w", eventID, err)
	}
	return nil
}

// GetThumbRef reads eventID's thumb_ref column back, for
// GetEventThumbnails' (rpc_events.go) fallback load of a generated
// thumbnail file. Returns ("", nil) — not an error — both when eventID
// doesn't exist and when it exists but thumb_ref is still NULL (no
// thumbnail generated yet, or generation found no covering segment): either
// way there is nothing to load, and the caller's handling is identical.
func (s *EventStore) GetThumbRef(eventID string) (string, error) {
	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(`SELECT thumb_ref FROM events WHERE id = ?`)
	if err != nil {
		return "", fmt.Errorf("store: prepare get thumb_ref: %w", err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, eventID); err != nil {
		return "", err
	}

	if !stmt.Step() {
		if err := stmt.Err(); err != nil {
			return "", fmt.Errorf("store: scan get thumb_ref: %w", err)
		}
		return "", nil
	}
	// ColumnText on a NULL thumb_ref returns "", matching this method's
	// documented "" zero value for "nothing generated yet".
	return stmt.ColumnText(0), nil
}

// DeletedEvent is one row DeleteOlderThan removed: just enough (ID, for
// cascading to any face/clip vector rows keyed by an event's id; ThumbRef,
// for removing its thumbnail file) for the retention task's cascade step.
// ThumbRef is "" for every row today — no code in this plugin populates
// thumb_ref yet (see Upsert's doc comment: that's a separate, still-unbuilt
// thumbnail-persistence task) — but DeleteOlderThan returns it regardless so
// the retention cascade needs no changes once that task lands.
type DeletedEvent struct {
	ID       string
	ThumbRef string
}

// DeleteOlderThan removes every event row for cameraID that has both fully
// ended and ended strictly before cutoffMs, and returns each removed row's
// id/thumb_ref for the caller (retention's cascade step) to remove its
// thumbnail file and any face/clip vector rows keyed by its id.
//
// "Fully ended" means end_ms > 0: sdk.DetectionEvent.EndTime is 0
// (omitempty) until an event's terminal lifecycle message, so an in-progress
// event's end_ms is 0 regardless of how old its start_ms is. Without this
// guard, `end_ms < cutoffMs` would be true for every still-active event too
// (0 is less than any positive cutoff), deleting events retention should
// never touch just because they started a while ago and haven't ended yet.
//
// Mirrors SegmentStore.DeleteOlderThan's read-then-delete-in-one-transaction
// shape, so the rows returned always match exactly what was removed.
func (s *EventStore) DeleteOlderThan(cameraID string, cutoffMs int64) ([]DeletedEvent, error) {
	s.db.Lock()
	defer s.db.Unlock()

	conn := s.db.Conn()

	if err := conn.Exec("BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("store: begin delete events older than: %w", err)
	}

	deleted, err := s.deletedEventsOlderThan(cameraID, cutoffMs)
	if err != nil {
		_ = conn.Exec("ROLLBACK")
		return nil, err
	}

	stmt, _, err := conn.Prepare(`DELETE FROM events WHERE camera_id = ? AND end_ms > 0 AND end_ms < ?`)
	if err != nil {
		_ = conn.Exec("ROLLBACK")
		return nil, fmt.Errorf("store: prepare delete events older than: %w", err)
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
		return nil, fmt.Errorf("store: delete events older than: %w", bindErr)
	}

	if err := conn.Exec("COMMIT"); err != nil {
		return nil, fmt.Errorf("store: commit delete events older than: %w", err)
	}
	return deleted, nil
}

// deletedEventsOlderThan returns the id/thumb_ref of the rows matching the
// same predicate used by DeleteOlderThan's DELETE, so the two stay in sync.
// Internal helper only called from DeleteOlderThan, which already holds the
// db lock (s.db.Lock()) — it must not lock again itself (sync.Mutex isn't
// reentrant).
func (s *EventStore) deletedEventsOlderThan(cameraID string, cutoffMs int64) ([]DeletedEvent, error) {
	stmt, _, err := s.db.Conn().Prepare(`
		SELECT id, thumb_ref FROM events WHERE camera_id = ? AND end_ms > 0 AND end_ms < ?`)
	if err != nil {
		return nil, fmt.Errorf("store: prepare select events older than: %w", err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, cameraID); err != nil {
		return nil, err
	}
	if err := stmt.BindInt64(2, cutoffMs); err != nil {
		return nil, err
	}

	var out []DeletedEvent
	for stmt.Step() {
		// ColumnText on a NULL thumb_ref (every row, until the later
		// thumbnail-persistence task starts populating it) returns "",
		// matching DeletedEvent.ThumbRef's documented "" zero value.
		out = append(out, DeletedEvent{ID: stmt.ColumnText(0), ThumbRef: stmt.ColumnText(1)})
	}
	if err := stmt.Err(); err != nil {
		return nil, fmt.Errorf("store: scan events older than: %w", err)
	}
	return out, nil
}

// Query returns events for cameraIDs (all cameras if empty, matching
// NVRInterface.getEvents' no-cameraIDs vs. getCameraEvents' cameraIDs
// distinction) matching opts, newest-first by start time, with HasMore
// reporting whether more rows exist past the returned page.
//
// Filters split two ways:
//   - CameraID/StartMs/EndMs/Before/MinConfidence/HasRecording run in SQL
//     against the indexed flat columns (camera_id, ts_ms, confidence,
//     has_recording), because they map directly onto a single column.
//   - Types/Triggers/TriggerLabels/Attributes/Search/HasDetections/State
//     don't: State has no dedicated column at all (see the package doc
//     below), and the others need the event's nested trigger/segment
//     structure that only the decoded raw JSON has. When any of these are
//     requested, Query fetches every SQL-matched row (skipping the SQL
//     LIMIT), decodes each one, filters in Go, and only then applies
//     Limit+1 pagination — correct at the event volumes a single-site local
//     NVR accumulates, but something to revisit (e.g. a JSON1 predicate or
//     dedicated columns) if that stops being true. When none of them are
//     requested, Query pushes `LIMIT <limit+1>` into the SQL itself, which
//     is the common case (Query with only pagination, MinConfidence, or a
//     time window) and avoids decoding rows that would just be discarded.
func (s *EventStore) Query(cameraIDs []string, opts GetEventsOptions) (GetEventsResult, error) {
	query, args, limit := buildEventsQuery(cameraIDs, opts)

	s.db.Lock()
	defer s.db.Unlock()

	stmt, _, err := s.db.Conn().Prepare(query)
	if err != nil {
		return GetEventsResult{}, fmt.Errorf("store: prepare query events: %w", err)
	}
	defer stmt.Close()

	if err := bindEventsQueryArgs(stmt, args); err != nil {
		return GetEventsResult{}, err
	}

	var rows []DetectionEvent
	for stmt.Step() {
		var ev DetectionEvent
		if err := json.Unmarshal([]byte(stmt.ColumnText(0)), &ev); err != nil {
			return GetEventsResult{}, fmt.Errorf("store: decode event raw json: %w", err)
		}
		rows = append(rows, ev)
	}
	if err := stmt.Err(); err != nil {
		return GetEventsResult{}, fmt.Errorf("store: scan events: %w", err)
	}

	if needsPostFilter(opts) {
		rows = filterEvents(rows, opts)
	}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	return GetEventsResult{Events: rows, HasMore: hasMore}, nil
}

// buildEventsQuery constructs the SQL text and positional bind args for
// Query, plus the effective page size (opts.Limit or defaultEventsLimit)
// the caller should slice/HasMore-check against. It only appends a SQL
// `LIMIT` clause (fetching limit+1 rows, per the brief) when no option
// requiring a Go-side post-filter pass is set; see Query's doc comment.
func buildEventsQuery(cameraIDs []string, opts GetEventsOptions) (string, []any, int) {
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
	if opts.Before != nil {
		clauses = append(clauses, "ts_ms < ?")
		args = append(args, *opts.Before)
	}
	if opts.MinConfidence != nil {
		clauses = append(clauses, "confidence >= ?")
		args = append(args, *opts.MinConfidence)
	}
	if opts.HasRecording != nil {
		clauses = append(clauses, "has_recording = ?")
		args = append(args, *opts.HasRecording)
	}

	query := "SELECT raw FROM events"
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY ts_ms DESC"

	limit := defaultEventsLimit
	if opts.Limit != nil && *opts.Limit > 0 {
		limit = int(*opts.Limit)
	}

	if !needsPostFilter(opts) {
		query += " LIMIT ?"
		args = append(args, limit+1)
	}

	return query, args, limit
}

// needsPostFilter reports whether opts has any filter that Query cannot
// express against the flat SQL columns and must instead apply in Go after
// decoding each row's raw JSON.
func needsPostFilter(opts GetEventsOptions) bool {
	return len(opts.Types) > 0 ||
		len(opts.Triggers) > 0 ||
		len(opts.TriggerLabels) > 0 ||
		len(opts.Attributes) > 0 ||
		opts.Search != "" ||
		opts.HasDetections != nil ||
		opts.State != ""
}

// bindEventsQueryArgs binds args (built by buildEventsQuery, always string,
// int64, float64, or bool) onto stmt in order.
func bindEventsQueryArgs(stmt *sqlite3.Stmt, args []any) error {
	for i, arg := range args {
		idx := i + 1
		var err error
		switch v := arg.(type) {
		case string:
			err = stmt.BindText(idx, v)
		case int64:
			err = stmt.BindInt64(idx, v)
		case int:
			err = stmt.BindInt64(idx, int64(v))
		case float64:
			err = stmt.BindFloat(idx, v)
		case bool:
			err = stmt.BindBool(idx, v)
		default:
			return fmt.Errorf("store: unsupported bind arg type %T", v)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// filterEvents applies every opts filter that needsPostFilter identified as
// requiring the decoded event structure, in Go.
func filterEvents(events []DetectionEvent, opts GetEventsOptions) []DetectionEvent {
	var out []DetectionEvent
	for _, ev := range events {
		if matchesFilters(ev, opts) {
			out = append(out, ev)
		}
	}
	return out
}

func matchesFilters(ev DetectionEvent, opts GetEventsOptions) bool {
	if opts.State != "" && ev.State != opts.State {
		return false
	}
	if len(opts.Types) > 0 && !hasAny(ev.Types, opts.Types) {
		return false
	}
	if len(opts.Triggers) > 0 {
		types := make([]string, 0, len(ev.Triggers))
		for _, t := range ev.Triggers {
			types = append(types, t.Type)
		}
		if !matchLogic(types, opts.Triggers, opts.FilterLogicTriggers) {
			return false
		}
	}
	if len(opts.TriggerLabels) > 0 {
		labels := make([]string, 0, len(ev.Triggers))
		for _, t := range ev.Triggers {
			if t.Label != "" {
				labels = append(labels, t.Label)
			}
		}
		if !hasAny(labels, opts.TriggerLabels) {
			return false
		}
	}
	if len(opts.Attributes) > 0 {
		var attrTypes []string
		for _, seg := range ev.Segments {
			for _, a := range seg.Attributes {
				attrTypes = append(attrTypes, a.Type)
			}
		}
		if !matchLogic(attrTypes, opts.Attributes, opts.FilterLogicAttributes) {
			return false
		}
	}
	// hasDetections is a two-state control from the frontend, NOT a strict
	// tri-state equality:
	//   true  -> restrict to object-detection events (person/vehicle/…);
	//   false -> the "detections-only" toggle is OFF, i.e. NO constraint.
	// The recordings label filter pairs hasDetections:false with an explicit
	// types:[...] (e.g. {"types":["person"],"hasDetections":false,...}) —
	// there "false" means "don't also require the generic detections flag",
	// not "exclude events that have detections". Interpreting false as an
	// equality wrongly dropped every person/vehicle/animal event the moment a
	// user picked a type chip, so only filter when hasDetections is true.
	if opts.HasDetections != nil && *opts.HasDetections && !EventHasDetections(ev) {
		return false
	}
	if opts.Search != "" && !matchesSearch(ev, opts.Search) {
		return false
	}
	return true
}

// hasAny reports whether haystack and needles share at least one element
// (membership / OR semantics).
func hasAny(haystack, needles []string) bool {
	set := make(map[string]struct{}, len(haystack))
	for _, h := range haystack {
		set[h] = struct{}{}
	}
	for _, n := range needles {
		if _, ok := set[n]; ok {
			return true
		}
	}
	return false
}

// hasAll reports whether haystack contains every element of needles (AND
// semantics).
func hasAll(haystack, needles []string) bool {
	set := make(map[string]struct{}, len(haystack))
	for _, h := range haystack {
		set[h] = struct{}{}
	}
	for _, n := range needles {
		if _, ok := set[n]; !ok {
			return false
		}
	}
	return true
}

// matchLogic dispatches to hasAll for logic == "and", hasAny otherwise
// (covers "or" and the unset/default case) — GetEventsOptions.FilterLogic*
// per the d.ts is 'and' | 'or' with 'or' implied when omitted.
func matchLogic(haystack, needles []string, logic string) bool {
	if logic == "and" {
		return hasAll(haystack, needles)
	}
	return hasAny(haystack, needles)
}

// EventHasDetections reports whether the event represents an object
// detection (person/vehicle/animal/package/face/etc.) as opposed to a
// motion-only or audio-only trigger.
//
// This mirrors the frontend's own client-side hasDetections predicate
// (compiled @camera.ui/nvr: `hasDetections && !types.some(t => t!=='motion'
// && t!=='audio')` hides an event) — i.e. an event "has detections" iff it
// carries at least one type other than "motion" and "audio". We key off
// ev.Types (not ev.Segments) deliberately: the core delivers detection
// events with an empty Segments slice (observed: segments=0 for both motion
// and object events), so the previous segment-based check filtered out ALL
// events under hasDetections:true — including the person/vehicle events the
// Recordings/home views default-request — leaving those views empty.
//
// Exported (was eventHasDetections) so events_ingest.go's push-notification
// gate (only notify on an actual object-detection event, never a
// motion-only or audio-only one) reuses this exact predicate rather than a
// second, potentially drifting copy of it.
func EventHasDetections(ev DetectionEvent) bool {
	for _, t := range ev.Types {
		if t != "motion" && t != "audio" {
			return true
		}
	}
	return false
}

// matchesSearch is a simple case-insensitive substring match against the
// event's camera id, detection types, trigger labels, and per-segment
// detection/attribute labels. Good enough for a local single-box NVR's
// event list search box; not a ranked or tokenized search (that's
// searchEventsByText's CLIP-embedding job, a different, later feature).
func matchesSearch(ev DetectionEvent, search string) bool {
	q := strings.ToLower(search)

	if strings.Contains(strings.ToLower(ev.CameraID), q) {
		return true
	}
	for _, t := range ev.Types {
		if strings.Contains(strings.ToLower(t), q) {
			return true
		}
	}
	for _, trig := range ev.Triggers {
		if strings.Contains(strings.ToLower(trig.Label), q) {
			return true
		}
	}
	for _, seg := range ev.Segments {
		for _, d := range seg.Detections {
			if strings.Contains(strings.ToLower(d.Label), q) {
				return true
			}
		}
		for _, a := range seg.Attributes {
			if strings.Contains(strings.ToLower(a.Label), q) {
				return true
			}
		}
	}
	return false
}
