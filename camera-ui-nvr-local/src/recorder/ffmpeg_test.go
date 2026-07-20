package recorder

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestSegmentArgs proves segmentArgs builds the exact stream-copy segmenting
// fMP4 command the plan requires: TCP-transport RTSP input, -c copy (no
// transcode), the segment muxer producing segmentSeconds-long fragmented MP4
// files, and an -strftime 1 output pattern (epoch-second filenames)
// segmentTimeRange later parses back out.
func TestSegmentArgs(t *testing.T) {
	ff := &FFmpeg{ffmpegPath: "ffmpeg"}

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
	ff := &FFmpeg{ffmpegPath: "ffmpeg"}

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
// CAMERAUI_FFMPEG_PATH (the core-injected path) over the bare-command-on-PATH
// fallback when set.
func TestResolveFFmpeg_HonorsEnvOverride(t *testing.T) {
	t.Setenv(envFFmpegPath, "/opt/custom/ffmpeg")

	ff := ResolveFFmpeg()

	if got := ff.Path(); got != "/opt/custom/ffmpeg" {
		t.Errorf("Path() = %q, want /opt/custom/ffmpeg", got)
	}
}

// TestResolveFFmpeg_FallsBackToPathWhenEnvUnset proves ResolveFFmpeg falls
// back to the bare command name (resolved against PATH by os/exec at call
// time) when the core hasn't set the env var.
func TestResolveFFmpeg_FallsBackToPathWhenEnvUnset(t *testing.T) {
	t.Setenv(envFFmpegPath, "")

	ff := ResolveFFmpeg()

	if got := ff.Path(); got != "ffmpeg" {
		t.Errorf("Path() = %q, want ffmpeg", got)
	}
}

// ---------------------------------------------------------------------------
// ResolveFFmpegSDK: the actual production bug fix. The core's master-mode
// runtime hands this plugin an explicit env allow-list with neither PATH nor
// CAMERAUI_FFMPEG_PATH set, so ResolveFFmpeg's bare "ffmpeg" fallback never
// resolves to anything there ("exec: ffmpeg: executable file not found in
// $PATH" — the exact live-server symptom this task fixes). The only correct
// resolution path in that runtime is the SDK's CoreManager.GetFFmpegPath RPC
// (github.com/cameraui/sdk/go@v1.1.11/manager_core.go), which returns the
// core's node-av-bundled ffmpeg binary. These tests inject a fake resolver
// (a real *sdk.CoreManager needs a live RPC client to construct) satisfying
// FFmpegPathResolver so both the happy path and every fallback trigger are
// covered without a live core connection.
// ---------------------------------------------------------------------------

// fakeFFmpegPathResolver is a FFmpegPathResolver test double: returns
// whatever path/err it's configured with, standing in for a real
// *sdk.CoreManager's GetFFmpegPath RPC.
type fakeFFmpegPathResolver struct {
	path string
	err  error
}

func (f fakeFFmpegPathResolver) GetFFmpegPath() (string, error) {
	return f.path, f.err
}

// TestResolveFFmpegSDK_UsesResolverPath proves ResolveFFmpegSDK uses the
// path returned by the injected resolver (standing in for the SDK's
// CoreManager.GetFFmpegPath) directly, without touching env/PATH resolution
// at all.
func TestResolveFFmpegSDK_UsesResolverPath(t *testing.T) {
	t.Setenv(envFFmpegPath, "/should/not/be/used")

	resolver := fakeFFmpegPathResolver{path: "/mnt/data/node-av/binary/ffmpeg"}
	ff := ResolveFFmpegSDK(resolver, nil)

	if got := ff.Path(); got != "/mnt/data/node-av/binary/ffmpeg" {
		t.Errorf("Path() = %q, want the resolver's path", got)
	}
}

// TestResolveFFmpegSDK_FallsBackOnResolverError proves ResolveFFmpegSDK falls
// back to the legacy env/PATH resolution (ResolveFFmpeg) when the resolver
// itself returns an error (e.g. the RPC round trip failed) rather than
// propagating the error or crashing.
func TestResolveFFmpegSDK_FallsBackOnResolverError(t *testing.T) {
	t.Setenv(envFFmpegPath, "/opt/custom/ffmpeg")

	resolver := fakeFFmpegPathResolver{err: errors.New("rpc: getFFmpegPath: connection lost")}
	ff := ResolveFFmpegSDK(resolver, nil)

	if got := ff.Path(); got != "/opt/custom/ffmpeg" {
		t.Errorf("Path() = %q, want the env-var fallback", got)
	}
}

// TestResolveFFmpegSDK_FallsBackOnEmptyPath proves ResolveFFmpegSDK falls
// back the same way when the resolver returns no error but an empty (or
// whitespace-only) path.
func TestResolveFFmpegSDK_FallsBackOnEmptyPath(t *testing.T) {
	t.Setenv(envFFmpegPath, "")

	resolver := fakeFFmpegPathResolver{path: "   "}
	ff := ResolveFFmpegSDK(resolver, nil)

	if got := ff.Path(); got != "ffmpeg" {
		t.Errorf("Path() = %q, want the PATH fallback", got)
	}
}
