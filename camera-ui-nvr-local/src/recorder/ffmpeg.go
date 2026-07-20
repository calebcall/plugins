package recorder

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/cameraui/sdk/go"
)

// envFFmpegPath is the environment variable the core may inject with the
// resolved ffmpeg binary path. Kept purely as a fallback (see
// ResolveFFmpeg/ResolveFFmpegSDK below): the core's master-mode runtime does
// NOT set this for plugins (it hands them an explicit env allow-list with
// neither this var nor PATH set), so the only resolution path that actually
// works in production is the SDK's CoreManager.GetFFmpegPath RPC
// (ResolveFFmpegSDK) — which itself already checks this exact env var before
// making the RPC round trip (github.com/cameraui/sdk/go@v1.1.11/
// manager_core.go). This constant/ResolveFFmpeg remain useful for local dev
// and tests where no core connection exists at all.
const envFFmpegPath = "CAMERAUI_FFMPEG_PATH"

// segmentMovflags is the fMP4 movflags value used for every segmented
// recording: fragmented (frag_keyframe), no trailing moov atom written after
// the whole file is closed (empty_moov — required since the segment muxer
// never "closes" a segment the way a normal one-shot mp4 mux would), and
// base-data-offsets relative to each fragment's own moof rather than the
// file start (default_base_moof) — together these make each segment file
// independently seekable/playable as soon as ffmpeg finishes writing it,
// with no separate remux/finalize pass needed.
const segmentMovflags = "+frag_keyframe+empty_moov+default_base_moof"

// codecProbeTimeout bounds how long probeCodecInfo's ffmpeg invocation is
// allowed to run before it's killed — best-effort codec/audio detection must
// never stall the segment-indexing sweep (sweepSegments, recorder.go)
// indefinitely on a slow or hung ffmpeg process.
const codecProbeTimeout = 5 * time.Second

// videoStreamRe/audioStreamRe extract stream info out of the stderr ffmpeg
// -i always emits for a file's inputs, before it errors out for lack of an
// output (see probeCodecInfo). Sample line this matches against:
//
//	Stream #0:0: Video: h264 (High), yuv420p, 320x240 [SAR 1:1 DAR 4:3], 10 fps, ...
//	Stream #0:1: Audio: aac (LC), 48000 Hz, stereo, fltp, 128 kb/s
var (
	videoStreamRe = regexp.MustCompile(`Stream #\d+:\d+[^:]*: Video: (\w+)`)
	audioStreamRe = regexp.MustCompile(`Stream #\d+:\d+[^:]*: Audio:`)
)

// FFmpeg holds the resolved ffmpeg binary path this recorder execs to spawn
// segmenting recordings (segmentArgs) and to best-effort detect a finished
// segment's audio presence/video codec (probeCodecInfo). There is
// deliberately no ffprobe path here: node-av (the core's bundled media
// toolchain) ships only ffmpeg — no ffprobe binary exists anywhere in
// production — so this package never execs ffprobe for anything; segment
// duration/timing is derived from filenames instead (see recorder.go's
// segmentTimeRange).
type FFmpeg struct {
	ffmpegPath string
}

// FFmpegPathResolver is the subset of *sdk.CoreManager this package needs:
// resolving the ffmpeg binary the core host wants plugins to use. Declared
// here (rather than depending on *sdk.CoreManager directly) purely so tests
// can inject a fake without a live core RPC connection to construct a real
// one; *sdk.CoreManager satisfies this interface as-is, with no adapter
// needed.
type FFmpegPathResolver interface {
	GetFFmpegPath() (string, error)
}

