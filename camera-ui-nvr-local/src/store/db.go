// Package store owns the camera-ui-nvr-local plugin's embedded SQLite
// database: schema bootstrap, versioned migrations, and the vector-search
// abstraction used for face/CLIP embeddings.
//
// SQLite driver: this package uses github.com/ncruces/go-sqlite3, a pure-Go
// (WASM, via wazero) SQLite build. It was chosen specifically because it
// requires no CGo, matching this plugin's build matrix (cameraui.config.ts
// sets go.cgoEnabled: '0' across all cross-compile targets, including
// windows/arm64 and linux musl, where a CGo dependency like mattn/go-sqlite3
// would not cross-compile cleanly). Higher-level stores (segments, events,
// faces, ...) are added in later tasks on top of the *DB returned by Open.
package store

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/ncruces/go-sqlite3"
)

// schemaSQL is the full set of DDL statements applied on migration. Every
// statement is IF NOT EXISTS so re-applying it is a safe no-op.
//
//go:embed schema.sql
var schemaSQL string

// schemaVersion is the target PRAGMA user_version. Bump it and extend
// migrate (or add a new embedded file switched on the current version) when
// schema.sql changes in a later task.
//
// Version history:
//   - 1: initial schema (Task 3).
//   - 2: segments.referenced column (Task 8) — see migrateToV2 and
//     schema.sql's segments table doc comment.
const schemaVersion = 2

// dbFileName is the SQLite database file created inside the directory
// passed to Open.
const dbFileName = "nvr.db"

// DB wraps a single connection to the plugin's SQLite database plus the
// vector-search backends used for face and CLIP embeddings.
//
// # Locking (Task 9 review fix)
//
// go-sqlite3's *sqlite3.Conn (a pure-Go/WASM build driven through wazero) is
// documented as not safe for concurrent use by multiple goroutines — not
// just "concurrent writes might conflict", but literally unsafe to have two
// goroutines calling into the same *sqlite3.Conn's methods at the same
// time, since every call runs the shared WASM VM's linear memory/stack.
// This plugin has exactly one *sqlite3.Conn per *DB (Open below), shared by
// every store built on top of it: SegmentStore, EventStore, and every
// VectorBackend (FaceVectors/ClipVectors) — and each of those is driven
// from its own goroutines in production (Task 7's recorder goroutines index
// segments; Task 5's detection-event callbacks upsert events; Task 9's
// retention ticker deletes from all three; RPC handlers read from yet
// another goroutine).
//
// An earlier version of this file gave SegmentStore its own private
// sync.Mutex, on the theory that serializing SegmentStore's own methods
// against each other was enough. It wasn't: two different stores (say,
// EventStore.Upsert from an ingestion goroutine and SegmentStore.
// DeleteOlderThan from the retention ticker) could still call into the same
// underlying conn at the same time, since each store's mutex only ever
// guarded that one store's own call sites. Reviewer reproduced this as an
// actual `panic: slice bounds out of range` inside the sqlite WASM VM under
// -race with EventStore/SegmentStore/VectorBackend operations running
// concurrently — not a hypothetical.
//
// The fix: exactly one lock, on the connection itself (DB.mu), and every
// store operation that touches conn — regardless of which store it's a
// method on — acquires it via Lock/Unlock (or the withConn helper) for the
// full duration of that operation, including any multi-statement
// transaction (BEGIN/.../COMMIT). SegmentStore's old per-store mutex is
// gone; there is now exactly one lock guarding this connection, not two
// independent ones that could each believe they had exclusive access.
type DB struct {
	conn *sqlite3.Conn

	// mu serializes every access to conn across every store built on this
	// DB (SegmentStore, EventStore, VectorBackend, and any future store) —
	// see the type doc comment above for why a single connection-level lock
	// is required instead of one per store.
	mu sync.Mutex

	// FaceVectors and ClipVectors implement VectorBackend for the
	// face_embeddings and clip_embeddings tables respectively. See
	// vector.go for which backend is active and why.
	FaceVectors VectorBackend
	ClipVectors VectorBackend
}

