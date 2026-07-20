package main

import (
	"testing"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/recorder"
	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// newTestPluginWithDB constructs an NVRPlugin backed by a real, temporary
// SQLite database (store.Open(t.TempDir())) — the same wiring NewPlugin
// does in production (plugin.go) — so the read-path RPC handlers in
// rpc_events.go/rpc_recording.go can be exercised against real
// EventStore/SegmentStore/SystemEventStore instances rather than fakes, per
// the task brief's TDD instructions ("seed the real store(s) in a temp
// DB"). Distinct from newTestPlugin (plugin_rpc_test.go), which
// deliberately has no store.DB at all — that constructor is for
// getManagedCameraIds/getInstanceId, which never touch it.
func newTestPluginWithDB(t *testing.T) *NVRPlugin {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	fake := newFakeInstanceStore()
	p := &NVRPlugin{
		recorder:     recorder.NewRecorderManager(),
		store:        fake,
		db:           db,
		events:       store.NewEventStore(db),
		segments:     store.NewSegmentStore(db),
		systemEvents: store.NewSystemEventStore(db),
	}
	declarePluginSchemas(p, fake)
	return p
}

func newTestDetectionEvent(id, cameraID string, startMs int64) DetectionEvent {
	return DetectionEvent{
		ID:         id,
		CameraID:   cameraID,
		State:      sdk.DetectionEventStateEnded,
		StartTime:  startMs,
		LastUpdate: startMs,
		Types:      []string{"motion"},
	}
}

// --- getEvents / getCameraEvents -------------------------------------------

func TestGetEvents_DelegatesToEventStoreAcrossAllCameras(t *testing.T) {
	p := newTestPluginWithDB(t)

	if err := p.events.Upsert([]DetectionEvent{
		newTestDetectionEvent("e1", "cam1", 1000),
		newTestDetectionEvent("e2", "cam2", 2000),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	result, err := p.GetEvents(GetEventsOptions{})
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("expected 2 events across both cameras, got %d: %+v", len(result.Events), result.Events)
	}
}

func TestGetEvents_NoStoreReturnsEmptyNotNil(t *testing.T) {
	p := &NVRPlugin{recorder: recorder.NewRecorderManager()}
	result, err := p.GetEvents(GetEventsOptions{})
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if result.Events == nil {
		t.Fatalf("expected a non-nil empty slice when p.events is nil, got nil")
	}
}

func TestGetCameraEvents_FiltersToRequestedCameras(t *testing.T) {
	p := newTestPluginWithDB(t)

	if err := p.events.Upsert([]DetectionEvent{
		newTestDetectionEvent("e1", "cam1", 1000),
		newTestDetectionEvent("e2", "cam2", 2000),
		newTestDetectionEvent("e3", "cam3", 3000),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	result, err := p.GetCameraEvents([]string{"cam1", "cam3"}, GetEventsOptions{})
	if err != nil {
		t.Fatalf("GetCameraEvents: %v", err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("expected 2 events (cam1+cam3 only), got %d: %+v", len(result.Events), result.Events)
	}
	for _, ev := range result.Events {
		if ev.CameraID == "cam2" {
			t.Fatalf("expected cam2's event to be excluded, got %+v", result.Events)
		}
	}
}

// --- getSystemEvents --------------------------------------------------------

func TestGetSystemEvents_QueriesSystemEventStore(t *testing.T) {
	p := newTestPluginWithDB(t)

	if err := p.systemEvents.Insert([]SystemEvent{
		{ID: "s1", Type: "recorder-start", Severity: "info", CameraID: "cam1", Timestamp: 1000, Message: "started"},
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	result, err := p.GetSystemEvents([]string{"cam1"}, GetSystemEventsOptions{})
	if err != nil {
		t.Fatalf("GetSystemEvents: %v", err)
	}
	if len(result.Events) != 1 || result.Events[0].ID != "s1" {
		t.Fatalf("expected [s1], got %+v", result.Events)
	}
}

func TestGetSystemEvents_NoStoreReturnsEmptyNotNil(t *testing.T) {
	p := &NVRPlugin{recorder: recorder.NewRecorderManager()}
	result, err := p.GetSystemEvents(nil, GetSystemEventsOptions{})
	if err != nil {
		t.Fatalf("GetSystemEvents: %v", err)
	}
	if result.Events == nil {
		t.Fatalf("expected a non-nil empty slice when p.systemEvents is nil, got nil")
	}
}

// --- getEventThumbnails ------------------------------------------------------

func TestGetEventThumbnails_EmptyWhenEventHasNoThumbnails(t *testing.T) {
	p := newTestPluginWithDB(t)

	if err := p.events.Upsert([]DetectionEvent{newTestDetectionEvent("e1", "cam1", 1000)}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	thumbs, err := p.GetEventThumbnails("cam1", 1000, "e1")
	if err != nil {
		t.Fatalf("GetEventThumbnails: %v", err)
	}
	if thumbs.Event != nil || thumbs.Scenes != nil || thumbs.Detections != nil || thumbs.Attributes != nil {
		t.Fatalf("expected an all-empty EventThumbnails, got %+v", thumbs)
	}
}

func TestGetEventThumbnails_UnknownEventReturnsEmpty(t *testing.T) {
	p := newTestPluginWithDB(t)
	thumbs, err := p.GetEventThumbnails("cam1", 1000, "does-not-exist")
	if err != nil {
		t.Fatalf("GetEventThumbnails: %v", err)
	}
	if thumbs.Event != nil {
		t.Fatalf("expected empty EventThumbnails for an unknown event, got %+v", thumbs)
	}
}

func TestGetEventThumbnails_PopulatedFromStoredEvent(t *testing.T) {
	p := newTestPluginWithDB(t)

	ev := newTestDetectionEvent("e1", "cam1", 1000)
	ev.Thumbnail = []byte("event-jpeg")
	ev.Segments = []sdk.EventSegment{
		{
			FirstSeen: 1000,
			LastSeen:  1500,
			Thumbnail: []byte("scene-jpeg"),
			Detections: []sdk.EventDetection{
				{Label: "person", Score: 0.9, Thumbnail: []byte("person-jpeg")},
			},
			Attributes: []sdk.EventAttribute{
				{Type: "face", Label: "john", Thumbnail: []byte("face-jpeg")},
			},
		},
	}
	if err := p.events.Upsert([]DetectionEvent{ev}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	thumbs, err := p.GetEventThumbnails("cam1", 1000, "e1")
	if err != nil {
		t.Fatalf("GetEventThumbnails: %v", err)
	}
	if string(thumbs.Event) != "event-jpeg" {
		t.Fatalf("expected Event=event-jpeg, got %q", thumbs.Event)
	}
	if string(thumbs.Scenes["0"]) != "scene-jpeg" {
		t.Fatalf("expected Scenes[0]=scene-jpeg, got %+v", thumbs.Scenes)
	}
	if string(thumbs.Detections["0:person"]) != "person-jpeg" {
		t.Fatalf("expected Detections[0:person]=person-jpeg, got %+v", thumbs.Detections)
	}
	if string(thumbs.Attributes["face:john"]) != "face-jpeg" {
		t.Fatalf("expected Attributes[face:john]=face-jpeg, got %+v", thumbs.Attributes)
	}
}

// --- getDetectionHeatmap -----------------------------------------------------

func TestGetDetectionHeatmap_NormalizesBoxCentersAndCountsEvents(t *testing.T) {
	p := newTestPluginWithDB(t)

	evWithBox := newTestDetectionEvent("e1", "cam1", 1000)
	evWithBox.Segments = []sdk.EventSegment{
		{
			FirstSeen: 1000,
			LastSeen:  1500,
			Detections: []sdk.EventDetection{
				{Label: "person", Score: 0.9, Box: &sdk.BoundingBox{X: 0.2, Y: 0.4, Width: 0.2, Height: 0.2}},
			},
		},
	}
	evNoBox := newTestDetectionEvent("e2", "cam1", 2000)

	if err := p.events.Upsert([]DetectionEvent{evWithBox, evNoBox}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	result, err := p.GetDetectionHeatmap("cam1", 0, 5000)
	if err != nil {
		t.Fatalf("GetDetectionHeatmap: %v", err)
	}
	if result.Count != 2 {
		t.Fatalf("expected Count=2 (total events, regardless of boxes), got %d", result.Count)
	}
	if len(result.Points) != 1 {
		t.Fatalf("expected 1 point (only evWithBox has a boxed detection), got %d: %+v", len(result.Points), result.Points)
	}
	const epsilon = 1e-9
	if diff := result.Points[0].X - 0.3; diff > epsilon || diff < -epsilon {
		t.Errorf("expected center X=0.3 (0.2+0.2/2), got %v", result.Points[0].X)
	}
	if diff := result.Points[0].Y - 0.5; diff > epsilon || diff < -epsilon {
		t.Errorf("expected center Y=0.5 (0.4+0.2/2), got %v", result.Points[0].Y)
	}
}

func TestGetDetectionHeatmap_NoStoreReturnsEmptyNotNil(t *testing.T) {
	p := &NVRPlugin{recorder: recorder.NewRecorderManager()}
	result, err := p.GetDetectionHeatmap("cam1", 0, 1000)
	if err != nil {
		t.Fatalf("GetDetectionHeatmap: %v", err)
	}
	if result.Points == nil {
		t.Fatalf("expected a non-nil empty Points slice, got nil")
	}
}

// --- RPCMethods allow-list ---------------------------------------------------

func TestRPCMethods_IncludesEveryReadPathMethod(t *testing.T) {
	p := newTestPlugin(t)
	allowed := map[string]bool{}
	for _, name := range p.RPCMethods() {
		allowed[name] = true
	}
	for _, want := range []string{
		"getEvents",
		"getCameraEvents",
		"getRecordingDays",
		"getRecordingSegments",
		"getSystemEvents",
		"getStorageStats",
		"getEventThumbnails",
		"getDetectionHeatmap",
	} {
		if !allowed[want] {
			t.Errorf("expected RPCMethods() to include %q, got %v", want, p.RPCMethods())
		}
	}
}
