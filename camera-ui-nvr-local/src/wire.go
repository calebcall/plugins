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

// RecordingState mirrors the frontend's RecordingState (declared
// unexported in the .d.ts as RecordingState_2, re-exported under the
// RecordingState name): a single recording lifecycle transition for one
// camera. Published by OnRecordingState (rpc_subscriptions.go) to that
// method's callback subscribers whenever RecorderManager's stateNotify
// hook fires (see onRecorderStateChange, plugin.go, wired via
// recorder.RecorderManager.SetStateNotifier) — and once immediately on
// subscribe, with the camera's current state, so a client subscribing to
// an already-running (or already-stopped) camera doesn't have to wait for
// the next transition to initialize its UI.
type RecordingState struct {
	CameraID  string `msgpack:"cameraId" json:"cameraId"`
	State     string `msgpack:"state" json:"state"`
	Timestamp int64  `msgpack:"timestamp" json:"timestamp"`
}

// NvrScrubFrame mirrors the frontend's NvrScrubFrame: one Annex-B H.264
// access unit plus its playback timestamp (microseconds) and whether it's a
// keyframe. Used both as NvrScrubResult's optional multi-frame window
// (unused in this task's v1 — see NvrScrub, rpc_playback.go) and as
// NvrPreviewResult's filmstrip entries (always Keyframe: true — every
// preview sample is independently extracted as a keyframe, see
// media.Scrubber.PreviewFrames).
type NvrScrubFrame struct {
	Frame    []byte `msgpack:"frame" json:"frame"`
	Ts       int64  `msgpack:"ts" json:"ts"`
	Keyframe bool   `msgpack:"keyframe" json:"keyframe"`
}

// NvrScrubResult mirrors the frontend's NvrScrubResult: NvrScrub's
// (rpc_playback.go) result. Frame is the single extracted Annex-B keyframe
// (nil/omitted when NoData); Ts echoes back the requested tsUs unchanged
// (the frontend's own request timestamp, not necessarily the exact
// keyframe timestamp — this task's v1 doesn't re-derive it, matching the
// brief's "returning the single primary frame is fine for v1" guidance for
// Frames/fine). NoData is a pointer (not a bare bool) so its absence
// (false, the common case) doesn't have to round-trip over the wire at
// all — matching the wire contract's `noData?: boolean`.
type NvrScrubResult struct {
	Frame       []byte          `msgpack:"frame,omitempty" json:"frame,omitempty"`
	Ts          int64           `msgpack:"ts" json:"ts"`
	VideoCodec  string          `msgpack:"videoCodec" json:"videoCodec"`
	NoData      *bool           `msgpack:"noData,omitempty" json:"noData,omitempty"`
	CodecString string          `msgpack:"codecString,omitempty" json:"codecString,omitempty"`
	Width       int             `msgpack:"width,omitempty" json:"width,omitempty"`
	Height      int             `msgpack:"height,omitempty" json:"height,omitempty"`
	Frames      []NvrScrubFrame `msgpack:"frames,omitempty" json:"frames,omitempty"`
}

// NvrPlaybackReady mirrors the frontend's NvrPlaybackReady: nvrPlayback's
// (rpc_playback.go) one-time "stream is starting" callback payload, fired
// exactly once per session via the pull-callback protocol's onReady
// invocation (see rpc_playback.go's package doc comment for the pinned
// wire mechanism). SessionID is the value nvrPlaybackCmd's own sessionID
// parameter must be called back with. The Audio* fields are always left
// zero (and so omitted on the wire) in this v1: audio streaming is
// deferred — see playbackSession.run's doc comment (playback_session.go).
type NvrPlaybackReady struct {
	SessionID        string `msgpack:"sessionId" json:"sessionId"`
	VideoCodec       string `msgpack:"videoCodec" json:"videoCodec"`
	CodecString      string `msgpack:"codecString" json:"codecString"`
	Width            int    `msgpack:"width" json:"width"`
	Height           int    `msgpack:"height" json:"height"`
	AudioCodec       string `msgpack:"audioCodec,omitempty" json:"audioCodec,omitempty"`
	AudioCodecString string `msgpack:"audioCodecString,omitempty" json:"audioCodecString,omitempty"`
	AudioSampleRate  int    `msgpack:"audioSampleRate,omitempty" json:"audioSampleRate,omitempty"`
	AudioChannels    int    `msgpack:"audioChannels,omitempty" json:"audioChannels,omitempty"`
	AudioDescription []byte `msgpack:"audioDescription,omitempty" json:"audioDescription,omitempty"`
}

