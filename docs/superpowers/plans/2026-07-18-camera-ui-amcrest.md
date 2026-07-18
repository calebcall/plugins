# camera-ui-amcrest Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a camera.ui plugin that integrates Amcrest / Dahua-compatible IP cameras, doorbells, and PTZ cameras using their native HTTP CGI API + RTSP, with live streaming, snapshots, two-way audio, motion/object/audio/doorbell events, PTZ, and Dahua UDP discovery.

**Architecture:** A TypeScript `CameraController` plugin (mirrors `camera-ui-eufy` / `camera-ui-onvif`). Pure, dependency-free logic (URL building, digest auth, CGI/event parsing, discovery packet coding, codec selection) is factored into small unit-tested modules under `src/amcrest/`. A per-camera controller (`camera.ts`) hosts a `@seydx/rtsp` relay that fronts the camera's RTSP and carries a `pcm_alaw` backchannel for talkback, opens the `eventManager` event stream, and wires typed events to SDK sensors. The plugin entry (`index.ts`) implements discovery, manual-add settings, adopt, and lifecycle.

**Tech Stack:** TypeScript 5.9 (ESM, NodeNext, `verbatimModuleSyntax`), Node ≥22, `@camera.ui/sdk`, `@camera.ui/cli`, `@seydx/rtsp`, Node built-in `node:test` runner via `tsx`, `eslint` + `prettier` (sibling-plugin config).

## Global Constraints

- Node engine: `>=22.0.0`; camera.ui engine: `>=2.0.12` (copy from `camera-ui-onvif`).
- Package name: `@camera.ui/camera-ui-amcrest`; `displayName: "Amcrest"`; `type: "module"`; `main: "./bundle/dist/index.js"`.
- TypeScript config mirrors `camera-ui-eufy/tsconfig.json` (ES2022 target, NodeNext, `strict`, `verbatimModuleSyntax: true`, `useUnknownInCatchVariables: false`).
- `verbatimModuleSyntax` is ON — all type-only imports MUST use `import type`.
- ESM only — all relative imports MUST use the `.js` extension (e.g. `import { x } from './amcrest/api.js'`).
- Author string: `"seydx (https://github.com/cameraui/plugins)"` (match siblings).
- License: MIT.
- Contract: `role: PluginRole.CameraController`; `provides: [SensorType.Motion, SensorType.Object, SensorType.Audio, SensorType.Doorbell, SensorType.PTZ]`; `interfaces: [PluginInterface.DiscoveryProvider]`.
- Two-way audio: Amcrest doorbell → AAC (`Audio/AAC`, adts) is the tested path; Dahua doorbell → G.711A (`Audio/G.711A`) is best-effort/untested.
- Test command (whole suite): `npm test` → `node --import tsx --test "src/**/*.test.ts"`. Single file: `node --import tsx --test <path>`.
- Never re-encode video in the relay; only transcode the talkback audio.

## File Structure

```
camera-ui-amcrest/
  contract.ts              # PluginContract
  cameraui.config.ts       # cui bundle config
  package.json
  tsconfig.json
  eslint.config.js
  .prettierrc.json         # (inherit repo root if present; else copy sibling)
  updates.config.js
  logo.png
  README.md
  CHANGELOG.md
  src/
    index.ts               # plugin entry: discovery, settings, adopt, lifecycle
    camera.ts              # per-camera controller: relay, streaming/snapshot, event loop, sensors
    types.ts               # shared TS types (storage values, parsed structs)
    amcrest/
      rtsp-url.ts          # buildRtspUrl()            [unit-tested]
      digest-auth.ts       # buildDigestAuthHeader() + digestFetch()   [header unit-tested]
      system-info.ts       # parseKeyValueBody(), parseSystemInfo()    [unit-tested]
      encode-config.ts     # parseEncodeConfig()       [unit-tested]
      events.ts            # parseAmcrestEvent(), AmcrestEventReader    [unit-tested]
      classify.ts          # classifyAmcrestEvent()    [unit-tested]
      discovery.ts         # buildDiscoveryProbe(), parseDiscoveryResponse(), discover()  [coding unit-tested]
      ptz-commands.ts      # ptzCommandForVelocity()   [unit-tested]
      talkback.ts          # selectTalkbackTarget() [unit-tested] + AmcrestTalkback stream
      api.ts               # AmcrestClient (composes the above over digestFetch)
    sensors/
      index.ts
      motion.ts
      object.ts
      audio.ts
      doorbell.ts
      ptz.ts
    fixtures/
      human-detected.json  # copied from scrypted dumps
      face-detected.json   # copied from scrypted dumps
```

---

### Task 1: Scaffold plugin package + test tooling

**Files:**
- Create: `camera-ui-amcrest/package.json`, `tsconfig.json`, `eslint.config.js`, `cameraui.config.ts`, `contract.ts`, `updates.config.js`, `src/index.ts`, `src/types.ts`
- Create (smoke test): `camera-ui-amcrest/src/smoke.test.ts`

**Interfaces:**
- Produces: a compiling, lint-clean, empty plugin skeleton; `npm test`, `npm run build`, `npm run lint` all runnable.

- [ ] **Step 1: Create `package.json`**

```json
{
  "displayName": "Amcrest",
  "name": "@camera.ui/camera-ui-amcrest",
  "version": "0.0.1",
  "description": "Integrates Amcrest and Dahua-compatible IP cameras, doorbells and PTZ cameras into camera.ui via their native CGI API, with discovery, live streaming, two-way audio, PTZ, and motion, object, audio and doorbell events.",
  "author": "seydx (https://github.com/cameraui/plugins)",
  "type": "module",
  "main": "./bundle/dist/index.js",
  "scripts": {
    "build": "rimraf dist && tsc",
    "bundle": "npm run format && npm run lint && npm run test && npm run build && cui bundle",
    "bundle:dev": "npm run build && cross-env MODE=development cui bundle",
    "format": "prettier --write \"src/\" --ignore-unknown --no-error-on-unmatched-pattern",
    "install-updates": "npm i --save --force",
    "lint": "eslint --fix .",
    "test": "node --import tsx --test \"src/**/*.test.ts\"",
    "prepublishOnly": "node -e \"if(!process.env.SAFE_PUBLISH){console.error('Error: Please use @camera.ui/cli to publish the plugin');process.exit(1)}\"",
    "publish:alpha": "npm i --save --force && npm run bundle && cui publish --alpha",
    "publish:beta": "npm i --save --force && npm run bundle && cui publish --beta",
    "publish:latest": "npm i --save --force && npm run bundle && cui publish --latest",
    "update": "updates --update ./"
  },
  "dependencies": {
    "@seydx/rtsp": "^1.0.5"
  },
  "devDependencies": {
    "@camera.ui/cli": "^0.0.65",
    "@camera.ui/sdk": "^0.0.22",
    "@stylistic/eslint-plugin": "^5.10.0",
    "@types/node": "26.1.1",
    "@typescript-eslint/parser": "^8.64.0",
    "cross-env": "^10.1.0",
    "eslint": "9.39.2",
    "globals": "^17.7.0",
    "prettier": "^3.9.5",
    "rimraf": "^6.1.3",
    "tsx": "^4.23.1",
    "typescript": "5.9.3",
    "typescript-eslint": "^8.64.0",
    "updates": "^17.19.1"
  },
  "engines": {
    "camera.ui": ">=2.0.12",
    "node": ">=22.0.0"
  },
  "os": [],
  "cpu": [],
  "license": "MIT",
  "keywords": ["camera-ui-plugin", "amcrest", "dahua", "doorbell", "ptz", "discovery"],
  "bugs": { "url": "https://github.com/cameraui/plugins/issues" },
  "homepage": "https://github.com/cameraui/plugins/tree/main/camera-ui-amcrest#readme",
  "repository": { "type": "git", "url": "git+https://github.com/cameraui/plugins.git", "directory": "camera-ui-amcrest" }
}
```

- [ ] **Step 2: Create `tsconfig.json`** (copy of `camera-ui-eufy/tsconfig.json`)

```json
{
  "compilerOptions": {
    "outDir": "./dist",
    "target": "ES2022",
    "moduleResolution": "NodeNext",
    "module": "NodeNext",
    "lib": ["ES2022"],
    "declaration": true,
    "sourceMap": true,
    "strict": true,
    "esModuleInterop": true,
    "preserveConstEnums": true,
    "skipLibCheck": true,
    "useUnknownInCatchVariables": false,
    "allowSyntheticDefaultImports": true,
    "resolveJsonModule": true,
    "experimentalDecorators": true,
    "emitDecoratorMetadata": true,
    "verbatimModuleSyntax": true,
    "noImplicitAny": true
  },
  "include": ["src"],
  "exclude": ["node_modules", "dist"]
}
```

- [ ] **Step 3: Create `eslint.config.js`, `cameraui.config.ts`, `updates.config.js`**

`eslint.config.js` — copy `camera-ui-eufy/eslint.config.js` verbatim, then add `'**/*.test.ts'` to the `ignores` array so tests are not type-checked by lint.

`cameraui.config.ts`:
```ts
import type { CameraUiBuildOptions } from '@camera.ui/cli';

const mode = process.env.MODE || 'production';

const config: CameraUiBuildOptions = {
  input: ['src/index.ts'],
  mode: mode === 'development' ? 'development' : 'production',
  external: [],
  additionalFiles: [],
};

export default config;
```

`updates.config.js`:
```js
export default {
  exclude: ['typescript', 'eslint', '@seydx/rtsp'],
};
```

- [ ] **Step 4: Create `contract.ts`**

```ts
import { PluginInterface, PluginRole, SensorType } from '@camera.ui/sdk';

import type { PluginContract } from '@camera.ui/sdk';

export const contract: PluginContract = {
  name: 'Amcrest',
  role: PluginRole.CameraController,
  provides: [SensorType.Motion, SensorType.Object, SensorType.Audio, SensorType.Doorbell, SensorType.PTZ],
  consumes: [],
  interfaces: [PluginInterface.DiscoveryProvider],
};

export default contract;
```

- [ ] **Step 5: Create `src/types.ts` (stub) and `src/index.ts` (stub)**

`src/types.ts`:
```ts
export interface AmcrestCameraStorage {
  ip: string;
  username: string;
  password: string;
  port?: number;
  channel?: number;
}
```

`src/index.ts`:
```ts
import { BasePlugin } from '@camera.ui/sdk';

export default class AmcrestPlugin extends BasePlugin {}
```

- [ ] **Step 6: Create the smoke test `src/smoke.test.ts`**

```ts
import assert from 'node:assert/strict';
import { test } from 'node:test';

test('test runner works', () => {
  assert.equal(1 + 1, 2);
});
```

- [ ] **Step 7: Install and verify tooling**

Run: `cd camera-ui-amcrest && npm install`
Then run: `npm test`
Expected: `smoke.test.ts` passes (1 test, 0 fail).
Then run: `npm run build`
Expected: `tsc` completes with no errors (dist/ produced).
Then run: `npm run lint`
Expected: eslint completes with no errors.

- [ ] **Step 8: Commit**

```bash
git add camera-ui-amcrest
git commit -m "feat(amcrest): scaffold plugin package and test tooling"
```

---

### Task 2: RTSP URL builder

**Files:**
- Create: `camera-ui-amcrest/src/amcrest/rtsp-url.ts`
- Test: `camera-ui-amcrest/src/amcrest/rtsp-url.test.ts`

**Interfaces:**
- Produces: `buildRtspUrl(opts: RtspUrlOptions): string` where
  `RtspUrlOptions = { ip: string; username: string; password: string; port?: number; channel?: number; subtype: number }`.
  Subtype `0` = main stream, `1` = sub stream.

- [ ] **Step 1: Write the failing test**

