package store

import "testing"

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
// database file does not error and does not attempt to re-run the schema
// (which would fail on CREATE TABLE without IF NOT EXISTS/guards).
func TestOpen_IsIdempotent(t *testing.T) {
	dir := t.TempDir()

	db1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
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
