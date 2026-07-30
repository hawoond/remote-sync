package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/jackc/pgx/v5"
)

func (p *Postgres) DeviceForCredential(ctx context.Context, digest domain.Hash) (string, error) {
	var deviceID string
	err := p.pool.QueryRow(ctx, `
		UPDATE devices d
		SET last_seen_at = $2
		FROM users u
		WHERE d.user_id = u.id
		  AND d.credential_digest = $1
		  AND d.status = 'ACTIVE'
		  AND u.status = 'ACTIVE'
		RETURNING d.id
	`, digest[:], p.now().UTC()).Scan(&deviceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve device credential: %w", err)
	}
	return deviceID, nil
}

func (p *Postgres) SetDeviceCredential(
	ctx context.Context,
	deviceID string,
	digest domain.Hash,
) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE devices
		SET credential_digest = $2
		WHERE id = $1 AND status = 'ACTIVE'
	`, deviceID, digest[:])
	if err != nil {
		return fmt.Errorf("set device credential: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) CreateEnrollment(ctx context.Context, params CreateEnrollmentParams) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		userID, err := restoreAdminUser(ctx, tx, params.CreatorDeviceID, params.FolderID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO enrollment_tokens (
				id, user_id, folder_id, secret_digest, role,
				created_by_device_id, expires_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
		`,
			params.ID,
			userID,
			params.FolderID,
			params.SecretDigest[:],
			params.Role.String(),
			params.CreatorDeviceID,
			params.ExpiresAt.UTC(),
		); err != nil {
			return fmt.Errorf("create enrollment token: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO audit_logs (
				actor_type, actor_id, action, folder_id, device_id, metadata
			) VALUES (
				'DEVICE', $1, 'DEVICE_ENROLLMENT_CREATED', $2, $1,
				jsonb_build_object(
					'enrollment_id', $3::text,
					'role', $4::text
				)
			)
		`, params.CreatorDeviceID, params.FolderID, params.ID, params.Role.String())
		if err != nil {
			return fmt.Errorf("audit enrollment creation: %w", err)
		}
		return nil
	})
}

func (p *Postgres) ConsumeEnrollment(
	ctx context.Context,
	params ConsumeEnrollmentParams,
) (domain.DeviceCredentials, error) {
	var result domain.DeviceCredentials
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var enrollmentID, userID, folderID, roleName string
		err := tx.QueryRow(ctx, `
			SELECT id, user_id, folder_id, role
			FROM enrollment_tokens
			WHERE secret_digest = $1
			  AND used_at IS NULL
			  AND revoked_at IS NULL
			  AND expires_at > $2
			FOR UPDATE
		`, params.SecretDigest[:], p.now().UTC()).Scan(
			&enrollmentID,
			&userID,
			&folderID,
			&roleName,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrEnrollmentExpired
		}
		if err != nil {
			return fmt.Errorf("lock enrollment token: %w", err)
		}
		role := parseFolderRole(roleName)
		if !role.Valid() {
			return ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO devices (
				id, user_id, name, platform, capabilities,
				credential_digest, status, last_seen_at
			) VALUES ($1, $2, $3, $4, $5::jsonb, $6, 'ACTIVE', $7)
		`,
			params.DeviceID,
			userID,
			params.DeviceName,
			params.Platform,
			params.CapabilitiesJSON,
			params.CredentialDigest[:],
			p.now().UTC(),
		); err != nil {
			return fmt.Errorf("create enrolled device: %w", err)
		}
		canWrite := role == domain.FolderRoleWriter || role == domain.FolderRoleRestoreAdmin
		if _, err := tx.Exec(ctx, `
			INSERT INTO folder_devices (
				folder_id, device_id, role, can_read, can_write
			) VALUES ($1, $2, $3, true, $4)
		`, folderID, params.DeviceID, role.String(), canWrite); err != nil {
			return fmt.Errorf("grant enrolled device access: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO device_cursors (folder_id, device_id, acked_sequence)
			SELECT id, $2, current_sequence
			FROM folders
			WHERE id = $1
		`, folderID, params.DeviceID); err != nil {
			return fmt.Errorf("initialize enrolled device cursor: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE enrollment_tokens
			SET used_at = $2, used_by_device_id = $3
			WHERE id = $1 AND used_at IS NULL
		`, enrollmentID, p.now().UTC(), params.DeviceID)
		if err != nil {
			return fmt.Errorf("consume enrollment token: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrEnrollmentExpired
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO audit_logs (
				actor_type, actor_id, action, folder_id, device_id, metadata
			) VALUES (
				'DEVICE', $1, 'DEVICE_ENROLLED', $2, $1,
				jsonb_build_object(
					'enrollment_id', $3::text,
					'role', $4::text
				)
			)
		`, params.DeviceID, folderID, enrollmentID, role.String()); err != nil {
			return fmt.Errorf("audit device enrollment: %w", err)
		}
		result = domain.DeviceCredentials{
			DeviceID: params.DeviceID,
			FolderID: folderID,
			Role:     role,
		}
		return nil
	})
	return result, err
}

func (p *Postgres) GetFolderPolicy(
	ctx context.Context,
	deviceID, folderID string,
) (domain.FolderPolicy, error) {
	var result domain.FolderPolicy
	var safetySeconds, graceSeconds int64
	err := p.pool.QueryRow(ctx, `
		SELECT fp.folder_id, fp.safety_window_seconds,
		       fp.gc_grace_period_seconds, fp.revision, fp.updated_at
		FROM folder_policies fp
		JOIN folder_devices fd ON fd.folder_id = fp.folder_id
		JOIN devices d ON d.id = fd.device_id
		WHERE fp.folder_id = $1
		  AND d.id = $2
		  AND d.status = 'ACTIVE'
		  AND fd.can_read
	`, folderID, deviceID).Scan(
		&result.FolderID,
		&safetySeconds,
		&graceSeconds,
		&result.Revision,
		&result.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.FolderPolicy{}, ErrPermissionDenied
	}
	if err != nil {
		return domain.FolderPolicy{}, fmt.Errorf("read folder policy: %w", err)
	}
	result.SafetyWindow = time.Duration(safetySeconds) * time.Second
	result.GCGracePeriod = time.Duration(graceSeconds) * time.Second
	return result, nil
}

func (p *Postgres) UpdateFolderPolicy(
	ctx context.Context,
	deviceID string,
	policy domain.FolderPolicy,
) (domain.FolderPolicy, error) {
	var result domain.FolderPolicy
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := restoreAdminUser(ctx, tx, deviceID, policy.FolderID); err != nil {
			return err
		}
		var safetySeconds, graceSeconds int64
		err := tx.QueryRow(ctx, `
			UPDATE folder_policies
			SET safety_window_seconds = $2,
			    gc_grace_period_seconds = $3,
			    revision = revision + 1,
			    updated_at = $4
			WHERE folder_id = $1
			RETURNING folder_id, safety_window_seconds,
			          gc_grace_period_seconds, revision, updated_at
		`,
			policy.FolderID,
			int64(policy.SafetyWindow/time.Second),
			int64(policy.GCGracePeriod/time.Second),
			p.now().UTC(),
		).Scan(
			&result.FolderID,
			&safetySeconds,
			&graceSeconds,
			&result.Revision,
			&result.UpdatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("update folder policy: %w", err)
		}
		result.SafetyWindow = time.Duration(safetySeconds) * time.Second
		result.GCGracePeriod = time.Duration(graceSeconds) * time.Second
		if _, err := tx.Exec(ctx, `
			UPDATE folders
			SET policy_version = $2
			WHERE id = $1
		`, policy.FolderID, result.Revision); err != nil {
			return fmt.Errorf("update folder policy version: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO audit_logs (
				actor_type, actor_id, action, folder_id, device_id, metadata
			) VALUES (
				'DEVICE', $1, 'FOLDER_POLICY_UPDATED', $2, $1,
				jsonb_build_object(
					'safety_window_seconds', $3::bigint,
					'gc_grace_period_seconds', $4::bigint,
					'revision', $5::bigint
				)
			)
		`,
			deviceID,
			policy.FolderID,
			safetySeconds,
			graceSeconds,
			result.Revision,
		)
		if err != nil {
			return fmt.Errorf("audit folder policy update: %w", err)
		}
		return nil
	})
	return result, err
}

