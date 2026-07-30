package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/hashing"
	"github.com/hawoond/remote-sync/internal/localdb"
	"github.com/hawoond/remote-sync/internal/pathpolicy"
)

type Scanner struct {
	rootPath string
	root     *os.Root
	store    *localdb.Store
	folderID string
}

type ScanIssue struct {
	Path string
	Err  error
}

type ScanReport struct {
	Generation int64
	FilesSeen  int
	Planned    int
	Deleted    int
	Issues     []ScanIssue
}

func NewScanner(rootPath, folderID string, store *localdb.Store) (*Scanner, error) {
	info, err := os.Stat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("stat sync root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("sync root is not a directory")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open sync root: %w", err)
	}
	return &Scanner{
		rootPath: rootPath,
		root:     root,
		store:    store,
		folderID: folderID,
	}, nil
}

func (s *Scanner) Close() error {
	return s.root.Close()
}

func (s *Scanner) Scan(ctx context.Context) (ScanReport, error) {
	info, err := os.Stat(s.rootPath)
	if err != nil {
		return ScanReport{}, fmt.Errorf("sync root unavailable: %w", err)
	}
	if !info.IsDir() {
		return ScanReport{}, fmt.Errorf("sync root is not a directory")
	}

	generation, err := s.store.BeginScan(ctx, s.folderID)
	if err != nil {
		return ScanReport{}, err
	}
	report := ScanReport{Generation: generation}

	walkErr := filepath.WalkDir(s.rootPath, func(fullPath string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if fullPath == s.rootPath {
			return nil
		}
		relative, err := filepath.Rel(s.rootPath, fullPath)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)

		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			report.Issues = append(report.Issues, ScanIssue{Path: relative, Err: hashing.ErrUnsupportedType})
			return nil
		}
		report.FilesSeen++

		canonical, err := pathpolicy.Canonicalize(relative)
		if err != nil {
			report.Issues = append(report.Issues, ScanIssue{Path: relative, Err: err})
			return nil
		}
		existing, entryErr := s.store.Entry(ctx, s.folderID, canonical.Key)
		found := entryErr == nil
		if entryErr != nil && !errors.Is(entryErr, localdb.ErrNotFound) {
			return entryErr
		}
		if found && existing.DisplayPath != canonical.Display {
			report.Issues = append(report.Issues, ScanIssue{
				Path: relative,
				Err:  fmt.Errorf("case-only or normalization-only rename is not portable"),
			})
			if err := s.store.MarkSeen(ctx, s.folderID, canonical.Key, generation); err != nil {
				return err
			}
			return nil
		}

		snapshot, err := hashing.Capture(ctx, s.root, filepath.FromSlash(canonical.Display))
		if err != nil {
			report.Issues = append(report.Issues, ScanIssue{Path: relative, Err: err})
			if found {
				return s.store.MarkSeen(ctx, s.folderID, canonical.Key, generation)
			}
			return nil
		}

		if found && existing.Present &&
			existing.Hash == snapshot.Hash &&
			existing.Size == snapshot.Size &&
			existing.MTimeUnixNano == snapshot.MTimeUnixNano &&
			existing.PortableMode == uint32(snapshot.Mode.Perm()) {
			existing.ScanGeneration = generation
			existing.Present = true
			return s.store.UpsertEntry(ctx, existing)
		}

		kind := domain.ChangeKindCreate
		baseVersion := ""
		if found {
			baseVersion = existing.ServerVersion
			if existing.Present {
				kind = domain.ChangeKindModify
			}
		}
		operation := localdb.Operation{
			OperationID:   uuid.NewString(),
			FolderID:      s.folderID,
			PathKey:       canonical.Key,
			DisplayPath:   canonical.Display,
			Kind:          kind,
			BaseVersionID: baseVersion,
			Hash:          snapshot.Hash,
			Size:          snapshot.Size,
			MTimeUnixNano: snapshot.MTimeUnixNano,
			PortableMode:  uint32(snapshot.Mode.Perm()),
		}
		if err := s.store.Enqueue(ctx, operation); err != nil {
			return err
		}
		if err := s.store.UpsertEntry(ctx, localdb.Entry{
			FolderID:       s.folderID,
			PathKey:        canonical.Key,
			DisplayPath:    canonical.Display,
			Size:           snapshot.Size,
			MTimeUnixNano:  snapshot.MTimeUnixNano,
			PortableMode:   uint32(snapshot.Mode.Perm()),
			Hash:           snapshot.Hash,
			ServerVersion:  baseVersion,
			Present:        true,
			ScanGeneration: generation,
		}); err != nil {
			return err
		}
		report.Planned++
		return nil
	})
	if walkErr != nil {
		return report, fmt.Errorf("scan sync root: %w", walkErr)
	}

	missing, err := s.store.MissingEntries(ctx, s.folderID, generation)
	if err != nil {
		return report, err
	}
	for _, entry := range missing {
		if entry.ServerVersion == "" {
			if err := s.store.CancelPath(ctx, s.folderID, entry.PathKey); err != nil {
				return report, err
			}
		} else {
			if err := s.store.Enqueue(ctx, localdb.Operation{
				OperationID:   uuid.NewString(),
				FolderID:      s.folderID,
				PathKey:       entry.PathKey,
				DisplayPath:   entry.DisplayPath,
				Kind:          domain.ChangeKindDelete,
				BaseVersionID: entry.ServerVersion,
			}); err != nil {
				return report, err
			}
			report.Deleted++
		}
		entry.Present = false
		entry.ScanGeneration = generation
		entry.Hash = domain.Hash{}
		entry.Size = 0
		if err := s.store.UpsertEntry(ctx, entry); err != nil {
			return report, err
		}
	}
	if err := s.store.CompleteScan(ctx, s.folderID, generation); err != nil {
		return report, err
	}
	return report, nil
}
