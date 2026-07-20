// Streaming playback frame extraction (Task PLAYBACK): the segment-lookup +
// ffmpeg-extraction + Annex-B access-unit splitting this NVR plugin's
// nvrPlayback RPC handler (rpc_playback.go, package main) drives to stream
// frames from an arbitrary timestamp forward, rolling from segment to
// segment as each one is exhausted.
//
// Like Scrubber (scrub.go) and Generator (thumbs.go), this package never
// resolves its own ffmpeg binary path (callers pass in the already-resolved
// one) and never uses ffprobe. Unlike Scrubber's single-keyframe
// "-frames:v 1" extraction, Player captures a segment's entire remaining
// Annex-B stream from a seek offset in one ffmpeg invocation — still just a
// stream-copy/remux (-c:v copy), not a transcode, so even a full ~60s
// segment completes in a small fraction of that wall-clock time.
//
// codecString parsing reuses annexBCodecString/nalUnits (scrub.go) directly
// — no ffprobe, no second SPS parser.
package media

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// playbackExtractTimeout bounds a single ffmpeg Annex-B extraction/probe
// invocation for streaming playback. Generous relative to scrubTimeout
// (8s, scrub.go) since this drains a whole segment's remainder rather than
// a single frame — but it is still just a stream copy/remux, never a
// transcode, so even a long segment is expected to finish in a small
// fraction of this budget; the generous ceiling exists purely to bound a
// hung/slow ffmpeg process, not because extraction is normally slow.
const playbackExtractTimeout = 30 * time.Second

// defaultPlaybackFPS is the ts-spacing fallback SegmentFrames.Timestamps
// uses when probeVideoInfo can't parse a frame rate out of ffmpeg's stderr
// (unexpected output, a corrupt file, a timed-out probe) — never a reason
// to fail the whole extraction, matching probeResolution's own "best
// effort, fall back rather than error" contract.
const defaultPlaybackFPS = 30.0

// videoFpsRe extracts a video stream's frame rate out of the same `ffmpeg
// -i` stderr dump videoResolutionRe (scrub.go) reads — e.g. the "10 fps,"
// in:
//
//	Stream #0:0: Video: h264 (High), yuv420p, 320x240 [SAR 1:1 DAR 4:3], 10 fps, 10 tbr, 10k tbn, 20 tbc
var videoFpsRe = regexp.MustCompile(`,\s*([0-9]+(?:\.[0-9]+)?)\s*fps\b`)

// AccessUnit is one Annex-B H.264 access unit produced by splitAccessUnits:
// zero or more parameter-set NALs (SPS nal_unit_type 7 / PPS type 8)
// immediately followed by exactly one VCL slice NAL (type 1 non-IDR or type
// 5 IDR), each NAL re-framed with its own 4-byte Annex-B start code. Any
// other NAL type present in the source stream (SEI, AUD, ...) is dropped —
// the task brief's deliberately simplified AU heuristic: "each AU = SPS/PPS
// (if present) + one VCL NAL (type 1 or 5)". This is exactly sufficient for
// a WebCodecs decoder: every AU that needs in-band parameter sets (the one
// containing the stream's first IDR) carries them, and no decoder-relevant
// information is lost by dropping SEI/AUD.
type AccessUnit struct {
	Data     []byte
	Keyframe bool
}

// annexBStartCode is the 4-byte Annex-B start code splitAccessUnits
// re-frames every NAL with. Annex-B parsers accept 3- or 4-byte start
// codes interchangeably, so the choice of 4 here (rather than mirroring
// whatever h264_mp4toannexb happened to emit) is arbitrary but consistent.
var annexBStartCode = []byte{0, 0, 0, 1}

// splitAccessUnits groups data's NAL units (nalUnits, scrub.go) into access
// units — see AccessUnit's doc comment for the exact grouping rule. NAL
// units are scanned in stream order; SPS/PPS units accumulate in a pending
// buffer that is attached to, and cleared by, the very next VCL slice NAL.
// A parameter set with no following VCL NAL before EOF (a truncated/corrupt
// capture) is silently dropped rather than emitted as its own (invalid)
// AU.
func splitAccessUnits(data []byte) []AccessUnit {
	var (
		units   []AccessUnit
		pending [][]byte
	)
	for _, nal := range nalUnits(data) {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 0x1F {
		case 7, 8: // SPS, PPS
			pending = append(pending, nal)
		case 1, 5: // non-IDR slice, IDR slice
			var buf bytes.Buffer
			for _, p := range pending {
				buf.Write(annexBStartCode)
				buf.Write(p)
			}
			buf.Write(annexBStartCode)
			buf.Write(nal)
			units = append(units, AccessUnit{Data: buf.Bytes(), Keyframe: nal[0]&0x1F == 5})
			pending = pending[:0]
		}
	}
	return units
}

