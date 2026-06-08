package rag

import (
	"context"

	"github.com/Anthony-Bible/sre-bible/internal/email"
)

// PersonaMode discriminates between standard and Deadpool persona voices.
type PersonaMode string

const (
	ModeStandard PersonaMode = "standard"
	ModeDeadpool PersonaMode = "deadpool"
)

type contextKey string

const (
	personaModeKey contextKey = "persona_mode"
)

// WithPersonaMode returns a new context carrying the specified PersonaMode.
func WithPersonaMode(ctx context.Context, mode PersonaMode) context.Context {
	return context.WithValue(ctx, personaModeKey, mode)
}

// PersonaModeFromContext extracts the PersonaMode from the context, defaulting to ModeStandard if not present or invalid.
func PersonaModeFromContext(ctx context.Context) PersonaMode {
	if v, ok := ctx.Value(personaModeKey).(PersonaMode); ok {
		switch v {
		case ModeStandard, ModeDeadpool:
			return v
		}
	}
	return ModeStandard
}

// EmailSender sends a single contact email on a Viewer's behalf.
// ok=false + reason = expected, user-relayable refusal (rate limit, validation,
// already sent, delivery failure). err != nil = internal failure (never shown raw).
type EmailSender interface {
	SendContactEmail(ctx context.Context, e email.ContactEmail) (ok bool, reason string, err error)
}

// EmailerFactory creates a session-bound EmailSender.
// A nil factory means the send_contact_email tool is not advertised to the model.
type EmailerFactory func(sessionID string) EmailSender

// Role is the participant in a conversation turn.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is a single conversational turn in a Session.
type Message struct {
	Role    Role
	Content string
}

// RetrievedChunk is a Chunk recovered from the vector store during Retrieval,
// annotated with its Source citation name for attribution.
type RetrievedChunk struct {
	Content    string
	SourceName string
}

// TraceStepKind discriminates the variants of a TraceStep.
type TraceStepKind string

const (
	// TraceKindRetrieval is the vector-search step: chunk/source counts + grounding excerpts.
	TraceKindRetrieval TraceStepKind = "retrieval"
	// TraceKindToolCall is a single model-invoked tool round (list/fetch/email).
	TraceKindToolCall TraceStepKind = "tool_call"
	// TraceKindAnswer is the terminal step: total tool rounds + wall-clock duration.
	TraceKindAnswer TraceStepKind = "answer"
	// TraceKindNotice is a transient user-facing status message (e.g. "still searching…").
	// Carries only Label — no detail struct — and is fired from long-running tool calls
	// so the UI can surface progress without blocking on the tool result.
	TraceKindNotice TraceStepKind = "notice"
)

// TraceStep is one entry in an Agent Trace — the persisted, ordered record of how
// the Resume Agent produced an answer. A single flat struct with a Kind discriminator
// and optional pointer detail structs serialises to one ordered JSONB array. The typed
// detail structs act as a PII allow-list by construction: only fields named here are
// ever stored. Exactly one detail pointer is non-nil, matching Kind.
type TraceStep struct {
	Kind      TraceStepKind    `json:"kind"`
	Label     string           `json:"label"`
	Retrieval *RetrievalDetail `json:"retrieval,omitempty"`
	ToolCall  *ToolCallDetail  `json:"tool_call,omitempty"`
	Answer    *AnswerDetail    `json:"answer,omitempty"`
}

// RetrievalDetail records the vector-search outcome for a TraceStep of kind retrieval.
// Excerpts is empty (never nil after construction) on the zero-chunk path.
type RetrievalDetail struct {
	ChunkCount  int                `json:"chunk_count"`
	SourceCount int                `json:"source_count"`
	Excerpts    []GroundingExcerpt `json:"excerpts"`
}

// GroundingExcerpt is the exact retrieved chunk text that grounded an answer, attributed
// to its Source. Text is the only field carrying document content in a trace — intended,
// as it is the same chunk.Content already sent to the model as context.
type GroundingExcerpt struct {
	SourceName string `json:"source_name"`
	Text       string `json:"text"`
}

// ToolCallDetail records one model-invoked tool round for a TraceStep of kind tool_call.
// Target is a SAFE specific (a document name only) and is EMPTY for send_contact_email —
// the Viewer's email, draft, and reason are never recorded.
type ToolCallDetail struct {
	Tool    string `json:"tool"`
	Target  string `json:"target,omitempty"`
	Outcome string `json:"outcome"` // ok | error | not_found | refused
}

// AnswerDetail records the terminal answer step: how many tool rounds ran and how long
// generation took end to end.
type AnswerDetail struct {
	ToolRounds int   `json:"tool_rounds"`
	DurationMs int64 `json:"duration_ms,omitempty"`
}

// DocumentInfo is the summary of a Source returned by ListSources.
type DocumentInfo struct {
	Name        string
	Type        string
	Description string // empty for legacy rows
}

// String formats a DocumentInfo for display as "name (type)" or "name (type): description".
func (d DocumentInfo) String() string {
	s := d.Name + " (" + d.Type + ")"
	if d.Description != "" {
		return s + ": " + d.Description
	}
	return s
}

// QueryEmbedder converts a Viewer's question into a 768-dim query vector.
// MUST use RETRIEVAL_QUERY task type — not RETRIEVAL_DOCUMENT.
type QueryEmbedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// ChunkSearcher finds the k most semantically similar RetrievedChunks via cosine similarity.
type ChunkSearcher interface {
	SearchChunks(ctx context.Context, queryEmbedding []float32, limit int) ([]RetrievedChunk, error)
}

// DocumentLister lists all Sources in the knowledge base.
// Consumed by the LLM tool loop; defined here per "accept interfaces" guideline.
type DocumentLister interface {
	ListSources(ctx context.Context) ([]DocumentInfo, error)
}

// FullTextFetcher retrieves the complete extracted text for a named Source.
// found is false when the source is absent or has no stored full text.
type FullTextFetcher interface {
	GetFullText(ctx context.Context, name string) (text string, found bool, err error)
}

// JobMatcher retrieves the RetrievedChunks most relevant to a single Job
// Description Requirement, for the match_job_description tool. k is the number of
// chunks to retrieve; k<=0 selects an implementation default. An empty result is
// normal and signals a Gap (no corpus evidence) to the caller — never an error.
type JobMatcher interface {
	MatchRequirement(ctx context.Context, requirement string, k int) ([]RetrievedChunk, error)
}

// ToolSet carries optional tool-use dependencies for the LLM generation step.
// A nil field means the corresponding tool is not advertised to the model.
type ToolSet struct {
	Lister  DocumentLister
	Fetcher FullTextFetcher
	Emailer EmailSender
	Matcher JobMatcher
}

// Generator streams a grounded answer token by token via onToken callback.
// It receives fully assembled messages (history + enriched current turn).
// tools provides document-access capabilities for multi-round tool use.
// onTrace, if non-nil, is called with each TraceStep the generator produces during
// generation: one tool_call step per tool round, then a terminal answer step.
// If onToken returns an error the stream must abort and that error is returned.
// The returned []string contains the names of any documents fetched via tool use,
// so callers can include them in citations alongside vector-retrieved chunks.
type Generator interface {
	StreamAnswer(ctx context.Context, messages []Message, tools ToolSet, onToken func(string) error, onTrace func(TraceStep) error) ([]string, error)
}
