package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// heatmapEventFetchLimit bounds how many events GetDetectionHeatmap asks
// EventStore.Query for. EventStore has no dedicated "count matching rows"
// query (see events.go's Query/buildEventsQuery) — Query only ever returns
// a page plus a HasMore flag — so a literal "count = total events in the
// window" (the wire contract's DetectionHeatmapResult.Count) needs a query
// whose page is large enough to never actually hit its limit in practice.
// 100000 events is comfortably beyond what a single-site local NVR
// accumulates in any query window a heatmap would reasonably be requested
// over (this plugin's other event-volume assumptions — e.g. matchesSearch's
// doc comment in events.go — make the same "good enough at this scale, not
// designed for unbounded volume" tradeoff). If that assumption ever stops
// holding, EventStore would need a real COUNT(*) query instead.
const heatmapEventFetchLimit = 100000

// GetEvents returns detection events across every managed camera, filtered
// by opts. Registered as the RPC method "getEvents". opts is a plain
// (non-pointer) GetEventsOptions specifically so a frontend call with no
// arguments at all — getEvents() — still works: github.com/cameraui/rpc/go's
// dispatcher (handler.go's callHandler) zero-fills any parameter position
// the caller didn't supply, and GetEventsOptions' zero value already means
// "no filters, default page size" (see buildEventsQuery/needsPostFilter in
// events.go), so no nil-vs-missing distinction is needed here the way it is
// for the *individual* optional fields inside GetEventsOptions.
func (p *NVRPlugin) GetEvents(opts GetEventsOptions) (GetEventsResult, error) {
	p.logRPC("getEvents")
	if p.events == nil {
		return GetEventsResult{Events: []DetectionEvent{}, HasMore: false}, nil
	}
	result, err := p.events.Query(nil, opts)
	if err != nil {
		return GetEventsResult{}, err
	}
	if p.Logger != nil {
		raw, _ := json.Marshal(opts)
		p.Logger.Debug(fmt.Sprintf("nvr-local: getEvents -> %d events hasMore=%v opts=%s", len(result.Events), result.HasMore, string(raw)))
	}
	return normalizeEventsResult(result), nil
}

// GetCameraEvents returns detection events for exactly cameraIDs, filtered
// by opts. Registered as the RPC method "getCameraEvents".
func (p *NVRPlugin) GetCameraEvents(cameraIDs []string, opts GetEventsOptions) (GetEventsResult, error) {
	p.logRPC("getCameraEvents", summarizeIDs(cameraIDs))
	if p.events == nil {
		return GetEventsResult{Events: []DetectionEvent{}, HasMore: false}, nil
	}
	result, err := p.events.Query(cameraIDs, opts)
	if err != nil {
		return GetEventsResult{}, err
	}
	if p.Logger != nil {
		raw, _ := json.Marshal(opts)
		p.Logger.Debug(fmt.Sprintf("nvr-local: getCameraEvents cams=%d -> %d events hasMore=%v opts=%s", len(cameraIDs), len(result.Events), result.HasMore, string(raw)))
	}
	return normalizeEventsResult(result), nil
}

// normalizeEventsResult guards against EventStore.Query's nil-slice zero
// value (Query's `var rows []DetectionEvent` is never allocated when no
// rows match) reaching the wire as msgpack nil/JS null instead of an empty
// array — the frontend's GetEventsResult.events field is a required
// DetectionEvent[], not DetectionEvent[] | null, so a caller doing
// `result.events.map(...)` on a null would throw.
func normalizeEventsResult(r GetEventsResult) GetEventsResult {
	if r.Events == nil {
		r.Events = []DetectionEvent{}
	}
	return r
}

