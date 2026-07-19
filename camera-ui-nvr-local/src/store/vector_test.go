package store

import "testing"

// TestBruteForceVectorBackend_KNN proves the fallback VectorBackend
// implementation (see vector.go for why sqlite-vec is not used) can store
// embeddings and return the nearest one first on query.
func TestBruteForceVectorBackend_KNN(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	backend := db.FaceVectors

	near := []float32{1, 0, 0, 0}
	far := []float32{0, 1, 0, 0}

	if err := backend.Upsert("near", near); err != nil {
		t.Fatalf("upsert near: %v", err)
	}
	if err := backend.Upsert("far", far); err != nil {
		t.Fatalf("upsert far: %v", err)
	}

	matches, err := backend.Query([]float32{0.9, 0.1, 0, 0}, 2)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
	if matches[0].ID != "near" {
		t.Errorf("expected nearest match to be %q, got %q (matches=%v)", "near", matches[0].ID, matches)
	}
	if matches[0].Distance > matches[1].Distance {
		t.Errorf("expected matches sorted by ascending distance, got %v", matches)
	}
}

// TestBruteForceVectorBackend_UpsertReplaces verifies Upsert overwrites a
// previously stored embedding for the same id rather than duplicating it.
func TestBruteForceVectorBackend_UpsertReplaces(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	backend := db.ClipVectors

	if err := backend.Upsert("x", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := backend.Upsert("x", []float32{0, 1}); err != nil {
		t.Fatal(err)
	}

	matches, err := backend.Query([]float32{0, 1}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 row after replacing id, got %d: %v", len(matches), matches)
	}
	if matches[0].Distance > 0.001 {
		t.Errorf("expected the replaced embedding to be used, distance=%v", matches[0].Distance)
	}
}

// TestBruteForceVectorBackend_Query_FiltersByDimension proves that rows
// stored with a different embedding dimension (e.g. left over from an
// older embedding model) do not break querying against the current
// dimension: Query must return the correct same-dimension nearest match
// with no error, and must simply omit rows of other dimensions rather than
// aborting the whole query with a dimension-mismatch error.
func TestBruteForceVectorBackend_Query_FiltersByDimension(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	backend := db.FaceVectors

	// An old-model embedding with a different dimension than the query.
	if err := backend.Upsert("old-model", []float32{1, 0}); err != nil {
		t.Fatalf("upsert old-model (dim 2): %v", err)
	}
	// A current-model embedding, same dimension as the query, and a near
	// perfect match for it.
	if err := backend.Upsert("current-model", []float32{1, 0, 0, 0}); err != nil {
		t.Fatalf("upsert current-model (dim 4): %v", err)
	}

	matches, err := backend.Query([]float32{0.9, 0.1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 same-dimension match, got %d: %v", len(matches), matches)
	}
	if matches[0].ID != "current-model" {
		t.Errorf("expected match %q, got %q", "current-model", matches[0].ID)
	}
}

// TestBruteForceVectorBackend_Delete verifies Delete removes an id from
// future query results.
func TestBruteForceVectorBackend_Delete(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	backend := db.FaceVectors

	if err := backend.Upsert("gone", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete("gone"); err != nil {
		t.Fatal(err)
	}

	matches, err := backend.Query([]float32{1, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("expected no matches after delete, got %v", matches)
	}
}
