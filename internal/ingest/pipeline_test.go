package ingest

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- fakes ---

type fakeScreener struct {
	prefix string // prepended to input to mark screened output
	err    error
}

func (f *fakeScreener) ScreenPII(_ context.Context, text string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.prefix + text, nil
}

type fakeDescriber struct {
	receivedText string
}

func (f *fakeDescriber) Describe(_ context.Context, text string) (string, error) {
	f.receivedText = text
	return "a description", nil
}

type fakeEmbedder struct {
	receivedTexts []string
}

func (f *fakeEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	f.receivedTexts = texts
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{0.1, 0.2}
	}
	return out, nil
}

type fakeStore struct {
	receivedSrc    Source
	receivedChunks []Chunk
	called         bool
}

func (f *fakeStore) ReplaceSource(_ context.Context, src Source, chunks []Chunk) error {
	f.called = true
	f.receivedSrc = src
	f.receivedChunks = chunks
	return nil
}

type fakePDFExtractor struct{}

func (fakePDFExtractor) ExtractPDFText(_ context.Context, _ string) (string, error) {
	return "", errors.New("pdf extractor should not be called in text-source tests")
}

type fakeURLExtractor struct{}

func (fakeURLExtractor) ExtractURL(_ context.Context, _ string) (string, error) {
	return "", errors.New("url extractor should not be called in text-source tests")
}

// --- helpers ---

// writeTempTxt creates a temporary .txt file containing content and registers
// cleanup with t. Returns the file path.
func writeTempTxt(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "src-*.txt")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}
	return f.Name()
}

// --- tests ---

// TestPipeline_ScreenedTextFlowsToAllSinks asserts the core contract: the text
// returned by ScreenPII — not the raw extracted text — is what reaches the
// embedder, the describer, and Source.FullText in the store. Redacting text at
// the source means PII never enters the chunks, embeddings, description, or
// persisted full text.
func TestPipeline_ScreenedTextFlowsToAllSinks(t *testing.T) {
	t.Parallel()

	const rawContent = "Call me at 555-867-5309 or email me@example.com. I worked at Acme Corp."
	const screenPrefix = "SCREENED:"
	screened := screenPrefix + rawContent

	src := writeTempTxt(t, rawContent)

	screener := &fakeScreener{prefix: screenPrefix}
	describer := &fakeDescriber{}
	embedder := &fakeEmbedder{}
	store := &fakeStore{}

	p := NewPipeline(fakePDFExtractor{}, embedder, describer, screener, fakeURLExtractor{}, store, slog.Default())
	if err := p.Run(context.Background(), src); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// The describer must receive the screened text, not the raw content.
	if describer.receivedText != screened {
		t.Errorf("describer received raw text; want screened text\ngot:  %q\nwant: %q",
			describer.receivedText, screened)
	}

	// Every chunk handed to the embedder must contain only screened text.
	for i, txt := range embedder.receivedTexts {
		if strings.Contains(txt, rawContent) && !strings.HasPrefix(txt, screenPrefix) {
			t.Errorf("embedder segment[%d] contains raw (unscreened) content: %q", i, txt)
		}
	}

	// Source.FullText persisted to the store must be the screened version.
	if store.receivedSrc.FullText != screened {
		t.Errorf("store Source.FullText is raw text; want screened text\ngot:  %q\nwant: %q",
			store.receivedSrc.FullText, screened)
	}

	// Every persisted chunk's content must start with the screened prefix,
	// confirming chunking ran on the screened text.
	for i, c := range store.receivedChunks {
		if !strings.HasPrefix(c.Content, screenPrefix) {
			t.Errorf("store chunk[%d] content does not start with screened prefix: %q", i, c.Content)
		}
	}
}

// TestPipeline_ScreenerErrorAbortsBeforeStore asserts that if ScreenPII returns
// an error, Run propagates it and never calls ReplaceSource. Documents must not
// be stored if PII screening fails.
func TestPipeline_ScreenerErrorAbortsBeforeStore(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("pii screen failed")
	src := writeTempTxt(t, "some document content")

	screener := &fakeScreener{err: sentinel}
	store := &fakeStore{}

	p := NewPipeline(fakePDFExtractor{}, &fakeEmbedder{}, &fakeDescriber{}, screener, fakeURLExtractor{}, store, slog.Default())
	err := p.Run(context.Background(), src)

	if err == nil {
		t.Fatal("Run() returned nil; want error when screener fails")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Run() error = %v; want wrapping %v", err, sentinel)
	}
	if store.called {
		t.Error("ReplaceSource was called despite screener error; documents must not be stored if PII screening fails")
	}
}

