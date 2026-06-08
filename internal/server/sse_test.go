package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

// testFlusher wraps httptest.ResponseRecorder and satisfies http.Flusher so
// tests can verify that writeSSE calls Flush after writing each frame.
type testFlusher struct {
	*httptest.ResponseRecorder

	flushed bool
}

func (f *testFlusher) Flush() { f.flushed = true }

func newTestFlusher() *testFlusher {
	return &testFlusher{ResponseRecorder: httptest.NewRecorder()}
}

// TestSseToken_Format verifies the exact wire encoding of a token event:
//
//	event: token\ndata: {"t":"<value>"}\n\n
//
// It also asserts that Flush is called (SSE requires flushing per frame).
func TestSseToken_Format(t *testing.T) {
	tf := newTestFlusher()

	err := sseToken(tf, tf, "hello")
	if err != nil {
		t.Fatalf("sseToken returned unexpected error: %v", err)
	}

	want := "event: token\ndata: {\"t\":\"hello\"}\n\n"
	got := tf.Body.String()
	if got != want {
		t.Errorf("sseToken body mismatch\n got:  %q\nwant: %q", got, want)
	}

	if !tf.flushed {
		t.Error("sseToken did not call Flush")
	}
}

// TestSseDone_Format verifies the exact wire encoding of a done event with a
// non-empty citations slice.
func TestSseDone_Format(t *testing.T) {
	tf := newTestFlusher()

	err := sseDone(tf, tf, []string{"a.pdf", "b.pdf"})
	if err != nil {
		t.Fatalf("sseDone returned unexpected error: %v", err)
	}

	want := "event: done\ndata: {\"citations\":[\"a.pdf\",\"b.pdf\"]}\n\n"
	got := tf.Body.String()
	if got != want {
		t.Errorf("sseDone body mismatch\n got:  %q\nwant: %q", got, want)
	}
}

// TestSseDone_NilCitations verifies that a nil citations slice is normalised
// to an empty JSON array — never JSON null — so clients always receive a
// consistent type.
func TestSseDone_NilCitations(t *testing.T) {
	tf := newTestFlusher()

	err := sseDone(tf, tf, nil)
	if err != nil {
		t.Fatalf("sseDone returned unexpected error: %v", err)
	}

	body := tf.Body.String()
	if strings.Contains(body, "null") {
		t.Errorf("sseDone with nil citations must not emit null, got: %q", body)
	}
	if !strings.Contains(body, `"citations":[]`) {
		t.Errorf("sseDone with nil citations must emit an empty array, got: %q", body)
	}
}

// TestSseError_Format verifies the wire encoding of an error event: it must
// use the "error" event name and embed the message under the "msg" key.
func TestSseError_Format(t *testing.T) {
	tf := newTestFlusher()

	err := sseError(tf, tf, "oops")
	if err != nil {
		t.Fatalf("sseError returned unexpected error: %v", err)
	}

	body := tf.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("sseError body missing event name, got: %q", body)
	}
	if !strings.Contains(body, `"msg":"oops"`) {
		t.Errorf("sseError body missing msg field, got: %q", body)
	}
}

// TestWriteSSE_MultipleEvents verifies that successive calls to writeSSE
// append independent, correctly formatted frames to the response body.
// Each frame must appear in order and be separated by the double-newline
// terminator required by the SSE protocol.
func TestWriteSSE_MultipleEvents(t *testing.T) {
	tf := newTestFlusher()

	if err := writeSSE(tf, tf, "token", tokenPayload{T: "first"}); err != nil {
		t.Fatalf("first writeSSE call returned unexpected error: %v", err)
	}
	if err := writeSSE(tf, tf, "token", tokenPayload{T: "second"}); err != nil {
		t.Fatalf("second writeSSE call returned unexpected error: %v", err)
	}

	body := tf.Body.String()

	wantFirst := "event: token\ndata: {\"t\":\"first\"}\n\n"
	wantSecond := "event: token\ndata: {\"t\":\"second\"}\n\n"

	if !strings.Contains(body, wantFirst) {
		t.Errorf("body missing first frame\n got:  %q\nwant: %q", body, wantFirst)
	}
	if !strings.Contains(body, wantSecond) {
		t.Errorf("body missing second frame\n got:  %q\nwant: %q", body, wantSecond)
	}

	// Frames must be appended in order.
	firstIdx := strings.Index(body, wantFirst)
	secondIdx := strings.Index(body, wantSecond)
	if firstIdx >= secondIdx {
		t.Errorf("frames are out of order: first at %d, second at %d", firstIdx, secondIdx)
	}
}

// TestSseTrace_Format verifies the wire encoding of a trace event: it must use the
// "trace" event name and embed the structured step under a "step" key. The step's typed
// fields (kind, label, and the matching detail object) must round-trip through the frame.
func TestSseTrace_Format(t *testing.T) {
	tf := newTestFlusher()

	step := rag.TraceStep{
		Kind:  rag.TraceKindRetrieval,
		Label: "Searched knowledge base",
		Retrieval: &rag.RetrievalDetail{
			ChunkCount:  2,
			SourceCount: 1,
			Excerpts:    []rag.GroundingExcerpt{{SourceName: "resume.pdf", Text: "hello"}},
		},
	}

	if err := sseTrace(tf, tf, step); err != nil {
		t.Fatalf("sseTrace returned unexpected error: %v", err)
	}

	body := tf.Body.String()
	if !strings.Contains(body, "event: trace") {
		t.Errorf("sseTrace body missing event name, got: %q", body)
	}
	if !strings.Contains(body, `"kind":"retrieval"`) {
		t.Errorf("sseTrace body missing kind field, got: %q", body)
	}
	if !strings.Contains(body, `"chunk_count":2`) {
		t.Errorf("sseTrace body missing retrieval detail, got: %q", body)
	}
	if !strings.Contains(body, `"source_name":"resume.pdf"`) {
		t.Errorf("sseTrace body missing grounding excerpt, got: %q", body)
	}
	if !tf.flushed {
		t.Error("sseTrace did not call Flush")
	}

	// The frame must be parseable back into the same step (data: <json> line).
	const dataPrefix = "data: "
	var jsonLine string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, dataPrefix) {
			jsonLine = strings.TrimPrefix(line, dataPrefix)
			break
		}
	}
	if jsonLine == "" {
		t.Fatalf("no data line found in frame: %q", body)
	}
	var got tracePayload
	if err := json.Unmarshal([]byte(jsonLine), &got); err != nil {
		t.Fatalf("unmarshal trace payload: %v", err)
	}
	if got.Step.Kind != rag.TraceKindRetrieval {
		t.Errorf("round-trip kind: got %q, want %q", got.Step.Kind, rag.TraceKindRetrieval)
	}
	if got.Step.Retrieval == nil || got.Step.Retrieval.ChunkCount != 2 {
		t.Errorf("round-trip retrieval detail mismatch: %+v", got.Step.Retrieval)
	}
}
