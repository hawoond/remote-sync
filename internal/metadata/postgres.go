package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Postgres struct {
	pool   *pgxpool.Pool
	limits Limits
	now    func() time.Time
}

func NewPostgres(pool *pgxpool.Pool, limits Limits) *Postgres {
	return &Postgres{
		pool:   pool,
		limits: limits,
		now:    time.Now,
	}
}

func (p *Postgres) BeginUpload(ctx context.Context, params BeginUploadParams) (domain.UploadReservation, error) {
	input := params.Upload
	now := p.now().UTC()

	var result domain.UploadReservation
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		userID, err := writerUser(ctx, tx, input.DeviceID, input.FolderID, false)
		if err != nil {
			return err
		}

		existing, found, err := readOperation(ctx, tx, input.DeviceID, input.OperationID)
		if err != nil {
			return err
		}
		if found {
			if !bytes.Equal(existing.RequestDigest, input.RequestDigest[:]) {
				return ErrOperationIDReused
			}
			result, err = replayBeginUpload(
				ctx,
				tx,
				input.DeviceID,
				input.OperationID,
				existing,
				input.DisplayPath,
				p.limits.MaxChunkSize,
			)
			return err
		}

		state := "RESERVED"
		if params.ObjectPresent {
			state = "READY"
			if err := ensureObjectTx(ctx, tx, input.Hash, input.Size, params.StorageKey); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO operations (
				device_id, operation_id, folder_id, path_key, display_path,
				request_digest, base_version_id, object_hash, declared_size,
				mtime_unix_nano, portable_mode, state
			) VALUES (
				$1, $2, $3, $4, $5,
				$6, NULLIF($7, '')::uuid, $8, $9,
				$10, $11, $12
			)
		`,
			input.DeviceID, input.OperationID, input.FolderID, input.PathKey, input.DisplayPath,
			input.RequestDigest[:], input.BaseVersionID, input.Hash[:], input.Size,
			input.MTimeUnixNano, input.PortableMode, state,
		)
		if err != nil {
			return fmt.Errorf("insert operation: %w", err)
		}

		result = domain.UploadReservation{
			DisplayPath:  input.DisplayPath,
			MaxChunkSize: p.limits.MaxChunkSize,
		}
		if params.ObjectPresent {
			result.Disposition = domain.UploadDispositionObjectPresent
			return nil
		}

		if err := reserveQuota(ctx, tx, userID, input.FolderID, input.Size, p.limits); err != nil {
			return err
		}
		sessionID := uuid.NewString()
		expiresAt := now.Add(p.limits.UploadSessionTTL)
		_, err = tx.Exec(ctx, `
			INSERT INTO upload_sessions (
				id, device_id, operation_id, folder_id, hash, declared_size,
				temp_key, state, reserved_bytes, expires_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, 'ACTIVE', $6, $8)
		`,
			sessionID, input.DeviceID, input.OperationID, input.FolderID, input.Hash[:], input.Size,
			"uploads/"+sessionID+".part", expiresAt,
		)
		if err != nil {
			return fmt.Errorf("insert upload session: %w", err)
		}
		result.Disposition = domain.UploadDispositionRequired
		result.SessionID = sessionID
		result.ExpiresAt = expiresAt
		return nil
	})
	if err != nil {
		return domain.UploadReservation{}, err
	}
	return result, nil
}

func (p *Postgres) UploadSession(ctx context.Context, sessionID string) (UploadSession, error) {
	var session UploadSession
	var hash []byte
	err := p.pool.QueryRow(ctx, `
		SELECT id, device_id, operation_id, folder_id, hash, declared_size,
		       received_size, state, expires_at
		FROM upload_sessions
		WHERE id = $1
	`, sessionID).Scan(
		&session.ID, &session.DeviceID, &session.OperationID, &session.FolderID,
		&hash, &session.DeclaredSize, &session.ReceivedSize, &session.State, &session.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return UploadSession{}, ErrNotFound
	}
	if err != nil {
		return UploadSession{}, fmt.Errorf("read upload session: %w", err)
	}
	session.Hash, err = domain.HashFromBytes(hash)
	if err != nil {
		return UploadSession{}, err
	}
	if session.ExpiresAt.Before(p.now()) && session.State == "ACTIVE" {
		return UploadSession{}, ErrSessionExpired
	}
	return session, nil
}

func (p *Postgres) UpdateUploadProgress(ctx context.Context, sessionID string, received int64) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE upload_sessions
		SET received_size = $2, updated_at = now()
		WHERE id = $1
		  AND state = 'ACTIVE'
		  AND expires_at > now()
		  AND received_size <= $2
		  AND declared_size >= $2
	`, sessionID, received)
	if err != nil {
		return fmt.Errorf("update upload progress: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return nil
}

func (p *Postgres) MarkUploadVerified(ctx context.Context, sessionID, storageKey string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var deviceID, operationID, state string
		var hashBytes []byte
		var declaredSize, receivedSize int64
		var expiresAt time.Time
		err := tx.QueryRow(ctx, `
			SELECT device_id, operation_id, hash, declared_size, received_size, state, expires_at
			FROM upload_sessions
			WHERE id = $1
			FOR UPDATE
		`, sessionID).Scan(
			&deviceID, &operationID, &hashBytes, &declaredSize, &receivedSize, &state, &expiresAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock upload session: %w", err)
		}
		if state == "VERIFIED" {
			return nil
		}
		if state != "ACTIVE" || expiresAt.Before(p.now()) || receivedSize != declaredSize {
			return ErrUploadNotVerified
		}
		hash, err := domain.HashFromBytes(hashBytes)
		if err != nil {
			return err
		}
		if err := ensureObjectTx(ctx, tx, hash, declaredSize, storageKey); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE upload_sessions
			SET state = 'VERIFIED', updated_at = now()
			WHERE id = $1
		`, sessionID); err != nil {
			return fmt.Errorf("mark session verified: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE operations
			SET state = 'READY'
			WHERE device_id = $1 AND operation_id = $2
		`, deviceID, operationID); err != nil {
			return fmt.Errorf("mark operation ready: %w", err)
		}
		return nil
	})
}

