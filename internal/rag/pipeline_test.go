package rag_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/email"
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
	called        bool
	received      []rag.Message
	receivedTools rag.ToolSet
	tokens        []string
	traceSteps    []rag.TraceStep // trace steps to emit via onTrace (simulating tool_call/answer)
	fetchedNames  []string        // tool-fetched source names to return from StreamAnswer
}

func (g *stubGenerator) StreamAnswer(_ context.Context, messages []rag.Message, tools rag.ToolSet, onToken func(string) error, onTrace func(rag.TraceStep) error) ([]string, error) {
	g.called = true
	g.received = messages
	g.receivedTools = tools
	for _, step := range g.traceSteps {
		if onTrace != nil {
			if err := onTrace(step); err != nil {
				return nil, err
			}
		}
	}
	for _, tok := range g.tokens {
		if err := onToken(tok); err != nil {
			return nil, err
		}
	}
	return g.fetchedNames, nil
}

// newPipe is a helper that builds a Pipeline with nil lister/fetcher/emailerFor (no tools).
func newPipe(searcher rag.ChunkSearcher, gen rag.Generator) *rag.Pipeline {
	return rag.NewPipeline(stubEmbedder{}, searcher, gen, nil, nil, nil, 0, nil)
}

func TestPipeline_EmptyChunkGuard(t *testing.T) {
	t.Parallel()

	gen := &stubGenerator{}
	pipe := newPipe(stubSearcher{chunks: nil}, gen)

	var got []string
	citations, err := pipe.Answer(context.Background(), "", nil, "anything?", func(tok string) error {
		got = append(got, tok)
		return nil
	}, nil)
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
	pipe := newPipe(stubSearcher{chunks: chunks}, gen)

	citations, err := pipe.Answer(context.Background(), "", nil, "q?", func(string) error { return nil }, nil)
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
	pipe := newPipe(stubSearcher{chunks: chunks}, gen)

	history := []rag.Message{
		{Role: rag.RoleUser, Content: "previous question"},
		{Role: rag.RoleAssistant, Content: "previous answer"},
	}

	_, err := pipe.Answer(context.Background(), "", history, "new question?", func(string) error { return nil }, nil)
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

func TestPipeline_ToolSetThreaded(t *testing.T) {
	t.Parallel()

	chunks := []rag.RetrievedChunk{{Content: "ctx", SourceName: "src"}}
	gen := &stubGenerator{tokens: []string{"ok"}}

	stubLister := &stubDocumentLister{docs: []rag.DocumentInfo{{Name: "resume.pdf", Type: "pdf"}}}
	stubFetcher := &stubFullTextFetcher{text: "full text"}
	pipe := rag.NewPipeline(stubEmbedder{}, stubSearcher{chunks: chunks}, gen, stubLister, stubFetcher, nil, 0, nil)

	_, err := pipe.Answer(context.Background(), "", nil, "q?", func(string) error { return nil }, nil)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if gen.receivedTools.Lister == nil {
		t.Error("ToolSet.Lister must be threaded through to generator")
	}
	if gen.receivedTools.Fetcher == nil {
		t.Error("ToolSet.Fetcher must be threaded through to generator")
	}
}

func TestPipeline_OnTraceThreaded(t *testing.T) {
	t.Parallel()

	chunks := []rag.RetrievedChunk{{Content: "ctx", SourceName: "src"}}
	// The generator emits a tool_call step; the pipeline must forward it to onTrace
	// after its own retrieval step.
	toolStep := rag.TraceStep{
		Kind:     rag.TraceKindToolCall,
		Label:    "Reading resume.pdf…",
		ToolCall: &rag.ToolCallDetail{Tool: "fetch_full_document", Target: "resume.pdf", Outcome: "ok"},
	}
	gen := &stubGenerator{
		tokens:     []string{"ok"},
		traceSteps: []rag.TraceStep{toolStep},
	}
	pipe := newPipe(stubSearcher{chunks: chunks}, gen)

	var steps []rag.TraceStep
	_, err := pipe.Answer(context.Background(), "", nil, "q?", func(string) error { return nil }, func(step rag.TraceStep) error {
		steps = append(steps, step)
		return nil
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	// Expect the pipeline's retrieval step first, then the generator's tool_call step.
	if len(steps) != 2 {
		t.Fatalf("onTrace steps: got %d, want 2 (retrieval + tool_call)", len(steps))
	}
	if steps[0].Kind != rag.TraceKindRetrieval {
		t.Errorf("steps[0].Kind: got %q, want %q", steps[0].Kind, rag.TraceKindRetrieval)
	}
	if steps[1].Kind != rag.TraceKindToolCall {
		t.Errorf("steps[1].Kind: got %q, want %q", steps[1].Kind, rag.TraceKindToolCall)
	}
	if steps[1].ToolCall == nil || steps[1].ToolCall.Target != "resume.pdf" {
		t.Errorf("steps[1] tool_call target: got %+v, want resume.pdf", steps[1].ToolCall)
	}
}

func TestPipeline_RetrievalStepEmitted(t *testing.T) {
	t.Parallel()

	// Two distinct sources, one duplicate → ChunkCount=3, SourceCount=2.
	chunks := []rag.RetrievedChunk{
		{Content: "alpha", SourceName: "resume.pdf"},
		{Content: "beta", SourceName: "about.html"},
		{Content: "gamma", SourceName: "resume.pdf"},
	}
	gen := &stubGenerator{tokens: []string{"ok"}}
	pipe := newPipe(stubSearcher{chunks: chunks}, gen)

	var steps []rag.TraceStep
	_, err := pipe.Answer(context.Background(), "", nil, "q?", func(string) error { return nil }, func(step rag.TraceStep) error {
		steps = append(steps, step)
		return nil
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if len(steps) == 0 {
		t.Fatal("expected at least the retrieval step, got none")
	}
	r := steps[0]
	if r.Kind != rag.TraceKindRetrieval {
		t.Fatalf("first step kind: got %q, want %q", r.Kind, rag.TraceKindRetrieval)
	}
	if r.Retrieval == nil {
		t.Fatal("retrieval detail is nil")
	}
	if r.Retrieval.ChunkCount != 3 {
		t.Errorf("ChunkCount: got %d, want 3", r.Retrieval.ChunkCount)
	}
	if r.Retrieval.SourceCount != 2 {
		t.Errorf("SourceCount: got %d, want 2", r.Retrieval.SourceCount)
	}
	if len(r.Retrieval.Excerpts) != 3 {
		t.Fatalf("excerpts: got %d, want 3 (one per chunk)", len(r.Retrieval.Excerpts))
	}
	// Excerpts preserve per-chunk source + raw content, in order.
	wantExcerpts := []rag.GroundingExcerpt{
		{SourceName: "resume.pdf", Text: "alpha"},
		{SourceName: "about.html", Text: "beta"},
		{SourceName: "resume.pdf", Text: "gamma"},
	}
	for i, w := range wantExcerpts {
		if r.Retrieval.Excerpts[i] != w {
			t.Errorf("excerpts[%d]: got %+v, want %+v", i, r.Retrieval.Excerpts[i], w)
		}
	}
}

func TestPipeline_ZeroChunkEmitsRetrievalStep(t *testing.T) {
	t.Parallel()

	gen := &stubGenerator{}
	pipe := newPipe(stubSearcher{chunks: nil}, gen)

	var steps []rag.TraceStep
	var tokens []string
	_, err := pipe.Answer(context.Background(), "", nil, "anything?", func(tok string) error {
		tokens = append(tokens, tok)
		return nil
	}, func(step rag.TraceStep) error {
		steps = append(steps, step)
		return nil
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	// Even on the zero-chunk early-return path, exactly the retrieval step fires.
	if len(steps) != 1 {
		t.Fatalf("onTrace steps: got %d, want 1 (retrieval only)", len(steps))
	}
	r := steps[0]
	if r.Kind != rag.TraceKindRetrieval || r.Retrieval == nil {
		t.Fatalf("expected a retrieval step with detail, got %+v", r)
	}
	if r.Retrieval.ChunkCount != 0 {
		t.Errorf("ChunkCount: got %d, want 0", r.Retrieval.ChunkCount)
	}
	if r.Retrieval.SourceCount != 0 {
		t.Errorf("SourceCount: got %d, want 0", r.Retrieval.SourceCount)
	}
	if len(r.Retrieval.Excerpts) != 0 {
		t.Errorf("excerpts: got %d, want 0", len(r.Retrieval.Excerpts))
	}
	// The generator must NOT be called, but the canned token must still be sent.
	if gen.called {
		t.Error("generator must not be called on the zero-chunk path")
	}
	if len(tokens) == 0 || !strings.Contains(strings.Join(tokens, ""), "couldn't find") {
		t.Errorf("expected canned 'couldn't find' message, got %v", tokens)
	}
}

func TestPipeline_ToolFetchedDocumentInCitations(t *testing.T) {
	t.Parallel()

	chunks := []rag.RetrievedChunk{{Content: "ctx", SourceName: "chunks.pdf"}}
	gen := &stubGenerator{
		tokens:       []string{"ok"},
		fetchedNames: []string{"runbook.md"},
	}
	pipe := newPipe(stubSearcher{chunks: chunks}, gen)

	citations, err := pipe.Answer(context.Background(), "", nil, "q?", func(string) error { return nil }, nil)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	// Both the chunk source and the tool-fetched document must appear in citations.
	wantSet := map[string]bool{"chunks.pdf": true, "runbook.md": true}
	if len(citations) != len(wantSet) {
		t.Fatalf("citations: got %v, want %v", citations, []string{"chunks.pdf", "runbook.md"})
	}
	for _, c := range citations {
		if !wantSet[c] {
			t.Errorf("unexpected citation %q", c)
		}
	}
}

func TestPipeline_ToolFetchedDocumentDeduplicatedWithChunks(t *testing.T) {
	t.Parallel()

	// Generator returns the same name that was already in chunks — must not duplicate.
	chunks := []rag.RetrievedChunk{{Content: "ctx", SourceName: "resume.pdf"}}
	gen := &stubGenerator{
		tokens:       []string{"ok"},
		fetchedNames: []string{"resume.pdf"},
	}
	pipe := newPipe(stubSearcher{chunks: chunks}, gen)

	citations, err := pipe.Answer(context.Background(), "", nil, "q?", func(string) error { return nil }, nil)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if len(citations) != 1 || citations[0] != "resume.pdf" {
		t.Errorf("citations: got %v, want [resume.pdf]", citations)
	}
}

func TestPipeline_EmailerFactoryReceivesSessionID(t *testing.T) {
	t.Parallel()

	chunks := []rag.RetrievedChunk{{Content: "ctx", SourceName: "src"}}
	gen := &stubGenerator{tokens: []string{"ok"}}

	const wantSID = "test-session-abc"
	var gotSID string
	factory := func(sid string) rag.EmailSender {
		gotSID = sid
		return &stubEmailSender{}
	}
	pipe := rag.NewPipeline(stubEmbedder{}, stubSearcher{chunks: chunks}, gen, nil, nil, factory, 0, nil)

	_, err := pipe.Answer(context.Background(), wantSID, nil, "q?", func(string) error { return nil }, nil)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if gotSID != wantSID {
		t.Errorf("factory received sessionID %q, want %q", gotSID, wantSID)
	}
	if gen.receivedTools.Emailer == nil {
		t.Error("ToolSet.Emailer must be non-nil when factory is configured")
	}
}

func TestPipeline_EmailerNilWhenNoFactory(t *testing.T) {
	t.Parallel()

	chunks := []rag.RetrievedChunk{{Content: "ctx", SourceName: "src"}}
	gen := &stubGenerator{tokens: []string{"ok"}}
	pipe := newPipe(stubSearcher{chunks: chunks}, gen)

	_, err := pipe.Answer(context.Background(), "", nil, "q?", func(string) error { return nil }, nil)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if gen.receivedTools.Emailer != nil {
		t.Error("ToolSet.Emailer must be nil when no factory is configured")
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

// ---------------------------------------------------------------------------
// stub implementations for DocumentLister / FullTextFetcher / EmailSender
// ---------------------------------------------------------------------------

type stubDocumentLister struct {
	docs []rag.DocumentInfo
}

func (s *stubDocumentLister) ListSources(_ context.Context) ([]rag.DocumentInfo, error) {
	return s.docs, nil
}

type stubFullTextFetcher struct {
	text  string
	found bool
}

func (s *stubFullTextFetcher) GetFullText(_ context.Context, _ string) (string, bool, error) {
	return s.text, s.found, nil
}

type stubEmailSender struct{}

func (s *stubEmailSender) SendContactEmail(_ context.Context, _ email.ContactEmail) (bool, string, error) {
	return true, "", nil
}
