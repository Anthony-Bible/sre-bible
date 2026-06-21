package eval

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// RecordingSearcher wraps a ChunkSearcher and records the chunks returned by
// the most recent SearchChunks call. It satisfies rag.ChunkSearcher.
type RecordingSearcher struct {
	inner  rag.ChunkSearcher
	Chunks []rag.RetrievedChunk
}

// SearchChunks delegates to the inner searcher and stores the result.
func (r *RecordingSearcher) SearchChunks(ctx context.Context, queryEmbedding []float32, limit int) ([]rag.RetrievedChunk, error) {
	chunks, err := r.inner.SearchChunks(ctx, queryEmbedding, limit)
	if err != nil {
		return nil, err
	}
	r.Chunks = chunks
	return chunks, nil
}

// Runner executes golden cases through the RAG pipeline and scores the results.
type Runner struct {
	embedder  rag.QueryEmbedder
	searcher  rag.ChunkSearcher
	generator rag.Generator
	lister    rag.DocumentLister
	fetcher   rag.FullTextFetcher
	judge     Judge
	k         int // retrieval depth; 0 → rag's default (top-k=8)
	log       *slog.Logger
}

// RunnerOption configures an optional Runner behavior.
type RunnerOption func(*Runner)

// WithRunnerK overrides the retrieval depth (top-k) the Runner's pipeline uses.
// A non-positive k leaves the rag default (8) in place. The chunking sweep uses
// this to vary k per config; cmd/eval passes no option and keeps k=8.
func WithRunnerK(k int) RunnerOption {
	return func(r *Runner) {
		if k > 0 {
			r.k = k
		}
	}
}

