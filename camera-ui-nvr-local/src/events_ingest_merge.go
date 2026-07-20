package main

import (
	"sort"
	"sync"

	sdk "github.com/cameraui/sdk/go"
)

// detectionAccumulatorCap bounds the number of concurrently-tracked events
// detectionAccumulator will hold before evicting the oldest one on insert.
// Normal operation never gets close to this: every event's entry is evicted
// the moment its terminal message arrives (see merge's doc comment) — this
// cap exists purely as a backstop against a pathological producer that
// starts events it never ends, so a leak there is bounded rather than
// unbounded.
const detectionAccumulatorCap = 4096

// attributeKey identifies an EventAttribute for accumulation purposes: the
// SDK has no natural single-field identity for an attribute the way
// EventDetection has Label, so (Type, Label) — e.g. ("face", "alice") or
// ("license_plate", "ABC123") — is the closest equivalent.
type attributeKey struct {
	Type  string
	Label string
}

// accumulatedEvent is the running union of every EventDetection/EventAttribute
// observed so far across one event's lifecycle messages, keyed the same way
// detectionAccumulator.merge folds a new message's Segments into it: at most
// one EventDetection per Label (the highest-Score one seen) and at most one
// EventAttribute per (Type, Label) (the highest-Confidence one seen).
type accumulatedEvent struct {
	detections map[string]sdk.EventDetection
	attributes map[attributeKey]sdk.EventAttribute
	types      map[string]struct{}
	// thumbnail is the most recently observed non-empty
	// sdk.DetectionEvent.Thumbnail across this event's messages. Most
	// messages carry none at all (see DetectionEvent.Thumbnail's doc
	// comment: inline only on the message that first delivers it) — an
	// empty later message must not blank out an earlier real one, the same
	// clobbering class of bug this whole accumulator exists to fix for
	// Segments.
	thumbnail []byte
	// segmentThumbnail is the most recently observed non-empty
	// EventSegment.Thumbnail (the per-segment "scene" JPEG) across this
	// event's messages — present on 'segment-start'/'segment-end' and the
	// first 'segment-update' with a candidate (see EventSegment.Thumbnail's
	// doc comment), and just as absent from the sparse terminal 'end'/plain
	// 'update' snapshots as Detections is. rpc_events.go's
	// thumbnailsFromEvent reads exactly this field (via ev.Segments[0]) to
	// populate EventThumbnails.Scenes, so losing it here silently empties
	// Scenes for every event that goes through this accumulator — the same
	// no-clobber rule as the event-level thumbnail above applies.
	segmentThumbnail []byte
	// zones is the union of every EventSegment.Zones name seen (deduplicated
	// the same way a single segment message already deduplicates its own).
	zones map[string]struct{}
	// description is the most recently observed non-nil
	// EventSegment.Description across this event's messages — an
	// AI-generated description is expected to arrive on at most one message
	// (if at all), so "most recent non-nil" is also "the only one", but
	// applying the same no-clobber rule keeps this consistent with
	// thumbnail/segmentThumbnail above rather than a special case.
	description *sdk.EventDescription
}

// detectionAccumulator merges each sdk.DetectionEvent lifecycle message
// (start/update/segment-start/segment-update/segment-end/end) into a running
// per-event union of its detections and attributes, so that a later message
// carrying a sparse snapshot (observed: the terminal 'end' and plain
// 'update' messages arrive with Segments:[] and no score at all, while the
// segment-* messages in between carry the real detections) never erases
// data an earlier message already reported. Without this, EventStore.Upsert
// — which replaces a stored event by id on every call (ON CONFLICT(id) DO
// UPDATE) — persists whatever the LAST message for an event happened to
// carry, which for a typical person/vehicle event is the empty terminal
// snapshot: every event ends up confidence=0, primaryLabel="motion", no
// detections, which then fails the frontend's default minConfidence/label
// filters and the recordings view shows nothing.
//
// Guarded by mu because handle (events_ingest.go) is the callback
// sdk.CameraDevice.OnDetectionEvent invokes with no documented
// single-goroutine guarantee: concurrent messages for different events (or,
// in principle, racing messages for the same event) must not corrupt the
// map or its entries.
type detectionAccumulator struct {
	mu      sync.Mutex
	entries map[string]*accumulatedEvent
	// order tracks entries' insertion order (oldest first), solely so
	// evictOverCapLocked knows which entry to drop first if the cap is
	// ever hit; it is not consulted for anything else.
	order []string
}

