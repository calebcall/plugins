# Design: `camera-ui-amcrest`

**Date:** 2026-07-18
**Status:** Approved design — ready for implementation planning
**Author:** brainstorming session

## 1. Summary

A camera.ui plugin that integrates Amcrest (and Dahua-compatible) IP cameras,
doorbells, and PTZ cameras using their **native HTTP CGI API + RTSP**, rather
than generic ONVIF. It provides live streaming, snapshots, two-way audio
(talkback), motion / smart-object / audio / doorbell events, and PTZ control.

The plugin is intentionally **separate from the existing `camera-ui-onvif`
plugin**. ONVIF remains the generic fallback; this plugin exists to deliver the
Amcrest-native features ONVIF cannot deliver well: the rich `eventManager`
event stream (smart detection, doorbell talk events) and reliable talkback via
`audio.cgi`.

### Reference implementations studied

- **`camera-ui-onvif`** (this repo, TypeScript) — the closest camera.ui pattern
  for a network camera: `DiscoveryProvider`, adopt-returns-`CameraConfig`,
  per-camera event loop feeding sensors, reconnect guard.
- **`camera-ui-eufy`** (this repo, TypeScript) — the reference for streaming as a
  `CameraController`: per-camera `@seydx/rtsp` relay (`RtspServerSink`) with a
  `pcm_alaw` backchannel and a `BackchannelTranscoder` for talkback.
- **`scrypted/plugins/amcrest`** (TypeScript) — the reference for Amcrest CGI
  specifics: event codes, `audio.cgi` talkback, codec/resolution mapping,
  `magicBox.cgi` device info, digest auth. Its `dumps/*.json` are reused as test
  fixtures.

## 2. Scope

### In scope (v1)

- Device types: **standard IP cameras, Amcrest-branded doorbells, PTZ cameras**.
- Manual add (IP + credentials) **and** Dahua UDP multicast discovery.
- Live view + snapshot.
- **Two-way audio (talkback)** — Amcrest doorbell AAC path is the primary,
  hardware-tested target.
- Events → sensors: Motion, Object (person/vehicle), Audio, Doorbell.
- PTZ control.

### Out of scope (deferred to v2)

- NVR channel adoption (cameras behind an Amcrest NVR).
- Siren / floodlight control.
- Continuous-recording configuration.
- Door lock / unlock (Dahua access-control doorbells).
- ONVIF-backchannel fallback for talkback.
- Dahua-style doorbell talkback (G.711A) and `CallNoAnswered` talk events are
  **implemented best-effort but flagged untested** — no Dahua doorbell available
  for verification.

## 3. Architecture

- **Language:** TypeScript (mirrors `camera-ui-onvif` / `camera-ui-eufy`).
- **Contract:**
  - `role: PluginRole.CameraController`
  - `provides: [SensorType.Motion, SensorType.Object, SensorType.Audio, SensorType.Doorbell, SensorType.PTZ]`
  - `interfaces: [PluginInterface.DiscoveryProvider]`
- **Streaming model:** `CameraController` (like eufy), because two-way audio
  requires the plugin to **host an RTSP endpoint that carries a backchannel**.
  A raw Amcrest RTSP URL handed directly to camera.ui cannot carry talkback, so
  the lean "URLs-only" ONVIF shape is insufficient for the v1 requirement.

### Module structure

```
camera-ui-amcrest/
  contract.ts
  cameraui.config.ts
  package.json
  tsconfig.json
  eslint.config.js
  logo.png
  README.md
  CHANGELOG.md
  updates.config.js
  src/
    index.ts            # plugin: discovery, onGetCameraSettings, onAdoptCamera, lifecycle
    camera.ts           # per-camera controller: relay, streaming/snapshot impl, sensor wiring
    amcrest/
      digest-auth.ts    # HTTP Digest fetch helper (Amcrest requires digest auth)
      api.ts            # AmcrestClient: getSystemInfo, snapshot, getCodecs, ptz, event attach, audio POST
      events.ts         # parse eventManager multipart stream -> typed AmcrestEvent objects
      discovery.ts      # Dahua UDP multicast discovery (239.255.255.251:37810)
      talkback.ts       # backchannel Writable -> chunked audio.cgi POST
    sensors/
      index.ts
      motion.ts
      object.ts
      audio.ts
      doorbell.ts
      ptz.ts
```

