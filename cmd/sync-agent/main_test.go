package main

import (
	"bytes"
	"errors"
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
	for _, command := range []string{
		"sync-agent enrollment create",
		"sync-agent enroll",
		"sync-agent policy get",
		"sync-agent restore",
	} {
		if !strings.Contains(output.String(), command) {
			t.Fatalf("help output does not contain %q: %q", command, output.String())
		}
	}
}

func TestRealMainVersion(t *testing.T) {
	t.Parallel()

	var output, errorOutput bytes.Buffer
	code := realMain([]string{"--version"}, nil, &output, &errorOutput)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, errorOutput.String())
	}
	if !strings.Contains(output.String(), "sync-agent") {
		t.Fatalf("version output = %q", output.String())
	}
}

func TestPublishRestoredFileWithoutReplacement(t *testing.T) {
	t.Parallel()

	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("target.txt", []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("restore.part", []byte("restored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishRestoredFile(root, "restore.part", "target.txt", false); err == nil {
		t.Fatal("expected no-replacement publication to fail")
	}
	content, err := root.ReadFile("target.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "existing" {
		t.Fatalf("target content = %q", content)
	}
}

func TestPublishRestoredFileCreatesAndReplacesAtomically(t *testing.T) {
	t.Parallel()

	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("new.part", []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishRestoredFile(root, "new.part", "new.txt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat("new.part"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new temporary file still exists: %v", err)
	}
	if err := root.WriteFile("replace.txt", []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("replace.part", []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishRestoredFile(root, "replace.part", "replace.txt", true); err != nil {
		t.Fatal(err)
	}
	content, err := root.ReadFile("replace.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "replacement" {
		t.Fatalf("replacement content = %q", content)
	}
}
