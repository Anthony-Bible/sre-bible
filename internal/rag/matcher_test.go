package rag_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// recordingEmbedder returns a fixed vector (or error) and records its input.
type recordingEmbedder struct {
	vec []float32
	err error
}

func (e recordingEmbedder) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	return e.vec, e.err
}

// recordingSearcher returns configured chunks (or error) and records the limit/vector.
type recordingSearcher struct {
	chunks   []rag.RetrievedChunk
	err      error
	gotLimit int
	gotVec   []float32
}

func (s *recordingSearcher) SearchChunks(_ context.Context, vec []float32, limit int) ([]rag.RetrievedChunk, error) {
	s.gotLimit = limit
	s.gotVec = vec
	return s.chunks, s.err
}

func TestMatcher_HappyPath(t *testing.T) {
	t.Parallel()
	want := []rag.RetrievedChunk{{Content: "c", SourceName: "resume.pdf"}}
	srch := &recordingSearcher{chunks: want}
	m := rag.NewMatcher(recordingEmbedder{vec: []float32{0.1, 0.2}}, srch)

	got, err := m.MatchRequirement(context.Background(), "Kubernetes", 4)
	if err != nil {
		t.Fatalf("MatchRequirement: %v", err)
	}
	if len(got) != 1 || got[0].SourceName != "resume.pdf" {
		t.Errorf("chunks: got %v, want %v", got, want)
	}
	if srch.gotLimit != 4 {
		t.Errorf("searcher limit: got %d, want 4", srch.gotLimit)
	}
	if len(srch.gotVec) != 2 {
		t.Errorf("searcher vec: got %v, want the embedder's vector", srch.gotVec)
	}
}

func TestMatcher_EmbedErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("embed boom")
	srch := &recordingSearcher{}
	m := rag.NewMatcher(recordingEmbedder{err: sentinel}, srch)

	_, err := m.MatchRequirement(context.Background(), "x", 4)
	if !errors.Is(err, sentinel) {
		t.Errorf("err: got %v, want %v", err, sentinel)
	}
	if srch.gotLimit != 0 {
		t.Error("searcher must not be called when embedding fails")
	}
}

func TestMatcher_SearchErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("search boom")
	m := rag.NewMatcher(recordingEmbedder{vec: []float32{0.1}}, &recordingSearcher{err: sentinel})

	_, err := m.MatchRequirement(context.Background(), "x", 4)
	if !errors.Is(err, sentinel) {
		t.Errorf("err: got %v, want %v", err, sentinel)
	}
}

func TestMatcher_DefaultsKWhenNonPositive(t *testing.T) {
	t.Parallel()
	for _, k := range []int{0, -1} {
		srch := &recordingSearcher{}
		m := rag.NewMatcher(recordingEmbedder{vec: []float32{0.1}}, srch)
		if _, err := m.MatchRequirement(context.Background(), "x", k); err != nil {
			t.Fatalf("k=%d: %v", k, err)
		}
		if srch.gotLimit != 4 {
			t.Errorf("k=%d: searcher limit got %d, want default 4", k, srch.gotLimit)
		}
	}
}