// PlaybackSegmentFinder is the subset of *store.SegmentStore Player needs:
// finding the segment covering a moment (like ScrubSegmentFinder,
// scrub.go) plus listing segments in a time range, which nextSegmentAfter
// uses to find whatever immediately follows a given segment for
// streaming's segment-to-segment rollover. *store.SegmentStore satisfies
// this directly.
type PlaybackSegmentFinder interface {
	CoveringSegmentForRole(cameraID, role string, atMs int64) (store.Segment, bool, error)
	InRange(cameraID, role string, startMs, endMs int64) ([]store.Segment, error)
}

// SegmentFrames is one segment's extraction result: every access unit from
// the requested seek offset to the segment's end, plus the codec/
// resolution/fps metadata probed from it. FPS is this segment's own probed
// frame rate — used only for this segment's own Timestamps() spacing, not
// cached/reused across a rollover to the next one, so a fps change between
// two recorded segments (unusual, but not impossible) is still handled
// correctly.
type SegmentFrames struct {
	Segment     store.Segment
	Units       []AccessUnit
	CodecString string
	Width       int
	Height      int
	FPS         float64

	// BaseUs is the absolute playback timestamp (microseconds) Units[0]
	// was extracted at: Segment.StartMs*1000 plus the requested seek
	// offset in microseconds. Because the Annex-B elementary stream ffmpeg
	// hands back here carries no per-frame timestamps of its own (that
	// information lives in the segment's container, which -f h264 strips
	// entirely), every frame's playback ts is synthesized from this base
	// plus its index times the fps-derived frame interval — see
	// Timestamps.
	BaseUs int64
}

// Timestamps returns each Units[i]'s absolute playback timestamp
// (microseconds): BaseUs + i*(1e6/FPS). FPS <= 0 (a probe miss) falls back
// to defaultPlaybackFPS rather than dividing by zero or producing
// degenerate (identical/negative) spacing.
func (f SegmentFrames) Timestamps() []int64 {
	fps := f.FPS
	if fps <= 0 {
		fps = defaultPlaybackFPS
	}
	interval := int64(1_000_000 / fps)
	ts := make([]int64, len(f.Units))
	for i := range ts {
		ts[i] = f.BaseUs + int64(i)*interval
	}
	return ts
}

// Player extracts streaming-playback Annex-B access units from recorded
// segments for the nvrPlayback RPC handler (rpc_playback.go): FirstSegment
// resolves the initial covering segment at a scrub timestamp (the same
// "expected, not a failure" no-covering-segment contract as
// Scrubber.Scrub), and NextSegment rolls forward once one segment's units
// are exhausted.
type Player struct {
	ffmpegPath string
	segments   PlaybackSegmentFinder
	timeout    time.Duration
	log        *sdk.Logger
	runner     frameCommandRunner
}

// NewPlayer returns a Player that reads segments via segments and execs
// ffmpegPath (the resolved ffmpeg binary — see recorder.ResolveFFmpeg().
// Path(); this package never resolves that path itself). log may be nil.
func NewPlayer(ffmpegPath string, segments PlaybackSegmentFinder, log *sdk.Logger) *Player {
	return &Player{
		ffmpegPath: ffmpegPath,
		segments:   segments,
		timeout:    playbackExtractTimeout,
		log:        log,
		runner:     execFrameRunner{},
	}
}

// FirstSegment finds the segment covering tsUs for cameraID under
// sourceRole's resolved role (resolveRole, scrub.go) and extracts every
// access unit from tsUs onward. ok=false (no error) when nothing covers
// tsUs — a scrub position before recording started, or one landing inside
// a segment ffmpeg hasn't finalized/indexed yet — never treated as a
// failure, exactly like Scrubber.Scrub's own contract.
func (p *Player) FirstSegment(ctx context.Context, cameraID string, tsUs int64, sourceRole string) (SegmentFrames, bool, error) {
	role := resolveRole(sourceRole)

	seg, ok, err := p.segments.CoveringSegmentForRole(cameraID, role, tsUs/1000)
	if err != nil {
		return SegmentFrames{}, false, fmt.Errorf("media: find covering segment: %w", err)
	}
	if !ok {
		return SegmentFrames{}, false, nil
	}

	frames, err := p.extractSegment(ctx, seg, scrubOffsetSeconds(seg, tsUs))
	if err != nil {
		return SegmentFrames{}, false, err
	}
	return frames, true, nil
}

