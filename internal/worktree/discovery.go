package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
)

const (
	defaultMaxDepth       = 6
	defaultMaxDirectories = 20_000
	maxClaudeProjects     = 1_000
	maxClaudeConfigSize   = 32 << 20
)

type Candidate struct {
	ID         string   `json:"id"`
	Provider   Provider `json:"provider"`
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Repository string   `json:"repository"`
	Branch     string   `json:"branch"`
	Head       string   `json:"head"`
}

type Options struct {
	HomeDir            string
	WorkingDir         string
	CodexHome          string
	ClaudeConfigDir    string
	ClaudeProjectsFile string
	CodexRoots         []string
	ClaudeRoots        []string
	ProjectRoots       []string
	Providers          []Provider
	GitExecutable      string
	MaxDepth           int
	MaxDirectories     int
}

func DefaultOptions() (Options, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Options{}, fmt.Errorf("resolve home directory: %w", err)
	}
	working, err := os.Getwd()
	if err != nil {
		return Options{}, fmt.Errorf("resolve working directory: %w", err)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	claudeConfig := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeConfig == "" {
		claudeConfig = filepath.Join(home, ".claude")
	}
	return Options{
		HomeDir:            home,
		WorkingDir:         working,
		CodexHome:          codexHome,
		ClaudeConfigDir:    claudeConfig,
		ClaudeProjectsFile: filepath.Join(home, ".claude.json"),
		CodexRoots:         splitPathList(os.Getenv("SYNC_CODEX_WORKTREE_ROOTS")),
		ClaudeRoots:        splitPathList(os.Getenv("SYNC_CLAUDE_WORKTREE_ROOTS")),
		ProjectRoots:       splitPathList(os.Getenv("SYNC_DISCOVERY_PROJECT_ROOTS")),
		Providers:          []Provider{ProviderCodex, ProviderClaude},
		GitExecutable:      "git",
		MaxDepth:           defaultMaxDepth,
		MaxDirectories:     defaultMaxDirectories,
	}, nil
}

func Discover(ctx context.Context, options Options) ([]Candidate, error) {
	options = withDefaults(options)
	gitPath, err := exec.LookPath(options.GitExecutable)
	if err != nil {
		return nil, fmt.Errorf("find git executable: %w", err)
	}

	enabled := make(map[Provider]bool, len(options.Providers))
	for _, provider := range options.Providers {
		if provider != ProviderCodex && provider != ProviderClaude {
			return nil, fmt.Errorf("unsupported worktree provider %q", provider)
		}
		enabled[provider] = true
	}

	roots := make([]providerRoot, 0)
	if enabled[ProviderCodex] {
		roots = append(roots, providerRoot{
			Provider: ProviderCodex,
			Path:     filepath.Join(options.CodexHome, "worktrees"),
		})
		for _, root := range options.CodexRoots {
			roots = append(roots, providerRoot{Provider: ProviderCodex, Path: root})
		}
	}
	if enabled[ProviderClaude] {
		roots = append(roots, providerRoot{
			Provider: ProviderClaude,
			Path:     filepath.Join(options.ClaudeConfigDir, "worktrees"),
		})
		for _, root := range options.ClaudeRoots {
			roots = append(roots, providerRoot{Provider: ProviderClaude, Path: root})
		}

		projects := append([]string(nil), options.ProjectRoots...)
		projects = append(projects, options.WorkingDir)
		projects = append(projects, claudeProjectRoots(options.ClaudeProjectsFile)...)
		for _, project := range uniquePaths(projects) {
			repositoryRoot, ok := gitRepositoryRoot(ctx, gitPath, project)
			if !ok {
				continue
			}
			roots = append(roots, providerRoot{
				Provider: ProviderClaude,
				Path:     filepath.Join(repositoryRoot, ".claude", "worktrees"),
			})
		}
	}

	seen := make(map[string]struct{})
	candidates := make([]Candidate, 0)
	for _, root := range uniqueProviderRoots(roots) {
		discovered, err := discoverRoot(ctx, gitPath, root, options.MaxDepth, options.MaxDirectories)
		if err != nil {
			return nil, err
		}
		for _, candidate := range discovered {
			key := comparisonPath(candidate.Path)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			candidates = append(candidates, candidate)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.Provider != right.Provider {
			return left.Provider < right.Provider
		}
		if left.Repository != right.Repository {
			return strings.ToLower(left.Repository) < strings.ToLower(right.Repository)
		}
		if left.Name != right.Name {
			return strings.ToLower(left.Name) < strings.ToLower(right.Name)
		}
		return comparisonPath(left.Path) < comparisonPath(right.Path)
	})
	return candidates, nil
}

