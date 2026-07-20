// playback_session.go implements the streaming-playback session driven by
// NvrPlayback (rpc_playback.go, Task PLAYBACK): given a *media.Player (the
// segment-lookup/ffmpeg-extraction/AU-splitting logic, src/media/
// playback.go) and a playbackEmitter (the pinned wire mechanism —
// rpc_playback.go's package doc comment), walk from the covering segment
// at a requested timestamp forward, segment by segment, emitting one
// onVideo callback per access unit, until either a real recording gap (no
// next segment) or the client disconnects/cancels.
package main

import (
	"context"
	"sync"
	"time"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/media"
	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// pausePollInterval bounds how long playbackSession.run's loop can go
// without rechecking pause/stop/client-liveness (waitToContinue/
// paceSleep) — short enough that pause/resume/stop feel instant to a human
// driving the transport controls, long enough not to spin the CPU.
const pausePollInterval = 20 * time.Millisecond

// fallbackFrameIntervalUs is the inter-frame pacing interval
// (playbackSession.run) used for a segment with fewer than two access
// units, where there is no ts delta to derive one from — an edge case (a
// near-empty segment), not the common path. ~30fps.
const fallbackFrameIntervalUs = int64(1_000_000 / 30)

// playbackEmitter is the subset of *rpc.CallbackInvoker
// (github.com/cameraui/rpc/go@v1.0.6/handler_pull_callback.go) run needs:
// Invoke to deliver the wire contract's five callbacks (onReady/onVideo/
// onBatch/onAudio/onNoData — see rpc_playback.go's package doc comment for
// how this exact shape was pinned against both the framework and the
// compiled frontend worker), and Active to detect that the client has
// disconnected/cancelled — the only cancellation signal
// handlePullCallbackRequestGo hands a pull-callback handler at all (it
// deactivates the invoker and drains this session's returned channel, but
// never touches this session directly). Declared as an interface, rather
// than depending on *rpc.CallbackInvoker directly, so tests can inject a
// fake without a live NATS connection; *rpc.CallbackInvoker satisfies it
// as-is, with no adapter needed.
type playbackEmitter interface {
	Invoke(method string, args ...any)
	Active() bool
}

// playbackSource is the subset of *media.Player NvrPlayback needs: finding
// and extracting the segment covering a timestamp, and rolling forward
// into whatever segment immediately follows it. *media.Player satisfies
// this directly; tests inject a fake so session-loop behavior (pause/
// resume/speed/rollover/no-data) can be proven without a real ffmpeg
// process or SQLite-backed SegmentStore.
type playbackSource interface {
	FirstSegment(ctx context.Context, cameraID string, tsUs int64, sourceRole string) (media.SegmentFrames, bool, error)
	NextSegment(ctx context.Context, cameraID string, prev store.Segment) (media.SegmentFrames, bool, error)
}

// playbackSession drives one nvrPlayback streaming session and holds the
// mutable pause/speed/stop state nvrPlaybackCmd (rpc_playback.go) mutates
// by sessionID via playbackSessionRegistry. Every method is safe for
// concurrent use: Pause/Resume/SetSpeed/Stop are called from the
// nvrPlaybackCmd RPC goroutine while run's own goroutine (started by
// NvrPlayback) concurrently reads paused/speed/stopped and emits frames.
type playbackSession struct {
	id string

	mu      sync.Mutex
	paused  bool
	speed   float64
	stopped bool
}

// newPlaybackSession returns a playbackSession identified by id (the value
// reported to the client as NvrPlaybackReady.SessionID and later passed
// back into nvrPlaybackCmd), at the default 1x speed.
func newPlaybackSession(id string) *playbackSession {
	return &playbackSession{id: id, speed: 1}
}

// Pause stops run's loop from emitting further frames until Resume — see
// waitToContinue. Idempotent.
func (s *playbackSession) Pause() {
	s.mu.Lock()
	s.paused = true
	s.mu.Unlock()
}

// Resume reverses Pause. Idempotent, and a no-op (not an error) when the
// session was never paused.
func (s *playbackSession) Resume() {
	s.mu.Lock()
	s.paused = false
	s.mu.Unlock()
}

// SetSpeed changes run's inter-frame pacing multiplier (paceSleep):
// speed 2 paces at roughly twice the extracted footage's own frame rate,
// speed 0.5 at half. speed <= 0 is rejected (left unchanged) — Pause
// already exists as this session's "stop emitting" primitive; honoring a
// zero/negative speed here would otherwise divide by zero or sleep
// forever between frames in paceSleep.
func (s *playbackSession) SetSpeed(speed float64) {
	if speed <= 0 {
		return
	}
	s.mu.Lock()
	s.speed = speed
	s.mu.Unlock()
}

// Stop requests run's loop exit at its next pause/pace check point (within
// pausePollInterval). Safe to call more than once, and after run has
// already returned on its own.
func (s *playbackSession) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
}

