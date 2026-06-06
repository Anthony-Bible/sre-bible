package db_test

import (
	"context"
	"log/slog"
	"math"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Anthony-Bible/sre-bible/internal/db"
	"github.com/Anthony-Bible/sre-bible/internal/ingest"
)

// testDB sets up a pool and migrates the schema, skipping if TEST_DATABASE_URL is unset.
// It returns the pool and a cleanup func that truncates sources (cascades to chunks).
//
// Bootstrap order:
//  1. Open a plain pool (no pgvector AfterConnect hook) to run migrations.
//     This installs the `vector` extension if it is not already present.
//  2. Close the plain pool and open a full db.NewPool which registers the
//     pgvector OID — this succeeds because the extension now exists.
func testDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()

	// Step 1: plain pool — no pgvector hook — used only to apply migrations.
	bootstrapPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("bootstrap pool: %v", err)
	}
	if err := db.Migrate(ctx, bootstrapPool, slog.Default()); err != nil {
		bootstrapPool.Close()
		t.Fatalf("Migrate: %v", err)
	}
	bootstrapPool.Close()

	// Step 2: full pool with pgvector types registered (extension now exists).
	pool, err := db.NewPool(ctx, dsn, slog.Default())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	cleanup := func() {
		_, cleanErr := pool.Exec(context.Background(), "TRUNCATE sources CASCADE")
		if cleanErr != nil {
			t.Errorf("cleanup truncate: %v", cleanErr)
		}
		pool.Close()
	}

	// Ensure a clean slate before the test body runs.
	if _, err := pool.Exec(ctx, "TRUNCATE sources CASCADE"); err != nil {
		pool.Close()
		t.Fatalf("pre-test truncate: %v", err)
	}

	return pool, cleanup
}

// makeEmbedding returns a deterministic 768-dimensional float32 slice.
// seed offsets every value so two different embeddings are distinguishable.
func makeEmbedding(seed float32) []float32 {
	v := make([]float32, 768)
	for i := range v {
		// keep values in [-1,1] so they are valid cosine-search candidates
		v[i] = float32(math.Sin(float64(seed)+float64(i)*0.01)) * 0.5
	}
	return v
}

// countRows queries a simple COUNT(*) … WHERE clause and returns the result.
func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("countRows(%q): %v", query, err)
	}
	return n
}

// --- Contract 1: Basic ingest ---

// TestReplaceSource_InsertsSourceRow verifies that after a successful call the
// sources table contains exactly one row with the given name.
func TestReplaceSource_InsertsSourceRow(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	store := db.NewSourceStore(pool, slog.Default())
	src := ingest.Source{Name: "doc-a", Type: "pdf", Location: "s3://bucket/doc-a.pdf"}
	chunks := []ingest.Chunk{
		{Idx: 0, Content: "hello world", Embedding: makeEmbedding(1)},
		{Idx: 1, Content: "foo bar", Embedding: makeEmbedding(2)},
	}

	if err := store.ReplaceSource(context.Background(), src, chunks); err != nil {
		t.Fatalf("ReplaceSource: %v", err)
	}

	n := countRows(t, pool, `SELECT COUNT(*) FROM sources WHERE name = $1`, "doc-a")
	if n != 1 {
		t.Errorf("expected 1 source row with name %q, got %d", "doc-a", n)
	}
}

