package main

import (
	"fmt"
	"os"
	"sort"
	"time"

	sdk "github.com/cameraui/sdk/go"
	"github.com/google/uuid"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/recorder"
	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// GetManagedCameraIds returns the camera IDs this NVR instance is actively
// recording — i.e. p.recorder.ManagedCameraIDs(), the cameras assigned to
// this Hub plugin whose recordingMode isn't "off" (see
// recorder.RecorderManager, src/recorder/manager.go). Registered as the RPC
// method "getManagedCameraIds" — see the casing findings at the top of
// plugin.go for how the Go method name maps to that wire name.
//
// Before Task 6 this delegated to a permanently-empty stub
// (managedCameraSource/noRecorders); it now reflects the real registry kept
// current by the Hub camera lifecycle hooks in plugin.go
// (ConfigureCameras/OnCameraAdded/OnCameraReleased).
func (p *NVRPlugin) GetManagedCameraIds() ([]string, error) {
	p.logRPC("getManagedCameraIds")
	return p.recorder.ManagedCameraIDs(), nil
}

// instanceIDStore is the minimal storage interface GetInstanceId needs to
// read and persist its generated instance id. sdk.DeviceStorage satisfies
// this exactly (see GetValue/SetValue in storage.go) — p.Storage is used in
// production; tests substitute an in-memory fake.
type instanceIDStore interface {
	GetValue(key string, defaultValue ...any) any
	SetValue(key string, value any) error
}

// instanceIDStorageKey is the plugin storage key the persistent instance id
// is kept under.
const instanceIDStorageKey = "instanceId"

// GetInstanceId returns a persistent, per-plugin-install UUID. Registered as
// the RPC method "getInstanceId".
//
// Revised finding (task 2 review): the compiled frontend uses getInstanceId()
// purely as a cache-invalidation change-token — it polls the value and, when
// it CHANGES from whatever it last saw, flushes the NVR event cache. It is
// never compared against the core's own settings.instanceId, and (per the
// findings at the top of plugin.go) no SDK/core accessor for that core value
// exists to plugin authors anyway. The original implementation returned
// os.Getenv("PLUGIN_ID") — the plugin's constant package id
// (e.g. "@calebcall/camera-ui-nvr-local") — which never changes across
// restarts and therefore could never drive the intended cache flush; it was
// the wrong value for the contract this method actually serves.
//
// The correct value is a UUID generated once and persisted in this plugin's
// own DeviceStorage (p.Storage), keyed under instanceIDStorageKey: stable
// across restarts and unique per install, changing only if the plugin's
// storage is wiped — exactly the semantics the frontend's cache-invalidation
// consumer needs, achievable entirely from this plugin's own state with no
// core/SDK change required.
//
// This only actually persists because NVRPlugin.StorageSchema (plugin.go)
// declares a schema for instanceIDStorageKey with Store: true.
// sdk.DeviceStorage.SetValue silently no-ops for any key with no declared
// schema — see the "Correction" note in plugin.go's doc comment for the bug
// this caused before StorageSchema existed, and why run.go's registration
// order guarantees the schema is in place before any RPC call reaches here.
func (p *NVRPlugin) GetInstanceId() (string, error) {
	p.logRPC("getInstanceId")
	if existing, ok := p.store.GetValue(instanceIDStorageKey, "").(string); ok && existing != "" {
		return existing, nil
	}

	id := uuid.NewString()
	if err := p.store.SetValue(instanceIDStorageKey, id); err != nil {
		return "", fmt.Errorf("persist instance id: %w", err)
	}
	return id, nil
}

// GetRecordingDays returns the "YYYY-MM-DD" days cameraID has at least one
// recorded segment starting in year/month, via SegmentStore.Days.
// Registered as the RPC method "getRecordingDays".
func (p *NVRPlugin) GetRecordingDays(cameraID string, year, month int) ([]string, error) {
	p.logRPC("getRecordingDays", cameraID)
	if p.segments == nil {
		return []string{}, nil
	}
	days, err := p.segments.Days(cameraID, year, month)
	if err != nil {
		return nil, err
	}
	if days == nil {
		days = []string{}
	}
	return days, nil
}

// GetRecordingSegments returns cameraID's continuous recorded ranges
// overlapping [startMs, endMs], merged across adjacent/overlapping
// SegmentStore rows (and, when more than one stream role is configured for
// this camera, across roles too) so the frontend's timeline sees one
// RecordingSegment per continuous stretch of footage instead of one row per
// underlying ~60s segment file. Registered as the RPC method
// "getRecordingSegments".
func (p *NVRPlugin) GetRecordingSegments(cameraID string, startMs, endMs int64) ([]RecordingSegment, error) {
	p.logRPC("getRecordingSegments", cameraID)
	if p.segments == nil {
		return []RecordingSegment{}, nil
	}

	var all []store.Segment
	for _, role := range recordingRolesFor(p.recorder, cameraID) {
		segs, err := p.segments.InRange(cameraID, role, startMs, endMs)
		if err != nil {
			return nil, err
		}
		all = append(all, segs...)
	}

	return mergeSegments(cameraID, all), nil
}

// recordingRolesFor returns the stream roles cameraID is configured to
// record, per mgr's RecorderEntry.Config.Roles (see recorder/manager.go's
// readRecordingConfig) — SegmentStore.InRange is scoped to a single
// camera+role pair, so GetRecordingSegments needs to know every role that
// might have written segments for this camera to see the complete picture.
// Falls back to sdk.CameraRoleHighRes (mirroring recorder/manager.go's own
// unexported defaultRoles default) when mgr has no entry for cameraID at
// all — e.g. a camera that recorded historical footage under a Hub
// assignment it no longer has.
func recordingRolesFor(mgr *recorder.RecorderManager, cameraID string) []string {
	if mgr != nil {
		if entry, ok := mgr.Camera(cameraID); ok && len(entry.Config.Roles) > 0 {
			return entry.Config.Roles
		}
	}
	return []string{string(sdk.CameraRoleHighRes)}
}

// mergeSegments sorts segs by start time and merges every run of
// overlapping-or-touching ranges (next.StartMs <= current.EndTime) into a
// single RecordingSegment, matching the timeline's "gap-aware continuous
// ranges, not per-file rows" expectation from the task brief. A gap between
// two segments (next.StartMs > current.EndTime — a recording interruption,
// restart, or simply two different stream roles that don't line up)
// deliberately starts a new output range rather than being merged over.
func mergeSegments(cameraID string, segs []store.Segment) []RecordingSegment {
	if len(segs) == 0 {
		return []RecordingSegment{}
	}

	sorted := make([]store.Segment, len(segs))
	copy(sorted, segs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].StartMs < sorted[j].StartMs })

	merged := make([]RecordingSegment, 0, len(sorted))
	current := RecordingSegment{StartTime: sorted[0].StartMs, EndTime: sorted[0].EndMs, CameraID: cameraID}
	for _, seg := range sorted[1:] {
		if seg.StartMs <= current.EndTime {
			if seg.EndMs > current.EndTime {
				current.EndTime = seg.EndMs
			}
			continue
		}
		merged = append(merged, current)
		current = RecordingSegment{StartTime: seg.StartMs, EndTime: seg.EndMs, CameraID: cameraID}
	}
	return append(merged, current)
}