func ParseProviders(value string) ([]Provider, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "all") {
		return []Provider{ProviderCodex, ProviderClaude}, nil
	}
	seen := make(map[Provider]struct{})
	providers := make([]Provider, 0, 2)
	for _, item := range strings.Split(value, ",") {
		provider := Provider(strings.ToLower(strings.TrimSpace(item)))
		if provider != ProviderCodex && provider != ProviderClaude {
			return nil, fmt.Errorf("unsupported worktree provider %q", item)
		}
		if _, exists := seen[provider]; exists {
			continue
		}
		seen[provider] = struct{}{}
		providers = append(providers, provider)
	}
	if len(providers) == 0 {
		return nil, errors.New("at least one worktree provider is required")
	}
	return providers, nil
}

type providerRoot struct {
	Provider Provider
	Path     string
}

func discoverRoot(
	ctx context.Context,
	gitPath string,
	root providerRoot,
	maxDepth, maxDirectories int,
) ([]Candidate, error) {
	rootPath, err := absolutePath(root.Path)
	if err != nil {
		return nil, nil
	}
	info, err := os.Stat(rootPath)
	if err != nil || !info.IsDir() {
		return nil, nil
	}

	var candidates []Candidate
	directories := 0
	err = filepath.WalkDir(rootPath, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return filepath.SkipDir
		}
		directories++
		if directories > maxDirectories {
			return filepath.SkipAll
		}

		relative, err := filepath.Rel(rootPath, path)
		if err != nil {
			return filepath.SkipDir
		}
		if relative != "." && pathDepth(relative) > maxDepth {
			return filepath.SkipDir
		}
		if path != rootPath && shouldSkipDirectory(entry.Name()) {
			return filepath.SkipDir
		}
		if _, err := os.Lstat(filepath.Join(path, ".git")); err != nil {
			return nil
		}

		candidate, ok, err := inspectCandidate(ctx, gitPath, root.Provider, path)
		if err != nil {
			return err
		}
		if ok {
			candidates = append(candidates, candidate)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s worktree root: %w", root.Provider, err)
	}
	return candidates, nil
}

func inspectCandidate(
	ctx context.Context,
	gitPath string,
	provider Provider,
	path string,
) (Candidate, bool, error) {
	canonical, err := absolutePath(path)
	if err != nil {
		return Candidate{}, false, nil
	}
	topLevel, err := gitOutput(ctx, gitPath, canonical, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctx.Err() != nil {
			return Candidate{}, false, ctx.Err()
		}
		return Candidate{}, false, nil
	}
	topLevel, err = absolutePath(topLevel)
	if err != nil || comparisonPath(topLevel) != comparisonPath(canonical) {
		return Candidate{}, false, nil
	}

	commonDirectory, err := gitCommonDirectory(ctx, gitPath, canonical)
	if err != nil {
		if ctx.Err() != nil {
			return Candidate{}, false, ctx.Err()
		}
		return Candidate{}, false, nil
	}
	repository := filepath.Base(filepath.Dir(commonDirectory))
	if repository == "." || repository == string(filepath.Separator) || repository == "" {
		repository = filepath.Base(canonical)
	}

	branch, err := gitOutput(ctx, gitPath, canonical, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			return Candidate{}, false, ctx.Err()
		}
		branch = "(detached)"
	}
	head, err := gitOutput(ctx, gitPath, canonical, "rev-parse", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			return Candidate{}, false, ctx.Err()
		}
		return Candidate{}, false, nil
	}
	if len(head) > 12 {
		head = head[:12]
	}
	return Candidate{
		ID:         candidateID(provider, canonical),
		Provider:   provider,
		Name:       filepath.Base(canonical),
		Path:       canonical,
		Repository: repository,
		Branch:     branch,
		Head:       head,
	}, true, nil
}

