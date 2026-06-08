package rag_test

import (
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/rag"
)

func TestDedupeSourceNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil", in: nil, want: []string{}},
		{name: "empty", in: []string{}, want: []string{}},
		{name: "no dups preserves order", in: []string{"a.pdf", "b.html", "c.txt"}, want: []string{"a.pdf", "b.html", "c.txt"}},
		{name: "dups keep first-seen order", in: []string{"resume.pdf", "about.html", "resume.pdf"}, want: []string{"resume.pdf", "about.html"}},
		{name: "all identical collapse to one", in: []string{"x", "x", "x"}, want: []string{"x"}},
		{name: "interleaved dups", in: []string{"a", "b", "a", "c", "b"}, want: []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := rag.DedupeSourceNames(tc.in)
			if got == nil {
				t.Fatal("result must be non-nil even for empty input")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len: got %v, want %v", got, tc.want)
			}
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("[%d]: got %q, want %q", i, got[i], w)
				}
			}
		})
	}
}
