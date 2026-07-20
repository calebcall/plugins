package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/media"
	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// fakeInvoke records one playbackEmitter.Invoke call, so tests can assert
// exactly what was delivered to which named callback without a live
// *rpc.CallbackInvoker/NATS connection.
type fakeInvoke struct {
	method string
	args   []any
}

// fakeEmitter is an in-memory stand-in for *rpc.CallbackInvoker: it
// records every Invoke call and reports Active() until told to stop being
// active (simulating the client disconnecting/cancelling — the only
// signal handlePullCallbackRequestGo gives a pull-callback handler at
// all — see rpc_playback.go's package doc comment).
type fakeEmitter struct {
	mu     sync.Mutex
	calls  []fakeInvoke
	active bool
}

func newFakeEmitter() *fakeEmitter {
	return &fakeEmitter{active: true}
}

func (f *fakeEmitter) Invoke(method string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeInvoke{method: method, args: args})
}

func (f *fakeEmitter) Active() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

func (f *fakeEmitter) deactivate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = false
}

func (f *fakeEmitter) callsOf(method string) []fakeInvoke {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeInvoke
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

// fakeSegment builds a media.SegmentFrames with count trivial access
// units at the given fps, starting at baseUs, tagged with id/startMs/
// endMs on its embedded store.Segment (id/role are what nextSegmentAfter-
// style rollover logic keys off of in a real *media.Player; the fakes
// here just need FirstSegment/NextSegment to agree on which segment
// "comes after" which).
func fakeSegment(id, startMs, endMs int64, baseUs int64, fps float64, count int) media.SegmentFrames {
	units := make([]media.AccessUnit, count)
	for i := range units {
		units[i] = media.AccessUnit{Data: []byte{byte(i + 1)}, Keyframe: i == 0}
	}
	return media.SegmentFrames{
		Segment:     store.Segment{ID: id, CameraID: "cam1", Role: "high-resolution", StartMs: startMs, EndMs: endMs},
		Units:       units,
		CodecString: "avc1.64001f",
		Width:       320,
		Height:      240,
		FPS:         fps,
		BaseUs:      baseUs,
	}
}

// fakePlaybackSource is an in-memory stand-in for *media.Player: a fixed
// first segment plus an ordered chain of "what comes after" segments
// (keyed by the previous segment's ID), so playbackSession.run's own
// rollover/no-data logic can be driven deterministically without a real
// ffmpeg process or SQLite-backed SegmentStore.
type fakePlaybackSource struct {
	mu sync.Mutex

	first    media.SegmentFrames
	firstOK  bool
	firstErr error

	next map[int64]media.SegmentFrames // keyed by the previous segment's ID

	// gotFirstArgs/nextCallCount record what run actually asked for, so
	// tests can assert arguments were threaded through correctly.
	gotCameraID   string
	gotTsUs       int64
	gotSourceRole string
	nextCallCount int
}

func (f *fakePlaybackSource) FirstSegment(ctx context.Context, cameraID string, tsUs int64, sourceRole string) (media.SegmentFrames, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotCameraID = cameraID
	f.gotTsUs = tsUs
	f.gotSourceRole = sourceRole
	return f.first, f.firstOK, f.firstErr
}

func (f *fakePlaybackSource) NextSegment(ctx context.Context, cameraID string, prev store.Segment) (media.SegmentFrames, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextCallCount++
	next, ok := f.next[prev.ID]
	return next, ok, nil
}

// --- run: basic ready/video/no-data sequencing ---------------------------

// TestPlaybackSession_Run_NoCoveringSegment_EmitsOnNoDataOnly proves a
// session whose FirstSegment call finds nothing (ok=false) emits exactly
// one onNoData, carrying the originally requested tsUs, and no onReady/
// onVideo at all.
func TestPlaybackSession_Run_NoCoveringSegment_EmitsOnNoDataOnly(t *testing.T) {
	src := &fakePlaybackSource{firstOK: false}
	emit := newFakeEmitter()
	sess := newPlaybackSession("sess-1")
	batches := make(chan struct{}, 64)

	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.run(context.Background(), src, "cam1", 42_000, "", emit, batches)
	}()
	waitOrTimeout(t, done, 2*time.Second, "run to return")

	if len(emit.callsOf("onReady")) != 0 {
		t.Errorf("expected no onReady, got %+v", emit.callsOf("onReady"))
	}
	noData := emit.callsOf("onNoData")
	if len(noData) != 1 {
		t.Fatalf("expected exactly 1 onNoData, got %d", len(noData))
	}
	payload, ok := noData[0].args[0].(NvrPlaybackNoData)
	if !ok || payload.Ts != 42_000 {
		t.Errorf("expected onNoData{Ts: 42000}, got %+v", noData[0].args[0])
	}
}

