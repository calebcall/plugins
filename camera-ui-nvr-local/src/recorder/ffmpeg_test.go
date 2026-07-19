package recorder

import (
	"path/filepath"
	"testing"
)

// TestSegmentArgs proves segmentArgs builds the exact stream-copy segmenting
// fMP4 command the plan requires: TCP-transport RTSP input, -c copy (no
// transcode), the segment muxer producing segmentSeconds-long fragmented MP4
// files, and an -strftime 1 output pattern (epoch-second filenames)
// segmentTimeRange later parses back out.
func TestSegmentArgs(t *testing.T) {
	ff := &FFmpeg{ffmpegPath: "ffmpeg", ffprobePath: "ffprobe"}

	args := ff.segmentArgs("rtsp://x/y", "/data/cam/high", 60, "high")

	want := []string{
		"-rtsp_transport", "tcp",
		"-i", "rtsp://x/y",
		"-c", "copy",
		"-f", "segment",
		"-segment_time", "60",
		"-segment_format", "mp4",
		"-reset_timestamps", "1",
		"-strftime", "1",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-metadata", "role=high",
		filepath.Join("/data/cam/high", "%s.mp4"),
	}

	if len(args) != len(want) {
		t.Fatalf("segmentArgs() = %v (len %d), want %v (len %d)", args, len(args), want, len(want))
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("segmentArgs()[%d] = %q, want %q\nfull got:  %v\nfull want: %v", i, args[i], want[i], args, want)
		}
	}
}

// TestSegmentArgs_UsesGivenSegmentSecondsAndRole proves segment_time and the
// role metadata tag actually reflect the arguments passed in, not hardcoded
// values that happen to match the first test's fixture.
func TestSegmentArgs_UsesGivenSegmentSecondsAndRole(t *testing.T) {
	ff := &FFmpeg{ffmpegPath: "ffmpeg", ffprobePath: "ffprobe"}

	args := ff.segmentArgs("rtsp://cam2/sub", "/data/cam2/low", 30, "low")

	assertFlagValue(t, args, "-segment_time", "30")
	assertFlagValue(t, args, "-metadata", "role=low")
	assertFlagValue(t, args, "-i", "rtsp://cam2/sub")

	last := args[len(args)-1]
	if last != filepath.Join("/data/cam2/low", "%s.mp4") {
		t.Fatalf("expected output pattern under the given outDir, got %q", last)
	}
}

// assertFlagValue fails the test unless args contains flag immediately
// followed by want.
func assertFlagValue(t *testing.T, args []string, flag, want string) {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 >= len(args) {
				t.Fatalf("flag %q has no following value in %v", flag, args)
			}
			if args[i+1] != want {
				t.Fatalf("flag %q = %q, want %q", flag, args[i+1], want)
			}
			return
		}
	}
	t.Fatalf("expected flag %q in %v", flag, args)
}

// TestResolveFFmpeg_HonorsEnvOverride proves ResolveFFmpeg prefers
// CAMERAUI_FFMPEG_PATH/CAMERAUI_FFPROBE_PATH (the core-injected path) over
// the bare-command-on-PATH fallback when set.
func TestResolveFFmpeg_HonorsEnvOverride(t *testing.T) {
	t.Setenv(envFFmpegPath, "/opt/custom/ffmpeg")
	t.Setenv(envFFprobePath, "/opt/custom/ffprobe")

	ff := ResolveFFmpeg()

	if got := ff.Path(); got != "/opt/custom/ffmpeg" {
		t.Errorf("Path() = %q, want /opt/custom/ffmpeg", got)
	}
	if got := ff.ProbePath(); got != "/opt/custom/ffprobe" {
		t.Errorf("ProbePath() = %q, want /opt/custom/ffprobe", got)
	}
}

// TestResolveFFmpeg_FallsBackToPathWhenEnvUnset proves ResolveFFmpeg falls
// back to the bare command name (resolved against PATH by os/exec at call
// time) when the core hasn't set either env var.
func TestResolveFFmpeg_FallsBackToPathWhenEnvUnset(t *testing.T) {
	t.Setenv(envFFmpegPath, "")
	t.Setenv(envFFprobePath, "")

	ff := ResolveFFmpeg()

	if got := ff.Path(); got != "ffmpeg" {
		t.Errorf("Path() = %q, want ffmpeg", got)
	}
	if got := ff.ProbePath(); got != "ffprobe" {
		t.Errorf("ProbePath() = %q, want ffprobe", got)
	}
}
