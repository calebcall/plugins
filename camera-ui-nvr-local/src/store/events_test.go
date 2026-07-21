package store

import (
	"testing"

	sdk "github.com/cameraui/sdk/go"
)

// newTestEvent builds a minimal-but-valid DetectionEvent for test fixtures:
// id/cameraID/startMs are the axes the pagination/filter tests vary, score
// is folded into a single trigger so bestConfidence/MinConfidence filtering
// has something to compare against, and eventType lets state-filter tests
// exercise both "active" and "ended".
func newTestEvent(id, cameraID string, startMs int64, score float64, state string, types ...string) DetectionEvent {
	return DetectionEvent{
		ID:         id,
		CameraID:   cameraID,
		State:      state,
		StartTime:  startMs,
		LastUpdate: startMs,
		Types:      types,
		Triggers: []sdk.EventTrigger{
			{Type: sdk.EventTriggerMotion, Score: score, FirstSeen: startMs, LastSeen: startMs},
		},
	}
}

func openTestEventStore(t *testing.T) *EventStore {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewEventStore(db)
}

// TestEventStore_Query_HasDetectionsIsTypeBased locks in the fix for the
// filter that left the Recordings/home/timeline-detections views empty:
// hasDetections keys off event Types (object type present), NOT ev.Segments
// (the core delivers detection events with empty Segments). person1 has NO
// segments yet must still pass hasDetections=true; motion/audio-only must not.
func TestEventStore_Query_HasDetectionsIsTypeBased(t *testing.T) {
	events := openTestEventStore(t)
	if err := events.Upsert([]DetectionEvent{
		newTestEvent("motion1", "cam", 1000, 0.9, "ended", "motion"),
		newTestEvent("person1", "cam", 2000, 0.9, "ended", "person"),
		newTestEvent("audio1", "cam", 3000, 0.9, "ended", "audio"),
	}); err != nil {
		t.Fatal(err)
	}

	yes := true
	got, err := events.Query(nil, GetEventsOptions{HasDetections: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 || got.Events[0].ID != "person1" {
		t.Fatalf("hasDetections=true expected only person1 (object type, empty Segments), got %d events", len(got.Events))
	}

	// hasDetections=false is the frontend's "detections-only toggle OFF"
	// signal, i.e. NO constraint — not "only trigger-only events". It must
	// return every event (object AND motion/audio). The recordings label
	// filter relies on this: it sends hasDetections:false alongside an
	// explicit types:[...] (see TestEventStore_Query_LabelFilterWithHasDetectionsFalse).
	no := false
	all, err := events.Query(nil, GetEventsOptions{HasDetections: &no})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Events) != 3 {
		t.Fatalf("hasDetections=false is a no-op constraint; expected all 3 events, got %d", len(all.Events))
	}
}

// TestEventStore_Query_LabelFilterWithHasDetectionsFalse reproduces the exact
// shape the frontend Recordings page sends when a user picks a label chip
// (observed live: {"types":["person"],"hasDetections":false,"minConfidence":
// 0.5,"hasRecording":true,"state":"ended"}). The person event, which HAS
// detections, must be returned: hasDetections:false there means "don't also
// require the generic detections flag", not "exclude events with detections".
// Before the fix, our filter read false as a strict equality and dropped
// every person/vehicle/animal event whenever a label chip was selected.
func TestEventStore_Query_LabelFilterWithHasDetectionsFalse(t *testing.T) {
	events := openTestEventStore(t)
	if err := events.Upsert([]DetectionEvent{
		newTestEvent("motion1", "cam", 1000, 0.9, "ended", "motion"),
		newTestEvent("person1", "cam", 2000, 0.9, "ended", "person"),
		newTestEvent("vehicle1", "cam", 3000, 0.9, "ended", "vehicle"),
	}); err != nil {
		t.Fatal(err)
	}

	no := false
	minConf := 0.5
	got, err := events.Query(nil, GetEventsOptions{
		Types:         []string{"person"},
		HasDetections: &no,
		MinConfidence: &minConf,
		State:         "ended",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 || got.Events[0].ID != "person1" {
		t.Fatalf("label filter types=[person] hasDetections=false expected only person1, got %d events", len(got.Events))
	}
}

// TestEventStore_Query_NewestFirstPaginatedWithHasMore is the brief's
// required proof: 5 events across 2 cameras/timestamps, Query with Limit=2 +
// Before=<ts> returns 2 rows newest-first with HasMore=true.
func TestEventStore_Query_NewestFirstPaginatedWithHasMore(t *testing.T) {
	events := openTestEventStore(t)

	fixtures := []DetectionEvent{
		newTestEvent("e1", "cam1", 1000, 0.5, sdk.DetectionEventStateEnded, "motion"),
		newTestEvent("e2", "cam2", 2000, 0.5, sdk.DetectionEventStateEnded, "person"),
		newTestEvent("e3", "cam1", 3000, 0.5, sdk.DetectionEventStateEnded, "motion"),
		newTestEvent("e4", "cam2", 4000, 0.5, sdk.DetectionEventStateEnded, "vehicle"),
		newTestEvent("e5", "cam1", 5000, 0.5, sdk.DetectionEventStateEnded, "motion"),
	}
	if err := events.Upsert(fixtures); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	before := int64(5000) // exclude e5
	limit := int64(2)
	result, err := events.Query(nil, GetEventsOptions{Limit: &limit, Before: &before})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if len(result.Events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(result.Events), result.Events)
	}
	if result.Events[0].ID != "e4" || result.Events[1].ID != "e3" {
		t.Fatalf("expected newest-first [e4, e3], got [%s, %s]", result.Events[0].ID, result.Events[1].ID)
	}
	if !result.HasMore {
		t.Fatalf("expected HasMore=true (e2/e1 remain beyond the page), got false")
	}
}

// TestEventStore_Query_NoCameraFilterReturnsAllCameras proves an empty/nil
// cameraIDs argument (the getEvents, as opposed to getCameraEvents, shape)
// returns events across every camera rather than none.
func TestEventStore_Query_NoCameraFilterReturnsAllCameras(t *testing.T) {
	events := openTestEventStore(t)

	if err := events.Upsert([]DetectionEvent{
		newTestEvent("e1", "cam1", 1000, 0.5, sdk.DetectionEventStateEnded, "motion"),
		newTestEvent("e2", "cam2", 2000, 0.5, sdk.DetectionEventStateEnded, "motion"),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	result, err := events.Query(nil, GetEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("expected 2 events across both cameras, got %d", len(result.Events))
	}
}

// TestEventStore_Query_FiltersByCameraIDs proves a non-empty cameraIDs
// argument scopes the result to just those cameras (the getCameraEvents
// shape).
func TestEventStore_Query_FiltersByCameraIDs(t *testing.T) {
	events := openTestEventStore(t)

	if err := events.Upsert([]DetectionEvent{
		newTestEvent("e1", "cam1", 1000, 0.5, sdk.DetectionEventStateEnded, "motion"),
		newTestEvent("e2", "cam2", 2000, 0.5, sdk.DetectionEventStateEnded, "motion"),
		newTestEvent("e3", "cam3", 3000, 0.5, sdk.DetectionEventStateEnded, "motion"),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	result, err := events.Query([]string{"cam1", "cam3"}, GetEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(result.Events), result.Events)
	}
	for _, ev := range result.Events {
		if ev.CameraID == "cam2" {
			t.Fatalf("expected cam2 to be excluded, got %+v", ev)
		}
	}
}

// TestEventStore_Upsert_SameIDUpdatesInsteadOfDuplicating proves re-Upsert
// of an existing id (e.g. a detection event's 'update'/'end' message
// following its 'start') replaces the row in place rather than creating a
// second one, and that the replacement's fields (here: State) actually take
// effect.
func TestEventStore_Upsert_SameIDUpdatesInsteadOfDuplicating(t *testing.T) {
	events := openTestEventStore(t)

	if err := events.Upsert([]DetectionEvent{
		newTestEvent("e1", "cam1", 1000, 0.5, sdk.DetectionEventStateActive, "motion"),
	}); err != nil {
		t.Fatalf("Upsert (start): %v", err)
	}
	if err := events.Upsert([]DetectionEvent{
		newTestEvent("e1", "cam1", 1000, 0.9, sdk.DetectionEventStateEnded, "motion", "person"),
	}); err != nil {
		t.Fatalf("Upsert (end): %v", err)
	}

	result, err := events.Query(nil, GetEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("expected exactly 1 row after re-upserting the same id, got %d: %+v", len(result.Events), result.Events)
	}
	if result.Events[0].State != sdk.DetectionEventStateEnded {
		t.Fatalf("expected the update to have taken effect (state=ended), got %+v", result.Events[0])
	}
	if len(result.Events[0].Types) != 2 {
		t.Fatalf("expected the update's Types to have taken effect, got %+v", result.Events[0].Types)
	}
}

// TestEventStore_Query_FiltersByTypes proves Types filtering is at least
// membership: an event is included if it has any type in opts.Types.
func TestEventStore_Query_FiltersByTypes(t *testing.T) {
	events := openTestEventStore(t)

	if err := events.Upsert([]DetectionEvent{
		newTestEvent("e-person", "cam1", 1000, 0.5, sdk.DetectionEventStateEnded, "person"),
		newTestEvent("e-vehicle", "cam1", 2000, 0.5, sdk.DetectionEventStateEnded, "vehicle"),
		newTestEvent("e-animal", "cam1", 3000, 0.5, sdk.DetectionEventStateEnded, "animal"),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	result, err := events.Query(nil, GetEventsOptions{Types: []string{"person", "vehicle"}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("expected 2 events (person, vehicle), got %d: %+v", len(result.Events), result.Events)
	}
	for _, ev := range result.Events {
		if ev.ID == "e-animal" {
			t.Fatalf("expected e-animal to be excluded by the Types filter, got %+v", result.Events)
		}
	}
}

// TestEventStore_Query_FiltersByMinConfidence proves MinConfidence excludes
// events whose best score falls below the threshold.
func TestEventStore_Query_FiltersByMinConfidence(t *testing.T) {
	events := openTestEventStore(t)

	if err := events.Upsert([]DetectionEvent{
		newTestEvent("low", "cam1", 1000, 0.2, sdk.DetectionEventStateEnded, "motion"),
		newTestEvent("high", "cam1", 2000, 0.9, sdk.DetectionEventStateEnded, "motion"),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	minConfidence := 0.5
	result, err := events.Query(nil, GetEventsOptions{MinConfidence: &minConfidence})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Events) != 1 || result.Events[0].ID != "high" {
		t.Fatalf("expected only the high-confidence event, got %+v", result.Events)
	}
}

// TestEventStore_Query_FiltersByTimeWindow proves StartMs/EndMs scope the
// result to events whose start time falls in [startMs, endMs].
func TestEventStore_Query_FiltersByTimeWindow(t *testing.T) {
	events := openTestEventStore(t)

	if err := events.Upsert([]DetectionEvent{
		newTestEvent("before", "cam1", 1000, 0.5, sdk.DetectionEventStateEnded, "motion"),
		newTestEvent("inside", "cam1", 5000, 0.5, sdk.DetectionEventStateEnded, "motion"),
		newTestEvent("after", "cam1", 9000, 0.5, sdk.DetectionEventStateEnded, "motion"),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	startMs, endMs := int64(2000), int64(8000)
	result, err := events.Query(nil, GetEventsOptions{StartMs: &startMs, EndMs: &endMs})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Events) != 1 || result.Events[0].ID != "inside" {
		t.Fatalf("expected only the event inside the window, got %+v", result.Events)
	}
}

// TestEventStore_Query_FiltersByState proves State scopes the result to
// events in that lifecycle state.
func TestEventStore_Query_FiltersByState(t *testing.T) {
	events := openTestEventStore(t)

	if err := events.Upsert([]DetectionEvent{
		newTestEvent("active-1", "cam1", 1000, 0.5, sdk.DetectionEventStateActive, "motion"),
		newTestEvent("ended-1", "cam1", 2000, 0.5, sdk.DetectionEventStateEnded, "motion"),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	result, err := events.Query(nil, GetEventsOptions{State: sdk.DetectionEventStateActive})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Events) != 1 || result.Events[0].ID != "active-1" {
		t.Fatalf("expected only the active event, got %+v", result.Events)
	}
}

// TestEventStore_Upsert_RoundTripsFullEvent proves the full DetectionEvent
// (thumbnail bytes, nested segments/triggers) round-trips through the raw
// JSON column unchanged, not just the flat indexed columns.
func TestEventStore_Upsert_RoundTripsFullEvent(t *testing.T) {
	events := openTestEventStore(t)

	original := DetectionEvent{
		ID:         "full",
		CameraID:   "cam1",
		State:      sdk.DetectionEventStateEnded,
		StartTime:  1000,
		EndTime:    4000,
		LastUpdate: 4000,
		Types:      []string{"person"},
		Thumbnail:  []byte{0xFF, 0xD8, 0xFF},
		Triggers: []sdk.EventTrigger{
			{Type: sdk.EventTriggerMotion, Score: 0.8, FirstSeen: 1000, LastSeen: 4000},
		},
		Segments: []sdk.EventSegment{
			{
				FirstSeen: 1000,
				LastSeen:  4000,
				Detections: []sdk.EventDetection{
					{Label: "person", Score: 0.95, MaxCount: 1, Box: &sdk.BoundingBox{}},
				},
				Attributes: []sdk.EventAttribute{
					{Type: "face", Label: "unknown", Confidence: 0.7},
				},
			},
		},
		HasRecording: true,
	}

	if err := events.Upsert([]DetectionEvent{original}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	result, err := events.Query(nil, GetEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(result.Events))
	}

	got := result.Events[0]
	if got.ID != original.ID || got.EndTime != original.EndTime || len(got.Segments) != 1 {
		t.Fatalf("round-tripped event doesn't match: got %+v, want %+v", got, original)
	}
	if len(got.Thumbnail) != len(original.Thumbnail) {
		t.Fatalf("expected thumbnail bytes to round-trip, got %v", got.Thumbnail)
	}
	if len(got.Segments[0].Detections) != 1 || got.Segments[0].Detections[0].Label != "person" {
		t.Fatalf("expected nested segment detections to round-trip, got %+v", got.Segments[0])
	}
	if !got.HasRecording {
		t.Fatalf("expected HasRecording to round-trip as true")
	}
}

// setThumbRef writes id's thumb_ref column directly (white-box: no
// production code sets this column yet — see Upsert's doc comment), so
// TestEventStore_DeleteOlderThan can prove DeleteOlderThan returns it.
func setThumbRef(t *testing.T, db *DB, id, thumbRef string) {
	t.Helper()
	stmt, _, err := db.Conn().Prepare(`UPDATE events SET thumb_ref = ? WHERE id = ?`)
	if err != nil {
		t.Fatalf("prepare set thumb_ref: %v", err)
	}
	defer stmt.Close()
	if err := stmt.BindText(1, thumbRef); err != nil {
		t.Fatal(err)
	}
	if err := stmt.BindText(2, id); err != nil {
		t.Fatal(err)
	}
	if err := stmt.Exec(); err != nil {
		t.Fatalf("set thumb_ref: %v", err)
	}
}

// endedEvent builds a DetectionEvent that has already ended at endMs (unlike
// newTestEvent, which leaves EndTime unset) — DeleteOlderThan's cutoff only
// ever considers ended events.
func endedEvent(id, cameraID string, startMs, endMs int64) DetectionEvent {
	ev := newTestEvent(id, cameraID, startMs, 0.5, "ended", "motion")
	ev.EndTime = endMs
	return ev
}

// TestEventStore_DeleteOlderThan proves DeleteOlderThan removes only the
// fully-ended rows for the requested camera whose end_ms falls before the
// cutoff, leaves an active (EndTime==0) event untouched regardless of how
// old its start time is, leaves another camera's equally-old event
// untouched, and returns each removed row's id/thumb_ref for the caller's
// cascade step.
func TestEventStore_DeleteOlderThan(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	events := NewEventStore(db)

	old := endedEvent("old", "cam1", 1000, 2000)
	recent := endedEvent("recent", "cam1", 9000, 10000)
	stillActive := endedEvent("active", "cam1", 500, 0) // EndTime reset below
	stillActive.EndTime = 0
	otherCameraOld := endedEvent("other-old", "cam2", 1000, 2000)

	if err := events.Upsert([]DetectionEvent{old, recent, stillActive, otherCameraOld}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	setThumbRef(t, db, "old", "/thumbs/old.jpg")

	deleted, err := events.DeleteOlderThan("cam1", 5000)
	if err != nil {
		t.Fatalf("DeleteOlderThan: %v", err)
	}
	if len(deleted) != 1 || deleted[0].ID != "old" || deleted[0].ThumbRef != "/thumbs/old.jpg" {
		t.Fatalf("expected exactly [{old /thumbs/old.jpg}], got %+v", deleted)
	}

	remaining, err := events.Query([]string{"cam1"}, GetEventsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	remainingIDs := map[string]bool{}
	for _, ev := range remaining.Events {
		remainingIDs[ev.ID] = true
	}
	if remainingIDs["old"] {
		t.Errorf("expected old event to be deleted")
	}
	if !remainingIDs["recent"] {
		t.Errorf("expected recent event (end_ms >= cutoff) to remain")
	}
	if !remainingIDs["active"] {
		t.Errorf("expected still-active (EndTime==0) event to remain regardless of age")
	}

	remainingCam2, err := events.Query([]string{"cam2"}, GetEventsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remainingCam2.Events) != 1 || remainingCam2.Events[0].ID != "other-old" {
		t.Fatalf("expected other camera's equally-old event to be untouched, got %+v", remainingCam2.Events)
	}
}
