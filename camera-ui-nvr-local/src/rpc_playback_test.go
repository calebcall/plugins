package main

import (
	"context"
	"errors"
	"testing"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/media"
)

// fakeScrubber is an in-memory stand-in for *media.Scrubber, letting
// NvrScrub/NvrPreviewFrames' own RPC-shape logic (field mapping, NoData
// pointer semantics, nil-scrubber handling) be unit-tested without a real
// ffmpeg process or SQLite-backed SegmentStore — media/scrub_test.go
// already covers the real-media extraction path itself (Task SCRUB's
// required TDD real-media proof).
type fakeScrubber struct {
	scrubResult media.ScrubResult
	scrubErr    error

	previewResult media.PreviewResult
	previewErr    error

	// gotCameraID/gotTsUs/gotSourceRole record the last Scrub call's
	// arguments, so tests can prove NvrScrub passes them through
	// unmodified.
	gotCameraID   string
	gotTsUs       int64
	gotSourceRole string
}

func (f *fakeScrubber) Scrub(ctx context.Context, cameraID string, tsUs int64, sourceRole string) (media.ScrubResult, error) {
	f.gotCameraID = cameraID
	f.gotTsUs = tsUs
	f.gotSourceRole = sourceRole
	return f.scrubResult, f.scrubErr
}

func (f *fakeScrubber) PreviewFrames(ctx context.Context, cameraID string, startUs, endUs int64, count int) (media.PreviewResult, error) {
	return f.previewResult, f.previewErr
}

// --- nvrScrub ---------------------------------------------------------------

