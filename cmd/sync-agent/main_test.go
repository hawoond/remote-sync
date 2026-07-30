package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultStateFilenameIsScopedToRoot(t *testing.T) {
	t.Parallel()

	const folderID = "681f7dd7-559b-4fab-8734-41b00f663425"
	first := defaultStateFilename(folderID, filepath.Join(t.TempDir(), "one"))
	second := defaultStateFilename(folderID, filepath.Join(t.TempDir(), "two"))
	if first == second {
		t.Fatalf("state filenames are equal: %q", first)
	}
	if !strings.HasPrefix(first, folderID+"-") || !strings.HasSuffix(first, ".db") {
		t.Fatalf("state filename = %q", first)
	}
}

func TestValidateRootPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	resolved, err := validateRootPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(resolved) {
		t.Fatalf("resolved path = %q, want absolute", resolved)
	}
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateRootPath(file); err == nil {
		t.Fatal("expected non-directory error")
	}
}

func TestRunDiscoverCommandRejectsUnknownProvider(t *testing.T) {
	t.Parallel()

	err := runDiscoverCommand(
		t.Context(),
		[]string{"--provider", "other"},
		&bytes.Buffer{},
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "unsupported worktree provider") {
		t.Fatalf("error = %v", err)
	}
}

func TestRealMainHelp(t *testing.T) {
	t.Parallel()

	var output, errorOutput bytes.Buffer
	code := realMain([]string{"help"}, nil, &output, &errorOutput)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, errorOutput.String())
	}
	if !strings.Contains(output.String(), "sync-agent discover") {
		t.Fatalf("help output = %q", output.String())
	}
}
