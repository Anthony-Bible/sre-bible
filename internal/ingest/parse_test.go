package ingest_test

import (
	"path/filepath"
	"testing"

	"github.com/Anthony-Bible/sre-bible/internal/ingest"
)

// deriveSourceNameCase is a single row in the TestDeriveSourceName table.
type deriveSourceNameCase struct {
	name        string
	location    string
	wantName    string
	wantType    string
	wantErrBool bool // true → expect a non-nil error
}

// assertDeriveSourceName runs DeriveSourceName and checks all observable contracts.
func assertDeriveSourceName(t *testing.T, tc deriveSourceNameCase) {
	t.Helper()

	gotName, gotType, gotErr := ingest.DeriveSourceName(tc.location)

	if tc.wantErrBool {
		// Contract 5: a bad URL must return a non-nil error.
		if gotErr == nil {
			t.Fatalf("DeriveSourceName(%q): expected non-nil error, got nil (name=%q type=%q)",
				tc.location, gotName, gotType)
		}
		// On error both name and type must be zero-value so callers cannot misuse them.
		if gotName != "" || gotType != "" {
			t.Errorf("DeriveSourceName(%q): on error want empty name/type, got name=%q type=%q",
				tc.location, gotName, gotType)
		}
		return
	}

	// Happy-path contracts.
	if gotErr != nil {
		t.Fatalf("DeriveSourceName(%q): unexpected error: %v", tc.location, gotErr)
	}
	if gotName != tc.wantName {
		t.Errorf("DeriveSourceName(%q): name = %q, want %q", tc.location, gotName, tc.wantName)
	}
	if gotType != tc.wantType {
		t.Errorf("DeriveSourceName(%q): type = %q, want %q", tc.location, gotType, tc.wantType)
	}
}

// TestDeriveSourceName exercises the observable contracts of DeriveSourceName:
//
//  1. HTTP URL  → (full URL string, "url", nil)
//  2. HTTPS URL → (full URL string, "url", nil)
//  3. plain-text file path (.txt, .md, .markdown) → (basename, "text", nil)
//  4. PDF / other file path (no directories) → (basename, "pdf", nil)
//  5. File path with directory segments → (basename only, inferred type, nil)
//  6. URL whose scheme prefix is http/https but whose body is unparseable
//     → ("", "", non-nil error)
func TestDeriveSourceName(t *testing.T) {
	t.Parallel()

	tests := []deriveSourceNameCase{
		// Contract 1: HTTP URL — name must equal the full location string, type must be "url".
		{
			name:     "http URL returns full URL as name and type url",
			location: "http://example.com/docs/runbook",
			wantName: "http://example.com/docs/runbook",
			wantType: "url",
		},
		// Contract 1 variant: query string must be preserved verbatim.
		{
			name:     "http URL with query string is preserved verbatim",
			location: "http://wiki.example.com/page?id=42&rev=3",
			wantName: "http://wiki.example.com/page?id=42&rev=3",
			wantType: "url",
		},
		// Contract 2: HTTPS URL — same semantics as HTTP.
		{
			name:     "https URL returns full URL as name and type url",
			location: "https://example.com/sre/oncall",
			wantName: "https://example.com/sre/oncall",
			wantType: "url",
		},
		// Contract 2 variant: HTTPS with root path only.
		{
			name:     "https URL with root path only",
			location: "https://example.com/",
			wantName: "https://example.com/",
			wantType: "url",
		},
		// Contract 3: .txt file — name must be basename, type must be "text".
		{
			name:     "plain txt filename returns basename and type text",
			location: "brag-doc.txt",
			wantName: "brag-doc.txt",
			wantType: "text",
		},
		{
			name:     "absolute path to txt file returns basename and type text",
			location: "/some/dir/notes.txt",
			wantName: "notes.txt",
			wantType: "text",
		},
		// Contract 3 variant: Markdown is plain text, not a PDF.
		{
			name:     "md filename returns basename and type text",
			location: "brag-doc.additions.md",
			wantName: "brag-doc.additions.md",
			wantType: "text",
		},
		{
			name:     "markdown extension returns basename and type text",
			location: "docs/notes.markdown",
			wantName: "notes.markdown",
			wantType: "text",
		},
		// Contract 3 variant: extension matching is case-insensitive.
		{
			name:     "uppercase MD extension returns type text",
			location: "/some/dir/README.MD",
			wantName: "README.MD",
			wantType: "text",
		},
		// Contract 4: PDF filename — name must be the basename, type must be "pdf".
		{
			name:     "plain pdf filename returns basename and type pdf",
			location: "resume.pdf",
			wantName: "resume.pdf",
			wantType: "pdf",
		},
		// Contract 4 variant: extensionless filename defaults to pdf.
		{
			name:     "extensionless filename returns basename and type pdf",
			location: "runbook",
			wantName: "runbook",
			wantType: "pdf",
		},
		// Contract 4: File path with directories — only the basename is returned.
		{
			name:     "absolute path returns only the filename component",
			location: "/some/dir/resume.pdf",
			wantName: "resume.pdf",
			wantType: "pdf",
		},
		{
			name:     "relative nested path returns only the filename component",
			location: "docs/runbooks/oncall.pdf",
			wantName: filepath.Base("docs/runbooks/oncall.pdf"),
			wantType: "pdf",
		},
		{
			name:     "deeply nested absolute path returns only the leaf filename",
			location: "/var/data/ingestion/2024/q1/sre-handbook.pdf",
			wantName: "sre-handbook.pdf",
			wantType: "pdf",
		},
		// Contract 5: http/https prefix with an unparseable body must return an error.
		// url.Parse rejects invalid percent-escape sequences (%ZZ is not valid hex).
		{
			name:        "http URL with invalid percent-escape returns error",
			location:    "http://example.com/bad%ZZpath",
			wantErrBool: true,
		},
		{
			name:        "https URL with invalid percent-escape returns error",
			location:    "https://example.com/bad%ZZpath",
			wantErrBool: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertDeriveSourceName(t, tc)
		})
	}
}
