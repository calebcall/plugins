package recorder

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// requireFFmpeg skips the calling test if the local ffmpeg/ffprobe binaries
// aren't available (both are present in this dev environment, but tests
// that shell out to them should degrade gracefully rather than failing hard
// on a machine that lacks them).
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found on PATH")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not found on PATH")
	}
}

// newTestSegmentStore returns a fresh SegmentStore backed by a throwaway
// SQLite file under t.TempDir(), closed automatically at test cleanup.
func newTestSegmentStore(t *testing.T) *store.SegmentStore {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return store.NewSegmentStore(db)
}

// ---------------------------------------------------------------------------
// segmentTimeRange
// ---------------------------------------------------------------------------

// TestSegmentTimeRange_ParsesEpochFilename proves the primary path: a
// filename that's a plain integer (as ffmpeg's -strftime 1 "%s.mp4" pattern
// always produces) is read directly as the segment's start time, in whole
// seconds since the epoch.
func TestSegmentTimeRange_ParsesEpochFilename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "1700000000.mp4")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	startMs, endMs, err := segmentTimeRange(path, 5000)
	if err != nil {
		t.Fatalf("segmentTimeRange: %v", err)
	}
	if startMs != 1700000000000 {
		t.Errorf("startMs = %d, want 1700000000000", startMs)
	}
	if endMs != 1700000005000 {
		t.Errorf("endMs = %d, want 1700000005000", endMs)
	}
}

// TestSegmentTimeRange_FallsBackToMtimeForNonEpochFilename proves the
// fallback path used when a file's basename isn't a plain integer (e.g. a
// fixture file with a human-chosen name, as the brief's own real-media
// example uses ("out.mp4")): start/end are derived from the file's mtime
// (treated as the moment it finished writing) minus the known duration.
func TestSegmentTimeRange_FallsBackToMtimeForNonEpochFilename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	startMs, endMs, err := segmentTimeRange(path, 2000)
	if err != nil {
		t.Fatalf("segmentTimeRange: %v", err)
	}
	if endMs-startMs != 2000 {
		t.Errorf("expected exactly the given duration apart, got start=%d end=%d", startMs, endMs)
	}

	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if endMs != info.ModTime().UnixMilli() {
		t.Errorf("expected endMs to equal the file's mtime, got endMs=%d mtime=%d", endMs, info.ModTime().UnixMilli())
	}
}

// ---------------------------------------------------------------------------
// finalizeSegment — required real-media proof (no RTSP needed)
// ---------------------------------------------------------------------------