// merge folds event's Segments (Detections/Attributes), Types, and
// Thumbnail into this event's running accumulatedEvent (creating one if this
// is the first message seen for event.ID), then returns a copy of event
// whose Types/Segments/Thumbnail are replaced by the accumulated union so
// far — so store/events.go's bestConfidence/primaryLabel (which scan
// ev.Segments[].Detections/Attributes) and eventHasDetections (which scans
// ev.Types) see the richest data observed across the event's ENTIRE
// lifecycle, not just whatever this one message happened to carry. Every
// other field (ID, CameraID, State, StartTime, EndTime, LastUpdate,
// Triggers, SegmentIndex, ExpectedEndTime, HasRecording) is left exactly as
// event carried it — those are naturally monotonic/"latest wins" already
// (StartTime never changes; EndTime/LastUpdate/State only move forward),
// unlike Segments/Thumbnail which can regress to empty on a later message.
//
// Once event is terminal (State == DetectionEventStateEnded, or a nonzero
// EndTime — the same test resolveHasRecording uses to detect an ended
// event), the accumulator entry for event.ID is evicted: the terminal
// message's merged result already carries the full accumulated union, so
// there is nothing further for a later message to add, and holding onto the
// entry would leak memory for every event this plugin ever ingests. An
// out-of-order message that somehow arrives for an already-evicted id
// starts a fresh (empty) accumulation rather than erroring — its own
// Segments, if it carries any, still get stored via that fresh entry.
func (a *detectionAccumulator) merge(event sdk.DetectionEvent) sdk.DetectionEvent {
	a.mu.Lock()

	entry := a.getOrCreateLocked(event.ID)

	for _, seg := range event.Segments {
		for _, d := range seg.Detections {
			if existing, ok := entry.detections[d.Label]; !ok || d.Score > existing.Score {
				entry.detections[d.Label] = d
			}
		}
		for _, attr := range seg.Attributes {
			key := attributeKey{Type: attr.Type, Label: attr.Label}
			if existing, ok := entry.attributes[key]; !ok || attr.Confidence > existing.Confidence {
				entry.attributes[key] = attr
			}
		}
		if len(seg.Thumbnail) > 0 {
			entry.segmentThumbnail = seg.Thumbnail
		}
		for _, zone := range seg.Zones {
			entry.zones[zone] = struct{}{}
		}
		if seg.Description != nil {
			entry.description = seg.Description
		}
	}
	for _, t := range event.Types {
		entry.types[t] = struct{}{}
	}
	if len(event.Thumbnail) > 0 {
		entry.thumbnail = event.Thumbnail
	}

	merged := event
	merged.Types = sortedTypeUnion(entry.types)
	merged.Segments = synthesizeSegments(entry, event)
	if len(entry.thumbnail) > 0 {
		merged.Thumbnail = entry.thumbnail
	}

	if event.State == sdk.DetectionEventStateEnded || event.EndTime > 0 {
		a.evictLocked(event.ID)
	}

	a.mu.Unlock()
	return merged
}

// size reports how many events are currently accumulating (i.e. have been
// seen but not yet evicted by a terminal message or the cap). Only used by
// tests, to prove eviction actually bounds growth rather than leaking.
func (a *detectionAccumulator) size() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.entries)
}

