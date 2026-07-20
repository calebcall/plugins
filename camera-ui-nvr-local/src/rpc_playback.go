// Streaming playback wire mechanism (findings, Task PLAYBACK)
//
// nvrPlayback(cameraID, tsUs, videoOnly, sourceRole, callbacks) hands the
// frontend an AsyncGenerator<void> and five named callbacks bundled into
// one object (NvrPlaybackCallbacks: onReady/onVideo/onBatch/onAudio/
// onNoData) — neither the push-stream mechanism (isStreamRequest/
// handleStreamRequestGo, github.com/cameraui/rpc/go@v1.0.6/handler.go +
// handler_stream.go: a handler returns a channel/slice, the framework
// drains it and republishes each value as one push message — no callback
// bundling at all) nor the single-func(T) callback-subscription mechanism
// this plugin already uses for onRecordingState/onSystemEvent
// (handleCallbackRequestGo, handler_callback.go: exactly one func(T)
// parameter, found by scanning for the first reflect.Func-kind parameter)
// can carry five independently-named callbacks through one call. Reading
// github.com/cameraui/rpc/go@v1.0.6's remaining mechanism —
// isPullCallbackRequest / handlePullCallbackRequestGo (handler_pull_callback.go)
// / CallPullIteratorWithCallback (client_pull_callback.go) — the
// "pull-iterator-with-callbacks" protocol is exactly this: a client-side
// map[string]any of named callbacks plus an explicit oneway list, fired
// via a *CallbackInvoker the server-side handler receives as its LAST
// parameter, with the handler returning a channel whose element type is
// irrelevant (every value/close is a pure batch-boundary signal — see
// handler_pull_callback.go: "Value is ignored by the protocol — batch
// boundary only").
//
// Cross-checked against the compiled frontend worker
// (/tmp/nvr-spike/package/dist/index.js, package @camera.ui/camera-ui-nvr):
// the play() path does
//
//	let o=La({onReady:...,onVideo:...,onBatch:...,onAudio:...,onNoData:...},{oneway:[...Kc]})
//	...
//	s=a.nvrPlayback(this.cameraId, e, t, n, o)
//	...
//	for await(let t of s) ...
//
// where Kc=[`onReady`,`onVideo`,`onBatch`,`onAudio`,`onNoData`] and La
// marks its first argument with two hidden Symbol-keyed properties
// (rpc.callbacks / rpc.callbacks.oneway — both present as string literals
// in the bundle) the RPC proxy's call-site detects via Ra(e) (checking the
// rpc.callbacks marker): whichever positional argument carries that marker
// is stripped out of the plain args array and instead routed through
// e.callPullIteratorWithCallback(subject, callbacksObj, onewayList,
// ...remainingArgs) — i.e. the wire payload's `args` is exactly
// [cameraID, tsUs, videoOnly, sourceRole], four values, with the callbacks
// object never appearing positionally at all. That fixes this handler's Go
// signature exactly: four ordinary parameters plus *rpc.CallbackInvoker as
// the fifth/last (handlePullCallbackRequestGo's own hard requirement —
// "pull-callback handler's last parameter must be *CallbackInvoker"),
// returning (<-chan struct{}, error):
//
//	func (p *NVRPlugin) NvrPlayback(cameraID string, tsUs int64, videoOnly bool, sourceRole string, invoker *rpc.CallbackInvoker) (<-chan struct{}, error)
//
// invoker.Invoke("onReady", payload) etc. publishes a oneway
// CallbackInvocation the client dispatches straight to the matching named
// callback (dispatchCallback, client_pull_callback.go) — confirmed
// deliverable for every one of the five names here, since the frontend
// passed {oneway:[...Kc]} (all five, not a subset). invoker.Active()
// (false once the client cancels — its for-await loop breaking/returning
// early, or a plain disconnect) is the only cancellation signal this
// handler ever receives; there is no ctx parameter and no separate
// "stop" callback, which is why playbackSession.run/waitToContinue/
// paceSleep (playback_session.go) poll it directly rather than relying on
// any context.Context cancellation.
//
// nvrPlaybackCmd(sessionID, cmd) is an ordinary request/response RPC by
// contrast (the frontend does a plain `await t.nvrPlaybackCmd(id, cmd)`,
// no callback bundle argument) — nothing pins its shape beyond the usual
// exported-method convention every other handler in this package already
// follows.
package main

import (
	"context"
	"fmt"

	rpc "github.com/cameraui/rpc/go"
	"github.com/google/uuid"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/media"
)

