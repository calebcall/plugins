// Scrub/preview-frame extraction (Task SCRUB): serves the playback frame
// path's phase 1 — a single Annex-B H.264 keyframe for a scrub timestamp
// (nvrScrub) and a filmstrip of evenly-spaced keyframes across a range
// (nvrPreviewFrames) — proving the exact frame encoding the closed
// frontend's WebCodecs decoder expects (in-band SPS/PPS, start codes, no
// `description`) before streaming playback (nvrPlayback/nvrPlaybackCmd) is
// built on top of it.
//
// Like Generator (thumbs.go), this package never resolves its own ffmpeg
// binary path (callers pass in the already-resolved one — see
// recorder.ResolveFFmpegSDK/ResolveFFmpeg) and never uses ffprobe: node-av
// (the core's bundled media toolchain) ships no ffprobe binary at all.
// width/height come from parsing `ffmpeg -i <path>`'s stderr (the same
// "ffmpeg -i always dumps stream info to stderr before erroring out for
// lack of an output" technique recorder.FFmpeg.probeCodecInfo already uses
// for codec/audio detection — duplicated here, not imported, since that
// method is unexported and only captures the codec name, not resolution).
package media

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// scrubTimeout bounds a single ffmpeg keyframe-extraction or resolution-probe
// invocation — scrubbing/previewing is interactive (a user dragging a
// timeline scrubber) and must never let a hung/slow ffmpeg process stall a
// request indefinitely.
const scrubTimeout = 8 * time.Second

// defaultPreviewCount is how many evenly-spaced keyframes PreviewFrames
// samples across the requested range when count is <= 0 (the wire
// contract's nvrPreviewFrames(..., count?: number) — count is optional).
const defaultPreviewCount = 10

// videoResolutionRe extracts a video stream's pixel dimensions out of the
// stderr ffmpeg -i always emits for a file's inputs, before it errors out
// for lack of an output specified (see probeResolution). Sample line this
// matches against:
//
//	Stream #0:0: Video: h264 (High) (avc1 / 0x31637661), yuv420p, 320x240 [SAR 1:1 DAR 4:3], ...
//
// The pixel-dimensions "WxH" is required to be preceded by ", " and
// followed by "," or whitespace: ffmpeg's codec-tag aside — "(avc1 /
// 0x31637661)" — also matches a naive "\d+x\d+" pattern (width=0,
// height=31637661!) since a hex literal is itself digits-'x'-digits; that
// aside is always preceded by "/ ", never ", ", so requiring the comma
// disambiguates the two. Go's regexp "." never matches '\n' by default (no
// (?s) flag here), so this can't accidentally span past this one stream's
// line into another.
var videoResolutionRe = regexp.MustCompile(`Stream #\d+:\d+[^:]*: Video:.*?,\s*(\d+)x(\d+)[,\s]`)

// ScrubSegmentFinder is the subset of *store.SegmentStore Scrubber needs:
// finding the recorded segment (if any) for a specific camera+role covering
// a moment in time. *store.SegmentStore satisfies this directly via
// CoveringSegmentForRole.
type ScrubSegmentFinder interface {
	CoveringSegmentForRole(cameraID, role string, atMs int64) (store.Segment, bool, error)
}

// frameCommandRunner abstracts running one ffmpeg invocation and capturing
// its stdout bytes, so tests can inject a fake for the non-ffmpeg-dependent
// cases (e.g. "no covering segment") while the TDD-required real-media
// tests exercise a genuine ffmpeg process via execFrameRunner. This differs
// from thumbs.go's commandRunner (which only reports success/failure,
// writing its real output to a JPEG file on disk) because a scrub frame's
// output *is* the RPC response payload: it's read straight off ffmpeg's
// stdout pipe, never touching disk.
type frameCommandRunner interface {
	RunCapture(ctx context.Context, name string, args []string) ([]byte, error)
}

type execFrameRunner struct{}

