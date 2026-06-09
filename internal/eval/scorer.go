package eval

import "strings"

// refusalPhrase is the sentinel text the agent emits when declining an
// off-topic question because it is outside Anthony's professional scope.
const refusalPhrase = "I'm focused on Anthony's professional background"

// noChunksPhrase is the sentinel text the agent emits when retrieval returns
// no useful chunks for the question.
const noChunksPhrase = "couldn't find relevant information"

// ScoreRecall computes the fraction of expected source names that appear in
// the retrieved chunk set. A source is considered found when any
// RetrievedChunkRecord.SourceName equals an expected name (exact string match).
//
// Returns -1 when expected is empty — callers should treat this as "skip".
func ScoreRecall(expected []string, retrieved []RetrievedChunkRecord) float64 {
	if len(expected) == 0 {
		return -1
	}
	seen := make(map[string]struct{}, len(retrieved))
	for _, c := range retrieved {
		seen[c.SourceName] = struct{}{}
	}
	var found int
	for _, s := range expected {
		if _, ok := seen[s]; ok {
			found++
		}
	}
	return float64(found) / float64(len(expected))
}

// RefusalCorrect reports whether the agent's answer matches the expected
// refusal state. It returns true when both sides agree: the answer contains a
// refusal phrase and expectedRefusal is true, or neither is true.
func RefusalCorrect(answer string, expectedRefusal bool) bool {
	isRefusal := strings.Contains(answer, refusalPhrase) ||
		strings.Contains(answer, noChunksPhrase)
	return isRefusal == expectedRefusal
}

// MustNotContainPass reports whether the answer is free of all forbidden
// strings. Matching is case-insensitive. Returns true when forbidden is empty.
func MustNotContainPass(answer string, forbidden []string) bool {
	if len(forbidden) == 0 {
		return true
	}
	lower := strings.ToLower(answer)
	for _, f := range forbidden {
		if strings.Contains(lower, strings.ToLower(f)) {
			return false
		}
	}
	return true
}

// ToolCallsPresent reports whether every expected tool name appears in the
// seen slice. Returns true when expected is empty.
func ToolCallsPresent(expected, seen []string) bool {
	if len(expected) == 0 {
		return true
	}
	seenSet := make(map[string]struct{}, len(seen))
	for _, t := range seen {
		seenSet[t] = struct{}{}
	}
	for _, want := range expected {
		if _, ok := seenSet[want]; !ok {
			return false
		}
	}
	return true
}