// getOrCreateLocked returns id's accumulatedEvent, creating and registering
// one (in both a.entries and a.order) if this is the first message seen for
// it. Must be called with a.mu already held.
func (a *detectionAccumulator) getOrCreateLocked(id string) *accumulatedEvent {
	if a.entries == nil {
		a.entries = make(map[string]*accumulatedEvent)
	}
	if entry, ok := a.entries[id]; ok {
		return entry
	}
	entry := &accumulatedEvent{
		detections: make(map[string]sdk.EventDetection),
		attributes: make(map[attributeKey]sdk.EventAttribute),
		types:      make(map[string]struct{}),
		zones:      make(map[string]struct{}),
	}
	a.entries[id] = entry
	a.order = append(a.order, id)
	a.evictOverCapLocked()
	return entry
}

// evictOverCapLocked drops the oldest tracked entries until a.entries is
// back within detectionAccumulatorCap. Must be called with a.mu already
// held.
func (a *detectionAccumulator) evictOverCapLocked() {
	for len(a.order) > detectionAccumulatorCap {
		oldest := a.order[0]
		a.order = a.order[1:]
		delete(a.entries, oldest)
	}
}

// evictLocked removes id's entry entirely. Must be called with a.mu already
// held.
func (a *detectionAccumulator) evictLocked(id string) {
	delete(a.entries, id)
	for idx, existing := range a.order {
		if existing == id {
			a.order = append(a.order[:idx], a.order[idx+1:]...)
			break
		}
	}
}

// sortedTypeUnion returns types' keys sorted, so merge's output is
// deterministic (map iteration order is not) rather than flapping between
// otherwise-equivalent Upserts.
func sortedTypeUnion(types map[string]struct{}) []string {
	if len(types) == 0 {
		return nil
	}
	out := make([]string, 0, len(types))
	for t := range types {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// synthesizeSegments rebuilds event.Segments as a single segment spanning
// entry's full accumulated union of detections/attributes/scene
// thumbnail/zones/description, so downstream code that scans ev.Segments
// (bestConfidence, primaryLabel, GetDetectionHeatmap, thumbnailsFromEvent's
// Scenes map, ...) sees everything observed across the event's lifecycle
// rather than only whichever single segment-* message happened to produce
// this one. Returns nil (not an empty non-nil slice) when nothing at all
// has been accumulated yet — e.g. a motion-only event that never carries a
// segment — matching the zero-value Segments a plain start/end message
// already carries, so a motion event's stored shape is unchanged from
// before this accumulator existed.
func synthesizeSegments(entry *accumulatedEvent, event sdk.DetectionEvent) []sdk.EventSegment {
	if len(entry.detections) == 0 && len(entry.attributes) == 0 && len(entry.segmentThumbnail) == 0 && len(entry.zones) == 0 && entry.description == nil {
		return nil
	}

	detections := make([]sdk.EventDetection, 0, len(entry.detections))
	labels := make([]string, 0, len(entry.detections))
	for label := range entry.detections {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		detections = append(detections, entry.detections[label])
	}

	attributes := make([]sdk.EventAttribute, 0, len(entry.attributes))
	keys := make([]attributeKey, 0, len(entry.attributes))
	for key := range entry.attributes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Type != keys[j].Type {
			return keys[i].Type < keys[j].Type
		}
		return keys[i].Label < keys[j].Label
	})
	for _, key := range keys {
		attributes = append(attributes, entry.attributes[key])
	}

	var zones []string
	if len(entry.zones) > 0 {
		zones = make([]string, 0, len(entry.zones))
		for z := range entry.zones {
			zones = append(zones, z)
		}
		sort.Strings(zones)
	}

	return []sdk.EventSegment{{
		FirstSeen:   event.StartTime,
		LastSeen:    event.LastUpdate,
		Thumbnail:   entry.segmentThumbnail,
		Detections:  detections,
		Attributes:  attributes,
		Zones:       zones,
		Description: entry.description,
	}}
}
