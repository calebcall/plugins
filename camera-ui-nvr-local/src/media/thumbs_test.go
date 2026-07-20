package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// requireFFmpeg skips the calling test if the local ffmpeg binary isn't
// available on PATH — mirrors recorder/recorder_test.go's helper of the
// same name (that package's own tests can't be reused directly here without
// creating a media->recorder test dependency).
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found on PATH")
	}
}

// jpegMagic is the two-byte SOI marker every valid JPEG file starts with.
var jpegMagic = []byte{0xFF, 0xD8}

// genFixtureSegment generates a real short fMP4 file via the local ffmpeg
// binary (a synthetic lavfi testsrc, exactly like recorder/recorder_test.go's
// fixture pattern) and returns a store.Segment describing it, covering
// [startMs, startMs+durationMs).
func genFixtureSegment(t *testing.T, dir string, startMs int64, durationSeconds int) store.Segment {
	t.Helper()
	path := filepath.Join(dir, "segment.mp4")

	genCmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration="+strconv.Itoa(durationSeconds)+":size=320x240:rate=10",
		"-c:v", "libx264", "-movflags", "+frag_keyframe+empty_moov", path)
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture fMP4: %v\n%s", err, out)
	}

	return store.Segment{
		CameraID: "cam1",
		Role:     "high-resolution",
		Path:     path,
		StartMs:  startMs,
		EndMs:    startMs + int64(durationSeconds)*1000,
		HasVideo: true,
		Codec:    "h264",
	}
}

// newTestStores returns a real SegmentStore/EventStore pair backed by a
// throwaway SQLite database, closed at test cleanup.
func newTestStores(t *testing.T) (*store.SegmentStore, *store.EventStore) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return store.NewSegmentStore(db), store.NewEventStore(db)
}

func resolvedFFmpegPath() string {
	if p := os.Getenv("CAMERAUI_FFMPEG_PATH"); p != "" {
		return p
	}
	return "ffmpeg"
}