// videoCodecH264 is the only videoCodec value NvrScrub/NvrPreviewFrames
// ever report — every segment this NVR plugin records/serves is H.264
// (recorder/ffmpeg.go's segmentArgs stream-copies whatever the camera
// sends, and every fixture/production camera this plugin has been built
// against is H.264; there is no per-segment codec branch here yet).
const videoCodecH264 = "h264"

// scrubber is the subset of *media.Scrubber the nvrScrub/nvrPreviewFrames
// RPC handlers need (Scrub, PreviewFrames) — declared as an interface here,
// rather than depending on *media.Scrubber directly, purely so tests can
// inject a fake without a real ffmpeg process or SQLite-backed
// SegmentStore; *media.Scrubber satisfies this directly, no adapter
// needed.
type scrubber interface {
	Scrub(ctx context.Context, cameraID string, tsUs int64, sourceRole string) (media.ScrubResult, error)
	PreviewFrames(ctx context.Context, cameraID string, startUs, endUs int64, count int) (media.PreviewResult, error)
}

// boolPtr returns a pointer to v — used to populate NvrScrubResult.NoData/
// NvrPreviewResult.NoData, which are *bool (not bare bool) on the wire so
// the common "false" case never has to round-trip at all (see wire.go's
// doc comments on both types).
func boolPtr(v bool) *bool { return &v }

// NvrScrub returns a single Annex-B H.264 keyframe for cameraID at tsUs (a
// microsecond timestamp) — the playback frame path's phase 1 (Task
// SCRUB): proving the exact frame encoding the closed frontend's WebCodecs
// decoder expects before streaming playback (nvrPlayback/nvrPlaybackCmd,
// a later task) is built on top of it. fine and sourceRole are accepted
// per the wire contract (nvrScrub(cameraID, tsUs, fine?, sourceRole?)) —
// fine (a hint for frame-exact vs. keyframe-only precision) is not yet
// acted on: this v1 always returns the keyframe at/before tsUs (see
// media.Scrubber.Scrub's doc comment on why that's exactly right for a
// scrub use case), and NvrScrubResult.Frames is left empty rather than
// populated with a decode window around it, matching the task brief's
// "returning the single primary frame is fine for v1" guidance.
// sourceRole selects which recorded stream role to draw from (resolved by
// media.Scrubber's resolveRole); "" defaults to the recorder's own default
// recording role ("high-resolution"). Registered as the RPC method
// "nvrScrub".
//
// NoData (with no error) is the expected, non-error response when no
// recorded segment covers tsUs for the resolved role at all — e.g. a scrub
// position before recording started, one landing inside a still-open
// segment ffmpeg hasn't finalized/indexed yet, or this plugin's database
// failed to open (p.scrubber nil) — never surfaced as an RPC error, since
// the frontend's scrubber is expected to handle "nothing here yet"
// gracefully for exactly these reasons.
func (p *NVRPlugin) NvrScrub(cameraID string, tsUs int64, fine bool, sourceRole string) (NvrScrubResult, error) {
	p.logRPC("nvrScrub", cameraID)

	noData := NvrScrubResult{Ts: tsUs, VideoCodec: videoCodecH264, NoData: boolPtr(true)}
	if p.scrubber == nil {
		return noData, nil
	}

	result, err := p.scrubber.Scrub(context.Background(), cameraID, tsUs, sourceRole)
	if err != nil {
		return NvrScrubResult{}, err
	}
	// Debugging aid for "scrub reports noData" reports: without this, the
	// only signal an operator has is the boolean NoData on the wire
	// response — this pins down exactly what tsUs was requested and
	// whether/where a covering segment was actually found, straight from
	// the plugin's own log, with no need to reproduce against a live
	// SegmentStore.
	p.logRPC("nvrScrub", cameraID, fmt.Sprintf("tsUs=%d segmentFound=%v segStartMs=%d segEndMs=%d", tsUs, result.Found, result.SegmentStartMs, result.SegmentEndMs))
	if !result.Found {
		return noData, nil
	}

	return NvrScrubResult{
		Frame:       result.Frame,
		Ts:          tsUs,
		VideoCodec:  videoCodecH264,
		CodecString: result.CodecString,
		Width:       result.Width,
		Height:      result.Height,
	}, nil
}

