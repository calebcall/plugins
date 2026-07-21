package main

import (
	"fmt"
	"sync"
	"testing"

	sdk "github.com/cameraui/sdk/go"
)

// TestDetectionEventIngester_Handle_AccumulatesDetectionsAcrossLifecycle
// reproduces the exact observed live-log sequence for a single person
// event: start (Segments:[]), segment-start (person=0.72), update
// (Segments:[]), segment-end (person=0.72), end (Segments:[]). Before the
// accumulator, the final upsert (the terminal 'end' message) stored
// Segments:[] — bestConfidence/primaryLabel (store/events.go) then saw no
// detections at all, so the row persisted with confidence=0,
// primaryLabel="motion", failing the frontend's default
// minConfidence:0.5 filter. This proves the FINAL upserted row instead
// retains the person=0.72 detection.
func TestDetectionEventIngester_Handle_AccumulatesDetectionsAcrossLifecycle(t *testing.T) {
	fake := &fakeEventStore{}
	ingester := newDetectionEventIngester(fake, nil, nil, nil, nil, nil, nil)

	base := sdk.DetectionEvent{ID: "evt-1", CameraID: "cam1", StartTime: 1000, LastUpdate: 1000}

	start := base
	start.State = sdk.DetectionEventStateActive
	ingester.handle(sdk.DetectionEventStart, start)

	segStart := base
	segStart.State = sdk.DetectionEventStateActive
	segStart.LastUpdate = 1500
	segStart.Types = []string{"person"}
	segStart.Segments = []sdk.EventSegment{{
		FirstSeen:  1000,
		LastSeen:   1500,
		Detections: []sdk.EventDetection{{Label: "person", Score: 0.72}},
	}}
	ingester.handle(sdk.DetectionEventSegmentStart, segStart)

	update := base
	update.State = sdk.DetectionEventStateActive
	update.LastUpdate = 2000
	ingester.handle(sdk.DetectionEventUpdate, update)

	segEnd := base
	segEnd.State = sdk.DetectionEventStateActive
	segEnd.LastUpdate = 2500
	segEnd.Types = []string{"person"}
	segEnd.Segments = []sdk.EventSegment{{
		FirstSeen:  1000,
		LastSeen:   2500,
		Detections: []sdk.EventDetection{{Label: "person", Score: 0.72}},
	}}
	ingester.handle(sdk.DetectionEventSegmentEnd, segEnd)

	end := base
	end.State = sdk.DetectionEventStateEnded
	end.EndTime = 3000
	end.LastUpdate = 3000
	ingester.handle(sdk.DetectionEventEnd, end)

	if len(fake.upserted) != 5 {
		t.Fatalf("expected 5 upserts (one per lifecycle message), got %d", len(fake.upserted))
	}

	final := fake.upserted[len(fake.upserted)-1]
	if len(final.Segments) != 1 {
		t.Fatalf("expected the final upserted event to retain exactly 1 synthesized segment, got %+v", final.Segments)
	}
	dets := final.Segments[0].Detections
	if len(dets) != 1 || dets[0].Label != "person" || dets[0].Score != 0.72 {
		t.Fatalf("expected the final upserted event to retain person=0.72, got %+v", dets)
	}
	if final.State != sdk.DetectionEventStateEnded || final.EndTime != 3000 {
		t.Fatalf("expected the final upserted event's own State/EndTime to be the terminal message's, got state=%s endTime=%d", final.State, final.EndTime)
	}
}

// TestDetectionAccumulator_Merge_MaxScoreWinsAcrossSegmentMessages proves
// two segment-* messages for the same label keep the higher score (0.86)
// rather than the later message's score unconditionally overwriting the
// earlier, higher one — the "union keyed by detection Label, keeping the
// MAX Score" rule the ingestion fix's design calls for.
func TestDetectionAccumulator_Merge_MaxScoreWinsAcrossSegmentMessages(t *testing.T) {
	acc := &detectionAccumulator{}

	first := sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000,
		Segments: []sdk.EventSegment{{Detections: []sdk.EventDetection{{Label: "person", Score: 0.72}}}},
	}
	acc.merge(first)

	second := sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000,
		Segments: []sdk.EventSegment{{Detections: []sdk.EventDetection{{Label: "person", Score: 0.86}}}},
	}
	merged := acc.merge(second)

	if len(merged.Segments) != 1 || len(merged.Segments[0].Detections) != 1 {
		t.Fatalf("expected exactly 1 synthesized segment with 1 detection, got %+v", merged.Segments)
	}
	if got := merged.Segments[0].Detections[0].Score; got != 0.86 {
		t.Fatalf("expected max-score-wins to keep 0.86, got %v", got)
	}

	// A third message with a LOWER score than what's already accumulated
	// must not regress the stored max.
	third := sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000,
		State: sdk.DetectionEventStateEnded, EndTime: 5000,
		Segments: []sdk.EventSegment{{Detections: []sdk.EventDetection{{Label: "person", Score: 0.10}}}},
	}
	merged = acc.merge(third)
	if got := merged.Segments[0].Detections[0].Score; got != 0.86 {
		t.Fatalf("expected max-score-wins to still be 0.86 after a lower-scored message, got %v", got)
	}
}