func (p *Postgres) EnsureObject(ctx context.Context, hash domain.Hash, size int64, storageKey string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		return ensureObjectTx(ctx, tx, hash, size, storageKey)
	})
}

func (p *Postgres) Commit(ctx context.Context, input domain.CommitChange) (domain.CommitResult, error) {
	var result domain.CommitResult
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		operation, found, err := readOperationForUpdate(ctx, tx, input.DeviceID, input.OperationID)
		if err != nil {
			return err
		}
		if found {
			if !bytes.Equal(operation.RequestDigest, input.RequestDigest[:]) {
				return ErrOperationIDReused
			}
			if operation.State == "COMPLETED" {
				result, err = decodeStoredResult(operation.Response)
				if err == nil {
					result.Disposition = domain.CommitDispositionIdempotentReplay
				}
				return err
			}
		} else {
			if input.Kind != domain.ChangeKindDelete {
				return ErrNotFound
			}
		}

		userID, currentSequence, err := lockWritableFolder(ctx, tx, input.DeviceID, input.FolderID)
		if err != nil {
			return err
		}

		if !found {
			_, err := tx.Exec(ctx, `
				INSERT INTO operations (
					device_id, operation_id, folder_id, path_key, display_path,
					request_digest, base_version_id, declared_size, mtime_unix_nano,
					portable_mode, state
				) VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::uuid, 0, $8, $9, 'READY')
			`,
				input.DeviceID, input.OperationID, input.FolderID, input.PathKey, input.DisplayPath,
				input.RequestDigest[:], input.BaseVersionID, input.MTimeUnixNano, input.PortableMode,
			)
			if err != nil {
				return fmt.Errorf("insert delete operation: %w", err)
			}
			operation = operationRow{
				State:         "READY",
				DisplayPath:   input.DisplayPath,
				PathKey:       input.PathKey,
				BaseVersionID: input.BaseVersionID,
			}
		}
		if operation.State != "READY" {
			return ErrUploadNotVerified
		}
		if operation.DisplayPath != input.DisplayPath || operation.PathKey != input.PathKey ||
			operation.BaseVersionID != input.BaseVersionID || operation.DeclaredSize != input.Size {
			return ErrOperationIDReused
		}

		if err := lockQuotaRows(ctx, tx, userID, input.FolderID); err != nil {
			return err
		}

		entry, found, err := lockEntry(ctx, tx, input.FolderID, input.PathKey)
		if err != nil {
			return err
		}
		if found {
			if entry.HeadVersionID != input.BaseVersionID {
				return ErrBaseVersionConflict
			}
		} else if input.BaseVersionID != "" {
			return ErrBaseVersionConflict
		}

		objectHash := operation.ObjectHash
		objectSize := operation.DeclaredSize
		if input.Kind == domain.ChangeKindDelete {
			objectHash = nil
			objectSize = 0
		} else {
			if input.ObjectHash != (domain.Hash{}) && !bytes.Equal(input.ObjectHash[:], objectHash) {
				return ErrOperationIDReused
			}
			if err := requireAvailableObject(ctx, tx, objectHash, objectSize); err != nil {
				return err
			}
			if err := requireMatchingSession(
				ctx,
				tx,
				input.DeviceID,
				input.OperationID,
				input.UploadSession,
			); err != nil {
				return err
			}
		}

		oldLive := int64(0)
		if found && !entry.Deleted {
			oldLive = entry.HeadSize
		}
		newLive := objectSize
		if input.Kind == domain.ChangeKindDelete {
			newLive = 0
		}
		if err := checkLiveQuota(ctx, tx, userID, input.FolderID, newLive-oldLive, p.limits); err != nil {
			return err
		}

		entryID := entry.ID
		if !found {
			entryID = uuid.NewString()
			if _, err := tx.Exec(ctx, `
				INSERT INTO entries (id, folder_id, path_key, display_path)
				VALUES ($1, $2, $3, $4)
			`, entryID, input.FolderID, input.PathKey, input.DisplayPath); err != nil {
				return fmt.Errorf("insert entry: %w", err)
			}
		}

		versionID := uuid.NewString()
		sequence := currentSequence + 1
		kind := kindName(input.Kind)
		_, err = tx.Exec(ctx, `
			INSERT INTO file_versions (
				id, folder_id, entry_id, base_version_id, object_hash, size,
				kind, origin_device_id, operation_id, sequence,
				mtime_unix_nano, portable_mode
			) VALUES (
				$1, $2, $3, NULLIF($4, '')::uuid, $5, $6,
				$7, $8, $9, $10, $11, $12
			)
		`,
			versionID, input.FolderID, entryID, input.BaseVersionID, objectHash, objectSize,
			kind, input.DeviceID, input.OperationID, sequence,
			input.MTimeUnixNano, input.PortableMode,
		)
		if err != nil {
			return fmt.Errorf("insert file version: %w", err)
		}

		if found && entry.HeadVersionID != "" {
			if _, err := tx.Exec(ctx, `
				UPDATE file_versions
				SET superseded_at = COALESCE(superseded_at, now())
				WHERE id = $1
			`, entry.HeadVersionID); err != nil {
				return fmt.Errorf("mark previous version superseded: %w", err)
			}
		}

		deleted := input.Kind == domain.ChangeKindDelete
		if _, err := tx.Exec(ctx, `
			UPDATE entries
			SET display_path = $2, head_version_id = $3, deleted = $4, updated_at = now()
			WHERE id = $1
		`, entryID, input.DisplayPath, versionID, deleted); err != nil {
			return fmt.Errorf("update entry head: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE folders SET current_sequence = $2 WHERE id = $1
		`, input.FolderID, sequence); err != nil {
			return fmt.Errorf("update folder sequence: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO changes (folder_id, sequence, entry_id, version_id, kind)
			VALUES ($1, $2, $3, $4, $5)
		`, input.FolderID, sequence, entryID, versionID, kind); err != nil {
			return fmt.Errorf("insert change: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO device_cursors (folder_id, device_id, acked_sequence)
			VALUES ($1, $2, $3)
			ON CONFLICT (folder_id, device_id) DO UPDATE
			SET acked_sequence = GREATEST(
					device_cursors.acked_sequence,
					EXCLUDED.acked_sequence
				),
			    updated_at = now()
		`, input.FolderID, input.DeviceID, sequence); err != nil {
			return fmt.Errorf("acknowledge origin change: %w", err)
		}

		if err := updateUsageAfterCommit(ctx, tx, userID, input.FolderID, oldLive, newLive, operation.DeclaredSize); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE upload_sessions
			SET state = 'COMMITTED', reserved_bytes = 0, updated_at = now()
			WHERE device_id = $1 AND operation_id = $2 AND state = 'VERIFIED'
		`, input.DeviceID, input.OperationID); err != nil {
			return fmt.Errorf("commit upload session: %w", err)
		}

		result = domain.CommitResult{
			Disposition: domain.CommitDispositionCommitted,
			VersionID:   versionID,
			Sequence:    sequence,
			DisplayPath: input.DisplayPath,
		}
		response, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE operations
			SET state = 'COMPLETED', version_id = $3, response = $4, completed_at = now()
			WHERE device_id = $1 AND operation_id = $2
		`, input.DeviceID, input.OperationID, versionID, response); err != nil {
			return fmt.Errorf("complete operation: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.CommitResult{}, err
	}
	return result, nil
}

func (p *Postgres) ListChanges(
	ctx context.Context,
	deviceID, folderID string,
	after int64,
	limit int,
) ([]domain.Change, int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var latest int64
	err := p.pool.QueryRow(ctx, `
		SELECT f.current_sequence
		FROM folders f
		JOIN folder_devices fd ON fd.folder_id = f.id
		JOIN devices d ON d.id = fd.device_id
		WHERE f.id = $1 AND d.id = $2 AND fd.can_read AND d.status = 'ACTIVE'
	`, folderID, deviceID).Scan(&latest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrPermissionDenied
	}
	if err != nil {
		return nil, 0, fmt.Errorf("authorize change list: %w", err)
	}

	rows, err := p.pool.Query(ctx, `
		SELECT c.sequence, c.entry_id, c.version_id, e.display_path, c.kind,
		       fv.object_hash, fv.size, fv.mtime_unix_nano, fv.portable_mode
		FROM changes c
		JOIN entries e ON e.id = c.entry_id
		JOIN file_versions fv ON fv.id = c.version_id
		WHERE c.folder_id = $1 AND c.sequence > $2
		ORDER BY c.sequence
		LIMIT $3
	`, folderID, after, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list changes: %w", err)
	}
	defer rows.Close()

	changes := make([]domain.Change, 0, limit)
	for rows.Next() {
		var change domain.Change
		var kind string
		var hashBytes []byte
		if err := rows.Scan(
			&change.Sequence, &change.EntryID, &change.VersionID, &change.DisplayPath,
			&kind, &hashBytes, &change.Size, &change.MTimeUnixNano, &change.PortableMode,
		); err != nil {
			return nil, 0, fmt.Errorf("scan change: %w", err)
		}
		change.Kind = parseKind(kind)
		if len(hashBytes) > 0 {
			change.ObjectHash, err = domain.HashFromBytes(hashBytes)
			if err != nil {
				return nil, 0, err
			}
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate changes: %w", err)
	}
	return changes, latest, nil
}

func (p *Postgres) AckChanges(ctx context.Context, deviceID, folderID string, sequence int64) error {
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO device_cursors (folder_id, device_id, acked_sequence)
		SELECT f.id, d.id, $3::bigint
		FROM folders f
		JOIN folder_devices fd ON fd.folder_id = f.id
		JOIN devices d ON d.id = fd.device_id
		WHERE f.id = $1
		  AND d.id = $2
		  AND fd.can_read
		  AND $3::bigint BETWEEN 0 AND f.current_sequence
		ON CONFLICT (folder_id, device_id) DO UPDATE
		SET acked_sequence = GREATEST(device_cursors.acked_sequence, EXCLUDED.acked_sequence),
		    updated_at = now()
	`, folderID, deviceID, sequence)
	if err != nil {
		return fmt.Errorf("ack changes: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrPermissionDenied
	}
	return nil
}

func (p *Postgres) BootstrapDevelopment(
	ctx context.Context,
	userID, deviceID, folderID string,
	credentialDigest domain.Hash,
) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO users (id, status) VALUES ($1, 'ACTIVE')
			ON CONFLICT (id) DO NOTHING
		`, userID); err != nil {
			return fmt.Errorf("bootstrap user: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO devices (
				id, user_id, name, platform, credential_digest, status
			)
			VALUES (
				$1, $2, 'development-device', 'development', $3, 'ACTIVE'
			)
			ON CONFLICT (id) DO UPDATE
			SET credential_digest = EXCLUDED.credential_digest,
			    status = 'ACTIVE'
		`, deviceID, userID, credentialDigest[:]); err != nil {
			return fmt.Errorf("bootstrap device: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO folders (id, owner_user_id, mode, state)
			VALUES ($1, $2, 'BACKUP', 'ACTIVE')
			ON CONFLICT (id) DO NOTHING
		`, folderID, userID); err != nil {
			return fmt.Errorf("bootstrap folder: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO folder_devices (folder_id, device_id, role, can_read, can_write)
			VALUES ($1, $2, 'RESTORE_ADMIN', true, true)
			ON CONFLICT (folder_id, device_id) DO UPDATE
			SET role = 'RESTORE_ADMIN', can_read = true, can_write = true
		`, folderID, deviceID); err != nil {
			return fmt.Errorf("bootstrap folder device: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO folder_policies (folder_id)
			VALUES ($1)
			ON CONFLICT (folder_id) DO NOTHING
		`, folderID); err != nil {
			return fmt.Errorf("bootstrap folder policy: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO device_cursors (folder_id, device_id, acked_sequence)
			VALUES ($1, $2, 0)
			ON CONFLICT (folder_id, device_id) DO NOTHING
		`, folderID, deviceID); err != nil {
			return fmt.Errorf("bootstrap device cursor: %w", err)
		}
		for _, scope := range []struct {
			Type string
			ID   string
		}{{"USER", userID}, {"FOLDER", folderID}} {
			if _, err := tx.Exec(ctx, `
				INSERT INTO quota_usages (scope_type, scope_id)
				VALUES ($1, $2)
				ON CONFLICT (scope_type, scope_id) DO NOTHING
			`, scope.Type, scope.ID); err != nil {
				return fmt.Errorf("bootstrap quota usage: %w", err)
			}
		}
		return nil
	})
}