func (p *Postgres) StartRestore(
	ctx context.Context,
	params StartRestoreParams,
) (domain.RestoreJob, error) {
	var result domain.RestoreJob
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		userID, err := readableUser(ctx, tx, params.DeviceID, params.FolderID)
		if err != nil {
			return err
		}
		var currentSequence, restoreFloor int64
		err = tx.QueryRow(ctx, `
			SELECT current_sequence, restore_floor_sequence
			FROM folders
			WHERE id = $1 AND state = 'ACTIVE'
			FOR SHARE
		`, params.FolderID).Scan(&currentSequence, &restoreFloor)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFolderUnavailable
		}
		if err != nil {
			return fmt.Errorf("read restore folder: %w", err)
		}
		snapshotSequence := params.SnapshotSequence
		if snapshotSequence == 0 {
			snapshotSequence = currentSequence
		}
		if snapshotSequence < restoreFloor || snapshotSequence > currentSequence {
			return ErrInvalidState
		}
		snapshotID := uuid.NewString()
		if _, err := tx.Exec(ctx, `
			INSERT INTO snapshots (
				id, folder_id, sequence, kind, pinned, created_by
			) VALUES ($1, $2, $3, 'PRE_RESTORE', true, $4)
		`, snapshotID, params.FolderID, snapshotSequence, userID); err != nil {
			return fmt.Errorf("create restore snapshot: %w", err)
		}
		plan, err := json.Marshal(map[string]bool{"overwrite_existing": params.Overwrite})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO restore_jobs (
				id, folder_id, snapshot_id, expected_folder_sequence,
				state, plan, created_by, target_device_id
			) VALUES ($1, $2, $3, $4, 'READY', $5, $6, $7)
		`,
			params.ID,
			params.FolderID,
			snapshotID,
			snapshotSequence,
			plan,
			userID,
			params.DeviceID,
		); err != nil {
			return fmt.Errorf("create restore job: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			WITH ranked AS (
				SELECT
					fv.entry_id,
					fv.id AS version_id,
					e.path_key,
					e.display_path,
					fv.object_hash,
					fv.size,
					fv.mtime_unix_nano,
					fv.portable_mode,
					fv.kind,
					ROW_NUMBER() OVER (
						PARTITION BY fv.entry_id
						ORDER BY fv.sequence DESC
					) AS version_rank
				FROM file_versions fv
				JOIN entries e ON e.id = fv.entry_id
				WHERE fv.folder_id = $2
				  AND fv.sequence <= $3
			),
			manifest AS (
				SELECT
					*,
					ROW_NUMBER() OVER (ORDER BY path_key) AS ordinal
				FROM ranked
				WHERE version_rank = 1
				  AND kind <> 'DELETE'
			)
			INSERT INTO restore_items (
				restore_id, ordinal, entry_id, version_id,
				path_key, display_path, object_hash, size,
				mtime_unix_nano, portable_mode
			)
			SELECT
				$1, ordinal, entry_id, version_id,
				path_key, display_path, object_hash, size,
				mtime_unix_nano, portable_mode
			FROM manifest
			ORDER BY ordinal
		`, params.ID, params.FolderID, snapshotSequence)
		if err != nil {
			return fmt.Errorf("build restore manifest: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE restore_jobs
			SET total_items = $2, updated_at = $3
			WHERE id = $1
		`, params.ID, tag.RowsAffected(), p.now().UTC()); err != nil {
			return fmt.Errorf("update restore manifest count: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO audit_logs (
				actor_type, actor_id, action, folder_id, device_id, metadata
			) VALUES (
				'DEVICE', $1, 'RESTORE_STARTED', $2, $1,
				jsonb_build_object(
					'restore_id', $3::text,
					'snapshot_sequence', $4::bigint,
					'total_items', $5::bigint
				)
			)
		`,
			params.DeviceID,
			params.FolderID,
			params.ID,
			snapshotSequence,
			tag.RowsAffected(),
		); err != nil {
			return fmt.Errorf("audit restore start: %w", err)
		}
		result, err = readRestoreJob(ctx, tx, params.ID)
		return err
	})
	return result, err
}