// TestDetectionAccumulator_Merge_UnionsAttributesByTypeAndLabel proves
// EventAttributes (faces/plates/classifiers) accumulate the same way
// EventDetections do: keyed by (Type, Label), keeping the highest
// Confidence seen.
func TestDetectionAccumulator_Merge_UnionsAttributesByTypeAndLabel(t *testing.T) {
	acc := &detectionAccumulator{}

	acc.merge(sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000,
		Segments: []sdk.EventSegment{{Attributes: []sdk.EventAttribute{{Type: "face", Label: "alice", Confidence: 0.5}}}},
	})
	merged := acc.merge(sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000,
		Segments: []sdk.EventSegment{{Attributes: []sdk.EventAttribute{{Type: "face", Label: "alice", Confidence: 0.9}}}},
	})

	if len(merged.Segments) != 1 || len(merged.Segments[0].Attributes) != 1 {
		t.Fatalf("expected exactly 1 synthesized segment with 1 attribute, got %+v", merged.Segments)
	}
	if got := merged.Segments[0].Attributes[0].Confidence; got != 0.9 {
		t.Fatalf("expected max-confidence-wins to keep 0.9, got %v", got)
	}
}

// TestDetectionEventIngester_Handle_PreservesSegmentThumbnailAcrossLifecycle
// is a regression test for a review finding: synthesizeSegments originally
// rebuilt the accumulated segment from only Detections/Attributes, dropping
// EventSegment.Thumbnail (the per-segment "scene" JPEG) entirely.
// rpc_events.go's thumbnailsFromEvent reads exactly that field (via
// ev.Segments[N].Thumbnail) to populate EventThumbnails.Scenes, so every
// event that went through the accumulator ended up with an always-empty
// Scenes map — a regression versus pre-accumulator behavior, where a
// client polling mid-lifecycle could still see a scene thumbnail from
// whichever raw message last carried one.
//
// Reproduces the same shape of bug Bug 1 itself fixed, one field over: a
// segment-start message carries a non-empty EventSegment.Thumbnail, but the
// terminal 'end' message (like every terminal/plain-update message) arrives
// with Segments:[] — an empty later message must not erase the segment
// thumbnail an earlier one already reported. Asserts the FINAL upserted
// event's synthesized segment still carries it.
func TestDetectionEventIngester_Handle_PreservesSegmentThumbnailAcrossLifecycle(t *testing.T) {
	fake := &fakeEventStore{}
	ingester := newDetectionEventIngester(fake, nil, nil, nil, nil, nil, nil)

	scene := []byte{0xFF, 0xD8, 0xFF, 0xD9} // stand-in JPEG bytes

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateActive, StartTime: 1000,
	})
	ingester.handle(sdk.DetectionEventSegmentStart, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateActive, StartTime: 1000, LastUpdate: 1500,
		Types: []string{"person"},
		Segments: []sdk.EventSegment{{
			FirstSeen:  1000,
			LastSeen:   1500,
			Thumbnail:  scene,
			Detections: []sdk.EventDetection{{Label: "person", Score: 0.72}},
		}},
	})
	// Terminal 'end' message: sparse, Segments:[], exactly like the
	// observed live-log sequence — must not erase the scene thumbnail
	// segment-start already reported.
	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded, StartTime: 1000, EndTime: 3000, LastUpdate: 3000,
	})

	if len(fake.upserted) != 3 {
		t.Fatalf("expected 3 upserts (one per lifecycle message), got %d", len(fake.upserted))
	}

	final := fake.upserted[len(fake.upserted)-1]
	if len(final.Segments) != 1 {
		t.Fatalf("expected the final upserted event to retain exactly 1 synthesized segment, got %+v", final.Segments)
	}
	if got := final.Segments[0].Thumbnail; len(got) == 0 {
		t.Fatalf("expected the final upserted event's synthesized segment to retain the scene thumbnail from segment-start, got empty (Scenes would be empty in thumbnailsFromEvent)")
	} else if string(got) != string(scene) {
		t.Fatalf("expected the retained scene thumbnail to match segment-start's, got %v want %v", got, scene)
	}
}

