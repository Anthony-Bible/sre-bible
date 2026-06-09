package eval

import (
	"context"
	"errors"
	"testing"
)

// stubJudge is a test double implementing Judge that returns a configured
// verdict or error on every Score call. It records invocation count for
// assertion convenience.
type stubJudge struct {
	verdict    JudgeVerdict
	err        error
	callCount  int
	refused    bool
	refusalErr error
}

func (s *stubJudge) Score(_ context.Context, _, _, _, _ string) (JudgeVerdict, error) {
	s.callCount++
	return s.verdict, s.err
}

func (s *stubJudge) IsRefusal(_ context.Context, _, _ string) (bool, error) {
	return s.refused, s.refusalErr
}

func TestStubJudge_ReturnsConfiguredVerdict(t *testing.T) {
	want := JudgeVerdict{Score: 0.85, Rationale: "well grounded"}
	j := &stubJudge{verdict: want}

	got, err := j.Score(context.Background(), "ctx", "q", "a", "rubric")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Score != want.Score {
		t.Errorf("Score: got %v, want %v", got.Score, want.Score)
	}
	if got.Rationale != want.Rationale {
		t.Errorf("Rationale: got %q, want %q", got.Rationale, want.Rationale)
	}
	if j.callCount != 1 {
		t.Errorf("callCount: got %d, want 1", j.callCount)
	}
}

func TestStubJudge_PropagatesError(t *testing.T) {
	sentinel := errors.New("judge unavailable")
	j := &stubJudge{err: sentinel}

	_, err := j.Score(context.Background(), "ctx", "q", "a", "rubric")
	if !errors.Is(err, sentinel) {
		t.Errorf("error: got %v, want %v", err, sentinel)
	}
}

// compile-time assertion: stubJudge satisfies the Judge interface.
var _ Judge = (*stubJudge)(nil)