// TestReplaceSource_InsertsNChunkRows verifies that after ingesting N chunks the
// chunks table contains exactly N rows tied to the source.
func TestReplaceSource_InsertsNChunkRows(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	store := db.NewSourceStore(pool, slog.Default())
	src := ingest.Source{Name: "doc-b", Type: "url", Location: "https://example.com/doc-b"}
	const chunkCount = 5
	chunks := make([]ingest.Chunk, chunkCount)
	for i := range chunks {
		chunks[i] = ingest.Chunk{Idx: i, Content: "chunk content", Embedding: makeEmbedding(float32(i))}
	}

	if err := store.ReplaceSource(context.Background(), src, chunks); err != nil {
		t.Fatalf("ReplaceSource: %v", err)
	}

	var sourceID int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM sources WHERE name = $1`, "doc-b").Scan(&sourceID); err != nil {
		t.Fatalf("fetch source id: %v", err)
	}

	n := countRows(t, pool, `SELECT COUNT(*) FROM chunks WHERE source_id = $1`, sourceID)
	if n != chunkCount {
		t.Errorf("expected %d chunk rows, got %d", chunkCount, n)
	}
}

// TestReplaceSource_ChunkIdxValuesAreSequential verifies that the idx column for
// ingested chunks matches the values provided (0..N-1 when supplied that way).
func TestReplaceSource_ChunkIdxValuesAreSequential(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	store := db.NewSourceStore(pool, slog.Default())
	src := ingest.Source{Name: "doc-idx", Type: "pdf", Location: "/local/doc-idx.pdf"}
	const chunkCount = 4
	chunks := make([]ingest.Chunk, chunkCount)
	for i := range chunks {
		chunks[i] = ingest.Chunk{Idx: i, Content: "content", Embedding: makeEmbedding(float32(i + 10))}
	}

	if err := store.ReplaceSource(context.Background(), src, chunks); err != nil {
		t.Fatalf("ReplaceSource: %v", err)
	}

	rows, err := pool.Query(context.Background(),
		`SELECT idx FROM chunks
		 JOIN sources ON sources.id = chunks.source_id
		 WHERE sources.name = $1
		 ORDER BY idx`, "doc-idx")
	if err != nil {
		t.Fatalf("query chunk idx: %v", err)
	}
	defer rows.Close()

	var got []int
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			t.Fatalf("scan idx: %v", err)
		}
		got = append(got, idx)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	if len(got) != chunkCount {
		t.Fatalf("expected %d idx values, got %d", chunkCount, len(got))
	}
	for i, v := range got {
		if v != i {
			t.Errorf("chunk idx[%d] = %d, want %d", i, v, i)
		}
	}
}

// --- Contract 2: Embedding round-trips ---

// TestReplaceSource_EmbeddingRoundTrips verifies that the float32 embedding values
// stored in the chunks table are retrieved with precision within 1e-6.
func TestReplaceSource_EmbeddingRoundTrips(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	store := db.NewSourceStore(pool, slog.Default())
	src := ingest.Source{Name: "doc-embed", Type: "url", Location: "https://example.com/embed"}
	want := makeEmbedding(42)
	chunks := []ingest.Chunk{
		{Idx: 0, Content: "embedding test", Embedding: want},
	}

	if err := store.ReplaceSource(context.Background(), src, chunks); err != nil {
		t.Fatalf("ReplaceSource: %v", err)
	}

	// pgvector stores vectors as text in wire format; fetch as text and parse
	// via the standard pgx array scan to avoid a pgvector import in the test.
	// We select the embedding cast to float4[] which pgx can scan as []float32.
	var got []float32
	if err := pool.QueryRow(context.Background(),
		`SELECT embedding::float4[]
		 FROM chunks
		 JOIN sources ON sources.id = chunks.source_id
		 WHERE sources.name = $1 AND chunks.idx = 0`, "doc-embed",
	).Scan(&got); err != nil {
		t.Fatalf("scan embedding: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("embedding length: got %d, want %d", len(got), len(want))
	}
	const epsilon = 1e-6
	for i, w := range want {
		diff := math.Abs(float64(got[i]) - float64(w))
		if diff > epsilon {
			t.Errorf("embedding[%d]: got %v, want %v (diff %v > %v)", i, got[i], w, diff, epsilon)
		}
	}
}

// --- Contract 3: Replace semantics ---

// TestReplaceSource_ReplacesChunksOnSecondCall verifies that calling ReplaceSource
// a second time with the same source name but a different chunk set leaves only the
// new chunks in the database (old chunks are gone).
func TestReplaceSource_ReplacesChunksOnSecondCall(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	store := db.NewSourceStore(pool, slog.Default())
	src := ingest.Source{Name: "doc-replace", Type: "pdf", Location: "/replace.pdf"}

	firstChunks := []ingest.Chunk{
		{Idx: 0, Content: "first-0", Embedding: makeEmbedding(1)},
		{Idx: 1, Content: "first-1", Embedding: makeEmbedding(2)},
		{Idx: 2, Content: "first-2", Embedding: makeEmbedding(3)},
	}
	if err := store.ReplaceSource(context.Background(), src, firstChunks); err != nil {
		t.Fatalf("first ReplaceSource: %v", err)
	}

	secondChunks := []ingest.Chunk{
		{Idx: 0, Content: "second-0", Embedding: makeEmbedding(10)},
	}
	if err := store.ReplaceSource(context.Background(), src, secondChunks); err != nil {
		t.Fatalf("second ReplaceSource: %v", err)
	}

	var sourceID int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM sources WHERE name = $1`, "doc-replace").Scan(&sourceID); err != nil {
		t.Fatalf("fetch source id: %v", err)
	}

	n := countRows(t, pool, `SELECT COUNT(*) FROM chunks WHERE source_id = $1`, sourceID)
	if n != len(secondChunks) {
		t.Errorf("after replace: expected %d chunks, got %d", len(secondChunks), n)
	}
}

