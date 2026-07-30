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

	"github.com/hawoond/remote-sync/internal/worktree"
)

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
	if configured := os.Getenv("SYNC_ROOT"); configured != "" {
		return validateRootPath(configured)
	}

	candidates, err := discoverWorktrees(
		ctx,
		environmentOrDefault("SYNC_WORKTREE_PROVIDERS", "all"),
	)
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", worktree.ErrNoCandidates
	}

	if reference := os.Getenv("SYNC_WORKTREE"); reference != "" {
		selected, err := worktree.SelectReference(candidates, reference)
		if err != nil {
			return "", err
		}
		if _, err := fmt.Fprintf(
			output,
			"Selected %s worktree %s at %s\n",
			selected.Provider,
			selected.ID,
			selected.Path,
		); err != nil {
			return "", err
		}
		return selected.Path, nil
	}

	if !isInteractive(input) {
		if err := worktree.WriteTable(output, candidates, true); err != nil {
			return "", err
		}
		return "", fmt.Errorf(
			"%w; set SYNC_WORKTREE to a listed ID or absolute path, or set SYNC_ROOT",
			worktree.ErrSelectionRequired,
		)
	}
	selected, err := worktree.SelectInteractive(input, output, candidates)
	if err != nil {
		return "", err
	}
	if _, err := fmt.Fprintf(
		output,
		"Selected %s worktree %s at %s\n",
		selected.Provider,
		selected.ID,
		selected.Path,
	); err != nil {
		return "", err
	}
	return selected.Path, nil
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
  sync-agent discover [--provider all|codex|claude] [--json]
  sync-agent enrollment create [--role reader|writer|restore-admin] [--expires 15m]
  sync-agent enroll [--name NAME] [--platform PLATFORM]
  sync-agent policy get
  sync-agent policy set [--safety-window DURATION] [--gc-grace-period DURATION]
  sync-agent restore --target PATH [--sequence NUMBER] [--overwrite]
  sync-agent restore --target PATH --resume RESTORE_ID

When SYNC_ROOT is unset, sync-agent discovers Codex and Claude Git worktrees
and requires a user selection. Set SYNC_WORKTREE to a listed ID or absolute
path for non-interactive startup.`)
}
