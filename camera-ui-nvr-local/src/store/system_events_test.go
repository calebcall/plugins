package store

import "testing"

func openTestSystemEventStore(t *testing.T) *SystemEventStore {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewSystemEventStore(db)
}

// TestSystemEventStore_InsertAndQuery_NewestFirst proves Insert persists
// every field (including the optional Duration pointer) and Query returns
// rows newest-first.
func TestSystemEventStore_InsertAndQuery_NewestFirst(t *testing.T) {
	store := openTestSystemEventStore(t)

	dur := int64(5000)
	fixtures := []SystemEvent{
		{ID: "s1", Type: "recorder-start", Severity: "info", CameraID: "cam1", Timestamp: 1000, Message: "recorder started"},
		{ID: "s2", Type: "disk-critical", Severity: "error", CameraID: "cam2", Timestamp: 2000, Duration: &dur, Message: "disk full"},
	}
	if err := store.Insert(fixtures); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	result, err := store.Query(nil, GetSystemEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(result.Events), result.Events)
	}
	if result.Events[0].ID != "s2" || result.Events[1].ID != "s1" {
		t.Fatalf("expected newest-first [s2, s1], got [%s, %s]", result.Events[0].ID, result.Events[1].ID)
	}
	if result.Events[0].Duration == nil || *result.Events[0].Duration != 5000 {
		t.Fatalf("expected Duration=5000 preserved, got %+v", result.Events[0].Duration)
	}
	if result.Events[1].Duration != nil {
		t.Fatalf("expected nil Duration for s1, got %v", *result.Events[1].Duration)
	}
	if result.Events[0].Severity != "error" || result.Events[0].Message != "disk full" {
		t.Fatalf("unexpected fields on s2: %+v", result.Events[0])
	}
}

// TestSystemEventStore_Query_FiltersByCameraIDsAndWindow proves the
// cameraIDs and StartMs/EndMs filters both narrow results.
func TestSystemEventStore_Query_FiltersByCameraIDsAndWindow(t *testing.T) {
	store := openTestSystemEventStore(t)

	if err := store.Insert([]SystemEvent{
		{ID: "s1", Type: "t", Severity: "info", CameraID: "cam1", Timestamp: 1000, Message: "m1"},
		{ID: "s2", Type: "t", Severity: "info", CameraID: "cam2", Timestamp: 2000, Message: "m2"},
		{ID: "s3", Type: "t", Severity: "info", CameraID: "cam1", Timestamp: 3000, Message: "m3"},
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	result, err := store.Query([]string{"cam1"}, GetSystemEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("expected 2 cam1 events, got %d: %+v", len(result.Events), result.Events)
	}

	startMs := int64(1500)
	windowed, err := store.Query(nil, GetSystemEventsOptions{StartMs: &startMs})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(windowed.Events) != 2 {
		t.Fatalf("expected 2 events at/after ts 1500, got %d: %+v", len(windowed.Events), windowed.Events)
	}
}

// TestSystemEventStore_Query_EmptyStoreReturnsEmptyNotNil proves a fresh
// store returns a non-nil empty slice (matching the frontend's required
// SystemEvent[] shape) rather than nil/null.
func TestSystemEventStore_Query_EmptyStoreReturnsEmptyNotNil(t *testing.T) {
	store := openTestSystemEventStore(t)

	result, err := store.Query(nil, GetSystemEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if result.Events == nil {
		t.Fatalf("expected a non-nil empty slice, got nil")
	}
	if len(result.Events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(result.Events))
	}
}

// TestSystemEventStore_Query_PaginatesWithHasMore proves Limit+HasMore
// pagination works the same way EventStore.Query's does.
func TestSystemEventStore_Query_PaginatesWithHasMore(t *testing.T) {
	store := openTestSystemEventStore(t)

	if err := store.Insert([]SystemEvent{
		{ID: "s1", Type: "t", Severity: "info", Timestamp: 1000, Message: "m1"},
		{ID: "s2", Type: "t", Severity: "info", Timestamp: 2000, Message: "m2"},
		{ID: "s3", Type: "t", Severity: "info", Timestamp: 3000, Message: "m3"},
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	limit := int64(2)
	result, err := store.Query(nil, GetSystemEventsOptions{Limit: &limit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(result.Events))
	}
	if !result.HasMore {
		t.Fatalf("expected HasMore=true, got false")
	}
}
