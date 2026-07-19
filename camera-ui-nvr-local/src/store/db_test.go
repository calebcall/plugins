package store

import (
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3"
)

// TestOpen_CreatesSchema verifies that Open bootstraps a fresh database
// with every table the later store layers (segments, events, faces,
// vector search) depend on.
func TestOpen_CreatesSchema(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, tbl := range []string{
		"cameras",
		"segments",
		"events",
		"system_events",
		"faces",
		"face_images",
		"unknown_faces",
		"face_embeddings",
		"clip_embeddings",
	} {
		if !db.hasTable(tbl) {
			t.Errorf("missing table %s", tbl)
		}
	}

	if db.hasTable("not_a_real_table") {
		t.Errorf("hasTable reported a table that does not exist")
	}
}

// TestOpen_IsIdempotent verifies that re-opening an already-migrated
// database file does not error, does not attempt to re-run the schema
// (which would fail on CREATE TABLE without IF NOT EXISTS/guards), and
// leaves PRAGMA user_version at schemaVersion both times — i.e. it actually
// exercises the `current >= schemaVersion` gate in migrate, not just the
// IF NOT EXISTS DDL guards (which alone would make this test pass even if
// that gate were deleted).
func TestOpen_IsIdempotent(t *testing.T) {
	dir := t.TempDir()

	db1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := userVersion(db1.Conn())
	if err != nil {
		t.Fatal(err)
	}
	if v1 != schemaVersion {
		t.Errorf("user_version after first Open = %d, want %d", v1, schemaVersion)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("re-opening a migrated database failed: %v", err)
	}
	defer db2.Close()

	if !db2.hasTable("cameras") {
		t.Errorf("missing table cameras after re-open")
	}

	v2, err := userVersion(db2.Conn())
	if err != nil {
		t.Fatal(err)
	}
	if v2 != schemaVersion {
		t.Errorf("user_version after second Open = %d, want %d (unchanged)", v2, schemaVersion)
	}
}

// TestMigrateToV2_AddsColumnToExistingV1Database proves the Task 8
// incremental migration path (migrate's `current < 2` step, migrateToV2):
// given a database file left at PRAGMA user_version 1 by an earlier build
// (its segments table created without the referenced column, exactly as
// schema.sql looked before this task), re-opening it via Open brings it to
// schemaVersion, adds the column via ALTER TABLE, and — critically —
// defaults every pre-existing row to referenced=true so footage recorded
// before this feature existed is never swept by the new events-mode
// janitor. This directly exercises the incremental-migration code path that
// TestOpen_CreatesSchema/TestOpen_IsIdempotent (a fresh database going
// straight to schemaVersion via schemaSQL) cannot: those never take the
// `current < 2` / migrateToV2 branch at all.
func TestMigrateToV2_AddsColumnToExistingV1Database(t *testing.T) {
	dir := t.TempDir()

	// Hand-build a v1 database: schemaSQL as it existed before this task
	// (segments table with no referenced column), user_version pinned to 1,
	// and one pre-existing segment row — standing in for footage a real
	// install already recorded under continuous mode before upgrading.
	conn, err := sqlite3.Open(filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(`CREATE TABLE segments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		camera_id TEXT, role TEXT, path TEXT,
		start_ms INTEGER, end_ms INTEGER,
		has_video INTEGER, has_audio INTEGER, codec TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(`INSERT INTO segments (camera_id, role, path, start_ms, end_ms)
		VALUES ('cam1', 'high', '/rec/pre-existing.mp4', 0, 1000)`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open (migrate v1 -> v2): %v", err)
	}
	defer db.Close()

	v, err := userVersion(db.Conn())
	if err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version after migration = %d, want %d", v, schemaVersion)
	}

	got, err := NewSegmentStore(db).InRange("cam1", "high", 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the pre-existing segment to survive migration, got %d rows", len(got))
	}
	if !got[0].Referenced {
		t.Errorf("expected the pre-existing row to default to referenced=true after migration, got %+v", got[0])
	}
}

// TestConn_ReturnsUnderlyingConnection verifies the Conn accessor exposes a
// usable *sqlite3.Conn for lower-level access by later tasks.
func TestConn_ReturnsUnderlyingConnection(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if db.Conn() == nil {
		t.Fatal("Conn() returned nil")
	}
	if err := db.Conn().Exec("SELECT 1"); err != nil {
		t.Fatalf("Conn() returned an unusable connection: %v", err)
	}
}