func (s *playbackSession) snapshot() (paused, stopped bool, speed float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused, s.stopped, s.speed
}

// waitToContinue blocks while the session is paused, polling every
// pausePollInterval so Stop() and emit.Active() going false are both
// noticed promptly rather than only on Resume() — a session that is
// paused and then abandoned (the client disconnects without ever calling
// resume or stop) must not leak this goroutine forever. Returns false the
// moment the session should stop emitting altogether.
func (s *playbackSession) waitToContinue(emit playbackEmitter) bool {
	for {
		paused, stopped, _ := s.snapshot()
		if stopped || !emit.Active() {
			return false
		}
		if !paused {
			return true
		}
		time.Sleep(pausePollInterval)
	}
}

// paceSleep sleeps roughly one frame interval (frameIntervalUs
// microseconds, scaled by 1/speed) in pausePollInterval-sized increments,
// so Stop()/emit.Active() going false interrupts it promptly rather than
// oversleeping past a client disconnect/stop by a full (possibly
// slow-speed) frame interval. Returns false the moment the session should
// stop.
func (s *playbackSession) paceSleep(emit playbackEmitter, frameIntervalUs int64) bool {
	_, _, speed := s.snapshot()
	remaining := time.Duration(float64(frameIntervalUs)/speed) * time.Microsecond

	for remaining > 0 {
		step := pausePollInterval
		if remaining < step {
			step = remaining
		}
		time.Sleep(step)
		remaining -= step

		_, stopped, _ := s.snapshot()
		if stopped || !emit.Active() {
			return false
		}
	}
	return true
}

// run drives the session end to end: finds the segment covering tsUs
// (src.FirstSegment), emits onReady exactly once (codec/resolution from
// that first segment only — a later rollover's own probed values are used
// solely for that segment's own Timestamps() spacing, never re-announced),
// then walks its access units — emitting onVideo per frame, gated by
// waitToContinue/paceSleep — rolling into whatever segment follows once
// the current one's units are exhausted (src.NextSegment), until either no
// next segment exists (onNoData: a real recording gap, or the live edge of
// what's been recorded) or the session stops (Stop(), or emit.Active()
// goes false — the client disconnected/cancelled).
//
// videoOnly (part of nvrPlayback's own RPC signature — see
// rpc_playback.go's NvrPlayback) is deliberately not a parameter here: it
// selects whether the client wants audio at all, but audio streaming is
// deferred in this v1 (see NvrPlaybackReady's doc comment, wire.go) — every
// session behaves as if videoOnly were true regardless of what the caller
// actually passed, so there is nothing for run to branch on.
//
// batches receives one value per emitted video frame — the pull-callback
// protocol's own batch-boundary signal (see rpc_playback.go's package doc
// comment): the framework only calls Recv() on it in response to the
// client's own "next" pull request, so a blocked send on batches already
// IS this session's backpressure — no separate lookahead buffer is needed.
// This also means one emitted frame == one client-driven pull, the
// simplest possible mapping and the one this v1 uses; onBatch (bundling
// several frames per pull) is left for a later performance pass once
// there's a real client to measure round-trip overhead against.
func (s *playbackSession) run(ctx context.Context, src playbackSource, cameraID string, tsUs int64, sourceRole string, emit playbackEmitter, batches chan<- struct{}) {
	frames, ok, err := src.FirstSegment(ctx, cameraID, tsUs, sourceRole)
	if err != nil || !ok {
		emit.Invoke("onNoData", NvrPlaybackNoData{Ts: tsUs})
		return
	}

	emit.Invoke("onReady", NvrPlaybackReady{
		SessionID:   s.id,
		VideoCodec:  videoCodecH264,
		CodecString: frames.CodecString,
		Width:       frames.Width,
		Height:      frames.Height,
	})

	nextTs := tsUs
	for {
		ts := frames.Timestamps()
		frameIntervalUs := fallbackFrameIntervalUs
		if len(ts) >= 2 {
			frameIntervalUs = ts[1] - ts[0]
		}

		for i, au := range frames.Units {
			if !s.waitToContinue(emit) {
				return
			}

			emit.Invoke("onVideo", NvrPlaybackVideo{Frame: au.Data, Ts: ts[i], Keyframe: au.Keyframe})
			nextTs = ts[i] + frameIntervalUs

			batches <- struct{}{}

			if !s.paceSleep(emit, frameIntervalUs) {
				return
			}
		}

		next, ok, err := src.NextSegment(ctx, cameraID, frames.Segment)
		if err != nil || !ok {
			emit.Invoke("onNoData", NvrPlaybackNoData{Ts: nextTs})
			return
		}
		frames = next
	}
}