// TestNvrScrub_NilScrubber_ReturnsNoData proves a plugin with no scrubber
// wired up (e.g. store.Open failed in NewPlugin) reports NoData rather
// than panicking or erroring — the same "best-effort, never a hard
// failure" contract every other nil-store guard in this package follows.
func TestNvrScrub_NilScrubber_ReturnsNoData(t *testing.T) {
	p := newTestPlugin(t)

	result, err := p.NvrScrub("cam1", 1_500_000, false, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NoData == nil || !*result.NoData {
		t.Fatalf("expected NoData=true, got %+v", result)
	}
	if result.Ts != 1_500_000 {
		t.Errorf("expected Ts to echo the request, got %d", result.Ts)
	}
	if result.VideoCodec != "h264" {
		t.Errorf("expected videoCodec h264, got %q", result.VideoCodec)
	}
	if len(result.Frame) != 0 {
		t.Errorf("expected no frame data, got %d bytes", len(result.Frame))
	}
}

// TestNvrScrub_Found_MapsFieldsAndPassesArgsThrough proves a found scrub
// result maps every media.ScrubResult field onto the wire's
// NvrScrubResult, with no NoData set, and that cameraID/tsUs/sourceRole
// reach the scrubber unchanged.
func TestNvrScrub_Found_MapsFieldsAndPassesArgsThrough(t *testing.T) {
	p := newTestPlugin(t)
	fake := &fakeScrubber{scrubResult: media.ScrubResult{
		Frame:       []byte{0x00, 0x00, 0x00, 0x01, 0x67},
		CodecString: "avc1.64001f",
		Width:       320,
		Height:      240,
		Found:       true,
	}}
	p.scrubber = fake

	result, err := p.NvrScrub("cam1", 2_500_000, true, "low")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NoData != nil {
		t.Errorf("expected NoData unset, got %v", *result.NoData)
	}
	if string(result.Frame) != "\x00\x00\x00\x01\x67" {
		t.Errorf("expected frame bytes to pass through unchanged, got %v", result.Frame)
	}
	if result.Ts != 2_500_000 {
		t.Errorf("expected Ts=2500000, got %d", result.Ts)
	}
	if result.CodecString != "avc1.64001f" || result.Width != 320 || result.Height != 240 {
		t.Errorf("expected codec metadata to pass through, got %+v", result)
	}
	if fake.gotCameraID != "cam1" || fake.gotTsUs != 2_500_000 || fake.gotSourceRole != "low" {
		t.Errorf("expected args to reach the scrubber unchanged, got cameraID=%q tsUs=%d sourceRole=%q",
			fake.gotCameraID, fake.gotTsUs, fake.gotSourceRole)
	}
}

// TestNvrScrub_NotFound_ReturnsNoData proves a scrubber reporting
// Found=false (its own "no covering segment" contract — media/scrub.go)
// surfaces as NoData=true, not an error.
func TestNvrScrub_NotFound_ReturnsNoData(t *testing.T) {
	p := newTestPlugin(t)
	p.scrubber = &fakeScrubber{scrubResult: media.ScrubResult{Found: false}}

	result, err := p.NvrScrub("cam1", 5_000_000, false, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NoData == nil || !*result.NoData {
		t.Fatalf("expected NoData=true, got %+v", result)
	}
}

// TestNvrScrub_ScrubberError_Propagates proves a genuine error from the
// scrubber (as opposed to its "not found" contract) surfaces as an RPC
// error rather than being swallowed into a NoData response.
func TestNvrScrub_ScrubberError_Propagates(t *testing.T) {
	p := newTestPlugin(t)
	wantErr := errors.New("boom")
	p.scrubber = &fakeScrubber{scrubErr: wantErr}

	_, err := p.NvrScrub("cam1", 1_000_000, false, "")
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected error to propagate, got %v", err)
	}
}

// --- nvrPreviewFrames --------------------------------------------------------

// TestNvrPreviewFrames_NilScrubber_ReturnsNoData mirrors
// TestNvrScrub_NilScrubber_ReturnsNoData for the preview-frames handler.
func TestNvrPreviewFrames_NilScrubber_ReturnsNoData(t *testing.T) {
	p := newTestPlugin(t)

	result, err := p.NvrPreviewFrames("cam1", 0, 4_000_000, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NoData == nil || !*result.NoData {
		t.Fatalf("expected NoData=true, got %+v", result)
	}
	if len(result.Frames) != 0 {
		t.Errorf("expected no frames, got %d", len(result.Frames))
	}
}

// TestNvrPreviewFrames_MapsFramesAndMetadata proves every media.Frame in a
// media.PreviewResult maps onto the wire's NvrScrubFrame (Frame/Ts/
// Keyframe), and shared codec metadata is passed through, with no NoData
// set when frames were found.
func TestNvrPreviewFrames_MapsFramesAndMetadata(t *testing.T) {
	p := newTestPlugin(t)
	p.scrubber = &fakeScrubber{previewResult: media.PreviewResult{
		Frames: []media.Frame{
			{Data: []byte{0x01}, TsUs: 1000, Keyframe: true},
			{Data: []byte{0x02}, TsUs: 2000, Keyframe: true},
		},
		CodecString: "avc1.64001f",
		Width:       320,
		Height:      240,
		NoData:      false,
	}}

	result, err := p.NvrPreviewFrames("cam1", 0, 4_000_000, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NoData != nil {
		t.Errorf("expected NoData unset, got %v", *result.NoData)
	}
	if len(result.Frames) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(result.Frames))
	}
	if result.Frames[0].Ts != 1000 || !result.Frames[0].Keyframe || string(result.Frames[0].Frame) != "\x01" {
		t.Errorf("unexpected frame 0: %+v", result.Frames[0])
	}
	if result.Frames[1].Ts != 2000 || string(result.Frames[1].Frame) != "\x02" {
		t.Errorf("unexpected frame 1: %+v", result.Frames[1])
	}
	if result.CodecString != "avc1.64001f" || result.Width != 320 || result.Height != 240 {
		t.Errorf("expected codec metadata to pass through, got %+v", result)
	}
}

// TestNvrPreviewFrames_NoData_ReturnsNoDataPointer proves a
// media.PreviewResult{NoData: true} (no frames resolved) maps onto the
// wire's *bool NoData pointer, per NvrScrubResult/NvrPreviewResult's
// "absent means false" wire contract.
func TestNvrPreviewFrames_NoData_ReturnsNoDataPointer(t *testing.T) {
	p := newTestPlugin(t)
	p.scrubber = &fakeScrubber{previewResult: media.PreviewResult{NoData: true}}

	result, err := p.NvrPreviewFrames("cam1", 0, 4_000_000, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NoData == nil || !*result.NoData {
		t.Fatalf("expected NoData=true, got %+v", result)
	}
}

// TestNvrPreviewFrames_ScrubberError_Propagates mirrors
// TestNvrScrub_ScrubberError_Propagates for the preview-frames handler.
func TestNvrPreviewFrames_ScrubberError_Propagates(t *testing.T) {
	p := newTestPlugin(t)
	wantErr := errors.New("boom")
	p.scrubber = &fakeScrubber{previewErr: wantErr}

	_, err := p.NvrPreviewFrames("cam1", 0, 4_000_000, 4)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected error to propagate, got %v", err)
	}
}
