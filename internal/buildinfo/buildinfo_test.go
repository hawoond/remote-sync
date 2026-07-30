package buildinfo

import (
	"strings"
	"testing"
)

func TestRequested(t *testing.T) {
	t.Parallel()

	for _, arguments := range [][]string{{"version"}, {"--version"}} {
		if !Requested(arguments) {
			t.Fatalf("Requested(%q) = false", arguments)
		}
	}
	for _, arguments := range [][]string{nil, {"help"}, {"--version", "extra"}} {
		if Requested(arguments) {
			t.Fatalf("Requested(%q) = true", arguments)
		}
	}
}

func TestString(t *testing.T) {
	originalVersion, originalCommit, originalDate := Version, Commit, Date
	t.Cleanup(func() {
		Version, Commit, Date = originalVersion, originalCommit, originalDate
	})
	Version = "v1.2.3"
	Commit = "0123456789ab"
	Date = "2026-07-30T00:00:00Z"

	value := String("sync-agent")
	for _, expected := range []string{
		"sync-agent",
		"v1.2.3",
		"0123456789ab",
		"2026-07-30T00:00:00Z",
	} {
		if !strings.Contains(value, expected) {
			t.Fatalf("String() = %q, missing %q", value, expected)
		}
	}
}
