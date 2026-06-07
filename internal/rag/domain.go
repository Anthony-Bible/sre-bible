package rag

import (
	"context"

	"github.com/Anthony-Bible/sre-bible/internal/email"
)

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

// DocumentInfo is the summary of a Source returned by ListSources.
type DocumentInfo struct {
	Name string
	Type string
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

// ToolSet carries optional tool-use dependencies for the LLM generation step.
// A nil field means the corresponding tool is not advertised to the model.
type ToolSet struct {
	Lister  DocumentLister
	Fetcher FullTextFetcher
	Emailer EmailSender
}

// Generator streams a grounded answer token by token via onToken callback.
// It receives fully assembled messages (history + enriched current turn).
// tools provides document-access capabilities for multi-round tool use.
// onStatus, if non-nil, is called with transient status messages during tool rounds.
// If onToken returns an error the stream must abort and that error is returned.
// The returned []string contains the names of any documents fetched via tool use,
// so callers can include them in citations alongside vector-retrieved chunks.
type Generator interface {
	StreamAnswer(ctx context.Context, messages []Message, tools ToolSet, onToken func(string) error, onStatus func(string) error) ([]string, error)
}
