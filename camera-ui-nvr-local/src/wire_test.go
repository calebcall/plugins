package main

import (
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

// msgpackKeys marshals v with msgpack (the wire encoding every RPC handler's
// return value actually travels over — see plugin.go's casing/dispatch
// findings) and decodes it back into a generic map, so the test can assert
// on the *actual on-wire field names* rather than just round-tripping
// through the same Go struct twice (which would pass even if every tag
// were misspelled, since Marshal/Unmarshal would agree with themselves
// either way). This is exactly the tag-fidelity guard the task brief asks
// for: "every arg order + return field/msgpack tag MUST match [the
// contract] exactly".
func msgpackKeys(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := msgpack.Marshal(v)
	if err != nil {
		t.Fatalf("msgpack.Marshal(%#v): %v", v, err)
	}
	var out map[string]any
	if err := msgpack.Unmarshal(data, &out); err != nil {
		t.Fatalf("msgpack.Unmarshal: %v", err)
	}
	return out
}

func assertHasKeys(t *testing.T, got map[string]any, want ...string) {
	t.Helper()
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("expected wire key %q, got keys %v", k, keysOf(got))
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestWire_RecordingSegment_MsgpackKeys(t *testing.T) {
	seg := RecordingSegment{StartTime: 1000, EndTime: 2000, CameraID: "cam1"}
	got := msgpackKeys(t, seg)
	assertHasKeys(t, got, "startTime", "endTime", "cameraId")

	// Round-trip: decoding back into the Go struct must reproduce the exact
	// values, proving the tags used for Marshal and Unmarshal agree (as
	// they must, being the same struct) as well as matching the wire.
	var back RecordingSegment
	data, _ := msgpack.Marshal(seg)
	if err := msgpack.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back != seg {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", back, seg)
	}
}

func TestWire_RecordingSegment_CameraIDOmittedWhenEmpty(t *testing.T) {
	got := msgpackKeys(t, RecordingSegment{StartTime: 1000, EndTime: 2000})
	if _, ok := got["cameraId"]; ok {
		t.Fatalf("expected cameraId to be omitted (omitempty) for a zero-value CameraID, got %v", got)
	}
}

func TestWire_CameraStorageStats_MsgpackKeys(t *testing.T) {
	stats := CameraStorageStats{
		UsedBytes:     123,
		SegmentCount:  4,
		OldestDay:     "2026-07-01",
		NewestDay:     "2026-07-19",
		DaysCount:     3,
		BandwidthMBh:  12.5,
		RecordingMode: "continuous",
		IsRecording:   true,
	}
	got := msgpackKeys(t, stats)
	assertHasKeys(t, got,
		"usedBytes", "segmentCount", "oldestDay", "newestDay",
		"daysCount", "bandwidthMBh", "recordingMode", "isRecording")
}

func TestWire_StorageStats_MsgpackKeys(t *testing.T) {
	stats := StorageStats{
		DiskTotalGB:     100,
		DiskUsedGB:      50,
		DiskFreeGB:      50,
		DiskFreePercent: 50,
		NvrUsedGB:       10,
		NvrQuotaGB:      0,
		RetentionDays:   7,
		SmallVolume:     false,
		Paused:          false,
		Cameras: map[string]CameraStorageStats{
			"cam1": {RecordingMode: "off"},
		},
	}
	got := msgpackKeys(t, stats)
	assertHasKeys(t, got,
		"diskTotalGB", "diskUsedGB", "diskFreeGB", "diskFreePercent",
		"nvrUsedGB", "nvrQuotaGB", "retentionDays", "smallVolume", "paused", "cameras")

	cameras, ok := got["cameras"].(map[string]any)
	if !ok {
		t.Fatalf("expected cameras to decode as a map, got %T", got["cameras"])
	}
	if _, ok := cameras["cam1"]; !ok {
		t.Fatalf("expected cameras map to be keyed by camera id, got %v", keysOf(cameras))
	}
}

func TestWire_SystemEvent_MsgpackKeys(t *testing.T) {
	dur := int64(5000)
	ev := SystemEvent{
		ID:        "s1",
		Type:      "disk-critical",
		Severity:  "error",
		CameraID:  "cam1",
		Timestamp: 1000,
		Duration:  &dur,
		Message:   "disk full",
	}
	got := msgpackKeys(t, ev)
	assertHasKeys(t, got, "id", "type", "severity", "cameraId", "timestamp", "duration", "message")
}

func TestWire_SystemEvent_OptionalFieldsOmittedWhenUnset(t *testing.T) {
	got := msgpackKeys(t, SystemEvent{ID: "s1", Type: "t", Severity: "info", Timestamp: 1000, Message: "m"})
	if _, ok := got["cameraId"]; ok {
		t.Fatalf("expected cameraId omitted for zero-value CameraID, got %v", got)
	}
	if _, ok := got["duration"]; ok {
		t.Fatalf("expected duration omitted for nil Duration, got %v", got)
	}
}

func TestWire_EventThumbnails_MsgpackKeys(t *testing.T) {
	thumbs := EventThumbnails{
		Event:      []byte{1, 2, 3},
		Scenes:     map[string][]byte{"0": {4, 5}},
		Detections: map[string][]byte{"0:person": {6}},
		Attributes: map[string][]byte{"face:john": {7}},
	}
	got := msgpackKeys(t, thumbs)
	assertHasKeys(t, got, "event", "scenes", "detections", "attributes")
}

func TestWire_EventThumbnails_EmptyOmitsEveryField(t *testing.T) {
	got := msgpackKeys(t, EventThumbnails{})
	if len(got) != 0 {
		t.Fatalf("expected every field to be omitted (omitempty) on a zero-value EventThumbnails, got keys %v", keysOf(got))
	}
}

func TestWire_DetectionHeatmapResult_MsgpackKeys(t *testing.T) {
	result := DetectionHeatmapResult{Points: []HeatmapPoint{{X: 0.5, Y: 0.25}}, Count: 3}
	got := msgpackKeys(t, result)
	assertHasKeys(t, got, "points", "count")

	points, ok := got["points"].([]any)
	if !ok || len(points) != 1 {
		t.Fatalf("expected points to decode as a 1-element slice, got %#v", got["points"])
	}
	point, ok := points[0].(map[string]any)
	if !ok {
		t.Fatalf("expected point to decode as a map, got %T", points[0])
	}
	assertHasKeys(t, point, "x", "y")
}

func TestWire_GetSystemEventsResult_MsgpackKeys(t *testing.T) {
	got := msgpackKeys(t, GetSystemEventsResult{Events: []SystemEvent{}, HasMore: true})
	assertHasKeys(t, got, "events", "hasMore")
}