// NvrPreviewFrames returns a filmstrip of up to count evenly-spaced Annex-B
// H.264 keyframes across [startUs, endUs] for cameraID — the timeline
// scrubber's hover-preview thumbnails. count defaults to 10 when <= 0 (see
// media.Scrubber.PreviewFrames' defaultPreviewCount) — the RPC dispatcher's
// zero-fill for an omitted optional parameter (the wire contract's
// nvrPreviewFrames(..., count?: number)) already produces exactly that
// "not supplied" value, count == 0, so no separate nil-vs-zero distinction
// is needed here. Registered as the RPC method "nvrPreviewFrames".
//
// NoData (with no error) is the expected, non-error response when none of
// the sampled points resolved to a covering segment at all (an empty
// range, or one entirely before recording started) — same "expected, not a
// failure" contract as NvrScrub's.
func (p *NVRPlugin) NvrPreviewFrames(cameraID string, startUs, endUs int64, count int) (NvrPreviewResult, error) {
	p.logRPC("nvrPreviewFrames", cameraID)

	if p.scrubber == nil {
		return NvrPreviewResult{Frames: []NvrScrubFrame{}, VideoCodec: videoCodecH264, NoData: boolPtr(true)}, nil
	}

	result, err := p.scrubber.PreviewFrames(context.Background(), cameraID, startUs, endUs, count)
	if err != nil {
		return NvrPreviewResult{}, err
	}

	frames := make([]NvrScrubFrame, 0, len(result.Frames))
	for _, f := range result.Frames {
		frames = append(frames, NvrScrubFrame{Frame: f.Data, Ts: f.TsUs, Keyframe: f.Keyframe})
	}

	out := NvrPreviewResult{
		Frames:      frames,
		VideoCodec:  videoCodecH264,
		CodecString: result.CodecString,
		Width:       result.Width,
		Height:      result.Height,
	}
	if result.NoData {
		out.NoData = boolPtr(true)
	}
	return out, nil
}

// NvrPlayback starts a streaming-playback session for cameraID from tsUs
// (a microsecond timestamp) and returns immediately with a channel the
// pull-callback framework drives one value per emitted video frame (see
// this file's package doc comment for why the channel's element type and
// values are otherwise meaningless — pure batch-boundary signals). The
// actual payloads reach the client exclusively through invoker.Invoke:
// onReady once, then onVideo per access unit, until onNoData (no covering
// segment at all, or a real recording gap partway through) ends the
// session. Registered as the RPC method "nvrPlayback".
//
// sourceRole is resolved the same way NvrScrub's is (media.resolveRole,
// via *media.Player); videoOnly is accepted per the wire contract but not
// yet acted on — see playbackSession.run's doc comment (audio streaming
// is deferred in this v1).
//
// A nil p.player (store.Open failed in NewPlugin, mirroring every other
// db-backed field's nil guard in this package) reports onNoData
// immediately rather than panicking, on its own goroutine so this method
// still returns a live channel either way — handlePullCallbackRequestGo
// requires a channel back, not an error, for "nothing to stream".
func (p *NVRPlugin) NvrPlayback(cameraID string, tsUs int64, videoOnly bool, sourceRole string, invoker *rpc.CallbackInvoker) (<-chan struct{}, error) {
	p.logRPC("nvrPlayback", cameraID, fmt.Sprintf("tsUs=%d videoOnly=%v sourceRole=%q", tsUs, videoOnly, sourceRole))

	batches := make(chan struct{})

	if p.player == nil {
		go func() {
			defer close(batches)
			invoker.Invoke("onNoData", NvrPlaybackNoData{Ts: tsUs})
		}()
		return batches, nil
	}

	sess := newPlaybackSession(uuid.NewString())
	p.playbackSessions.add(sess)

	go func() {
		defer close(batches)
		defer p.playbackSessions.remove(sess.id)
		sess.run(context.Background(), p.player, cameraID, tsUs, sourceRole, invoker, batches)
	}()

	return batches, nil
}

// NvrPlaybackCmd adjusts a live nvrPlayback session's emission: pause
// stops it, resume continues it, speed changes its pacing multiplier (see
// playbackSession's Pause/Resume/SetSpeed). A sessionID with no
// currently-registered session (already ended, or never existed) is a
// no-op, not an error — see playbackSessionRegistry.get's doc comment for
// why that race is expected, not a bug. Registered as the RPC method
// "nvrPlaybackCmd".
func (p *NVRPlugin) NvrPlaybackCmd(sessionID string, cmd NvrPlaybackCommand) error {
	p.logRPC("nvrPlaybackCmd", sessionID, cmd.Cmd)

	sess, ok := p.playbackSessions.get(sessionID)
	if !ok {
		return nil
	}

	switch cmd.Cmd {
	case "pause":
		sess.Pause()
	case "resume":
		sess.Resume()
	case "speed":
		sess.SetSpeed(cmd.Speed)
	}
	return nil
}