// TestReplaceSource_DoesNotDuplicateSourceRow verifies that calling ReplaceSource
// twice for the same source name results in exactly one row in the sources table.
func TestReplaceSource_DoesNotDuplicateSourceRow(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	store := db.NewSourceStore(pool, slog.Default())
	src := ingest.Source{Name: "doc-dedup", Type: "url", Location: "https://example.com/dedup"}
	chunk := ingest.Chunk{Idx: 0, Content: "x", Embedding: makeEmbedding(0)}

	for i := range 2 {
		if err := store.ReplaceSource(context.Background(), src, []ingest.Chunk{chunk}); err != nil {
			t.Fatalf("call %d: ReplaceSource: %v", i+1, err)
		}
	}

	n := countRows(t, pool, `SELECT COUNT(*) FROM sources WHERE name = $1`, "doc-dedup")
	if n != 1 {
		t.Errorf("expected exactly 1 source row after two calls, got %d", n)
	}
}

// TestReplaceSource_UpdatesSourceMetadataOnReplace verifies that when ReplaceSource
// is called a second time with changed Type/Location for the same name the source
// row reflects the new values.
func TestReplaceSource_UpdatesSourceMetadataOnReplace(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	store := db.NewSourceStore(pool, slog.Default())
	first := ingest.Source{Name: "doc-meta", Type: "pdf", Location: "/old.pdf"}
	second := ingest.Source{Name: "doc-meta", Type: "url", Location: "https://new.example.com"}
	chunk := ingest.Chunk{Idx: 0, Content: "content", Embedding: makeEmbedding(7)}

	if err := store.ReplaceSource(context.Background(), first, []ingest.Chunk{chunk}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := store.ReplaceSource(context.Background(), second, []ingest.Chunk{chunk}); err != nil {
		t.Fatalf("second call: %v", err)
	}

	var gotType, gotLocation string
	if err := pool.QueryRow(context.Background(),
		`SELECT type, location FROM sources WHERE name = $1`, "doc-meta",
	).Scan(&gotType, &gotLocation); err != nil {
		t.Fatalf("query updated source: %v", err)
	}

	if gotType != second.Type {
		t.Errorf("type: got %q, want %q", gotType, second.Type)
	}
	if gotLocation != second.Location {
		t.Errorf("location: got %q, want %q", gotLocation, second.Location)
	}
}

// --- Contract 4: Empty chunks slice ---

// TestReplaceSource_EmptyChunksLeavesConsistentState verifies the DB is consistent
// when ReplaceSource is called with zero chunks.  The implementation upserts the
// source row and then calls CopyFrom with an empty set, so we expect:
//   - exactly 1 source row (the upsert succeeded inside the committed transaction)
//   - exactly 0 chunk rows for that source
//
// If the implementation changes to roll back on empty input the test accepts that
// by asserting the total chunks count is 0 either way.
func TestReplaceSource_EmptyChunksLeavesConsistentState(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	store := db.NewSourceStore(pool, slog.Default())
	src := ingest.Source{Name: "doc-empty", Type: "pdf", Location: "/empty.pdf"}

	if err := store.ReplaceSource(context.Background(), src, []ingest.Chunk{}); err != nil {
		t.Fatalf("ReplaceSource with empty chunks: %v", err)
	}

	// Either the source was inserted (upsert committed) or it was not (rollback).
	// In both cases there must be zero chunks associated with it.
	sourceCount := countRows(t, pool, `SELECT COUNT(*) FROM sources WHERE name = $1`, "doc-empty")
	chunkCount := countRows(t, pool,
		`SELECT COUNT(*) FROM chunks
		 JOIN sources ON sources.id = chunks.source_id
		 WHERE sources.name = $1`, "doc-empty")

	if chunkCount != 0 {
		t.Errorf("expected 0 chunks for empty ingest, got %d", chunkCount)
	}

	// Document the observed behaviour so regressions are caught.
	// The implementation commits after CopyFrom(empty), so we expect 1 source row.
	if sourceCount != 1 {
		t.Errorf("expected 1 source row after empty-chunks call (upsert in committed tx), got %d", sourceCount)
	}
}

// TestReplaceSource_EmptyChunksReplacesExistingChunks verifies that calling
// ReplaceSource with an empty slice after a prior non-empty call removes all
// previously stored chunks for that source.
func TestReplaceSource_EmptyChunksReplacesExistingChunks(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	store := db.NewSourceStore(pool, slog.Default())
	src := ingest.Source{Name: "doc-clear", Type: "url", Location: "https://example.com/clear"}
	initial := []ingest.Chunk{
		{Idx: 0, Content: "a", Embedding: makeEmbedding(1)},
		{Idx: 1, Content: "b", Embedding: makeEmbedding(2)},
	}

	if err := store.ReplaceSource(context.Background(), src, initial); err != nil {
		t.Fatalf("initial ReplaceSource: %v", err)
	}
	if err := store.ReplaceSource(context.Background(), src, []ingest.Chunk{}); err != nil {
		t.Fatalf("empty ReplaceSource: %v", err)
	}

	chunkCount := countRows(t, pool,
		`SELECT COUNT(*) FROM chunks
		 JOIN sources ON sources.id = chunks.source_id
		 WHERE sources.name = $1`, "doc-clear")

	if chunkCount != 0 {
		t.Errorf("expected 0 chunks after replacing with empty slice, got %d", chunkCount)
	}
}

// --- Contract 5: Different sources coexist ---

// TestReplaceSource_TwoSourcesCoexist verifies that ingesting two distinct sources
// results in 2 source rows and that each source's chunk count is independent.
func TestReplaceSource_TwoSourcesCoexist(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	store := db.NewSourceStore(pool, slog.Default())

	srcA := ingest.Source{Name: "doc-co-a", Type: "pdf", Location: "/a.pdf"}
	srcB := ingest.Source{Name: "doc-co-b", Type: "url", Location: "https://example.com/b"}

	chunksA := []ingest.Chunk{
		{Idx: 0, Content: "a0", Embedding: makeEmbedding(1)},
		{Idx: 1, Content: "a1", Embedding: makeEmbedding(2)},
	}
	chunksB := []ingest.Chunk{
		{Idx: 0, Content: "b0", Embedding: makeEmbedding(3)},
		{Idx: 1, Content: "b1", Embedding: makeEmbedding(4)},
		{Idx: 2, Content: "b2", Embedding: makeEmbedding(5)},
	}

	if err := store.ReplaceSource(context.Background(), srcA, chunksA); err != nil {
		t.Fatalf("ReplaceSource A: %v", err)
	}
	if err := store.ReplaceSource(context.Background(), srcB, chunksB); err != nil {
		t.Fatalf("ReplaceSource B: %v", err)
	}

	totalSources := countRows(t, pool, `SELECT COUNT(*) FROM sources WHERE name IN ($1, $2)`, "doc-co-a", "doc-co-b")
	if totalSources != 2 {
		t.Errorf("expected 2 source rows, got %d", totalSources)
	}

	nA := countRows(t, pool,
		`SELECT COUNT(*) FROM chunks
		 JOIN sources ON sources.id = chunks.source_id
		 WHERE sources.name = $1`, "doc-co-a")
	if nA != len(chunksA) {
		t.Errorf("source A: expected %d chunks, got %d", len(chunksA), nA)
	}

	nB := countRows(t, pool,
		`SELECT COUNT(*) FROM chunks
		 JOIN sources ON sources.id = chunks.source_id
		 WHERE sources.name = $1`, "doc-co-b")
	if nB != len(chunksB) {
		t.Errorf("source B: expected %d chunks, got %d", len(chunksB), nB)
	}
}

// --- Contract 6: Vector similarity search ---

// TestSearchChunks verifies that SearchChunks returns the most similar chunk first.
func TestSearchChunks(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	store := db.NewSourceStore(pool, slog.Default())

	srcA := ingest.Source{Name: "search-a", Type: "pdf", Location: "/a.pdf"}
	srcB := ingest.Source{Name: "search-b", Type: "url", Location: "https://b.example.com"}

	chunksA := []ingest.Chunk{{Idx: 0, Content: "close chunk", Embedding: makeEmbedding(1.0)}}
	chunksB := []ingest.Chunk{{Idx: 0, Content: "far chunk", Embedding: makeEmbedding(100.0)}}

	if err := store.ReplaceSource(context.Background(), srcA, chunksA); err != nil {
		t.Fatalf("ingest A: %v", err)
	}
	if err := store.ReplaceSource(context.Background(), srcB, chunksB); err != nil {
		t.Fatalf("ingest B: %v", err)
	}

	results, err := store.SearchChunks(context.Background(), makeEmbedding(1.1), 5)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}
	if len(results) < 1 {
		t.Fatalf("expected at least 1 result, got 0")
	}
	if results[0].SourceName != "search-a" {
		t.Errorf("results[0].SourceName: got %q, want %q", results[0].SourceName, "search-a")
	}
}