type operationRow struct {
	State         string
	RequestDigest []byte
	DisplayPath   string
	PathKey       string
	BaseVersionID string
	ObjectHash    []byte
	DeclaredSize  int64
	Response      []byte
}

func readOperation(ctx context.Context, tx pgx.Tx, deviceID, operationID string) (operationRow, bool, error) {
	return scanOperation(tx.QueryRow(ctx, `
		SELECT state, request_digest, display_path, path_key,
		       COALESCE(base_version_id::text, ''), object_hash, declared_size,
		       COALESCE(response::text, '')
		FROM operations
		WHERE device_id = $1 AND operation_id = $2
	`, deviceID, operationID))
}

func readOperationForUpdate(ctx context.Context, tx pgx.Tx, deviceID, operationID string) (operationRow, bool, error) {
	return scanOperation(tx.QueryRow(ctx, `
		SELECT state, request_digest, display_path, path_key,
		       COALESCE(base_version_id::text, ''), object_hash, declared_size,
		       COALESCE(response::text, '')
		FROM operations
		WHERE device_id = $1 AND operation_id = $2
		FOR UPDATE
	`, deviceID, operationID))
}

func scanOperation(row pgx.Row) (operationRow, bool, error) {
	var operation operationRow
	var responseText string
	err := row.Scan(
		&operation.State, &operation.RequestDigest, &operation.DisplayPath, &operation.PathKey,
		&operation.BaseVersionID, &operation.ObjectHash, &operation.DeclaredSize, &responseText,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return operationRow{}, false, nil
	}
	if err != nil {
		return operationRow{}, false, fmt.Errorf("read operation: %w", err)
	}
	operation.Response = []byte(responseText)
	return operation, true, nil
}

