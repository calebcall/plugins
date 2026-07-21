<h1 align="center">NVR (Local)</h1>

<p align="center">
  An open-source, fully-local NVR hub for <a href="https://github.com/seydx/camera.ui">camera.ui</a> —
  records, stores, and plays back your camera video entirely on your own hardware, with no cloud
  dependency and no license check.
</p>

---

## Why this exists

camera.ui's official NVR backend (`@camera.ui/camera-ui-nvr`) is closed-source and performs an
online **license check**. When the host can't reach the internet, the NVR stops working —
recording, timeline, and playback all go dark.

This plugin is a **drop-in, open-source replacement** for that backend. It implements the same
frontend contract, so the existing (unmodified) camera.ui web/mobile UI talks to it directly, but
everything runs locally:

- No license check — it never phones home.
- No cloud dependency for recording, retention, timeline, scrubbing, or playback.
- Paired detection plugins (e.g. an object/face detector that calls an LLM) may still use the
  internet if *they* need it — that's their choice. The NVR itself stays local.

## Features

- **Continuous recording** — per-camera H.264 segments written to disk via core-provided ffmpeg.
- **Timeline + read path** — recording days/segments, merged and queryable per camera.
- **Streaming playback & scrub** — frame-accurate Annex-B playback and keyframe scrubbing.
- **Detection events** — object/motion/audio detection events ingested, merged across the event
  lifecycle, and linked to their recordings; thumbnails generated on disk.
- **Configurable storage** — point recordings at any volume (`recordingPath`) and cap total disk
  usage (`nvrQuotaGB`); retention evicts the oldest segments when over the cap.
- **In-app notifications** — publishes object-detection events to the camera.ui notification center.
- **Local "License & Cloud" panel** — reports a fixed *connected-locally* state so the settings
  page renders normally instead of "Not connected".

### Notifications: what's local and what isn't

This plugin is a notification **publisher** — object-detection events appear in the camera.ui
notification center while the app is connected. It is **not** a device-owning notifier.

