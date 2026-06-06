package server

import (
	"net/http/httptest"
	"strings"
	"testing"
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
