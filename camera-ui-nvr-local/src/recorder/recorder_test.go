package recorder

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// requireFFmpeg skips the calling test if the local ffmpeg binary isn't
// available (it is present in this dev environment, but tests that shell out
// to it should degrade gracefully rather than failing hard on a machine that
// lacks it). This package never requires ffprobe — see ffmpeg.go's FFmpeg
// doc comment for why (node-av, the core's bundled toolchain, ships no
// ffprobe binary at all).
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found on PATH")
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

// TestSegmentTimeRange_ParsesEpochFilename_UsesNextSegmentStartAsEnd proves
// the primary, ffprobe-free path: a filename that's a plain integer (as
// ffmpeg's -strftime 1 "%s.mp4" pattern always produces) is read directly as
// the segment's start time, and when the caller supplies the immediately
// following segment's own start time (nextStartMs — the incremental
// watcher, sweepSegments, always has this once a segment is superseded by a
// newer one), that becomes the exact end time: the two segments are
// sequential, so the moment ffmpeg stopped writing this one is exactly the
// moment it started the next one.
func TestSegmentTimeRange_ParsesEpochFilename_UsesNextSegmentStartAsEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "1700000000.mp4")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	next := int64(1700000005000)
	startMs, endMs, err := segmentTimeRange(path, &next, 60)
	if err != nil {
		t.Fatalf("segmentTimeRange: %v", err)
	}
	if startMs != 1700000000000 {
		t.Errorf("startMs = %d, want 1700000000000", startMs)
	}
	if endMs != 1700000005000 {
		t.Errorf("endMs = %d, want 1700000005000 (the next segment's start)", endMs)
	}
}

// TestSegmentTimeRange_ParsesEpochFilename_EstimatesEndWhenNoNextSegment
// proves the fallback used when there is no next segment yet (the final
// sweep's last file, or a recorder that stopped mid-segment): endMs is
// estimated as startMs + segmentSeconds.
func TestSegmentTimeRange_ParsesEpochFilename_EstimatesEndWhenNoNextSegment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "1700000000.mp4")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	startMs, endMs, err := segmentTimeRange(path, nil, 5)
	if err != nil {
		t.Fatalf("segmentTimeRange: %v", err)
	}
	if startMs != 1700000000000 {
		t.Errorf("startMs = %d, want 1700000000000", startMs)
	}
	if endMs != 1700000005000 {
		t.Errorf("endMs = %d, want 1700000005000 (start + 5s estimate)", endMs)
	}
}

// TestSegmentTimeRange_FallsBackToMtimeForNonEpochFilename proves the
// fallback path used when a file's basename isn't a plain integer (e.g. a
// fixture file with a human-chosen name, as the brief's own real-media
// example uses ("out.mp4")): with no next-segment start available, end is
// derived from the file's mtime (treated as the moment it finished writing)
// and start is estimated as end minus segmentSeconds.
func TestSegmentTimeRange_FallsBackToMtimeForNonEpochFilename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	startMs, endMs, err := segmentTimeRange(path, nil, 2)
	if err != nil {
		t.Fatalf("segmentTimeRange: %v", err)
	}
	if endMs-startMs != 2000 {
		t.Errorf("expected exactly the segmentSeconds estimate apart, got start=%d end=%d", startMs, endMs)
	}

	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if endMs != info.ModTime().UnixMilli() {
		t.Errorf("expected endMs to equal the file's mtime, got endMs=%d mtime=%d", endMs, info.ModTime().UnixMilli())
	}
}