// NewRunner creates a Runner. judge may be nil; when nil, judge-based scoring
// is skipped regardless of whether a JudgeRubric is set on a case.
func NewRunner(
	embedder rag.QueryEmbedder,
	searcher rag.ChunkSearcher,
	generator rag.Generator,
	lister rag.DocumentLister,
	fetcher rag.FullTextFetcher,
	judge Judge,
	log *slog.Logger,
	opts ...RunnerOption,
) *Runner {
	if log == nil {
		log = slog.Default()
	}
	r := &Runner{
		embedder:  embedder,
		searcher:  searcher,
		generator: generator,
		lister:    lister,
		fetcher:   fetcher,
		judge:     judge,
		log:       log,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run executes a single GoldenCase through the RAG pipeline and returns the
// raw Result. It does not score; call Score on the returned Result to get a
// ScoredResult.
func (r *Runner) Run(ctx context.Context, c GoldenCase) Result {
	rec := &RecordingSearcher{inner: r.searcher}

	var toolCalls []string
	onToolCall := func(toolName string) {
		toolCalls = append(toolCalls, toolName)
	}

	matcher := rag.NewMatcher(r.embedder, rec)

	pipe := rag.NewPipeline(
		r.embedder,
		rec,
		r.generator,
		r.lister,
		r.fetcher,
		matcher,
		nil, // emailerFor — not used in eval
		r.k, // 0 → rag default (top-k=8); sweep overrides via WithRunnerK
		r.log,
		rag.WithOnToolCall(onToolCall),
	)

	var sb strings.Builder
	collectToken := func(tok string) error {
		sb.WriteString(tok)
		return nil
	}

	sessionID := "eval-session-" + c.ID
	citations, err := pipe.Answer(ctx, sessionID, nil, c.Question, collectToken, nil)

	// Convert rag.RetrievedChunk → RetrievedChunkRecord.
	chunks := make([]RetrievedChunkRecord, 0, len(rec.Chunks))
	retrievedSources := make([]string, 0, len(rec.Chunks))
	for _, ch := range rec.Chunks {
		chunks = append(chunks, RetrievedChunkRecord{
			Content:    ch.Content,
			SourceName: ch.SourceName,
		})
		retrievedSources = append(retrievedSources, ch.SourceName)
	}

	// The agent's full answer and the source names in the retrieved top-k
	// together explain every score: the answer is the Claude agent's raw output,
	// and the retrieved sources reveal whether distractor documents are genuinely
	// competing for the slots (the expected source winning over topical
	// distractors is the signal the retrieval_check gate is meant to measure).
	// Both are logged at debug so a normal run stays quiet; enable with
	// EVAL_DEBUG. The Gemini judge's verdict is logged separately by the judge
	// (see internal/eval/judge.go), so an EVAL_DEBUG run shows both models.
	r.log.DebugContext(ctx, "eval: agent answer",
		"case_id", c.ID,
		"question", c.Question,
		"answer", sb.String(),
	)
	r.log.DebugContext(ctx, "eval: retrieved chunk sources",
		"case_id", c.ID,
		"retrieved_sources", retrievedSources,
	)

	return Result{
		Case:            c,
		Answer:          sb.String(),
		Citations:       citations,
		RetrievedChunks: chunks,
		ToolCallsSeen:   toolCalls,
		Error:           err,
	}
}

// buildContextBlock formats chunks as XML-like snippet for the LLM judge.
func buildContextBlock(chunks []RetrievedChunkRecord) string {
	var sb strings.Builder
	for _, c := range chunks {
		fmt.Fprintf(&sb, "<chunk source=%q>%s</chunk>\n", c.SourceName, c.Content)
	}
	return sb.String()
}

// refusalCorrect decides whether the answer's refusal behaviour matches the
// case expectation. For cases that expect a refusal it prefers the semantic
// judge: the agent declines in several wordings (an off-topic sentinel and
// tailored PII redirects) that the single-phrase keyword heuristic misses,
// causing false negatives. The deterministic heuristic (RefusalCorrect) is
// used when no refusal is expected (cheap, and the only behaviour worth a
// hard substring check), when no judge is configured, or as a fallback when
// the judge call fails — so a judge outage degrades to the old behaviour
// rather than crashing the gate.
func (r *Runner) refusalCorrect(ctx context.Context, c GoldenCase, answer string) bool {
	if !c.ExpectedRefusal || r.judge == nil {
		return RefusalCorrect(answer, c.ExpectedRefusal)
	}
	refused, err := r.judge.IsRefusal(ctx, c.Question, answer)
	if err != nil {
		r.log.WarnContext(ctx, "refusal judge failed; falling back to keyword heuristic",
			"case_id", c.ID,
			"error", err,
		)
		return RefusalCorrect(answer, c.ExpectedRefusal)
	}
	return refused == c.ExpectedRefusal
}

// Score computes all scoring dimensions for a Result and returns a ScoredResult.
func (r *Runner) Score(ctx context.Context, result Result) ScoredResult {
	c := result.Case

	// A pipeline error means there is no real answer to score. Never let an
	// errored run count as a pass: an empty answer otherwise satisfies the
	// check-light categories (e.g. contact_flow, which only has must-not-contain),
	// silently inflating that gate. Fail it visibly instead.
	if result.Error != nil {
		return ScoredResult{
			Result: result,
			Score:  ScoreDetail{RecallScore: -1, GroundScore: -1, JudgeSkipped: true},
			Pass:   false,
			Notes:  "pipeline error: " + result.Error.Error(),
		}
	}

	recallScore := ScoreRecall(c.ExpectedSourceNames, result.RetrievedChunks)
	refusalPass := r.refusalCorrect(ctx, c, result.Answer)
	mustNotPass := MustNotContainPass(result.Answer, c.MustNotContain)
	toolCallsOK := ToolCallsPresent(c.ExpectedToolCalls, result.ToolCallsSeen)

	groundScore := float64(-1)
	judgeSkipped := true

	if c.JudgeRubric != "" && r.judge != nil {
		ctxBlock := buildContextBlock(result.RetrievedChunks)
		verdict, err := r.judge.Score(ctx, ctxBlock, c.Question, result.Answer, c.JudgeRubric)
		if err != nil {
			r.log.WarnContext(ctx, "judge scoring failed; skipping",
				"case_id", c.ID,
				"error", err,
			)
			// judgeSkipped stays true (its initial value); groundScore stays -1.
		} else {
			groundScore = verdict.Score
			judgeSkipped = false
		}
	}

	// Determine pass: all applicable checks must pass.
	// RecallScore of -1 means unchecked (skip gate). Otherwise must be >= 0.5.
	recallOK := recallScore < 0 || recallScore >= 0.5

	pass := refusalPass && mustNotPass && toolCallsOK && recallOK

	// Build human-readable failure notes.
	var failures []string
	if !refusalPass {
		failures = append(failures, "refusal expectation mismatch")
	}
	if !mustNotPass {
		failures = append(failures, "must-not-contain violated")
	}
	if !toolCallsOK {
		failures = append(failures, "required tool calls not seen")
	}
	if !recallOK {
		failures = append(failures, fmt.Sprintf("recall %.2f < 0.50", recallScore))
	}
	notes := strings.Join(failures, "; ")

	return ScoredResult{
		Result: result,
		Score: ScoreDetail{
			RecallScore:  recallScore,
			RefusalPass:  refusalPass,
			MustNotPass:  mustNotPass,
			GroundScore:  groundScore,
			JudgeSkipped: judgeSkipped,
		},
		Pass:  pass,
		Notes: notes,
	}
}
