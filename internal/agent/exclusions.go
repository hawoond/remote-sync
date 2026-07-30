package agent

import (
	"os"
	"path/filepath"
	"strings"
)

func shouldSkipRelativePath(relative string) bool {
	relative = filepath.ToSlash(filepath.Clean(relative))
	return relative == ".git" ||
		strings.HasPrefix(relative, ".git/") ||
		relative == ".claude/worktrees" ||
		strings.HasPrefix(relative, ".claude/worktrees/")
}

func isNestedRepository(path string) bool {
	_, err := os.Lstat(filepath.Join(path, ".git"))
	return err == nil
}