// GetSystemEvents returns system-level events for cameraIDs (every camera
// if empty, matching GetEvents/GetCameraEvents' distinction), filtered by
// opts. Registered as the RPC method "getSystemEvents".
//
// GAP: nothing in this plugin currently calls SystemEventStore.Insert (see
// that type's doc comment, src/store/system_events.go) — no code path
// (recorder start/stop, retention runs, disk-critical detection, ...) has
// been wired to populate the system_events table yet. This handler is
// therefore correct but will only ever return an empty result until a
// later task adds that producer side.
func (p *NVRPlugin) GetSystemEvents(cameraIDs []string, opts GetSystemEventsOptions) (GetSystemEventsResult, error) {
	p.logRPC("getSystemEvents", summarizeIDs(cameraIDs))
	if p.systemEvents == nil {
		return GetSystemEventsResult{Events: []SystemEvent{}, HasMore: false}, nil
	}
	result, err := p.systemEvents.Query(cameraIDs, opts)
	if err != nil {
		return GetSystemEventsResult{}, err
	}
	if result.Events == nil {
		result.Events = []SystemEvent{}
	}
	return result, nil
}

// GetEventThumbnails returns the thumbnail JPEGs stored inline on the event
// identified by (cameraID, startMs, eventID) — startMs is the event's own
// StartTime, which the frontend already knows (it's a DetectionEvent field)
// and passes back so this lookup can use EventStore's ts_ms index directly
// rather than scanning every event for the camera. Registered as the RPC
// method "getEventThumbnails".
//
// Thumbnail GENERATION/persistence for the primary event thumbnail is Task
// 11 (see src/media/thumbs.go: media.Generator, dispatched on every
// DetectionEvent lifecycle message from events_ingest.go's
// detectionEventIngester). Scene/detection/attribute thumbnails remain
// whatever happens to still be present on the stored event's raw JSON as of
// its most recent Upsert (see sdk.DetectionEvent.Thumbnail's own doc
// comment: "Inline only on the first message that delivers it ... the NVR
// plugin persists it and clients fetch it on demand") — Upsert replaces the
// whole `raw` column on every call, so a later message without inline
// thumbnail bytes overwrites earlier ones already stored; generating those
// nested thumbnails from disk too is not yet implemented (see
// thumbnailsFromEvent's DEFERRED note). This handler returns an all-empty
// EventThumbnails{} rather than an error when nothing is found at all,
// matching the brief's "empty maps if none yet" contract exactly.
func (p *NVRPlugin) GetEventThumbnails(cameraID string, startMs int64, eventID string) (EventThumbnails, error) {
	p.logRPC("getEventThumbnails", cameraID, eventID)
	if p.events == nil {
		return EventThumbnails{}, nil
	}

	result, err := p.events.Query([]string{cameraID}, GetEventsOptions{StartMs: &startMs, EndMs: &startMs})
	if err != nil {
		return EventThumbnails{}, err
	}
	for _, ev := range result.Events {
		if ev.ID == eventID {
			out := thumbnailsFromEvent(ev)
			if len(out.Event) == 0 {
				out.Event = p.loadGeneratedThumbnail(eventID)
			}
			return out, nil
		}
	}
	return EventThumbnails{}, nil
}

// loadGeneratedThumbnail returns the primary JPEG thumbnail
// media.Generator generated for eventID (Task 11), read from the path
// recorded on the event's thumb_ref column — or nil if none was ever
// generated (no covering segment at ingestion time, generation still
// pending/failed, or nothing has ingested this event at all). Any error
// reading thumb_ref or the file itself (e.g. the file existed but was since
// removed by retention) is treated identically to "none": thumbnail serving
// is always best-effort, never a reason to fail the RPC.
func (p *NVRPlugin) loadGeneratedThumbnail(eventID string) []byte {
	if p.events == nil {
		return nil
	}
	ref, err := p.events.GetThumbRef(eventID)
	if err != nil || ref == "" {
		return nil
	}
	data, err := os.ReadFile(ref)
	if err != nil {
		return nil
	}
	return data
}