func (p *Postgres) ListRestoreItems(
	ctx context.Context,
	deviceID, restoreID string,
	after int64,
	limit int,
) (domain.RestoreJob, []domain.RestoreItem, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var job domain.RestoreJob
	var items []domain.RestoreItem
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE restore_jobs r
			SET state = CASE WHEN state = 'READY' THEN 'RUNNING' ELSE state END,
			    updated_at = $3
			FROM devices d
			WHERE r.id = $1
			  AND r.target_device_id = $2
			  AND d.id = $2
			  AND d.status = 'ACTIVE'
			  AND r.state IN ('READY', 'RUNNING')
		`, restoreID, deviceID, p.now().UTC())
		if err != nil {
			return fmt.Errorf("claim restore job: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrPermissionDenied
		}
		job, err = readRestoreJob(ctx, tx, restoreID)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT ordinal, entry_id, COALESCE(version_id::text, ''),
			       path_key, display_path, object_hash, size,
			       mtime_unix_nano, portable_mode, state, error_message
			FROM restore_items
			WHERE restore_id = $1 AND ordinal > $2
			ORDER BY ordinal
			LIMIT $3
		`, restoreID, after, limit)
		if err != nil {
			return fmt.Errorf("list restore items: %w", err)
		}
		defer rows.Close()
		items = make([]domain.RestoreItem, 0, limit)
		for rows.Next() {
			var item domain.RestoreItem
			var hashBytes []byte
			var state string
			if err := rows.Scan(
				&item.Ordinal,
				&item.EntryID,
				&item.VersionID,
				&item.PathKey,
				&item.DisplayPath,
				&hashBytes,
				&item.Size,
				&item.MTimeUnixNano,
				&item.PortableMode,
				&state,
				&item.ErrorMessage,
			); err != nil {
				return fmt.Errorf("scan restore item: %w", err)
			}
			item.ObjectHash, err = domain.HashFromBytes(hashBytes)
			if err != nil {
				return err
			}
			item.State = parseRestoreItemState(state)
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate restore items: %w", err)
		}
		return nil
	})
	return job, items, err
}

