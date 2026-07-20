package main

import (
	"context"

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