// thumbnailsFromEvent flattens ev's inline thumbnail bytes (its own
// top-level Thumbnail, plus every segment/detection/attribute thumbnail
// nested inside ev.Segments) into the wire's EventThumbnails shape.
//
// Key formats are deliberately not arbitrary: they match exactly what the
// compiled frontend already parses in
// ui/src/components/CuiRecordings/RecordingCard.vue (in the camera.ui repo)
// — Scenes keyed by segment index as a bare numeric string ("0", "1", ...,
// sorted numerically there), Detections keyed by "<segmentIndex>:<label>"
// (RecordingCard.vue matches on `key.endsWith(':'+type)` and splits off
// everything after the first ':' for the label), Attributes keyed by
// "<type>:<label>" (RecordingCard.vue reads `key.split(':')[0]` as the
// attribute type). Getting these keys wrong wouldn't be a compile error or
// even a runtime error on this side — the frontend would just silently fail
// to find/display any thumbnail, since it already only reads, never
// validates, these maps.
//
// DEFERRED (Task 11 scope): Scenes/Detections/Attributes below are only
// ever populated from bytes an sdk.DetectionEvent message happened to carry
// inline — nothing generates per-segment/per-detection/per-attribute crops
// from a recorded segment the way media.Generator does for the primary
// Event thumbnail. The task brief calls per-detection crops optional
// ("scenes/attributes can stay empty"); this plugin currently leaves all
// three empty unless the SDK itself already inlined them.
func thumbnailsFromEvent(ev DetectionEvent) EventThumbnails {
	var out EventThumbnails

	if len(ev.Thumbnail) > 0 {
		out.Event = ev.Thumbnail
	}

	for segIdx, seg := range ev.Segments {
		if len(seg.Thumbnail) > 0 {
			if out.Scenes == nil {
				out.Scenes = make(map[string][]byte)
			}
			out.Scenes[fmt.Sprintf("%d", segIdx)] = seg.Thumbnail
		}
		for _, det := range seg.Detections {
			if len(det.Thumbnail) == 0 {
				continue
			}
			if out.Detections == nil {
				out.Detections = make(map[string][]byte)
			}
			out.Detections[fmt.Sprintf("%d:%s", segIdx, det.Label)] = det.Thumbnail
		}
		for _, attr := range seg.Attributes {
			if len(attr.Thumbnail) == 0 {
				continue
			}
			if out.Attributes == nil {
				out.Attributes = make(map[string][]byte)
			}
			out.Attributes[fmt.Sprintf("%s:%s", attr.Type, attr.Label)] = attr.Thumbnail
		}
	}

	return out
}

// GetDetectionHeatmap aggregates every boxed detection's normalized (0..1)
// bounding-box center, across every event in [startMs, endMs] for cameraID,
// into HeatmapPoints. Count is the total number of events in the window
// (per the wire contract), which is not the same as len(Points): an event
// whose detections carry no bounding box at all contributes to Count but no
// point, and an event with several boxed detections across its segments
// contributes more than one point — a genuine density heatmap needs every
// detection's location, not one point per event. Registered as the RPC
// method "getDetectionHeatmap".
func (p *NVRPlugin) GetDetectionHeatmap(cameraID string, startMs, endMs int64) (DetectionHeatmapResult, error) {
	p.logRPC("getDetectionHeatmap", cameraID)
	if p.events == nil {
		return DetectionHeatmapResult{Points: []HeatmapPoint{}, Count: 0}, nil
	}

	limit := int64(heatmapEventFetchLimit)
	result, err := p.events.Query([]string{cameraID}, GetEventsOptions{StartMs: &startMs, EndMs: &endMs, Limit: &limit})
	if err != nil {
		return DetectionHeatmapResult{}, err
	}

	points := make([]HeatmapPoint, 0)
	for _, ev := range result.Events {
		for _, seg := range ev.Segments {
			for _, det := range seg.Detections {
				if det.Box == nil {
					continue
				}
				points = append(points, HeatmapPoint{
					X: det.Box.X + det.Box.Width/2,
					Y: det.Box.Y + det.Box.Height/2,
				})
			}
		}
	}

	return DetectionHeatmapResult{Points: points, Count: len(result.Events)}, nil
}

// summarizeIDs joins ids for logRPC's arg summary, capped so a call with an
// unusually large camera-id list still produces one short log line rather
// than one long enough to defeat the "not full payloads" point of logRPC.
func summarizeIDs(ids []string) string {
	const maxLogged = 5
	if len(ids) == 0 {
		return "cameras=all"
	}
	shown := ids
	suffix := ""
	if len(shown) > maxLogged {
		shown = shown[:maxLogged]
		suffix = fmt.Sprintf(" (+%d more)", len(ids)-maxLogged)
	}
	summary := "cameras="
	for i, id := range shown {
		if i > 0 {
			summary += ","
		}
		summary += id
	}
	return summary + suffix
}
