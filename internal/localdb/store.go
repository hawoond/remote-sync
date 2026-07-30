package localdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hawoond/remote-sync/internal/domain"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound       = errors.New("local state not found")
	ErrInvalidState   = errors.New("invalid local operation state")
	ErrCursorRollback = errors.New("cursor cannot move backwards")
)

type Store struct {
	db  *sql.DB
	now func() time.Time
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create local state directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open local state database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db, now: time.Now}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, statement := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA synchronous = FULL`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure local state database: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate local state database: %w", err)
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

type Entry struct {
	FolderID       string
	PathKey        string
	DisplayPath    string
	Size           int64
	MTimeUnixNano  int64
	PortableMode   uint32
	Hash           domain.Hash
	ServerVersion  string
	Present        bool
	ScanGeneration int64
}

func (s *Store) UpsertEntry(ctx context.Context, entry Entry) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO local_entries (
			folder_id, path_key, display_path, size, mtime_unix_nano,
			portable_mode, hash, server_version_id, present,
			scan_generation, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?)
		ON CONFLICT (folder_id, path_key) DO UPDATE SET
			display_path = excluded.display_path,
			size = excluded.size,
			mtime_unix_nano = excluded.mtime_unix_nano,
			portable_mode = excluded.portable_mode,
			hash = excluded.hash,
			server_version_id = COALESCE(excluded.server_version_id, local_entries.server_version_id),
			present = excluded.present,
			scan_generation = excluded.scan_generation,
			updated_at = excluded.updated_at
	`,
		entry.FolderID, entry.PathKey, entry.DisplayPath, entry.Size, entry.MTimeUnixNano,
		entry.PortableMode, nullableHash(entry.Hash), entry.ServerVersion, boolInt(entry.Present),
		entry.ScanGeneration, s.now().UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("upsert local entry: %w", err)
	}
	return nil
}

func (s *Store) Entry(ctx context.Context, folderID, pathKey string) (Entry, error) {
	var entry Entry
	var hash []byte
	var present int
	err := s.db.QueryRowContext(ctx, `
		SELECT folder_id, path_key, display_path, size, mtime_unix_nano,
		       portable_mode, hash, COALESCE(server_version_id, ''), present,
		       scan_generation
		FROM local_entries
		WHERE folder_id = ? AND path_key = ?
	`, folderID, pathKey).Scan(
		&entry.FolderID, &entry.PathKey, &entry.DisplayPath, &entry.Size,
		&entry.MTimeUnixNano, &entry.PortableMode, &hash, &entry.ServerVersion,
		&present, &entry.ScanGeneration,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("read local entry: %w", err)
	}
	if len(hash) > 0 {
		entry.Hash, err = domain.HashFromBytes(hash)
		if err != nil {
			return Entry{}, err
		}
	}
	entry.Present = present == 1
	return entry, nil
}

type Operation struct {
	OperationID   string
	FolderID      string
	PathKey       string
	DisplayPath   string
	Kind          domain.ChangeKind
	State         string
	BaseVersionID string
	Hash          domain.Hash
	Size          int64
	MTimeUnixNano int64
	PortableMode  uint32
	UploadSession string
	NextOffset    int64
	Attempt       int
	NextAttemptAt time.Time
	LastErrorCode string
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (s *Store) Enqueue(ctx context.Context, operation Operation) error {
	now := s.now().UnixNano()
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE pending_operations
			SET state = 'CANCELLED', updated_at = ?
			WHERE folder_id = ?
			  AND path_key = ?
			  AND state NOT IN ('COMPLETED', 'CANCELLED')
		`, now, operation.FolderID, operation.PathKey); err != nil {
			return fmt.Errorf("cancel superseded operation: %w", err)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO pending_operations (
				operation_id, folder_id, path_key, display_path, kind, state,
				base_version_id, hash, size, mtime_unix_nano, portable_mode,
				upload_session_id, next_offset, attempt, next_attempt_at,
				last_error_code, last_error_message, created_at, updated_at
			) VALUES (
				?, ?, ?, ?, ?, 'QUEUED',
				NULLIF(?, ''), ?, ?, ?, ?,
				NULL, 0, 0, 0, NULL, NULL, ?, ?
			)
		`,
			operation.OperationID, operation.FolderID, operation.PathKey, operation.DisplayPath,
			operation.Kind, operation.BaseVersionID, nullableHash(operation.Hash), operation.Size,
			operation.MTimeUnixNano, operation.PortableMode, now, now,
		)
		if err != nil {
			return fmt.Errorf("enqueue operation: %w", err)
		}
		return nil
	})
}