func gitRepositoryRoot(ctx context.Context, gitPath, path string) (string, bool) {
	canonical, err := absolutePath(path)
	if err != nil {
		return "", false
	}
	topLevel, err := gitOutput(ctx, gitPath, canonical, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false
	}
	topLevel, err = absolutePath(topLevel)
	if err != nil {
		return "", false
	}
	return topLevel, true
}

func gitCommonDirectory(ctx context.Context, gitPath, directory string) (string, error) {
	output, err := gitOutput(
		ctx,
		gitPath,
		directory,
		"rev-parse",
		"--path-format=absolute",
		"--git-common-dir",
	)
	if err != nil {
		output, err = gitOutput(ctx, gitPath, directory, "rev-parse", "--git-common-dir")
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(output) {
			output = filepath.Join(directory, output)
		}
	}
	return absolutePath(output)
}

func gitOutput(ctx context.Context, gitPath, directory string, arguments ...string) (string, error) {
	args := append([]string{"-C", directory}, arguments...)
	command := exec.CommandContext(ctx, gitPath, args...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func claudeProjectRoots(configPath string) []string {
	file, err := os.Open(configPath)
	if err != nil {
		return nil
	}
	defer file.Close()

	var config struct {
		Projects map[string]json.RawMessage `json:"projects"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxClaudeConfigSize))
	if err := decoder.Decode(&config); err != nil {
		return nil
	}
	roots := make([]string, 0, len(config.Projects))
	for path := range config.Projects {
		if !filepath.IsAbs(path) {
			continue
		}
		roots = append(roots, path)
	}
	sort.Strings(roots)
	if len(roots) > maxClaudeProjects {
		roots = roots[:maxClaudeProjects]
	}
	return roots
}

func withDefaults(options Options) Options {
	if options.GitExecutable == "" {
		options.GitExecutable = "git"
	}
	if options.MaxDepth <= 0 {
		options.MaxDepth = defaultMaxDepth
	}
	if options.MaxDirectories <= 0 {
		options.MaxDirectories = defaultMaxDirectories
	}
	if len(options.Providers) == 0 {
		options.Providers = []Provider{ProviderCodex, ProviderClaude}
	}
	return options
}

func candidateID(provider Provider, path string) string {
	sum := sha256.Sum256([]byte(string(provider) + "\x00" + comparisonPath(path)))
	return string(provider) + ":" + hex.EncodeToString(sum[:6])
}

func uniqueProviderRoots(roots []providerRoot) []providerRoot {
	seen := make(map[string]struct{})
	result := make([]providerRoot, 0, len(roots))
	for _, root := range roots {
		path, err := absolutePath(root.Path)
		if err != nil {
			continue
		}
		key := string(root.Provider) + "\x00" + comparisonPath(path)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, providerRoot{Provider: root.Provider, Path: path})
	}
	return result
}

func uniquePaths(paths []string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		canonical, err := absolutePath(path)
		if err != nil {
			continue
		}
		key := comparisonPath(canonical)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, canonical)
	}
	return result
}

func absolutePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is empty")
	}
	if path == "~" || strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"+string(filepath.Separator)))
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute), nil
}

func comparisonPath(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func splitPathList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return filepath.SplitList(value)
}

func pathDepth(relative string) int {
	return strings.Count(filepath.Clean(relative), string(filepath.Separator)) + 1
}

func shouldSkipDirectory(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", ".cache":
		return true
	default:
		return false
	}
}
