package media

import (
	"context"
	"regexp"
	"testing"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// scrubFixtureRole is the store role genFixtureSegment (thumbs_test.go)
// stamps every fixture segment with — every test below records it under
// "high-resolution" too so Scrubber's default-role resolution (empty
// sourceRole -> "high-resolution") finds it without any extra wiring.
const scrubFixtureRole = "high-resolution"

// newTestSegmentStore returns a real *store.SegmentStore backed by a
// throwaway SQLite database, closed at test cleanup — the same pattern
// thumbs_test.go's newTestStores uses, minus the EventStore half this
// package's scrub tests don't need.
func newTestSegmentStore(t *testing.T) *store.SegmentStore {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return store.NewSegmentStore(db)
}

// codecStringRe matches the wire contract's required codecString shape:
// "avc1." followed by exactly 6 lowercase hex digits (profile_idc,
// constraint_flags, level_idc, each a 2-digit hex byte).
var codecStringRe = regexp.MustCompile(`^avc1\.[0-9a-f]{6}$`)

// annexBStartCode reports whether data begins with an Annex-B start code
// (3-byte 00 00 01 or 4-byte 00 00 00 01) — the frame format the wire
// contract requires (see task brief: "frame bytes MUST be Annex-B").
func hasAnnexBStartCode(data []byte) bool {
	if len(data) >= 4 && data[0] == 0 && data[1] == 0 && data[2] == 0 && data[3] == 1 {
		return true
	}
	return len(data) >= 3 && data[0] == 0 && data[1] == 0 && data[2] == 1
}

// containsNALType reports whether any NAL unit in Annex-B data has the
// given nal_unit_type (7 = SPS, 5 = IDR slice — the two this task's tests
// need to find in an extracted scrub keyframe).
func containsNALType(data []byte, nalType byte) bool {
	for _, nal := range nalUnits(data) {
		if len(nal) > 0 && nal[0]&0x1F == nalType {
			return true
		}
	}
	return false
}

// TestScrub_ExtractsRealAnnexBKeyframe is the task's required real-media
// proof: a genuine fragmented-MP4 segment is generated and indexed, and
// Scrub at a timestamp inside it returns a non-empty Annex-B keyframe
// (start code present, SPS type-7 and IDR type-5 NALs both present), a
// codecString matching ^avc1\.[0-9a-f]{6}$, and the fixture's known
// 320x240 resolution.
func TestScrub_ExtractsRealAnnexBKeyframe(t *testing.T) {
	requireFFmpeg(t)

	segDir := t.TempDir()
	seg := genFixtureSegment(t, segDir, 10_000, 3)
	seg.Role = scrubFixtureRole

	segments := newTestSegmentStore(t)
	if _, err := segments.Add(seg); err != nil {
		t.Fatalf("segments.Add: %v", err)
	}

	scrubber := NewScrubber(resolvedFFmpegPath(), segments, nil)

	// 11_500ms in microseconds: 1.5s into the 3s fixture segment (StartMs
	// 10_000).
	result, err := scrubber.Scrub(context.Background(), "cam1", 11_500_000, "")
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if !result.Found {
		t.Fatalf("expected Found=true for a timestamp inside the fixture segment")
	}
	if len(result.Frame) == 0 {
		t.Fatalf("expected a non-empty extracted frame")
	}
	if !hasAnnexBStartCode(result.Frame) {
		t.Fatalf("expected frame to start with an Annex-B start code, got %v", result.Frame[:minInt(8, len(result.Frame))])
	}
	if !containsNALType(result.Frame, 7) {
		t.Errorf("expected frame to contain an SPS NAL (type 7)")
	}
	if !containsNALType(result.Frame, 5) {
		t.Errorf("expected frame to contain an IDR NAL (type 5)")
	}
	if !codecStringRe.MatchString(result.CodecString) {
		t.Errorf("expected codecString to match %s, got %q", codecStringRe.String(), result.CodecString)
	}
	if result.Width != 320 || result.Height != 240 {
		t.Errorf("expected 320x240, got %dx%d", result.Width, result.Height)
	}
}

// TestScrub_NoCoveringSegment_ReturnsNoDataWithoutError proves a scrub
// timestamp not covered by any recorded segment (e.g. before recording
// started) resolves to Found=false and no error, mirroring
// media.Generator's own "skip gracefully" contract for exactly the same
// underlying condition.
func TestScrub_NoCoveringSegment_ReturnsNoDataWithoutError(t *testing.T) {
	segments := newTestSegmentStore(t)
	scrubber := NewScrubber("ffmpeg", segments, nil)

	result, err := scrubber.Scrub(context.Background(), "cam1", 5_000_000, "")
	if err != nil {
		t.Fatalf("Scrub: expected nil error for no covering segment, got %v", err)
	}
	if result.Found {
		t.Fatalf("expected Found=false, got %+v", result)
	}
	if len(result.Frame) != 0 {
		t.Fatalf("expected an empty frame, got %d bytes", len(result.Frame))
	}
}

// TestPreviewFrames_ReturnsRequestedCountAcrossRange proves PreviewFrames
// samples exactly `count` evenly-spaced keyframes across [startUs, endUs]
// out of a real fixture segment, each one independently a valid Annex-B
// keyframe.
func TestPreviewFrames_ReturnsRequestedCountAcrossRange(t *testing.T) {
	requireFFmpeg(t)

	segDir := t.TempDir()
	seg := genFixtureSegment(t, segDir, 0, 4)
	seg.Role = scrubFixtureRole

	segments := newTestSegmentStore(t)
	if _, err := segments.Add(seg); err != nil {
		t.Fatalf("segments.Add: %v", err)
	}

	scrubber := NewScrubber(resolvedFFmpegPath(), segments, nil)

	result, err := scrubber.PreviewFrames(context.Background(), "cam1", 0, 4_000_000, 4)
	if err != nil {
		t.Fatalf("PreviewFrames: %v", err)
	}
	if result.NoData {
		t.Fatalf("expected NoData=false, got frames=%d", len(result.Frames))
	}
	if len(result.Frames) != 4 {
		t.Fatalf("expected 4 frames, got %d", len(result.Frames))
	}
	for i, f := range result.Frames {
		if len(f.Data) == 0 {
			t.Errorf("frame %d: expected non-empty data", i)
		}
		if !hasAnnexBStartCode(f.Data) {
			t.Errorf("frame %d: expected an Annex-B start code", i)
		}
		if !f.Keyframe {
			t.Errorf("frame %d: expected Keyframe=true", i)
		}
	}
	if !codecStringRe.MatchString(result.CodecString) {
		t.Errorf("expected codecString to match %s, got %q", codecStringRe.String(), result.CodecString)
	}
}

// TestPreviewFrames_NoCoveringSegment_ReturnsNoData proves a preview
// window with no recorded segments at all resolves to NoData=true and no
// frames, without error.
func TestPreviewFrames_NoCoveringSegment_ReturnsNoData(t *testing.T) {
	segments := newTestSegmentStore(t)
	scrubber := NewScrubber("ffmpeg", segments, nil)

	result, err := scrubber.PreviewFrames(context.Background(), "cam1", 0, 4_000_000, 4)
	if err != nil {
		t.Fatalf("PreviewFrames: %v", err)
	}
	if !result.NoData {
		t.Fatalf("expected NoData=true, got %+v", result)
	}
	if len(result.Frames) != 0 {
		t.Fatalf("expected no frames, got %d", len(result.Frames))
	}
}

// TestSPSCodecString_ParsesKnownProfileConstraintLevel is the required
// unit test for SPS -> codecString parsing, run against a fabricated
// minimal Annex-B buffer (start code + a synthetic SPS NAL header with
// known profile_idc/constraint_flags/level_idc bytes) rather than a real
// encoder's SPS — proving the byte-offset parsing itself, independent of
// ffmpeg.
func TestSPSCodecString_ParsesKnownProfileConstraintLevel(t *testing.T) {
	// NAL header 0x67 = forbidden_zero_bit(0) + nal_ref_idc(3) + type(7,
	// SPS). Followed by profile_idc=0x64 (High), constraint_flags=0x00,
	// level_idc=0x1f (level 3.1) - deliberately not a valid/complete SPS
	// bitstream past this point, since annexBCodecString only ever reads
	// these first 3 payload bytes.
	frame := []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x64, 0x00, 0x1f, 0xAA, 0xBB, 0xCC}

	got, err := annexBCodecString(frame)
	if err != nil {
		t.Fatalf("annexBCodecString: %v", err)
	}
	if got != "avc1.64001f" {
		t.Errorf("expected avc1.64001f, got %q", got)
	}
}

// TestSPSCodecString_NoSPS_ReturnsError proves annexBCodecString errors
// (rather than returning a zero-value string that would silently violate
// the wire contract's codecString shape) when no SPS NAL is present at
// all.
func TestSPSCodecString_NoSPS_ReturnsError(t *testing.T) {
	// A single non-SPS NAL (type 1, a plain P/B slice).
	frame := []byte{0x00, 0x00, 0x01, 0x21, 0xAA, 0xBB, 0xCC}

	if _, err := annexBCodecString(frame); err == nil {
		t.Fatalf("expected an error when no SPS NAL is present")
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
