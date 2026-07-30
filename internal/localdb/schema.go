package localdb

const schema = `
CREATE TABLE IF NOT EXISTS local_entries (
    folder_id TEXT NOT NULL,
    path_key TEXT NOT NULL,
    display_path TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    mtime_unix_nano INTEGER NOT NULL,
    portable_mode INTEGER NOT NULL CHECK (portable_mode >= 0),
    hash BLOB CHECK (hash IS NULL OR length(hash) = 32),
    server_version_id TEXT,
    present INTEGER NOT NULL CHECK (present IN (0, 1)),
    scan_generation INTEGER NOT NULL DEFAULT 0 CHECK (scan_generation >= 0),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (folder_id, path_key)
);

CREATE TABLE IF NOT EXISTS pending_operations (
    operation_id TEXT PRIMARY KEY,
    folder_id TEXT NOT NULL,
    path_key TEXT NOT NULL,
    display_path TEXT NOT NULL,
    kind INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (
        state IN (
            'QUEUED', 'RESERVING', 'UPLOADING', 'COMMITTING',
            'RETRY_WAIT', 'BLOCKED', 'QUARANTINED', 'COMPLETED', 'CANCELLED'
        )
    ),
    base_version_id TEXT,
    hash BLOB CHECK (hash IS NULL OR length(hash) = 32),
    size INTEGER NOT NULL CHECK (size >= 0),
    mtime_unix_nano INTEGER NOT NULL,
    portable_mode INTEGER NOT NULL CHECK (portable_mode >= 0),
    upload_session_id TEXT,
    next_offset INTEGER NOT NULL DEFAULT 0 CHECK (next_offset >= 0),
    attempt INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    last_error_code TEXT,
    last_error_message TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS pending_operations_due_idx
    ON pending_operations (state, next_attempt_at, created_at);

CREATE INDEX IF NOT EXISTS pending_operations_path_idx
    ON pending_operations (folder_id, path_key, created_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS pending_operations_active_path_idx
    ON pending_operations (folder_id, path_key)
    WHERE state NOT IN ('COMPLETED', 'CANCELLED');

CREATE TABLE IF NOT EXISTS folder_cursors (
    folder_id TEXT PRIMARY KEY,
    received_sequence INTEGER NOT NULL DEFAULT 0 CHECK (received_sequence >= 0),
    acked_sequence INTEGER NOT NULL DEFAULT 0 CHECK (acked_sequence >= 0),
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS expected_remote_applies (
    folder_id TEXT NOT NULL,
    path_key TEXT NOT NULL,
    expected_hash BLOB NOT NULL CHECK (length(expected_hash) = 32),
    server_version_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'APPLIED', 'FAILED')),
    cleanup_after INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (folder_id, path_key)
);

CREATE TABLE IF NOT EXISTS folder_scans (
    folder_id TEXT PRIMARY KEY,
    generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
    started_at INTEGER NOT NULL DEFAULT 0,
    completed_at INTEGER
);
`
