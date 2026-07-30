package worktree

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
)

var (
	ErrNoCandidates       = errors.New("no Codex or Claude worktrees found")
	ErrSelectionCancelled = errors.New("worktree selection cancelled")
	ErrSelectionRequired  = errors.New("worktree selection requires interactive input")
)

func SelectInteractive(
	input io.Reader,
	output io.Writer,
	candidates []Candidate,
) (Candidate, error) {
	if len(candidates) == 0 {
		return Candidate{}, ErrNoCandidates
	}
	if err := WriteTable(output, candidates, true); err != nil {
		return Candidate{}, err
	}

	reader := bufio.NewReader(input)
	for {
		if _, err := fmt.Fprintf(
			output,
			"Select a worktree [1-%d] or q to cancel: ",
			len(candidates),
		); err != nil {
			return Candidate{}, err
		}
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return Candidate{}, fmt.Errorf("read worktree selection: %w", err)
		}
		value := strings.TrimSpace(line)
		if strings.EqualFold(value, "q") || strings.EqualFold(value, "quit") {
			return Candidate{}, ErrSelectionCancelled
		}
		index, parseErr := strconv.Atoi(value)
		if parseErr == nil && index >= 1 && index <= len(candidates) {
			return candidates[index-1], nil
		}
		if errors.Is(err, io.EOF) {
			return Candidate{}, ErrSelectionRequired
		}
		if _, err := fmt.Fprintln(output, "Invalid selection. Enter a listed number or q."); err != nil {
			return Candidate{}, err
		}
	}
}

func SelectReference(candidates []Candidate, reference string) (Candidate, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return Candidate{}, ErrSelectionRequired
	}
	for _, candidate := range candidates {
		if strings.EqualFold(candidate.ID, reference) {
			return candidate, nil
		}
	}

	path, err := absolutePath(reference)
	if err == nil {
		for _, candidate := range candidates {
			if comparisonPath(candidate.Path) == comparisonPath(path) {
				return candidate, nil
			}
		}
	}
	return Candidate{}, fmt.Errorf("worktree %q was not found", reference)
}

func WriteTable(output io.Writer, candidates []Candidate, numbered bool) error {
	if len(candidates) == 0 {
		_, err := fmt.Fprintln(output, "No Codex or Claude worktrees found.")
		return err
	}

	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if numbered {
		if _, err := fmt.Fprintln(writer, "#\tPROVIDER\tREPOSITORY\tBRANCH\tID\tPATH"); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(writer, "PROVIDER\tREPOSITORY\tBRANCH\tID\tPATH"); err != nil {
		return err
	}
	for index, candidate := range candidates {
		branch := candidate.Branch
		if branch == "" {
			branch = "(detached)"
		}
		if numbered {
			if _, err := fmt.Fprintf(
				writer,
				"%d\t%s\t%s\t%s\t%s\t%s\n",
				index+1,
				candidate.Provider,
				candidate.Repository,
				branch,
				candidate.ID,
				candidate.Path,
			); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(
			writer,
			"%s\t%s\t%s\t%s\t%s\n",
			candidate.Provider,
			candidate.Repository,
			branch,
			candidate.ID,
			candidate.Path,
		); err != nil {
			return err
		}
	}
	return writer.Flush()
}