### Key dependencies

- `@camera.ui/sdk`, `@camera.ui/cli` (dev) — same versions as sibling plugins.
- `@seydx/rtsp` — `Relay` / `RtspServerSink` / `BackchannelTranscoder` for the
  streaming relay and talkback transcode (already used by `camera-ui-eufy`).
- No ONVIF dependency (pure Amcrest CGI + RTSP).

## 4. Components

### 4.1 `digest-auth.ts`
Small HTTP Digest client (Amcrest CGI requires digest auth). Computes the
`Authorization` header from a `WWW-Authenticate` challenge (MD5, `qop=auth`,
nonce/cnonce/nc). Supports both buffered responses (device info, snapshot, PTZ)
and streaming responses (event attach, audio POST). Unit-testable header
computation.

### 4.2 `api.ts` — `AmcrestClient`
Thin wrapper over the CGI endpoints, keyed by `ip` + credentials:
- `getSystemInfo()` → `magicBox.cgi?action=getSystemInfo` (deviceType,
  hardwareVersion, serialNumber; also used to confirm "is Amcrest/Dahua").
- `snapshot(channel)` → `snapshot.cgi?channel=N` (returns image buffer).
- `getCodecs(channel)` → parse `configManager.cgi?action=getConfig&name=Encode`
  to enumerate main/extra streams and their codec/resolution.
- `ptzStart/ptzStop(channel, code, args)` → `ptz.cgi`.
- `attachEvents()` → opens the long-lived `eventManager.cgi?action=attach&codes=[All]`
  multipart stream (returns a readable + destroy handle).
- `postAudio(channel, contentType, body)` → chunked POST to
  `audio.cgi?action=postAudio&httptype=singlepart&channel=N`.

### 4.3 `events.ts`
Parses the `eventManager` multipart stream into typed events. Recognized codes
(from the scrypted reference):
- `VideoMotion` (Start/Stop) → Motion
- `SmartMotionHuman` / `CrossLineDetection` / `CrossRegionDetection` (with
  `Object.ObjectType` = `Human` | `Vehicle`) → Object (person / vehicle)
- `AudioMutation` (Start/Stop) → Audio
- `_DoTalkAction_` (Invite/Hangup/Pulse) → Doorbell (Amcrest, **tested**)
- `CallNoAnswered` / `PassiveHungup` → Doorbell (Dahua, **untested, best-effort**)

Handles known device quirks: `-- boundary` vs `--boundary`, stray
`HTTP/1.x 200 OK` framing, and body-without-newline separation.

### 4.4 `discovery.ts`
Dahua/Amcrest UDP multicast discovery on `239.255.255.251:37810`: sends the
discovery probe, parses responses (IP, MAC, deviceType/model), and yields
`DiscoveredCamera` entries. All socket errors/timeouts are non-fatal.

### 4.5 `talkback.ts`
A `Writable` sink that streams transcoded audio to `audio.cgi`. Target codec is
device-type driven:
- **Amcrest doorbell → AAC** (`Audio/AAC`, adts) — primary, hardware-tested;
  matches eufy's `BackchannelTranscoder` target.
- **Dahua doorbell / other → G.711A** (`Audio/G.711A`, pcm_alaw 8kHz) —
  best-effort, untested.
camera.ui advertises a `pcm_alaw` backchannel; incoming RTP is transcoded to the
target and streamed as the chunked POST body.

### 4.6 `camera.ts` — per-camera controller
Owns the runtime for one adopted camera:
- Starts a `@seydx/rtsp` relay fronting the Amcrest RTSP
  (`rtsp://<ip>/cam/realmonitor?channel=N&subtype=0|1`) and advertising the
  `pcm_alaw` backchannel.
