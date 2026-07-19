package store

import (
	"reflect"
	"testing"
	"time"
)

// TestSegmentStore_AddAndInRange proves Add indexes a segment row and
// InRange returns only the segments overlapping the requested window, in
// ascending start_ms order.
func TestSegmentStore_AddAndInRange(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	segs := NewSegmentStore(db)

	// Inserted out of start_ms order to prove InRange sorts, not just
	// echoes insertion order.
	third := Segment{CameraID: "cam1", Role: "main", Path: "/rec/c.mp4", StartMs: 6000, EndMs: 7000, HasVideo: true, HasAudio: false, Codec: "h264"}
	first := Segment{CameraID: "cam1", Role: "main", Path: "/rec/a.mp4", StartMs: 1000, EndMs: 2000, HasVideo: true, HasAudio: true, Codec: "h264"}
	second := Segment{CameraID: "cam1", Role: "main", Path: "/rec/b.mp4", StartMs: 3000, EndMs: 4000, HasVideo: true, HasAudio: true, Codec: "hevc"}

	for _, seg := range []Segment{third, first, second} {
		id, err := segs.Add(seg)
		if err != nil {
			t.Fatalf("Add(%+v): %v", seg, err)
		}
		if id <= 0 {
			t.Fatalf("Add(%+v) returned non-positive id %d", seg, id)
		}
	}

	// Window [1500, 3500] overlaps "first" (1000-2000) and "second"
	// (3000-4000) but not "third" (6000-7000).
	got, err := segs.InRange("cam1", "main", 1500, 3500)
	if err != nil {
		t.Fatalf("InRange: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 segments, got %d: %+v", len(got), got)
	}
	if got[0].Path != "/rec/a.mp4" || got[1].Path != "/rec/b.mp4" {
		t.Errorf("expected [a.mp4, b.mp4] in start order, got [%s, %s]", got[0].Path, got[1].Path)
	}
	if got[0].StartMs != 1000 || got[0].EndMs != 2000 {
		t.Errorf("unexpected first segment bounds: %+v", got[0])
	}
	if !got[0].HasVideo || !got[0].HasAudio || got[0].Codec != "h264" {
		t.Errorf("unexpected first segment flags/codec: %+v", got[0])
	}
	if got[1].HasAudio != true || got[1].Codec != "hevc" {
		t.Errorf("unexpected second segment flags/codec: %+v", got[1])
	}
	if got[0].ID == 0 || got[1].ID == 0 {
		t.Errorf("expected populated IDs, got %+v", got)
	}
}

// TestSegmentStore_InRange_BoundaryStraddle proves a segment whose start or
// end exactly touches the query window's boundary is included (the overlap
// test is inclusive: end_ms >= startMs AND start_ms <= endMs), not excluded
// by an off-by-one.
func TestSegmentStore_InRange_BoundaryStraddle(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	segs := NewSegmentStore(db)

	seg := Segment{CameraID: "cam1", Role: "main", Path: "/rec/edge.mp4", StartMs: 1000, EndMs: 2000, HasVideo: true, Codec: "h264"}
	if _, err := segs.Add(seg); err != nil {
		t.Fatal(err)
	}

	// Window starts exactly at the segment's end_ms.
	got, err := segs.InRange("cam1", "main", 2000, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected segment straddling the window start boundary to be included, got %d results", len(got))
	}

	// Window ends exactly at the segment's start_ms.
	got, err = segs.InRange("cam1", "main", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected segment straddling the window end boundary to be included, got %d results", len(got))
	}

	// Window strictly outside the segment on both sides is excluded.
	got, err = segs.InRange("cam1", "main", 2001, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no overlap past the boundary, got %d results", len(got))
	}
}