// NvrPlaybackVideo mirrors the frontend's NvrPlaybackVideo: one streamed
// Annex-B H.264 access unit, delivered via the pull-callback protocol's
// onVideo invocation (playbackSession.run, playback_session.go). Ts is a
// synthesized microsecond playback timestamp (see
// media.SegmentFrames.Timestamps' doc comment for why this stream carries
// no timestamps of its own to just forward).
type NvrPlaybackVideo struct {
	Frame    []byte `msgpack:"frame" json:"frame"`
	Ts       int64  `msgpack:"ts" json:"ts"`
	Keyframe bool   `msgpack:"keyframe,omitempty" json:"keyframe,omitempty"`
}

// NvrPlaybackAudio mirrors the frontend's NvrPlaybackAudio: onAudio's
// payload shape. Declared for wire-contract completeness; nothing in this
// v1 invokes onAudio (see playbackSession.run's doc comment — audio
// streaming is deferred).
type NvrPlaybackAudio struct {
	Frame []byte `msgpack:"frame" json:"frame"`
	Ts    int64  `msgpack:"ts" json:"ts"`
}

// NvrPlaybackBatchItem/NvrPlaybackBatch mirror the frontend's types of the
// same names: onBatch's payload shape, an alternative to per-frame onVideo
// delivery for bundling several frames into one callback invocation.
// Declared for wire-contract completeness; nothing in this v1 invokes
// onBatch — see playbackSession.run's doc comment on why per-frame onVideo
// (itself already the pull-callback protocol's own per-frame backpressure
// unit) was chosen instead for this first streaming implementation.
type NvrPlaybackBatchItem struct {
	Frame []byte `msgpack:"frame" json:"frame"`
	Ts    int64  `msgpack:"ts" json:"ts"`
	Audio bool   `msgpack:"audio,omitempty" json:"audio,omitempty"`
}

type NvrPlaybackBatch struct {
	Items []NvrPlaybackBatchItem `msgpack:"items" json:"items"`
}

// NvrPlaybackNoData mirrors the frontend's NvrPlaybackNoData: onNoData's
// payload, fired when nvrPlayback finds no covering segment at the
// requested Ts at all, or hits a real recording gap partway through a
// session (no next segment to roll into) — see playbackSession.run.
type NvrPlaybackNoData struct {
	Ts int64 `msgpack:"ts" json:"ts"`
}

// NvrPlaybackCommand mirrors the frontend's NvrPlaybackCommand:
// nvrPlaybackCmd's (rpc_playback.go) request payload. Speed is only
// meaningful (and only sent by the frontend) when Cmd == "speed".
type NvrPlaybackCommand struct {
	Cmd   string  `msgpack:"cmd" json:"cmd"`
	Speed float64 `msgpack:"speed,omitempty" json:"speed,omitempty"`
}

// NvrPreviewResult mirrors the frontend's NvrPreviewResult:
// NvrPreviewFrames' (rpc_playback.go) result — a filmstrip of
// evenly-spaced keyframes across a requested range plus their shared codec
// metadata (see media.PreviewResult's doc comment for why codecString/
// width/height are reported once, not per-frame).
type NvrPreviewResult struct {
	Frames      []NvrScrubFrame `msgpack:"frames" json:"frames"`
	VideoCodec  string          `msgpack:"videoCodec" json:"videoCodec"`
	CodecString string          `msgpack:"codecString,omitempty" json:"codecString,omitempty"`
	Width       int             `msgpack:"width,omitempty" json:"width,omitempty"`
	Height      int             `msgpack:"height,omitempty" json:"height,omitempty"`
	NoData      *bool           `msgpack:"noData,omitempty" json:"noData,omitempty"`
}
