package metadata

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/jackc/pgx/v5"
)

const orphanObjectGracePeriod = 24 * time.Hour

func (p *Postgres) ExpireUploadSessions(
	ctx context.Context,
	before time.Time,
	limit int,
) ([]string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	type expiredSession struct {
		ID          string
		DeviceID    string
		OperationID string
		UserID      string
		FolderID    string
		Reserved    int64
	}
	var expired []expiredSession
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT
				us.id,
				us.device_id,
				us.operation_id,
				d.user_id,
				us.folder_id,
				us.reserved_bytes
			FROM upload_sessions us
			JOIN devices d ON d.id = us.device_id
			WHERE us.state IN ('ACTIVE', 'VERIFIED', 'EXPIRED')
			  AND us.expires_at <= $1
			ORDER BY us.expires_at
			FOR UPDATE OF us SKIP LOCKED
			LIMIT $2
		`, before.UTC(), limit)
		if err != nil {
			return fmt.Errorf("select expired uploads: %w", err)
		}
		for rows.Next() {
			var item expiredSession
			if err := rows.Scan(
				&item.ID,
				&item.DeviceID,
				&item.OperationID,
				&item.UserID,
				&item.FolderID,
				&item.Reserved,
			); err != nil {
				rows.Close()
				return fmt.Errorf("scan expired upload: %w", err)
			}
			expired = append(expired, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate expired uploads: %w", err)
		}
		rows.Close()

		for _, item := range expired {
			if _, err := tx.Exec(ctx, `
				UPDATE upload_sessions
				SET state = 'EXPIRED', reserved_bytes = 0, updated_at = $2
				WHERE id = $1
			`, item.ID, before.UTC()); err != nil {
				return fmt.Errorf("expire upload session: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE operations
				SET state = 'FAILED', completed_at = $3
				WHERE device_id = $1
				  AND operation_id = $2
				  AND state <> 'COMPLETED'
			`, item.DeviceID, item.OperationID, before.UTC()); err != nil {
				return fmt.Errorf("fail expired operation: %w", err)
			}
			if item.Reserved > 0 {
				if _, err := tx.Exec(ctx, `
					UPDATE quota_usages
					SET reserved_bytes = GREATEST(0, reserved_bytes - $3),
					    updated_at = $4
					WHERE (scope_type = 'USER' AND scope_id = $1)
					   OR (scope_type = 'FOLDER' AND scope_id = $2)
				`, item.UserID, item.FolderID, item.Reserved, before.UTC()); err != nil {
					return fmt.Errorf("release expired upload quota: %w", err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sessionIDs := make([]string, 0, len(expired))
	for _, item := range expired {
		sessionIDs = append(sessionIDs, item.ID)
	}
	return sessionIDs, nil
}

func (p *Postgres) CompleteUploadCleanup(
	ctx context.Context,
	sessionID string,
) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE upload_sessions
		SET state = 'ABORTED', updated_at = now()
		WHERE id = $1 AND state = 'EXPIRED'
	`, sessionID)
	if err != nil {
		return fmt.Errorf("complete upload cleanup: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return nil
}

func (p *Postgres) PruneVersions(
	ctx context.Context,
	now time.Time,
	limit int,
) (PruneResult, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	type candidate struct {
		ID           uuid.UUID
		FolderID     string
		UserID       string
		Hash         domain.Hash
		Size         int64
		GraceSeconds int64
	}
	var candidates []candidate
	result := PruneResult{}
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH cursor_floor AS (
				SELECT
					f.id AS folder_id,
					MIN(COALESCE(dc.acked_sequence, 0)) AS acked_sequence
				FROM folders f
				JOIN folder_devices fd
				  ON fd.folder_id = f.id AND fd.can_read
				JOIN devices d
				  ON d.id = fd.device_id AND d.status = 'ACTIVE'
				LEFT JOIN device_cursors dc
				  ON dc.folder_id = f.id AND dc.device_id = d.id
				GROUP BY f.id
			)
			SELECT
				fv.id,
				fv.folder_id,
				f.owner_user_id,
				fv.object_hash,
				fv.size,
				fp.gc_grace_period_seconds
			FROM file_versions fv
			JOIN folders f ON f.id = fv.folder_id
			JOIN folder_policies fp ON fp.folder_id = fv.folder_id
			JOIN cursor_floor cf ON cf.folder_id = fv.folder_id
			WHERE fv.sequence <= cf.acked_sequence
			  AND fv.superseded_at IS NOT NULL
			  AND fv.superseded_at <= $1::timestamptz
			      - make_interval(secs => fp.safety_window_seconds)
			  AND NOT EXISTS (
				SELECT 1 FROM entries e WHERE e.head_version_id = fv.id
			  )
			  AND NOT EXISTS (
				SELECT 1
				FROM restore_items ri
				JOIN restore_jobs r ON r.id = ri.restore_id
				WHERE ri.version_id = fv.id
				  AND r.state IN ('READY', 'RUNNING')
			  )
			  AND NOT EXISTS (
				SELECT 1
				FROM snapshots s
				WHERE s.folder_id = fv.folder_id
				  AND s.pinned
				  AND fv.id = (
					SELECT preserved.id
					FROM file_versions preserved
					WHERE preserved.entry_id = fv.entry_id
					  AND preserved.sequence <= s.sequence
					ORDER BY preserved.sequence DESC
					LIMIT 1
				  )
			  )
			ORDER BY fv.superseded_at, fv.id
			FOR UPDATE OF fv SKIP LOCKED
			LIMIT $2
		`, now.UTC(), limit)
		if err != nil {
			return fmt.Errorf("select prunable versions: %w", err)
		}
		for rows.Next() {
			var item candidate
			var hashBytes []byte
			if err := rows.Scan(
				&item.ID,
				&item.FolderID,
				&item.UserID,
				&hashBytes,
				&item.Size,
				&item.GraceSeconds,
			); err != nil {
				rows.Close()
				return fmt.Errorf("scan prunable version: %w", err)
			}
			if len(hashBytes) > 0 {
				item.Hash, err = domain.HashFromBytes(hashBytes)
				if err != nil {
					rows.Close()
					return err
				}
			}
			candidates = append(candidates, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate prunable versions: %w", err)
		}
		rows.Close()

		ids := make([]uuid.UUID, 0, len(candidates))
		for _, item := range candidates {
			ids = append(ids, item.ID)
		}
		if len(ids) > 0 {
			if _, err := tx.Exec(ctx, `
				DELETE FROM changes WHERE version_id = ANY($1::uuid[])
			`, ids); err != nil {
				return fmt.Errorf("delete pruned changes: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE operations
				SET version_id = NULL
				WHERE version_id = ANY($1::uuid[])
			`, ids); err != nil {
				return fmt.Errorf("detach pruned operations: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE file_versions
				SET base_version_id = NULL
				WHERE base_version_id = ANY($1::uuid[])
			`, ids); err != nil {
				return fmt.Errorf("detach pruned version bases: %w", err)
			}
			tag, err := tx.Exec(ctx, `
				DELETE FROM file_versions WHERE id = ANY($1::uuid[])
			`, ids)
			if err != nil {
				return fmt.Errorf("delete pruned versions: %w", err)
			}
			result.Versions = int(tag.RowsAffected())
			folders := make(map[string]struct{})
			for _, item := range candidates {
				folders[item.FolderID] = struct{}{}
			}
			for folderID := range folders {
				if _, err := tx.Exec(ctx, `
					UPDATE folders
					SET restore_floor_sequence = current_sequence
					WHERE id = $1
				`, folderID); err != nil {
					return fmt.Errorf("advance restore floor: %w", err)
				}
			}
		}

		type quotaKey struct {
			UserID   string
			FolderID string
		}
		quotaDeltas := make(map[quotaKey]int64)
		type objectMark struct {
			Hash  domain.Hash
			Grace time.Duration
		}
		objectMarks := make(map[string]objectMark)
		for _, item := range candidates {
			key := quotaKey{UserID: item.UserID, FolderID: item.FolderID}
			quotaDeltas[key] += item.Size
			if !item.Hash.IsZero() {
				hashKey := item.Hash.String()
				grace := time.Duration(item.GraceSeconds) * time.Second
				if existing, ok := objectMarks[hashKey]; !ok || grace > existing.Grace {
					objectMarks[hashKey] = objectMark{Hash: item.Hash, Grace: grace}
				}
			}
		}
		for key, size := range quotaDeltas {
			if _, err := tx.Exec(ctx, `
				UPDATE quota_usages
				SET history_bytes = GREATEST(0, history_bytes - $3),
				    updated_at = $4
				WHERE (scope_type = 'USER' AND scope_id = $1)
				   OR (scope_type = 'FOLDER' AND scope_id = $2)
			`, key.UserID, key.FolderID, size, now.UTC()); err != nil {
				return fmt.Errorf("release pruned history quota: %w", err)
			}
		}
		for _, mark := range objectMarks {
			tag, err := tx.Exec(ctx, `
				UPDATE objects o
				SET state = 'PENDING_DELETE',
				    pending_delete_at = $2
				WHERE o.hash = $1
				  AND o.state = 'AVAILABLE'
				  AND NOT EXISTS (
					SELECT 1 FROM file_versions fv WHERE fv.object_hash = o.hash
				  )
				  AND NOT EXISTS (
					SELECT 1
					FROM operations op
					WHERE op.object_hash = o.hash
					  AND op.state IN ('RESERVED', 'UPLOADING', 'READY', 'COMMITTING')
				  )
			`, mark.Hash[:], now.UTC().Add(mark.Grace))
			if err != nil {
				return fmt.Errorf("mark unreferenced object: %w", err)
			}
			result.Objects += int(tag.RowsAffected())
		}

		orphanTag, err := tx.Exec(ctx, `
			WITH orphaned AS (
				SELECT o.hash
				FROM objects o
				WHERE o.state = 'AVAILABLE'
				  AND o.created_at <= $1
				  AND NOT EXISTS (
					SELECT 1 FROM file_versions fv WHERE fv.object_hash = o.hash
				  )
				  AND NOT EXISTS (
					SELECT 1
					FROM operations op
					WHERE op.object_hash = o.hash
					  AND op.state IN ('RESERVED', 'UPLOADING', 'READY', 'COMMITTING')
				  )
				ORDER BY o.created_at
				LIMIT $2
				FOR UPDATE OF o SKIP LOCKED
			)
			UPDATE objects o
			SET state = 'PENDING_DELETE',
			    pending_delete_at = $3
			FROM orphaned
			WHERE o.hash = orphaned.hash
			  AND o.state = 'AVAILABLE'
		`,
			now.UTC().Add(-orphanObjectGracePeriod),
			limit,
			now.UTC().Add(orphanObjectGracePeriod),
		)
		if err != nil {
			return fmt.Errorf("mark orphan objects: %w", err)
		}
		result.Objects += int(orphanTag.RowsAffected())
		return nil
	})
	return result, err
}