func (execFrameRunner) RunCapture(ctx context.Context, name string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			return nil, fmt.Errorf("%w: %s", err, tail)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// Frame is one extracted Annex-B H.264 access unit plus its playback
// timestamp — the Go-side shape PreviewFrames/Scrub build, mapped onto the
// wire's NvrScrubFrame by the RPC handler (rpc_playback.go).
type Frame struct {
	Data     []byte
	TsUs     int64
	Keyframe bool
}

// ScrubResult is Scrub's return value: either a found keyframe with its
// codec metadata, or Found=false (no error) when nothing covers the
// requested timestamp. SegmentStartMs/SegmentEndMs (zero when !Found) exist
// purely so callers can log which recorded segment (if any) actually
// covered the request — see rpc_playback.go's NvrScrub, which logs them to
// diagnose "scrub reports noData" reports without needing to reproduce
// against a live SegmentStore.
type ScrubResult struct {
	Frame          []byte
	CodecString    string
	Width          int
	Height         int
	Found          bool
	SegmentStartMs int64
	SegmentEndMs   int64
}

// PreviewResult is PreviewFrames' return value: the sampled keyframes plus
// shared codec metadata (all frames in one preview request come from the
// same camera/role, so codecString/width/height are reported once, not
// per-frame — matching the wire contract's NvrPreviewResult shape).
type PreviewResult struct {
	Frames      []Frame
	CodecString string
	Width       int
	Height      int
	NoData      bool
}

// Scrubber extracts single Annex-B H.264 keyframes (Scrub) and
// evenly-spaced filmstrips of them (PreviewFrames) from recorded segments,
// using ffmpeg's own `-ss`-before-`-i` input seeking (fast, keyframe-
// granularity — exactly right for scrub/preview, which only ever need "the
// keyframe at/before this moment", never frame-exact precision) plus the
// h264_mp4toannexb bitstream filter to convert the segment's stored
// length-prefixed (AVCC) NAL encoding into the start-code-delimited
// Annex-B format, with in-band SPS/PPS, the frontend's WebCodecs decoder
// requires (see this package's doc comment).
type Scrubber struct {
	ffmpegPath string
	segments   ScrubSegmentFinder
	timeout    time.Duration
	log        *sdk.Logger
	runner     frameCommandRunner
}

// NewScrubber returns a Scrubber that reads segments via segments and execs
// ffmpegPath (the resolved ffmpeg binary — see recorder.ResolveFFmpeg().
// Path(); this package never resolves that path itself) to extract frames.
// log may be nil.
func NewScrubber(ffmpegPath string, segments ScrubSegmentFinder, log *sdk.Logger) *Scrubber {
	return &Scrubber{
		ffmpegPath: ffmpegPath,
		segments:   segments,
		timeout:    scrubTimeout,
		log:        log,
		runner:     execFrameRunner{},
	}
}

// resolveRole maps the wire contract's NvrSourceRole ("" | 'high' | 'mid' |
// 'low' | 'scrub' | arbitrary string) onto the store.Segment role string
// SegmentStore rows are actually indexed under. "" (unset), "high", and
// "scrub" (the contract's dedicated low-bandwidth scrub-preview role, which
// this NVR plugin doesn't record a separate stream for) all fall back to
// the recorder's own default recording role, sdk.CameraRoleHighRes
// ("high-resolution") — matching recorder/manager.go's defaultRoles and
// rpc_recording.go's recordingRolesFor, which make the same default.
// Anything else not recognized (a custom role string the contract's type
// allows for future extension) passes through unchanged, on the theory
// that a caller supplying an exact role string already knows what
// SegmentStore role it wants.
func resolveRole(sourceRole string) string {
	switch sourceRole {
	case "", "high", "scrub":
		return string(sdk.CameraRoleHighRes)
	case "mid":
		return string(sdk.CameraRoleMidRes)
	case "low":
		return string(sdk.CameraRoleLowRes)
	default:
		return sourceRole
	}
}

// scrubOffsetSeconds computes how far into seg (in seconds) tsUs (a
// microsecond timestamp) falls, clamped to [0, seg duration) the same way
// thumbs.go's frameOffsetSeconds is — a slightly-out-of-range timestamp
// (e.g. right at a segment's own boundary, due to independent clock
// sources) still resolves to a valid seek target inside the file rather
// than one ffmpeg would refuse or seek past the end with. Per the task
// brief: offset seconds = (tsUs/1000 - segment.StartMs)/1000.
func scrubOffsetSeconds(seg store.Segment, tsUs int64) float64 {
	tsMs := tsUs / 1000
	offsetMs := tsMs - seg.StartMs
	if offsetMs < 0 {
		offsetMs = 0
	}
	if durMs := seg.EndMs - seg.StartMs; durMs > 0 && offsetMs >= durMs {
		offsetMs = durMs - 1
	}
	return float64(offsetMs) / 1000.0
}

// Scrub extracts a single Annex-B keyframe for cameraID at tsUs (a
// microsecond timestamp), from the segment covering it under sourceRole's
// resolved SegmentStore role (resolveRole). Returns Found=false, with no
// error, when no segment covers tsUs for that role — an expected condition
// (a scrub position before recording started, or landing inside a segment
// ffmpeg hasn't finalized/indexed yet), never treated as a failure, exactly
// like media.Generator's own CoveringSegment contract.
func (s *Scrubber) Scrub(ctx context.Context, cameraID string, tsUs int64, sourceRole string) (ScrubResult, error) {
	role := resolveRole(sourceRole)

	seg, ok, err := s.segments.CoveringSegmentForRole(cameraID, role, tsUs/1000)
	if err != nil {
		return ScrubResult{}, fmt.Errorf("media: find covering segment: %w", err)
	}
	if !ok {
		return ScrubResult{}, nil
	}

	frame, err := s.extractKeyframe(ctx, seg.Path, scrubOffsetSeconds(seg, tsUs))
	if err != nil {
		return ScrubResult{}, err
	}

	codecString, err := annexBCodecString(frame)
	if err != nil {
		s.logf("media: scrub: parse codecString for %s: %v", seg.Path, err)
	}
	width, height := s.probeResolution(ctx, seg.Path)

	return ScrubResult{
		Frame:          frame,
		CodecString:    codecString,
		Width:          width,
		Height:         height,
		Found:          true,
		SegmentStartMs: seg.StartMs,
		SegmentEndMs:   seg.EndMs,
	}, nil
}

// PreviewFrames samples up to count evenly-spaced keyframes across
// [startUs, endUs] for cameraID (defaulting to the recorder's default
// recording role — see resolveRole(""); the wire contract's
// nvrPreviewFrames has no sourceRole parameter of its own, unlike
// nvrScrub). Per the task brief, this keeps v1 simple: one ffmpeg
// keyframe-extraction invocation per sample point, rather than a single
// fps-filtered call. A sample point with no covering segment is silently
// skipped (not an error) — e.g. a preview range spanning a recording gap —
// so the result may have fewer than count frames; NoData is true only when
// none of them resolved to a frame at all.
func (s *Scrubber) PreviewFrames(ctx context.Context, cameraID string, startUs, endUs int64, count int) (PreviewResult, error) {
	if count <= 0 {
		count = defaultPreviewCount
	}
	role := resolveRole("")

	var (
		frames        []Frame
		codecString   string
		width, height int
	)
	for i := 0; i < count; i++ {
		ts := sampleTimestamp(startUs, endUs, i, count)

		seg, ok, err := s.segments.CoveringSegmentForRole(cameraID, role, ts/1000)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("media: find covering segment: %w", err)
		}
		if !ok {
			continue
		}

		frame, err := s.extractKeyframe(ctx, seg.Path, scrubOffsetSeconds(seg, ts))
		if err != nil {
			s.logf("media: previewFrames: extract frame at %dus: %v", ts, err)
			continue
		}

		if codecString == "" {
			if cs, err := annexBCodecString(frame); err == nil {
				codecString = cs
			}
		}
		if width == 0 {
			width, height = s.probeResolution(ctx, seg.Path)
		}

		frames = append(frames, Frame{Data: frame, TsUs: ts, Keyframe: true})
	}

	return PreviewResult{
		Frames:      frames,
		CodecString: codecString,
		Width:       width,
		Height:      height,
		NoData:      len(frames) == 0,
	}, nil
}

