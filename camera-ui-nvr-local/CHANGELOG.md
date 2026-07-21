# Changelog

All notable changes to **NVR (Local)** are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-07-21

Initial release — an open-source, fully-local, drop-in replacement for the closed-source
`@camera.ui/camera-ui-nvr` backend, which stops working offline due to a license check. Implements
the existing camera.ui frontend contract, so the unmodified web/mobile UI drives it directly.

### Added

- **Recording** — continuous per-camera H.264 segment recording via core-provided ffmpeg, with a
  supervised recorder manager (per-camera locking to prevent handle leaks) and event-aware protected
  windows.
- **Storage & read path** — pure-Go, cgo-free SQLite store for events and segments; queryable
  recording days/segments (merged) per camera; brute-force cosine vector store for future semantic
  search.
- **Playback & scrub** — frame-accurate Annex-B (in-band SPS/PPS) streaming playback and keyframe
  scrubbing/preview-frame extraction; codec string derived from the SPS.
- **Detection events** — full DetectionEvent lifecycle ingestion, with object-detection data merged
  across `start`/`update`/`segment-*`/`end` messages so sparse messages never clobber earlier data;
  events linked to their recordings.
- **Thumbnails** — on-disk thumbnail generation for detection events (home "recent detections",
  camera view, and the recordings list).
- **Configurable storage** — `recordingPath` (write recordings to any volume) and `nvrQuotaGB`
  (instance-wide disk cap) exposed on Settings → Recordings; retention evicts the oldest segments
  when usage exceeds the cap, and sweeps untracked orphan files.
- **Subscriptions** — `onRecordingState` and `onSystemEvent` callback subscriptions.
- **In-app notifications** — publishes object-detection events to the camera.ui notification center
  (`PublishNotifications` capability).
- **Local "License & Cloud" panel** — a fixed, always-connected-locally OAuth state so the settings
  page renders normally instead of "Not connected"; no real authentication flow.

### Fixed

- **Disk retention** — enforce the `nvrQuotaGB` cap reliably: run retention immediately at startup
  and every 10 minutes (previously the cap could be exceeded for long stretches). Added an orphan
  sweep that rechecks the database per-file immediately before deleting and skips any file a running
  recorder is actively writing, so a concurrently-finalized recording can never be deleted.
- **Detection labels** — derive the event's primary label from the highest-score detection (falling
  back to the first non-motion/audio/clip type), so events show `person`/`vehicle`/`animal` instead
  of `clip` or `motion`.
- **Recordings label filter** — filtering the recordings list by a label (e.g. "person") now returns
  the matching object events. The frontend sends `hasDetections:false` alongside an explicit
  `types:[...]` chip to mean "no detections-only constraint"; treating it as strict equality
  previously excluded every object event whenever a chip was selected.
- **`hasDetections` semantics** — the detections-only view keys off event *types* (an object type is
  present) rather than segment presence, which the core delivers empty.

### Changed

- **Notifications** — the plugin is a pure notification *publisher*; the unimplemented `Notifier`
  interface was removed from the contract. Declaring it without implementing the device methods
  caused repeated `getDevices` "no responders" warnings on the host and broke the mobile app's device
  registration. Background push to the camera.ui app requires camera.ui's proprietary cloud relay and
  is intentionally out of scope for a local plugin.

[0.1.0]: https://github.com/calebcall/plugins/tree/nvr-plugin/camera-ui-nvr-local