func (p *Postgres) ReportRestoreItem(
	ctx context.Context,
	deviceID, restoreID string,
	ordinal int64,
	state domain.RestoreItemState,
	errorMessage string,
) (domain.RestoreJob, error) {
	var result domain.RestoreJob
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		stateName := restoreItemStateName(state)
		tag, err := tx.Exec(ctx, `
			UPDATE restore_items ri
			SET state = $4, error_message = $5, updated_at = $6
			FROM restore_jobs r
			WHERE ri.restore_id = r.id
			  AND ri.restore_id = $1
			  AND ri.ordinal = $2
			  AND r.target_device_id = $3
			  AND r.state IN ('READY', 'RUNNING')
			  AND (ri.state = 'PENDING' OR ri.state = $4)
		`, restoreID, ordinal, deviceID, stateName, errorMessage, p.now().UTC())
		if err != nil {
			return fmt.Errorf("report restore item: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `
			UPDATE restore_jobs
			SET state = 'RUNNING', updated_at = $2
			WHERE id = $1 AND state = 'READY'
		`, restoreID, p.now().UTC()); err != nil {
			return fmt.Errorf("mark restore running: %w", err)
		}
		result, err = readRestoreJob(ctx, tx, restoreID)
		return err
	})
	return result, err
}

func (p *Postgres) FinishRestore(
	ctx context.Context,
	deviceID, restoreID string,
	success bool,
	errorMessage string,
) (domain.RestoreJob, error) {
	var result domain.RestoreJob
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var lockedID string
		err := tx.QueryRow(ctx, `
			SELECT id
			FROM restore_jobs
			WHERE id = $1
			  AND target_device_id = $2
			  AND state IN ('READY', 'RUNNING')
			FOR UPDATE
		`, restoreID, deviceID).Scan(&lockedID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPermissionDenied
		}
		if err != nil {
			return fmt.Errorf("lock restore completion: %w", err)
		}
		var pending, failed int64
		err = tx.QueryRow(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE state = 'PENDING'),
				COUNT(*) FILTER (WHERE state = 'FAILED')
			FROM restore_items
			WHERE restore_id = $1
		`, lockedID).Scan(&pending, &failed)
		if err != nil {
			return fmt.Errorf("count restore completion: %w", err)
		}
		if success && (pending > 0 || failed > 0) {
			return ErrRestoreIncomplete
		}
		state := "FAILED"
		action := "RESTORE_FAILED"
		if success {
			state = "COMPLETED"
			action = "RESTORE_COMPLETED"
			errorMessage = ""
		}
		var snapshotID, folderID string
		var snapshotSequence int64
		err = tx.QueryRow(ctx, `
			UPDATE restore_jobs
			SET state = $3, error_message = $4,
			    completed_at = $5, updated_at = $5
			WHERE id = $1 AND target_device_id = $2
			RETURNING snapshot_id, folder_id, expected_folder_sequence
		`, restoreID, deviceID, state, errorMessage, p.now().UTC()).Scan(
			&snapshotID,
			&folderID,
			&snapshotSequence,
		)
		if err != nil {
			return fmt.Errorf("finish restore job: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE snapshots SET pinned = false WHERE id = $1
		`, snapshotID); err != nil {
			return fmt.Errorf("unpin restore snapshot: %w", err)
		}
		if success {
			if _, err := tx.Exec(ctx, `
				INSERT INTO device_cursors (
					folder_id, device_id, acked_sequence
				) VALUES ($1, $2, $3)
				ON CONFLICT (folder_id, device_id) DO UPDATE
				SET acked_sequence = GREATEST(
						device_cursors.acked_sequence,
						EXCLUDED.acked_sequence
					),
				    updated_at = now()
			`, folderID, deviceID, snapshotSequence); err != nil {
				return fmt.Errorf("acknowledge restored snapshot: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO audit_logs (
				actor_type, actor_id, action, folder_id, device_id, metadata
			) VALUES (
				'DEVICE', $1, $2, $3, $1,
				jsonb_build_object(
					'restore_id', $4::text,
					'error', $5::text
				)
			)
		`, deviceID, action, folderID, restoreID, errorMessage); err != nil {
			return fmt.Errorf("audit restore completion: %w", err)
		}
		result, err = readRestoreJob(ctx, tx, restoreID)
		return err
	})
	return result, err
}

