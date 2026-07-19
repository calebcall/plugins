package store

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// VectorBackend abstracts nearest-neighbor search over embeddings keyed by
// an arbitrary string id (a face_images.id or events.id, depending on
// table). The plan calls for two possible implementations: one backed by
// the sqlite-vec extension's vec0 virtual tables, and a brute-force
// fallback that stores embeddings as BLOBs and computes cosine distance in
// Go.
//
// Active backend in this task: the brute-force fallback
// (bruteForceVectorBackend, below).
//
// sqlite-vec was evaluated and is not usable yet with this plugin's pinned
// SQLite driver:
//
//   - github.com/asg017/sqlite-vec-go-bindings/ncruces (the only published
//     ncruces/go-sqlite3 binding for sqlite-vec) fails to compile against
//     github.com/ncruces/go-sqlite3 v0.35.2 (the version this plugin
//     otherwise uses): its init.go sets sqlite3.Binary, a package-level
//     variable that v0.35.2 no longer exports (go-sqlite3's embed package is
//     now a deprecated no-op; the WASM-binary override mechanism changed).
//   - Forcing the module graph down to the version those bindings actually
//     pin, github.com/ncruces/go-sqlite3 v0.17.1 (released ~1.5 years
//     earlier), does compile with CGO_ENABLED=0, but the bundled sqlite-vec
//     WASM binary fails at *runtime* under the wazero build actually
//     resolved:
//
//       invalid function[11] export["sqlite3_soft_heap_limit64"]:
//       i32.atomic.store invalid as feature "" is disabled
//
//     i.e. it does not load cleanly, and pinning a SQLite driver 1.5 years
//     out of date for the whole plugin just to get a vector extension is
//     not an acceptable trade.
//
// Per the plan (spec §5), this is an accepted, documented fallback. Every
// caller depends only on the VectorBackend interface, so a future task can
// swap in a real sqlite-vec-backed implementation transparently, once the
// upstream bindings support a current go-sqlite3 release, without touching
// callers. encodeEmbedding/decodeEmbedding below intentionally use the same
// little-endian float32 BLOB layout sqlite-vec uses for its own vectors, so
// rows written by this fallback would remain readable by that future
// implementation.
type VectorBackend interface {
	// Upsert stores or replaces the embedding for id.
	Upsert(id string, embedding []float32) error
	// Delete removes the embedding for id, if present. Deleting an id that
	// was never stored is not an error.
	Delete(id string) error
	// Query returns up to k ids nearest to embedding, ordered by ascending
	// distance (nearest first).
	Query(embedding []float32, k int) ([]VectorMatch, error)
}

// VectorMatch is one result row from VectorBackend.Query.
type VectorMatch struct {
	ID string
	// Distance is 1 - cosine_similarity(query, candidate): 0 for an
	// identical direction, 2 for an exactly opposite one.
	Distance float64
}

// bruteForceVectorBackend implements VectorBackend by storing each
// embedding as a little-endian float32 BLOB in a table (face_embeddings or
// clip_embeddings) and computing cosine distance against every stored
// embedding in Go at query time. This is O(n) per query, which is
// acceptable for the per-installation embedding counts this plugin expects;
// it sits behind VectorBackend precisely so it can be swapped for an
// index-backed implementation later without touching callers.
type bruteForceVectorBackend struct {
	// db, not a bare *sqlite3.Conn: every method below must go through
	// db.Lock()/Unlock() (or withConn) around its conn access, same as every
	// other store, since this backend's face_embeddings/clip_embeddings
	// tables live on the exact same shared connection SegmentStore/
	// EventStore do — see the DB type doc comment (db.go) for why that
	// connection-level lock exists and why a backend-private lock (or none
	// at all, as this type had before the Task 9 review fix) isn't enough.
	db    *DB
	table string
}

func newBruteForceVectorBackend(db *DB, table string) *bruteForceVectorBackend {
	return &bruteForceVectorBackend{db: db, table: table}
}

