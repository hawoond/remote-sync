package worktree

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"
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

func SelectInteractiveMany(
	input io.Reader,
	output io.Writer,
	candidates []Candidate,
) ([]Candidate, error) {
	if len(candidates) == 0 {
		return nil, ErrNoCandidates
	}
	if err := WriteTable(output, candidates, true); err != nil {
		return nil, err
	}

	reader := bufio.NewReader(input)
	for {
		if _, err := fmt.Fprintf(
			output,
			"Select worktrees (for example 1,3-5 or all) or q to cancel: ",
		); err != nil {
			return nil, err
		}
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read worktree selection: %w", err)
		}
		value := strings.TrimSpace(line)
		if strings.EqualFold(value, "q") || strings.EqualFold(value, "quit") {
			return nil, ErrSelectionCancelled
		}
		if strings.EqualFold(value, "all") {
			return append([]Candidate(nil), candidates...), nil
		}
		indices, parseErr := parseSelectionIndices(value, len(candidates))
		if parseErr == nil {
			selected := make([]Candidate, 0, len(indices))
			for _, index := range indices {
				selected = append(selected, candidates[index-1])
			}
			return selected, nil
		}
		if errors.Is(err, io.EOF) {
			return nil, ErrSelectionRequired
		}
		if _, err := fmt.Fprintln(
			output,
			"Invalid selection. Enter numbers, ranges, all, or q.",
		); err != nil {
			return nil, err
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

func SelectReferences(candidates []Candidate, references []string) ([]Candidate, error) {
	if len(references) == 0 {
		return nil, ErrSelectionRequired
	}
	selected := make([]Candidate, 0, len(references))
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		candidate, err := SelectReference(candidates, reference)
		if err != nil {
			return nil, err
		}
		key := comparisonPath(candidate.Path)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		selected = append(selected, candidate)
	}
	if len(selected) == 0 {
		return nil, ErrSelectionRequired
	}
	return selected, nil
}

func parseSelectionIndices(value string, maximum int) ([]int, error) {
	if maximum < 1 || strings.TrimSpace(value) == "" {
		return nil, ErrSelectionRequired
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
	indices := make([]int, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		start, end, err := parseSelectionPart(part)
		if err != nil || start < 1 || end > maximum {
			return nil, ErrSelectionRequired
		}
		for index := start; index <= end; index++ {
			if _, exists := seen[index]; exists {
				continue
			}
			seen[index] = struct{}{}
			indices = append(indices, index)
		}
	}
	if len(indices) == 0 {
		return nil, ErrSelectionRequired
	}
	return indices, nil
}

func parseSelectionPart(value string) (int, int, error) {
	if !strings.Contains(value, "-") {
		index, err := strconv.Atoi(value)
		return index, index, err
	}
	bounds := strings.Split(value, "-")
	if len(bounds) != 2 {
		return 0, 0, ErrSelectionRequired
	}
	start, err := strconv.Atoi(bounds[0])
	if err != nil {
		return 0, 0, err
	}
	end, err := strconv.Atoi(bounds[1])
	if err != nil || start > end {
		return 0, 0, ErrSelectionRequired
	}
	return start, end, nil
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