// Open creates (if needed) and opens the NVR SQLite database inside dir,
// applying any pending schema migrations before returning. dir must already
// exist; Open does not create it.
func Open(dir string) (*DB, error) {
	path := filepath.Join(dir, dbFileName)

	conn, err := sqlite3.Open(path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// WAL improves concurrent read performance for the recording/query
	// access pattern later tasks introduce; foreign_keys enforces the
	// face_images/events references declared in schema.sql.
	if err := conn.Exec("PRAGMA journal_mode=WAL"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("store: set journal_mode: %w", err)
	}
	if err := conn.Exec("PRAGMA foreign_keys=ON"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("store: set foreign_keys: %w", err)
	}

	// No DB.Lock() needed for this call: migrate runs here, before Open has
	// returned a *DB to anything else, so by construction no other
	// goroutine can be touching conn yet.
	if err := migrate(conn); err != nil {
		conn.Close()
		return nil, err
	}

	db := &DB{conn: conn}
	// FaceVectors/ClipVectors are constructed after db exists (rather than
	// inline in the composite literal above) because bruteForceVectorBackend
	// now holds a *DB — so it can go through db.Lock()/Unlock() like every
	// other store — not a bare *sqlite3.Conn.
	db.FaceVectors = newBruteForceVectorBackend(db, "face_embeddings")
	db.ClipVectors = newBruteForceVectorBackend(db, "clip_embeddings")
	return db, nil
}

// Conn exposes the underlying connection for lower-level access by stores
// (segment/event/face stores, vector backends). Callers MUST hold the lock
// (Lock/Unlock, or withConn) for as long as they use the returned *Conn —
// this method does not lock on its own, since callers typically need the
// lock held across several conn calls (Prepare/Bind/Step/...), not just
// this one accessor.
func (db *DB) Conn() *sqlite3.Conn { return db.conn }

// Lock acquires the single mutex guarding this DB's shared connection.
// Every store operation that touches conn must call Lock (directly, or via
// withConn) before its first conn access and Unlock after its last —
// including across an entire multi-statement transaction — so that no two
// goroutines, regardless of which store they're calling through, are ever
// inside the underlying *sqlite3.Conn at the same time. See the DB type doc
// comment for why this must be connection-scoped rather than per-store.
func (db *DB) Lock() { db.mu.Lock() }

// Unlock releases the lock acquired by Lock.
func (db *DB) Unlock() { db.mu.Unlock() }

// withConn locks db, calls fn with the connection, and unlocks — a
// convenience wrapper for the common "one self-contained operation" case.
// Callers whose operation needs finer-grained control over exactly when the
// lock is released (e.g. SegmentStore.DeleteOlderThan's helper split across
// pathsOlderThan + the DELETE itself) call Lock/Unlock directly instead;
// both routes serialize on the exact same db.mu.
func (db *DB) withConn(fn func(conn *sqlite3.Conn) error) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return fn(db.conn)
}

// Close releases the underlying SQLite connection.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.conn.Close()
}