func replayBeginUpload(
	ctx context.Context,
	tx pgx.Tx,
	deviceID string,
	operationID string,
	operation operationRow,
	displayPath string,
	maxChunkSize int32,
) (domain.UploadReservation, error) {
	result := domain.UploadReservation{
		DisplayPath:  displayPath,
		MaxChunkSize: maxChunkSize,
	}
	if operation.State == "READY" || operation.State == "COMPLETED" {
		result.Disposition = domain.UploadDispositionObjectPresent
		return result, nil
	}

	var state string
	err := tx.QueryRow(ctx, `
		SELECT id, received_size, expires_at, state
		FROM upload_sessions
		WHERE device_id = $1
		  AND operation_id = $2
	`, deviceID, operationID).Scan(
		&result.SessionID, &result.NextOffset, &result.ExpiresAt, &state,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.UploadReservation{}, ErrInvalidState
		}
		return domain.UploadReservation{}, fmt.Errorf("replay upload reservation: %w", err)
	}
	if state != "ACTIVE" && state != "VERIFIED" {
		return domain.UploadReservation{}, ErrInvalidState
	}
	result.Disposition = domain.UploadDispositionRequired
	return result, nil
}

type entryRow struct {
	ID            string
	HeadVersionID string
	Deleted       bool
	HeadSize      int64
}