// TestDetectionAccumulator_Merge_PreservesZonesAndDescription proves Zones
// (unioned, deduplicated) and Description (latest non-nil) survive the same
// no-clobber treatment as the scene thumbnail above, across a later message
// whose Segments is empty.
func TestDetectionAccumulator_Merge_PreservesZonesAndDescription(t *testing.T) {
	acc := &detectionAccumulator{}

	desc := &sdk.EventDescription{Title: "Person at front door"}
	acc.merge(sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000,
		Segments: []sdk.EventSegment{{
			Detections:  []sdk.EventDetection{{Label: "person", Score: 0.72}},
			Zones:       []string{"driveway"},
			Description: desc,
		}},
	})
	merged := acc.merge(sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000, State: sdk.DetectionEventStateEnded, EndTime: 5000,
	})

	if len(merged.Segments) != 1 {
		t.Fatalf("expected exactly 1 synthesized segment, got %+v", merged.Segments)
	}
	if got := merged.Segments[0].Zones; len(got) != 1 || got[0] != "driveway" {
		t.Fatalf("expected the empty terminal message to leave Zones=[driveway] unchanged, got %v", got)
	}
	if got := merged.Segments[0].Description; got == nil || got.Title != desc.Title {
		t.Fatalf("expected the empty terminal message to leave Description unchanged, got %+v", got)
	}
}

// TestDetectionAccumulator_Merge_EvictsOnTerminalMessage proves the
// accumulator forgets an event once its terminal message (State ==
// DetectionEventStateEnded, or a nonzero EndTime) has been merged, so it
// doesn't grow forever across this plugin's lifetime.
func TestDetectionAccumulator_Merge_EvictsOnTerminalMessage(t *testing.T) {
	acc := &detectionAccumulator{}

	acc.merge(sdk.DetectionEvent{ID: "evt-1", CameraID: "cam1", StartTime: 1000})
	if got := acc.size(); got != 1 {
		t.Fatalf("expected 1 tracked entry after the start message, got %d", got)
	}

	acc.merge(sdk.DetectionEvent{ID: "evt-1", CameraID: "cam1", StartTime: 1000, State: sdk.DetectionEventStateEnded, EndTime: 5000})
	if got := acc.size(); got != 0 {
		t.Fatalf("expected the entry to be evicted after the terminal message, got %d tracked", got)
	}

	// An out-of-order message for the now-evicted id must not panic and
	// starts a fresh accumulation rather than erroring.
	merged := acc.merge(sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", StartTime: 1000,
		Segments: []sdk.EventSegment{{Detections: []sdk.EventDetection{{Label: "person", Score: 0.5}}}},
	})
	if len(merged.Segments) != 1 || merged.Segments[0].Detections[0].Label != "person" {
		t.Fatalf("expected a fresh accumulation for the re-arriving id, got %+v", merged.Segments)
	}
}

// TestDetectionAccumulator_Merge_DoesNotGrowUnbounded proves a pathological
// stream of events that never terminate is still bounded by
// detectionAccumulatorCap rather than leaking one entry per event forever.
func TestDetectionAccumulator_Merge_DoesNotGrowUnbounded(t *testing.T) {
	acc := &detectionAccumulator{}

	for i := 0; i < detectionAccumulatorCap*2; i++ {
		acc.merge(sdk.DetectionEvent{ID: fmt.Sprintf("evt-%d", i), CameraID: "cam1", StartTime: int64(i)})
	}

	if got := acc.size(); got != detectionAccumulatorCap {
		t.Fatalf("expected size to be capped at %d, got %d", detectionAccumulatorCap, got)
	}
}

// TestDetectionAccumulator_Merge_ConcurrentHandleCalls exercises handle
// (not just merge directly) from many goroutines at once, for both distinct
// event ids and repeated messages for the SAME id, to be run under -race:
// OnDetectionEvent's callback has no documented single-goroutine guarantee,
// so the accumulator's map access must be safe under concurrent handle
// calls.
func TestDetectionAccumulator_Merge_ConcurrentHandleCalls(t *testing.T) {
	fake := &fakeEventStore{}
	ingester := newDetectionEventIngester(fake, nil, nil, nil, nil, nil, nil)

	var wg sync.WaitGroup
	const goroutines = 50
	const messagesPerGoroutine = 20

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for m := 0; m < messagesPerGoroutine; m++ {
				ingester.handle(sdk.DetectionEventSegmentUpdate, sdk.DetectionEvent{
					ID:        fmt.Sprintf("evt-%d", g),
					CameraID:  "cam1",
					StartTime: 1000,
					Segments:  []sdk.EventSegment{{Detections: []sdk.EventDetection{{Label: "person", Score: float64(m) / 100}}}},
				})
			}
		}(g)
	}

	// Also hammer the SAME event id concurrently from multiple goroutines.
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ingester.handle(sdk.DetectionEventSegmentUpdate, sdk.DetectionEvent{
				ID:        "evt-shared",
				CameraID:  "cam1",
				StartTime: 1000,
				Segments:  []sdk.EventSegment{{Detections: []sdk.EventDetection{{Label: "person", Score: float64(g) / 100}}}},
			})
		}(g)
	}

	wg.Wait()
}
