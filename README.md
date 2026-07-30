# Remote Sync

Remote Sync keeps local folders backed up to a central server. The first
release focuses on durable whole-file uploads, immutable versions, resumable
transfers, and recovery after process or network failures.

## Architecture

- `sync-agent` watches and scans a local folder, persists work in SQLite, and
  uploads stable file snapshots.
- `sync-server` validates paths and limits, stores metadata in PostgreSQL, and
  promotes verified content into an immutable local blob store.
- gRPC is used for control messages and chunked file transfer.

## Requirements

- Go 1.25.12 or later
- Protocol Buffers compiler
- Docker for the local PostgreSQL environment

## Development

```sh
make tools
make generate
make check
```

Start PostgreSQL:

```sh
docker compose up -d postgres
```

Create development identifiers and apply the database migration:

```sh
export DATABASE_URL='postgres://remote_sync:remote_sync@localhost:5432/remote_sync?sslmode=disable'
export SYNC_USER_ID="$(uuidgen)"
export SYNC_DEVICE_ID="$(uuidgen)"
export SYNC_FOLDER_ID="$(uuidgen)"
export SYNC_DEVICE_TOKEN="$(openssl rand -hex 32)"

go run ./cmd/sync-migrate
```

Start the server with the development bootstrap enabled:

```sh
export BLOB_ROOT='./data/blobs'
export ALLOW_INSECURE=true
export DEV_BOOTSTRAP=true

go run ./cmd/sync-server
```

In another shell, export the same `SYNC_FOLDER_ID` and `SYNC_DEVICE_TOKEN`,
then start the agent:

```sh
export SYNC_ROOT='/absolute/path/to/folder'
export SYNC_SERVER_ADDRESS='127.0.0.1:8443'
export ALLOW_INSECURE=true

go run ./cmd/sync-agent
```

`ALLOW_INSECURE=true` is intended only for local development. The server
requires `TLS_CERT_FILE` and `TLS_KEY_FILE` by default. The agent verifies the
system trust store by default and accepts `TLS_CA_FILE` and `TLS_SERVER_NAME`
when a private certificate authority is used.

The server never applies migrations automatically. Run `sync-migrate` as a
separate deployment step before starting a new server version.

## Runtime configuration

Server variables:

- `DATABASE_URL`, `BLOB_ROOT`, `SYNC_DEVICE_ID`, and `SYNC_DEVICE_TOKEN` are
  required.
- `LISTEN_ADDR` defaults to `:8443`.
- `DEV_BOOTSTRAP=true` creates the configured `SYNC_USER_ID`,
  `SYNC_DEVICE_ID`, and `SYNC_FOLDER_ID` records for local development.
- File, folder, user, pending-upload, and chunk limits can be adjusted with the
  corresponding `MAX_*_BYTES` variables. Chunks cannot exceed 4 MiB.

Agent variables:

- `SYNC_ROOT`, `SYNC_FOLDER_ID`, and `SYNC_DEVICE_TOKEN` are required.
- `SYNC_SERVER_ADDRESS` defaults to `127.0.0.1:8443`.
- `SYNC_STATE_PATH` defaults to a per-folder SQLite database in the operating
  system user configuration directory.
- `SCAN_INTERVAL` defaults to `15m`, and `WATCH_DEBOUNCE` defaults to `500ms`.

## Current scope

The current implementation is the Phase 1 backup foundation: portable path
validation, stable snapshot hashing, persistent client operations, resumable
whole-file uploads, immutable versions and tombstones, idempotent commits,
change cursors, and authenticated gRPC transport. Restore orchestration,
multi-device enrollment, safety-window policy, and garbage collection remain
separate follow-up work.