func lockEntry(ctx context.Context, tx pgx.Tx, folderID, pathKey string) (entryRow, bool, error) {
	var entry entryRow
	err := tx.QueryRow(ctx, `
		SELECT e.id, COALESCE(e.head_version_id::text, ''), e.deleted,
		       COALESCE(fv.size, 0)
		FROM entries e
		LEFT JOIN file_versions fv ON fv.id = e.head_version_id
		WHERE e.folder_id = $1 AND e.path_key = $2
		FOR UPDATE OF e
	`, folderID, pathKey).Scan(&entry.ID, &entry.HeadVersionID, &entry.Deleted, &entry.HeadSize)
	if errors.Is(err, pgx.ErrNoRows) {
		return entryRow{}, false, nil
	}
	if err != nil {
		return entryRow{}, false, fmt.Errorf("lock entry: %w", err)
	}
	return entry, true, nil
}

func writerUser(ctx context.Context, tx pgx.Tx, deviceID, folderID string, lockFolder bool) (string, error) {
	query := `
		SELECT d.user_id
		FROM devices d
		JOIN folder_devices fd ON fd.device_id = d.id
		JOIN folders f ON f.id = fd.folder_id
		WHERE d.id = $1
		  AND f.id = $2
		  AND d.status = 'ACTIVE'
		  AND f.state = 'ACTIVE'
		  AND fd.can_write
	`
	if lockFolder {
		query += " FOR UPDATE OF f"
	}
	var userID string
	err := tx.QueryRow(ctx, query, deviceID, folderID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPermissionDenied
	}
	if err != nil {
		return "", fmt.Errorf("authorize writer: %w", err)
	}
	return userID, nil
}