func (p *Postgres) AuthorizeObjectRead(
	ctx context.Context,
	deviceID, folderID string,
	hash domain.Hash,
) error {
	var allowed bool
	err := p.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM folder_devices fd
			JOIN devices d ON d.id = fd.device_id
			JOIN objects o ON o.hash = $3 AND o.state = 'AVAILABLE'
			WHERE fd.folder_id = $1
			  AND fd.device_id = $2
			  AND fd.can_read
			  AND d.status = 'ACTIVE'
			  AND (
				EXISTS (
					SELECT 1
					FROM file_versions fv
					WHERE fv.folder_id = $1 AND fv.object_hash = o.hash
				)
				OR EXISTS (
					SELECT 1
					FROM restore_items ri
					JOIN restore_jobs r ON r.id = ri.restore_id
					WHERE r.folder_id = $1
					  AND r.target_device_id = $2
					  AND ri.object_hash = o.hash
					  AND r.state IN ('READY', 'RUNNING')
				)
			  )
		)
	`, folderID, deviceID, hash[:]).Scan(&allowed)
	if err != nil {
		return fmt.Errorf("authorize object read: %w", err)
	}
	if !allowed {
		return ErrPermissionDenied
	}
	return nil
}

func restoreAdminUser(
	ctx context.Context,
	tx pgx.Tx,
	deviceID, folderID string,
) (string, error) {
	var userID string
	err := tx.QueryRow(ctx, `
		SELECT d.user_id
		FROM devices d
		JOIN users u ON u.id = d.user_id
		JOIN folder_devices fd ON fd.device_id = d.id
		JOIN folders f ON f.id = fd.folder_id
		WHERE d.id = $1
		  AND f.id = $2
		  AND d.status = 'ACTIVE'
		  AND u.status = 'ACTIVE'
		  AND f.state = 'ACTIVE'
		  AND fd.role = 'RESTORE_ADMIN'
	`, deviceID, folderID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPermissionDenied
	}
	if err != nil {
		return "", fmt.Errorf("authorize restore administrator: %w", err)
	}
	return userID, nil
}

func readableUser(
	ctx context.Context,
	tx pgx.Tx,
	deviceID, folderID string,
) (string, error) {
	var userID string
	err := tx.QueryRow(ctx, `
		SELECT d.user_id
		FROM devices d
		JOIN users u ON u.id = d.user_id
		JOIN folder_devices fd ON fd.device_id = d.id
		JOIN folders f ON f.id = fd.folder_id
		WHERE d.id = $1
		  AND f.id = $2
		  AND d.status = 'ACTIVE'
		  AND u.status = 'ACTIVE'
		  AND f.state = 'ACTIVE'
		  AND fd.can_read
	`, deviceID, folderID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPermissionDenied
	}
	if err != nil {
		return "", fmt.Errorf("authorize folder reader: %w", err)
	}
	return userID, nil
}

func readRestoreJob(ctx context.Context, tx pgx.Tx, restoreID string) (domain.RestoreJob, error) {
	var job domain.RestoreJob
	var state string
	var completedAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT
			r.id,
			r.folder_id,
			COALESCE(r.target_device_id::text, ''),
			r.expected_folder_sequence,
			r.state,
			COALESCE((r.plan->>'overwrite_existing')::boolean, false),
			r.total_items,
			COUNT(*) FILTER (WHERE ri.state = 'APPLIED'),
			COUNT(*) FILTER (WHERE ri.state = 'SKIPPED'),
			COUNT(*) FILTER (WHERE ri.state = 'FAILED'),
			r.error_message,
			r.created_at,
			r.completed_at
		FROM restore_jobs r
		LEFT JOIN restore_items ri ON ri.restore_id = r.id
		WHERE r.id = $1
		GROUP BY r.id
	`, restoreID).Scan(
		&job.ID,
		&job.FolderID,
		&job.TargetDeviceID,
		&job.SnapshotSequence,
		&state,
		&job.Overwrite,
		&job.TotalItems,
		&job.AppliedItems,
		&job.SkippedItems,
		&job.FailedItems,
		&job.ErrorMessage,
		&job.CreatedAt,
		&completedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RestoreJob{}, ErrNotFound
	}
	if err != nil {
		return domain.RestoreJob{}, fmt.Errorf("read restore job: %w", err)
	}
	job.State = parseRestoreState(state)
	if completedAt != nil {
		job.CompletedAt = *completedAt
	}
	return job, nil
}

