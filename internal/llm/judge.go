package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// judgeModel is the Anthropic model the interview judge runs on. Pinned to Haiku
// rather than read from CLAUDE_MODEL: the parent agent may be running a larger
// model, but grading is a fixed-cost, low-latency step and we don't want
// production model upgrades to drag the judge's cost/latency along with them.
const judgeModel = "claude-haiku-4-5-20251001"

// judgeMaxTokens caps the judge response. The result is a tiny JSON object
// (≤ ~150 tokens of structured payload); 512 leaves headroom for short feedback.
const judgeMaxTokens = 512

// judgeRecordToolName is the forced tool the judge model is required to call.
// Using forced tool-use is Anthropic's structured-output mechanism: it
// guarantees the model returns a parseable object matching our schema rather
// than free text we'd have to regex.
const judgeRecordToolName = "record_evaluation"

// judgeSystemPrompt is the strict grader persona. It pins behavior so the model
// returns structured output via the record_evaluation tool and never converses
// back at the candidate (that's the parent agent's job).
const judgeSystemPrompt = `You are an SRE Principal Lead Interviewer grading a single candidate answer for a reverse-screening interview.

You will be given:
- A scenario question (one of three pre-defined SRE incident scenarios).
- The expected high-value concepts for that scenario (the rubric).
- The candidate's answer.

Your job: score the answer 0-100 based on conceptual coverage and operational realism, and explain the score in 1-3 sentences of feedback the parent agent will relay to the candidate.

Scoring guide:
- 85-100: covers most rubric concepts AND demonstrates operational judgement (sequencing, tradeoffs, blast-radius awareness). Cite their strong moves.
- 70-84: covers core rubric concepts but misses depth or sequencing nuance.
- 40-69: partial coverage; touches the right area but leaves obvious gaps.
- 0-39: low-effort, off-topic, "I don't know", or wrong direction.

Never reward keyword-stuffing — an answer that lists buzzwords without explaining how they apply scores in the 40-69 band, not above. Reward concrete, scenario-fit reasoning even when the candidate's vocabulary differs from the rubric.

Return your decision by calling the record_evaluation tool exactly once. Do not produce any plain-text reply.`

// judgeRubric is the rubric block injected into the user turn for each scenario.
// These are guidance for the judge model — NOT keyword-match logic. The judge
// is explicitly told (in the system prompt) to value scenario-fit reasoning
// over vocabulary matches.
var judgeRubric = map[int]string{
	rag.InterviewScenarioCascadeCacheStampede: `Scenario 0 — Cascading failure / cache stampede during a flash sale.
High-value concepts: cache stampede mitigation (singleflight / request coalescing / cache locks), graceful degradation, circuit breakers on downstream DB calls, exponential backoff with jitter on retries, rate-limiting upstream, load shedding, warming/pre-populating the cache, observability into cache hit ratio.
Strong answers sequence stabilization (stop the bleed → protect the DB → re-warm cache) and call out the retry-storm risk explicitly.`,
	rag.InterviewScenarioBGPDNS: `Scenario 1 — BGP route leak / DNS hijack: a third-party payment provider is unreachable.
High-value concepts: structured triage (is it our network, our DNS, or their transit?), tools like dig / mtr / traceroute / looking glasses, checking RIPE/RouteViews for BGP changes, comparing resolution across resolvers, failover to a secondary provider, circuit-breaking the payment dependency, customer-facing graceful degradation.
Strong answers explicitly isolate the failure domain before mitigating and avoid the trap of assuming it's "always DNS" without evidence.`,
	rag.InterviewScenarioServerlessColdStart: `Scenario 2 — Serverless cold starts exhausting a Postgres connection pool at peak traffic.
High-value concepts: connection pooling at the edge (pgbouncer / RDS Proxy / Cloud SQL Auth Proxy), tuning serverless concurrency / reserved-concurrency caps, read replicas for query offload, throttling / rate-limiting at the API edge, pre-warming connections, switching short-lived connections to a transaction-pooled model, batching writes.
Strong answers identify that "more replicas" without a connection-pooling layer just multiplies the problem, and they consider both the immediate mitigation and the structural fix.`,
}