// TestPlaybackSession_Run_EmitsReadyThenVideoFrames proves the core
// sequencing contract: exactly one onReady (codec/resolution from the
// first segment), followed by one onVideo per access unit with
// strictly-increasing ts and the right keyframe flags, followed by
// onNoData once no further segment exists.
func TestPlaybackSession_Run_EmitsReadyThenVideoFrames(t *testing.T) {
	first := fakeSegment(1, 1_000, 2_000, 1_000_000, 10, 3)
	src := &fakePlaybackSource{first: first, firstOK: true, next: map[int64]media.SegmentFrames{}}
	emit := newFakeEmitter()
	sess := newPlaybackSession("sess-2")
	batches := make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.run(context.Background(), src, "cam1", 1_000_000, "high", emit, batches)
	}()

	// Drain exactly 3 batch-boundary signals (one per access unit) —
	// mirrors the pull-callback framework calling Recv() once per client
	// "next" request; see playbackSession.run's own doc comment on why a
	// blocked send on batches IS this session's backpressure.
	for i := 0; i < 3; i++ {
		waitOrTimeoutValue(t, batches, 2*time.Second, "batch signal")
	}
	waitOrTimeout(t, done, 2*time.Second, "run to return")

	ready := emit.callsOf("onReady")
	if len(ready) != 1 {
		t.Fatalf("expected exactly 1 onReady, got %d", len(ready))
	}
	readyPayload, ok := ready[0].args[0].(NvrPlaybackReady)
	if !ok {
		t.Fatalf("expected NvrPlaybackReady payload, got %T", ready[0].args[0])
	}
	if readyPayload.SessionID != "sess-2" || readyPayload.VideoCodec != "h264" ||
		readyPayload.CodecString != first.CodecString || readyPayload.Width != 320 || readyPayload.Height != 240 {
		t.Errorf("unexpected onReady payload: %+v", readyPayload)
	}

	video := emit.callsOf("onVideo")
	if len(video) != 3 {
		t.Fatalf("expected 3 onVideo calls, got %d", len(video))
	}
	var lastTs int64 = -1
	for i, v := range video {
		payload, ok := v.args[0].(NvrPlaybackVideo)
		if !ok {
			t.Fatalf("call %d: expected NvrPlaybackVideo payload, got %T", i, v.args[0])
		}
		if payload.Ts <= lastTs {
			t.Fatalf("call %d: expected strictly increasing ts, got %d after %d", i, payload.Ts, lastTs)
		}
		lastTs = payload.Ts
		if i == 0 && !payload.Keyframe {
			t.Errorf("expected the first frame to be a keyframe")
		}
	}

	if len(src.gotSourceRole) == 0 || src.gotCameraID != "cam1" || src.gotTsUs != 1_000_000 {
		t.Errorf("expected FirstSegment args to pass through: cameraID=%q tsUs=%d sourceRole=%q", src.gotCameraID, src.gotTsUs, src.gotSourceRole)
	}

	noData := emit.callsOf("onNoData")
	if len(noData) != 1 {
		t.Fatalf("expected onNoData once no next segment exists, got %d", len(noData))
	}
}

