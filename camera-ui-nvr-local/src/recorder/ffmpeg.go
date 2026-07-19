package recorder

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Environment variables the core injects with the resolved ffmpeg/ffprobe
// binary paths (see manager_core.go's CoreManager.GetFFmpegPath in the SDK —
// this plugin isn't wired to that RPC yet, but the env-var convention lets
// the host set it without an extra round trip, and always gives us a clean
// fallback for local dev/tests where the host never sets it at all).
const (
	envFFmpegPath  = "CAMERAUI_FFMPEG_PATH"
	envFFprobePath = "CAMERAUI_FFPROBE_PATH"
)

// segmentMovflags is the fMP4 movflags value used for every segmented
// recording: fragmented (frag_keyframe), no trailing moov atom written after
// the whole file is closed (empty_moov — required since the segment muxer
// never "closes" a segment the way a normal one-shot mp4 mux would), and
// base-data-offsets relative to each fragment's own moof rather than the
// file start (default_base_moof) — together these make each segment file
// independently seekable/playable as soon as ffmpeg finishes writing it,
// with no separate remux/finalize pass needed.
const segmentMovflags = "+frag_keyframe+empty_moov+default_base_moof"

// FFmpeg holds the resolved ffmpeg/ffprobe binary paths this recorder uses
// to spawn segmenting recordings (segmentArgs) and to probe finished
// segments for duration/codecs (probe).
type FFmpeg struct {
	ffmpegPath  string
	ffprobePath string
}

// ResolveFFmpeg resolves the ffmpeg/ffprobe binaries to run: each honors its
// own CAMERAUI_*_PATH env var (set by the core host before launching this
// plugin) and falls back to the bare command name, resolved against PATH by
// os/exec at call time, when unset.
func ResolveFFmpeg() *FFmpeg {
	return &FFmpeg{
		ffmpegPath:  resolveBinaryPath(envFFmpegPath, "ffmpeg"),
		ffprobePath: resolveBinaryPath(envFFprobePath, "ffprobe"),
	}
}

func resolveBinaryPath(envKey, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	return fallback
}

// Path returns the ffmpeg binary path (name or absolute path) to exec.
func (f *FFmpeg) Path() string { return f.ffmpegPath }

// ProbePath returns the ffprobe binary path (name or absolute path) to exec.
func (f *FFmpeg) ProbePath() string { return f.ffprobePath }

// segmentArgs builds the ffmpeg CLI args for a single continuous-recording
// role: pull url over RTSP (TCP transport, to avoid UDP packet loss silently
// corrupting recordings) and stream-copy (-c copy — no transcode, per the
// plan's decision to keep CPU/quality untouched) into segmentSeconds-long
// fragmented MP4 files under outDir, named by the epoch-second timestamp
// each segment was opened at (-strftime 1, pattern "%s.mp4" — segmentTimeRange
// parses this back out when a finished segment is indexed). role is stamped
// onto the segment file as a "role" metadata tag purely for operator
// debugging (e.g. inspecting a file with ffprobe) — the recorder itself
// tracks role via the on-disk directory layout
// (recordings/<cameraId>/<date>/<hour>/<role>/), not this tag.
func (f *FFmpeg) segmentArgs(url, outDir string, segmentSeconds int, role string) []string {
	pattern := filepath.Join(outDir, "%s.mp4")
	return []string{
		"-rtsp_transport", "tcp",
		"-i", url,
		"-c", "copy",
		"-f", "segment",
		"-segment_time", strconv.Itoa(segmentSeconds),
		"-segment_format", "mp4",
		"-reset_timestamps", "1",
		"-strftime", "1",
		"-movflags", segmentMovflags,
		"-metadata", "role=" + role,
		pattern,
	}
}

// probeStream is the subset of one `ffprobe -show_streams` stream entry
// finalizeSegment needs.
type probeStream struct {
	CodecType string `json:"codec_type"`
	CodecName string `json:"codec_name"`
}

// probeFormat is the subset of `ffprobe -show_format` finalizeSegment needs.
type probeFormat struct {
	Duration string `json:"duration"`
}

// probeResult is the parsed `ffprobe -print_format json -show_format
// -show_streams` output for one file.
type probeResult struct {
	Streams []probeStream `json:"streams"`
	Format  probeFormat   `json:"format"`
}

// durationMs parses the probed container duration (seconds, as a decimal
// string — ffprobe's JSON format always emits it that way) into whole
// milliseconds. ok is false if the field is missing/unparseable (e.g. a
// truncated file ffprobe could open but not fully measure).
func (r probeResult) durationMs() (ms int64, ok bool) {
	seconds, err := strconv.ParseFloat(r.Format.Duration, 64)
	if err != nil {
		return 0, false
	}
	return int64(seconds * 1000), true
}

// videoAudio reduces the probed stream list to the three flags
// finalizeSegment needs: whether any video/audio stream is present, and the
// codec name of the first video stream (empty if there is none).
func (r probeResult) videoAudio() (hasVideo, hasAudio bool, codec string) {
	for _, s := range r.Streams {
		switch s.CodecType {
		case "video":
			hasVideo = true
			if codec == "" {
				codec = s.CodecName
			}
		case "audio":
			hasAudio = true
		}
	}
	return hasVideo, hasAudio, codec
}

// probe shells out to ffprobe for path's format/stream metadata.
func (f *FFmpeg) probe(path string) (probeResult, error) {
	cmd := exec.Command(f.ffprobePath, "-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", path)
	out, err := cmd.Output()
	if err != nil {
		return probeResult{}, fmt.Errorf("recorder: ffprobe %s: %w", path, err)
	}

	var res probeResult
	if err := json.Unmarshal(out, &res); err != nil {
		return probeResult{}, fmt.Errorf("recorder: ffprobe %s: parse output: %w", path, err)
	}
	return res, nil
}