// migrate brings the database from whatever PRAGMA user_version it's
// currently at up to schemaVersion, applying each step in order inside a
// single transaction, so repeated calls to Open against the same file are
// idempotent and cheap (a single PRAGMA read) once fully migrated.
//
// Step 0->1 (current < 1) runs the full embedded schemaSQL: every statement
// in it is CREATE TABLE/INDEX IF NOT EXISTS, and schemaSQL as embedded today
// already includes everything through the latest version (e.g. the
// segments.referenced column added for version 2) — so a genuinely fresh
// database goes straight from 0 to schemaVersion in this one step, and
// migrateToV2 below (current < 2) then finds the column already present via
// hasColumn and no-ops.
//
// Step 1->2 (current < 2, migrateToV2) exists specifically for databases
// that were already migrated to version 1 by an earlier build, before
// schema.sql grew the segments.referenced column: re-running schemaSQL's
// `CREATE TABLE IF NOT EXISTS segments (...)` against such a database is a
// no-op (the table already exists) and would silently leave the column
// missing, so that step uses an explicit ALTER TABLE instead. This is the
// first version bump since schemaVersion was introduced (Task 3), so this
// is also the first incremental (non-schemaSQL) migration step in this
// function — later version bumps should follow the same "current < N" step
// pattern rather than assuming a fresh install.
func migrate(conn *sqlite3.Conn) error {
	current, err := userVersion(conn)
	if err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}
	if current >= schemaVersion {
		return nil
	}

	if err := conn.Exec("BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: begin migration: %w", err)
	}

	if current < 1 {
		if err := conn.Exec(schemaSQL); err != nil {
			_ = conn.Exec("ROLLBACK")
			return fmt.Errorf("store: apply schema: %w", err)
		}
	}
	if current < 2 {
		if err := migrateToV2(conn); err != nil {
			_ = conn.Exec("ROLLBACK")
			return fmt.Errorf("store: migrate to v2: %w", err)
		}
	}

	if err := conn.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		_ = conn.Exec("ROLLBACK")
		return fmt.Errorf("store: set user_version: %w", err)
	}
	if err := conn.Exec("COMMIT"); err != nil {
		return fmt.Errorf("store: commit migration: %w", err)
	}
	return nil
}

// migrateToV2 adds the segments.referenced column (Task 8: event-mode
// recording's retain/discard mechanism — see schema.sql's doc comment on the
// column) to a database that doesn't have it yet. DEFAULT 1 means every
// pre-existing row (recorded before this column existed, i.e. under
// continuous-mode recording only, since events mode didn't exist either) is
// retroactively treated as referenced/retained — none of it is newly
// eligible for the events-mode janitor's deletion just because this column
// was added. Guarded by hasColumn so it's a no-op when schemaSQL (a fresh
// install going straight from version 0) already created the column
// directly.
func migrateToV2(conn *sqlite3.Conn) error {
	has, err := hasColumn(conn, "segments", "referenced")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	return conn.Exec("ALTER TABLE segments ADD COLUMN referenced INTEGER NOT NULL DEFAULT 1")
}

// hasColumn reports whether table has a column named column, via PRAGMA
// table_info (the standard SQLite way to introspect a table's columns
// without depending on sqlite_master's raw CREATE TABLE SQL text).
func hasColumn(conn *sqlite3.Conn, table, column string) (bool, error) {
	stmt, _, err := conn.Prepare(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("store: prepare table_info(%s): %w", table, err)
	}
	defer stmt.Close()

	for stmt.Step() {
		// PRAGMA table_info columns: cid, name, type, notnull, dflt_value, pk.
		if stmt.ColumnText(1) == column {
			return true, nil
		}
	}
	if err := stmt.Err(); err != nil {
		return false, fmt.Errorf("store: scan table_info(%s): %w", table, err)
	}
	return false, nil
}

// userVersion reads the database's current PRAGMA user_version.
func userVersion(conn *sqlite3.Conn) (int, error) {
	stmt, _, err := conn.Prepare("PRAGMA user_version")
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	if !stmt.Step() {
		if err := stmt.Err(); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("store: PRAGMA user_version returned no row")
	}
	return stmt.ColumnInt(0), nil
}

// hasTable reports whether tbl exists as a table in the database. It is a
// test helper for asserting schema bootstrap; production code that needs to
// branch on schema state should prefer an explicit migration version check.
func (db *DB) hasTable(tbl string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()

	stmt, _, err := db.conn.Prepare(
		"SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?")
	if err != nil {
		return false
	}
	defer stmt.Close()

	if err := stmt.BindText(1, tbl); err != nil {
		return false
	}
	return stmt.Step()
}