// fallbackRetentionDays is what StorageStats.RetentionDays reports when no
// camera this NVR instance knows about resolves a retention setting at all
// (an instance with no cameras configured yet). Mirrors
// recorder/manager.go's own unexported defaultRetentionDays constant (7) —
// duplicated here rather than exported from package recorder solely for
// this read-only default, matching recordingRolesFor's same tradeoff for
// sdk.CameraRoleHighRes above.
const fallbackRetentionDays = 7

// smallVolumeThresholdGB is the diskTotalGB below which GetStorageStats
// reports StorageStats.SmallVolume = true — the frontend's
// disk_small_volume warning banner (ui/src/subviews/SettingsRecordings.vue
// in the camera.ui repo) for a disk too small to usefully run an NVR on
// (e.g. a small SD card). Nothing elsewhere in this codebase already
// defines this threshold; 32 is a reasonable, clearly-documented judgment
// call, not a value derived from any existing constant.
const smallVolumeThresholdGB = 32

// diskCriticalFreePercent is the diskFreePercent below which
// GetStorageStats reports StorageStats.Paused = true — the frontend's
// disk_critical error banner. This is purely informational today: nothing
// in recorder/retention.go actually checks StorageStats or halts recording
// when the disk is this full (see this task's report for that gap) — the
// frontend just renders a banner from this flag, it doesn't drive any
// enforcement.
const diskCriticalFreePercent = 2