func parseFolderRole(value string) domain.FolderRole {
	switch value {
	case "READER":
		return domain.FolderRoleReader
	case "WRITER":
		return domain.FolderRoleWriter
	case "RESTORE_ADMIN":
		return domain.FolderRoleRestoreAdmin
	default:
		return domain.FolderRoleUnspecified
	}
}

func parseRestoreState(value string) domain.RestoreState {
	switch value {
	case "READY":
		return domain.RestoreStateReady
	case "RUNNING":
		return domain.RestoreStateRunning
	case "COMPLETED":
		return domain.RestoreStateCompleted
	case "FAILED":
		return domain.RestoreStateFailed
	default:
		return domain.RestoreStateUnspecified
	}
}

func restoreItemStateName(value domain.RestoreItemState) string {
	switch value {
	case domain.RestoreItemStateApplied:
		return "APPLIED"
	case domain.RestoreItemStateSkipped:
		return "SKIPPED"
	case domain.RestoreItemStateFailed:
		return "FAILED"
	default:
		return ""
	}
}

func parseRestoreItemState(value string) domain.RestoreItemState {
	switch value {
	case "PENDING":
		return domain.RestoreItemStatePending
	case "APPLIED":
		return domain.RestoreItemStateApplied
	case "SKIPPED":
		return domain.RestoreItemStateSkipped
	case "FAILED":
		return domain.RestoreItemStateFailed
	default:
		return domain.RestoreItemStateUnspecified
	}
}