func lockWritableFolder(ctx context.Context, tx pgx.Tx, deviceID, folderID string) (string, int64, error) {
	var userID string
	var sequence int64
	var state string
	err := tx.QueryRow(ctx, `
		SELECT d.user_id, f.current_sequence, f.state
		FROM devices d
		JOIN folder_devices fd ON fd.device_id = d.id
		JOIN folders f ON f.id = fd.folder_id
		WHERE d.id = $1
		  AND f.id = $2
		  AND d.status = 'ACTIVE'
		  AND fd.can_write
		FOR UPDATE OF f
	`, deviceID, folderID).Scan(&userID, &sequence, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, ErrPermissionDenied
	}
	if err != nil {
		return "", 0, fmt.Errorf("lock writable folder: %w", err)
	}
	if state != "ACTIVE" {
		return "", 0, ErrFolderUnavailable
	}
	return userID, sequence, nil
}

func reserveQuota(ctx context.Context, tx pgx.Tx, userID, folderID string, size int64, limits Limits) error {
	if err := lockQuotaRows(ctx, tx, userID, folderID); err != nil {
		return err
	}
	var userReserved int64
	err := tx.QueryRow(ctx, `
		SELECT reserved_bytes FROM quota_usages
		WHERE scope_type = 'USER' AND scope_id = $1
	`, userID).Scan(&userReserved)
	if err != nil {
		return fmt.Errorf("read user reserved quota: %w", err)
	}
	if userReserved > limits.MaxPendingUploadSizePerUser-size {
		return ErrQuotaExceeded
	}
	_, err = tx.Exec(ctx, `
		UPDATE quota_usages
		SET reserved_bytes = reserved_bytes + $3, updated_at = now()
		WHERE (scope_type = 'USER' AND scope_id = $1)
		   OR (scope_type = 'FOLDER' AND scope_id = $2)
	`, userID, folderID, size)
	if err != nil {
		return fmt.Errorf("reserve quota: %w", err)
	}
	return nil
}

