package worktree

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverFindsCodexAndClaudeWorktrees(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	repository := initializeRepository(t, t.TempDir())
	codexPath := filepath.Join(home, ".codex", "worktrees", "session-one")
	claudePath := filepath.Join(repository, ".claude", "worktrees", "feature-auth")
	addWorktree(t, repository, codexPath, "")
	addWorktree(t, repository, claudePath, "worktree-feature-auth")
	codexPath, _ = absolutePath(codexPath)
	claudePath, _ = absolutePath(claudePath)
	repository, _ = absolutePath(repository)

	candidates, err := Discover(context.Background(), Options{
		HomeDir:            home,
		WorkingDir:         repository,
		CodexHome:          filepath.Join(home, ".codex"),
		ClaudeConfigDir:    filepath.Join(home, ".claude"),
		ClaudeProjectsFile: filepath.Join(home, ".claude.json"),
		Providers:          []Provider{ProviderCodex, ProviderClaude},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %+v, want two", candidates)
	}

	byProvider := make(map[Provider]Candidate)
	for _, candidate := range candidates {
		byProvider[candidate.Provider] = candidate
		if candidate.Repository != filepath.Base(repository) {
			t.Fatalf("repository = %q, want %q", candidate.Repository, filepath.Base(repository))
		}
		if candidate.Head == "" || len(candidate.Head) != 12 {
			t.Fatalf("head = %q, want 12-character commit", candidate.Head)
		}
		if !strings.HasPrefix(candidate.ID, string(candidate.Provider)+":") {
			t.Fatalf("candidate ID = %q", candidate.ID)
		}
	}
	if byProvider[ProviderCodex].Path != codexPath {
		t.Fatalf("Codex path = %q, want %q", byProvider[ProviderCodex].Path, codexPath)
	}
	if byProvider[ProviderCodex].Branch != "(detached)" {
		t.Fatalf("Codex branch = %q, want detached", byProvider[ProviderCodex].Branch)
	}
	if byProvider[ProviderClaude].Path != claudePath {
		t.Fatalf("Claude path = %q, want %q", byProvider[ProviderClaude].Path, claudePath)
	}
	if byProvider[ProviderClaude].Branch != "worktree-feature-auth" {
		t.Fatalf("Claude branch = %q", byProvider[ProviderClaude].Branch)
	}
}

func TestDiscoverUsesClaudeProjectHistoryAndDeduplicatesRoots(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	repository := initializeRepository(t, t.TempDir())
	claudePath := filepath.Join(repository, ".claude", "worktrees", "remembered")
	addWorktree(t, repository, claudePath, "worktree-remembered")
	claudePath, _ = absolutePath(claudePath)
	repository, _ = absolutePath(repository)

	config := struct {
		Projects map[string]any `json:"projects"`
	}{
		Projects: map[string]any{repository: map[string]any{}},
	}
	content, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	projectsFile := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(projectsFile, content, 0o600); err != nil {
		t.Fatal(err)
	}

	candidates, err := Discover(context.Background(), Options{
		HomeDir:            home,
		WorkingDir:         t.TempDir(),
		CodexHome:          filepath.Join(home, ".codex"),
		ClaudeConfigDir:    filepath.Join(home, ".claude"),
		ClaudeProjectsFile: projectsFile,
		ClaudeRoots:        []string{filepath.Join(repository, ".claude", "worktrees")},
		ProjectRoots:       []string{repository},
		Providers:          []Provider{ProviderClaude},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %+v, want one deduplicated worktree", candidates)
	}
	if candidates[0].Provider != ProviderClaude || candidates[0].Path != claudePath {
		t.Fatalf("candidate = %+v", candidates[0])
	}
}

func TestDiscoverUsesLinkedWorktreeAsClaudeProjectRoot(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	repository := initializeRepository(t, t.TempDir())
	projectPath := filepath.Join(t.TempDir(), "linked-project")
	claudePath := filepath.Join(projectPath, ".claude", "worktrees", "child")
	addWorktree(t, repository, projectPath, "linked-project")
	addWorktree(t, repository, claudePath, "linked-project-child")
	claudePath, _ = absolutePath(claudePath)

	candidates, err := Discover(context.Background(), Options{
		HomeDir:            home,
		WorkingDir:         projectPath,
		CodexHome:          filepath.Join(home, ".codex"),
		ClaudeConfigDir:    filepath.Join(home, ".claude"),
		ClaudeProjectsFile: filepath.Join(home, ".claude.json"),
		Providers:          []Provider{ProviderClaude},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Path != claudePath {
		t.Fatalf("candidates = %+v, want linked worktree child %q", candidates, claudePath)
	}
}

func TestDiscoverSkipsNonGitDirectories(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "worktrees")
	if err := os.MkdirAll(filepath.Join(root, "stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stale", ".git"), []byte("not a worktree"), 0o600); err != nil {
		t.Fatal(err)
	}

	candidates, err := Discover(context.Background(), Options{
		WorkingDir:      t.TempDir(),
		CodexHome:       t.TempDir(),
		ClaudeConfigDir: t.TempDir(),
		CodexRoots:      []string{root},
		Providers:       []Provider{ProviderCodex},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %+v, want none", candidates)
	}
}

func TestParseProviders(t *testing.T) {
	t.Parallel()

	providers, err := ParseProviders("claude,codex,claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 2 || providers[0] != ProviderClaude || providers[1] != ProviderCodex {
		t.Fatalf("providers = %v", providers)
	}
	if _, err := ParseProviders("other"); err == nil {
		t.Fatal("expected unsupported provider error")
	}
}

func initializeRepository(t *testing.T, repository string) string {
	t.Helper()
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init")
	runGit(t, repository, "config", "user.name", "Remote Sync Test")
	runGit(t, repository, "config", "user.email", "remote-sync-test@example.invalid")
	runGit(t, repository, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(
		filepath.Join(repository, ".gitignore"),
		[]byte(".claude/worktrees/\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", ".")
	runGit(t, repository, "commit", "-m", "Initial commit")
	runGit(t, repository, "branch", "-M", "main")
	return repository
}

func addWorktree(t *testing.T, repository, path, branch string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if branch == "" {
		runGit(t, repository, "worktree", "add", "--detach", path)
		return
	}
	runGit(t, repository, "worktree", "add", "-b", branch, path)
}

func runGit(t *testing.T, repository string, arguments ...string) {
	t.Helper()
	args := append([]string{"-C", repository}, arguments...)
	command := exec.Command("git", args...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}