func (p *Postgres) PendingGarbage(
	ctx context.Context,
	now time.Time,
	limit int,
) ([]domain.GarbageObject, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := p.pool.Query(ctx, `
		SELECT o.hash, o.storage_key
		FROM objects o
		WHERE o.state = 'PENDING_DELETE'
		  AND o.pending_delete_at <= $1
		  AND NOT EXISTS (
			SELECT 1 FROM file_versions fv WHERE fv.object_hash = o.hash
		  )
		  AND NOT EXISTS (
			SELECT 1
			FROM operations op
			WHERE op.object_hash = o.hash
			  AND op.state IN ('RESERVED', 'UPLOADING', 'READY', 'COMMITTING')
		  )
		ORDER BY o.pending_delete_at
		LIMIT $2
	`, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list pending garbage: %w", err)
	}
	defer rows.Close()
	result := make([]domain.GarbageObject, 0, limit)
	for rows.Next() {
		var item domain.GarbageObject
		var hashBytes []byte
		if err := rows.Scan(&hashBytes, &item.StorageKey); err != nil {
			return nil, fmt.Errorf("scan pending garbage: %w", err)
		}
		item.Hash, err = domain.HashFromBytes(hashBytes)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending garbage: %w", err)
	}
	return result, nil
}

func (p *Postgres) DeleteGarbageRecord(
	ctx context.Context,
	hash domain.Hash,
) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
		DELETE FROM objects o
		WHERE o.hash = $1
		  AND o.state = 'PENDING_DELETE'
		  AND NOT EXISTS (
			SELECT 1 FROM file_versions fv WHERE fv.object_hash = o.hash
		  )
		  AND NOT EXISTS (
			SELECT 1
			FROM operations op
			WHERE op.object_hash = o.hash
			  AND op.state IN ('RESERVED', 'UPLOADING', 'READY', 'COMMITTING')
		  )
	`, hash[:])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("delete garbage metadata: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