// TestFinalizeSegment_IndexesRealFMP4File is the task's required real-media
// proof: it generates a genuine fragmented-MP4 file with the local ffmpeg
// binary (the exact fixture command the brief names —
// "testsrc=duration=2:size=320x240:rate=10" — under a plain, non-epoch
// filename to exercise the mtime-fallback branch of segmentTimeRange too),
// feeds it through finalizeSegment, and asserts the resulting store.Segment
// row has a correct duration (EndMs-StartMs), HasVideo, and Codec, all
// derived from real ffprobe output rather than a fake.
func TestFinalizeSegment_IndexesRealFMP4File(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "out.mp4")

	genCmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-movflags", "+frag_keyframe+empty_moov", path)
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture fMP4: %v\n%s", err, out)
	}

	segStore := newTestSegmentStore(t)
	ff := ResolveFFmpeg()

	seg, err := finalizeSegment(ff, segStore, "cam1", "high", path)
	if err != nil {
		t.Fatalf("finalizeSegment: %v", err)
	}

	if seg.ID == 0 {
		t.Errorf("expected a positive assigned ID, got %d", seg.ID)
	}
	if seg.CameraID != "cam1" || seg.Role != "high" || seg.Path != path {
		t.Errorf("unexpected identity fields: %+v", seg)
	}
	if !seg.HasVideo {
		t.Errorf("expected HasVideo=true for a testsrc-generated file")
	}
	if seg.HasAudio {
		t.Errorf("expected HasAudio=false (no audio stream was generated)")
	}
	if seg.Codec != "h264" {
		t.Errorf("expected codec h264, got %q", seg.Codec)
	}

	durationMs := seg.EndMs - seg.StartMs
	if durationMs < 1500 || durationMs > 2500 {
		t.Errorf("expected duration ~2000ms (source was testsrc=duration=2), got %dms (start=%d end=%d)", durationMs, seg.StartMs, seg.EndMs)
	}

	got, err := segStore.InRange("cam1", "high", seg.StartMs, seg.EndMs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != path {
		t.Fatalf("expected the finalized segment to be queryable via InRange, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Supervision: restart-with-backoff and clean stop, via an injected fake
// runner (no real process, no real timing).
// ---------------------------------------------------------------------------

// fakeRunner is a commandRunner test double: behavior decides what the Nth
// call does, letting tests simulate a crash-looping or long-running ffmpeg
// without spawning one.
type fakeRunner struct {
	mu       sync.Mutex
	calls    int
	behavior func(callN int, ctx context.Context, name string, args []string) error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args []string) error {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return f.behavior(n, ctx, name, args)
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestRecorder_SupervisionRestartsWithBackoffAndStopsCleanly proves
// superviseRole (driven via Start/Stop): (1) restarts ffmpeg after an
// unexpected exit while ctx is alive, backing off between attempts, (2)
// leaves State() reporting "recording" throughout, and (3) on a clean Stop,
// cancels the in-flight run and does not restart again afterward. The
// injected runner never spawns a process and the injected sleep never
// blocks on a real timer, so this test's outcome doesn't depend on real
// process or wall-clock timing.
func TestRecorder_SupervisionRestartsWithBackoffAndStopsCleanly(t *testing.T) {
	segStore := newTestSegmentStore(t)
	ff := &FFmpeg{ffmpegPath: "ffmpeg", ffprobePath: "ffprobe"}

	cfg := RecorderConfig{
		CameraID:       "cam1",
		StreamURL:      func(role string) (string, error) { return "rtsp://cam1/" + role, nil },
		Roles:          []string{"high"},
		SegmentSeconds: 60,
		DataDir:        t.TempDir(),
	}

	r := NewRecorder(cfg, segStore, ff, nil)
	r.pollInterval = time.Millisecond

	var sleepCalls int32
	r.sleep = func(ctx context.Context, d time.Duration) bool {
		atomic.AddInt32(&sleepCalls, 1)
		return ctx.Err() == nil // never actually block; just honor ctx liveness
	}

	fake := &fakeRunner{}
	// Calls 1 and 2 simulate ffmpeg crashing immediately; call 3 simulates a
	// healthy, long-running process that only exits when ctx is canceled
	// (i.e. Stop()).
	fake.behavior = func(n int, ctx context.Context, name string, args []string) error {
		if n < 3 {
			return errors.New("boom")
		}
		<-ctx.Done()
		return ctx.Err()
	}
	r.runner = fake

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Starting twice must be a no-op (idempotent), not a second set of
	// supervisors.
	if err := r.Start(ctx); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool { return fake.callCount() >= 3 })

	if state := r.State(); state.State != StateRecording || state.CameraID != "cam1" {
		t.Fatalf("expected recording state while supervising, got %+v", state)
	}

	if err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if state := r.State(); state.State != StateStopped {
		t.Fatalf("expected stopped state after Stop, got %+v", state)
	}

	if got := fake.callCount(); got != 3 {
		t.Fatalf("expected exactly 3 runner calls (no restart after a clean Stop), got %d", got)
	}
	if atomic.LoadInt32(&sleepCalls) < 2 {
		t.Fatalf("expected at least 2 backoff sleeps between the 2 crash-loop restarts, got %d", sleepCalls)
	}

	// Stop must also be idempotent.
	if err := r.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// waitForCondition polls cond every millisecond until it's true or timeout
// elapses, failing the test in the latter case. Used instead of a fixed
// sleep so the test runs as fast as the fake allows rather than padding
// itself with a worst-case delay.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Start()-driven segment indexing against a synthetic source (no RTSP).
// ---------------------------------------------------------------------------

// TestRecorder_StartIndexesRealSegments_SyntheticSource is the
// no-RTSP-needed alternative to a full RTSP integration test (see the task
// report's DEFERRED-live note: setting up a real RTSP source in this
// sandbox would be flaky and isn't exercised here). The injected runner
// substitutes a real local ffmpeg process reading a synthetic lavfi testsrc
// in place of RTSP — segmentArgs' own exact flags are separately proven by
// TestSegmentArgs and were manually verified against a real input — so this
// test instead proves the *rest* of the pipeline Start() wires together
// end-to-end against genuine files on disk: directory creation, the
// concurrent segment watcher, ffprobe-based finalization, SegmentStore
// indexing, and State() reporting.
func TestRecorder_StartIndexesRealSegments_SyntheticSource(t *testing.T) {
	requireFFmpeg(t)

	segStore := newTestSegmentStore(t)
	ff := ResolveFFmpeg()

	cfg := RecorderConfig{
		CameraID:       "cam1",
		StreamURL:      func(role string) (string, error) { return "rtsp://unused/" + role, nil },
		Roles:          []string{"high"},
		SegmentSeconds: 1,
		DataDir:        t.TempDir(),
	}

	r := NewRecorder(cfg, segStore, ff, nil)
	r.pollInterval = 200 * time.Millisecond

	fake := &fakeRunner{}
	fake.behavior = func(n int, ctx context.Context, name string, args []string) error {
		// args' last element is the segmentArgs output pattern
		// (<outDir>/%s.mp4); reuse that directory so the recorder's own
		// watcher (pointed at the same outDir) discovers these files.
		outDir := filepath.Dir(args[len(args)-1])
		realArgs := []string{
			"-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc=duration=5:size=320x240:rate=10",
			"-c:v", "libx264",
			"-f", "segment", "-segment_time", "1", "-segment_format", "mp4",
			"-reset_timestamps", "1", "-strftime", "1",
			"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
			filepath.Join(outDir, "%s.mp4"),
		}
		cmd := exec.CommandContext(ctx, "ffmpeg", realArgs...)
		return cmd.Run()
	}
	r.runner = fake

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if state := r.State(); state.State != StateRecording {
		t.Fatalf("expected recording state right after Start, got %+v", state)
	}

	waitForCondition(t, 10*time.Second, func() bool {
		segs, err := segStore.InRange("cam1", "high", 0, time.Now().UnixMilli()+int64(time.Hour/time.Millisecond))
		if err != nil {
			t.Fatalf("InRange: %v", err)
		}
		return len(segs) >= 1
	})

	if err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if state := r.State(); state.State != StateStopped {
		t.Fatalf("expected stopped state after Stop, got %+v", state)
	}
}