// TestPipeline_RechunkPreservesMetadataAndReplacesChunks asserts the Rechunk
// contract: it re-segments src.FullText with the current chunker, embeds each
// segment, and calls ReplaceSource with the source metadata preserved verbatim
// (Name, Type, Location, Description) and one Chunk per segment with sequential
// Idx. Extraction, PII screening, and description must NOT run — the fakePDF/URL
// extractors error if touched, and we pass the description through unchanged.
func TestPipeline_RechunkPreservesMetadataAndReplacesChunks(t *testing.T) {
	t.Parallel()

	// FullText long enough to force multiple chunks (> hard cap).
	fullText := makeParagraphs(6, 500)
	src := Source{
		Name:        "resume.pdf",
		Type:        "pdf",
		Location:    "/data/resume.pdf",
		FullText:    fullText,
		Description: "An SRE résumé.",
	}

	embedder := &fakeEmbedder{}
	store := &fakeStore{}
	p := NewPipeline(fakePDFExtractor{}, embedder, &fakeDescriber{}, &fakeScreener{}, fakeURLExtractor{}, store, slog.Default())

	n, err := p.Rechunk(context.Background(), src)
	if err != nil {
		t.Fatalf("Rechunk() error: %v", err)
	}

	wantSegments := ChunkText(fullText)
	if n != len(wantSegments) {
		t.Errorf("Rechunk returned %d; want %d chunks", n, len(wantSegments))
	}
	if !store.called {
		t.Fatal("ReplaceSource was not called")
	}
	if store.receivedSrc != src {
		t.Errorf("source metadata mutated\ngot:  %+v\nwant: %+v", store.receivedSrc, src)
	}
	if len(store.receivedChunks) != len(wantSegments) {
		t.Fatalf("stored %d chunks; want %d", len(store.receivedChunks), len(wantSegments))
	}
	for i, c := range store.receivedChunks {
		if c.Idx != i {
			t.Errorf("chunk[%d].Idx = %d; want %d", i, c.Idx, i)
		}
		if c.Content != wantSegments[i] {
			t.Errorf("chunk[%d] content does not match ChunkText output", i)
		}
		if len(c.Embedding) == 0 {
			t.Errorf("chunk[%d] has no embedding", i)
		}
	}
}

// TestPipeline_RechunkEmptyFullTextErrors asserts Rechunk refuses a source with
// no stored full text (it has nothing to re-segment) and never writes.
func TestPipeline_RechunkEmptyFullTextErrors(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	p := NewPipeline(fakePDFExtractor{}, &fakeEmbedder{}, &fakeDescriber{}, &fakeScreener{}, fakeURLExtractor{}, store, slog.Default())

	if _, err := p.Rechunk(context.Background(), Source{Name: "legacy.pdf"}); err == nil {
		t.Fatal("Rechunk() returned nil; want error for source with empty FullText")
	}
	if store.called {
		t.Error("ReplaceSource was called for a source with no full text")
	}
}

// TestPipeline_SourceNameDerivedFromTxtFile asserts that the pipeline sets the
// source name to the basename of the .txt file (the contract of DeriveSourceName
// for text-type sources).
func TestPipeline_SourceNameDerivedFromTxtFile(t *testing.T) {
	t.Parallel()

	src := writeTempTxt(t, "hello world content for deriving source name test")
	expectedName := filepath.Base(src)

	store := &fakeStore{}
	p := NewPipeline(fakePDFExtractor{}, &fakeEmbedder{}, &fakeDescriber{}, &fakeScreener{}, fakeURLExtractor{}, store, slog.Default())
	if err := p.Run(context.Background(), src); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if store.receivedSrc.Name != expectedName {
		t.Errorf("Source.Name = %q; want %q", store.receivedSrc.Name, expectedName)
	}
}

// TestPipeline_RechunkWithChunkConfigChangesSegmentCount asserts the sweep's
// load-bearing contract: a non-default WithChunkConfig changes the chunk geometry
// that Rechunk produces. A smaller target/hardCap must yield strictly more
// segments than the default chunker over the same FullText, and the stored chunks
// must match ChunkTextWith(fullText, cfg) exactly — proving the option threads all
// the way through to what is embedded and persisted, not just the returned count.
func TestPipeline_RechunkWithChunkConfigChangesSegmentCount(t *testing.T) {
	t.Parallel()

	fullText := makeParagraphs(6, 500)
	src := Source{
		Name:        "resume.pdf",
		Type:        "pdf",
		Location:    "/data/resume.pdf",
		FullText:    fullText,
		Description: "An SRE résumé.",
	}

	small := ChunkConfig{Target: 300, HardCap: 400, Overlap: 50}
	wantSegments := ChunkTextWith(fullText, small)
	if len(wantSegments) <= len(ChunkText(fullText)) {
		t.Fatalf("test setup invalid: small config produced %d segments, not more than default's %d",
			len(wantSegments), len(ChunkText(fullText)))
	}

	store := &fakeStore{}
	p := NewPipeline(fakePDFExtractor{}, &fakeEmbedder{}, &fakeDescriber{}, &fakeScreener{}, fakeURLExtractor{}, store,
		slog.Default(), WithChunkConfig(small))

	n, err := p.Rechunk(context.Background(), src)
	if err != nil {
		t.Fatalf("Rechunk() error: %v", err)
	}
	if n != len(wantSegments) {
		t.Errorf("Rechunk returned %d segments; want %d (small config)", n, len(wantSegments))
	}
	if len(store.receivedChunks) != len(wantSegments) {
		t.Fatalf("stored %d chunks; want %d", len(store.receivedChunks), len(wantSegments))
	}
	for i, c := range store.receivedChunks {
		if c.Content != wantSegments[i] {
			t.Errorf("chunk[%d] content does not match ChunkTextWith output", i)
		}
	}
}