func (b *bruteForceVectorBackend) Upsert(id string, embedding []float32) error {
	blob, err := encodeEmbedding(embedding)
	if err != nil {
		return fmt.Errorf("store: encode embedding for %s: %w", id, err)
	}

	b.db.Lock()
	defer b.db.Unlock()

	stmt, _, err := b.db.Conn().Prepare(fmt.Sprintf(
		`INSERT INTO %s (id, embedding, dim) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET embedding = excluded.embedding, dim = excluded.dim`,
		b.table))
	if err != nil {
		return fmt.Errorf("store: prepare upsert into %s: %w", b.table, err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, id); err != nil {
		return err
	}
	if err := stmt.BindBlob(2, blob); err != nil {
		return err
	}
	if err := stmt.BindInt(3, len(embedding)); err != nil {
		return err
	}
	return stmt.Exec()
}

func (b *bruteForceVectorBackend) Delete(id string) error {
	b.db.Lock()
	defer b.db.Unlock()

	stmt, _, err := b.db.Conn().Prepare(fmt.Sprintf("DELETE FROM %s WHERE id = ?", b.table))
	if err != nil {
		return fmt.Errorf("store: prepare delete from %s: %w", b.table, err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, id); err != nil {
		return err
	}
	return stmt.Exec()
}

func (b *bruteForceVectorBackend) Query(embedding []float32, k int) ([]VectorMatch, error) {
	if k <= 0 {
		return nil, nil
	}

	b.db.Lock()
	defer b.db.Unlock()

	// Only compare rows whose stored dimension matches the query vector.
	// Without this filter, a single row left over from a different
	// embedding model (a different dim) would make cosineDistance error
	// out and abort the whole query, discarding every valid same-dimension
	// match — exactly the case the dim column exists to guard against
	// (e.g. migrating to a new embedding model without a data migration).
	stmt, _, err := b.db.Conn().Prepare(fmt.Sprintf("SELECT id, embedding FROM %s WHERE dim = ?", b.table))
	if err != nil {
		return nil, fmt.Errorf("store: prepare scan of %s: %w", b.table, err)
	}
	defer stmt.Close()

	if err := stmt.BindInt(1, len(embedding)); err != nil {
		return nil, err
	}

	var matches []VectorMatch
	for stmt.Step() {
		id := stmt.ColumnText(0)
		blob := stmt.ColumnBlob(1, nil)

		candidate, err := decodeEmbedding(blob)
		if err != nil {
			return nil, fmt.Errorf("store: decode embedding for %s: %w", id, err)
		}

		distance, err := cosineDistance(embedding, candidate)
		if err != nil {
			return nil, fmt.Errorf("store: compare embedding for %s: %w", id, err)
		}
		matches = append(matches, VectorMatch{ID: id, Distance: distance})
	}
	if err := stmt.Err(); err != nil {
		return nil, fmt.Errorf("store: scan %s: %w", b.table, err)
	}

	sort.Slice(matches, func(i, j int) bool { return matches[i].Distance < matches[j].Distance })
	if len(matches) > k {
		matches = matches[:k]
	}
	return matches, nil
}

// encodeEmbedding serializes a float32 vector to a little-endian BLOB.
func encodeEmbedding(v []float32) ([]byte, error) {
	buf := new(bytes.Buffer)
	buf.Grow(len(v) * 4)
	if err := binary.Write(buf, binary.LittleEndian, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeEmbedding is the inverse of encodeEmbedding.
func decodeEmbedding(blob []byte) ([]float32, error) {
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("embedding blob length %d is not a multiple of 4", len(blob))
	}
	v := make([]float32, len(blob)/4)
	if err := binary.Read(bytes.NewReader(blob), binary.LittleEndian, v); err != nil {
		return nil, err
	}
	return v, nil
}

// cosineDistance returns 1 - cosine_similarity(a, b): 0 for vectors
// pointing in the same direction, 2 for exactly opposite directions.
func cosineDistance(a, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("embedding dimension mismatch: %d vs %d", len(a), len(b))
	}

	var dot, normA, normB float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		normA += fa * fa
		normB += fb * fb
	}
	if normA == 0 || normB == 0 {
		// A zero vector has no defined direction; treat as maximally
		// distant rather than dividing by zero.
		return 2, nil
	}

	similarity := dot / (math.Sqrt(normA) * math.Sqrt(normB))
	if similarity > 1 {
		similarity = 1
	} else if similarity < -1 {
		similarity = -1
	}
	return 1 - similarity, nil
}
