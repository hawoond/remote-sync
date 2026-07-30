package main

import (
	"bytes"
	"errors"
	"log/slog"
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

func TestValidateSyncRootsRejectsOverlappingDirectories(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := validateSyncRoots([]syncRoot{
		{Path: parent, Label: "parent"},
		{Path: child, Label: "child"},
	})
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("error = %v, want overlap error", err)
	}
}

func TestValidateSyncRootsEnforcesSelectionLimit(t *testing.T) {
	t.Parallel()

	roots := make([]syncRoot, maxSyncRoots+1)
	if _, err := validateSyncRoots(roots); err == nil ||
		!strings.Contains(err.Error(), "at most") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseWorktreeReferencesSupportsListAndJSON(t *testing.T) {
	t.Parallel()

	commaSeparated, err := parseWorktreeReferences("codex:one, claude:two")
	if err != nil {
		t.Fatal(err)
	}
	jsonList, err := parseWorktreeReferences(`["codex:one", "/tmp/two"]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(commaSeparated) != 2 || commaSeparated[1] != "claude:two" {
		t.Fatalf("comma-separated references = %v", commaSeparated)
	}
	if len(jsonList) != 2 || jsonList[1] != "/tmp/two" {
		t.Fatalf("JSON references = %v", jsonList)
	}
}

func TestConfigForRootUsesIndependentDefaultStatePaths(t *testing.T) {
	t.Parallel()

	shared := sharedConfig{stateDirectory: t.TempDir()}
	first, err := configForRoot(
		shared,
		filepath.Join(t.TempDir(), "one"),
		"681f7dd7-559b-4fab-8734-41b00f663425",
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := configForRoot(
		shared,
		filepath.Join(t.TempDir(), "two"),
		"b250c9bf-f4a9-43e4-81c6-33a5962aa861",
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.statePath == second.statePath {
		t.Fatalf("state paths are equal: %q", first.statePath)
	}
}

func TestRootClientKeyIsStableAndDoesNotExposePath(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "private-project")
	first := rootClientKey(root)
	second := rootClientKey(root)
	if first != second {
		t.Fatalf("client keys differ: %q and %q", first, second)
	}
	if strings.Contains(first, root) || !strings.HasPrefix(first, "root:v1:") {
		t.Fatalf("client key = %q", first)
	}
}

func TestFindExistingAnchorRootPreservesLegacyAssignment(t *testing.T) {
	t.Parallel()

	const folderID = "681f7dd7-559b-4fab-8734-41b00f663425"
	shared := sharedConfig{
		baseFolderID:   folderID,
		stateDirectory: t.TempDir(),
	}
	roots := []syncRoot{
		{Path: filepath.Join(t.TempDir(), "one")},
		{Path: filepath.Join(t.TempDir(), "two")},
	}
	statePath := filepath.Join(
		shared.stateDirectory,
		defaultStateFilename(folderID, roots[1].Path),
	)
	if err := os.WriteFile(statePath, []byte("legacy state"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := findExistingAnchorRoot(roots, shared)
	if err != nil {
		t.Fatal(err)
	}
	if selected != roots[1].Path {
		t.Fatalf("existing anchor root = %q, want %q", selected, roots[1].Path)
	}
}

func TestFindExistingAnchorRootRejectsAmbiguousLegacyState(t *testing.T) {
	t.Parallel()

	const folderID = "681f7dd7-559b-4fab-8734-41b00f663425"
	shared := sharedConfig{
		baseFolderID:   folderID,
		stateDirectory: t.TempDir(),
	}
	roots := []syncRoot{
		{Path: filepath.Join(t.TempDir(), "one")},
		{Path: filepath.Join(t.TempDir(), "two")},
	}
	for _, root := range roots {
		statePath := filepath.Join(
			shared.stateDirectory,
			defaultStateFilename(folderID, root.Path),
		)
		if err := os.WriteFile(statePath, []byte("legacy state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := findExistingAnchorRoot(roots, shared); err == nil ||
		!strings.Contains(err.Error(), "multiple selected roots") {
		t.Fatalf("error = %v", err)
	}
}

func TestPrepareConfigsRejectsOneExplicitStateFileForMultipleRoots(t *testing.T) {
	t.Parallel()

	_, err := prepareConfigs(
		t.Context(),
		[]syncRoot{{Path: "/one"}, {Path: "/two"}},
		sharedConfig{explicitStatePath: "/state.db"},
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	)
	if err == nil || !strings.Contains(err.Error(), "SYNC_STATE_PATH") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateStateConfigurationRejectsStateInsideSyncRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	err := validateStateConfiguration(
		[]syncRoot{{Path: root}},
		sharedConfig{
			baseFolderID:   "681f7dd7-559b-4fab-8734-41b00f663425",
			stateDirectory: filepath.Join(root, ".remote-sync"),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "outside sync root") {
		t.Fatalf("error = %v", err)
	}
}

func TestFolderMapIsPrivateAndReadableFromCommand(t *testing.T) {
	const folderID = "681f7dd7-559b-4fab-8734-41b00f663425"
	stateDirectory := t.TempDir()
	t.Setenv("SYNC_FOLDER_ID", folderID)
	t.Setenv("SYNC_DEVICE_TOKEN", strings.Repeat("x", 32))
	t.Setenv("SYNC_STATE_DIR", stateDirectory)
	t.Setenv("SYNC_FOLDER_MAP_PATH", "")

	shared, err := loadSharedConfig()
	if err != nil {
		t.Fatal(err)
	}
	root := syncRoot{
		Path:      t.TempDir(),
		Reference: "codex:111111111111",
		Label:     "codex repository worktree",
	}
	cfg, err := configForRoot(shared, root.Path, folderID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFolderMap(shared, []syncRoot{root}, []config{cfg}); err != nil {
		t.Fatal(err)
	}
	path, err := folderMapPath(shared)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("folder map mode = %o", info.Mode().Perm())
	}

	var output, errorOutput bytes.Buffer
	if err := runFoldersCommand(nil, &output, &errorOutput); err != nil {
		t.Fatalf("folders command: %v, stderr = %s", err, errorOutput.String())
	}
	if !strings.Contains(output.String(), folderID) ||
		!strings.Contains(output.String(), root.Path) {
		t.Fatalf("folders output = %q", output.String())
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
		"sync-agent folders",
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

func TestRestoreRejectsInvalidFolderOverride(t *testing.T) {
	t.Parallel()

	err := runRestoreCommand(
		t.Context(),
		[]string{"--target", t.TempDir(), "--folder-id", "not-a-uuid"},
		&bytes.Buffer{},
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "folder ID must be a UUID") {
		t.Fatalf("error = %v", err)
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
