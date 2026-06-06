package rag

import "context"

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

// QueryEmbedder converts a Viewer's question into a 768-dim query vector.
// MUST use RETRIEVAL_QUERY task type — not RETRIEVAL_DOCUMENT.
type QueryEmbedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// ChunkSearcher finds the k most semantically similar RetrievedChunks via cosine similarity.
type ChunkSearcher interface {
	SearchChunks(ctx context.Context, queryEmbedding []float32, limit int) ([]RetrievedChunk, error)
}

// Generator streams a grounded answer token by token via callback.
// It receives fully assembled messages (history + enriched current turn).
// If onToken returns an error the stream must abort and that error is returned.
type Generator interface {
	StreamAnswer(ctx context.Context, messages []Message, onToken func(string) error) error
}