// TestGenerate_ExtractsRealFrame_StoresJPEG_AndSetsThumbRef is the task's
// required real-media proof: a genuine fragmented-MP4 segment is indexed,
// an event whose timestamp falls inside it is generated against, and the
// result is asserted to be a real, non-empty JPEG (valid SOI magic bytes)
// both on disk and via the event's persisted thumb_ref.
func TestGenerate_ExtractsRealFrame_StoresJPEG_AndSetsThumbRef(t *testing.T) {
	requireFFmpeg(t)

	segDir := t.TempDir()
	seg := genFixtureSegment(t, segDir, 1_000_000, 2)

	segments, events := newTestStores(t)
	if _, err := segments.Add(seg); err != nil {
		t.Fatalf("segments.Add: %v", err)
	}
	if err := events.Upsert([]store.DetectionEvent{{
		ID:        "evt-1",
		CameraID:  "cam1",
		StartTime: 1_000_500, // 500ms into the 2s segment
	}}); err != nil {
		t.Fatalf("events.Upsert: %v", err)
	}

	thumbsDir := t.TempDir()
	gen := NewGenerator(thumbsDir, resolvedFFmpegPath(), segments, events, nil)

	if err := gen.Generate(context.Background(), store.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1_000_500,
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	ref, err := events.GetThumbRef("evt-1")
	if err != nil {
		t.Fatalf("GetThumbRef: %v", err)
	}
	if ref == "" {
		t.Fatalf("expected thumb_ref to be set after Generate")
	}

	data, err := os.ReadFile(ref)
	if err != nil {
		t.Fatalf("read generated thumbnail: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("expected a non-empty JPEG file")
	}
	if len(data) < 2 || data[0] != jpegMagic[0] || data[1] != jpegMagic[1] {
		t.Fatalf("expected JPEG magic bytes 0xFFD8, got %v", data[:2])
	}
}

// TestGenerate_NoCoveringSegment_SkipsGracefully proves an event whose
// timestamp isn't covered by any recorded segment (e.g. before recording
// started) produces no thumbnail and, critically, no error — Generate's
// contract is "skip gracefully", not "fail".
func TestGenerate_NoCoveringSegment_SkipsGracefully(t *testing.T) {
	segments, events := newTestStores(t)
	if err := events.Upsert([]store.DetectionEvent{{ID: "evt-1", CameraID: "cam1", StartTime: 5000}}); err != nil {
		t.Fatalf("events.Upsert: %v", err)
	}

	gen := NewGenerator(t.TempDir(), "ffmpeg", segments, events, nil)

	if err := gen.Generate(context.Background(), store.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 5000,
	}); err != nil {
		t.Fatalf("Generate: expected nil error for no covering segment, got %v", err)
	}

	ref, err := events.GetThumbRef("evt-1")
	if err != nil {
		t.Fatalf("GetThumbRef: %v", err)
	}
	if ref != "" {
		t.Fatalf("expected no thumb_ref to be set, got %q", ref)
	}
}

// TestGenerate_AlreadyDone_SkipsWithoutRerunningFFmpeg proves a second
// Generate call for an event that already succeeded is a pure no-op: it
// must not attempt to run ffmpeg again (proven via a fake runner that fails
// the test if invoked).
func TestGenerate_AlreadyDone_SkipsWithoutRerunningFFmpeg(t *testing.T) {
	requireFFmpeg(t)

	segDir := t.TempDir()
	seg := genFixtureSegment(t, segDir, 2_000_000, 2)

	segments, events := newTestStores(t)
	if _, err := segments.Add(seg); err != nil {
		t.Fatalf("segments.Add: %v", err)
	}

	event := store.DetectionEvent{ID: "evt-2", CameraID: "cam1", StartTime: 2_000_500}
	if err := events.Upsert([]store.DetectionEvent{event}); err != nil {
		t.Fatalf("events.Upsert: %v", err)
	}

	gen := NewGenerator(t.TempDir(), resolvedFFmpegPath(), segments, events, nil)

	if err := gen.Generate(context.Background(), event); err != nil {
		t.Fatalf("first Generate: %v", err)
	}

	gen.runner = failingRunner{t: t}
	if err := gen.Generate(context.Background(), event); err != nil {
		t.Fatalf("second Generate (should be a no-op): %v", err)
	}
}

type failingRunner struct{ t *testing.T }

func (f failingRunner) Run(ctx context.Context, name string, args []string) error {
	f.t.Fatalf("unexpected ffmpeg invocation on an already-generated event: %s %v", name, args)
	return nil
}

// TestGenerateAsync_WaitBlocksUntilDone proves GenerateAsync actually runs
// Generate in the background (crossing goroutines, per the task's -race
// requirement) and that Wait deterministically blocks until it has
// finished, rather than a test needing to sleep/poll.
func TestGenerateAsync_WaitBlocksUntilDone(t *testing.T) {
	requireFFmpeg(t)

	segDir := t.TempDir()
	seg := genFixtureSegment(t, segDir, 3_000_000, 2)

	segments, events := newTestStores(t)
	if _, err := segments.Add(seg); err != nil {
		t.Fatalf("segments.Add: %v", err)
	}
	event := store.DetectionEvent{ID: "evt-3", CameraID: "cam1", StartTime: 3_000_500}
	if err := events.Upsert([]store.DetectionEvent{event}); err != nil {
		t.Fatalf("events.Upsert: %v", err)
	}

	gen := NewGenerator(t.TempDir(), resolvedFFmpegPath(), segments, events, nil)

	gen.GenerateAsync(event)
	gen.Wait()

	ref, err := events.GetThumbRef("evt-3")
	if err != nil {
		t.Fatalf("GetThumbRef: %v", err)
	}
	if ref == "" {
		t.Fatalf("expected GenerateAsync+Wait to have persisted a thumb_ref")
	}
}

// TestExtractFrameArgs_ClampsOffsetIntoSegment proves frameOffsetSeconds
// clamps an out-of-range timestamp into [0, duration) rather than handing
// ffmpeg a negative or past-the-end -ss value.
func TestExtractFrameArgs_ClampsOffsetIntoSegment(t *testing.T) {
	seg := store.Segment{Path: "/x.mp4", StartMs: 1000, EndMs: 3000}

	if got := frameOffsetSeconds(seg, 500); got != 0 {
		t.Errorf("expected 0 for a timestamp before the segment start, got %v", got)
	}
	if got := frameOffsetSeconds(seg, 1500); got != 0.5 {
		t.Errorf("expected 0.5 for a timestamp 500ms into the segment, got %v", got)
	}
	if got := frameOffsetSeconds(seg, 5000); got != 1.999 {
		t.Errorf("expected clamping to just under the 2s duration, got %v", got)
	}
}