func lockQuotaRows(ctx context.Context, tx pgx.Tx, userID, folderID string) error {
	for _, scope := range []struct {
		Type string
		ID   string
	}{{"USER", userID}, {"FOLDER", folderID}} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO quota_usages (scope_type, scope_id)
			VALUES ($1, $2)
			ON CONFLICT (scope_type, scope_id) DO NOTHING
		`, scope.Type, scope.ID); err != nil {
			return fmt.Errorf("ensure quota row: %w", err)
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT scope_type, scope_id
		FROM quota_usages
		WHERE (scope_type = 'USER' AND scope_id = $1)
		   OR (scope_type = 'FOLDER' AND scope_id = $2)
		ORDER BY scope_type, scope_id
		FOR UPDATE
	`, userID, folderID)
	if err != nil {
		return fmt.Errorf("lock quota rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var scopeType, scopeID string
		if err := rows.Scan(&scopeType, &scopeID); err != nil {
			return err
		}
	}
	return rows.Err()
}

func checkLiveQuota(
	ctx context.Context,
	tx pgx.Tx,
	userID, folderID string,
	delta int64,
	limits Limits,
) error {
	var userLive, folderLive int64
	if err := tx.QueryRow(ctx, `
		SELECT live_bytes FROM quota_usages
		WHERE scope_type = 'USER' AND scope_id = $1
	`, userID).Scan(&userLive); err != nil {
		return fmt.Errorf("read user live quota: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT live_bytes FROM quota_usages
		WHERE scope_type = 'FOLDER' AND scope_id = $1
	`, folderID).Scan(&folderLive); err != nil {
		return fmt.Errorf("read folder live quota: %w", err)
	}
	if delta > 0 &&
		(userLive > limits.MaxUserLiveSize-delta || folderLive > limits.MaxFolderLiveSize-delta) {
		return ErrQuotaExceeded
	}
	return nil
}

func updateUsageAfterCommit(
	ctx context.Context,
	tx pgx.Tx,
	userID, folderID string,
	oldLive, newLive, reserved int64,
) error {
	delta := newLive - oldLive
	historyDelta := oldLive
	_, err := tx.Exec(ctx, `
		UPDATE quota_usages
		SET live_bytes = live_bytes + $3,
		    history_bytes = history_bytes + $4,
		    reserved_bytes = GREATEST(0, reserved_bytes - $5),
		    updated_at = now()
		WHERE (scope_type = 'USER' AND scope_id = $1)
		   OR (scope_type = 'FOLDER' AND scope_id = $2)
	`, userID, folderID, delta, historyDelta, reserved)
	if err != nil {
		return fmt.Errorf("update quota usage: %w", err)
	}
	return nil
}

func ensureObjectTx(
	ctx context.Context,
	tx pgx.Tx,
	hash domain.Hash,
	size int64,
	storageKey string,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO objects (hash, size, storage_key, state)
		VALUES ($1, $2, $3, 'AVAILABLE')
		ON CONFLICT (hash) DO NOTHING
	`, hash[:], size, storageKey)
	if err != nil {
		return fmt.Errorf("ensure object: %w", err)
	}
	var existingSize int64
	var state string
	if err := tx.QueryRow(ctx, `
		SELECT size, state FROM objects WHERE hash = $1
	`, hash[:]).Scan(&existingSize, &state); err != nil {
		return fmt.Errorf("verify object: %w", err)
	}
	if existingSize != size || state != "AVAILABLE" {
		return ErrUploadNotVerified
	}
	return nil
}

func requireAvailableObject(ctx context.Context, tx pgx.Tx, hash []byte, size int64) error {
	if len(hash) != domain.SHA256Size {
		return ErrUploadNotVerified
	}
	var existingSize int64
	err := tx.QueryRow(ctx, `
		SELECT size FROM objects WHERE hash = $1 AND state = 'AVAILABLE'
	`, hash).Scan(&existingSize)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUploadNotVerified
	}
	if err != nil {
		return fmt.Errorf("read object: %w", err)
	}
	if existingSize != size {
		return ErrUploadNotVerified
	}
	return nil
}

