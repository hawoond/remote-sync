package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hawoond/remote-sync/internal/worktree"
)

type syncRoot struct {
	Path      string
	Reference string
	Label     string
}

const maxSyncRoots = 128

func runDiscoverCommand(
	ctx context.Context,
	arguments []string,
	output, errorOutput io.Writer,
) error {
	flags := flag.NewFlagSet("discover", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	jsonOutput := flags.Bool("json", false, "write the discovered worktrees as JSON")
	provider := flags.String(
		"provider",
		environmentOrDefault("SYNC_WORKTREE_PROVIDERS", "all"),
		"provider filter: all, codex, or claude",
	)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("discover does not accept positional arguments")
	}

	candidates, err := discoverWorktrees(ctx, *provider)
	if err != nil {
		return err
	}
	if *jsonOutput {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(candidates); err != nil {
			return fmt.Errorf("encode discovered worktrees: %w", err)
		}
		return nil
	}
	return worktree.WriteTable(output, candidates, false)
}

func resolveSyncRoot(
	ctx context.Context,
	input *os.File,
	output io.Writer,
) (string, error) {
	roots, err := resolveSyncRoots(ctx, input, output)
	if err != nil {
		return "", err
	}
	if len(roots) != 1 {
		return "", errors.New("multiple sync roots were selected")
	}
	return roots[0].Path, nil
}

func resolveSyncRoots(
	ctx context.Context,
	input *os.File,
	output io.Writer,
) ([]syncRoot, error) {
	singleRoot := strings.TrimSpace(os.Getenv("SYNC_ROOT"))
	multipleRoots := strings.TrimSpace(os.Getenv("SYNC_ROOTS"))
	if singleRoot != "" && multipleRoots != "" {
		return nil, errors.New("set only one of SYNC_ROOT or SYNC_ROOTS")
	}
	if singleRoot != "" {
		return validateSyncRoots([]syncRoot{{
			Path:      singleRoot,
			Reference: singleRoot,
			Label:     filepath.Base(filepath.Clean(singleRoot)),
		}})
	}
	if multipleRoots != "" {
		paths := filepath.SplitList(multipleRoots)
		roots := make([]syncRoot, 0, len(paths))
		for _, path := range paths {
			if strings.TrimSpace(path) == "" {
				continue
			}
			roots = append(roots, syncRoot{
				Path:      path,
				Reference: path,
				Label:     filepath.Base(filepath.Clean(path)),
			})
		}
		return validateSyncRoots(roots)
	}

	candidates, err := discoverWorktrees(
		ctx,
		environmentOrDefault("SYNC_WORKTREE_PROVIDERS", "all"),
	)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, worktree.ErrNoCandidates
	}

	singleReference := strings.TrimSpace(os.Getenv("SYNC_WORKTREE"))
	multipleReferences := strings.TrimSpace(os.Getenv("SYNC_WORKTREES"))
	if singleReference != "" && multipleReferences != "" {
		return nil, errors.New("set only one of SYNC_WORKTREE or SYNC_WORKTREES")
	}
	if singleReference != "" || multipleReferences != "" {
		references := []string{singleReference}
		if multipleReferences != "" {
			references, err = parseWorktreeReferences(multipleReferences)
			if err != nil {
				return nil, err
			}
		}
		selected, err := worktree.SelectReferences(candidates, references)
		if err != nil {
			return nil, err
		}
		if err := writeSelectedWorktrees(output, selected); err != nil {
			return nil, err
		}
		return validateSyncRoots(syncRootsFromCandidates(selected))
	}

	if !isInteractive(input) {
		if err := worktree.WriteTable(output, candidates, true); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf(
			"%w; set SYNC_WORKTREES to listed IDs or paths, or set SYNC_ROOTS",
			worktree.ErrSelectionRequired,
		)
	}
	selected, err := worktree.SelectInteractiveMany(input, output, candidates)
	if err != nil {
		return nil, err
	}
	if err := writeSelectedWorktrees(output, selected); err != nil {
		return nil, err
	}
	return validateSyncRoots(syncRootsFromCandidates(selected))
}