True **background push** to the camera.ui mobile app is *not* possible locally: that path runs
through camera.ui's own proprietary FCM/APNs cloud relay, which we neither have credentials for nor
can replicate. If you want real background push without that cloud, the local option is a
self-hosted push service (e.g. [ntfy](https://ntfy.sh) or [Gotify](https://gotify.net/)) delivered
to *that* app — not implemented here yet.

## Important: the package name

> **The package name in `package.json` is deliberately `@camera.ui/camera-ui-nvr` — the same id as
> the closed plugin.**

The camera.ui frontend does **not** discover the NVR backend by interface. It **hardcodes the
package id `@camera.ui/camera-ui-nvr`** as its RPC target. A differently-named plugin will load and
even record, but the UI will never send it the events/recordings/playback calls. So this plugin must
be **installed into the `@camera.ui/camera-ui-nvr` slot**, replacing the closed backend.

Because that namespace is not ours, **this plugin is not published to npm.** You build it and deploy
it to your server yourself — see below.

## Tech stack

- **Go** (chosen for high-concurrency recording across many cameras).
- **ffmpeg** resolved from the camera.ui core (`CoreManager.GetFFmpegPath()`); no separate binary or
  ffprobe required.
- **SQLite** via the pure-Go, cgo-free [`ncruces/go-sqlite3`](https://github.com/ncruces/go-sqlite3)
  (WASM) driver, with a brute-force cosine vector store for future semantic search.

## Prerequisites

- **Go 1.26+** (to build the plugin binary).
- **Node.js 22+** and the plugin's dev dependencies (`npm install`) to produce the `contract.cjs`
  bundle via the camera.ui CLI.
- A running camera.ui instance you control (the host where the plugin gets installed).

## Build & deploy locally

camera.ui loads the **built artifact**, not source — a `git pull` alone does nothing. You must
build the bundle (`contract.cjs` + the platform binary) and copy it into the install slot.

The install slot is:

```
<camera.ui-install>/plugins/@camera.ui/camera-ui-nvr/
```

and the plugin's recordings/database live under
`<camera.ui-install>/volume/plugins/storage/@camera.ui/camera-ui-nvr/` — reinstalling the code does
not touch them.

### Option A — build natively on the server (preferred)

If Go 1.26+ and Node are available on the host, build there and skip the cross-compile round-trip:

```bash
# 1. Get the latest source
cd ~/path/to/plugins && git pull
cd camera-ui-nvr-local
npm install                 # first time only — pulls the camera.ui CLI bundler

# 2. Build the bundle (produces bundle/{contract.cjs, package.json, dist/bin/plugin})
npm run bundle:dev

# 3. Install into the @camera.ui/camera-ui-nvr slot
D=<camera.ui-install>/plugins/@camera.ui/camera-ui-nvr
rm -rf "$D"/* && cp -a bundle/. "$D/"

# 4. First time only: enable it — remove the "@camera.ui/camera-ui-nvr" line
#    from disabledPlugins in <camera.ui-install>/volume/camera.ui.yaml

# 5. Restart camera.ui
systemctl restart cameraui   # or however your instance is managed
```

### Option B — cross-compile from a workstation

Build a statically-linked Linux binary locally, then ship the bundle to the server:

```bash
cd camera-ui-nvr-local

# 1. Produce contract.cjs (+ package.json) under bundle/
npm install
npm run bundle:dev

# 2. Cross-compile the Linux binary into the bundle's dev path
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -ldflags "-s -w" -o bundle/dist/bin/plugin ./src/
chmod 755 bundle/dist/bin/plugin

# 3. Ship it (strip macOS junk so the server dir stays clean)
COPYFILE_DISABLE=1 tar czf /tmp/nvr-install.tgz -C bundle .
scp /tmp/nvr-install.tgz root@YOUR_SERVER:/tmp/

# 4. On the server: install into the slot, enable (first time), restart
ssh root@YOUR_SERVER '
  D=<camera.ui-install>/plugins/@camera.ui/camera-ui-nvr
  rm -rf "$D"/* && tar xzf /tmp/nvr-install.tgz -C "$D"
  systemctl restart cameraui
'
```

Set `GOARCH=arm64` for ARM hosts. The binary is resolved from the dev path
`<slot>/dist/bin/plugin` before any platform `node_modules` package, so no npm publish is involved.

### Verify it loaded and is being used

Tail the camera.ui log after restart:

```bash
grep -iE "NVR|Spawning Go|nvr-local: rpc" <camera.ui-install>/volume/camera.ui.log | tail
```

You want to see the plugin spawn (`Spawning Go plugin ... dist/bin/plugin`) **and** the frontend
actually routing to it — e.g. `nvr-local: rpc getManagedCameraIds` / `getInstanceId` /
`getCameraEvents`. If you only see it spawn but never any `rpc` lines, the install slot/name is
wrong (see [Important: the package name](#important-the-package-name)).

## Configuration

Configured from the camera.ui **Settings → Recordings** page:

| Setting         | Meaning                                                                                  |
| --------------- | ---------------------------------------------------------------------------------------- |
| `recordingPath` | Directory (ideally on a large/dedicated volume) where recordings are written.            |
| `nvrQuotaGB`    | Instance-wide disk cap. Retention evicts the oldest segments once usage exceeds the cap. |

> **Set a quota.** With no cap, continuous recording of several high-resolution cameras can fill a
> disk in hours. Point `recordingPath` at a roomy volume and set `nvrQuotaGB` to a safe ceiling.

## Development

```bash
go test ./src/...                 # full suite
go test ./src/... -race -count=2  # race detector
```

## License

[MIT](./LICENSE.md).

The `logo.png` is reused from the official NVR plugin for visual continuity in the plugin list; all
plugin code here is original and independently implemented.
