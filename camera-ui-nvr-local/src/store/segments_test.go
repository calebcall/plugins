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

// TestSegmentStore_CoversRange proves CoversRange reports true exactly when
// some segment for cameraID (regardless of role) overlaps the requested
// window, and false both when the camera has no segments at all and when
// its segments exist but don't overlap the window.
func TestSegmentStore_CoversRange(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	segs := NewSegmentStore(db)

	if _, err := segs.Add(Segment{CameraID: "cam1", Role: "high-resolution", Path: "/rec/a.mp4", StartMs: 1000, EndMs: 2000, HasVideo: true}); err != nil {
		t.Fatal(err)
	}

	covered, err := segs.CoversRange("cam1", 1500, 1600)
	if err != nil {
		t.Fatalf("CoversRange: %v", err)
	}
	if !covered {
		t.Fatalf("expected a window fully inside the segment to be covered")
	}

	// A point-in-time check (startMs == endMs) inside the segment.
	covered, err = segs.CoversRange("cam1", 1500, 1500)
	if err != nil {
		t.Fatalf("CoversRange: %v", err)
	}
	if !covered {
		t.Fatalf("expected a point-in-time check inside the segment to be covered")
	}

	// A window that only partially overlaps still counts.
	covered, err = segs.CoversRange("cam1", 1900, 2500)
	if err != nil {
		t.Fatalf("CoversRange: %v", err)
	}
	if !covered {
		t.Fatalf("expected a partially overlapping window to be covered")
	}

	// A window strictly after the segment ends is not covered.
	covered, err = segs.CoversRange("cam1", 3000, 4000)
	if err != nil {
		t.Fatalf("CoversRange: %v", err)
	}
	if covered {
		t.Fatalf("expected a window strictly after the segment to be uncovered")
	}

	// A camera with no segments at all is not covered.
	covered, err = segs.CoversRange("cam-unknown", 1000, 2000)
	if err != nil {
		t.Fatalf("CoversRange: %v", err)
	}
	if covered {
		t.Fatalf("expected an unknown camera to be uncovered")
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

// TestSegmentStore_CoveringSegmentForRole proves CoveringSegmentForRole
// scopes its lookup to exactly the requested role (unlike CoveringSegment,
// which ranks across every role) and reports ok=false, with no error, when
// nothing covers the timestamp for that role.
func TestSegmentStore_CoveringSegmentForRole(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	segs := NewSegmentStore(db)

	for _, seg := range []Segment{
		{CameraID: "cam1", Role: "high-resolution", Path: "/rec/high.mp4", StartMs: 1000, EndMs: 2000},
		{CameraID: "cam1", Role: "low-resolution", Path: "/rec/low.mp4", StartMs: 1000, EndMs: 2000},
	} {
		if _, err := segs.Add(seg); err != nil {
			t.Fatal(err)
		}
	}

	got, ok, err := segs.CoveringSegmentForRole("cam1", "high-resolution", 1500)
	if err != nil {
		t.Fatalf("CoveringSegmentForRole: %v", err)
	}
	if !ok {
		t.Fatalf("expected a covering high-resolution segment")
	}
	if got.Path != "/rec/high.mp4" {
		t.Errorf("expected the high-resolution segment, got %+v", got)
	}

	got, ok, err = segs.CoveringSegmentForRole("cam1", "mid-resolution", 1500)
	if err != nil {
		t.Fatalf("CoveringSegmentForRole: %v", err)
	}
	if ok {
		t.Fatalf("expected no covering mid-resolution segment, got %+v", got)
	}

	_, ok, err = segs.CoveringSegmentForRole("cam1", "high-resolution", 5000)
	if err != nil {
		t.Fatalf("CoveringSegmentForRole: %v", err)
	}
	if ok {
		t.Fatalf("expected no covering segment for an out-of-range timestamp")
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

// TestSegmentStore_AllPaths proves AllPaths returns the path of every
// indexed segment row, across every camera and role — the "known to be
// indexed" set retention's orphan sweep (recorder/retention.go,
// sweepOrphanFiles) diffs disk contents against.
func TestSegmentStore_AllPaths(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	segs := NewSegmentStore(db)

	want := map[string]bool{
		"/rec/cam1/a.mp4": true,
		"/rec/cam1/b.mp4": true,
		"/rec/cam2/c.mp4": true,
	}
	for path := range want {
		if _, err := segs.Add(Segment{CameraID: "cam1", Role: "main", Path: path, StartMs: 1000, EndMs: 2000}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := segs.AllPaths()
	if err != nil {
		t.Fatalf("AllPaths: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d paths, got %d: %v", len(want), len(got), got)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected path %q returned by AllPaths", p)
		}
		delete(want, p)
	}
	if len(want) != 0 {
		t.Errorf("AllPaths missing expected paths: %v", want)
	}
}

// TestSegmentStore_AllPaths_EmptyStoreReturnsNoError proves AllPaths on a
// store with no segment rows at all returns (nil, nil) rather than an error.
func TestSegmentStore_AllPaths_EmptyStoreReturnsNoError(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	got, err := NewSegmentStore(db).AllPaths()
	if err != nil {
		t.Fatalf("AllPaths: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no paths, got %v", got)
	}
}

// TestSegmentStore_HasPath proves HasPath reports true for exactly the
// path(s) with a row in the segments table, false for anything else — the
// fresh, un-cached, point-in-time check retention's orphan sweep
// (recorder/retention.go, sweepOrphanFiles) relies on to close its TOCTOU
// window (see HasPath's own doc comment).
func TestSegmentStore_HasPath(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	segs := NewSegmentStore(db)

	if _, err := segs.Add(Segment{CameraID: "cam1", Role: "main", Path: "/rec/cam1/a.mp4", StartMs: 1000, EndMs: 2000}); err != nil {
		t.Fatal(err)
	}

	has, err := segs.HasPath("/rec/cam1/a.mp4")
	if err != nil {
		t.Fatalf("HasPath: %v", err)
	}
	if !has {
		t.Errorf("expected HasPath to report true for an indexed path")
	}

	has, err = segs.HasPath("/rec/cam1/never-indexed.mp4")
	if err != nil {
		t.Fatalf("HasPath: %v", err)
	}
	if has {
		t.Errorf("expected HasPath to report false for a path with no row")
	}
}

// TestSegmentStore_HasPath_ReflectsRowsAddedAfterAnEarlierListing proves
// HasPath is a fresh, un-cached check: a path added AFTER an earlier
// AllPaths() call already ran is reported as present by a subsequent
// HasPath call — the exact freshness property retention's orphan sweep
// depends on to avoid deleting a segment finalized/indexed after its own
// AllPaths() snapshot was taken.
func TestSegmentStore_HasPath_ReflectsRowsAddedAfterAnEarlierListing(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	segs := NewSegmentStore(db)

	before, err := segs.AllPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatalf("expected an empty initial listing, got %v", before)
	}

	if _, err := segs.Add(Segment{CameraID: "cam1", Role: "main", Path: "/rec/cam1/late.mp4", StartMs: 1000, EndMs: 2000}); err != nil {
		t.Fatal(err)
	}

	has, err := segs.HasPath("/rec/cam1/late.mp4")
	if err != nil {
		t.Fatalf("HasPath: %v", err)
	}
	if !has {
		t.Errorf("expected HasPath to see a row added after the earlier AllPaths() snapshot")
	}
}

// TestSegmentStore_DistinctCameraIDs proves DistinctCameraIDs returns every
// camera with at least one indexed segment, deduplicated and sorted, and an
// empty (non-nil) slice for a store with no segments at all.
func TestSegmentStore_DistinctCameraIDs(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	segs := NewSegmentStore(db)

	empty, err := segs.DistinctCameraIDs()
	if err != nil {
		t.Fatalf("DistinctCameraIDs (empty store): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected 0 camera ids on an empty store, got %v", empty)
	}

	for _, seg := range []Segment{
		{CameraID: "cam2", Role: "main", Path: "/rec/a.mp4", StartMs: 1000, EndMs: 2000},
		{CameraID: "cam1", Role: "main", Path: "/rec/b.mp4", StartMs: 3000, EndMs: 4000},
		{CameraID: "cam2", Role: "sub", Path: "/rec/c.mp4", StartMs: 5000, EndMs: 6000},
	} {
		if _, err := segs.Add(seg); err != nil {
			t.Fatal(err)
		}
	}

	ids, err := segs.DistinctCameraIDs()
	if err != nil {
		t.Fatalf("DistinctCameraIDs: %v", err)
	}
	if len(ids) != 2 || ids[0] != "cam1" || ids[1] != "cam2" {
		t.Fatalf("expected deduplicated sorted [cam1, cam2], got %v", ids)
	}
}
