package store

import (
	"testing"

	sdk "github.com/cameraui/sdk/go"
)

// TestPrimaryLabel_PrefersHighestScoreSegmentDetectionOverAlphabeticalTypes
// is the FIX B regression test: ev.Types is alphabetically sorted (not
// lifecycle-ordered), so a person-detection event carrying
// ["clip","motion","person"] previously returned Types[0] == "clip"
// (primaryLabel's old behavior) instead of the actual detected object.
// PrimaryLabel must instead surface "person", driven by the segment
// detection's Label, since that's the strongest signal for what the event
// actually is.
func TestPrimaryLabel_PrefersHighestScoreSegmentDetectionOverAlphabeticalTypes(t *testing.T) {
	ev := DetectionEvent{
		Types: []string{"clip", "motion", "person"},
		Segments: []sdk.EventSegment{
			{
				Detections: []sdk.EventDetection{
					{Label: "person", Score: 0.8},
				},
			},
		},
	}
	if got := PrimaryLabel(ev); got != "person" {
		t.Errorf("PrimaryLabel = %q, want %q", got, "person")
	}
}

// TestPrimaryLabel_HighestScoreDetectionWinsAcrossMultipleSegments proves
// the ranking picks the highest-Score detection across every segment, not
// merely the first one seen.
func TestPrimaryLabel_HighestScoreDetectionWinsAcrossMultipleSegments(t *testing.T) {
	ev := DetectionEvent{
		Types: []string{"motion", "vehicle", "person"},
		Segments: []sdk.EventSegment{
			{Detections: []sdk.EventDetection{{Label: "vehicle", Score: 0.4}}},
			{Detections: []sdk.EventDetection{{Label: "person", Score: 0.91}}},
		},
	}
	if got := PrimaryLabel(ev); got != "person" {
		t.Errorf("PrimaryLabel = %q, want %q (the higher-scoring detection)", got, "person")
	}
}

// TestPrimaryLabel_MotionOnlyFallsBackToTypesZero proves a motion-only
// event (no segment detections, no non-motion/audio/clip type, no trigger
// label) still reports "motion" — the same fallback primaryLabel's previous
// (buggy) implementation always used, preserved as the last resort.
func TestPrimaryLabel_MotionOnlyFallsBackToTypesZero(t *testing.T) {
	ev := DetectionEvent{Types: []string{"motion"}}
	if got := PrimaryLabel(ev); got != "motion" {
		t.Errorf("PrimaryLabel = %q, want %q", got, "motion")
	}
}

// TestPrimaryLabel_AudioOnlyPrefersTriggerLabelOverType proves an audio-only
// event (no segment detections, and the only Types entry is "audio" itself
// — excluded from the object-type-name step) falls through to the trigger's
// own Label ("doorbell") rather than the coarse "audio" type name, since the
// trigger label is the more specific, human-meaningful signal here.
func TestPrimaryLabel_AudioOnlyPrefersTriggerLabelOverType(t *testing.T) {
	ev := DetectionEvent{
		Types: []string{"audio"},
		Triggers: []sdk.EventTrigger{
			{Type: sdk.EventTriggerAudio, Label: "doorbell", Score: 0.7},
		},
	}
	if got := PrimaryLabel(ev); got != "doorbell" {
		t.Errorf("PrimaryLabel = %q, want %q", got, "doorbell")
	}
}

// TestPrimaryLabel_NoSignalAtAllReturnsEmpty proves an event with no Types,
// no Triggers, and no Segments returns "" rather than panicking.
func TestPrimaryLabel_NoSignalAtAllReturnsEmpty(t *testing.T) {
	if got := PrimaryLabel(DetectionEvent{}); got != "" {
		t.Errorf("PrimaryLabel = %q, want empty string", got)
	}
}