func (s *Store) CancelPath(ctx context.Context, folderID, pathKey string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE pending_operations
		SET state = 'CANCELLED', updated_at = ?
		WHERE folder_id = ?
		  AND path_key = ?
		  AND state NOT IN ('COMPLETED', 'CANCELLED')
	`, s.now().UnixNano(), folderID, pathKey)
	if err != nil {
		return fmt.Errorf("cancel path operations: %w", err)
	}
	return nil
}

func (s *Store) ClaimNext(ctx context.Context) (Operation, error) {
	now := s.now().UnixNano()
	var operation Operation
	var hash []byte
	var nextAttempt, createdAt, updatedAt int64
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			UPDATE pending_operations
			SET state = 'RESERVING', attempt = attempt + 1, updated_at = ?
			WHERE operation_id = (
				SELECT operation_id
				FROM pending_operations
				WHERE state = 'QUEUED'
				   OR (state = 'RETRY_WAIT' AND next_attempt_at <= ?)
				ORDER BY created_at
				LIMIT 1
			)
			RETURNING operation_id, folder_id, path_key, display_path, kind, state,
			          COALESCE(base_version_id, ''), hash, size, mtime_unix_nano,
			          portable_mode, COALESCE(upload_session_id, ''), next_offset,
			          attempt, next_attempt_at, COALESCE(last_error_code, ''),
			          COALESCE(last_error_message, ''), created_at, updated_at
		`, now, now)
		err := row.Scan(
			&operation.OperationID, &operation.FolderID, &operation.PathKey,
			&operation.DisplayPath, &operation.Kind, &operation.State,
			&operation.BaseVersionID, &hash, &operation.Size, &operation.MTimeUnixNano,
			&operation.PortableMode, &operation.UploadSession, &operation.NextOffset,
			&operation.Attempt, &nextAttempt, &operation.LastErrorCode,
			&operation.LastError, &createdAt, &updatedAt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return Operation{}, err
	}
	if len(hash) > 0 {
		operation.Hash, err = domain.HashFromBytes(hash)
		if err != nil {
			return Operation{}, err
		}
	}
	operation.NextAttemptAt = fromUnixNano(nextAttempt)
	operation.CreatedAt = fromUnixNano(createdAt)
	operation.UpdatedAt = fromUnixNano(updatedAt)
	return operation, nil
}

type Transition struct {
	UploadSession string
	NextOffset    int64
	NextAttemptAt time.Time
	ErrorCode     string
	ErrorMessage  string
}

func (s *Store) Transition(
	ctx context.Context,
	operationID, from, to string,
	update Transition,
) error {
	if !allowedTransition(from, to) {
		return ErrInvalidState
	}
	tag, err := s.db.ExecContext(ctx, `
		UPDATE pending_operations
		SET state = ?,
		    upload_session_id = CASE WHEN ? <> '' THEN ? ELSE upload_session_id END,
		    next_offset = CASE WHEN ? >= 0 THEN ? ELSE next_offset END,
		    next_attempt_at = ?,
		    last_error_code = NULLIF(?, ''),
		    last_error_message = NULLIF(?, ''),
		    updated_at = ?
		WHERE operation_id = ? AND state = ?
	`,
		to,
		update.UploadSession, update.UploadSession,
		update.NextOffset, update.NextOffset,
		unixNano(update.NextAttemptAt),
		update.ErrorCode, update.ErrorMessage,
		s.now().UnixNano(),
		operationID, from,
	)
	if err != nil {
		return fmt.Errorf("transition operation: %w", err)
	}
	rows, err := tag.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrInvalidState
	}
	return nil
}

func (s *Store) UpdateProgress(
	ctx context.Context,
	operationID, uploadSession string,
	nextOffset int64,
) error {
	if nextOffset < 0 {
		return ErrInvalidState
	}
	tag, err := s.db.ExecContext(ctx, `
		UPDATE pending_operations
		SET upload_session_id = NULLIF(?, ''),
		    next_offset = ?,
		    updated_at = ?
		WHERE operation_id = ? AND state = 'UPLOADING'
	`, uploadSession, nextOffset, s.now().UnixNano(), operationID)
	if err != nil {
		return fmt.Errorf("update operation progress: %w", err)
	}
	rows, _ := tag.RowsAffected()
	if rows != 1 {
		return ErrInvalidState
	}
	return nil
}

func (s *Store) RecoverInFlight(ctx context.Context) (int64, error) {
	tag, err := s.db.ExecContext(ctx, `
		UPDATE pending_operations
		SET state = 'RETRY_WAIT',
		    next_attempt_at = ?,
		    last_error_code = 'PROCESS_RESTARTED',
		    last_error_message = NULL,
		    updated_at = ?
		WHERE state IN ('RESERVING', 'UPLOADING', 'COMMITTING')
	`, s.now().UnixNano(), s.now().UnixNano())
	if err != nil {
		return 0, fmt.Errorf("recover in-flight operations: %w", err)
	}
	return tag.RowsAffected()
}