// TestPlaybackSession_Run_RollsIntoNextSegment proves a session whose
// first segment's units are exhausted rolls into whatever NextSegment
// reports for it, emitting that segment's own frames too — all under the
// single onReady already emitted for the first segment (no second
// onReady).
func TestPlaybackSession_Run_RollsIntoNextSegment(t *testing.T) {
	first := fakeSegment(1, 0, 1_000, 0, 10, 2)
	second := fakeSegment(2, 1_000, 2_000, 1_000_000, 10, 2)
	src := &fakePlaybackSource{
		first: first, firstOK: true,
		next: map[int64]media.SegmentFrames{1: second},
	}
	emit := newFakeEmitter()
	sess := newPlaybackSession("sess-3")
	batches := make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.run(context.Background(), src, "cam1", 0, "", emit, batches)
	}()

	for i := 0; i < 4; i++ { // 2 units per segment x 2 segments
		waitOrTimeoutValue(t, batches, 2*time.Second, "batch signal")
	}
	waitOrTimeout(t, done, 2*time.Second, "run to return")

	if len(emit.callsOf("onReady")) != 1 {
		t.Fatalf("expected exactly 1 onReady across the rollover, got %d", len(emit.callsOf("onReady")))
	}
	if len(emit.callsOf("onVideo")) != 4 {
		t.Fatalf("expected 4 onVideo calls (2 segments x 2 units), got %d", len(emit.callsOf("onVideo")))
	}
	if src.nextCallCount != 2 {
		// Once to roll from segment 1 into segment 2, once more to
		// discover segment 2 has nothing after it (onNoData).
		t.Errorf("expected NextSegment to be called twice, got %d", src.nextCallCount)
	}
}

// TestPlaybackSession_Run_FirstSegmentError_EmitsOnNoData proves a
// genuine error from FirstSegment (as opposed to its ok=false "not
// found" contract) still degrades to onNoData rather than propagating —
// there is no RPC-error path available once nvrPlayback has already
// returned its channel to the pull-callback framework.
func TestPlaybackSession_Run_FirstSegmentError_EmitsOnNoData(t *testing.T) {
	src := &fakePlaybackSource{firstErr: errors.New("boom")}
	emit := newFakeEmitter()
	sess := newPlaybackSession("sess-4")
	batches := make(chan struct{}, 8)

	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.run(context.Background(), src, "cam1", 7, "", emit, batches)
	}()
	waitOrTimeout(t, done, 2*time.Second, "run to return")

	if len(emit.callsOf("onNoData")) != 1 {
		t.Fatalf("expected onNoData on a FirstSegment error, got %+v", emit.calls)
	}
}

// --- pause/resume/speed ---------------------------------------------------

// TestPlaybackSession_Pause_StopsEmission_ResumeContinues proves
// nvrPlaybackCmd's pause/resume contract at the session level: pausing
// blocks further onVideo emission until resumed, with no frames lost or
// reordered across the pause.
func TestPlaybackSession_Pause_StopsEmission_ResumeContinues(t *testing.T) {
	// fps=20 -> a 50ms pace interval between frames, generous margin for
	// sess.Pause() (a single mutex lock, sub-microsecond) to land before
	// run's loop reaches the next frame's waitToContinue check.
	first := fakeSegment(1, 0, 1_000, 0, 20, 5)
	src := &fakePlaybackSource{first: first, firstOK: true, next: map[int64]media.SegmentFrames{}}
	emit := newFakeEmitter()
	sess := newPlaybackSession("sess-5")
	batches := make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.run(context.Background(), src, "cam1", 0, "", emit, batches)
	}()

	// Drain the first 2 frames, then pause.
	waitOrTimeoutValue(t, batches, 2*time.Second, "batch 1")
	waitOrTimeoutValue(t, batches, 2*time.Second, "batch 2")
	sess.Pause()

	// No further batch signal should arrive while paused.
	select {
	case <-batches:
		t.Fatalf("expected no further emission while paused")
	case <-time.After(150 * time.Millisecond):
	}
	if got := len(emit.callsOf("onVideo")); got != 2 {
		t.Fatalf("expected exactly 2 onVideo calls while paused, got %d", got)
	}

	sess.Resume()
	waitOrTimeoutValue(t, batches, 2*time.Second, "batch 3 after resume")
	waitOrTimeoutValue(t, batches, 2*time.Second, "batch 4 after resume")
	waitOrTimeoutValue(t, batches, 2*time.Second, "batch 5 after resume")
	waitOrTimeout(t, done, 2*time.Second, "run to return")

	if got := len(emit.callsOf("onVideo")); got != 5 {
		t.Fatalf("expected all 5 frames emitted after resume, got %d", got)
	}
}

