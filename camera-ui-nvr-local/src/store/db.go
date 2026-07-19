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
const schemaVersion = 1

// dbFileName is the SQLite database file created inside the directory
// passed to Open.
const dbFileName = "nvr.db"

// DB wraps a single connection to the plugin's SQLite database plus the
// vector-search backends used for face and CLIP embeddings.
type DB struct {
	conn *sqlite3.Conn

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

	if err := migrate(conn); err != nil {
		conn.Close()
		return nil, err
	}

	return &DB{
		conn:        conn,
		FaceVectors: newBruteForceVectorBackend(conn, "face_embeddings"),
		ClipVectors: newBruteForceVectorBackend(conn, "clip_embeddings"),
	}, nil
}

// Conn exposes the underlying connection for lower-level access by later
// tasks (segment/event/face stores, vector backends).
func (db *DB) Conn() *sqlite3.Conn { return db.conn }

// Close releases the underlying SQLite connection.
func (db *DB) Close() error { return db.conn.Close() }

// migrate applies schemaSQL exactly once per database file, tracked via
// PRAGMA user_version, so repeated calls to Open against the same file are
// idempotent and cheap (a single PRAGMA read) once migrated.
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
	if err := conn.Exec(schemaSQL); err != nil {
		_ = conn.Exec("ROLLBACK")
		return fmt.Errorf("store: apply schema: %w", err)
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