// sampleTimestamp returns the i-th of count evenly-spaced timestamps across
// [startUs, endUs] (inclusive of both ends: i==0 -> startUs, i==count-1 ->
// endUs). count==1 returns startUs.
func sampleTimestamp(startUs, endUs int64, i, count int) int64 {
	if count <= 1 {
		return startUs
	}
	span := endUs - startUs
	return startUs + span*int64(i)/int64(count-1)
}

// extractKeyframe runs the task brief's exact extraction invocation:
//
//	ffmpeg -ss <offsetSeconds> -i <path> -frames:v 1 -c:v copy -bsf:v h264_mp4toannexb -f h264 -
//
// -ss before -i is input seeking (fast, keyframe-granularity — ffmpeg seeks
// to the keyframe at/before offsetSeconds, exactly right for scrub/preview,
// which want "the keyframe here", not frame-exact decode). -c:v copy
// (stream copy, no transcode) plus the h264_mp4toannexb bitstream filter
// convert the segment's stored AVCC (length-prefixed) NAL encoding into
// Annex-B (start-code-delimited, in-band SPS/PPS) without touching a single
// encoded byte — exactly the format the frontend's WebCodecs decoder
// requires (see this package's doc comment). stdout is the resulting single
// Annex-B access unit.
func (s *Scrubber) extractKeyframe(ctx context.Context, path string, offsetSeconds float64) ([]byte, error) {
	genCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	args := []string{
		"-ss", strconv.FormatFloat(offsetSeconds, 'f', 3, 64),
		"-i", path,
		"-frames:v", "1",
		"-c:v", "copy",
		"-bsf:v", "h264_mp4toannexb",
		"-f", "h264",
		"-",
	}
	data, err := s.runner.RunCapture(genCtx, s.ffmpegPath, args)
	if err != nil {
		return nil, fmt.Errorf("media: ffmpeg extract keyframe: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("media: ffmpeg produced no frame data for %s", path)
	}
	return data, nil
}

// probeResolution best-effort determines path's video stream's pixel
// dimensions without ffprobe (see this package's doc comment). Never
// returns an error: a parse miss (unexpected ffmpeg output, a corrupt
// file, the process timing out) just falls back to 0, 0 — callers treat
// that as "resolution unknown", never as a reason to fail the whole
// scrub/preview request, since the extracted frame itself is still valid
// and usable without it.
func (s *Scrubber) probeResolution(ctx context.Context, path string) (width, height int) {
	probeCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, s.ffmpegPath, "-hide_banner", "-i", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run() // always "fails" (no -f/output given); stderr is what we want

	m := videoResolutionRe.FindStringSubmatch(stderr.String())
	if m == nil {
		return 0, 0
	}
	w, errW := strconv.Atoi(m[1])
	h, errH := strconv.Atoi(m[2])
	if errW != nil || errH != nil {
		return 0, 0
	}
	return w, h
}

// logf logs through s.log if one was provided (log may be nil — see
// NewScrubber's doc comment).
func (s *Scrubber) logf(format string, args ...any) {
	if s.log == nil {
		return
	}
	s.log.Log(fmt.Sprintf(format, args...))
}

// nalUnits splits Annex-B data into each NAL unit it contains (the header
// byte included, start-code bytes excluded), by scanning for 3-byte start
// codes (00 00 01 — a 4-byte start code 00 00 00 01 is just a 3-byte one
// with an extra leading zero, matched at the same 00 00 01 position, one
// byte later than the leading zero). Only ever used to find/read the first
// few bytes of specific NAL types (SPS, IDR) — see annexBCodecString and
// scrub_test.go's containsNALType — never to reconstruct exact NAL
// boundaries for re-muxing, so imprecision at a NAL's trailing edge (e.g.
// leftover zero_byte padding folded into the wrong neighbor) is harmless
// here.
func nalUnits(data []byte) [][]byte {
	var starts []int
	for i := 0; i+2 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			starts = append(starts, i+3)
		}
	}

	nals := make([][]byte, 0, len(starts))
	for idx, s := range starts {
		end := len(data)
		if idx+1 < len(starts) {
			end = starts[idx+1] - 3
			for end > s && data[end-1] == 0 {
				end--
			}
		}
		if s < end {
			nals = append(nals, data[s:end])
		}
	}
	return nals
}

// annexBCodecString parses the SPS NAL (nal_unit_type 7) out of frame and
// builds the wire contract's codecString: "avc1." + hex2(profile_idc) +
// hex2(constraint_flags) + hex2(level_idc), read from the first 3 bytes of
// the SPS NAL's payload (right after its 1-byte NAL header) — per the task
// brief, this is reliable and needs no ffprobe (profile_idc/
// constraint_flags/level_idc are always the first 3 bytes of a raw H.264
// SPS, never subject to RBSP emulation-prevention escaping this early in
// the bitstream). Returns an error if frame contains no SPS NAL at all
// (e.g. a corrupt/empty extraction) — callers treat that as "codecString
// unknown", not a reason to fail the whole request (see Scrub).
func annexBCodecString(frame []byte) (string, error) {
	for _, nal := range nalUnits(frame) {
		if len(nal) < 4 {
			continue
		}
		if nal[0]&0x1F != 7 {
			continue
		}
		profileIdc := nal[1]
		constraintFlags := nal[2]
		levelIdc := nal[3]
		return fmt.Sprintf("avc1.%02x%02x%02x", profileIdc, constraintFlags, levelIdc), nil
	}
	return "", fmt.Errorf("media: no SPS NAL (type 7) found in extracted frame")
}
