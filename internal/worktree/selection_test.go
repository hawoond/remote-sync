package worktree

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectInteractiveRequiresUserChoice(t *testing.T) {
	t.Parallel()

	candidates := selectionCandidates(t)
	var output bytes.Buffer
	selected, err := SelectInteractive(strings.NewReader("invalid\n2\n"), &output, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != candidates[1].ID {
		t.Fatalf("selected = %+v, want second candidate", selected)
	}
	text := output.String()
	if !strings.Contains(text, "Invalid selection") ||
		!strings.Contains(text, candidates[0].Path) ||
		!strings.Contains(text, candidates[1].Path) {
		t.Fatalf("selection output = %q", text)
	}
}

func TestSelectInteractiveCanCancel(t *testing.T) {
	t.Parallel()

	_, err := SelectInteractive(
		strings.NewReader("q\n"),
		&bytes.Buffer{},
		selectionCandidates(t),
	)
	if !errors.Is(err, ErrSelectionCancelled) {
		t.Fatalf("error = %v, want ErrSelectionCancelled", err)
	}
}

func TestSelectInteractiveRejectsEOF(t *testing.T) {
	t.Parallel()

	_, err := SelectInteractive(strings.NewReader(""), &bytes.Buffer{}, selectionCandidates(t))
	if !errors.Is(err, ErrSelectionRequired) {
		t.Fatalf("error = %v, want ErrSelectionRequired", err)
	}
}

func TestSelectInteractiveDoesNotAutoSelectSingleCandidate(t *testing.T) {
	t.Parallel()

	candidates := selectionCandidates(t)[:1]
	var output bytes.Buffer
	_, err := SelectInteractive(strings.NewReader(""), &output, candidates)
	if !errors.Is(err, ErrSelectionRequired) {
		t.Fatalf("error = %v, want ErrSelectionRequired", err)
	}
	if !strings.Contains(output.String(), candidates[0].Path) {
		t.Fatalf("selection output = %q", output.String())
	}
}

func TestSelectInteractiveManyAcceptsNumbersRangesAndDeduplicates(t *testing.T) {
	t.Parallel()

	candidates := append(selectionCandidates(t), Candidate{
		ID:         "codex:333333333333",
		Provider:   ProviderCodex,
		Name:       "three",
		Path:       filepath.Join(t.TempDir(), "three"),
		Repository: "repo",
		Branch:     "worktree-three",
		Head:       "fedcba654321",
	})
	var output bytes.Buffer
	selected, err := SelectInteractiveMany(
		strings.NewReader("invalid\n2,1-2 3\n"),
		&output,
		candidates,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 3 ||
		selected[0].ID != candidates[1].ID ||
		selected[1].ID != candidates[0].ID ||
		selected[2].ID != candidates[2].ID {
		t.Fatalf("selected = %+v", selected)
	}
	if !strings.Contains(output.String(), "Invalid selection") {
		t.Fatalf("selection output = %q", output.String())
	}
}

func TestSelectInteractiveManyAcceptsExplicitAll(t *testing.T) {
	t.Parallel()

	candidates := selectionCandidates(t)
	selected, err := SelectInteractiveMany(
		strings.NewReader("all\n"),
		&bytes.Buffer{},
		candidates,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != len(candidates) {
		t.Fatalf("selected %d candidates, want %d", len(selected), len(candidates))
	}
}

func TestSelectInteractiveManyStillRequiresChoiceForSingleCandidate(t *testing.T) {
	t.Parallel()

	_, err := SelectInteractiveMany(
		strings.NewReader(""),
		&bytes.Buffer{},
		selectionCandidates(t)[:1],
	)
	if !errors.Is(err, ErrSelectionRequired) {
		t.Fatalf("error = %v, want ErrSelectionRequired", err)
	}
}

func TestSelectReferenceAcceptsIDOrPath(t *testing.T) {
	t.Parallel()

	candidates := selectionCandidates(t)
	byID, err := SelectReference(candidates, strings.ToUpper(candidates[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if byID.ID != candidates[0].ID {
		t.Fatalf("selected by ID = %+v", byID)
	}

	byPath, err := SelectReference(candidates, candidates[1].Path)
	if err != nil {
		t.Fatal(err)
	}
	if byPath.ID != candidates[1].ID {
		t.Fatalf("selected by path = %+v", byPath)
	}
	if _, err := SelectReference(candidates, "codex:missing"); err == nil {
		t.Fatal("expected missing reference error")
	}
}

func TestSelectReferencesPreservesOrderAndDeduplicates(t *testing.T) {
	t.Parallel()

	candidates := selectionCandidates(t)
	selected, err := SelectReferences(candidates, []string{
		candidates[1].ID,
		candidates[0].Path,
		candidates[1].Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 ||
		selected[0].ID != candidates[1].ID ||
		selected[1].ID != candidates[0].ID {
		t.Fatalf("selected = %+v", selected)
	}
}

func TestWriteTableHandlesEmptyResult(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	if err := WriteTable(&output, nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "No Codex or Claude worktrees found") {
		t.Fatalf("output = %q", output.String())
	}
}

func selectionCandidates(t *testing.T) []Candidate {
	t.Helper()
	root := t.TempDir()
	return []Candidate{
		{
			ID:         "codex:111111111111",
			Provider:   ProviderCodex,
			Name:       "one",
			Path:       filepath.Join(root, "one"),
			Repository: "repo",
			Branch:     "(detached)",
			Head:       "123456789abc",
		},
		{
			ID:         "claude:222222222222",
			Provider:   ProviderClaude,
			Name:       "two",
			Path:       filepath.Join(root, "two"),
			Repository: "repo",
			Branch:     "worktree-two",
			Head:       "abcdef123456",
		},
	}
}