func (s *Store) MarkSeen(
	ctx context.Context,
	folderID, pathKey string,
	generation int64,
) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE local_entries
		SET present = 1, scan_generation = ?, updated_at = ?
		WHERE folder_id = ? AND path_key = ?
	`, generation, s.now().UnixNano(), folderID, pathKey)
	if err != nil {
		return fmt.Errorf("mark local entry seen: %w", err)
	}
	return nil
}

func (s *Store) SetServerVersion(
	ctx context.Context,
	folderID, pathKey, versionID string,
) error {
	tag, err := s.db.ExecContext(ctx, `
		UPDATE local_entries
		SET server_version_id = NULLIF(?, ''), updated_at = ?
		WHERE folder_id = ? AND path_key = ?
	`, versionID, s.now().UnixNano(), folderID, pathKey)
	if err != nil {
		return fmt.Errorf("set local server version: %w", err)
	}
	rows, _ := tag.RowsAffected()
	if rows != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) BeginScan(ctx context.Context, folderID string) (int64, error) {
	var generation int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO folder_scans (folder_id, generation, started_at, completed_at)
		VALUES (?, 1, ?, NULL)
		ON CONFLICT (folder_id) DO UPDATE SET
			generation = folder_scans.generation + 1,
			started_at = excluded.started_at,
			completed_at = NULL
		RETURNING generation
	`, folderID, s.now().UnixNano()).Scan(&generation)
	if err != nil {
		return 0, fmt.Errorf("begin scan: %w", err)
	}
	return generation, nil
}

func (s *Store) CompleteScan(ctx context.Context, folderID string, generation int64) error {
	tag, err := s.db.ExecContext(ctx, `
		UPDATE folder_scans
		SET completed_at = ?
		WHERE folder_id = ? AND generation = ? AND completed_at IS NULL
	`, s.now().UnixNano(), folderID, generation)
	if err != nil {
		return fmt.Errorf("complete scan: %w", err)
	}
	rows, _ := tag.RowsAffected()
	if rows != 1 {
		return ErrInvalidState
	}
	return nil
}

func (s *Store) MissingEntries(ctx context.Context, folderID string, generation int64) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT folder_id, path_key, display_path, size, mtime_unix_nano,
		       portable_mode, hash, COALESCE(server_version_id, ''), present,
		       scan_generation
		FROM local_entries
		WHERE folder_id = ? AND present = 1 AND scan_generation < ?
		ORDER BY path_key
	`, folderID, generation)
	if err != nil {
		return nil, fmt.Errorf("list missing entries: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var entry Entry
		var hash []byte
		var present int
		if err := rows.Scan(
			&entry.FolderID, &entry.PathKey, &entry.DisplayPath, &entry.Size,
			&entry.MTimeUnixNano, &entry.PortableMode, &hash, &entry.ServerVersion,
			&present, &entry.ScanGeneration,
		); err != nil {
			return nil, err
		}
		if len(hash) > 0 {
			entry.Hash, err = domain.HashFromBytes(hash)
			if err != nil {
				return nil, err
			}
		}
		entry.Present = present == 1
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (s *Store) SetCursor(ctx context.Context, folderID string, received, acked int64) error {
	if received < 0 || acked < 0 || acked > received {
		return ErrCursorRollback
	}
	tag, err := s.db.ExecContext(ctx, `
		INSERT INTO folder_cursors (folder_id, received_sequence, acked_sequence, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (folder_id) DO UPDATE SET
			received_sequence = excluded.received_sequence,
			acked_sequence = excluded.acked_sequence,
			updated_at = excluded.updated_at
		WHERE excluded.received_sequence >= folder_cursors.received_sequence
		  AND excluded.acked_sequence >= folder_cursors.acked_sequence
	`, folderID, received, acked, s.now().UnixNano())
	if err != nil {
		return fmt.Errorf("set folder cursor: %w", err)
	}
	rows, _ := tag.RowsAffected()
	if rows != 1 {
		return ErrCursorRollback
	}
	return nil
}

func allowedTransition(from, to string) bool {
	allowed := map[string]map[string]bool{
		"RESERVING": {
			"UPLOADING": true, "COMMITTING": true, "RETRY_WAIT": true,
			"BLOCKED": true, "QUARANTINED": true,
		},
		"UPLOADING": {
			"COMMITTING": true, "RETRY_WAIT": true, "BLOCKED": true,
		},
		"COMMITTING": {
			"COMPLETED": true, "RETRY_WAIT": true, "BLOCKED": true,
			"QUARANTINED": true,
		},
	}
	return allowed[from][to]
}

func nullableHash(hash domain.Hash) any {
	if hash.IsZero() {
		return nil
	}
	return hash[:]
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func unixNano(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}

func fromUnixNano(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

func withTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
