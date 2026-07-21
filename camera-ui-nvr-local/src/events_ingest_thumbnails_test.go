package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/media"
	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// requireFFmpegForThumbs skips the calling test if the local ffmpeg binary
// isn't on PATH, matching the same-named helper pattern already established
// in recorder/recorder_test.go and src/media/thumbs_test.go.
func requireFFmpegForThumbs(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found on PATH")
	}
}

// genThumbFixtureSegment generates a real short fMP4 file via the local
// ffmpeg binary (a synthetic lavfi testsrc) and indexes it into segStore as
// the covering recording for [startMs, startMs+durationSeconds*1000).
func genThumbFixtureSegment(t *testing.T, segStore *store.SegmentStore, cameraID string, startMs int64, durationSeconds int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "segment.mp4")

	genCmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-movflags", "+frag_keyframe+empty_moov", path)
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture fMP4: %v\n%s", err, out)
	}

	seg := store.Segment{
		CameraID: cameraID,
		Role:     "high-resolution",
		Path:     path,
		StartMs:  startMs,
		EndMs:    startMs + int64(durationSeconds)*1000,
		HasVideo: true,
		Codec:    "h264",
	}
	if _, err := segStore.Add(seg); err != nil {
		t.Fatalf("segStore.Add: %v", err)
	}
}

// TestIngestion_GeneratesThumbnail_AndGetEventThumbnailsServesIt is the
// task's required end-to-end proof: a real recorded segment covers a
// detection event's timestamp, ingesting that event (via
// detectionEventIngester.handle, the exact production wiring path) triggers
// async thumbnail generation, and once generation completes,
// GetEventThumbnails serves back a real, valid JPEG.
func TestIngestion_GeneratesThumbnail_AndGetEventThumbnailsServesIt(t *testing.T) {
	requireFFmpegForThumbs(t)

	p := newTestPluginWithDB(t)
	genThumbFixtureSegment(t, p.segments, "cam1", 10_000_000, 2)

	gen := media.NewGenerator(t.TempDir(), resolvedFFmpegPathForThumbs(), p.segments, p.events, nil)
	p.thumbs = gen

	ingester := newDetectionEventIngester(p.events, nil, gen, nil, nil, nil, nil)
	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID:        "evt-thumb-1",
		CameraID:  "cam1",
		State:     sdk.DetectionEventStateEnded,
		StartTime: 10_000_500, // 500ms into the 2s segment
		EndTime:   10_001_000,
		Types:     []string{"motion"},
	})

	gen.Wait() // deterministically await GenerateAsync's background goroutine

	thumbs, err := p.GetEventThumbnails("cam1", 10_000_500, "evt-thumb-1")
	if err != nil {
		t.Fatalf("GetEventThumbnails: %v", err)
	}
	if len(thumbs.Event) < 2 {
		t.Fatalf("expected a non-empty generated thumbnail, got %d bytes", len(thumbs.Event))
	}
	if thumbs.Event[0] != 0xFF || thumbs.Event[1] != 0xD8 {
		t.Fatalf("expected JPEG magic bytes 0xFFD8, got %v", thumbs.Event[:2])
	}

	// The event itself must still be stored regardless of thumbnail
	// generation, since generation is best-effort and must never affect
	// ingestion (see events_ingest.go's generateThumbnail doc comment).
	result, err := p.GetCameraEvents([]string{"cam1"}, GetEventsOptions{})
	if err != nil {
		t.Fatalf("GetCameraEvents: %v", err)
	}
	if len(result.Events) != 1 || result.Events[0].ID != "evt-thumb-1" {
		t.Fatalf("expected the ingested event to be stored, got %+v", result.Events)
	}
}

// TestIngestion_NoCoveringSegment_StoresEventWithNoThumbnail proves an
// event ingested with no recorded segment covering its timestamp (e.g. one
// that arrives before recording has produced any indexed segment) is still
// stored, without error, and GetEventThumbnails returns an all-empty result
// for it rather than erroring.
func TestIngestion_NoCoveringSegment_StoresEventWithNoThumbnail(t *testing.T) {
	p := newTestPluginWithDB(t)

	gen := media.NewGenerator(t.TempDir(), "ffmpeg", p.segments, p.events, nil)
	p.thumbs = gen

	ingester := newDetectionEventIngester(p.events, nil, gen, nil, nil, nil, nil)
	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID:        "evt-thumb-2",
		CameraID:  "cam1",
		State:     sdk.DetectionEventStateEnded,
		StartTime: 5000,
		EndTime:   6000,
		Types:     []string{"motion"},
	})

	gen.Wait()

	result, err := p.GetCameraEvents([]string{"cam1"}, GetEventsOptions{})
	if err != nil {
		t.Fatalf("GetCameraEvents: %v", err)
	}
	if len(result.Events) != 1 || result.Events[0].ID != "evt-thumb-2" {
		t.Fatalf("expected the event to be stored despite no covering segment, got %+v", result.Events)
	}

	thumbs, err := p.GetEventThumbnails("cam1", 5000, "evt-thumb-2")
	if err != nil {
		t.Fatalf("GetEventThumbnails: %v", err)
	}
	if thumbs.Event != nil || thumbs.Scenes != nil || thumbs.Detections != nil || thumbs.Attributes != nil {
		t.Fatalf("expected an all-empty EventThumbnails when no segment covers the event, got %+v", thumbs)
	}
}

func resolvedFFmpegPathForThumbs() string {
	if p := os.Getenv("CAMERAUI_FFMPEG_PATH"); p != "" {
		return p
	}
	return "ffmpeg"
}