// GetStorageStats reports disk-level stats for this plugin's recordings
// directory (via diskStats — syscall.Statfs on linux/darwin,
// GetDiskFreeSpaceEx on windows, see diskstats_unix.go/
// diskstats_windows.go) plus this NVR instance's own recorded-usage
// breakdown, instance-wide and per camera. Registered as the RPC method
// "getStorageStats".
//
// Reports against p.recordingsDir (Feature #1's resolved recordingPath
// override, or api.StoragePath unchanged when unset) rather than
// api.StoragePath directly: once recordings are configured to live
// somewhere else (e.g. a larger external drive), THAT filesystem's
// free/used space is what actually matters to the operator here — reporting
// api.StoragePath's own (likely small, e.g. an internal boot volume) disk
// stats instead would defeat the point of moving recordings off it in the
// first place. p.recordingsDir is empty in unit tests that construct
// NVRPlugin directly rather than through NewPlugin, in which case this
// falls back to p.API.StoragePath, preserving this method's pre-existing
// behavior for those tests.
func (p *NVRPlugin) GetStorageStats() (StorageStats, error) {
	p.logRPC("getStorageStats")

	stats := StorageStats{Cameras: map[string]CameraStorageStats{}, NvrQuotaGB: p.nvrQuotaGB()}

	statsDir := p.recordingsDir
	if statsDir == "" && p.API != nil {
		statsDir = p.API.StoragePath
	}

	if statsDir != "" {
		total, free, err := diskStats(statsDir)
		if err != nil {
			if p.Logger != nil {
				p.Logger.Warn("nvr-local: getStorageStats: disk stats failed:", err)
			}
		} else {
			stats.DiskTotalGB = bytesToGB(total)
			stats.DiskFreeGB = bytesToGB(free)
			stats.DiskUsedGB = bytesToGB(total - free)
			if total > 0 {
				stats.DiskFreePercent = float64(free) / float64(total) * 100
				stats.SmallVolume = stats.DiskTotalGB < smallVolumeThresholdGB
				stats.Paused = stats.DiskFreePercent < diskCriticalFreePercent
			}
		}
	}

	cameraIDs, err := p.storageCameraIDs()
	if err != nil {
		return StorageStats{}, err
	}

	var nvrUsedBytes int64
	maxRetentionDays := 0
	for _, id := range cameraIDs {
		camStats, usedBytes, retentionDays, err := p.cameraStorageStats(id)
		if err != nil {
			return StorageStats{}, err
		}
		stats.Cameras[id] = camStats
		nvrUsedBytes += usedBytes
		if retentionDays > maxRetentionDays {
			maxRetentionDays = retentionDays
		}
	}
	stats.NvrUsedGB = bytesToGB(uint64(nvrUsedBytes))
	if maxRetentionDays > 0 {
		stats.RetentionDays = maxRetentionDays
	} else {
		stats.RetentionDays = fallbackRetentionDays
	}

	return stats, nil
}

// storageCameraIDs returns every camera GetStorageStats should build a
// CameraStorageStats entry for: the union of SegmentStore.DistinctCameraIDs
// (cameras with actual recorded segments on disk, regardless of whether
// they're still managed) and p.recorder.ManagedCameraIDs() (cameras
// currently assigned and enabled, even before their first segment lands) —
// see DistinctCameraIDs' doc comment (store/segments.go) for why segments
// alone isn't quite enough either.
func (p *NVRPlugin) storageCameraIDs() ([]string, error) {
	set := make(map[string]struct{})

	if p.segments != nil {
		ids, err := p.segments.DistinctCameraIDs()
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			set[id] = struct{}{}
		}
	}
	for _, id := range p.recorder.ManagedCameraIDs() {
		set[id] = struct{}{}
	}

	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// cameraStorageStats builds cameraID's CameraStorageStats from every