// NextSegment finds and extracts, from its own start (offset 0), whatever
// segment immediately follows prev under prev's own camera/role. ok=false
// (no error) when no later segment exists — a real recording gap, or the
// live edge of what has been recorded so far.
func (p *Player) NextSegment(ctx context.Context, cameraID string, prev store.Segment) (SegmentFrames, bool, error) {
	seg, ok, err := p.nextSegmentAfter(cameraID, prev)
	if err != nil {
		return SegmentFrames{}, false, err
	}
	if !ok {
		return SegmentFrames{}, false, nil
	}

	frames, err := p.extractSegment(ctx, seg, 0)
	if err != nil {
		return SegmentFrames{}, false, err
	}
	return frames, true, nil
}

// nextSegmentAfter returns the earliest segment for cameraID/prev.Role
// starting at or after prev.EndMs, excluding prev itself. InRange
// (segments.go) is queried with an unbounded upper edge (math.MaxInt64) —
// there is no natural "how far ahead should I look" cutoff for streaming
// playback the way there is for a fixed preview window.
func (p *Player) nextSegmentAfter(cameraID string, prev store.Segment) (store.Segment, bool, error) {
	rows, err := p.segments.InRange(cameraID, prev.Role, prev.EndMs, math.MaxInt64)
	if err != nil {
		return store.Segment{}, false, fmt.Errorf("media: find next segment: %w", err)
	}
	for _, seg := range rows {
		if seg.ID == prev.ID || seg.StartMs < prev.EndMs {
			continue
		}
		return seg, true, nil
	}
	return store.Segment{}, false, nil
}

// extractSegment runs:
//
//	ffmpeg -ss <offsetSeconds> -i <seg.Path> -c:v copy -bsf:v h264_mp4toannexb -f h264 -
//
// (Scrubber.extractKeyframe's exact invocation, minus "-frames:v 1" — this
// captures the segment's entire remainder, not one frame), splits the
// result into access units, and probes codecString/width/height/fps.
func (p *Player) extractSegment(ctx context.Context, seg store.Segment, offsetSeconds float64) (SegmentFrames, error) {
	genCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	args := []string{
		"-ss", strconv.FormatFloat(offsetSeconds, 'f', 3, 64),
		"-i", seg.Path,
		"-c:v", "copy",
		"-bsf:v", "h264_mp4toannexb",
		"-f", "h264",
		"-",
	}
	raw, err := p.runner.RunCapture(genCtx, p.ffmpegPath, args)
	if err != nil {
		return SegmentFrames{}, fmt.Errorf("media: ffmpeg extract playback segment: %w", err)
	}

	units := splitAccessUnits(raw)

	codecString, err := annexBCodecString(raw)
	if err != nil {
		p.logf("media: playback: parse codecString for %s: %v", seg.Path, err)
	}
	width, height, fps := p.probeVideoInfo(ctx, seg.Path)

	baseUs := seg.StartMs*1000 + int64(offsetSeconds*1_000_000)

	return SegmentFrames{
		Segment:     seg,
		Units:       units,
		CodecString: codecString,
		Width:       width,
		Height:      height,
		FPS:         fps,
		BaseUs:      baseUs,
	}, nil
}

// probeVideoInfo best-effort determines path's video stream's pixel
// dimensions and frame rate without ffprobe (see this file's package doc
// comment). Never returns an error: a parse miss just falls back to 0, 0,
// 0 (width/height "unknown") — Timestamps' own fps<=0 fallback covers the
// zero-fps case.
func (p *Player) probeVideoInfo(ctx context.Context, path string) (width, height int, fps float64) {
	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, p.ffmpegPath, "-hide_banner", "-i", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run() // always "fails" (no -f/output given); stderr is what we want

	out := stderr.String()
	if m := videoResolutionRe.FindStringSubmatch(out); m != nil {
		width, _ = strconv.Atoi(m[1])
		height, _ = strconv.Atoi(m[2])
	}
	if m := videoFpsRe.FindStringSubmatch(out); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v > 0 {
			fps = v
		}
	}
	return width, height, fps
}

// logf logs through p.log if one was provided (log may be nil — see
// NewPlayer's doc comment).
func (p *Player) logf(format string, args ...any) {
	if p.log == nil {
		return
	}
	p.log.Log(fmt.Sprintf(format, args...))
}