func parseWorktreeReferences(value string) ([]string, error) {
	var references []string
	if strings.HasPrefix(strings.TrimSpace(value), "[") {
		if err := json.Unmarshal([]byte(value), &references); err != nil {
			return nil, fmt.Errorf("parse SYNC_WORKTREES JSON: %w", err)
		}
	} else {
		references = strings.Split(value, ",")
	}
	result := make([]string, 0, len(references))
	for _, reference := range references {
		reference = strings.TrimSpace(reference)
		if reference != "" {
			result = append(result, reference)
		}
	}
	if len(result) == 0 {
		return nil, errors.New("SYNC_WORKTREES must contain at least one ID or path")
	}
	return result, nil
}

func syncRootsFromCandidates(candidates []worktree.Candidate) []syncRoot {
	roots := make([]syncRoot, 0, len(candidates))
	for _, candidate := range candidates {
		label := strings.TrimSpace(
			fmt.Sprintf("%s %s %s", candidate.Provider, candidate.Repository, candidate.Name),
		)
		roots = append(roots, syncRoot{
			Path:      candidate.Path,
			Reference: candidate.ID,
			Label:     label,
		})
	}
	return roots
}

func writeSelectedWorktrees(output io.Writer, candidates []worktree.Candidate) error {
	for _, selected := range candidates {
		if _, err := fmt.Fprintf(
			output,
			"Selected %s worktree %s at %s\n",
			selected.Provider,
			selected.ID,
			selected.Path,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateSyncRoots(roots []syncRoot) ([]syncRoot, error) {
	if len(roots) == 0 {
		return nil, errors.New("at least one sync root is required")
	}
	if len(roots) > maxSyncRoots {
		return nil, fmt.Errorf("at most %d sync roots can be selected", maxSyncRoots)
	}
	validated := make([]syncRoot, 0, len(roots))
	byPath := make(map[string]syncRoot, len(roots))
	for _, root := range roots {
		path, err := validateRootPath(root.Path)
		if err != nil {
			return nil, err
		}
		root.Path = path
		if strings.TrimSpace(root.Label) == "" {
			root.Label = filepath.Base(path)
		}
		if existing, duplicate := byPath[path]; duplicate {
			return nil, fmt.Errorf(
				"sync roots overlap: %s and %s",
				existing.Path,
				root.Path,
			)
		}
		byPath[path] = root
		validated = append(validated, root)
	}
	for _, root := range validated {
		for parent := filepath.Dir(root.Path); parent != root.Path; parent = filepath.Dir(parent) {
			if existing, overlaps := byPath[parent]; overlaps {
				return nil, fmt.Errorf(
					"sync roots overlap: %s and %s",
					existing.Path,
					root.Path,
				)
			}
			next := filepath.Dir(parent)
			if next == parent {
				break
			}
		}
	}
	return validated, nil
}

func discoverWorktrees(ctx context.Context, providerValue string) ([]worktree.Candidate, error) {
	options, err := worktree.DefaultOptions()
	if err != nil {
		return nil, err
	}
	options.Providers, err = worktree.ParseProviders(providerValue)
	if err != nil {
		return nil, err
	}
	return worktree.Discover(ctx, options)
}

func validateRootPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve sync root: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect sync root: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("sync root must be a directory")
	}
	return filepath.Clean(absolute), nil
}

func isInteractive(file *os.File) bool {
	if file == nil {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func writeAgentUsage(output io.Writer) {
	_, _ = fmt.Fprintln(output, `Usage:
  sync-agent
  sync-agent --version
  sync-agent discover [--provider all|codex|claude] [--json]
  sync-agent folders [--json]
  sync-agent enrollment create [--role reader|writer|restore-admin] [--expires 15m]
  sync-agent enroll [--name NAME] [--platform PLATFORM]
  sync-agent policy get
  sync-agent policy set [--safety-window DURATION] [--gc-grace-period DURATION]
  sync-agent restore --target PATH [--folder-id UUID] [--sequence NUMBER] [--overwrite]
  sync-agent restore --target PATH --resume RESTORE_ID

When SYNC_ROOT is unset, sync-agent discovers Codex and Claude Git worktrees
and requires a user selection. Enter multiple numbers or ranges to synchronize
more than one worktree. Set SYNC_WORKTREES to a comma-separated list of IDs or
absolute paths for non-interactive startup.`)
}