// TestSegmentStore_InRange_FiltersByCameraAndRole proves InRange scopes its
// query to the requested camera and role, not just the time window.
func TestSegmentStore_InRange_FiltersByCameraAndRole(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	segs := NewSegmentStore(db)

	for _, seg := range []Segment{
		{CameraID: "cam1", Role: "main", Path: "/rec/main.mp4", StartMs: 1000, EndMs: 2000},
		{CameraID: "cam1", Role: "sub", Path: "/rec/sub.mp4", StartMs: 1000, EndMs: 2000},
		{CameraID: "cam2", Role: "main", Path: "/rec/other-camera.mp4", StartMs: 1000, EndMs: 2000},
	} {
		if _, err := segs.Add(seg); err != nil {
			t.Fatal(err)
		}
	}

	got, err := segs.InRange("cam1", "main", 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "/rec/main.mp4" {
		t.Fatalf("expected only cam1/main segment, got %+v", got)
	}
}

// dayMs returns the UTC start-of-day-ish epoch millisecond timestamp for a
// given date, used to seed segments for TestSegmentStore_Days.
func dayMs(year, month, day int) int64 {
	return time.Date(year, time.Month(month), day, 12, 0, 0, 0, time.UTC).UnixMilli()
}

// TestSegmentStore_Days proves Days returns the distinct, deduped, sorted
// set of "YYYY-MM-DD" strings (derived from start_ms as UTC) for a camera,
// filtered to the requested year/month.
func TestSegmentStore_Days(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	segs := NewSegmentStore(db)

	for _, seg := range []Segment{
		// Two segments on the same day: must dedupe to one entry.
		{CameraID: "cam1", Role: "main", Path: "/rec/1.mp4", StartMs: dayMs(2024, 1, 16)},
		{CameraID: "cam1", Role: "main", Path: "/rec/2.mp4", StartMs: dayMs(2024, 1, 16)},
		// Earlier day, same month: included, and must sort before 1-16.
		{CameraID: "cam1", Role: "main", Path: "/rec/3.mp4", StartMs: dayMs(2024, 1, 5)},
		// Different month: excluded from a Jan 2024 query.
		{CameraID: "cam1", Role: "main", Path: "/rec/4.mp4", StartMs: dayMs(2024, 2, 1)},
		// Different camera: excluded regardless of date.
		{CameraID: "cam2", Role: "main", Path: "/rec/5.mp4", StartMs: dayMs(2024, 1, 16)},
	} {
		if _, err := segs.Add(seg); err != nil {
			t.Fatal(err)
		}
	}

	got, err := segs.Days("cam1", 2024, 1)
	if err != nil {
		t.Fatalf("Days: %v", err)
	}

	want := []string{"2024-01-05", "2024-01-16"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Days(cam1, 2024, 1) = %v, want %v", got, want)
	}
}

// TestSegmentStore_DeleteOlderThan proves DeleteOlderThan removes only the
// rows for the requested camera whose segments end before the cutoff, and
// returns their file paths so a later retention task can remove the files
// themselves (this method only touches the DB rows).
func TestSegmentStore_DeleteOlderThan(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	segs := NewSegmentStore(db)

	old := Segment{CameraID: "cam1", Role: "main", Path: "/rec/old.mp4", StartMs: 1000, EndMs: 2000}
	recent := Segment{CameraID: "cam1", Role: "main", Path: "/rec/recent.mp4", StartMs: 9000, EndMs: 10000}
	otherCameraOld := Segment{CameraID: "cam2", Role: "main", Path: "/rec/other-old.mp4", StartMs: 1000, EndMs: 2000}

	for _, seg := range []Segment{old, recent, otherCameraOld} {
		if _, err := segs.Add(seg); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := segs.DeleteOlderThan("cam1", 5000)
	if err != nil {
		t.Fatalf("DeleteOlderThan: %v", err)
	}
	if !reflect.DeepEqual(removed, []string{"/rec/old.mp4"}) {
		t.Errorf("removed paths = %v, want [/rec/old.mp4]", removed)
	}

	remainingCam1, err := segs.InRange("cam1", "main", 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(remainingCam1) != 1 || remainingCam1[0].Path != "/rec/recent.mp4" {
		t.Fatalf("expected only the recent cam1 segment to remain, got %+v", remainingCam1)
	}

	remainingCam2, err := segs.InRange("cam2", "main", 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(remainingCam2) != 1 {
		t.Fatalf("expected the other camera's old segment to be untouched, got %+v", remainingCam2)
	}
}

// TestSegmentStore_AllByCamera proves AllByCamera returns every segment for
// the requested camera, across every role, ordered oldest (start_ms) first,
// and excludes other cameras' segments.
func TestSegmentStore_AllByCamera(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	segs := NewSegmentStore(db)

	for _, seg := range []Segment{
		{CameraID: "cam1", Role: "sub", Path: "/rec/c.mp4", StartMs: 5000, EndMs: 6000},
		{CameraID: "cam1", Role: "main", Path: "/rec/a.mp4", StartMs: 1000, EndMs: 2000},
		{CameraID: "cam1", Role: "main", Path: "/rec/b.mp4", StartMs: 3000, EndMs: 4000},
		{CameraID: "cam2", Role: "main", Path: "/rec/other.mp4", StartMs: 500, EndMs: 900},
	} {
		if _, err := segs.Add(seg); err != nil {
			t.Fatal(err)
		}
	}

	got, err := segs.AllByCamera("cam1")
	if err != nil {
		t.Fatalf("AllByCamera: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 segments for cam1, got %d: %+v", len(got), got)
	}
	if got[0].Path != "/rec/a.mp4" || got[1].Path != "/rec/b.mp4" || got[2].Path != "/rec/c.mp4" {
		t.Fatalf("expected oldest-first [a,b,c] across roles, got [%s,%s,%s]", got[0].Path, got[1].Path, got[2].Path)
	}
}
