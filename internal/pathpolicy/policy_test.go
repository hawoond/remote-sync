package pathpolicy

import (
	"errors"
	"strings"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		display string
		key     string
		code    Code
	}{
		{name: "simple", input: "Documents/report.pdf", display: "Documents/report.pdf", key: "documents/report.pdf"},
		{name: "normalizes unicode", input: "Cafe\u0301/menu.txt", display: "Café/menu.txt", key: "café/menu.txt"},
		{name: "folds case", input: "Straße.txt", display: "Straße.txt", key: "strasse.txt"},
		{name: "empty", input: "", code: CodeEmpty},
		{name: "absolute", input: "/etc/passwd", code: CodeAbsolute},
		{name: "drive", input: "C:/Windows/file.txt", code: CodeAbsolute},
		{name: "backslash", input: `folder\file.txt`, code: CodeSeparator},
		{name: "parent traversal", input: "folder/../file.txt", code: CodeSegment},
		{name: "empty segment", input: "folder//file.txt", code: CodeSegment},
		{name: "reserved character", input: "folder/what?.txt", code: CodeReservedCharacter},
		{name: "reserved name", input: "folder/CON.txt", code: CodeReservedName},
		{name: "trailing dot", input: "folder/file.", code: CodeTrailingDotSpace},
		{name: "long segment", input: strings.Repeat("a", MaxSegmentBytes+1), code: CodeSegmentTooLong},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := Canonicalize(test.input)
			if test.code != "" {
				var violation *Violation
				if !errors.As(err, &violation) {
					t.Fatalf("expected Violation, got %v", err)
				}
				if violation.Code != test.code {
					t.Fatalf("expected code %s, got %s", test.code, violation.Code)
				}
				return
			}
			if err != nil {
				t.Fatalf("Canonicalize() error = %v", err)
			}
			if got.Display != test.display {
				t.Errorf("display = %q, want %q", got.Display, test.display)
			}
			if got.Key != test.key {
				t.Errorf("key = %q, want %q", got.Key, test.key)
			}
		})
	}
}

func TestCanonicalizeRejectsDeepPath(t *testing.T) {
	t.Parallel()

	_, err := Canonicalize(strings.Repeat("a/", MaxDepth) + "a")
	var violation *Violation
	if !errors.As(err, &violation) || violation.Code != CodeTooDeep {
		t.Fatalf("expected %s, got %v", CodeTooDeep, err)
	}
}