// segment SegmentStore.AllByCamera knows about for it (usedBytes summed via
// os.Stat on each segment's file path — SegmentStore itself does not track
// file size, only the path — segmentCount/oldestDay/newestDay/daysCount/
// bandwidthMBh derived from that same set), plus RecordingMode/IsRecording/
// RetentionDays from p.recorder if this camera is still registered there.
// Returns the built stats, its total usedBytes (for the caller's
// instance-wide nvrUsedGB sum), and its resolved RetentionDays (for the
// caller's instance-wide fallbackRetentionDays-or-max resolution) —
// separately from the CameraStorageStats value itself because the wire
// contract's CameraStorageStats has no RetentionDays field of its own (see
// wire.go), only StorageStats' single top-level one.
func (p *NVRPlugin) cameraStorageStats(cameraID string) (CameraStorageStats, int64, int, error) {
	mode := string(recorder.RecordingModeOff)
	isRecording := false
	retentionDays := 0
	if entry, ok := p.recorder.Camera(cameraID); ok {
		mode = string(entry.Config.Mode)
		// RecorderManager has no public accessor for whether a camera's
		// Recorder is *currently* running (m.active is private — see
		// manager.go) — only its configured Mode. Approximating IsRecording
		// as "mode isn't off" is a documented simplification, not a live
		// process check; see this task's report for the gap.
		isRecording = entry.Config.Mode != recorder.RecordingModeOff
		retentionDays = entry.Config.RetentionDays
	}

	if p.segments == nil {
		return CameraStorageStats{RecordingMode: mode, IsRecording: isRecording}, 0, retentionDays, nil
	}

	segs, err := p.segments.AllByCamera(cameraID)
	if err != nil {
		return CameraStorageStats{}, 0, 0, err
	}

	var usedBytes int64
	var totalDurationMs int64
	days := make(map[string]struct{})
	for _, seg := range segs {
		if info, statErr := os.Stat(seg.Path); statErr == nil {
			usedBytes += info.Size()
		}
		totalDurationMs += seg.EndMs - seg.StartMs
		days[dayString(seg.StartMs)] = struct{}{}
	}

	var oldestDay, newestDay string
	if len(segs) > 0 {
		// AllByCamera orders by start_ms ascending (see its own doc
		// comment, segments.go), so the first/last rows are the
		// oldest/newest by construction.
		oldestDay = dayString(segs[0].StartMs)
		newestDay = dayString(segs[len(segs)-1].StartMs)
	}

	var bandwidthMBh float64
	if totalDurationMs > 0 {
		hours := float64(totalDurationMs) / 3_600_000
		bandwidthMBh = (float64(usedBytes) / 1_000_000) / hours
	}

	return CameraStorageStats{
		UsedBytes:     usedBytes,
		SegmentCount:  len(segs),
		OldestDay:     oldestDay,
		NewestDay:     newestDay,
		DaysCount:     len(days),
		BandwidthMBh:  bandwidthMBh,
		RecordingMode: mode,
		IsRecording:   isRecording,
	}, usedBytes, retentionDays, nil
}

// dayString formats ms (a UTC Unix epoch millisecond timestamp) as
// "YYYY-MM-DD", matching SegmentStore.Days' own UTC convention
// (segments.go) for exactly the same reason: nothing elsewhere in this
// package establishes any other timezone convention for a "day".
func dayString(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02")
}

// bytesToGB converts a byte count to gigabytes (10^9 bytes, matching the
// frontend's "GB" fields — diskTotalGB and friends — which read as decimal
// GB in ui/src/subviews/SettingsRecordings.vue's storage-overview display,
// not binary GiB).
func bytesToGB(b uint64) float64 {
	return float64(b) / 1_000_000_000
}