```ts
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { buildRtspUrl } from './rtsp-url.js';

test('builds main stream url with defaults', () => {
  const url = buildRtspUrl({ ip: '192.168.1.50', username: 'admin', password: 'pw', subtype: 0 });
  assert.equal(url, 'rtsp://admin:pw@192.168.1.50:554/cam/realmonitor?channel=1&subtype=0');
});

test('builds sub stream url with custom port and channel', () => {
  const url = buildRtspUrl({ ip: '10.0.0.9', username: 'admin', password: 'pw', port: 5544, channel: 2, subtype: 1 });
  assert.equal(url, 'rtsp://admin:pw@10.0.0.9:5544/cam/realmonitor?channel=2&subtype=1');
});

test('url-encodes credentials with special characters', () => {
  const url = buildRtspUrl({ ip: '192.168.1.50', username: 'ad@min', password: 'p:w/d', subtype: 0 });
  assert.equal(url, 'rtsp://ad%40min:p%3Aw%2Fd@192.168.1.50:554/cam/realmonitor?channel=1&subtype=0');
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `node --import tsx --test src/amcrest/rtsp-url.test.ts`
Expected: FAIL — cannot find module `./rtsp-url.js`.

- [ ] **Step 3: Write the implementation**

```ts
export interface RtspUrlOptions {
  ip: string;
  username: string;
  password: string;
  port?: number;
  channel?: number;
  subtype: number;
}

