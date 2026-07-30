CREATE TABLE users (
    id uuid PRIMARY KEY,
    status text NOT NULL CHECK (status IN ('ACTIVE', 'SUSPENDED')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE devices (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id),
    name text NOT NULL,
    platform text NOT NULL,
    capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
    credential_digest bytea,
    status text NOT NULL CHECK (status IN ('ACTIVE', 'REVOKED')),
    last_seen_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX devices_user_status_idx ON devices (user_id, status);

CREATE TABLE folders (
    id uuid PRIMARY KEY,
    owner_user_id uuid NOT NULL REFERENCES users(id),
    mode text NOT NULL CHECK (mode IN ('BACKUP', 'MIRROR', 'BIDIRECTIONAL')),
    state text NOT NULL CHECK (state IN ('ACTIVE', 'RESTORING', 'SUSPENDED')),
    current_sequence bigint NOT NULL DEFAULT 0 CHECK (current_sequence >= 0),
    active_generation_id uuid,
    policy_version bigint NOT NULL DEFAULT 1 CHECK (policy_version > 0),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX folders_owner_state_idx ON folders (owner_user_id, state);

CREATE TABLE folder_devices (
    folder_id uuid NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    device_id uuid NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('READER', 'WRITER', 'RESTORE_ADMIN')),
    can_read boolean NOT NULL DEFAULT true,
    can_write boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (folder_id, device_id)
);

CREATE TABLE objects (
    hash bytea PRIMARY KEY CHECK (octet_length(hash) = 32),
    size bigint NOT NULL CHECK (size >= 0),
    storage_key text NOT NULL,
    state text NOT NULL CHECK (state IN ('AVAILABLE', 'PENDING_DELETE', 'CORRUPT')),
    created_at timestamptz NOT NULL DEFAULT now(),
    pending_delete_at timestamptz
);

CREATE INDEX objects_pending_delete_idx
    ON objects (pending_delete_at)
    WHERE pending_delete_at IS NOT NULL;

CREATE TABLE entries (
    id uuid PRIMARY KEY,
    folder_id uuid NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    path_key text NOT NULL,
    display_path text NOT NULL,
    head_version_id uuid,
    deleted boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (folder_id, path_key)
);

CREATE TABLE operations (
    device_id uuid NOT NULL REFERENCES devices(id),
    operation_id uuid NOT NULL,
    folder_id uuid NOT NULL REFERENCES folders(id),
    path_key text NOT NULL,
    display_path text NOT NULL,
    request_digest bytea NOT NULL CHECK (octet_length(request_digest) = 32),
    base_version_id uuid,
    object_hash bytea CHECK (object_hash IS NULL OR octet_length(object_hash) = 32),
    declared_size bigint NOT NULL DEFAULT 0 CHECK (declared_size >= 0),
    mtime_unix_nano bigint NOT NULL DEFAULT 0,
    portable_mode integer NOT NULL DEFAULT 0 CHECK (portable_mode >= 0),
    state text NOT NULL CHECK (
        state IN ('RESERVED', 'UPLOADING', 'READY', 'COMMITTING', 'COMPLETED', 'QUARANTINED', 'FAILED')
    ),
    version_id uuid,
    response jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    PRIMARY KEY (device_id, operation_id)
);

CREATE INDEX operations_folder_created_idx ON operations (folder_id, created_at DESC);

CREATE TABLE upload_sessions (
    id uuid PRIMARY KEY,
    device_id uuid NOT NULL,
    operation_id uuid NOT NULL,
    folder_id uuid NOT NULL REFERENCES folders(id),
    hash bytea NOT NULL CHECK (octet_length(hash) = 32),
    declared_size bigint NOT NULL CHECK (declared_size >= 0),
    received_size bigint NOT NULL DEFAULT 0 CHECK (received_size >= 0),
    temp_key text NOT NULL,
    state text NOT NULL CHECK (state IN ('ACTIVE', 'VERIFIED', 'COMMITTED', 'EXPIRED', 'ABORTED')),
    reserved_bytes bigint NOT NULL CHECK (reserved_bytes >= 0),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (device_id, operation_id),
    FOREIGN KEY (device_id, operation_id) REFERENCES operations(device_id, operation_id)
);

CREATE INDEX upload_sessions_active_expiry_idx
    ON upload_sessions (expires_at)
    WHERE state IN ('ACTIVE', 'VERIFIED');

CREATE TABLE file_versions (
    id uuid PRIMARY KEY,
    folder_id uuid NOT NULL REFERENCES folders(id),
    entry_id uuid NOT NULL REFERENCES entries(id),
    base_version_id uuid REFERENCES file_versions(id),
    object_hash bytea REFERENCES objects(hash),
    size bigint NOT NULL CHECK (size >= 0),
    kind text NOT NULL CHECK (kind IN ('CREATE', 'MODIFY', 'DELETE', 'RESTORE', 'CONFLICT')),
    origin_device_id uuid REFERENCES devices(id),
    operation_id uuid NOT NULL,
    sequence bigint NOT NULL CHECK (sequence > 0),
    mtime_unix_nano bigint NOT NULL DEFAULT 0,
    portable_mode integer NOT NULL DEFAULT 0 CHECK (portable_mode >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (folder_id, sequence),
    CHECK (
        (kind = 'DELETE' AND object_hash IS NULL AND size = 0)
        OR
        (kind <> 'DELETE' AND object_hash IS NOT NULL)
    )
);

CREATE INDEX file_versions_entry_created_idx ON file_versions (entry_id, created_at DESC);

ALTER TABLE entries
    ADD CONSTRAINT entries_head_version_fk
    FOREIGN KEY (head_version_id) REFERENCES file_versions(id);

ALTER TABLE operations
    ADD CONSTRAINT operations_version_fk
    FOREIGN KEY (version_id) REFERENCES file_versions(id);

CREATE TABLE changes (
    folder_id uuid NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    sequence bigint NOT NULL CHECK (sequence > 0),
    entry_id uuid NOT NULL REFERENCES entries(id),
    version_id uuid NOT NULL REFERENCES file_versions(id),
    kind text NOT NULL CHECK (kind IN ('CREATE', 'MODIFY', 'DELETE', 'RESTORE', 'CONFLICT')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (folder_id, sequence)
);

CREATE TABLE device_cursors (
    folder_id uuid NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    device_id uuid NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    acked_sequence bigint NOT NULL DEFAULT 0 CHECK (acked_sequence >= 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (folder_id, device_id)
);

CREATE TABLE quota_usages (
    scope_type text NOT NULL CHECK (scope_type IN ('USER', 'FOLDER')),
    scope_id uuid NOT NULL,
    live_bytes bigint NOT NULL DEFAULT 0 CHECK (live_bytes >= 0),
    history_bytes bigint NOT NULL DEFAULT 0 CHECK (history_bytes >= 0),
    physical_bytes bigint NOT NULL DEFAULT 0 CHECK (physical_bytes >= 0),
    reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope_type, scope_id)
);

CREATE TABLE quarantines (
    id uuid PRIMARY KEY,
    folder_id uuid NOT NULL REFERENCES folders(id),
    device_id uuid NOT NULL REFERENCES devices(id),
    operation_id uuid NOT NULL,
    reason text NOT NULL,
    evidence jsonb NOT NULL DEFAULT '{}'::jsonb,
    state text NOT NULL CHECK (state IN ('PENDING', 'APPROVED', 'REJECTED', 'EXPIRED')),
    reviewed_by uuid REFERENCES users(id),
    reviewed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (device_id, operation_id)
);

CREATE INDEX quarantines_folder_state_idx
    ON quarantines (folder_id, state, created_at);

CREATE TABLE change_window_buckets (
    folder_id uuid NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    device_id uuid NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    bucket_start timestamptz NOT NULL,
    kind text NOT NULL CHECK (kind IN ('CREATE', 'MODIFY', 'DELETE', 'RESTORE', 'CONFLICT')),
    change_count bigint NOT NULL DEFAULT 0 CHECK (change_count >= 0),
    changed_bytes bigint NOT NULL DEFAULT 0 CHECK (changed_bytes >= 0),
    PRIMARY KEY (folder_id, device_id, bucket_start, kind)
);

CREATE TABLE snapshots (
    id uuid PRIMARY KEY,
    folder_id uuid NOT NULL REFERENCES folders(id),
    sequence bigint NOT NULL CHECK (sequence >= 0),
    kind text NOT NULL CHECK (kind IN ('MANUAL', 'SCHEDULED', 'PRE_RESTORE')),
    pinned boolean NOT NULL DEFAULT false,
    created_by uuid REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX snapshots_folder_created_idx ON snapshots (folder_id, created_at DESC);

CREATE TABLE restore_jobs (
    id uuid PRIMARY KEY,
    folder_id uuid NOT NULL REFERENCES folders(id),
    snapshot_id uuid REFERENCES snapshots(id),
    expected_folder_sequence bigint NOT NULL CHECK (expected_folder_sequence >= 0),
    state text NOT NULL CHECK (state IN ('PREVIEW', 'READY', 'RUNNING', 'COMPLETED', 'FAILED', 'EXPIRED')),
    plan jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_by uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);

CREATE INDEX restore_jobs_folder_state_idx ON restore_jobs (folder_id, state, created_at DESC);

CREATE TABLE audit_logs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_type text NOT NULL,
    actor_id uuid,
    action text NOT NULL,
    folder_id uuid REFERENCES folders(id),
    device_id uuid REFERENCES devices(id),
    operation_id uuid,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_folder_created_idx ON audit_logs (folder_id, created_at DESC);
