// recording_path.go implements the configurable recording storage path
// setting (StorageSchema's recordingPathStorageKey, plugin.go): resolving
// which base directory NEW recordings (RecorderConfig.DataDir, recorder/
// manager.go) and the thumbnail Generator (media.NewGenerator) are rooted
// under, once at startup (NewPlugin).
//
// Segment paths are stored ABSOLUTE in SegmentStore (see recorder.go's
// outDir/finalizeSegment): changing this setting only affects where FUTURE
// recordings land, never rewrites or moves anything already recorded, so
// existing reads/playback/scrub keep working unchanged across a path
// change — the frontend contract's SettingsRecordings.vue renders this
// field purely as a plugin-storage config value, with no migration step of
// its own.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	sdk "github.com/cameraui/sdk/go"
)

// recordingPathStorageKey is the plugin-level (instance-wide, not
// per-camera — matching nvrQuotaGBStorageKey's own reasoning) storage key
// holding the optional custom recording storage path. Declared via
// StorageSchema (plugin.go) so the settings page renders it as part of the
// plugin's config form (SettingsRecordings.vue: CuiSchema from
// usePluginStorage) and edits persist across restarts.
const recordingPathStorageKey = "recordingPath"

// recordingsWriteProbeFile is the throwaway file ensureWritableDir
// creates-then-removes inside a candidate directory to prove it's actually
// writable, not just present. Namespaced so it never collides with a real
// recording/thumbnail file.
const recordingsWriteProbeFile = ".nvr-local-write-test"

// resolveRecordingsBaseDir decides the base directory NEW recordings
// (RecorderConfig.DataDir) and the thumbnail Generator are rooted under:
// configured (the recordingPathStorageKey value read from this plugin's own
// storage) when non-empty and usable, or defaultDir (api.StoragePath — this
// plugin's own default storage directory, the pre-existing behavior)
// otherwise.
//
// "Usable" means ensureWritableDir succeeds: the directory exists (created
// if missing, including any missing parents) and a probe file can actually
// be written to it. A configured path that fails either check (permission
// error, path is actually a file, a parent doesn't exist and can't be
// created, ...) logs a clear error — so an operator sees why their setting
// was ignored, rather than recordings silently going to the wrong place —
// and falls back to defaultDir rather than propagating the error into
// NewPlugin (which has no error return to propagate it through — see
// NewPlugin's own doc comment on why a failure here must be logged, not
// fatal) or leaving RecorderConfig.DataDir pointed at a directory nothing
// can actually write into.
//
// log may be nil (as in unit tests) — every log call below guards for that.
func resolveRecordingsBaseDir(defaultDir, configured string, log *sdk.Logger) string {
	if configured == "" {
		return defaultDir
	}

	if err := ensureWritableDir(configured); err != nil {
		if log != nil {
			log.Error(fmt.Sprintf("nvr-local: configured recordingPath %q is not usable (%v); falling back to the default storage path %q", configured, err, defaultDir))
		}
		return defaultDir
	}

	return configured
}

// ensureWritableDir creates dir (and any missing parents, mirroring
// os.MkdirAll's own semantics) if it doesn't already exist, then proves it
// is actually writable by creating and removing a small probe file inside
// it. MkdirAll alone would silently report success against an
// already-existing but read-only (or otherwise unwritable — a mounted
// read-only volume, restrictive permissions, ...) directory, which is
// exactly the case resolveRecordingsBaseDir needs to detect and fall back
// from rather than only discovering it later, mid-recording, inside a
// recorder goroutine with nowhere better to report the failure than a log
// line no one may be watching.
func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	probe := filepath.Join(dir, recordingsWriteProbeFile)
	f, err := os.Create(probe)
	if err != nil {
		return fmt.Errorf("directory is not writable: %w", err)
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return nil
}