- Implements `StreamingInterface.streamUrl()` (returns relay URL) and
  `SnapshotInterface.snapshot()` (via `AmcrestClient.snapshot`).
- Opens one `attachEvents()` stream and routes parsed events to the sensors.
- Adds only the sensors the device supports (PTZ only if detected, Doorbell only
  for doorbell models).
- Handles reconnect (backoff), talkback lifecycle, and teardown.

### 4.7 `sensors/*`
Thin subclasses of the SDK sensor base classes
(`MotionSensor`, object, audio, `DoorbellTrigger`, PTZ), following the
`camera-ui-onvif` sensor pattern. The PTZ sensor translates PTZ commands into
`AmcrestClient.ptzStart/Stop` calls.

### 4.8 `index.ts` — plugin entry
- `onDiscoverCameras()` → runs Dahua discovery, returns `DiscoveredCamera[]`.
- `onGetCameraSettings()` → IP / username / password fields for manual add.
- `onAdoptCamera()` → connect, `getSystemInfo`, enumerate streams via
  `getCodecs`, detect PTZ + doorbell capability, build and return `CameraConfig`.
- `configureCameras` / `onCameraAdded` / `onCameraReleased` → lifecycle,
  create/destroy per-camera controllers.
- Subscribes to `API_EVENT.SHUTDOWN` for clean teardown.

## 5. Data flow

1. **Discovery / add:** Dahua multicast or manual entry → `onAdoptCamera`
   connects, reads device info, enumerates streams, detects capabilities →
   returns `CameraConfig`.
2. **Streaming:** camera.ui connects to the per-camera relay URL; relay remuxes
   the Amcrest RTSP (no video transcode) and exposes the backchannel.
3. **Snapshot:** `snapshot.cgi` fetched on demand.
4. **Events:** one `eventManager` attach stream per camera → parsed → dispatched
   to Motion / Object / Audio / Doorbell sensors.
5. **PTZ:** PTZ sensor command → `ptz.cgi`.
6. **Talkback:** backchannel RTP (`pcm_alaw`) → `BackchannelTranscoder` →
   target codec → chunked POST to `audio.cgi`.

## 6. Error handling

- **Digest auth failure (401):** surface `logger.attention('Check Amcrest
  credentials')`; do not crash.
- **Event stream:** auto-reconnect with backoff (mirrors the ONVIF plugin's
  reconnect guard); dedup repeated error log lines.
- **Relay / talkback:** lifecycle bound to the camera; teardown on release /
  shutdown; reset transcoder on stream stop (eufy pattern).
- **Discovery:** UDP socket errors and timeouts caught and logged, never fatal.
- **Capability gating:** never add a sensor the device does not support.

## 7. Testing

### 7.1 Unit tests (fast regression, no hardware)
- `events.ts` parser against `scrypted/plugins/amcrest/dumps/*.json`
  (`amcrest-face-detected.json`, `amcrest-human-detected.json`) plus synthetic
  motion / audio / talk lines and the known framing quirks.
- `digest-auth.ts` header computation against a fixed challenge/nonce.
- `discovery.ts` probe encode + response parse against a captured packet.
- RTSP URL builder (channel/subtype).
- Codec / resolution mapping.

### 7.2 Real-hardware verification (primary, per feature)
Verified on the user's actual devices (standard IP camera, Amcrest doorbell, PTZ
camera) before the corresponding work item is marked done:
- Live view (main + sub stream)
- Snapshot
- Motion event
- Human / vehicle detection
- Doorbell press (`_DoTalkAction_`)
- Talkback (AAC path, Amcrest doorbell)
- PTZ control

## 8. Open risks

- **Talkback framing:** `audio.cgi` chunk sizing for the Amcrest doorbell may
  need tuning (scrypted noted ~1024-byte chunks for Dahua). Validate on
  hardware.
- **Dahua discovery packet format:** exact probe/response bytes to be confirmed
  against a captured exchange; keep discovery non-fatal so manual add always
  works.
- **PTZ capability detection:** confirm the detection method (config vs
  `ptz.cgi` status) works across the user's PTZ model.