// TestSegmentTimeRange_FallsBackToMtimeForNonEpochFilename_UsesNextSegmentStart
// proves the non-epoch-filename path also prefers nextStartMs over mtime for
// the end time when one is available.
func TestSegmentTimeRange_FallsBackToMtimeForNonEpochFilename_UsesNextSegmentStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	next := int64(1700000005000)
	startMs, endMs, err := segmentTimeRange(path, &next, 2)
	if err != nil {
		t.Fatalf("segmentTimeRange: %v", err)
	}
	if endMs != 1700000005000 {
		t.Errorf("endMs = %d, want 1700000005000 (the next segment's start)", endMs)
	}
	if startMs != 1700000003000 {
		t.Errorf("startMs = %d, want 1700000003000 (end - 2s estimate)", startMs)
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
// feeds it through finalizeSegment (with no next-segment start, so end is
// estimated from segmentSeconds — see segmentTimeRange), and asserts the
// resulting store.Segment row has a correct duration (EndMs-StartMs),
// HasVideo, and Codec — all without ever execing ffprobe (removed entirely;
// see ffmpeg.go's FFmpeg doc comment). Codec/HasAudio come from
// FFmpeg.probeCodecInfo's best-effort `ffmpeg -i` stderr parsing.
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

	const segmentSeconds = 2 // matches the fixture's testsrc=duration=2
	seg, err := finalizeSegment(ff, segStore, "cam1", "high", path, true, nil, segmentSeconds)
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
	if durationMs != 2000 {
		t.Errorf("expected duration exactly 2000ms (segmentSeconds estimate), got %dms (start=%d end=%d)", durationMs, seg.StartMs, seg.EndMs)
	}

	got, err := segStore.InRange("cam1", "high", seg.StartMs, seg.EndMs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != path {
		t.Fatalf("expected the finalized segment to be queryable via InRange, got %+v", got)
	}
}

// TestFinalizeSegment_NeverExecsFfprobe is the task's required RED/GREEN
// proof that finalization never shells out to ffprobe: it restricts PATH to
// a directory containing only a wrapper "ffmpeg" script (which forwards to
// the real, pre-resolved system ffmpeg) and deliberately no "ffprobe" at
// all. Against the pre-fix code (which called FFmpeg.probe, execing
// f.ffprobePath == "ffprobe") this fails with "executable file not found in
// $PATH"; against the fixed code it passes, since finalizeSegment/
// probeCodecInfo only ever exec f.ffmpegPath.
func TestFinalizeSegment_NeverExecsFfprobe(t *testing.T) {
	requireFFmpeg(t)

	realFFmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatalf("LookPath ffmpeg: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "out.mp4")
	genCmd := exec.Command(realFFmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=5",
		"-c:v", "libx264", "-movflags", "+frag_keyframe+empty_moov", path)
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture fMP4: %v\n%s", err, out)
	}

	binDir := t.TempDir()
	wrapper := filepath.Join(binDir, "ffmpeg")
	script := "#!/bin/sh\nexec " + realFFmpeg + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write ffmpeg wrapper: %v", err)
	}
	// binDir intentionally contains no "ffprobe" — restricting PATH to just
	// this directory means any attempt to exec ffprobe fails immediately
	// with "not found", proving finalization never tries to.
	t.Setenv("PATH", binDir)

	segStore := newTestSegmentStore(t)
	ff := &FFmpeg{ffmpegPath: "ffmpeg"} // resolved against the restricted PATH

	seg, err := finalizeSegment(ff, segStore, "cam1", "high", path, true, nil, 1)
	if err != nil {
		t.Fatalf("finalizeSegment: %v (should never need ffprobe)", err)
	}
	if !seg.HasVideo {
		t.Errorf("expected HasVideo=true")
	}
}

// ---------------------------------------------------------------------------
// Supervision: restart-with-backoff and clean stop, via an injected fake
// runner (no real process, no real timing).
// ---------------------------------------------------------------------------

