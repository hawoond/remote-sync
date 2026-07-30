package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func Up(ctx context.Context, pool *pgxpool.Pool, directory string) error {
	if err := ensureTable(ctx, pool); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read migration directory: %w", err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		checksum := sha256.Sum256(content)
		if err := apply(ctx, pool, name, hex.EncodeToString(checksum[:]), string(content)); err != nil {
			return err
		}
	}
	return nil
}

func ensureTable(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version text PRIMARY KEY,
			checksum text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)
	`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	return nil
}

func apply(ctx context.Context, pool *pgxpool.Pool, version, checksum, sql string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var existing string
		err := tx.QueryRow(ctx,
			`SELECT checksum FROM schema_migrations WHERE version = $1 FOR UPDATE`,
			version,
		).Scan(&existing)
		switch {
		case err == nil:
			if existing != checksum {
				return fmt.Errorf("migration %s checksum changed", version)
			}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("read migration %s state: %w", version, err)
		}

		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations(version, checksum) VALUES ($1, $2)`,
			version, checksum,
		); err != nil {
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		return nil
	})
}

func ValidateDirectory(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) != ".sql" {
			return fmt.Errorf("unexpected migration file %s", entry.Name())
		}
		if _, err := fs.Stat(os.DirFS(directory), entry.Name()); err != nil {
			return err
		}
	}
	return nil
}
