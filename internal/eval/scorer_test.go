package eval

import "testing"

// ---------------------------------------------------------------------------
// ScoreRecall
// ---------------------------------------------------------------------------

func TestScoreRecall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		expected  []string
		retrieved []RetrievedChunkRecord
		want      float64
		pass      bool // true = expected "passing" scenario, false = "failing"
	}{
		// --- passing cases ---
		{
			name:     "empty expected returns -1 (skip)",
			expected: []string{},
			retrieved: []RetrievedChunkRecord{
				{SourceName: "resume.pdf"},
			},
			want: -1,
			pass: true,
		},
		{
			name:     "all expected sources found returns 1.0",
			expected: []string{"resume.pdf", "linkedin.html"},
			retrieved: []RetrievedChunkRecord{
				{SourceName: "resume.pdf"},
				{SourceName: "linkedin.html"},
				{SourceName: "other.pdf"},
			},
			want: 1.0,
			pass: true,
		},
		{
			name:     "half expected sources found returns 0.5",
			expected: []string{"resume.pdf", "missing.pdf"},
			retrieved: []RetrievedChunkRecord{
				{SourceName: "resume.pdf"},
			},
			want: 0.5,
			pass: true,
		},
		// --- failing cases ---
		{
			name:     "no expected sources found returns 0.0 not -1",
			expected: []string{"resume.pdf"},
			retrieved: []RetrievedChunkRecord{
				{SourceName: "other.pdf"},
			},
			want: 0.0,
			pass: false,
		},
		{
			name:      "empty retrieved returns 0.0",
			expected:  []string{"resume.pdf"},
			retrieved: []RetrievedChunkRecord{},
			want:      0.0,
			pass:      false,
		},
		{
			name:     "partial match returns fraction not 1.0",
			expected: []string{"a.pdf", "b.pdf", "c.pdf"},
			retrieved: []RetrievedChunkRecord{
				{SourceName: "a.pdf"},
			},
			want: 1.0 / 3.0,
			pass: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ScoreRecall(tc.expected, tc.retrieved)
			if got != tc.want {
				t.Errorf("ScoreRecall() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ScoreCitations
// ---------------------------------------------------------------------------

func TestScoreCitations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		expected []string
		actual   []string
		want     float64
		pass     bool // true = expected "passing" scenario, false = "failing"
	}{
		// --- passing cases ---
		{
			name:     "empty expected returns -1 (skip)",
			expected: []string{},
			actual:   []string{"resume.pdf"},
			want:     -1,
			pass:     true,
		},
		{
			name:     "all expected citations present returns 1.0",
			expected: []string{"resume.pdf", "about.txt"},
			actual:   []string{"resume.pdf", "about.txt", "other.pdf"},
			want:     1.0,
			pass:     true,
		},
		{
			name:     "half expected citations present returns 0.5",
			expected: []string{"resume.pdf", "missing.pdf"},
			actual:   []string{"resume.pdf"},
			want:     0.5,
			pass:     true,
		},
		// --- failing cases ---
		{
			name:     "no expected citation present returns 0.0 not -1",
			expected: []string{"resume.pdf"},
			actual:   []string{"other.pdf"},
			want:     0.0,
			pass:     false,
		},
		{
			name:     "empty actual returns 0.0",
			expected: []string{"resume.pdf"},
			actual:   []string{},
			want:     0.0,
			pass:     false,
		},
		{
			name:     "partial match returns fraction not 1.0",
			expected: []string{"a.pdf", "b.pdf", "c.pdf"},
			actual:   []string{"a.pdf"},
			want:     1.0 / 3.0,
			pass:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ScoreCitations(tc.expected, tc.actual)
			if got != tc.want {
				t.Errorf("ScoreCitations(%v, %v) = %v, want %v", tc.expected, tc.actual, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MustContainPass
// ---------------------------------------------------------------------------

func TestMustContainPass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		answer   string
		required []string
		want     bool
		pass     bool
	}{
		// --- passing cases ---
		{
			name:     "empty required slice always passes",
			answer:   "Anthony prefers LinkedIn for contact.",
			required: []string{},
			want:     true,
			pass:     true,
		},
		{
			name:     "answer contains the single required string",
			answer:   "You can reach him via linkedin.com/in/anthonybible/.",
			required: []string{"linkedin"},
			want:     true,
			pass:     true,
		},
		{
			name:     "case-insensitive match of all required strings",
			answer:   "Reach out on LinkedIn or GitHub.",
			required: []string{"linkedin", "github"},
			want:     true,
			pass:     true,
		},
		// --- failing cases ---
		{
			name:     "answer missing the only required string → false",
			answer:   "I can't share that information.",
			required: []string{"linkedin"},
			want:     false,
			pass:     false,
		},
		{
			name:     "answer missing one of several required strings → false",
			answer:   "You can find him on LinkedIn.",
			required: []string{"linkedin", "github"},
			want:     false,
			pass:     false,
		},
		{
			name:     "empty answer with a required string → false",
			answer:   "",
			required: []string{"contact"},
			want:     false,
			pass:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := MustContainPass(tc.answer, tc.required)
			if got != tc.want {
				t.Errorf("MustContainPass(%q, %v) = %v, want %v",
					tc.answer, tc.required, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// RefusalCorrect
// ---------------------------------------------------------------------------

func TestScoreRefusalCorrect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		answer          string
		expectedRefusal bool
		want            bool
		pass            bool
	}{
		// --- passing cases ---
		{
			name:            "refusal answer with expectedRefusal=true → true",
			answer:          "I'm focused on Anthony's professional background and cannot discuss that.",
			expectedRefusal: true,
			want:            true,
			pass:            true,
		},
		{
			name:            "no-chunks phrase with expectedRefusal=true → true",
			answer:          "I couldn't find relevant information about that topic.",
			expectedRefusal: true,
			want:            true,
			pass:            true,
		},
		{
			name:            "normal answer with expectedRefusal=false → true",
			answer:          "Anthony led the migration to Kubernetes in 2022.",
			expectedRefusal: false,
			want:            true,
			pass:            true,
		},
		// --- failing cases ---
		{
			name:            "refusal answer but expectedRefusal=false → false",
			answer:          "I'm focused on Anthony's professional background only.",
			expectedRefusal: false,
			want:            false,
			pass:            false,
		},
		{
			name:            "normal answer but expectedRefusal=true → false",
			answer:          "Anthony has 8 years of SRE experience.",
			expectedRefusal: true,
			want:            false,
			pass:            false,
		},
		{
			name:            "no-chunks phrase but expectedRefusal=false → false",
			answer:          "Sorry, I couldn't find relevant information for that.",
			expectedRefusal: false,
			want:            false,
			pass:            false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := RefusalCorrect(tc.answer, tc.expectedRefusal)
			if got != tc.want {
				t.Errorf("RefusalCorrect(%q, %v) = %v, want %v",
					tc.answer, tc.expectedRefusal, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MustNotContainPass
// ---------------------------------------------------------------------------

func TestScoreMustNotContainPass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		answer    string
		forbidden []string
		want      bool
		pass      bool
	}{
		// --- passing cases ---
		{
			name:      "empty forbidden slice always passes",
			answer:    "Anthony achieved 99.99% uptime.",
			forbidden: []string{},
			want:      true,
			pass:      true,
		},
		{
			name:      "answer contains none of the forbidden words",
			answer:    "Anthony led the SRE team at Example Corp.",
			forbidden: []string{"confidential", "secret", "hack"},
			want:      true,
			pass:      true,
		},
		{
			name:      "case-insensitive check does not match partial substring correctly",
			answer:    "He was promoted to staff engineer.",
			forbidden: []string{"fired", "terminated", "resigned"},
			want:      true,
			pass:      true,
		},
		// --- failing cases ---
		{
			name:      "answer contains one forbidden word → false",
			answer:    "Confidential: Anthony earned $250k salary.",
			forbidden: []string{"confidential"},
			want:      false,
			pass:      false,
		},
		{
			name:      "case-insensitive match triggers failure",
			answer:    "His SSN is 123-45-6789.",
			forbidden: []string{"ssn"},
			want:      false,
			pass:      false,
		},
		{
			name:      "second forbidden word in list causes failure",
			answer:    "The answer is unrelated to Anthony's background.",
			forbidden: []string{"promoted", "unrelated"},
			want:      false,
			pass:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := MustNotContainPass(tc.answer, tc.forbidden)
			if got != tc.want {
				t.Errorf("MustNotContainPass(%q, %v) = %v, want %v",
					tc.answer, tc.forbidden, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ToolCallsPresent
// ---------------------------------------------------------------------------

func TestScoreToolCallsPresent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		expected []string
		seen     []string
		want     bool
		pass     bool
	}{
		// --- passing cases ---
		{
			name:     "empty expected always passes",
			expected: []string{},
			seen:     []string{"list_documents"},
			want:     true,
			pass:     true,
		},
		{
			name:     "all expected tools present in seen",
			expected: []string{"list_documents", "fetch_full_document"},
			seen:     []string{"list_documents", "fetch_full_document", "send_contact_email"},
			want:     true,
			pass:     true,
		},
		{
			name:     "single expected tool found in seen",
			expected: []string{"send_contact_email"},
			seen:     []string{"list_documents", "send_contact_email"},
			want:     true,
			pass:     true,
		},
		// --- failing cases ---
		{
			name:     "expected tool absent from seen → false",
			expected: []string{"fetch_full_document"},
			seen:     []string{"list_documents"},
			want:     false,
			pass:     false,
		},
		{
			name:     "empty seen when tools expected → false",
			expected: []string{"list_documents"},
			seen:     []string{},
			want:     false,
			pass:     false,
		},
		{
			name:     "one of two expected tools missing → false",
			expected: []string{"list_documents", "fetch_full_document"},
			seen:     []string{"list_documents"},
			want:     false,
			pass:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ToolCallsPresent(tc.expected, tc.seen)
			if got != tc.want {
				t.Errorf("ToolCallsPresent(%v, %v) = %v, want %v",
					tc.expected, tc.seen, got, tc.want)
			}
		})
	}
}
