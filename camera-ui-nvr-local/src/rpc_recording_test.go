package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/recorder"
	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// --- getRecordingDays --------------------------------------------------------

func TestGetRecordingDays_DelegatesToSegmentStore(t *testing.T) {
	p := newTestPluginWithDB(t)

	// 2026-07-05 00:00:00 UTC in ms, well within July 2026.
	startMs := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC).UnixMilli()
	if _, err := p.segments.Add(store.Segment{CameraID: "cam1", Role: "high-resolution", Path: "/a", StartMs: startMs, EndMs: startMs + 60000}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	days, err := p.GetRecordingDays("cam1", 2026, 7)
	if err != nil {
		t.Fatalf("GetRecordingDays: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("expected 1 day, got %v", days)
	}
}

func TestGetRecordingDays_NoStoreReturnsEmptyNotNil(t *testing.T) {
	p := &NVRPlugin{recorder: recorder.NewRecorderManager()}
	days, err := p.GetRecordingDays("cam1", 2026, 7)
	if err != nil {
		t.Fatalf("GetRecordingDays: %v", err)
	}
	if days == nil {
		t.Fatalf("expected a non-nil empty slice, got nil")
	}
}

// --- getRecordingSegments / mergeSegments -----------------------------------

func TestGetRecordingSegments_MergesTouchingAndOverlapping_LeavesGapsSeparate(t *testing.T) {
	p := newTestPluginWithDB(t)

	// Inserted out of order to prove the merge sorts first. Role defaults
	// to sdk.CameraRoleHighRes ("high-resolution") since this camera has no
	// recorder.RecorderManager entry — see recordingRolesFor's fallback.
	fixtures := []store.Segment{
		{CameraID: "cam1", Role: "high-resolution", Path: "/d", StartMs: 5000, EndMs: 6000}, // gap after 3000
		{CameraID: "cam1", Role: "high-resolution", Path: "/a", StartMs: 1000, EndMs: 2000}, // base
		{CameraID: "cam1", Role: "high-resolution", Path: "/c", StartMs: 2500, EndMs: 2800}, // fully contained overlap
		{CameraID: "cam1", Role: "high-resolution", Path: "/b", StartMs: 2000, EndMs: 3000}, // touching
	}
	for _, seg := range fixtures {
		if _, err := p.segments.Add(seg); err != nil {
			t.Fatalf("Add(%+v): %v", seg, err)
		}
	}

	got, err := p.GetRecordingSegments("cam1", 0, 10000)
	if err != nil {
		t.Fatalf("GetRecordingSegments: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 merged ranges, got %d: %+v", len(got), got)
	}
	if got[0].StartTime != 1000 || got[0].EndTime != 3000 {
		t.Errorf("expected first merged range [1000,3000], got [%d,%d]", got[0].StartTime, got[0].EndTime)
	}
	if got[1].StartTime != 5000 || got[1].EndTime != 6000 {
		t.Errorf("expected second range [5000,6000] left unmerged (gap), got [%d,%d]", got[1].StartTime, got[1].EndTime)
	}
	for _, seg := range got {
		if seg.CameraID != "cam1" {
			t.Errorf("expected every merged segment to carry cameraId=cam1, got %+v", seg)
		}
	}
}

func TestGetRecordingSegments_MergesAcrossConfiguredRoles(t *testing.T) {
	p := newTestPluginWithDB(t)

	cam := newFakeManagedCamera("cam1", "Front Door", "continuous")
	cam.storage.values["roles"] = []string{"high-resolution", "sub"}
	if err := p.recorder.Add(cam); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, err := p.segments.Add(store.Segment{CameraID: "cam1", Role: "high-resolution", Path: "/a", StartMs: 1000, EndMs: 2000}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := p.segments.Add(store.Segment{CameraID: "cam1", Role: "sub", Path: "/b", StartMs: 1900, EndMs: 3000}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := p.GetRecordingSegments("cam1", 0, 10000)
	if err != nil {
		t.Fatalf("GetRecordingSegments: %v", err)
	}
	if len(got) != 1 || got[0].StartTime != 1000 || got[0].EndTime != 3000 {
		t.Fatalf("expected the overlapping high-resolution+sub segments merged into one [1000,3000] range, got %+v", got)
	}
}

func TestGetRecordingSegments_NoSegmentsReturnsEmptyNotNil(t *testing.T) {
	p := newTestPluginWithDB(t)
	got, err := p.GetRecordingSegments("cam1", 0, 1000)
	if err != nil {
		t.Fatalf("GetRecordingSegments: %v", err)
	}
	if got == nil {
		t.Fatalf("expected a non-nil empty slice, got nil")
	}
}

// --- getStorageStats ---------------------------------------------------------

func TestGetStorageStats_ComputesDiskAndPerCameraBreakdown(t *testing.T) {
	p := newTestPluginWithDB(t)

	dataDir := t.TempDir()
	p.API = &sdk.PluginAPI{StoragePath: dataDir}

	cam := newFakeManagedCamera("cam1", "Front Door", "continuous")
	if err := p.recorder.Add(cam); err != nil {
		t.Fatalf("Add: %v", err)
	}

	segPath := filepath.Join(dataDir, "seg1.mp4")
	if err := os.WriteFile(segPath, make([]byte, 1000), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	segStartMs := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC).UnixMilli()
	if _, err := p.segments.Add(store.Segment{
		CameraID: "cam1", Role: "high-resolution", Path: segPath,
		StartMs: segStartMs, EndMs: segStartMs + 60000, // 60s segment
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	stats, err := p.GetStorageStats()
	if err != nil {
		t.Fatalf("GetStorageStats: %v", err)
	}

	if stats.DiskTotalGB <= 0 {
		t.Errorf("expected DiskTotalGB > 0 for a real temp dir's filesystem, got %v", stats.DiskTotalGB)
	}
	if stats.DiskFreePercent <= 0 || stats.DiskFreePercent > 100 {
		t.Errorf("expected a sane DiskFreePercent (0,100], got %v", stats.DiskFreePercent)
	}

	cam1, ok := stats.Cameras["cam1"]
	if !ok {
		t.Fatalf("expected cam1 in Cameras map, got %v", stats.Cameras)
	}
	if cam1.UsedBytes != 1000 {
		t.Errorf("expected UsedBytes=1000 (the real file size), got %d", cam1.UsedBytes)
	}
	if cam1.SegmentCount != 1 {
		t.Errorf("expected SegmentCount=1, got %d", cam1.SegmentCount)
	}
	if cam1.RecordingMode != "continuous" {
		t.Errorf("expected RecordingMode=continuous (from RecorderManager), got %q", cam1.RecordingMode)
	}
	if !cam1.IsRecording {
		t.Errorf("expected IsRecording=true for a continuous-mode camera")
	}
	if cam1.OldestDay == "" || cam1.NewestDay == "" {
		t.Errorf("expected non-empty OldestDay/NewestDay, got %+v", cam1)
	}

	if stats.NvrUsedGB <= 0 {
		t.Errorf("expected NvrUsedGB > 0 given a 1000-byte segment on disk, got %v", stats.NvrUsedGB)
	}
	if stats.Cameras == nil {
		t.Fatalf("expected a non-nil Cameras map")
	}
}

func TestGetStorageStats_IncludesCamerasWithSegmentsEvenIfUnmanaged(t *testing.T) {
	p := newTestPluginWithDB(t)

	// No p.recorder.Add call for "cam-orphaned" — simulates a camera
	// reassigned away from this Hub (or switched off) that still has
	// historical segments on disk.
	if _, err := p.segments.Add(store.Segment{CameraID: "cam-orphaned", Role: "high-resolution", Path: "/nonexistent", StartMs: 1000, EndMs: 2000}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	stats, err := p.GetStorageStats()
	if err != nil {
		t.Fatalf("GetStorageStats: %v", err)
	}

	cam, ok := stats.Cameras["cam-orphaned"]
	if !ok {
		t.Fatalf("expected cam-orphaned to still appear in Cameras, got %v", stats.Cameras)
	}
	if cam.RecordingMode != "off" {
		t.Errorf("expected a sane default RecordingMode=off for an unmanaged camera, got %q", cam.RecordingMode)
	}
	if cam.IsRecording {
		t.Errorf("expected IsRecording=false for an unmanaged camera")
	}
}

func TestGetStorageStats_NoAPIStillReturnsCameraBreakdown(t *testing.T) {
	// p.API is nil (as in most of this package's other tests) — disk stats
	// can't be read, but the camera breakdown should still work.
	p := newTestPluginWithDB(t)

	stats, err := p.GetStorageStats()
	if err != nil {
		t.Fatalf("GetStorageStats: %v", err)
	}
	if stats.DiskTotalGB != 0 {
		t.Errorf("expected DiskTotalGB=0 with no API/StoragePath, got %v", stats.DiskTotalGB)
	}
	if stats.Cameras == nil {
		t.Fatalf("expected a non-nil Cameras map even with no disk stats")
	}
	if stats.RetentionDays != fallbackRetentionDays {
		t.Errorf("expected RetentionDays=%d fallback with no managed cameras, got %d", fallbackRetentionDays, stats.RetentionDays)
	}
}