export function buildRtspUrl(opts: RtspUrlOptions): string {
  const port = opts.port ?? 554;
  const channel = opts.channel ?? 1;
  const user = encodeURIComponent(opts.username);
  const pass = encodeURIComponent(opts.password);
  return `rtsp://${user}:${pass}@${opts.ip}:${port}/cam/realmonitor?channel=${channel}&subtype=${opts.subtype}`;
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `node --import tsx --test src/amcrest/rtsp-url.test.ts`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add camera-ui-amcrest/src/amcrest/rtsp-url.ts camera-ui-amcrest/src/amcrest/rtsp-url.test.ts
git commit -m "feat(amcrest): add RTSP URL builder"
```

---

### Task 3: HTTP Digest auth (header + fetch wrapper)

**Files:**
- Create: `camera-ui-amcrest/src/amcrest/digest-auth.ts`
- Test: `camera-ui-amcrest/src/amcrest/digest-auth.test.ts`

**Interfaces:**
- Produces:
  - `buildDigestAuthHeader(p: DigestParams): string` — computes an RFC 2617 MD5 digest `Authorization` header. `DigestParams = { username: string; password: string; realm: string; nonce: string; method: string; uri: string; qop?: string; nc?: string; cnonce?: string; opaque?: string; algorithm?: string }`.
  - `parseWwwAuthenticate(header: string): Record<string, string>` — parses a `Digest ...` challenge into a key/value map.
  - `digestFetch(opts: DigestFetchOptions): Promise<Response>` — performs a GET/POST with automatic digest handshake using the global `fetch`. `DigestFetchOptions = { url: string; username: string; password: string; method?: string; headers?: Record<string,string>; body?: BodyInit; signal?: AbortSignal }`. Returns the second (authenticated) `Response`.

- [ ] **Step 1: Write the failing test** (RFC 2617 canonical vector)

```ts
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { test } from 'node:test';

import { buildDigestAuthHeader, parseWwwAuthenticate } from './digest-auth.js';

const md5 = (s: string) => createHash('md5').update(s).digest('hex');

test('parseWwwAuthenticate parses a Digest challenge', () => {
  const parsed = parseWwwAuthenticate('Digest realm="Login to camera", qop="auth", nonce="abc123", opaque="xyz"');
  assert.equal(parsed.realm, 'Login to camera');
  assert.equal(parsed.qop, 'auth');
  assert.equal(parsed.nonce, 'abc123');
  assert.equal(parsed.opaque, 'xyz');
});

test('buildDigestAuthHeader computes the RFC 2617 response with qop=auth', () => {
  const p = {
    username: 'admin',
    password: 'secret',
    realm: 'Login to camera',
    nonce: 'abc123',
    method: 'GET',
    uri: '/cgi-bin/magicBox.cgi?action=getSystemInfo',
    qop: 'auth',
    nc: '00000001',
    cnonce: 'deadbeef',
  };
  const ha1 = md5(`${p.username}:${p.realm}:${p.password}`);
  const ha2 = md5(`${p.method}:${p.uri}`);
  const expectedResponse = md5(`${ha1}:${p.nonce}:${p.nc}:${p.cnonce}:${p.qop}:${ha2}`);

  const header = buildDigestAuthHeader(p);
  assert.match(header, /^Digest /);
  assert.ok(header.includes(`username="admin"`));
  assert.ok(header.includes(`realm="Login to camera"`));
  assert.ok(header.includes(`nonce="abc123"`));
  assert.ok(header.includes(`uri="${p.uri}"`));
  assert.ok(header.includes(`qop=auth`));
  assert.ok(header.includes(`nc=00000001`));
  assert.ok(header.includes(`cnonce="deadbeef"`));
  assert.ok(header.includes(`response="${expectedResponse}"`));
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `node --import tsx --test src/amcrest/digest-auth.test.ts`
Expected: FAIL — cannot find module `./digest-auth.js`.

- [ ] **Step 3: Write the implementation**

```ts
import { createHash, randomBytes } from 'node:crypto';

export interface DigestParams {
  username: string;
  password: string;
  realm: string;
  nonce: string;
  method: string;
  uri: string;
  qop?: string;
  nc?: string;
  cnonce?: string;
  opaque?: string;
  algorithm?: string;
}

const md5 = (s: string): string => createHash('md5').update(s).digest('hex');

export function parseWwwAuthenticate(header: string): Record<string, string> {
  const out: Record<string, string> = {};
  const body = header.replace(/^Digest\s+/i, '');
  const re = /(\w+)=(?:"([^"]*)"|([^,]*))/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(body)) !== null) {
    out[m[1]] = (m[2] ?? m[3] ?? '').trim();
  }
  return out;
}

export function buildDigestAuthHeader(p: DigestParams): string {
  const qop = p.qop;
  const nc = p.nc ?? '00000001';
  const cnonce = p.cnonce ?? randomBytes(8).toString('hex');
  const ha1 = md5(`${p.username}:${p.realm}:${p.password}`);
  const ha2 = md5(`${p.method}:${p.uri}`);
  const response = qop
    ? md5(`${ha1}:${p.nonce}:${nc}:${cnonce}:${qop}:${ha2}`)
    : md5(`${ha1}:${p.nonce}:${ha2}`);

  const parts = [
    `username="${p.username}"`,
    `realm="${p.realm}"`,
    `nonce="${p.nonce}"`,
    `uri="${p.uri}"`,
    `response="${response}"`,
  ];
  if (p.algorithm) parts.push(`algorithm=${p.algorithm}`);
  if (qop) {
    parts.push(`qop=${qop}`, `nc=${nc}`, `cnonce="${cnonce}"`);
  }
  if (p.opaque) parts.push(`opaque="${p.opaque}"`);
  return `Digest ${parts.join(', ')}`;
}

export interface DigestFetchOptions {
  url: string;
  username: string;
  password: string;
  method?: string;
  headers?: Record<string, string>;
  body?: BodyInit;
  signal?: AbortSignal;
}

export async function digestFetch(opts: DigestFetchOptions): Promise<Response> {
  const method = opts.method ?? 'GET';
  const first = await fetch(opts.url, { method, headers: opts.headers, signal: opts.signal });
  if (first.status !== 401) {
    return first;
  }
  const challenge = first.headers.get('www-authenticate');
  if (!challenge) {
    return first;
  }
  // Drain the 401 body so the socket can be reused.
  await first.arrayBuffer().catch(() => undefined);

  const c = parseWwwAuthenticate(challenge);
  const u = new URL(opts.url);
  const uri = `${u.pathname}${u.search}`;
  const authHeader = buildDigestAuthHeader({
    username: opts.username,
    password: opts.password,
    realm: c.realm ?? '',
    nonce: c.nonce ?? '',
    method,
    uri,
    qop: c.qop ? c.qop.split(',')[0].trim() : undefined,
    opaque: c.opaque,
    algorithm: c.algorithm,
  });

  return fetch(opts.url, {
    method,
    headers: { ...opts.headers, authorization: authHeader },
    body: opts.body,
    signal: opts.signal,
  });
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `node --import tsx --test src/amcrest/digest-auth.test.ts`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add camera-ui-amcrest/src/amcrest/digest-auth.ts camera-ui-amcrest/src/amcrest/digest-auth.test.ts
git commit -m "feat(amcrest): add HTTP digest auth header and fetch wrapper"
```

---

### Task 4: Device system-info parser

**Files:**
- Create: `camera-ui-amcrest/src/amcrest/system-info.ts`
- Test: `camera-ui-amcrest/src/amcrest/system-info.test.ts`

**Interfaces:**
- Produces:
  - `parseKeyValueBody(text: string): Record<string, string>` — parses Amcrest CGI `key=value` line bodies.
  - `parseSystemInfo(text: string): AmcrestSystemInfo` where `AmcrestSystemInfo = { deviceType?: string; hardwareVersion?: string; serialNumber?: string }`. Throws `Error('not amcrest')` if none of the three fields are present.

- [ ] **Step 1: Write the failing test**

```ts
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { parseKeyValueBody, parseSystemInfo } from './system-info.js';

const SAMPLE = `appAutoStart=true
deviceType=IP4M-1041B
hardwareVersion=1.00
processor=SSC327DE
serialNumber=ABC12345`;

test('parseKeyValueBody splits key=value lines', () => {
  const kv = parseKeyValueBody(SAMPLE);
  assert.equal(kv.deviceType, 'IP4M-1041B');
  assert.equal(kv.serialNumber, 'ABC12345');
  assert.equal(kv.processor, 'SSC327DE');
});

test('parseSystemInfo extracts identity fields', () => {
  const info = parseSystemInfo(SAMPLE);
  assert.deepEqual(info, { deviceType: 'IP4M-1041B', hardwareVersion: '1.00', serialNumber: 'ABC12345' });
});

test('parseSystemInfo throws when not an amcrest device', () => {
  assert.throws(() => parseSystemInfo('foo=bar\nbaz=qux'), /not amcrest/);
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `node --import tsx --test src/amcrest/system-info.test.ts`
Expected: FAIL — cannot find module.

- [ ] **Step 3: Write the implementation**

```ts
export interface AmcrestSystemInfo {
  deviceType?: string;
  hardwareVersion?: string;
  serialNumber?: string;
}

export function parseKeyValueBody(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split(/\r?\n/)) {
    if (!line) continue;
    let idx = line.indexOf('=');
    if (idx === -1) idx = line.length;
    out[line.substring(0, idx)] = line.substring(idx + 1).trim();
  }
  return out;
}

export function parseSystemInfo(text: string): AmcrestSystemInfo {
  const kv = parseKeyValueBody(text);
  const info: AmcrestSystemInfo = {
    deviceType: kv.deviceType || undefined,
    hardwareVersion: kv.hardwareVersion || undefined,
    serialNumber: kv.serialNumber || undefined,
  };
  if (!info.deviceType && !info.hardwareVersion && !info.serialNumber) {
    throw new Error('not amcrest');
  }
  return info;
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `node --import tsx --test src/amcrest/system-info.test.ts`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add camera-ui-amcrest/src/amcrest/system-info.ts camera-ui-amcrest/src/amcrest/system-info.test.ts
git commit -m "feat(amcrest): add device system-info parser"
```

---

### Task 5: Encode-config (stream) parser

**Files:**
- Create: `camera-ui-amcrest/src/amcrest/encode-config.ts`
- Test: `camera-ui-amcrest/src/amcrest/encode-config.test.ts`

**Interfaces:**
- Consumes: `parseKeyValueBody` (Task 4).
- Produces: `parseEncodeConfig(text: string, channel: number): AmcrestStream[]` where
  `AmcrestStream = { role: 'main' | 'sub'; subtype: number; codec?: string; width?: number; height?: number }`.
  Parses the `configManager.cgi?action=getConfig&name=Encode` body. `MainFormat[0]` → subtype 0 (`main`); `ExtraFormat[0]` → subtype 1 (`sub`). Skips formats with `VideoEnable=false`.

- [ ] **Step 1: Write the failing test**

```ts
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { parseEncodeConfig } from './encode-config.js';

const SAMPLE = `table.Encode[0].MainFormat[0].VideoEnable=true
table.Encode[0].MainFormat[0].Video.Compression=H.264
table.Encode[0].MainFormat[0].Video.Width=1920
table.Encode[0].MainFormat[0].Video.Height=1080
table.Encode[0].ExtraFormat[0].VideoEnable=true
table.Encode[0].ExtraFormat[0].Video.Compression=H.265
table.Encode[0].ExtraFormat[0].Video.Width=704
table.Encode[0].ExtraFormat[0].Video.Height=480`;

test('parses main and sub streams for channel 1', () => {
  const streams = parseEncodeConfig(SAMPLE, 1);
  assert.deepEqual(streams, [
    { role: 'main', subtype: 0, codec: 'h264', width: 1920, height: 1080 },
    { role: 'sub', subtype: 1, codec: 'h265', width: 704, height: 480 },
  ]);
});

test('skips a disabled extra format', () => {
  const disabled = SAMPLE.replace('ExtraFormat[0].VideoEnable=true', 'ExtraFormat[0].VideoEnable=false');
  const streams = parseEncodeConfig(disabled, 1);
  assert.equal(streams.length, 1);
  assert.equal(streams[0].role, 'main');
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `node --import tsx --test src/amcrest/encode-config.test.ts`
Expected: FAIL — cannot find module.

- [ ] **Step 3: Write the implementation**

```ts
import { parseKeyValueBody } from './system-info.js';

export interface AmcrestStream {
  role: 'main' | 'sub';
  subtype: number;
  codec?: string;
  width?: number;
  height?: number;
}

function fromAmcrestVideoCodec(codec?: string): string | undefined {
  const c = codec?.trim();
  if (c === 'H.264') return 'h264';
  if (c === 'H.265') return 'h265';
  return c || undefined;
}

export function parseEncodeConfig(text: string, channel: number): AmcrestStream[] {
  const kv = parseKeyValueBody(text);
  const ch = channel - 1;
  const prefix = `table.Encode[${ch}]`;
  const streams: AmcrestStream[] = [];

  const formats: Array<{ role: 'main' | 'sub'; subtype: number; key: string }> = [
    { role: 'main', subtype: 0, key: `${prefix}.MainFormat[0]` },
    { role: 'sub', subtype: 1, key: `${prefix}.ExtraFormat[0]` },
  ];

  for (const fmt of formats) {
    const compression = kv[`${fmt.key}.Video.Compression`];
    if (compression === undefined) continue;
    if (kv[`${fmt.key}.VideoEnable`] === 'false') continue;

    const width = kv[`${fmt.key}.Video.Width`];
    const height = kv[`${fmt.key}.Video.Height`];
    streams.push({
      role: fmt.role,
      subtype: fmt.subtype,
      codec: fromAmcrestVideoCodec(compression),
      width: width ? parseInt(width, 10) : undefined,
      height: height ? parseInt(height, 10) : undefined,
    });
  }

  return streams;
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `node --import tsx --test src/amcrest/encode-config.test.ts`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add camera-ui-amcrest/src/amcrest/encode-config.ts camera-ui-amcrest/src/amcrest/encode-config.test.ts
git commit -m "feat(amcrest): add encode-config stream parser"
```

---

### Task 6: Event blob parser

**Files:**
- Create: `camera-ui-amcrest/src/amcrest/events.ts`
- Test: `camera-ui-amcrest/src/amcrest/events.test.ts`
- Create fixtures: `camera-ui-amcrest/src/fixtures/human-detected.json`, `camera-ui-amcrest/src/fixtures/face-detected.json` (copy from `../../scrypted/scrypted/plugins/amcrest/dumps/`)

**Interfaces:**
- Produces: `parseAmcrestEvent(blob: string): AmcrestEvent | undefined` where
  `AmcrestEvent = { code: string; action: string; index?: number; data?: unknown }`.
  Parses one event blob of the form `Code=<code>;action=<action>;index=<n>;data=<json>`. `data` is `JSON.parse`d when present; malformed JSON yields `data: undefined` (never throws). Returns `undefined` when no `Code=` is present.

- [ ] **Step 1: Copy fixtures**

Run:
```bash
mkdir -p camera-ui-amcrest/src/fixtures
cp ../scrypted/scrypted/plugins/amcrest/dumps/amcrest-human-detected.json camera-ui-amcrest/src/fixtures/human-detected.json
cp ../scrypted/scrypted/plugins/amcrest/dumps/amcrest-face-detected.json camera-ui-amcrest/src/fixtures/face-detected.json
```
(Adjust the relative path if the scrypted checkout is elsewhere.)

- [ ] **Step 2: Write the failing test**

```ts
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import { parseAmcrestEvent } from './events.js';

const humanData = readFileSync(fileURLToPath(new URL('../fixtures/human-detected.json', import.meta.url)), 'utf8');

test('parses a motion start event without data', () => {
  const ev = parseAmcrestEvent('Code=VideoMotion;action=Start;index=0');
  assert.deepEqual(ev, { code: 'VideoMotion', action: 'Start', index: 0, data: undefined });
});

test('parses a smart event with JSON data payload', () => {
  const blob = `Code=CrossRegionDetection;action=Start;index=0;data=${humanData}`;
  const ev = parseAmcrestEvent(blob);
  assert.equal(ev?.code, 'CrossRegionDetection');
  assert.equal(ev?.action, 'Start');
  assert.equal((ev?.data as { Object: { ObjectType: string } }).Object.ObjectType, 'Human');
});

test('tolerates malformed JSON data', () => {
  const ev = parseAmcrestEvent('Code=X;action=Start;data={not json');
  assert.equal(ev?.code, 'X');
  assert.equal(ev?.data, undefined);
});

test('returns undefined when no Code present', () => {
  assert.equal(parseAmcrestEvent('Heartbeat'), undefined);
});
```

- [ ] **Step 3: Run test to verify it fails**

Run: `node --import tsx --test src/amcrest/events.test.ts`
Expected: FAIL — cannot find module `./events.js`.

- [ ] **Step 4: Write the implementation**

```ts
export interface AmcrestEvent {
  code: string;
  action: string;
  index?: number;
  data?: unknown;
}

export function parseAmcrestEvent(blob: string): AmcrestEvent | undefined {
  const codeMatch = /Code=([^;]+)/.exec(blob);
  if (!codeMatch) return undefined;

  const actionMatch = /action=([^;]+)/.exec(blob);
  const indexMatch = /index=([0-9]+)/.exec(blob);

  // data= may itself contain ';' inside JSON, so capture everything after 'data='.
  const dataIdx = blob.indexOf('data=');
  let data: unknown;
  if (dataIdx !== -1) {
    const raw = blob.substring(dataIdx + 'data='.length).trim();
    try {
      data = JSON.parse(raw);
    } catch {
      data = undefined;
    }
  }

  return {
    code: codeMatch[1].trim(),
    action: actionMatch ? actionMatch[1].trim() : '',
    index: indexMatch ? parseInt(indexMatch[1], 10) : undefined,
    data,
  };
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `node --import tsx --test src/amcrest/events.test.ts`
Expected: PASS (4 tests).

- [ ] **Step 6: Commit**

```bash
git add camera-ui-amcrest/src/amcrest/events.ts camera-ui-amcrest/src/amcrest/events.test.ts camera-ui-amcrest/src/fixtures
git commit -m "feat(amcrest): add event blob parser and fixtures"
```

---

### Task 7: Event classification

**Files:**
- Create: `camera-ui-amcrest/src/amcrest/classify.ts`
- Test: `camera-ui-amcrest/src/amcrest/classify.test.ts`

**Interfaces:**
- Consumes: `AmcrestEvent` (Task 6).
- Produces: `classifyAmcrestEvent(ev: AmcrestEvent): AmcrestClassification | undefined` where
  `AmcrestClassification =
     | { kind: 'motion'; active: boolean }
     | { kind: 'audio'; active: boolean }
     | { kind: 'object'; category: 'person' | 'vehicle'; active: boolean }
     | { kind: 'doorbell' }`.
  Returns `undefined` for events we don't map. Mapping rules:
  - `VideoMotion` → motion; `active = action === 'Start'`.
  - `AudioMutation` → audio; `active = action === 'Start'`.
  - `SmartMotionHuman` → object person, active on `Start`.
  - `Vehicle` (SmartMotionVehicle) → object vehicle, active on `Start`.
  - `FaceDetection` → object person, active on `Start`.
  - `CrossLineDetection` / `CrossRegionDetection` → object; category from `data.Object.ObjectType` (`Human`→person, `Vehicle`→vehicle); active on `Start`.
  - `_DoTalkAction_` with `action === 'Invite'` → doorbell. (Dahua `CallNoAnswered` also → doorbell; best-effort.)

- [ ] **Step 1: Write the failing test**

```ts
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { classifyAmcrestEvent } from './classify.js';

test('classifies motion start/stop', () => {
  assert.deepEqual(classifyAmcrestEvent({ code: 'VideoMotion', action: 'Start' }), { kind: 'motion', active: true });
  assert.deepEqual(classifyAmcrestEvent({ code: 'VideoMotion', action: 'Stop' }), { kind: 'motion', active: false });
});

test('classifies audio mutation', () => {
  assert.deepEqual(classifyAmcrestEvent({ code: 'AudioMutation', action: 'Start' }), { kind: 'audio', active: true });
});

test('classifies smart human and vehicle', () => {
  assert.deepEqual(classifyAmcrestEvent({ code: 'SmartMotionHuman', action: 'Start' }), { kind: 'object', category: 'person', active: true });
  assert.deepEqual(classifyAmcrestEvent({ code: 'Vehicle', action: 'Start' }), { kind: 'object', category: 'vehicle', active: true });
});

test('classifies cross-region by ObjectType', () => {
  const ev = { code: 'CrossRegionDetection', action: 'Start', data: { Object: { ObjectType: 'Human' } } };
  assert.deepEqual(classifyAmcrestEvent(ev), { kind: 'object', category: 'person', active: true });
});

test('classifies amcrest doorbell invite', () => {
  assert.deepEqual(classifyAmcrestEvent({ code: '_DoTalkAction_', action: 'Invite' }), { kind: 'doorbell' });
});

test('ignores unrelated events', () => {
  assert.equal(classifyAmcrestEvent({ code: 'NTPAdjustTime', action: 'Start' }), undefined);
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `node --import tsx --test src/amcrest/classify.test.ts`
Expected: FAIL — cannot find module.

- [ ] **Step 3: Write the implementation**

```ts
import type { AmcrestEvent } from './events.js';

export type AmcrestClassification =
  | { kind: 'motion'; active: boolean }
  | { kind: 'audio'; active: boolean }
  | { kind: 'object'; category: 'person' | 'vehicle'; active: boolean }
  | { kind: 'doorbell' };

function objectTypeToCategory(objectType?: string): 'person' | 'vehicle' | undefined {
  if (objectType === 'Human') return 'person';
  if (objectType === 'Vehicle') return 'vehicle';
  return undefined;
}

export function classifyAmcrestEvent(ev: AmcrestEvent): AmcrestClassification | undefined {
  const active = ev.action === 'Start';

  switch (ev.code) {
    case 'VideoMotion':
      return { kind: 'motion', active };
    case 'AudioMutation':
      return { kind: 'audio', active };
    case 'SmartMotionHuman':
      return { kind: 'object', category: 'person', active };
    case 'Vehicle':
      return { kind: 'object', category: 'vehicle', active };
    case 'FaceDetection':
      return { kind: 'object', category: 'person', active };
    case 'CrossLineDetection':
    case 'CrossRegionDetection': {
      const objectType = (ev.data as { Object?: { ObjectType?: string } } | undefined)?.Object?.ObjectType;
      const category = objectTypeToCategory(objectType);
      if (!category) return undefined;
      return { kind: 'object', category, active };
    }
    case '_DoTalkAction_':
      return ev.action === 'Invite' ? { kind: 'doorbell' } : undefined;
    case 'CallNoAnswered':
      // Dahua doorbell (best-effort, untested)
      return { kind: 'doorbell' };
    default:
      return undefined;
  }
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `node --import tsx --test src/amcrest/classify.test.ts`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add camera-ui-amcrest/src/amcrest/classify.ts camera-ui-amcrest/src/amcrest/classify.test.ts
git commit -m "feat(amcrest): add event classification"
```

---

### Task 8: PTZ command mapping

**Files:**
- Create: `camera-ui-amcrest/src/amcrest/ptz-commands.ts`
- Test: `camera-ui-amcrest/src/amcrest/ptz-commands.test.ts`

**Interfaces:**
- Produces: `ptzCommandForVelocity(v: { panSpeed?: number; tiltSpeed?: number; zoomSpeed?: number }): PtzCommand` where
  `PtzCommand = { action: 'start' | 'stop'; code: string; arg2: number }`.
  Maps a velocity vector to an Amcrest `ptz.cgi` continuous command. Zero vector → `{ action: 'stop', code: 'Up', arg2: 0 }` (code is ignored on stop). `arg2` = speed 1–8 from `max(|components|)` scaled. Priority: zoom, then tilt, then pan (single-axis continuous move). Codes: `Left`/`Right`, `Up`/`Down`, `ZoomTele`/`ZoomWide`.

- [ ] **Step 1: Write the failing test**

```ts
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { ptzCommandForVelocity } from './ptz-commands.js';

test('zero velocity is a stop', () => {
  assert.deepEqual(ptzCommandForVelocity({ panSpeed: 0, tiltSpeed: 0, zoomSpeed: 0 }), { action: 'stop', code: 'Up', arg2: 0 });
});

test('pan right', () => {
  const c = ptzCommandForVelocity({ panSpeed: 1 });
  assert.equal(c.action, 'start');
  assert.equal(c.code, 'Right');
  assert.equal(c.arg2, 8);
});

test('pan left at half speed', () => {
  const c = ptzCommandForVelocity({ panSpeed: -0.5 });
  assert.equal(c.code, 'Left');
  assert.equal(c.arg2, 4);
});

test('tilt up takes priority over pan', () => {
  const c = ptzCommandForVelocity({ panSpeed: 0.2, tiltSpeed: 0.9 });
  assert.equal(c.code, 'Up');
});

test('zoom tele takes highest priority', () => {
  const c = ptzCommandForVelocity({ panSpeed: 0.9, tiltSpeed: 0.9, zoomSpeed: 0.3 });
  assert.equal(c.code, 'ZoomTele');
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `node --import tsx --test src/amcrest/ptz-commands.test.ts`
Expected: FAIL — cannot find module.

- [ ] **Step 3: Write the implementation**

```ts
export interface PtzVelocity {
  panSpeed?: number;
  tiltSpeed?: number;
  zoomSpeed?: number;
}

export interface PtzCommand {
  action: 'start' | 'stop';
  code: string;
  arg2: number;
}

function toSpeed(magnitude: number): number {
  const clamped = Math.min(1, Math.abs(magnitude));
  return Math.max(1, Math.round(clamped * 8));
}

export function ptzCommandForVelocity(v: PtzVelocity): PtzCommand {
  const pan = v.panSpeed ?? 0;
  const tilt = v.tiltSpeed ?? 0;
  const zoom = v.zoomSpeed ?? 0;

  if (pan === 0 && tilt === 0 && zoom === 0) {
    return { action: 'stop', code: 'Up', arg2: 0 };
  }

  if (zoom !== 0) {
    return { action: 'start', code: zoom > 0 ? 'ZoomTele' : 'ZoomWide', arg2: toSpeed(zoom) };
  }
  if (tilt !== 0) {
    return { action: 'start', code: tilt > 0 ? 'Up' : 'Down', arg2: toSpeed(tilt) };
  }
  return { action: 'start', code: pan > 0 ? 'Right' : 'Left', arg2: toSpeed(pan) };
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `node --import tsx --test src/amcrest/ptz-commands.test.ts`
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add camera-ui-amcrest/src/amcrest/ptz-commands.ts camera-ui-amcrest/src/amcrest/ptz-commands.test.ts
git commit -m "feat(amcrest): add PTZ velocity-to-command mapping"
```

---

### Task 9: Talkback target selection

**Files:**
- Create: `camera-ui-amcrest/src/amcrest/talkback.ts` (selection function only in this task)
- Test: `camera-ui-amcrest/src/amcrest/talkback.test.ts`

**Interfaces:**
- Produces: `selectTalkbackTarget(deviceType: string | undefined): TalkbackTarget` where
  `TalkbackTarget = { codec: 'aac' | 'pcm_alaw'; contentType: 'Audio/AAC' | 'Audio/G.711A'; sampleRate: number }`.
  Amcrest devices (deviceType containing `AD1` / `AD4` doorbell prefixes, or default) → AAC 16000 (`Audio/AAC`). Explicit Dahua doorbell (deviceType starting `DH-` or `DB`) → G.711A 8000 (`Audio/G.711A`).

- [ ] **Step 1: Write the failing test**

```ts
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { selectTalkbackTarget } from './talkback.js';

test('amcrest doorbell defaults to AAC', () => {
  assert.deepEqual(selectTalkbackTarget('AD410'), { codec: 'aac', contentType: 'Audio/AAC', sampleRate: 16000 });
});

test('unknown device defaults to AAC', () => {
  assert.deepEqual(selectTalkbackTarget(undefined), { codec: 'aac', contentType: 'Audio/AAC', sampleRate: 16000 });
});

test('dahua device uses G.711A', () => {
  assert.deepEqual(selectTalkbackTarget('DH-VTO2211'), { codec: 'pcm_alaw', contentType: 'Audio/G.711A', sampleRate: 8000 });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `node --import tsx --test src/amcrest/talkback.test.ts`
Expected: FAIL — cannot find module.

- [ ] **Step 3: Write the implementation** (selection only; stream sink added in Task 12)

```ts
export interface TalkbackTarget {
  codec: 'aac' | 'pcm_alaw';
  contentType: 'Audio/AAC' | 'Audio/G.711A';
  sampleRate: number;
}

export function selectTalkbackTarget(deviceType: string | undefined): TalkbackTarget {
  const dt = (deviceType ?? '').toUpperCase();
  const isDahua = dt.startsWith('DH-') || dt.startsWith('DB');
  if (isDahua) {
    return { codec: 'pcm_alaw', contentType: 'Audio/G.711A', sampleRate: 8000 };
  }
  return { codec: 'aac', contentType: 'Audio/AAC', sampleRate: 16000 };
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `node --import tsx --test src/amcrest/talkback.test.ts`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add camera-ui-amcrest/src/amcrest/talkback.ts camera-ui-amcrest/src/amcrest/talkback.test.ts
git commit -m "feat(amcrest): add talkback target codec selection"
```

---

### Task 10: `AmcrestClient` (CGI composition)

**Files:**
- Create: `camera-ui-amcrest/src/amcrest/api.ts`
- Test: `camera-ui-amcrest/src/amcrest/api.test.ts`

**Interfaces:**
- Consumes: `digestFetch` (Task 3), `parseSystemInfo` (Task 4), `parseEncodeConfig` (Task 5), `ptzCommandForVelocity` (Task 8), `buildRtspUrl` (Task 2).
- Produces: class `AmcrestClient` constructed with `{ ip: string; username: string; password: string; port?: number; httpPort?: number }` exposing:
  - `urlFor(pathAndQuery: string): string`  → full `http://ip[:httpPort]/...` URL (unit-tested; pure).
  - `getSystemInfo(): Promise<AmcrestSystemInfo>`
  - `getStreams(channel: number): Promise<AmcrestStream[]>`
  - `snapshot(channel: number, signal?: AbortSignal): Promise<Buffer>`
  - `ptz(channel: number, v: PtzVelocity): Promise<void>`
  - `attachEvents(signal: AbortSignal): Promise<ReadableStream<Uint8Array>>`  (returns the body stream of the long-lived `eventManager` connection)
  - `rtspUrl(channel: number, subtype: number): string`

- [ ] **Step 1: Write the failing test** (only the pure `urlFor` / `rtspUrl` are unit-tested; network methods are hardware-verified)

```ts
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { AmcrestClient } from './api.js';

test('urlFor builds default http url', () => {
  const c = new AmcrestClient({ ip: '192.168.1.50', username: 'admin', password: 'pw' });
  assert.equal(c.urlFor('/cgi-bin/magicBox.cgi?action=getSystemInfo'), 'http://192.168.1.50/cgi-bin/magicBox.cgi?action=getSystemInfo');
});

test('urlFor honours a custom http port', () => {
  const c = new AmcrestClient({ ip: '192.168.1.50', username: 'admin', password: 'pw', httpPort: 8080 });
  assert.equal(c.urlFor('/cgi-bin/snapshot.cgi?channel=1'), 'http://192.168.1.50:8080/cgi-bin/snapshot.cgi?channel=1');
});

test('rtspUrl delegates to buildRtspUrl', () => {
  const c = new AmcrestClient({ ip: '10.0.0.9', username: 'admin', password: 'pw', port: 5544 });
  assert.equal(c.rtspUrl(2, 1), 'rtsp://admin:pw@10.0.0.9:5544/cam/realmonitor?channel=2&subtype=1');
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `node --import tsx --test src/amcrest/api.test.ts`
Expected: FAIL — cannot find module.

- [ ] **Step 3: Write the implementation**

```ts
import { Buffer } from 'node:buffer';

import { digestFetch } from './digest-auth.js';
import { parseEncodeConfig } from './encode-config.js';
import { ptzCommandForVelocity } from './ptz-commands.js';
import { buildRtspUrl } from './rtsp-url.js';
import { parseSystemInfo } from './system-info.js';

import type { AmcrestStream } from './encode-config.js';
import type { PtzVelocity } from './ptz-commands.js';
import type { AmcrestSystemInfo } from './system-info.js';

export interface AmcrestClientOptions {
  ip: string;
  username: string;
  password: string;
  port?: number; // RTSP port
  httpPort?: number; // CGI/HTTP port
}

export class AmcrestClient {
  constructor(private readonly opts: AmcrestClientOptions) {}

  urlFor(pathAndQuery: string): string {
    const host = this.opts.httpPort && this.opts.httpPort !== 80 ? `${this.opts.ip}:${this.opts.httpPort}` : this.opts.ip;
    return `http://${host}${pathAndQuery}`;
  }

  private fetch(pathAndQuery: string, init?: { method?: string; headers?: Record<string, string>; body?: BodyInit; signal?: AbortSignal }): Promise<Response> {
    return digestFetch({
      url: this.urlFor(pathAndQuery),
      username: this.opts.username,
      password: this.opts.password,
      method: init?.method,
      headers: init?.headers,
      body: init?.body,
      signal: init?.signal,
    });
  }

  rtspUrl(channel: number, subtype: number): string {
    return buildRtspUrl({ ip: this.opts.ip, username: this.opts.username, password: this.opts.password, port: this.opts.port, channel, subtype });
  }

  async getSystemInfo(): Promise<AmcrestSystemInfo> {
    const res = await this.fetch('/cgi-bin/magicBox.cgi?action=getSystemInfo');
    return parseSystemInfo(await res.text());
  }

  async getStreams(channel: number): Promise<AmcrestStream[]> {
    const res = await this.fetch('/cgi-bin/configManager.cgi?action=getConfig&name=Encode');
    return parseEncodeConfig(await res.text(), channel);
  }

  async snapshot(channel: number, signal?: AbortSignal): Promise<Buffer> {
    const res = await this.fetch(`/cgi-bin/snapshot.cgi?channel=${channel}`, { signal });
    return Buffer.from(await res.arrayBuffer());
  }

  async ptz(channel: number, v: PtzVelocity): Promise<void> {
    const cmd = ptzCommandForVelocity(v);
    const q = `/cgi-bin/ptz.cgi?action=${cmd.action}&channel=${channel}&code=${cmd.code}&arg1=0&arg2=${cmd.arg2}&arg3=0`;
    await this.fetch(q);
  }

  async attachEvents(signal: AbortSignal): Promise<ReadableStream<Uint8Array>> {
    const res = await this.fetch('/cgi-bin/eventManager.cgi?action=attach&codes=[All]', { signal });
    if (!res.body) {
      throw new Error('event stream has no body');
    }
    return res.body;
  }
}
```

- [ ] **Step 4: Run test to verify it passes, then typecheck**

Run: `node --import tsx --test src/amcrest/api.test.ts`
Expected: PASS (3 tests).
Run: `npm run build`
Expected: `tsc` succeeds.

- [ ] **Step 5: Commit**

```bash
git add camera-ui-amcrest/src/amcrest/api.ts camera-ui-amcrest/src/amcrest/api.test.ts
git commit -m "feat(amcrest): add AmcrestClient CGI wrapper"
```

---

### Task 11: Sensors + event reader

**Files:**
- Create: `camera-ui-amcrest/src/sensors/motion.ts`, `object.ts`, `audio.ts`, `doorbell.ts`, `ptz.ts`, `index.ts`
- Create: `camera-ui-amcrest/src/amcrest/event-reader.ts`
- Test: `camera-ui-amcrest/src/amcrest/event-reader.test.ts`
- Test: `camera-ui-amcrest/src/sensors/object.test.ts`

**Interfaces:**
- Consumes: `MotionSensor`, `ObjectSensor`, `AudioSensor`, `DoorbellTrigger`, `PTZControl` from `@camera.ui/sdk`; `AmcrestClient` (Task 10); `parseAmcrestEvent` (Task 6); `classifyAmcrestEvent` (Task 7); `ptzCommandForVelocity` (Task 8).
- Produces:
  - `AmcrestMotionSensor`, `AmcrestAudioSensor`, `AmcrestDoorbellTrigger` (thin subclasses).
  - `AmcrestObjectSensor` with `report(category: 'person' | 'vehicle', active: boolean): void` that tracks active categories and calls `reportDetections` with synthesized full-frame `TrackedDetection`s (mirrors `OnvifObjectSensor`).
  - `AmcrestPTZSensor extends PTZControl` with `setCapabilities(pan, tilt, zoom)` and overridden `setVelocity` calling `client.ptz(channel, ...)`.
  - `splitEventMultipart(chunk: string, boundary: string): string[]` — splits an event multipart buffer into event blobs, handling `--boundary` / `-- boundary` and stray `HTTP/1.x 200 OK` lines (unit-tested).

- [ ] **Step 1: Write the failing test for the multipart splitter**

```ts
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { splitEventMultipart } from './event-reader.js';

const BODY = [
  '--myboundary',
  'Content-Type: text/plain',
  'Content-Length: 40',
  '',
  'Code=VideoMotion;action=Start;index=0',
  '--myboundary',
  'Content-Type: text/plain',
  'Content-Length: 39',
  '',
  'Code=VideoMotion;action=Stop;index=0',
  '--myboundary--',
].join('\r\n');

test('splits multipart body into event blobs', () => {
  const blobs = splitEventMultipart(BODY, 'myboundary');
  assert.equal(blobs.length, 2);
  assert.ok(blobs[0].includes('Code=VideoMotion;action=Start'));
  assert.ok(blobs[1].includes('Code=VideoMotion;action=Stop'));
});

test('handles the "-- boundary" (spaced) variant', () => {
  const spaced = BODY.replace(/--myboundary/g, '-- myboundary');
  const blobs = splitEventMultipart(spaced, 'myboundary');
  assert.equal(blobs.length, 2);
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `node --import tsx --test src/amcrest/event-reader.test.ts`
Expected: FAIL — cannot find module.

- [ ] **Step 3: Implement `event-reader.ts`**

```ts
export function splitEventMultipart(chunk: string, boundary: string): string[] {
  const marker = `--${boundary}`;
  const spaced = `-- ${boundary}`;
  const normalized = chunk.split(spaced).join(marker);

  const parts = normalized.split(marker);
  const blobs: string[] = [];

  for (const part of parts) {
    const trimmed = part.trim();
    if (!trimmed || trimmed === '--') continue;

    // Drop MIME headers: the payload is after the first blank line.
    const sepIdx = trimmed.indexOf('\r\n\r\n');
    const payload = sepIdx !== -1 ? trimmed.substring(sepIdx + 4) : trimmed;
    const cleaned = payload
      .split(/\r?\n/)
      .filter((line) => line && !/^HTTP\/1\.[01] 200 OK$/.test(line.trim()))
      .join('\n')
      .trim();

    if (cleaned.includes('Code=')) {
      blobs.push(cleaned);
    }
  }

  return blobs;
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `node --import tsx --test src/amcrest/event-reader.test.ts`
Expected: PASS (2 tests).

- [ ] **Step 5: Write the failing test for `AmcrestObjectSensor`**

Note: this test stubs the SDK base by asserting on the arguments passed to `reportDetections`. Create `src/sensors/object.test.ts`:

```ts
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { AmcrestObjectSensor } from './object.js';

test('tracks active categories and reports full-frame detections', () => {
  const calls: Array<{ active: boolean; labels: string[] }> = [];
  const sensor = new AmcrestObjectSensor();
  // Override the SDK method to observe calls.
  (sensor as unknown as { reportDetections: (active: boolean, dets?: Array<{ label: string }>) => void }).reportDetections = (active, dets) => {
    calls.push({ active, labels: (dets ?? []).map((d) => d.label) });
  };

  sensor.report('person', true);
  sensor.report('vehicle', true);
  sensor.report('person', false);
  sensor.report('vehicle', false);

  assert.deepEqual(calls[0], { active: true, labels: ['person'] });
  assert.deepEqual(calls[1], { active: true, labels: ['person', 'vehicle'] });
  assert.deepEqual(calls[2], { active: true, labels: ['vehicle'] });
  assert.deepEqual(calls[3], { active: false, labels: [] });
});
```

- [ ] **Step 6: Run test to verify it fails**

Run: `node --import tsx --test src/sensors/object.test.ts`
Expected: FAIL — cannot find module `./object.js`.

- [ ] **Step 7: Implement the sensors**

`src/sensors/motion.ts`:
```ts
import { MotionSensor } from '@camera.ui/sdk';

export class AmcrestMotionSensor extends MotionSensor {
  constructor() {
    super('Amcrest Motion');
  }
}
```

`src/sensors/audio.ts`:
```ts
import { AudioSensor } from '@camera.ui/sdk';

export class AmcrestAudioSensor extends AudioSensor {
  constructor() {
    super('Amcrest Audio');
  }

  report(active: boolean): void {
    this.reportDetections(active);
  }
}
```

`src/sensors/doorbell.ts`:
```ts
import { DoorbellTrigger } from '@camera.ui/sdk';

export class AmcrestDoorbellTrigger extends DoorbellTrigger {
  constructor() {
    super('Amcrest Doorbell');
  }
}
```

`src/sensors/object.ts`:
```ts
import { ObjectSensor } from '@camera.ui/sdk';

import type { TrackedDetection } from '@camera.ui/sdk';

type ObjectCategory = 'person' | 'vehicle';

export class AmcrestObjectSensor extends ObjectSensor {
  private active = new Set<ObjectCategory>();

  constructor() {
    super('Amcrest Object');
  }

  report(category: ObjectCategory, detected: boolean): void {
    if (detected) {
      this.active.add(category);
    } else {
      this.active.delete(category);
    }

    if (this.active.size === 0) {
      this.reportDetections(false);
      return;
    }

    // Amcrest smart events lack usable normalized boxes — synthesize a full-frame detection per category for labels.
    const detections: TrackedDetection[] = Array.from(this.active).map((label) => ({
      label,
      confidence: 1,
      box: { x: 0, y: 0, width: 1, height: 1 },
    }));
    this.reportDetections(true, detections);
  }
}
```

`src/sensors/ptz.ts`:
```ts
import { PTZCapability, PTZControl } from '@camera.ui/sdk';

import type { AmcrestClient } from '../amcrest/api.js';
import type { PTZDirection } from '@camera.ui/sdk';

export class AmcrestPTZSensor extends PTZControl {
  constructor(
    private readonly client: AmcrestClient,
    private readonly channel: number,
    name = 'Amcrest PTZ',
  ) {
    super(name);
  }

  setCapabilities(pan: boolean, tilt: boolean, zoom: boolean): void {
    const caps: PTZCapability[] = [PTZCapability.VelocityControl];
    if (pan) caps.push(PTZCapability.Pan);
    if (tilt) caps.push(PTZCapability.Tilt);
    if (zoom) caps.push(PTZCapability.Zoom);
    this.capabilities = caps;
  }

  override async setVelocity(velocity: PTZDirection | undefined): Promise<void> {
    if (!velocity) return;
    try {
      await this.client.ptz(this.channel, {
        panSpeed: velocity.panSpeed,
        tiltSpeed: velocity.tiltSpeed,
        zoomSpeed: velocity.zoomSpeed,
      });
      await super.setVelocity(velocity);
    } catch {
      // Non-fatal; a failed PTZ command should not crash the camera controller.
    }
  }
}
```

`src/sensors/index.ts`:
```ts
export { AmcrestAudioSensor } from './audio.js';
export { AmcrestDoorbellTrigger } from './doorbell.js';
export { AmcrestMotionSensor } from './motion.js';
export { AmcrestObjectSensor } from './object.js';
export { AmcrestPTZSensor } from './ptz.js';
```

- [ ] **Step 8: Run object test + full suite**

Run: `node --import tsx --test src/sensors/object.test.ts`
Expected: PASS (1 test).
Run: `npm test`
Expected: all suites pass.

> **Note:** If any sensor base-class name (`AudioSensor`, `ObjectSensor`, `DoorbellTrigger`, `PTZControl`, `PTZCapability`, `TrackedDetection`, `PTZDirection`) does not resolve at build time, confirm the exact export against `node_modules/@camera.ui/sdk` (the `camera-ui-onvif`/`camera-ui-eufy` sources use these names) and adjust the imports. Then re-run `npm run build`.

- [ ] **Step 9: Typecheck and commit**

Run: `npm run build`
Expected: `tsc` succeeds.

```bash
git add camera-ui-amcrest/src/sensors camera-ui-amcrest/src/amcrest/event-reader.ts camera-ui-amcrest/src/amcrest/event-reader.test.ts
git commit -m "feat(amcrest): add sensors and event multipart reader"
```

---

### Task 12: Per-camera controller (`camera.ts`) + talkback stream

**Files:**
- Modify: `camera-ui-amcrest/src/amcrest/talkback.ts` (add `AmcrestTalkback` sink)
- Create: `camera-ui-amcrest/src/camera.ts`
- Modify: `camera-ui-amcrest/src/types.ts`

**Interfaces:**
- Consumes: `Relay`, `BackchannelTranscoder`, `RtspServerSink`, `Logger` from `@seydx/rtsp`; `AmcrestClient` (Task 10); sensors (Task 11); `parseAmcrestEvent` (Task 6), `classifyAmcrestEvent` (Task 7), `splitEventMultipart` (Task 11); `selectTalkbackTarget` (Task 9); SDK `CameraDevice`, `DeviceStorage`, `LoggerService`, `StreamingInterface`, `SnapshotInterface`.
- Produces: class `AmcrestCamera` with `initialize(): Promise<void>` and `destroy(): void`. Owns the relay, event loop (with reconnect/backoff), sensor wiring, and talkback lifecycle. Exposes nothing else to the plugin beyond construct/initialize/destroy.

> **This task is verified primarily on real hardware** (streaming, events, talkback, PTZ). The `@seydx/rtsp` relay+backchannel wiring mirrors `camera-ui-eufy/src/camera.ts`; the RTSP-URL source API for `Relay` differs from eufy's P2P source and MUST be confirmed against the installed package before implementing Step 2.

- [ ] **Step 1: Confirm the `@seydx/rtsp` RTSP-source + relay API**

Run: `ls node_modules/@seydx/rtsp/dist` and inspect the type declarations, e.g.:
```bash
sed -n '1,200p' node_modules/@seydx/rtsp/dist/index.d.ts
```
Confirm: how `Relay` accepts an upstream **RTSP URL** source (as opposed to eufy's custom `source` object), the `serveRtsp({ path, backchannel })` signature, the `RtspServerSink` `url` property and `'backchannel'` event, and `BackchannelTranscoder` `{ from, to, output }`. Record the exact source construction to use in Step 3. If `Relay` requires a source object with an RTSP URL, use that; if it accepts `{ url }` directly, use that.

- [ ] **Step 2: Add the `AmcrestTalkback` sink to `talkback.ts`**

Append to `src/amcrest/talkback.ts`:
```ts
import { Writable } from 'node:stream';

import type { AmcrestClient } from './api.js';
import type { TalkbackTarget } from './talkback.js';

// Streams transcoded talkback audio to the camera's audio.cgi endpoint as a chunked POST.
export class AmcrestTalkback extends Writable {
  private started = false;

  constructor(
    private readonly client: AmcrestClient,
    private readonly channel: number,
    private readonly target: TalkbackTarget,
  ) {
    super();
  }

  // The camera controller pipes transcoder output here; the first write opens the POST.
  // Implemented in Step 3 wiring where the POST body is a stream fed by these writes.
}
```
> Because `digestFetch` needs the full request at call time, the actual streaming POST is opened in `camera.ts` (Step 3) using a `PassThrough` as the `body`, and audio chunks are written to that `PassThrough`. `AmcrestTalkback` exists to hold `target`/`channel` context. Keep this class minimal.

- [ ] **Step 3: Implement `camera.ts`**

```ts
import { PassThrough } from 'node:stream';

import { BackchannelTranscoder, Relay } from '@seydx/rtsp';

import { AmcrestClient } from './amcrest/api.js';
import { classifyAmcrestEvent } from './amcrest/classify.js';
import { splitEventMultipart } from './amcrest/event-reader.js';
import { parseAmcrestEvent } from './amcrest/events.js';
import { selectTalkbackTarget } from './amcrest/talkback.js';
import { digestFetch } from './amcrest/digest-auth.js';
import { AmcrestAudioSensor, AmcrestDoorbellTrigger, AmcrestMotionSensor, AmcrestObjectSensor, AmcrestPTZSensor } from './sensors/index.js';

import type { AmcrestCapabilities, AmcrestCameraStorage } from './types.js';
import type { CameraDevice, DeviceStorage, LoggerService, SnapshotInterface, StreamingInterface } from '@camera.ui/sdk';
import type { Logger, RtspServerSink } from '@seydx/rtsp';

const BACKCHANNEL_ADVERTISE = { codec: 'pcm_alaw', payloadType: 8, clockRate: 8000, channels: 1 } as const;
const EVENT_RECONNECT_BASE_MS = 2000;
const EVENT_RECONNECT_MAX_MS = 30000;

class Implementations implements StreamingInterface, SnapshotInterface {
  constructor(private readonly cam: AmcrestCamera) {}
  async streamUrl(): Promise<string> {
    return this.cam.getStreamUrl();
  }
  async snapshot(): Promise<ArrayBuffer | undefined> {
    return this.cam.getSnapshot();
  }
}

export class AmcrestCamera {
  private readonly client: AmcrestClient;
  private readonly storage: DeviceStorage<AmcrestCameraStorage>;
  private readonly log: LoggerService;

  private relay?: Relay;
  private rtspServer?: RtspServerSink;
  private relayLogger?: Logger;
  private transcoder?: BackchannelTranscoder;
  private transcoderStarting?: Promise<void>;
  private talkbackBody?: PassThrough;

  private motion?: AmcrestMotionSensor;
  private object?: AmcrestObjectSensor;
  private audio?: AmcrestAudioSensor;
  private doorbell?: AmcrestDoorbellTrigger;
  private ptz?: AmcrestPTZSensor;

  private eventAbort?: AbortController;
  private eventReconnectStreak = 0;
  private stopped = false;

  constructor(
    private readonly cameraDevice: CameraDevice,
    private readonly capabilities: AmcrestCapabilities,
  ) {
    this.log = cameraDevice.logger;
    this.storage = this.createStorage();
    const v = this.storage.values;
    this.client = new AmcrestClient({ ip: v.ip, username: v.username, password: v.password, port: v.port, httpPort: v.httpPort });
  }

  async initialize(): Promise<void> {
    const v = this.storage.values;
    if (!v.ip || !v.username || !v.password) {
      this.cameraDevice.logger.attention('Please configure the Amcrest connection settings');
      return;
    }

    await this.setupStreaming();
    await this.cameraDevice.implement(new Implementations(this));
    await this.setupSensors();
    this.startEventLoop();
    this.cameraDevice.connect();
  }

  async getStreamUrl(): Promise<string> {
    if (this.rtspServer) return `${this.rtspServer.url}#timeout=30`;
    // Fallback: direct RTSP (no backchannel) if relay unavailable.
    return this.client.rtspUrl(this.channel, 0);
  }

  async getSnapshot(): Promise<ArrayBuffer | undefined> {
    try {
      const buf = await this.client.snapshot(this.channel);
      return buf.buffer.slice(buf.byteOffset, buf.byteOffset + buf.byteLength) as ArrayBuffer;
    } catch (error) {
      this.log.error('Snapshot failed:', error);
      return undefined;
    }
  }

  destroy(): void {
    this.stopped = true;
    this.eventAbort?.abort();
    this.resetTalkback();
    void this.rtspServer?.shutdown();
    void this.relay?.stop();
    this.log.log('Amcrest camera destroyed:', this.cameraDevice.name);
  }

  private get channel(): number {
    return this.storage.values.channel ?? 1;
  }

  private async setupStreaming(): Promise<void> {
    this.relayLogger = this.createRelayLogger();
    // NOTE: exact Relay RTSP-source construction confirmed in Step 1.
    this.relay = new Relay({
      source: { url: this.client.rtspUrl(this.channel, 0) },
      idleTimeout: 30_000,
      stallTimeout: 8_000,
      logger: this.relayLogger,
    });
    this.relay.on('stop', () => this.resetTalkback());
    this.rtspServer = await this.relay.serveRtsp({ path: 'live', backchannel: { ...BACKCHANNEL_ADVERTISE }, sdpTimeout: 30000 });
    this.rtspServer.on('backchannel', (rtp: Buffer) => this.handleTalkbackRtp(rtp));
    this.log.log('Amcrest RTSP relay started');
  }

  private handleTalkbackRtp(rtp: Buffer): void {
    const target = selectTalkbackTarget(this.capabilities.deviceType);
    if (!this.transcoder) {
      this.talkbackBody = new PassThrough();
      this.openTalkbackPost(target.contentType, this.talkbackBody);
      this.transcoder = new BackchannelTranscoder({
        from: { ...BACKCHANNEL_ADVERTISE },
        to: { codec: target.codec, sampleRate: target.sampleRate, channels: 1, format: target.codec === 'aac' ? 'adts' : 'alaw', bitRate: 32000 },
        output: (chunk: Buffer) => this.talkbackBody?.write(chunk),
        logger: this.relayLogger,
      });
      this.transcoderStarting = this.transcoder.start();
    }
    this.transcoderStarting?.then(() => this.transcoder?.push(rtp)).catch((e) => this.log.error('Talkback transcode failed:', e));
  }

  private openTalkbackPost(contentType: string, body: PassThrough): void {
    const url = this.client.urlFor(`/cgi-bin/audio.cgi?action=postAudio&httptype=singlepart&channel=${this.channel}`);
    void digestFetch({
      url,
      username: this.storage.values.username,
      password: this.storage.values.password,
      method: 'POST',
      headers: { 'Content-Type': contentType },
      body: body as unknown as BodyInit,
    }).catch((e) => this.log.error('Talkback POST failed:', e));
  }

  private resetTalkback(): void {
    this.transcoder?.close();
    this.transcoder = undefined;
    this.transcoderStarting = undefined;
    this.talkbackBody?.end();
    this.talkbackBody = undefined;
  }

  private async setupSensors(): Promise<void> {
    this.motion = new AmcrestMotionSensor();
    await this.cameraDevice.addSensor(this.motion);

    this.object = new AmcrestObjectSensor();
    await this.cameraDevice.addSensor(this.object);

    this.audio = new AmcrestAudioSensor();
    await this.cameraDevice.addSensor(this.audio);

    if (this.capabilities.doorbell) {
      this.doorbell = new AmcrestDoorbellTrigger();
      await this.cameraDevice.addSensor(this.doorbell);
    }

    if (this.capabilities.ptz) {
      this.ptz = new AmcrestPTZSensor(this.client, this.channel);
      this.ptz.setCapabilities(this.capabilities.ptzPan, this.capabilities.ptzTilt, this.capabilities.ptzZoom);
      await this.cameraDevice.addSensor(this.ptz);
    }
  }

  private startEventLoop(): void {
    if (this.stopped) return;
    this.eventAbort = new AbortController();
    void this.runEventLoop(this.eventAbort.signal);
  }

  private async runEventLoop(signal: AbortSignal): Promise<void> {
    try {
      const stream = await this.client.attachEvents(signal);
      this.eventReconnectStreak = 0;
      this.log.log('Amcrest event stream connected');
      const decoder = new TextDecoder();
      let buffer = '';
      // Amcrest streams a multipart body; we scan the running buffer for complete boundary blocks.
      for await (const chunk of stream as unknown as AsyncIterable<Uint8Array>) {
        buffer += decoder.decode(chunk, { stream: true });
        const boundary = this.detectBoundary(buffer);
        if (!boundary) continue;
        const blobs = splitEventMultipart(buffer, boundary);
        for (const blob of blobs) this.dispatchEvent(blob);
        // Keep only the trailing partial section after the last boundary marker.
        const lastIdx = buffer.lastIndexOf(`--${boundary}`);
        if (lastIdx > 0) buffer = buffer.substring(lastIdx);
        if (buffer.length > 1_000_000) buffer = '';
      }
    } catch (error) {
      if (signal.aborted || this.stopped) return;
      this.log.debug('Amcrest event stream error:', error);
    }
    if (signal.aborted || this.stopped) return;
    this.eventReconnectStreak++;
    const delay = Math.min(EVENT_RECONNECT_BASE_MS * 2 ** (this.eventReconnectStreak - 1), EVENT_RECONNECT_MAX_MS);
    this.log.debug(`Reconnecting Amcrest event stream in ${delay}ms`);
    setTimeout(() => this.startEventLoop(), delay);
  }

  private detectBoundary(buffer: string): string | undefined {
    const m = /--([A-Za-z0-9'()+_,\-./:=? ]+)\r?\n/.exec(buffer);
    return m ? m[1].trim().replace(/^-+/, '') : undefined;
  }

  private dispatchEvent(blob: string): void {
    const ev = parseAmcrestEvent(blob);
    if (!ev) return;
    const c = classifyAmcrestEvent(ev);
    if (!c) return;
    switch (c.kind) {
      case 'motion':
        this.motion?.reportDetections(c.active);
        break;
      case 'audio':
        this.audio?.report(c.active);
        break;
      case 'object':
        this.object?.report(c.category, c.active);
        break;
      case 'doorbell':
        this.doorbell?.trigger();
        break;
    }
  }

  private createRelayLogger(): Logger {
    return {
      log: (...a: unknown[]) => this.log.log(...a),
      warn: (...a: unknown[]) => this.log.warn(...a),
      error: (...a: unknown[]) => this.log.error(...a),
      debug: (...a: unknown[]) => this.log.debug(...a),
    } as Logger;
  }

  private createStorage(): DeviceStorage<AmcrestCameraStorage> {
    return this.cameraDevice.createStorage<AmcrestCameraStorage>([
      { type: 'string', key: 'ip', title: 'IP Address', description: 'Camera IP address, e.g. 192.168.1.50', store: true, required: true },
      { type: 'string', key: 'username', title: 'Username', description: 'Amcrest account username.', store: true, required: true },
      { type: 'string', format: 'password', key: 'password', title: 'Password', description: 'Amcrest account password.', store: true, required: true },
      { type: 'number', key: 'port', title: 'RTSP Port', description: 'RTSP port (default 554).', store: true, required: false, defaultValue: 554 },
      { type: 'number', key: 'httpPort', title: 'HTTP Port', description: 'HTTP/CGI port (default 80).', store: true, required: false, defaultValue: 80 },
      { type: 'number', key: 'channel', title: 'Channel', description: 'Camera channel (default 1).', store: true, required: false, defaultValue: 1 },
    ]);
  }
}
```

- [ ] **Step 4: Update `src/types.ts`**

```ts
export interface AmcrestCameraStorage {
  ip: string;
  username: string;
  password: string;
  port?: number;
  httpPort?: number;
  channel?: number;
}

export interface AmcrestCapabilities {
  deviceType?: string;
  doorbell: boolean;
  ptz: boolean;
  ptzPan: boolean;
  ptzTilt: boolean;
  ptzZoom: boolean;
}
```

- [ ] **Step 5: Typecheck**

Run: `npm run build`
Expected: `tsc` succeeds. If the `@seydx/rtsp` `Relay` source shape from Step 1 differs from `{ source: { url } }`, adjust `setupStreaming` accordingly and re-run.

- [ ] **Step 6: Commit**

```bash
git add camera-ui-amcrest/src/camera.ts camera-ui-amcrest/src/types.ts camera-ui-amcrest/src/amcrest/talkback.ts
git commit -m "feat(amcrest): add per-camera controller with relay, events and talkback"
```

- [ ] **Step 7: Hardware verification (deferred to plugin load in Task 14)** — recorded here as the acceptance criteria for this task: live view (main), snapshot, motion event, human/vehicle detection, doorbell press, PTZ move, talkback all function on real devices.

---

### Task 13: Dahua discovery

**Files:**
- Create: `camera-ui-amcrest/src/amcrest/discovery.ts`
- Test: `camera-ui-amcrest/src/amcrest/discovery.test.ts`

**Interfaces:**
- Produces:
  - `buildDiscoveryProbe(): Buffer` — the UDP probe payload sent to `239.255.255.251:37810`.
  - `parseDiscoveryResponse(buf: Buffer): DiscoveredAmcrest | undefined` where `DiscoveredAmcrest = { ip: string; mac?: string; deviceType?: string }`.
  - `discover(timeoutMs: number, logger: { debug: (...a: unknown[]) => void }): Promise<DiscoveredAmcrest[]>` — opens a UDP socket, broadcasts the probe, collects responses. All socket errors non-fatal.

> **Protocol note:** Dahua's DHIP discovery frame is a 32-byte little-endian header followed by a JSON body. The exact header bytes MUST be captured from a real device (you have Amcrest hardware) before finalizing `buildDiscoveryProbe`. Steps 1–3 use TDD against a **captured** fixture so the implementation matches your actual devices rather than a guessed format. Discovery is non-fatal: manual add (Task 14) always works even if discovery yields nothing.

- [ ] **Step 1: Capture a real discovery exchange**

On the machine with the cameras, capture the Amcrest mobile app or `IP Config` tool doing a LAN scan (or send a known probe):
```bash
sudo tcpdump -i any -X 'udp port 37810' -w /tmp/amcrest-disco.pcap
```
Open in Wireshark, copy the probe request bytes and one response's bytes as hex. Save them as `camera-ui-amcrest/src/fixtures/discovery-probe.hex` and `discovery-response.hex` (hex string, no spaces). Record the JSON body observed in the response (fields for IP / MAC / deviceType).

- [ ] **Step 2: Write the failing test using the captured fixtures**

```ts
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import { buildDiscoveryProbe, parseDiscoveryResponse } from './discovery.js';

const probeHex = readFileSync(fileURLToPath(new URL('../fixtures/discovery-probe.hex', import.meta.url)), 'utf8').trim();
const responseHex = readFileSync(fileURLToPath(new URL('../fixtures/discovery-response.hex', import.meta.url)), 'utf8').trim();

test('probe matches the captured request bytes', () => {
  assert.equal(buildDiscoveryProbe().toString('hex'), probeHex);
});

test('parses ip/mac/deviceType from a captured response', () => {
  const parsed = parseDiscoveryResponse(Buffer.from(responseHex, 'hex'));
  assert.ok(parsed);
  assert.match(parsed!.ip, /^\d+\.\d+\.\d+\.\d+$/);
});
```

- [ ] **Step 3: Implement `discovery.ts` to match the captured bytes**

Implement `buildDiscoveryProbe` to reproduce the captured probe (DHIP: 32-byte header — bytes `0..1` = `0x20 0x00`, byte `4` onward = magic `DHIP` where applicable — plus the JSON body `{"method":"DHDiscover.search","params":{"mac":"","uni":1}}`; adjust header fields to match the capture exactly). Implement `parseDiscoveryResponse` to locate the JSON body (`{ ... }`) in the datagram, `JSON.parse` it, and extract IP/MAC/deviceType from the observed field names. Then:

```ts
import { createSocket } from 'node:dgram';

export interface DiscoveredAmcrest {
  ip: string;
  mac?: string;
  deviceType?: string;
}

const MCAST_ADDR = '239.255.255.251';
const MCAST_PORT = 37810;

export function buildDiscoveryProbe(): Buffer {
  const body = Buffer.from(JSON.stringify({ method: 'DHDiscover.search', params: { mac: '', uni: 1 } }), 'utf8');
  const header = Buffer.alloc(32);
  header.writeUInt8(0x20, 0);
  header.writeUInt8(0x00, 1);
  header.writeUInt32LE(body.length, 4);
  header.writeUInt32LE(body.length, 12);
  // NOTE: adjust the above header fields to exactly match the captured probe from Step 1.
  return Buffer.concat([header, body]);
}

export function parseDiscoveryResponse(buf: Buffer): DiscoveredAmcrest | undefined {
  const start = buf.indexOf(0x7b); // '{'
  const end = buf.lastIndexOf(0x7d); // '}'
  if (start === -1 || end === -1 || end <= start) return undefined;
  try {
    const json = JSON.parse(buf.subarray(start, end + 1).toString('utf8')) as {
      params?: { deviceInfo?: { IPv4Address?: { IPAddress?: string }; DeviceType?: string; PhysicalAddress?: string } };
    };
    const info = json.params?.deviceInfo;
    const ip = info?.IPv4Address?.IPAddress;
    if (!ip) return undefined;
    return { ip, mac: info?.PhysicalAddress, deviceType: info?.DeviceType };
  } catch {
    return undefined;
  }
}

export async function discover(timeoutMs: number, logger: { debug: (...a: unknown[]) => void }): Promise<DiscoveredAmcrest[]> {
  return new Promise((resolvePromise) => {
    const found = new Map<string, DiscoveredAmcrest>();
    const socket = createSocket({ type: 'udp4', reuseAddr: true });

    const finish = () => {
      try {
        socket.close();
      } catch {
        // ignore
      }
      resolvePromise(Array.from(found.values()));
    };

    socket.on('error', (err) => {
      logger.debug('Amcrest discovery socket error:', err);
      finish();
    });

    socket.on('message', (msg) => {
      const parsed = parseDiscoveryResponse(msg);
      if (parsed) found.set(parsed.ip, parsed);
    });

    socket.bind(() => {
      try {
        socket.setBroadcast(true);
        const probe = buildDiscoveryProbe();
        socket.send(probe, MCAST_PORT, MCAST_ADDR);
        socket.send(probe, MCAST_PORT, '255.255.255.255');
      } catch (err) {
        logger.debug('Amcrest discovery send failed:', err);
      }
      setTimeout(finish, timeoutMs);
    });
  });
}
```
Adjust the header bytes and JSON field paths so the two tests pass against the captured fixtures.

- [ ] **Step 4: Run test to verify it passes**

Run: `node --import tsx --test src/amcrest/discovery.test.ts`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add camera-ui-amcrest/src/amcrest/discovery.ts camera-ui-amcrest/src/amcrest/discovery.test.ts camera-ui-amcrest/src/fixtures/discovery-probe.hex camera-ui-amcrest/src/fixtures/discovery-response.hex
git commit -m "feat(amcrest): add Dahua UDP discovery"
```

---

### Task 14: Plugin entry (`index.ts`) — discovery, manual add, adopt, lifecycle

**Files:**
- Modify: `camera-ui-amcrest/src/index.ts`
- Create: `camera-ui-amcrest/src/adopt.ts` (pure `buildCameraConfig`)
- Test: `camera-ui-amcrest/src/adopt.test.ts`

**Interfaces:**
- Consumes: `BasePlugin`, `API_EVENT`, `CameraConfig`, `CameraDevice`, `DiscoveredCamera`, `DiscoveryProvider`, `JsonSchemaWithoutCallbacks` from `@camera.ui/sdk`; `AmcrestClient` (Task 10); `discover` (Task 13); `AmcrestCamera` (Task 12); `AmcrestStream` (Task 5).
- Produces:
  - `buildCameraConfig(input): CameraConfig` (pure, unit-tested) — turns device info + streams + adopt settings into a `CameraConfig` with `sources` (main/sub RTSP via `buildRtspUrl`, snapshot marked on main).
  - `AmcrestPlugin` implementing `DiscoveryProvider`: `onDiscoverCameras`, `onGetCameraSettings`, `onAdoptCamera`, `configureCameras`, `onCameraAdded`, `onCameraReleased`, shutdown.

- [ ] **Step 1: Write the failing test for `buildCameraConfig`**

```ts
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { buildCameraConfig } from './adopt.js';

test('builds a config with main+sub sources and snapshot on main', () => {
  const config = buildCameraConfig({
    name: 'Front Door',
    nativeId: 'amcrest-192.168.1.50',
    ip: '192.168.1.50',
    username: 'admin',
    password: 'pw',
    port: 554,
    channel: 1,
    info: { manufacturer: 'Amcrest', model: 'AD410', serialNumber: 'ABC', firmwareVersion: '1.0' },
    streams: [
      { role: 'main', subtype: 0, codec: 'h264', width: 1920, height: 1080 },
      { role: 'sub', subtype: 1, codec: 'h265', width: 704, height: 480 },
    ],
  });

  assert.equal(config.name, 'Front Door');
  assert.equal(config.sources.length, 2);
  assert.equal(config.sources[0].role, 'high-resolution');
  assert.equal(config.sources[0].useForSnapshot, true);
  assert.ok(config.sources[0].urls[0].startsWith('rtsp://admin:pw@192.168.1.50:554/cam/realmonitor?channel=1&subtype=0'));
  assert.equal(config.sources[1].role, 'low-resolution');
  assert.equal(config.sources[1].useForSnapshot, false);
});

test('falls back to a single main source when only one stream is present', () => {
  const config = buildCameraConfig({
    name: 'Cam', nativeId: 'x', ip: '10.0.0.1', username: 'a', password: 'b', port: 554, channel: 1,
    info: {}, streams: [{ role: 'main', subtype: 0, codec: 'h264', width: 1920, height: 1080 }],
  });
  assert.equal(config.sources.length, 1);
  assert.equal(config.sources[0].useForSnapshot, true);
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `node --import tsx --test src/adopt.test.ts`
Expected: FAIL — cannot find module `./adopt.js`.

- [ ] **Step 3: Implement `adopt.ts`**

```ts
import { buildRtspUrl } from './amcrest/rtsp-url.js';

import type { AmcrestStream } from './amcrest/encode-config.js';
import type { CameraConfig } from '@camera.ui/sdk';

export interface BuildCameraConfigInput {
  name: string;
  nativeId: string;
  ip: string;
  username: string;
  password: string;
  port: number;
  channel: number;
  info: { manufacturer?: string; model?: string; serialNumber?: string; firmwareVersion?: string };
  streams: AmcrestStream[];
}

export function buildCameraConfig(input: BuildCameraConfigInput): CameraConfig {
  const main = input.streams.find((s) => s.role === 'main') ?? input.streams[0];
  const sub = input.streams.find((s) => s.role === 'sub');

  const sources: CameraConfig['sources'] = [];
  if (main) {
    sources.push({
      name: 'main',
      role: 'high-resolution',
      urls: [buildRtspUrl({ ip: input.ip, username: input.username, password: input.password, port: input.port, channel: input.channel, subtype: main.subtype })],
      useForSnapshot: true,
      hotMode: true,
      preload: true,
    });
  }
  if (sub) {
    sources.push({
      name: 'sub',
      role: 'low-resolution',
      urls: [buildRtspUrl({ ip: input.ip, username: input.username, password: input.password, port: input.port, channel: input.channel, subtype: sub.subtype })],
      useForSnapshot: false,
      hotMode: false,
      preload: false,
    });
  }

  return {
    name: input.name,
    nativeId: input.nativeId,
    info: {
      manufacturer: input.info.manufacturer,
      model: input.info.model,
      serialNumber: input.info.serialNumber,
      firmwareVersion: input.info.firmwareVersion,
    },
    sources,
  };
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `node --import tsx --test src/adopt.test.ts`
Expected: PASS (2 tests).

- [ ] **Step 5: Implement `index.ts`**

```ts
import { API_EVENT, BasePlugin } from '@camera.ui/sdk';

import { buildCameraConfig } from './adopt.js';
import { AmcrestClient } from './amcrest/api.js';
import { discover } from './amcrest/discovery.js';
import { AmcrestCamera } from './camera.js';

import type { AmcrestCapabilities, AmcrestCameraStorage } from './types.js';
import type {
  CameraConfig,
  CameraDevice,
  DeviceStorage,
  DiscoveredCamera,
  DiscoveryProvider,
  JsonSchemaWithoutCallbacks,
  LoggerService,
  PluginAPI,
} from '@camera.ui/sdk';

const DISCOVERY_TIMEOUT_MS = 5000;

export default class AmcrestPlugin extends BasePlugin implements DiscoveryProvider {
  private cameras = new Map<string, AmcrestCamera>();
  private existing = new Map<string, CameraDevice>();

  constructor(logger: LoggerService, api: PluginAPI, storage: DeviceStorage<unknown>) {
    super(logger, api, storage);
    this.api.on(API_EVENT.SHUTDOWN, this.stop.bind(this));
  }

  async configureCameras(cameras: CameraDevice[]): Promise<void> {
    for (const camera of cameras) {
      this.existing.set(camera.id, camera);
      await this.initCamera(camera);
    }
  }

  async onCameraAdded(camera: CameraDevice): Promise<void> {
    this.existing.set(camera.id, camera);
    await this.initCamera(camera);
  }

  async onCameraReleased(cameraId: string): Promise<void> {
    this.cameras.get(cameraId)?.destroy();
    this.cameras.delete(cameraId);
    this.existing.delete(cameraId);
  }

  async onDiscoverCameras(): Promise<DiscoveredCamera[]> {
    const devices = await discover(DISCOVERY_TIMEOUT_MS, { debug: (...a) => this.logger.debug(...a) });
    return devices
      .filter((d) => !Array.from(this.existing.values()).some((c) => c.nativeId === `amcrest-${d.ip}`))
      .map((d) => ({ id: `amcrest-${d.ip}`, name: d.deviceType ? `Amcrest ${d.deviceType}` : `Amcrest (${d.ip})`, model: d.deviceType, address: d.ip }));
  }

  async onGetCameraSettings(_camera: DiscoveredCamera): Promise<JsonSchemaWithoutCallbacks[]> {
    return [
      { type: 'string', key: 'ip', title: 'IP Address', description: 'Camera IP address, e.g. 192.168.1.50', required: true },
      { type: 'string', key: 'username', title: 'Username', description: 'Amcrest account username.', required: true },
      { type: 'string', format: 'password', key: 'password', title: 'Password', description: 'Amcrest account password.', required: true },
      { type: 'number', key: 'channel', title: 'Channel', description: 'Camera channel (default 1).', required: false, defaultValue: 1 },
    ];
  }

  async onAdoptCamera(camera: DiscoveredCamera, settings: Record<string, unknown>): Promise<CameraConfig> {
    const ip = (settings.ip as string) || camera.address;
    const username = settings.username as string;
    const password = settings.password as string;
    const channel = (settings.channel as number) || 1;
    if (!ip || !username || !password) {
      throw new Error('IP address, username and password are required');
    }

    const client = new AmcrestClient({ ip, username, password });
    const info = await client.getSystemInfo(); // throws 'not amcrest' if wrong device
    const streams = await client.getStreams(channel);
    if (streams.length === 0) {
      throw new Error('No enabled video streams found on the device');
    }

    const name = camera.name || (info.deviceType ? `Amcrest ${info.deviceType}` : `Amcrest (${ip})`);
    const config = buildCameraConfig({
      name,
      nativeId: `amcrest-${ip}`,
      ip,
      username,
      password,
      port: 554,
      channel,
      info: { manufacturer: 'Amcrest', model: info.deviceType, serialNumber: info.serialNumber, firmwareVersion: info.hardwareVersion },
      streams,
    });
    this.logger.log(`Amcrest device adopted: ${name} (${streams.length} stream(s))`);
    return config;
  }

  private async initCamera(camera: CameraDevice): Promise<void> {
    const capabilities = await this.detectCapabilities(camera);
    const controller = new AmcrestCamera(camera, capabilities);
    this.cameras.set(camera.id, controller);
    await controller.initialize();
  }

  private async detectCapabilities(camera: CameraDevice): Promise<AmcrestCapabilities> {
    const storage = camera.createStorage<AmcrestCameraStorage>([]);
    const v = storage.values;
    const caps: AmcrestCapabilities = { deviceType: undefined, doorbell: false, ptz: false, ptzPan: false, ptzTilt: false, ptzZoom: false };
    if (!v.ip || !v.username || !v.password) return caps;

    const client = new AmcrestClient({ ip: v.ip, username: v.username, password: v.password, port: v.port, httpPort: v.httpPort });
    try {
      const info = await client.getSystemInfo();
      caps.deviceType = info.deviceType;
      const dt = (info.deviceType ?? '').toUpperCase();
      caps.doorbell = dt.startsWith('AD') || dt.includes('DB') || dt.includes('VTO');
    } catch (error) {
      this.logger.debug('Capability detection (system info) failed:', error);
    }
    try {
      // ptz.cgi getCurrentProtocolCaps returns an error on non-PTZ devices.
      const res = await client['fetch' as never]; // placeholder guard; real probe below
      void res;
    } catch {
      // ignore
    }
    try {
      const probe = await fetchPtzCaps(client, v.channel ?? 1);
      caps.ptz = probe.ptz;
      caps.ptzPan = probe.pan;
      caps.ptzTilt = probe.tilt;
      caps.ptzZoom = probe.zoom;
    } catch (error) {
      this.logger.debug('PTZ capability probe failed:', error);
    }
    return caps;
  }

  private async stop(): Promise<void> {
    for (const c of this.cameras.values()) c.destroy();
    this.cameras.clear();
  }
}

async function fetchPtzCaps(client: AmcrestClient, channel: number): Promise<{ ptz: boolean; pan: boolean; tilt: boolean; zoom: boolean }> {
  const res = await fetch(client.urlFor(`/cgi-bin/ptz.cgi?action=getCurrentProtocolCaps&channel=${channel}`)).catch(() => undefined);
  // Non-authed probe may 401; treat presence of caps.PTZ or a 401 challenge as "device answered".
  if (!res) return { ptz: false, pan: false, tilt: false, zoom: false };
  const text = res.status === 401 ? '' : await res.text().catch(() => '');
  const hasPanTilt = /Left|Right|Up|Down/i.test(text);
  const hasZoom = /Zoom/i.test(text);
  const ptz = hasPanTilt || hasZoom;
  return { ptz, pan: hasPanTilt, tilt: hasPanTilt, zoom: hasZoom };
}
```

> **Cleanup note for the implementer:** delete the `res = await client['fetch' as never]` placeholder guard block — it exists only to flag that PTZ detection should go through a real request. Use only the `fetchPtzCaps` helper. Then confirm `camera.createStorage([])` returns persisted values for capability detection; if the SDK requires the full schema to read values, move `detectCapabilities` to read from the controller's storage instead (the controller already builds the schema). Re-run `npm run build` after cleanup.

- [ ] **Step 6: Typecheck + full suite**

Run: `npm run build`
Expected: `tsc` succeeds after the cleanup note is addressed.
Run: `npm test`
Expected: all unit suites pass.

- [ ] **Step 7: Commit**

```bash
git add camera-ui-amcrest/src/index.ts camera-ui-amcrest/src/adopt.ts camera-ui-amcrest/src/adopt.test.ts
git commit -m "feat(amcrest): add plugin entry with discovery, adopt and lifecycle"
```

---

### Task 15: Real-hardware verification pass

**Files:** none (verification only). Use `npm run bundle:dev` and load the plugin into a local camera.ui instance.

**Interfaces:** none.

- [ ] **Step 1: Build the dev bundle**

Run: `cd camera-ui-amcrest && npm run bundle:dev`
Expected: bundle produced under `bundle/`.

- [ ] **Step 2: Verify each feature on real hardware and check the box**

Load the plugin in camera.ui, add each device, and confirm:
- [ ] Manual add of the standard IP camera succeeds; live view (main stream) plays.
- [ ] Sub-stream selectable and plays.
- [ ] Snapshot renders.
- [ ] Motion event triggers the Motion sensor.
- [ ] Human/vehicle detection triggers the Object sensor with the right label.
- [ ] Audio event triggers the Audio sensor (if enabled on device).
- [ ] Doorbell press (Amcrest doorbell) triggers the Doorbell sensor.
- [ ] Talkback to the Amcrest doorbell produces audio at the device (AAC path).
- [ ] PTZ camera pan/tilt/zoom move via the PTZ control.
- [ ] Dahua UDP discovery lists at least one device on the LAN.
- [ ] Plugin reload / camera release cleanly tears down relay + event stream (no leaked sockets in logs).

- [ ] **Step 3: Fix any defects found**

For each failing checkbox, debug using `superpowers:systematic-debugging`, fix, re-run the relevant unit test(s) and the hardware check, and commit with a `fix(amcrest): ...` message referencing the symptom.

---

### Task 16: Packaging, docs, and catalog registration

**Files:**
- Create: `camera-ui-amcrest/README.md`, `camera-ui-amcrest/CHANGELOG.md`, `camera-ui-amcrest/logo.png`
- Modify: `catalog.json` (repo root)

**Interfaces:** none.

- [ ] **Step 1: Write `README.md`**

Cover: supported devices (standard cameras, Amcrest doorbells, PTZ), features (live view, snapshot, two-way audio, motion/object/audio/doorbell events, PTZ, discovery), setup (manual add fields + discovery), the admin-credential note for doorbells (from the scrypted README), and the v2 deferred list. Model structure on `camera-ui-onvif/README.md`.

- [ ] **Step 2: Write `CHANGELOG.md`**

```markdown
# Changelog

## 0.0.1

- Initial release: Amcrest / Dahua-compatible cameras, doorbells and PTZ.
- Live streaming and snapshots via native RTSP + CGI.
- Two-way audio (Amcrest doorbell AAC path).
- Motion, object (person/vehicle), audio and doorbell events via the native event stream.
- PTZ control.
- Dahua UDP discovery and manual add.
```

- [ ] **Step 3: Add `logo.png`**

Add an Amcrest logo PNG (same dimensions as sibling `logo.png` files). If unavailable, use a neutral placeholder and note it for later replacement.

- [ ] **Step 4: Register in `catalog.json`**

Add, in alphabetical position:
```json
"@camera.ui/camera-ui-amcrest": {
  "displayName": "Amcrest",
  "category": "camera-source",
  "featured": false,
  "tagline": "Integrates Amcrest and Dahua-compatible cameras, doorbells and PTZ into camera.ui with discovery, live streaming, two-way audio, PTZ, and motion, object, audio and doorbell events.",
  "logo": "https://raw.githubusercontent.com/cameraui/plugins/main/camera-ui-amcrest/logo.png",
  "screenshots": []
}
```

- [ ] **Step 5: Final production bundle**

Run: `cd camera-ui-amcrest && npm run bundle`
Expected: format + lint + tests + build + `cui bundle` all succeed.

- [ ] **Step 6: Commit**

```bash
git add camera-ui-amcrest/README.md camera-ui-amcrest/CHANGELOG.md camera-ui-amcrest/logo.png catalog.json
git commit -m "docs(amcrest): add README, changelog, logo and catalog entry"
```

---

## Self-Review

**1. Spec coverage:**
- Standard cameras / doorbells / PTZ → Tasks 10–14, capability gating in 12/14. ✓
- Manual add + Dahua discovery → Tasks 13, 14. ✓
- Live view + snapshot → Task 12 (`getStreamUrl`, `getSnapshot`), adopt sources Task 14. ✓
- Two-way audio (AAC tested, G.711A best-effort) → Tasks 9, 12. ✓
- Events → sensors (motion/object/audio/doorbell) → Tasks 6, 7, 11, 12. ✓
- PTZ → Tasks 8, 11, 12. ✓
- Digest auth → Task 3. ✓
- Error handling (auth attention, event reconnect/backoff, non-fatal discovery, capability gating) → Tasks 3, 12, 13, 14. ✓
- Testing (unit fixtures + hardware) → unit tests throughout; hardware Task 15. ✓
- v2 deferrals (NVR, siren, recording config, lock, ONVIF backchannel) → documented, not implemented. ✓

**2. Placeholder scan:** The only intentional "confirm against installed package/hardware" steps are: `@seydx/rtsp` source API (Task 12 Step 1), Dahua packet bytes (Task 13, TDD against captured fixture), SDK export names (Task 11 note), and the PTZ-detection cleanup note (Task 14 Step 5). These are explicit verification steps with concrete commands, not vague placeholders — appropriate given the SDK/third-party packages are not installable at plan-writing time and the discovery protocol must match real devices.

**3. Type consistency:** `AmcrestClient`, `AmcrestStream`, `AmcrestEvent`, `AmcrestClassification`, `AmcrestCapabilities`, `AmcrestCameraStorage`, `buildRtspUrl`, `buildDigestAuthHeader`, `parseSystemInfo`, `parseEncodeConfig`, `parseAmcrestEvent`, `classifyAmcrestEvent`, `ptzCommandForVelocity`, `selectTalkbackTarget`, `splitEventMultipart`, `buildCameraConfig`, `discover` — names are consistent across producing and consuming tasks.