// TestPlaybackSession_SetSpeed_RejectsNonPositive proves SetSpeed leaves
// the pacing multiplier unchanged for a non-positive value rather than
// letting paceSleep divide by zero or sleep forever between frames.
func TestPlaybackSession_SetSpeed_RejectsNonPositive(t *testing.T) {
	sess := newPlaybackSession("sess-6")
	sess.SetSpeed(4)
	sess.SetSpeed(0)
	sess.SetSpeed(-1)

	_, _, speed := sess.snapshot()
	if speed != 4 {
		t.Fatalf("expected speed to stay at 4 after rejecting non-positive values, got %v", speed)
	}
}

// --- stop/cancel: no leaked goroutine --------------------------------------

// TestPlaybackSession_Stop_EndsRunPromptly_NoLeak proves Stop() makes
// run's goroutine exit promptly (bounded by pausePollInterval) even when
// the session is mid-pause and the source has effectively unlimited
// frames left to emit — the "clean teardown, no leaked goroutine"
// requirement, verified with a done channel rather than a fixed sleep.
func TestPlaybackSession_Stop_EndsRunPromptly_NoLeak(t *testing.T) {
	first := fakeSegment(1, 0, 100_000, 0, 10, 100_000) // effectively unbounded frames
	src := &fakePlaybackSource{first: first, firstOK: true}
	emit := newFakeEmitter()
	sess := newPlaybackSession("sess-7")
	batches := make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.run(context.Background(), src, "cam1", 0, "", emit, batches)
	}()

	waitOrTimeoutValue(t, batches, 2*time.Second, "first batch")
	sess.Pause()
	sess.Stop()

	waitOrTimeout(t, done, 2*time.Second, "run to return promptly after Stop() while paused")
}

// TestPlaybackSession_ClientDisconnect_EndsRunPromptly_NoLeak proves the
// production cancellation path — emit.Active() going false, the only
// signal a pull-callback handler receives when the client disconnects or
// its for-await loop breaks — also ends run's goroutine promptly, without
// the session ever having Stop() called on it directly.
func TestPlaybackSession_ClientDisconnect_EndsRunPromptly_NoLeak(t *testing.T) {
	first := fakeSegment(1, 0, 100_000, 0, 10, 100_000)
	src := &fakePlaybackSource{first: first, firstOK: true}
	emit := newFakeEmitter()
	sess := newPlaybackSession("sess-8")
	batches := make(chan struct{})

	// Mirrors NvrPlayback's own wrapping goroutine (rpc_playback.go):
	// close(batches) once run returns. Deferred after (so it runs before,
	// LIFO) close(done) — done closing is proof batches is closed too.
	//
	// A framework-side drain goroutine (handlePullCallbackRequestGo spawns
	// one on "cancel" — see rpc_playback.go's package doc comment) isn't
	// needed to make this deterministic: once emit.deactivate() runs,
	// run's very next check point is paceSleep (not another send on
	// batches — the send for the frame already received below has
	// already completed), so run returns on its own without ever
	// blocking on a second send this test would need to drain.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(batches)
		sess.run(context.Background(), src, "cam1", 0, "", emit, batches)
	}()

	waitOrTimeoutValue(t, batches, 2*time.Second, "first batch")
	emit.deactivate()

	waitOrTimeout(t, done, 2*time.Second, "run to return promptly once emit.Active() is false")

	if _, open := <-batches; open {
		t.Fatalf("expected batches to be closed once run returned")
	}
}

func waitOrTimeout(t *testing.T, done <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitOrTimeoutValue(t *testing.T, ch <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}
