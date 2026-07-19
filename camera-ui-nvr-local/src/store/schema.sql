-- Schema for the camera-ui-nvr-local plugin's embedded SQLite database.
--
-- Applied once by store.migrate and tracked via PRAGMA user_version (see
-- db.go). Every statement is written defensively with IF NOT EXISTS so that
-- re-running this file against an already-migrated database is a no-op,
-- even if the user_version bookkeeping were ever bypassed.
--
-- Vector search (face_embeddings, clip_embeddings) is NOT backed by the
-- sqlite-vec virtual table extension here — see vector.go for why (the
-- currently maintained ncruces/go-sqlite3 release is API-incompatible with
-- the last published sqlite-vec WASM bindings). Both tables are plain
-- tables storing the embedding as a BLOB of little-endian float32s; nearest
-- neighbor search is done in Go by bruteForceVectorBackend.

CREATE TABLE IF NOT EXISTS cameras (
  id TEXT PRIMARY KEY,
  config JSON,
  updated_ms INTEGER
);

CREATE TABLE IF NOT EXISTS segments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  camera_id TEXT,
  role TEXT,
  path TEXT,
  start_ms INTEGER,
  end_ms INTEGER,
  has_video INTEGER,
  has_audio INTEGER,
  codec TEXT
);

CREATE INDEX IF NOT EXISTS idx_segments_camera_role_start
  ON segments (camera_id, role, start_ms);

CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY,
  camera_id TEXT,
  ts_ms INTEGER,
  end_ms INTEGER,
  types JSON,
  label TEXT,
  confidence REAL,
  box JSON,
  thumb_ref TEXT,
  has_recording INTEGER,
  raw JSON
);

CREATE INDEX IF NOT EXISTS idx_events_camera_ts
  ON events (camera_id, ts_ms);

CREATE TABLE IF NOT EXISTS system_events (
  id TEXT PRIMARY KEY,
  camera_id TEXT,
  ts_ms INTEGER,
  type TEXT,
  severity TEXT,
  message TEXT,
  duration_ms INTEGER
);

CREATE TABLE IF NOT EXISTS faces (
  name TEXT PRIMARY KEY,
  created_ms INTEGER,
  updated_ms INTEGER,
  thumbnail BLOB
);

CREATE TABLE IF NOT EXISTS face_images (
  id TEXT PRIMARY KEY,
  name TEXT,
  jpeg BLOB,
  confidence REAL
);

CREATE TABLE IF NOT EXISTS unknown_faces (
  id TEXT PRIMARY KEY,
  camera_id TEXT,
  event_id TEXT,
  ts_ms INTEGER,
  jpeg BLOB,
  cluster_id TEXT
);

-- Vector tables (brute-force fallback backend; see vector.go).
-- Keyed to face_images.id and events.id respectively.

CREATE TABLE IF NOT EXISTS face_embeddings (
  id TEXT PRIMARY KEY,
  embedding BLOB NOT NULL,
  dim INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS clip_embeddings (
  id TEXT PRIMARY KEY,
  embedding BLOB NOT NULL,
  dim INTEGER NOT NULL
);
