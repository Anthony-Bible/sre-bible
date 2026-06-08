package gemini

import (
	"strings"
	"testing"
)

func TestCheckPIIDrift(t *testing.T) {
	t.Parallel()

	// Build a string of n runes for use as test input.
	makeRunes := func(n int) string { return strings.Repeat("a", n) }

	cases := []struct {
		name     string
		original string
		screened string
		wantErr  bool
	}{
		{
			name:     "identical output passes",
			original: makeRunes(1000),
			screened: makeRunes(1000),
			wantErr:  false,
		},
		{
			name:     "output at exactly 70% passes",
			original: makeRunes(1000),
			screened: makeRunes(700),
			wantErr:  false,
		},
		{
			name:     "output one rune above threshold passes",
			original: makeRunes(1000),
			screened: makeRunes(701),
			wantErr:  false,
		},
		{
			name:     "output one rune below threshold fails",
			original: makeRunes(1000),
			screened: makeRunes(699),
			wantErr:  true,
		},
		{
			name:     "drastically short output fails",
			original: makeRunes(1000),
			screened: makeRunes(10),
			wantErr:  true,
		},
		{
			name:     "empty original is always ok",
			original: "",
			screened: "",
			wantErr:  false,
		},
		{
			name:     "empty original with non-empty screened is ok",
			original: "",
			screened: makeRunes(100),
			wantErr:  false,
		},
		{
			name:     "small redaction within threshold passes",
			original: makeRunes(100),
			screened: makeRunes(80), // 80% — well above 70%
			wantErr:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkPIIDrift(tc.original, tc.screened)
			if tc.wantErr && err == nil {
				t.Error("checkPIIDrift() returned nil; want error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("checkPIIDrift() returned unexpected error: %v", err)
			}
		})
	}
}
