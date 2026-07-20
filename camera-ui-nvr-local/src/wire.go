package main

import "github.com/calebcall/plugins/camera-ui-nvr-local/src/store"

// wire.go holds the msgpack request/response types mirroring the frontend's
// reconstructed contract (docs/superpowers/specs/2026-07-19-nvr-frontend-contract.d.ts
// in the camera.ui repo), for the RPC handlers later tasks add to this
// package.
//
// DetectionEvent, GetEventsOptions, and GetEventsResult are declared here as
// type aliases for the canonical definitions in package store
// (src/store/events.go) rather than as separate struct types. EventStore
// (store.EventStore) already needs exactly these shapes for Upsert/Query,
// msgpack tags and all — defining them a second time here, with the same
// field names and tags, would just be a duplicate that could silently drift
// out of sync with what EventStore actually persists/returns. Aliasing keeps
// a single source of truth: this package's future getEvents/getCameraEvents
// RPC handlers can use the bare names below and pass the result straight
// through to/from store.EventStore with no conversion step.
type (
	DetectionEvent   = store.DetectionEvent
	GetEventsOptions = store.GetEventsOptions
	GetEventsResult  = store.GetEventsResult

	// SystemEvent, GetSystemEventsOptions, and GetSystemEventsResult are
	// aliased the same way, for the same reason: store.SystemEventStore
	// (src/store/system_events.go) already needs exactly these shapes for
	// Insert/Query, and defining them a second time here would just be a
	// duplicate that could drift out of sync.
	SystemEvent            = store.SystemEvent
	GetSystemEventsOptions = store.GetSystemEventsOptions
	GetSystemEventsResult  = store.GetSystemEventsResult
)

// RecordingSegment mirrors the frontend's RecordingSegment
// (docs/superpowers/specs/2026-07-19-nvr-frontend-contract.d.ts in the
// camera.ui repo): a single continuous recorded time range for a camera.
// Built by GetRecordingSegments (rpc_recording.go) from one or more
// store.Segment rows (SegmentStore.InRange) merged across adjacent/
// overlapping ranges and, when more than one stream role is recorded,
// across roles too — see mergeSegments' doc comment. Unlike store.Segment,
// this has no Role/Path/Codec/... fields: the frontend's timeline only
// cares about "was something recorded during this window", not which file
// or stream provided it.
type RecordingSegment struct {
	StartTime int64  `msgpack:"startTime" json:"startTime"`
	EndTime   int64  `msgpack:"endTime" json:"endTime"`
	CameraID  string `msgpack:"cameraId,omitempty" json:"cameraId,omitempty"`
}

// CameraStorageStats mirrors the frontend's CameraStorageStats: one managed
// camera's contribution to GetStorageStats' StorageStats.Cameras map, built
// by cameraStorageStats (rpc_recording.go) from SegmentStore.AllByCamera
// plus a live RecorderManager lookup for RecordingMode/IsRecording.
type CameraStorageStats struct {
	UsedBytes     int64   `msgpack:"usedBytes" json:"usedBytes"`
	SegmentCount  int     `msgpack:"segmentCount" json:"segmentCount"`
	OldestDay     string  `msgpack:"oldestDay" json:"oldestDay"`
	NewestDay     string  `msgpack:"newestDay" json:"newestDay"`
	DaysCount     int     `msgpack:"daysCount" json:"daysCount"`
	BandwidthMBh  float64 `msgpack:"bandwidthMBh" json:"bandwidthMBh"`
	RecordingMode string  `msgpack:"recordingMode" json:"recordingMode"`
	IsRecording   bool    `msgpack:"isRecording" json:"isRecording"`
}

// StorageStats mirrors the frontend's StorageStats: GetStorageStats'
// (rpc_recording.go) full result — disk-level stats (from this plugin's
// data directory's filesystem, see diskstats_unix.go/diskstats_windows.go)
// plus this NVR instance's own usage/quota/retention and a per-camera
// breakdown.
type StorageStats struct {
	DiskTotalGB     float64                       `msgpack:"diskTotalGB" json:"diskTotalGB"`
	DiskUsedGB      float64                       `msgpack:"diskUsedGB" json:"diskUsedGB"`
	DiskFreeGB      float64                       `msgpack:"diskFreeGB" json:"diskFreeGB"`
	DiskFreePercent float64                       `msgpack:"diskFreePercent" json:"diskFreePercent"`
	NvrUsedGB       float64                       `msgpack:"nvrUsedGB" json:"nvrUsedGB"`
	NvrQuotaGB      float64                       `msgpack:"nvrQuotaGB" json:"nvrQuotaGB"`
	RetentionDays   int                           `msgpack:"retentionDays" json:"retentionDays"`
	SmallVolume     bool                          `msgpack:"smallVolume" json:"smallVolume"`
	Paused          bool                          `msgpack:"paused" json:"paused"`
	Cameras         map[string]CameraStorageStats `msgpack:"cameras" json:"cameras"`
}

// EventThumbnails mirrors the frontend's EventThumbnails: the JPEG bytes
// stored inline on a DetectionEvent's own Thumbnail/segment/detection/
// attribute fields (sdk.DetectionEvent, sdk.EventSegment, ...), keyed the
// way ui/src/components/CuiRecordings/RecordingCard.vue's thumbnail-picking
// logic expects: Scenes by segment index ("0", "1", ...), Detections by
// "<segmentIndex>:<label>", Attributes by "<type>:<label>". See
// thumbnailsFromEvent (rpc_events.go) for where these keys are built.
type EventThumbnails struct {
	Event      []byte            `msgpack:"event,omitempty" json:"event,omitempty"`
	Scenes     map[string][]byte `msgpack:"scenes,omitempty" json:"scenes,omitempty"`
	Detections map[string][]byte `msgpack:"detections,omitempty" json:"detections,omitempty"`
	Attributes map[string][]byte `msgpack:"attributes,omitempty" json:"attributes,omitempty"`
}

// HeatmapPoint mirrors the frontend's HeatmapPoint: one detection's
// normalized (0..1) bounding-box center.
type HeatmapPoint struct {
	X float64 `msgpack:"x" json:"x"`
	Y float64 `msgpack:"y" json:"y"`
}

// DetectionHeatmapResult mirrors the frontend's DetectionHeatmapResult:
// GetDetectionHeatmap's (rpc_events.go) result. Count is the total number
// of events in the requested window, which is not necessarily len(Points) —
// an event with no detection carrying a bounding box contributes to Count
// but not to Points, and an event with multiple boxed detections
// contributes more than one point.
type DetectionHeatmapResult struct {
	Points []HeatmapPoint `msgpack:"points" json:"points"`
	Count  int            `msgpack:"count" json:"count"`
}
