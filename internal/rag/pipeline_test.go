package rag_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// stubEmbedder always returns the same fixed vector.
type stubEmbedder struct{}

func (s stubEmbedder) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

// stubSearcher returns a configurable list of chunks.
type stubSearcher struct {
	chunks []rag.RetrievedChunk
}

func (s stubSearcher) SearchChunks(_ context.Context, _ []float32, _ int) ([]rag.RetrievedChunk, error) {
	return s.chunks, nil
}

// stubGenerator records calls and collects messages.
type stubGenerator struct {
	called   bool
	received []rag.Message
	tokens   []string
}

func (g *stubGenerator) StreamAnswer(_ context.Context, messages []rag.Message, onToken func(string) error) error {
	g.called = true
	g.received = messages
	for _, tok := range g.tokens {
		if err := onToken(tok); err != nil {
			return err
		}
	}
	return nil
}

func TestPipeline_EmptyChunkGuard(t *testing.T) {
	t.Parallel()

	gen := &stubGenerator{}
	pipe := rag.NewPipeline(stubEmbedder{}, stubSearcher{chunks: nil}, gen, 0, nil)

	var got []string
	citations, err := pipe.Answer(context.Background(), nil, "anything?", func(tok string) error {
		got = append(got, tok)
		return nil
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if gen.called {
		t.Error("Generator must not be called when no chunks are retrieved")
	}
	if len(citations) != 0 {
		t.Errorf("citations: got %v, want empty", citations)
	}
	if len(got) == 0 {
		t.Error("expected canned message via onToken, got nothing")
	}
	msg := strings.Join(got, "")
	if !strings.Contains(msg, "couldn't find") {
		t.Errorf("canned message %q doesn't look right", msg)
	}
}

func TestPipeline_CitationDeduplication(t *testing.T) {
	t.Parallel()

	chunks := []rag.RetrievedChunk{
		{Content: "c1", SourceName: "resume.pdf"},
		{Content: "c2", SourceName: "about.html"},
		{Content: "c3", SourceName: "resume.pdf"}, // duplicate
	}
	gen := &stubGenerator{tokens: []string{"answer"}}
	pipe := rag.NewPipeline(stubEmbedder{}, stubSearcher{chunks: chunks}, gen, 0, nil)

	citations, err := pipe.Answer(context.Background(), nil, "q?", func(string) error { return nil })
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	want := []string{"resume.pdf", "about.html"}
	if len(citations) != len(want) {
		t.Fatalf("citations len: got %d, want %d (%v)", len(citations), len(want), citations)
	}
	for i, w := range want {
		if citations[i] != w {
			t.Errorf("citations[%d]: got %q, want %q", i, citations[i], w)
		}
	}
}

func TestPipeline_HistoryPassedToGenerator(t *testing.T) {
	t.Parallel()

	chunks := []rag.RetrievedChunk{{Content: "ctx", SourceName: "src"}}
	gen := &stubGenerator{tokens: []string{"ok"}}
	pipe := rag.NewPipeline(stubEmbedder{}, stubSearcher{chunks: chunks}, gen, 0, nil)

	history := []rag.Message{
		{Role: rag.RoleUser, Content: "previous question"},
		{Role: rag.RoleAssistant, Content: "previous answer"},
	}

	_, err := pipe.Answer(context.Background(), history, "new question?", func(string) error { return nil })
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if !gen.called {
		t.Fatal("Generator was not called")
	}
	if len(gen.received) != 3 {
		t.Fatalf("generator received %d messages, want 3", len(gen.received))
	}
	if gen.received[0].Content != history[0].Content {
		t.Errorf("messages[0]: got %q, want %q", gen.received[0].Content, history[0].Content)
	}
	if gen.received[1].Content != history[1].Content {
		t.Errorf("messages[1]: got %q, want %q", gen.received[1].Content, history[1].Content)
	}
	if gen.received[2].Role != rag.RoleUser {
		t.Errorf("messages[2] role: got %q, want %q", gen.received[2].Role, rag.RoleUser)
	}
}

func TestBuildContextBlock(t *testing.T) {
	t.Parallel()

	chunks := []rag.RetrievedChunk{
		{Content: "hello world", SourceName: "resume.pdf"},
		{Content: "foo bar", SourceName: "about.html"},
	}
	block := rag.BuildContextBlock(chunks)

	if !strings.Contains(block, `source="resume.pdf"`) {
		t.Errorf("missing source attribute for resume.pdf in %q", block)
	}
	if !strings.Contains(block, `index="0"`) {
		t.Errorf("missing index=0 in %q", block)
	}
	if !strings.Contains(block, `source="about.html"`) {
		t.Errorf("missing source attribute for about.html in %q", block)
	}
	if !strings.Contains(block, `index="1"`) {
		t.Errorf("missing index=1 in %q", block)
	}
	if !strings.Contains(block, "hello world") {
		t.Errorf("missing chunk content in %q", block)
	}
}

func TestBuildUserMessage(t *testing.T) {
	t.Parallel()

	chunks := []rag.RetrievedChunk{
		{Content: "content here", SourceName: "doc.pdf"},
	}
	msg := rag.BuildUserMessage("what is anthony's background?", chunks)

	if msg.Role != rag.RoleUser {
		t.Errorf("role: got %q, want %q", msg.Role, rag.RoleUser)
	}
	if !strings.Contains(msg.Content, "<context>") {
		t.Errorf("missing <context> tag in %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "Question:") {
		t.Errorf("missing 'Question:' in %q", msg.Content)
	}
	idx := strings.Index(msg.Content, "<context>")
	qIdx := strings.Index(msg.Content, "Question:")
	if idx > qIdx {
		t.Errorf("context block must precede 'Question:' in %q", msg.Content)
	}
}