// ResolveFFmpegSDK resolves the ffmpeg binary to run via resolver's
// GetFFmpegPath (github.com/cameraui/sdk/go@v1.1.11's
// CoreManager.GetFFmpegPath — itself already checks CAMERAUI_FFMPEG_PATH
// before making the RPC round trip, and returns the core's
// node-av-bundled ffmpeg binary, e.g. /mnt/.../node-av/binary/ffmpeg,
// otherwise) and falls back to the legacy env/PATH-only resolution
// (ResolveFFmpeg) whenever that call errors or returns an empty path — e.g.
// an older core that doesn't support the RPC, a plugin running with no live
// core connection at all (local dev), or any other transient failure. log
// receives a Warn-level message describing the fallback and why; log may be
// nil (as in tests and every other logging call site in this package).
//
// This is the fix for the confirmed production bug: the core's master-mode
// runtime hands this plugin an explicit env allow-list with neither PATH nor
// CAMERAUI_FFMPEG_PATH set, so ResolveFFmpeg's bare "ffmpeg" fallback never
// resolved to anything there ("exec: ffmpeg: executable file not found in
// $PATH") — only this RPC-based resolution actually works in that runtime.
func ResolveFFmpegSDK(resolver FFmpegPathResolver, log *sdk.Logger) *FFmpeg {
	path, err := resolver.GetFFmpegPath()
	if err != nil {
		if log != nil {
			log.Warn(fmt.Sprintf("recorder: CoreManager.GetFFmpegPath failed (%v); falling back to env/PATH resolution", err))
		}
		return ResolveFFmpeg()
	}

	path = strings.TrimSpace(path)
	if path == "" {
		if log != nil {
			log.Warn("recorder: CoreManager.GetFFmpegPath returned an empty path; falling back to env/PATH resolution")
		}
		return ResolveFFmpeg()
	}

	return &FFmpeg{ffmpegPath: path}
}

// ResolveFFmpeg resolves the ffmpeg binary to run purely from
// CAMERAUI_FFMPEG_PATH/PATH, with no core RPC involved: it honors
// CAMERAUI_FFMPEG_PATH (set by the core host before launching this plugin,
// in runtimes that do set it) and falls back to the bare command name,
// resolved against PATH by os/exec at call time, when unset. Used directly
// by local dev/tests, and as ResolveFFmpegSDK's own fallback when the SDK
// RPC path isn't available.
func ResolveFFmpeg() *FFmpeg {
	return &FFmpeg{ffmpegPath: resolveBinaryPath(envFFmpegPath, "ffmpeg")}
}

func resolveBinaryPath(envKey, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	return fallback
}

// Path returns the ffmpeg binary path (name or absolute path) to exec.
func (f *FFmpeg) Path() string { return f.ffmpegPath }

// segmentArgs builds the ffmpeg CLI args for a single continuous-recording
// role: pull url over RTSP (TCP transport, to avoid UDP packet loss silently
// corrupting recordings) and stream-copy (-c copy — no transcode, per the
// plan's decision to keep CPU/quality untouched) into segmentSeconds-long
// fragmented MP4 files under outDir, named by the epoch-second timestamp
// each segment was opened at (-strftime 1, pattern "%s.mp4" — segmentTimeRange
// parses this back out when a finished segment is indexed). role is stamped
// onto the segment file as a "role" metadata tag purely for operator
// debugging, the recorder itself tracks role via the on-disk directory layout
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

// probeCodecInfo best-effort determines whether path has an audio stream and
// the codec name of its video stream, without ffprobe: ffmpeg -i <file>
// always dumps every input stream's format info to stderr before erroring
// out for lack of an output specified — a well-known technique for reading
// stream info out of ffmpeg alone. Never returns an error and never blocks a
// segment from being indexed on the result: a parse miss (unexpected ffmpeg
// output format/version, a corrupt or still-being-written file, the process
// timing out) just falls back to hasAudio=false, codec="" — see
// finalizeSegment (recorder.go), which always sets HasVideo=true regardless
// (every segment comes from a video RTSP role).
func (f *FFmpeg) probeCodecInfo(path string) (hasAudio bool, codec string) {
	ctx, cancel := context.WithTimeout(context.Background(), codecProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, f.ffmpegPath, "-hide_banner", "-i", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run() // always "fails" (no -f/output given); stderr is what we want

	out := stderr.String()
	if m := videoStreamRe.FindStringSubmatch(out); m != nil {
		codec = m[1]
	}
	hasAudio = audioStreamRe.MatchString(out)
	return hasAudio, codec
}
