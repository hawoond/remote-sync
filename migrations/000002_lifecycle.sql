ALTER TABLE devices
    ADD CONSTRAINT devices_credential_digest_length
    CHECK (credential_digest IS NULL OR octet_length(credential_digest) = 32);

CREATE UNIQUE INDEX devices_credential_digest_idx
    ON devices (credential_digest)
    WHERE credential_digest IS NOT NULL;

ALTER TABLE folders
    ADD COLUMN restore_floor_sequence bigint NOT NULL DEFAULT 0
        CHECK (restore_floor_sequence >= 0);

ALTER TABLE file_versions
    ADD COLUMN superseded_at timestamptz;

UPDATE file_versions fv
SET superseded_at = now()
WHERE NOT EXISTS (
    SELECT 1
    FROM entries e
    WHERE e.head_version_id = fv.id
);

CREATE INDEX file_versions_superseded_idx
    ON file_versions (folder_id, superseded_at, sequence)
    WHERE superseded_at IS NOT NULL;

CREATE TABLE folder_policies (
    folder_id uuid PRIMARY KEY REFERENCES folders(id) ON DELETE CASCADE,
    safety_window_seconds bigint NOT NULL DEFAULT 2592000
        CHECK (safety_window_seconds BETWEEN 60 AND 31536000),
    gc_grace_period_seconds bigint NOT NULL DEFAULT 86400
        CHECK (gc_grace_period_seconds BETWEEN 60 AND 2592000),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO folder_policies (folder_id)
SELECT id FROM folders
ON CONFLICT (folder_id) DO NOTHING;

CREATE TABLE enrollment_tokens (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id),
    folder_id uuid NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    secret_digest bytea NOT NULL UNIQUE CHECK (octet_length(secret_digest) = 32),
    role text NOT NULL CHECK (role IN ('READER', 'WRITER', 'RESTORE_ADMIN')),
    created_by_device_id uuid NOT NULL REFERENCES devices(id),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    used_by_device_id uuid REFERENCES devices(id),
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX enrollment_tokens_active_idx
    ON enrollment_tokens (folder_id, expires_at)
    WHERE used_at IS NULL AND revoked_at IS NULL;

ALTER TABLE restore_jobs
    ADD COLUMN target_device_id uuid REFERENCES devices(id),
    ADD COLUMN total_items bigint NOT NULL DEFAULT 0 CHECK (total_items >= 0),
    ADD COLUMN error_message text NOT NULL DEFAULT '',
    ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();

CREATE TABLE restore_items (
    restore_id uuid NOT NULL REFERENCES restore_jobs(id) ON DELETE CASCADE,
    ordinal bigint NOT NULL CHECK (ordinal > 0),
    entry_id uuid NOT NULL REFERENCES entries(id),
    version_id uuid REFERENCES file_versions(id) ON DELETE SET NULL,
    path_key text NOT NULL,
    display_path text NOT NULL,
    object_hash bytea NOT NULL CHECK (octet_length(object_hash) = 32),
    size bigint NOT NULL CHECK (size >= 0),
    mtime_unix_nano bigint NOT NULL,
    portable_mode integer NOT NULL CHECK (portable_mode >= 0),
    state text NOT NULL DEFAULT 'PENDING'
        CHECK (state IN ('PENDING', 'APPLIED', 'SKIPPED', 'FAILED')),
    error_message text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (restore_id, ordinal)
);

CREATE INDEX restore_items_state_idx
    ON restore_items (restore_id, state, ordinal);

CREATE INDEX snapshots_pinned_idx
    ON snapshots (folder_id, sequence)
    WHERE pinned;

ALTER TABLE file_versions
    DROP CONSTRAINT file_versions_base_version_id_fkey,
    ADD CONSTRAINT file_versions_base_version_id_fkey
        FOREIGN KEY (base_version_id) REFERENCES file_versions(id) ON DELETE SET NULL;

ALTER TABLE operations
    DROP CONSTRAINT operations_version_fk,
    ADD CONSTRAINT operations_version_fk
        FOREIGN KEY (version_id) REFERENCES file_versions(id) ON DELETE SET NULL;
