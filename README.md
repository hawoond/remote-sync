# Remote Sync

[English](#english) | [한국어](#한국어)

## English

Remote Sync backs up a selected local folder or Git worktree to a central
server. It is designed for durable whole-file uploads, immutable versions,
resumable transfers, and recovery after process or network failures.

### Architecture

- `sync-agent` discovers or accepts a sync root, scans and watches it, persists
  pending work in SQLite, and uploads stable file snapshots.
- `sync-server` validates paths and limits, stores metadata in PostgreSQL, and
  promotes verified content into an immutable local blob store.
- gRPC carries control messages and chunked file transfers.
- `sync-migrate` applies versioned PostgreSQL migrations as a separate
  deployment step.

### Requirements

- Go 1.25.12 or later
- Git
- Protocol Buffers compiler
- Docker for the local PostgreSQL development environment

### Build and test

```sh
make tools
make generate
make check
```

`make check` regenerates protobuf code, formats the source, runs race-enabled
tests, runs `go vet`, and builds the server, migrator, and agent.

### Docker server quick start

Docker Compose can run the server, schema migration, blob volume, and optionally
PostgreSQL. Start by creating a private `.env` file:

```sh
./scripts/init-env.sh
```

The setup always asks which database deployment to use:

1. **Bundled PostgreSQL** — Compose starts `postgres:18-alpine`, waits for its
   health check, applies migrations, and then starts `sync-server`.
2. **External PostgreSQL** — Compose starts no database container. It applies
   migrations to the PostgreSQL `DATABASE_URL` supplied by the user and starts
   the server only if migration succeeds.

The choice is between bundled and user-managed PostgreSQL. PostgreSQL remains
the supported metadata engine; an external URL can point to a local instance or
a managed service such as RDS, Cloud SQL, Neon, or Supabase.
The wrapper selects `docker-compose.yml` for bundled mode and
`docker-compose.external-db.yml` for external mode.

For automation, make the selection explicitly:

```sh
# Bundled database
./scripts/init-env.sh --database bundled

# External database
DATABASE_URL='postgres://user:password@db.example.com:5432/remote_sync?sslmode=require' \
  ./scripts/init-env.sh --database external
```

The script creates random user, device, and folder UUIDs, a database password,
and a device token. It writes the file with mode `0600`, refuses to overwrite
an existing file, and `.env` is ignored by Git.

Start the selected stack:

```sh
./scripts/docker-compose.sh
```

With no arguments, the wrapper runs `docker compose up -d --build` against the
Compose file selected by `DATABASE_MODE`. The same wrapper handles normal
Compose commands:

```sh
./scripts/docker-compose.sh ps
./scripts/docker-compose.sh logs -f server
./scripts/docker-compose.sh down
```

The equivalent convenience targets are `make docker-init`, `make docker-up`,
`make docker-logs`, and `make docker-down`.

`down` preserves the PostgreSQL and blob volumes. Use `down --volumes` only
when the stored database and blobs should be permanently removed.

The server and PostgreSQL ports bind to `127.0.0.1` by default. The generated
configuration uses `ALLOW_INSECURE=true` for local operation. Before binding
`REMOTE_SYNC_BIND_ADDRESS=0.0.0.0`, mount the certificate and private-key files
into the server container, set their container paths in `TLS_CERT_FILE` and
`TLS_KEY_FILE`, and set `ALLOW_INSECURE=false`.

### Local development from source

Start only PostgreSQL with the bundled Compose file:

```sh
docker compose --env-file .env up -d postgres
```

Use the host-facing database URL, then run migration and server processes:

```sh
export DATABASE_URL='postgres://remote_sync:<password>@localhost:5432/remote_sync?sslmode=disable'
export BLOB_ROOT='./data/blobs'
export SYNC_USER_ID='<user UUID from .env>'
export SYNC_DEVICE_ID='<device UUID from .env>'
export SYNC_FOLDER_ID='<folder UUID from .env>'
export SYNC_DEVICE_TOKEN='<device token from .env>'
export ALLOW_INSECURE=true
export DEV_BOOTSTRAP=true

go run ./cmd/sync-migrate
go run ./cmd/sync-server
```

### Discover Codex and Claude worktrees

The discovery command does not require server credentials:

```sh
go run ./cmd/sync-agent discover
```

Example output:

```text
PROVIDER  REPOSITORY  BRANCH             ID                  PATH
claude   storefront   worktree-checkout  claude:4a90b23c719e /home/alice/storefront/.claude/worktrees/checkout
codex    payments     (detached)          codex:183f2b73a9d1  /home/alice/.codex/worktrees/task-42
```

Filter by provider or produce machine-readable JSON:

```sh
go run ./cmd/sync-agent discover --provider codex
go run ./cmd/sync-agent discover --provider claude
go run ./cmd/sync-agent discover --json
```

Discovery uses the following sources:

- Codex: `$CODEX_HOME/worktrees`; when `CODEX_HOME` is unset, the default is
  `~/.codex/worktrees`.
- Claude Code and Claude Desktop: `<project-root>/.claude/worktrees`.
- Claude project hints: the current Git repository and absolute project paths
  recorded in `~/.claude.json`.
- Claude config fallback: `$CLAUDE_CONFIG_DIR/worktrees`; when
  `CLAUDE_CONFIG_DIR` is unset, the default is `~/.claude/worktrees`.
- Additional roots supplied through the variables described below.

Every result is validated with Git. Stale directories and directories that are
not Git worktrees are omitted.

### Interactive selection is mandatory

When `SYNC_ROOT` and `SYNC_WORKTREE` are both unset, `sync-agent` discovers all
available Codex and Claude worktrees and asks the user to select one:

```sh
unset SYNC_ROOT
unset SYNC_WORKTREE

export SYNC_FOLDER_ID='<server-authorized-folder-uuid>'
export SYNC_DEVICE_TOKEN='<device-token-with-at-least-32-characters>'
export SYNC_SERVER_ADDRESS='127.0.0.1:8443'
export ALLOW_INSECURE=true

go run ./cmd/sync-agent
```

The prompt always requires a choice, even when only one worktree is found:

```text
#  PROVIDER  REPOSITORY  BRANCH             ID                  PATH
1  claude    storefront  worktree-checkout  claude:4a90b23c719e /home/alice/storefront/.claude/worktrees/checkout
2  codex     payments    (detached)          codex:183f2b73a9d1  /home/alice/.codex/worktrees/task-42
Select a worktree [1-2] or q to cancel:
```

The agent never picks the first result automatically.

### Non-interactive selection

A service, CI job, launch daemon, or container normally has no interactive
terminal. In that case, discovery prints the candidates and exits unless
`SYNC_WORKTREE` is set to a discovered ID or absolute path:

```sh
export SYNC_WORKTREE='codex:183f2b73a9d1'
go run ./cmd/sync-agent
```

or:

```sh
export SYNC_WORKTREE='/home/alice/.codex/worktrees/task-42'
go run ./cmd/sync-agent
```

IDs are stable for the same provider and canonical path. Run
`sync-agent discover --json` when a startup script needs to refresh them.

### Explicit folder mode

`SYNC_ROOT` remains the highest-priority option and bypasses worktree discovery
because the path itself is an explicit user choice:

```sh
export SYNC_ROOT='/absolute/path/to/folder'
unset SYNC_WORKTREE

go run ./cmd/sync-agent
```

The root does not have to be a Git repository in explicit folder mode.

### Custom discovery locations

Codex and Claude both allow custom worktree locations. Add those locations as
operating-system path lists:

```sh
export SYNC_CODEX_WORKTREE_ROOTS="$HOME/custom-codex:$HOME/other-codex"
export SYNC_CLAUDE_WORKTREE_ROOTS="$HOME/custom-claude"
export SYNC_DISCOVERY_PROJECT_ROOTS="$HOME/src/storefront:$HOME/src/payments"
```

Use `:` between paths on POSIX systems and `;` on Windows:

```powershell
$env:SYNC_CODEX_WORKTREE_ROOTS = 'D:\CodexWorktrees;E:\OtherCodex'
$env:SYNC_CLAUDE_WORKTREE_ROOTS = 'D:\ClaudeWorktrees'
$env:SYNC_DISCOVERY_PROJECT_ROOTS = 'D:\Projects\Storefront;D:\Projects\Payments'
```

- `SYNC_CODEX_WORKTREE_ROOTS` adds Codex worktree containers.
- `SYNC_CLAUDE_WORKTREE_ROOTS` adds Claude worktree containers, including a
  custom Claude Desktop “Worktree location” or a Git-based `WorktreeCreate`
  hook destination.
- `SYNC_DISCOVERY_PROJECT_ROOTS` adds Git project roots whose
  `.claude/worktrees` directories should be inspected.
- `SYNC_WORKTREE_PROVIDERS` limits normal startup discovery. Accepted values
  are `all`, `codex`, `claude`, or a comma-separated provider list.

Custom Claude hooks that create non-Git SVN, Perforce, or Mercurial checkouts
are not reported because Remote Sync validates Git worktrees.

### Worktree backup behavior

- One `sync-agent` process watches one selected worktree.
- The default SQLite state database is scoped by both `SYNC_FOLDER_ID` and the
  canonical selected root, preventing pending operations from another
  worktree from being reused accidentally.
- `.git`, nested `.claude/worktrees`, and nested Git repositories are excluded
  from scanning and filesystem watches.
- Use a distinct server-authorized `SYNC_FOLDER_ID` for worktrees whose remote
  histories must remain independent. Reusing a folder ID means the selected
  worktree is intended to update that same remote folder history.
- Deleting or archiving a worktree while the agent is running makes the root
  unavailable. The agent stops instead of treating the missing root as a mass
  deletion.

### Enroll another device

The bootstrap device has the `RESTORE_ADMIN` role. Build the binaries and use
that device's environment to create a short-lived, one-time enrollment token:

```bash
make build

export SYNC_SERVER_ADDRESS=127.0.0.1:8443
export SYNC_FOLDER_ID=681f7dd7-559b-4fab-8734-41b00f663425
export SYNC_DEVICE_TOKEN='existing-device-token'
export ALLOW_INSECURE=true

./bin/sync-agent enrollment create --role writer --expires 15m
```

Transfer only the returned `enrollment_token` to the new device. Enroll there
without exposing the token in the process list:

```bash
export SYNC_SERVER_ADDRESS=server.example.com:8443
export SYNC_ENROLLMENT_TOKEN='one-time-enrollment-token'

./bin/sync-agent enroll --name laptop --platform linux/amd64
```

The command returns a new `device_id`, `device_token`, and `folder_id` as JSON.
Store the device token in a secret manager and set `SYNC_DEVICE_TOKEN` and
`SYNC_FOLDER_ID` before starting that device's agent. Enrollment tokens are
stored as SHA-256 digests, expire automatically, and cannot be used twice.
Device bearer tokens are also stored only as digests.

Available roles:

- `reader`: restore and download access without backup writes.
- `writer`: read, restore, and backup-write access.
- `restore-admin`: writer access plus enrollment-token and lifecycle-policy
  administration.

### Restore a folder

Restore always creates a persistent server-side job and an immutable manifest
at the selected folder sequence. The client downloads each object, verifies its
SHA-256 and size, publishes it with an atomic rename, restores portable mode and
mtime, and records the server version in the local SQLite state.

```bash
# Latest server sequence into an empty directory.
./bin/sync-agent restore --target /srv/recovered-project

# Historical sequence.
./bin/sync-agent restore --target /srv/recovered-project --sequence 420

# Explicitly replace existing regular files.
./bin/sync-agent restore --target /srv/recovered-project --overwrite

# Continue an interrupted job using the ID printed when it was created.
./bin/sync-agent restore \
  --target /srv/recovered-project \
  --resume 2f32b0c2-1cef-4c58-aeca-70124c01372e
```

Without `--overwrite`, differing existing files are reported as skipped.
Directories, symlinks, and other non-regular paths are never replaced.
Restoration does not delete files that are absent from the manifest. Stop a
running agent before restoring into that agent's active root. A historical
sequence older than the folder's retained restore floor is rejected instead of
producing a partial manifest.

### Safety window and garbage collection

Each folder starts with a 30-day safety window and a 24-hour two-phase deletion
grace period. A restore administrator can inspect or change both:

```bash
./bin/sync-agent policy get

./bin/sync-agent policy set \
  --safety-window 720h \
  --gc-grace-period 24h
```

The collector prunes only superseded, non-head versions that satisfy every
condition below:

- The version has remained superseded for at least the folder's safety window.
- Every active reader device has acknowledged a sequence at or beyond it.
- No active restore job or pinned snapshot needs the version.
- The object is not referenced by another version or in-flight operation.

An unreferenced object first becomes `PENDING_DELETE`; its blob is removed only
after the folder's GC grace period and a final reference check. Expired upload
sessions and their reserved quota are also reclaimed; failed temporary-file
cleanup remains retryable until it is acknowledged. `GC_ENABLED`,
`GC_INTERVAL`, and `GC_BATCH_SIZE` control the bounded background collector.

### Runtime configuration

Docker setup variables:

| Variable | Used with | Default | Description |
| --- | --- | --- | --- |
| `DATABASE_MODE` | Wrapper | Selected during setup | `bundled` or `external` |
| `COMPOSE_PROJECT_NAME` | Both modes | `remote-sync` | Compose project and resource prefix |
| `DATABASE_URL` | External DB | — | User-selected PostgreSQL connection string |
| `POSTGRES_DB` | Bundled DB | `remote_sync` | Bundled database name |
| `POSTGRES_USER` | Bundled DB | `remote_sync` | Bundled database user |
| `POSTGRES_PASSWORD` | Bundled DB | Random | Bundled database password |
| `POSTGRES_BIND_ADDRESS` | Bundled DB | `127.0.0.1` | Host address for PostgreSQL |
| `POSTGRES_PORT` | Bundled DB | `5432` | Host PostgreSQL port |
| `REMOTE_SYNC_BIND_ADDRESS` | Server | `127.0.0.1` | Host address for gRPC |
| `REMOTE_SYNC_PORT` | Server | `8443` | Host gRPC port |
| `REMOTE_SYNC_IMAGE` | Both modes | `remote-sync:local` | Image name or prebuilt image reference |
| `REMOTE_SYNC_ENV_FILE` | Wrapper | `.env` | Alternate environment-file path |

Server variables:

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `DATABASE_URL` | Yes | — | PostgreSQL connection string |
| `BLOB_ROOT` | Yes | — | Local immutable blob-store root |
| `SYNC_DEVICE_ID` | With bootstrap or credential update | — | Configured device UUID |
| `SYNC_DEVICE_TOKEN` | With bootstrap or credential update | — | Device bearer token, at least 32 characters |
| `LISTEN_ADDR` | No | `:8443` | gRPC listen address |
| `TLS_CERT_FILE` | Production | — | TLS certificate chain |
| `TLS_KEY_FILE` | Production | — | TLS private key |
| `ALLOW_INSECURE` | No | `false` | Disable TLS for local development only |
| `DEV_BOOTSTRAP` | No | `false` | Create local development user/device/folder records |
| `SYNC_USER_ID` | With bootstrap | — | Development user UUID |
| `SYNC_FOLDER_ID` | With bootstrap | — | Development folder UUID |
| `GC_ENABLED` | No | `true` | Run lifecycle garbage collection |
| `GC_INTERVAL` | No | `1h` | Collection interval |
| `GC_BATCH_SIZE` | No | `100` | Maximum records per collection phase, up to 1000 |

The server also accepts `MAX_FILE_SIZE_BYTES`,
`MAX_FOLDER_LIVE_SIZE_BYTES`, `MAX_USER_LIVE_SIZE_BYTES`,
`MAX_PENDING_UPLOAD_SIZE_BYTES`, and `MAX_CHUNK_SIZE_BYTES`. A chunk cannot
exceed 4 MiB.

Agent variables:

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `SYNC_FOLDER_ID` | Yes | — | Server-authorized folder UUID |
| `SYNC_DEVICE_TOKEN` | Yes | — | Device bearer token, at least 32 characters |
| `SYNC_ENROLLMENT_TOKEN` | Enrollment only | — | One-time token consumed by `sync-agent enroll` |
| `SYNC_ROOT` | No | — | Explicit root; bypasses discovery |
| `SYNC_WORKTREE` | Non-interactive discovery | — | Discovered ID or absolute path |
| `SYNC_WORKTREE_PROVIDERS` | No | `all` | Provider filter |
| `SYNC_SERVER_ADDRESS` | No | `127.0.0.1:8443` | gRPC server address |
| `SYNC_STATE_PATH` | No | OS config directory | Explicit SQLite state path |
| `SCAN_INTERVAL` | No | `15m` | Full scan interval |
| `WATCH_DEBOUNCE` | No | `500ms` | Filesystem event debounce |
| `TLS_CA_FILE` | No | System trust store | Private CA bundle |
| `TLS_SERVER_NAME` | No | Address host | TLS server name override |
| `ALLOW_INSECURE` | No | `false` | Disable TLS for local development only |

The server binary never applies migrations automatically. Both Compose modes
run `sync-migrate` as a separate one-shot service and start the server only
after it succeeds. When running binaries directly, execute `sync-migrate`
before starting a server version that requires a new schema.

## 한국어

Remote Sync는 사용자가 선택한 로컬 폴더 또는 Git worktree를 중앙 서버에
백업합니다. 전체 파일 단위의 내구성 있는 업로드, 불변 버전, 전송 재개,
프로세스·네트워크 장애 후 복구를 목표로 합니다.

### 구성

- `sync-agent`는 동기화 루트를 탐지하거나 명시적으로 전달받고, 파일을
  스캔·감시하며, SQLite에 작업을 보존한 뒤 안정된 파일 스냅샷을 업로드합니다.
- `sync-server`는 경로와 용량 제한을 검증하고 PostgreSQL에 메타데이터를
  저장하며, 검증된 콘텐츠를 불변 로컬 Blob Store로 승격합니다.
- gRPC가 제어 메시지와 청크 파일 전송을 담당합니다.
- `sync-migrate`는 PostgreSQL 마이그레이션을 서버 실행과 분리해 적용합니다.

### 요구 사항

- Go 1.25.12 이상
- Git
- Protocol Buffers 컴파일러
- 로컬 PostgreSQL 개발 환경용 Docker

### 빌드와 테스트

```sh
make tools
make generate
make check
```

`make check`는 protobuf 코드 재생성, 포맷, race 테스트, `go vet`,
서버·마이그레이터·에이전트 빌드를 모두 실행합니다.

### Docker 서버 빠른 시작

Docker Compose로 서버, 스키마 마이그레이션, Blob 볼륨과 선택한 경우
PostgreSQL까지 실행할 수 있습니다. 먼저 비공개 `.env` 파일을 만듭니다.

```sh
./scripts/init-env.sh
```

설정 과정에서 사용할 데이터베이스 배포 방식을 반드시 선택합니다.

1. **내장 PostgreSQL** — Compose가 `postgres:18-alpine`을 실행하고
   health check 통과를 기다린 뒤 마이그레이션과 `sync-server`를 순서대로
   실행합니다.
2. **외부 PostgreSQL** — 데이터베이스 컨테이너를 실행하지 않습니다. 사용자가
   지정한 `DATABASE_URL`에 마이그레이션을 적용하고, 성공한 경우에만 서버를
   시작합니다.

선택 대상은 내장 PostgreSQL과 사용자 관리 PostgreSQL입니다. 메타데이터
엔진은 PostgreSQL을 사용하며, 외부 URL에는 로컬 DB뿐 아니라 RDS, Cloud SQL,
Neon, Supabase 같은 관리형 서비스를 지정할 수 있습니다.
래퍼는 내장 모드에서 `docker-compose.yml`, 외부 모드에서
`docker-compose.external-db.yml`을 선택합니다.

자동화 환경에서는 선택을 인자로 명시합니다.

```sh
# 내장 데이터베이스
./scripts/init-env.sh --database bundled

# 외부 데이터베이스
DATABASE_URL='postgres://user:password@db.example.com:5432/remote_sync?sslmode=require' \
  ./scripts/init-env.sh --database external
```

스크립트는 사용자·기기·폴더 UUID, DB 비밀번호와 기기 토큰을 무작위로 만들고
파일 권한을 `0600`으로 설정합니다. 기존 파일은 덮어쓰지 않으며 `.env`는
Git에서 제외됩니다.

선택한 구성을 실행합니다.

```sh
./scripts/docker-compose.sh
```

인자가 없으면 `DATABASE_MODE`에 맞는 Compose 파일을 선택해
`docker compose up -d --build`를 실행합니다. 같은 래퍼로 일반 Compose
명령도 사용할 수 있습니다.

```sh
./scripts/docker-compose.sh ps
./scripts/docker-compose.sh logs -f server
./scripts/docker-compose.sh down
```

같은 작업은 `make docker-init`, `make docker-up`, `make docker-logs`,
`make docker-down`으로도 실행할 수 있습니다.

`down`은 PostgreSQL과 Blob 볼륨을 보존합니다. 저장된 DB와 Blob을 영구
삭제하려는 경우에만 `down --volumes`를 사용하세요.

서버와 PostgreSQL 포트는 기본적으로 `127.0.0.1`에만 바인딩됩니다. 생성된
설정의 `ALLOW_INSECURE=true`는 로컬 실행용입니다.
`REMOTE_SYNC_BIND_ADDRESS=0.0.0.0`으로 외부에 공개하기 전에는 인증서와
개인 키를 서버 컨테이너에 읽기 전용으로 마운트하고, 컨테이너 내부 경로를
`TLS_CERT_FILE`과 `TLS_KEY_FILE`에 지정한 뒤 `ALLOW_INSECURE=false`로
변경하세요.

### 소스 기반 로컬 개발

내장 Compose 파일에서 PostgreSQL만 실행합니다.

```sh
docker compose --env-file .env up -d postgres
```

호스트 접속용 DB URL과 `.env`의 식별자를 사용해 마이그레이션과 서버를
실행합니다.

```sh
export DATABASE_URL='postgres://remote_sync:<password>@localhost:5432/remote_sync?sslmode=disable'
export BLOB_ROOT='./data/blobs'
export SYNC_USER_ID='<.env의 사용자 UUID>'
export SYNC_DEVICE_ID='<.env의 기기 UUID>'
export SYNC_FOLDER_ID='<.env의 폴더 UUID>'
export SYNC_DEVICE_TOKEN='<.env의 기기 토큰>'
export ALLOW_INSECURE=true
export DEV_BOOTSTRAP=true

go run ./cmd/sync-migrate
go run ./cmd/sync-server
```

### Codex·Claude worktree 탐지

탐지 명령에는 서버 인증 정보가 필요하지 않습니다.

```sh
go run ./cmd/sync-agent discover
```

제공자 필터와 JSON 출력을 사용할 수 있습니다.

```sh
go run ./cmd/sync-agent discover --provider codex
go run ./cmd/sync-agent discover --provider claude
go run ./cmd/sync-agent discover --json
```

탐지에 사용하는 위치는 다음과 같습니다.

- Codex: `$CODEX_HOME/worktrees`. `CODEX_HOME`이 없으면
  `~/.codex/worktrees`를 사용합니다.
- Claude Code·Claude Desktop: 각 프로젝트의
  `<project-root>/.claude/worktrees`.
- Claude 프로젝트 힌트: 현재 Git 저장소와 `~/.claude.json`에 기록된 절대
  프로젝트 경로.
- Claude 설정 폴백: `$CLAUDE_CONFIG_DIR/worktrees`.
  `CLAUDE_CONFIG_DIR`이 없으면 `~/.claude/worktrees`.
- 아래 환경 변수로 전달한 추가 위치.

모든 결과는 Git으로 다시 검증합니다. 오래된 디렉터리와 실제 Git worktree가
아닌 디렉터리는 목록에서 제외합니다.

### 사용자가 반드시 선택

`SYNC_ROOT`와 `SYNC_WORKTREE`가 모두 없으면 `sync-agent`가 Codex·Claude
worktree를 찾은 뒤 번호 목록을 보여줍니다.

```sh
unset SYNC_ROOT
unset SYNC_WORKTREE

export SYNC_FOLDER_ID='<서버에서 허용한 폴더 UUID>'
export SYNC_DEVICE_TOKEN='<32자 이상의 기기 토큰>'
export SYNC_SERVER_ADDRESS='127.0.0.1:8443'
export ALLOW_INSECURE=true

go run ./cmd/sync-agent
```

발견한 worktree가 하나뿐이어도 사용자가 번호를 입력해야 합니다.

```text
#  PROVIDER  REPOSITORY  BRANCH             ID                  PATH
1  claude    storefront  worktree-checkout  claude:4a90b23c719e /home/alice/storefront/.claude/worktrees/checkout
2  codex     payments    (detached)          codex:183f2b73a9d1  /home/alice/.codex/worktrees/task-42
Select a worktree [1-2] or q to cancel:
```

첫 번째 항목을 임의로 고르는 동작은 없습니다.

### 비대화형 실행

서비스, CI, launch daemon, 컨테이너처럼 터미널 입력이 없는 환경에서는
탐지 목록을 출력한 뒤 종료합니다. 이때는 `SYNC_WORKTREE`에 탐지 ID 또는 절대
경로를 지정해야 합니다.

```sh
export SYNC_WORKTREE='codex:183f2b73a9d1'
go run ./cmd/sync-agent
```

또는:

```sh
export SYNC_WORKTREE='/home/alice/.codex/worktrees/task-42'
go run ./cmd/sync-agent
```

ID는 제공자와 정규화된 경로가 같으면 안정적으로 유지됩니다. 시작 스크립트에서
목록을 갱신하려면 `sync-agent discover --json`을 사용합니다.

### 명시적 폴더 모드

`SYNC_ROOT`는 최우선 설정입니다. 경로 자체가 사용자의 명시적 선택이므로
worktree 탐지를 건너뜁니다.

```sh
export SYNC_ROOT='/absolute/path/to/folder'
unset SYNC_WORKTREE

go run ./cmd/sync-agent
```

명시적 폴더 모드에서는 Git 저장소가 아닌 일반 디렉터리도 사용할 수 있습니다.

### 사용자 지정 탐지 위치

Codex와 Claude에서 worktree 위치를 변경했다면 운영체제 경로 목록으로
추가합니다.

```sh
export SYNC_CODEX_WORKTREE_ROOTS="$HOME/custom-codex:$HOME/other-codex"
export SYNC_CLAUDE_WORKTREE_ROOTS="$HOME/custom-claude"
export SYNC_DISCOVERY_PROJECT_ROOTS="$HOME/src/storefront:$HOME/src/payments"
```

POSIX에서는 경로 사이에 `:`, Windows에서는 `;`를 사용합니다.

```powershell
$env:SYNC_CODEX_WORKTREE_ROOTS = 'D:\CodexWorktrees;E:\OtherCodex'
$env:SYNC_CLAUDE_WORKTREE_ROOTS = 'D:\ClaudeWorktrees'
$env:SYNC_DISCOVERY_PROJECT_ROOTS = 'D:\Projects\Storefront;D:\Projects\Payments'
```

- `SYNC_CODEX_WORKTREE_ROOTS`: Codex worktree 컨테이너를 추가합니다.
- `SYNC_CLAUDE_WORKTREE_ROOTS`: Claude Desktop의 사용자 지정 “Worktree
  location” 또는 Git 기반 `WorktreeCreate` 훅 위치를 포함한 Claude worktree
  컨테이너를 추가합니다.
- `SYNC_DISCOVERY_PROJECT_ROOTS`: `.claude/worktrees`를 확인할 Git 프로젝트
  루트를 추가합니다.
- `SYNC_WORKTREE_PROVIDERS`: 일반 실행의 제공자를 제한합니다. `all`,
  `codex`, `claude` 또는 쉼표로 구분한 제공자 목록을 지원합니다.

사용자 지정 Claude 훅이 SVN, Perforce, Mercurial 등 Git이 아닌 체크아웃을
만들면 Git worktree 검증을 통과하지 않으므로 탐지하지 않습니다.

### Worktree 백업 동작

- `sync-agent` 프로세스 하나는 사용자가 선택한 worktree 하나만 감시합니다.
- 기본 SQLite 상태 DB는 `SYNC_FOLDER_ID`와 정규화된 선택 루트에 함께
  종속됩니다. 다른 worktree의 미완료 작업을 실수로 재사용하지 않습니다.
- `.git`, 중첩된 `.claude/worktrees`, 중첩 Git 저장소는 스캔과 파일 감시에서
  제외합니다.
- 원격 이력을 독립적으로 보존할 worktree마다 서버에서 허용된 별도
  `SYNC_FOLDER_ID`를 사용하세요. 같은 폴더 ID를 다시 사용하면 같은 원격 폴더
  이력을 갱신하려는 의도로 처리됩니다.
- 에이전트 실행 중 worktree를 삭제하거나 보관해 루트가 사라지면 대량 삭제로
  간주하지 않고 에이전트가 중단됩니다.

### 다른 기기 등록

Bootstrap 기기에는 `RESTORE_ADMIN` 역할이 부여됩니다. 바이너리를 빌드한 뒤
기존 기기의 환경에서 유효 시간이 짧은 일회용 등록 토큰을 만드세요.

```bash
make build

export SYNC_SERVER_ADDRESS=127.0.0.1:8443
export SYNC_FOLDER_ID=681f7dd7-559b-4fab-8734-41b00f663425
export SYNC_DEVICE_TOKEN='existing-device-token'
export ALLOW_INSECURE=true

./bin/sync-agent enrollment create --role writer --expires 15m
```

응답의 `enrollment_token`만 새 기기로 안전하게 전달합니다. 토큰이 프로세스
목록에 노출되지 않도록 환경 변수로 등록하세요.

```bash
export SYNC_SERVER_ADDRESS=server.example.com:8443
export SYNC_ENROLLMENT_TOKEN='one-time-enrollment-token'

./bin/sync-agent enroll --name laptop --platform linux/amd64
```

명령은 새 `device_id`, `device_token`, `folder_id`를 JSON으로 반환합니다.
기기 토큰은 Secret Manager에 보관하고 에이전트 시작 전에
`SYNC_DEVICE_TOKEN`과 `SYNC_FOLDER_ID`로 설정하세요. 등록 토큰은 SHA-256
digest로만 저장되고 자동 만료되며 한 번만 사용할 수 있습니다. 기기 Bearer
토큰도 digest만 저장합니다.

역할별 권한:

- `reader`: 백업 쓰기 없이 복원과 다운로드만 허용합니다.
- `writer`: 읽기, 복원, 백업 쓰기를 허용합니다.
- `restore-admin`: writer 권한에 더해 등록 토큰 발급과 보존 정책 관리를
  허용합니다.

### 폴더 복원

복원할 때 서버는 선택한 폴더 sequence의 영속 작업과 immutable manifest를
생성합니다. 클라이언트는 각 객체의 SHA-256과 크기를 확인한 뒤 atomic
rename으로 게시하고, portable mode와 mtime을 복원하며 서버 버전을 로컬
SQLite 상태 DB에 기록합니다.

```bash
# 최신 서버 sequence를 빈 디렉터리에 복원합니다.
./bin/sync-agent restore --target /srv/recovered-project

# 지정한 과거 sequence를 복원합니다.
./bin/sync-agent restore --target /srv/recovered-project --sequence 420

# 기존 일반 파일 교체를 명시적으로 허용합니다.
./bin/sync-agent restore --target /srv/recovered-project --overwrite

# 작업 생성 시 출력된 ID로 중단된 복원을 이어갑니다.
./bin/sync-agent restore \
  --target /srv/recovered-project \
  --resume 2f32b0c2-1cef-4c58-aeca-70124c01372e
```

`--overwrite`가 없으면 내용이 다른 기존 파일은 skipped로 기록합니다.
디렉터리, 심볼릭 링크, 기타 일반 파일이 아닌 경로는 교체하지 않습니다.
manifest에 없는 파일도 삭제하지 않습니다. 에이전트가 감시 중인 루트에
복원하려면 먼저 해당 에이전트를 중지하세요. 폴더의 보존 가능한 복원
sequence보다 오래된 요청은 불완전한 manifest를 만들지 않고 거부합니다.

### 안전 유예와 Garbage Collection

각 폴더의 초기 안전 유예는 30일이고 2단계 삭제 유예는 24시간입니다. 복원
관리자는 두 값을 조회하거나 변경할 수 있습니다.

```bash
./bin/sync-agent policy get

./bin/sync-agent policy set \
  --safety-window 720h \
  --gc-grace-period 24h
```

Collector는 다음 조건을 모두 충족한 이전 non-head 버전만 정리합니다.

- 이전 버전으로 대체된 뒤 폴더의 안전 유예가 모두 지났습니다.
- 모든 활성 reader 기기가 해당 sequence 이상을 확인했습니다.
- 활성 복원 작업과 고정된 snapshot이 해당 버전을 사용하지 않습니다.
- 다른 버전이나 진행 중 작업이 객체를 참조하지 않습니다.

참조가 사라진 객체는 먼저 `PENDING_DELETE`가 됩니다. 폴더의 GC 유예 종료 후
참조를 다시 확인한 경우에만 Blob을 삭제합니다. 만료된 업로드 세션과 예약
용량도 함께 회수하며, 임시 파일 정리가 실패하면 완료 처리 전까지 다시
시도합니다. `GC_ENABLED`, `GC_INTERVAL`, `GC_BATCH_SIZE`로
제한된 백그라운드 Collector를 제어합니다.

### 실행 설정

Docker 설정 변수:

| 변수 | 사용 모드 | 기본값 | 설명 |
| --- | --- | --- | --- |
| `DATABASE_MODE` | 래퍼 | 설정 시 선택 | `bundled` 또는 `external` |
| `COMPOSE_PROJECT_NAME` | 두 모드 | `remote-sync` | Compose 프로젝트·리소스 접두사 |
| `DATABASE_URL` | 외부 DB | — | 사용자가 선택한 PostgreSQL 연결 문자열 |
| `POSTGRES_DB` | 내장 DB | `remote_sync` | 내장 데이터베이스 이름 |
| `POSTGRES_USER` | 내장 DB | `remote_sync` | 내장 데이터베이스 사용자 |
| `POSTGRES_PASSWORD` | 내장 DB | 무작위 값 | 내장 데이터베이스 비밀번호 |
| `POSTGRES_BIND_ADDRESS` | 내장 DB | `127.0.0.1` | PostgreSQL 호스트 바인딩 주소 |
| `POSTGRES_PORT` | 내장 DB | `5432` | PostgreSQL 호스트 포트 |
| `REMOTE_SYNC_BIND_ADDRESS` | 서버 | `127.0.0.1` | gRPC 호스트 바인딩 주소 |
| `REMOTE_SYNC_PORT` | 서버 | `8443` | gRPC 호스트 포트 |
| `REMOTE_SYNC_IMAGE` | 두 모드 | `remote-sync:local` | 이미지 이름 또는 사전 빌드 이미지 |
| `REMOTE_SYNC_ENV_FILE` | 래퍼 | `.env` | 다른 환경 파일 경로 |

서버 환경 변수:

| 변수 | 필수 | 기본값 | 설명 |
| --- | --- | --- | --- |
| `DATABASE_URL` | 필수 | — | PostgreSQL 연결 문자열 |
| `BLOB_ROOT` | 필수 | — | 불변 로컬 Blob Store 루트 |
| `SYNC_DEVICE_ID` | bootstrap 또는 credential 갱신 시 | — | 설정할 기기 UUID |
| `SYNC_DEVICE_TOKEN` | bootstrap 또는 credential 갱신 시 | — | 32자 이상의 기기 Bearer 토큰 |
| `LISTEN_ADDR` | 선택 | `:8443` | gRPC 수신 주소 |
| `TLS_CERT_FILE` | 운영 필수 | — | TLS 인증서 체인 |
| `TLS_KEY_FILE` | 운영 필수 | — | TLS 개인 키 |
| `ALLOW_INSECURE` | 선택 | `false` | 로컬 개발에서만 TLS 비활성화 |
| `DEV_BOOTSTRAP` | 선택 | `false` | 개발용 사용자·기기·폴더 생성 |
| `SYNC_USER_ID` | bootstrap 시 | — | 개발 사용자 UUID |
| `SYNC_FOLDER_ID` | bootstrap 시 | — | 개발 폴더 UUID |
| `GC_ENABLED` | 선택 | `true` | 수명 주기 Garbage Collection 실행 |
| `GC_INTERVAL` | 선택 | `1h` | 정리 실행 주기 |
| `GC_BATCH_SIZE` | 선택 | `100` | 단계별 최대 처리 건수, 최대 1000 |

서버는 `MAX_FILE_SIZE_BYTES`, `MAX_FOLDER_LIVE_SIZE_BYTES`,
`MAX_USER_LIVE_SIZE_BYTES`, `MAX_PENDING_UPLOAD_SIZE_BYTES`,
`MAX_CHUNK_SIZE_BYTES`도 지원합니다. 청크 크기는 4 MiB를 넘을 수 없습니다.

에이전트 환경 변수:

| 변수 | 필수 | 기본값 | 설명 |
| --- | --- | --- | --- |
| `SYNC_FOLDER_ID` | 필수 | — | 서버에서 허용한 폴더 UUID |
| `SYNC_DEVICE_TOKEN` | 필수 | — | 32자 이상의 기기 Bearer 토큰 |
| `SYNC_ENROLLMENT_TOKEN` | 기기 등록 시 | — | `sync-agent enroll`이 소비할 일회용 토큰 |
| `SYNC_ROOT` | 선택 | — | 명시적 루트, 탐지 생략 |
| `SYNC_WORKTREE` | 비대화형 탐지 시 | — | 탐지 ID 또는 절대 경로 |
| `SYNC_WORKTREE_PROVIDERS` | 선택 | `all` | 제공자 필터 |
| `SYNC_SERVER_ADDRESS` | 선택 | `127.0.0.1:8443` | gRPC 서버 주소 |
| `SYNC_STATE_PATH` | 선택 | OS 설정 디렉터리 | SQLite 상태 DB 명시 경로 |
| `SCAN_INTERVAL` | 선택 | `15m` | 전체 스캔 주기 |
| `WATCH_DEBOUNCE` | 선택 | `500ms` | 파일 이벤트 디바운스 |
| `TLS_CA_FILE` | 선택 | 시스템 신뢰 저장소 | 사설 CA 번들 |
| `TLS_SERVER_NAME` | 선택 | 주소의 호스트 | TLS 서버 이름 재정의 |
| `ALLOW_INSECURE` | 선택 | `false` | 로컬 개발에서만 TLS 비활성화 |

서버 바이너리는 마이그레이션을 자동 적용하지 않습니다. 두 Compose 모드 모두
`sync-migrate`를 별도 일회성 서비스로 실행하고, 성공한 경우에만 서버를
시작합니다. 바이너리를 직접 실행할 때는 새 스키마가 필요한 서버 버전을
시작하기 전에 `sync-migrate`를 실행해야 합니다.