// TestReplaceSource_ReplacingOneSourceDoesNotAffectOther verifies that calling
// ReplaceSource for source A does not alter the chunks belonging to source B.
func TestReplaceSource_ReplacingOneSourceDoesNotAffectOther(t *testing.T) {
	pool, cleanup := testDB(t)
	defer cleanup()

	store := db.NewSourceStore(pool, slog.Default())

	srcA := ingest.Source{Name: "doc-iso-a", Type: "pdf", Location: "/iso-a.pdf"}
	srcB := ingest.Source{Name: "doc-iso-b", Type: "url", Location: "https://example.com/iso-b"}
	chunk := ingest.Chunk{Idx: 0, Content: "stable", Embedding: makeEmbedding(99)}

	if err := store.ReplaceSource(context.Background(), srcA, []ingest.Chunk{chunk}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := store.ReplaceSource(context.Background(), srcB, []ingest.Chunk{chunk}); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	// Replace A with three new chunks; B must remain untouched.
	newChunksA := []ingest.Chunk{
		{Idx: 0, Content: "new-0", Embedding: makeEmbedding(20)},
		{Idx: 1, Content: "new-1", Embedding: makeEmbedding(21)},
		{Idx: 2, Content: "new-2", Embedding: makeEmbedding(22)},
	}
	if err := store.ReplaceSource(context.Background(), srcA, newChunksA); err != nil {
		t.Fatalf("replace A: %v", err)
	}

	nB := countRows(t, pool,
		`SELECT COUNT(*) FROM chunks
		 JOIN sources ON sources.id = chunks.source_id
		 WHERE sources.name = $1`, "doc-iso-b")
	if nB != 1 {
		t.Errorf("source B chunk count should be unchanged at 1, got %d", nB)
	}
}