// fakeRunner is a commandRunner test double: behavior decides what the Nth
// call does, letting tests simulate a crash-looping or long-running ffmpeg
// without spawning one. It receives the stderr io.Writer runOnce passes
// through, so a test can write to it to prove stderr capture works.
type fakeRunner struct {
	mu       sync.Mutex
	calls    int
	behavior func(callN int, ctx context.Context, name string, args []string, stderr io.Writer) error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args []string, stderr io.Writer) error {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return f.behavior(n, ctx, name, args, stderr)
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
	ff := &FFmpeg{ffmpegPath: "ffmpeg"}

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
	fake.behavior = func(n int, ctx context.Context, name string, args []string, stderr io.Writer) error {
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
	fake.behavior = func(n int, ctx context.Context, name string, args []string, stderr io.Writer) error {
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

// ---------------------------------------------------------------------------
// ffmpeg stderr capture on failure.
// ---------------------------------------------------------------------------

// TestTailBuffer_BoundedToMaxSize proves tailBuffer keeps only the most
// recent maxSize bytes rather than accumulating everything ever written to
// it — the property that keeps a long-running ffmpeg process's stderr
// capture from growing without bound.
func TestTailBuffer_BoundedToMaxSize(t *testing.T) {
	tb := newTailBuffer(10)

	if _, err := tb.Write([]byte("0123456789ABCDEFGHIJ")); err != nil { // 20 bytes
		t.Fatalf("Write: %v", err)
	}

	if got := tb.String(); got != "ABCDEFGHIJ" {
		t.Fatalf("String() = %q, want %q (only the last 10 bytes)", got, "ABCDEFGHIJ")
	}
}

// TestRunOnce_CapturesFfmpegStderrTailOnFailure proves runOnce wires the
// runner's stderr into the returned error: a crash-looping camera's actual
// failure reason (auth failure, connection refused, RTSP negotiation error,
// ...) must be visible in the logged error, not just a bare exit status.
func TestRunOnce_CapturesFfmpegStderrTailOnFailure(t *testing.T) {
	segStore := newTestSegmentStore(t)
	ff := &FFmpeg{ffmpegPath: "ffmpeg"}

	cfg := RecorderConfig{
		CameraID:       "cam1",
		StreamURL:      func(role string) (string, error) { return "rtsp://cam1/" + role, nil },
		Roles:          []string{"high"},
		SegmentSeconds: 60,
		DataDir:        t.TempDir(),
	}

	r := NewRecorder(cfg, segStore, ff, nil)

	fake := &fakeRunner{}
	const wantStderr = "Connection to rtsp://cam1/high failed: 401 Unauthorized"
	fake.behavior = func(n int, ctx context.Context, name string, args []string, stderr io.Writer) error {
		if _, err := io.WriteString(stderr, wantStderr+"\n"); err != nil {
			t.Fatalf("write to stderr: %v", err)
		}
		return errors.New("exit status 1")
	}
	r.runner = fake

	err := r.runOnce(context.Background(), "high")
	if err == nil {
		t.Fatalf("expected an error from runOnce")
	}
	if !strings.Contains(err.Error(), wantStderr) {
		t.Fatalf("expected the ffmpeg stderr tail in the returned error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("expected the original exit error to still be present, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Start/Stop WaitGroup race regression test.
// ---------------------------------------------------------------------------

// TestRecorder_ConcurrentStartStop_NoRaceAndCleanStop calls Start and Stop
// from separate goroutines concurrently, many times, and asserts no
// panic/data race and that the recorder always converges to a clean stopped
// state. This regression-tests a fixed bug where Start set running = true
// and released r.mu *before* calling wg.Add for its supervisor goroutines:
// a concurrent Stop() could observe running == true and call wg.Wait()
// before any Add() had run, violating sync.WaitGroup's documented
// Add-before-Wait contract — which either lets Stop() return while a
// just-launched supervisor goroutine is still unaccounted for, or trips
// WaitGroup's own "Add called concurrently with Wait" misuse panic. The fix
// (recorder.go, Start) moves every wg.Add call inside the same r.mu
// critical section that sets running = true, so Stop() cannot reach the
// point of calling Wait() until every Add() for that Start() has already
// happened — a structural guarantee, not a timing one, so this test should
// pass reliably (not just probabilistically) against the fixed code.
//
// Confirmed this fails against the pre-fix code: reverting the Start fix
// (moving wg.Add back into the per-role loop, after r.mu.Unlock()) and
// running this test under `go test -race -run ConcurrentStartStop -count=20`
// reproduces both a `-race` data race and, on some runs, a runtime
// "sync: WaitGroup misuse" panic.
func TestRecorder_ConcurrentStartStop_NoRaceAndCleanStop(t *testing.T) {
	segStore := newTestSegmentStore(t)
	ff := &FFmpeg{ffmpegPath: "ffmpeg"}

	cfg := RecorderConfig{
		CameraID:       "cam1",
		StreamURL:      func(role string) (string, error) { return "rtsp://cam1/" + role, nil },
		Roles:          []string{"high"},
		SegmentSeconds: 60,
		DataDir:        t.TempDir(),
	}

	r := NewRecorder(cfg, segStore, ff, nil)
	r.pollInterval = time.Minute // avoid the watcher's ticker firing during this test

	fake := &fakeRunner{}
	// Blocks until ctx is canceled, like a healthy real ffmpeg process would
	// — this avoids the supervisor busy-looping between iterations (it would
	// otherwise restart immediately every time the fake returns) and keeps
	// this test's total work bounded regardless of how the Start/Stop race
	// resolves on any given iteration.
	fake.behavior = func(n int, ctx context.Context, name string, args []string, stderr io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	}
	r.runner = fake

	const iterations = 300
	for i := 0; i < iterations; i++ {
		ctx, cancel := context.WithCancel(context.Background())

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = r.Start(ctx)
		}()
		go func() {
			defer wg.Done()
			_ = r.Stop()
		}()
		wg.Wait()

		// Whichever way the race above resolved, force this iteration's
		// recorder back to a clean stopped state before the next iteration
		// reuses the same instance (a no-op if the racing Stop() already won).
		_ = r.Stop()
		cancel()
	}

	if state := r.State(); state.State != StateStopped {
		t.Fatalf("expected a clean stopped state after %d concurrent Start/Stop iterations, got %+v", iterations, state)
	}
}

// ---------------------------------------------------------------------------
// Incremental segment watcher: bounded per-tick work.
// ---------------------------------------------------------------------------

// TestSweepSegments_SkipsAlreadyProcessedFilesOnSubsequentTicks proves
// sweepSegments' per-tick work is bounded by how many *new* files appeared
// since the last tick, not by the total number of files ever seen in
// outDir: files already indexed on an earlier call are never re-examined or
// re-finalized on a later one (previously, sweepSegments re-globbed and
// re-sorted every file in the directory on every tick, forever, for as long
// as a single ffmpeg run kept accumulating segments in one outDir).
func TestSweepSegments_SkipsAlreadyProcessedFilesOnSubsequentTicks(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	genSegment := func(name string) {
		path := filepath.Join(dir, name)
		cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=5",
			"-c:v", "libx264", "-movflags", "+frag_keyframe+empty_moov", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generate %s: %v\n%s", name, err, out)
		}
	}

	genSegment("1000.mp4")
	genSegment("1001.mp4")
	genSegment("1002.mp4")

	segStore := newTestSegmentStore(t)
	ff := ResolveFFmpeg()
	r := NewRecorder(RecorderConfig{CameraID: "cam1", DataDir: t.TempDir()}, segStore, ff, nil)

	countRows := func() int {
		segs, err := segStore.InRange("cam1", "high", 0, time.Now().UnixMilli()+int64(time.Hour/time.Millisecond))
		if err != nil {
			t.Fatalf("InRange: %v", err)
		}
		return len(segs)
	}

	var processedUpTo string

	// First tick: 3 files present, newest ("1002.mp4") is skipped as
	// presumably still being written.
	r.sweepSegments(dir, "high", &processedUpTo, false)
	if got := countRows(); got != 2 {
		t.Fatalf("expected the first 2 files indexed (newest skipped), got %d rows", got)
	}
	if want := filepath.Join(dir, "1001.mp4"); processedUpTo != want {
		t.Fatalf("processedUpTo = %q, want %q", processedUpTo, want)
	}

	// A new file appears; a second tick must index exactly the one file
	// that became "not-newest" (1002.mp4) and must NOT re-touch 1000/1001.
	genSegment("1003.mp4")
	r.sweepSegments(dir, "high", &processedUpTo, false)
	if got := countRows(); got != 3 {
		t.Fatalf("expected exactly 1 newly-indexed file, not a re-index of earlier ones; got %d rows total", got)
	}
	if want := filepath.Join(dir, "1002.mp4"); processedUpTo != want {
		t.Fatalf("processedUpTo = %q, want %q", processedUpTo, want)
	}

	// Final sweep indexes the last remaining file too.
	r.sweepSegments(dir, "high", &processedUpTo, true)
	if got := countRows(); got != 4 {
		t.Fatalf("expected the final sweep to also index the last remaining file, got %d rows", got)
	}
}
