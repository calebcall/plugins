package media

import (
	"bytes"
	"context"
	"testing"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// playbackFixtureRole is the store role genFixtureSegment (thumbs_test.go)
// stamps every fixture segment with, matching scrubFixtureRole
// (scrub_test.go) — every Player test below records fixtures under
// "high-resolution" so resolveRole's default ("" -> "high-resolution")
// finds them without extra wiring.
const playbackFixtureRole = "high-resolution"

// annexBNAL builds one start-code-delimited NAL unit for the AU-splitter
// tests below: a 4-byte Annex-B start code, a single NAL header byte
// encoding nalType in its low 5 bits (forbidden_zero_bit=0, nal_ref_idc=0
// — neither matters to splitAccessUnits, which only ever reads the type),
// and whatever payload bytes the caller supplies (also irrelevant to
// splitAccessUnits — it never looks past the header byte).
func annexBNAL(nalType byte, payload ...byte) []byte {
	buf := []byte{0, 0, 0, 1, nalType & 0x1F}
	return append(buf, payload...)
}

// TestSplitAccessUnits_GroupsParameterSetsWithFollowingVCL proves the
// task's required AU-splitting behavior end to end: given a stream of
// SPS(7) + PPS(8) + IDR(5) + non-IDR(1) NALs, splitAccessUnits yields
// exactly two access units — the first carrying the accumulated SPS/PPS
// ahead of the IDR slice (Keyframe=true), the second just the bare
// non-IDR slice (Keyframe=false, no parameter sets — they were already
// consumed and cleared by the first AU).
func TestSplitAccessUnits_GroupsParameterSetsWithFollowingVCL(t *testing.T) {
	sps := annexBNAL(7, 0x64, 0x00, 0x1f)
	pps := annexBNAL(8, 0xAA)
	idr := annexBNAL(5, 0xBB, 0xCC)
	nonIDR := annexBNAL(1, 0xDD)

	data := bytes.Join([][]byte{sps, pps, idr, nonIDR}, nil)

	units := splitAccessUnits(data)
	if len(units) != 2 {
		t.Fatalf("expected 2 access units, got %d: %+v", len(units), units)
	}

	au0 := units[0]
	if !au0.Keyframe {
		t.Errorf("expected the first AU (SPS+PPS+IDR) to be a keyframe")
	}
	if !containsNALType(au0.Data, 7) || !containsNALType(au0.Data, 8) || !containsNALType(au0.Data, 5) {
		t.Errorf("expected the first AU to contain SPS, PPS, and the IDR slice, got %v", au0.Data)
	}

	au1 := units[1]
	if au1.Keyframe {
		t.Errorf("expected the second AU (bare non-IDR slice) to not be a keyframe")
	}
	if containsNALType(au1.Data, 7) || containsNALType(au1.Data, 8) {
		t.Errorf("expected the second AU to carry no parameter sets (already consumed by AU 0), got %v", au1.Data)
	}
	if !containsNALType(au1.Data, 1) {
		t.Errorf("expected the second AU to contain the non-IDR slice, got %v", au1.Data)
	}

	if !hasAnnexBStartCode(au0.Data) || !hasAnnexBStartCode(au1.Data) {
		t.Errorf("expected every AU to start with an Annex-B start code")
	}
}

// TestSplitAccessUnits_DropsNonParameterSetNonVCLNals proves an
// interleaved NAL type this task's simplified AU heuristic doesn't
// recognize (SEI, type 6) is silently dropped — neither starting its own
// AU nor leaking into the next one's pending parameter-set buffer.
func TestSplitAccessUnits_DropsNonParameterSetNonVCLNals(t *testing.T) {
	sei := annexBNAL(6, 0x01)
	idr := annexBNAL(5, 0xAA)

	data := bytes.Join([][]byte{sei, idr}, nil)

	units := splitAccessUnits(data)
	if len(units) != 1 {
		t.Fatalf("expected exactly 1 access unit (the SEI dropped), got %d", len(units))
	}
	if containsNALType(units[0].Data, 6) {
		t.Errorf("expected the SEI NAL to be dropped, got %v", units[0].Data)
	}
	if !units[0].Keyframe {
		t.Errorf("expected the sole AU (IDR) to be a keyframe")
	}
}

// TestSplitAccessUnits_TrailingParameterSetsWithNoVCL_AreDropped proves a
// truncated capture (parameter sets with no following VCL NAL before EOF)
// never emits a dangling/invalid access unit.
func TestSplitAccessUnits_TrailingParameterSetsWithNoVCL_AreDropped(t *testing.T) {
	idr := annexBNAL(5, 0xAA)
	trailingSPS := annexBNAL(7, 0x64, 0x00, 0x1f)

	data := bytes.Join([][]byte{idr, trailingSPS}, nil)

	units := splitAccessUnits(data)
	if len(units) != 1 {
		t.Fatalf("expected exactly 1 access unit (the trailing SPS dropped), got %d", len(units))
	}
}

// TestSegmentFramesTimestamps_SpacedByFPS_Monotonic proves the ts/fps
// assignment formula the task brief specifies: ts[i] = BaseUs +
// i*(1e6/FPS), strictly increasing.
func TestSegmentFramesTimestamps_SpacedByFPS_Monotonic(t *testing.T) {
	frames := SegmentFrames{
		BaseUs: 1_000_000,
		FPS:    10,
		Units:  make([]AccessUnit, 5),
	}

	ts := frames.Timestamps()
	if len(ts) != 5 {
		t.Fatalf("expected 5 timestamps, got %d", len(ts))
	}

	wantInterval := int64(100_000) // 1e6/10
	for i, got := range ts {
		want := frames.BaseUs + int64(i)*wantInterval
		if got != want {
			t.Errorf("ts[%d]: expected %d, got %d", i, want, got)
		}
	}
	for i := 1; i < len(ts); i++ {
		if ts[i] <= ts[i-1] {
			t.Fatalf("expected strictly increasing timestamps, got %v", ts)
		}
	}
}

// TestSegmentFramesTimestamps_FallsBackWhenFPSMissing proves a
// non-positive FPS (a probe miss) falls back to defaultPlaybackFPS rather
// than dividing by zero or producing degenerate spacing.
func TestSegmentFramesTimestamps_FallsBackWhenFPSMissing(t *testing.T) {
	frames := SegmentFrames{BaseUs: 0, FPS: 0, Units: make([]AccessUnit, 2)}

	ts := frames.Timestamps()
	fallbackFPS := defaultPlaybackFPS
	wantInterval := int64(1_000_000 / fallbackFPS)
	if ts[1]-ts[0] != wantInterval {
		t.Errorf("expected fallback interval %d, got %d", wantInterval, ts[1]-ts[0])
	}
}

// --- Player: real-media tests ------------------------------------------

// TestPlayer_FirstSegment_ExtractsAccessUnitsFromOffset is this task's
// required real-media proof for the extraction side: a genuine
// fragmented-MP4 fixture is generated and indexed, and FirstSegment at a
// timestamp inside it returns real, valid Annex-B access units (start
// codes present) with a parsed codecString/resolution/fps matching the
// fixture's known encoding.
func TestPlayer_FirstSegment_ExtractsAccessUnitsFromOffset(t *testing.T) {
	requireFFmpeg(t)

	segDir := t.TempDir()
	seg := genFixtureSegment(t, segDir, 10_000, 3) // 3s @ 10fps, 320x240
	seg.Role = playbackFixtureRole

	segments := newTestSegmentStore(t)
	if _, err := segments.Add(seg); err != nil {
		t.Fatalf("segments.Add: %v", err)
	}

	player := NewPlayer(resolvedFFmpegPath(), segments, nil)

	// 11_000ms in microseconds: 1s into the 3s fixture segment (StartMs
	// 10_000), leaving ~2s (roughly 20 frames at 10fps) to extract.
	frames, ok, err := player.FirstSegment(context.Background(), "cam1", 11_000_000, "")
	if err != nil {
		t.Fatalf("FirstSegment: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true for a timestamp inside the fixture segment")
	}
	if len(frames.Units) == 0 {
		t.Fatalf("expected at least one extracted access unit")
	}
	for i, au := range frames.Units {
		if len(au.Data) == 0 {
			t.Fatalf("unit %d: expected non-empty data", i)
		}
		if !hasAnnexBStartCode(au.Data) {
			t.Fatalf("unit %d: expected an Annex-B start code, got %v", i, au.Data[:minInt(8, len(au.Data))])
		}
	}
	if !units0KeyframeConsistent(frames.Units) {
		t.Errorf("expected the first extracted unit to be a keyframe (extraction starts at a keyframe boundary — ffmpeg's own -ss seek)")
	}
	if !codecStringRe.MatchString(frames.CodecString) {
		t.Errorf("expected codecString to match %s, got %q", codecStringRe.String(), frames.CodecString)
	}
	if frames.Width != 320 || frames.Height != 240 {
		t.Errorf("expected 320x240, got %dx%d", frames.Width, frames.Height)
	}
	if frames.FPS < 9 || frames.FPS > 11 {
		t.Errorf("expected ~10fps, got %v", frames.FPS)
	}

	ts := frames.Timestamps()
	for i := 1; i < len(ts); i++ {
		if ts[i] <= ts[i-1] {
			t.Fatalf("expected strictly increasing timestamps, got %v", ts)
		}
	}
}

// units0KeyframeConsistent reports whether the first extracted access
// unit is a keyframe — ffmpeg's own -ss seeks to the nearest keyframe
// at/before the requested offset, so the very first unit out of any
// extraction is always expected to be one.
func units0KeyframeConsistent(units []AccessUnit) bool {
	return len(units) > 0 && units[0].Keyframe
}

// TestPlayer_FirstSegment_NoCoveringSegment_ReturnsNotFound proves a
// timestamp not covered by any recorded segment resolves to ok=false with
// no error, mirroring Scrubber.Scrub's own contract for the same
// underlying condition.
func TestPlayer_FirstSegment_NoCoveringSegment_ReturnsNotFound(t *testing.T) {
	segments := newTestSegmentStore(t)
	player := NewPlayer("ffmpeg", segments, nil)

	_, ok, err := player.FirstSegment(context.Background(), "cam1", 5_000_000, "")
	if err != nil {
		t.Fatalf("FirstSegment: expected nil error for no covering segment, got %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false")
	}
}

// TestPlayer_NextSegment_RollsIntoFollowingSegment proves NextSegment
// finds and extracts (from its own start) whatever segment immediately
// follows a given one, across two real fixture segments recorded
// back-to-back.
func TestPlayer_NextSegment_RollsIntoFollowingSegment(t *testing.T) {
	requireFFmpeg(t)

	segDir := t.TempDir()
	seg1 := genFixtureSegment(t, segDir, 0, 2)
	seg1.Role = playbackFixtureRole

	// genFixtureSegment always writes to the same "segment.mp4" name
	// within dir, so segment 2 needs its own directory.
	seg2Dir := t.TempDir()
	seg2 := genFixtureSegment(t, seg2Dir, 2_000, 2)
	seg2.Role = playbackFixtureRole

	segments := newTestSegmentStore(t)
	id1, err := segments.Add(seg1)
	if err != nil {
		t.Fatalf("segments.Add seg1: %v", err)
	}
	seg1.ID = id1
	if _, err := segments.Add(seg2); err != nil {
		t.Fatalf("segments.Add seg2: %v", err)
	}

	player := NewPlayer(resolvedFFmpegPath(), segments, nil)

	next, ok, err := player.NextSegment(context.Background(), "cam1", seg1)
	if err != nil {
		t.Fatalf("NextSegment: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true: seg2 starts exactly where seg1 ends")
	}
	if next.Segment.StartMs != seg2.StartMs {
		t.Errorf("expected to roll into seg2 (StartMs=%d), got StartMs=%d", seg2.StartMs, next.Segment.StartMs)
	}
	if len(next.Units) == 0 {
		t.Fatalf("expected next segment's extraction to yield access units")
	}
	if next.BaseUs != seg2.StartMs*1000 {
		t.Errorf("expected BaseUs to be seg2's own start (offset 0), got %d want %d", next.BaseUs, seg2.StartMs*1000)
	}
}

// TestPlayer_NextSegment_NoFurtherSegment_ReturnsNotFound proves a
// segment with nothing recorded after it (the live edge of what's been
// recorded, or a real gap) resolves to ok=false with no error.
func TestPlayer_NextSegment_NoFurtherSegment_ReturnsNotFound(t *testing.T) {
	segments := newTestSegmentStore(t)
	player := NewPlayer("ffmpeg", segments, nil)

	prev := store.Segment{ID: 1, CameraID: "cam1", Role: playbackFixtureRole, StartMs: 0, EndMs: 2000}
	_, ok, err := player.NextSegment(context.Background(), "cam1", prev)
	if err != nil {
		t.Fatalf("NextSegment: expected nil error, got %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false when nothing follows prev")
	}
}