// judgeRecordTool is the forced-tool schema definition. Hoisted to package level
// because it is immutable for the lifetime of the process — re-allocating the
// nested property maps on every grading call would be wasted work.
var judgeRecordTool = anthropic.ToolParam{
	Name:        judgeRecordToolName,
	Description: anthropic.String("Record your evaluation of the candidate's answer."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"score": map[string]any{
				schemaType:        "integer",
				schemaDescription: "Integer score from 0 to 100.",
			},
			"feedback": map[string]any{
				schemaType:        schemaTypeString,
				schemaDescription: "1-3 sentences of feedback for the candidate, explaining what they hit and what they missed.",
			},
			"passed": map[string]any{
				schemaType:        "boolean",
				schemaDescription: "True if the candidate cleared the bar for this scenario (>=60).",
			},
			"concepts_demonstrated": map[string]any{
				schemaType:        schemaTypeArray,
				schemaDescription: "Short labels (1-4 words each) for the rubric concepts the candidate actually demonstrated. Empty array if none.",
				schemaItems:       map[string]any{schemaType: schemaTypeString},
			},
		},
		Required: []string{"score", "feedback", "passed", "concepts_demonstrated"},
	},
}

// judgeForceTool pins the model to call judgeRecordTool exactly once.
var judgeForceTool = anthropic.ToolChoiceToolParam{Name: judgeRecordToolName}

// Judge is the Anthropic-backed implementation of rag.Judge. It holds a thin
// SDK client and a pinned model; it is independent of llm.Client so the judge's
// model and prompt cannot drift when the parent agent is reconfigured.
type Judge struct {
	inner *anthropic.Client
	model string
	log   *slog.Logger
}

// NewJudge creates an interview judge backed by Claude Haiku.
func NewJudge(apiKey string, log *slog.Logger) *Judge {
	if log == nil {
		log = slog.Default()
	}
	c := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &Judge{
		inner: &c,
		model: judgeModel,
		log:   log,
	}
}

// EvaluateAnswer issues one structured Claude Haiku call and returns the parsed
// InterviewEvaluation. The judge's tool wrapper (runEvaluateInterviewAnswer)
// further clamps and trims the result; this method is responsible only for the
// model call and JSON parsing.
//
// The raw userAnswer is sent to the model in this call (it has to be — the model
// is grading it) but is never logged, stored, or surfaced in traces by this code.
func (j *Judge) EvaluateAnswer(ctx context.Context, qIdx int, qText, userAnswer string) (*rag.InterviewEvaluation, error) {
	rubric, ok := judgeRubric[qIdx]
	if !ok {
		return nil, fmt.Errorf("judge: no rubric for question_index=%d", qIdx)
	}

	userBlock := fmt.Sprintf(
		"RUBRIC:\n%s\n\nSCENARIO QUESTION:\n%s\n\nCANDIDATE ANSWER:\n%s\n\nGrade this answer now by calling record_evaluation.",
		rubric, qText, userAnswer,
	)

	req := anthropic.MessageNewParams{
		Model:     j.model,
		MaxTokens: judgeMaxTokens,
		// Pin temperature=0 — grading should be stable, not creative.
		Temperature: anthropic.Float(0),
		System: []anthropic.TextBlockParam{
			{Text: judgeSystemPrompt},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userBlock)),
		},
		Tools:      []anthropic.ToolUnionParam{{OfTool: &judgeRecordTool}},
		ToolChoice: anthropic.ToolChoiceUnionParam{OfTool: &judgeForceTool},
	}

	resp, err := j.inner.Messages.New(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("judge: anthropic call: %w", err)
	}

	for _, block := range resp.Content {
		tu := block.AsToolUse()
		if tu.Name != judgeRecordToolName {
			continue
		}
		var parsed struct {
			Score                int      `json:"score"`
			Feedback             string   `json:"feedback"`
			Passed               bool     `json:"passed"`
			ConceptsDemonstrated []string `json:"concepts_demonstrated"`
		}
		if err := json.Unmarshal(tu.Input, &parsed); err != nil {
			return nil, fmt.Errorf("judge: parse tool input: %w", err)
		}
		// Trimming/clamping is the wrapper's job (normalizeEvaluation) — it's the
		// single safety gate before the result reaches the agent loop, so doing it
		// here too would just duplicate work and risk drift between the two paths.
		return &rag.InterviewEvaluation{
			Score:                parsed.Score,
			Feedback:             parsed.Feedback,
			Passed:               parsed.Passed,
			ConceptsDemonstrated: parsed.ConceptsDemonstrated,
		}, nil
	}
	return nil, fmt.Errorf("judge: model did not call %s", judgeRecordToolName)
}