func requireMatchingSession(
	ctx context.Context,
	tx pgx.Tx,
	deviceID, operationID, suppliedSessionID string,
) error {
	var sessionID, state string
	err := tx.QueryRow(ctx, `
		SELECT id, state
		FROM upload_sessions
		WHERE device_id = $1 AND operation_id = $2
	`, deviceID, operationID).Scan(&sessionID, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		if suppliedSessionID != "" {
			return ErrUploadNotVerified
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read operation upload session: %w", err)
	}
	if state != "VERIFIED" || suppliedSessionID != sessionID {
		return ErrUploadNotVerified
	}
	return nil
}

func decodeStoredResult(value []byte) (domain.CommitResult, error) {
	if len(value) == 0 {
		return domain.CommitResult{}, ErrInvalidState
	}
	var result domain.CommitResult
	if err := json.Unmarshal(value, &result); err != nil {
		return domain.CommitResult{}, fmt.Errorf("decode stored operation response: %w", err)
	}
	return result, nil
}

func kindName(kind domain.ChangeKind) string {
	switch kind {
	case domain.ChangeKindCreate:
		return "CREATE"
	case domain.ChangeKindModify:
		return "MODIFY"
	case domain.ChangeKindDelete:
		return "DELETE"
	case domain.ChangeKindRestore:
		return "RESTORE"
	default:
		return "UNSPECIFIED"
	}
}

func parseKind(value string) domain.ChangeKind {
	switch value {
	case "CREATE":
		return domain.ChangeKindCreate
	case "MODIFY":
		return domain.ChangeKindModify
	case "DELETE":
		return domain.ChangeKindDelete
	case "RESTORE":
		return domain.ChangeKindRestore
	default:
		return domain.ChangeKindUnspecified
	}
}
