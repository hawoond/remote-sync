package agent

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

type Watcher struct {
	rootPath string
}

func NewWatcher(rootPath string) *Watcher {
	return &Watcher{rootPath: rootPath}
}

func (w *Watcher) Run(ctx context.Context, triggers chan<- struct{}) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create filesystem watcher: %w", err)
	}
	defer watcher.Close()

	if err := addDirectories(watcher, w.rootPath); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-watcher.Errors:
			if !ok {
				return fmt.Errorf("filesystem watcher stopped")
			}
			return fmt.Errorf("filesystem watcher: %w", err)
		case event, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("filesystem watcher stopped")
			}
			if event.Has(fsnotify.Create) {
				info, statErr := os.Lstat(event.Name)
				if statErr == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
					if err := addDirectories(watcher, event.Name); err != nil {
						return err
					}
				}
			}
			select {
			case triggers <- struct{}{}:
			default:
			}
		}
	}
}

func addDirectories(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if err := watcher.Add(path); err != nil {
			return fmt.Errorf("watch directory: %w", err)
		}
		return nil
	})
}
