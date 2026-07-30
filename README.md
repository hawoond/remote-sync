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
tests, runs `go vet`, and builds the server and agent.

### Local development quick start

Start PostgreSQL:

```sh
docker compose up -d postgres
```

Create development identifiers and apply the migration:

```sh
export DATABASE_URL='postgres://remote_sync:remote_sync@localhost:5432/remote_sync?sslmode=disable'
export SYNC_USER_ID="$(uuidgen)"
export SYNC_DEVICE_ID="$(uuidgen)"
export SYNC_FOLDER_ID="$(uuidgen)"
export SYNC_DEVICE_TOKEN="$(openssl rand -hex 32)"

go run ./cmd/sync-migrate
```

Start the server with development bootstrap and plaintext transport enabled:

```sh
export BLOB_ROOT='./data/blobs'
export ALLOW_INSECURE=true
export DEV_BOOTSTRAP=true

go run ./cmd/sync-server
```

`ALLOW_INSECURE=true` is only for local development. Production startup
requires `TLS_CERT_FILE` and `TLS_KEY_FILE`.

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

### Runtime configuration

Server variables:

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `DATABASE_URL` | Yes | — | PostgreSQL connection string |
| `BLOB_ROOT` | Yes | — | Local immutable blob-store root |
| `SYNC_DEVICE_ID` | Yes | — | Authorized device UUID |
| `SYNC_DEVICE_TOKEN` | Yes | — | Device bearer token, at least 32 characters |
| `LISTEN_ADDR` | No | `:8443` | gRPC listen address |
| `TLS_CERT_FILE` | Production | — | TLS certificate chain |
| `TLS_KEY_FILE` | Production | — | TLS private key |
| `ALLOW_INSECURE` | No | `false` | Disable TLS for local development only |
| `DEV_BOOTSTRAP` | No | `false` | Create local development user/device/folder records |
| `SYNC_USER_ID` | With bootstrap | — | Development user UUID |
| `SYNC_FOLDER_ID` | With bootstrap | — | Development folder UUID |

The server also accepts `MAX_FILE_SIZE_BYTES`,
`MAX_FOLDER_LIVE_SIZE_BYTES`, `MAX_USER_LIVE_SIZE_BYTES`,
`MAX_PENDING_UPLOAD_SIZE_BYTES`, and `MAX_CHUNK_SIZE_BYTES`. A chunk cannot
exceed 4 MiB.

Agent variables:

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `SYNC_FOLDER_ID` | Yes | — | Server-authorized folder UUID |
| `SYNC_DEVICE_TOKEN` | Yes | — | Device bearer token, at least 32 characters |
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

The server never applies migrations automatically. Run `sync-migrate` before
starting a server version that requires a new schema.

### Current scope

The current implementation is the Phase 1 backup foundation: portable path
validation, stable snapshot hashing, persistent client operations, resumable
whole-file uploads, immutable versions and tombstones, idempotent commits,
change cursors, authenticated gRPC transport, and interactive Codex/Claude
worktree selection. Restore orchestration, multi-device enrollment,
safety-window policy, and garbage collection remain follow-up work.

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

`make check`는 protobuf 코드 재생성, 포맷, race 테스트, `go vet`, 서버·에이전트
빌드를 모두 실행합니다.

### 로컬 개발 빠른 시작

PostgreSQL을 실행합니다.

```sh
docker compose up -d postgres
```

개발용 식별자를 만들고 마이그레이션을 적용합니다.

```sh
export DATABASE_URL='postgres://remote_sync:remote_sync@localhost:5432/remote_sync?sslmode=disable'
export SYNC_USER_ID="$(uuidgen)"
export SYNC_DEVICE_ID="$(uuidgen)"
export SYNC_FOLDER_ID="$(uuidgen)"
export SYNC_DEVICE_TOKEN="$(openssl rand -hex 32)"

go run ./cmd/sync-migrate
```

개발용 레코드 생성과 평문 전송을 허용해 서버를 실행합니다.

```sh
export BLOB_ROOT='./data/blobs'
export ALLOW_INSECURE=true
export DEV_BOOTSTRAP=true

go run ./cmd/sync-server
```

`ALLOW_INSECURE=true`는 로컬 개발에서만 사용해야 합니다. 운영 실행에는
`TLS_CERT_FILE`과 `TLS_KEY_FILE`이 필요합니다.

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

### 실행 설정

서버 환경 변수:

| 변수 | 필수 | 기본값 | 설명 |
| --- | --- | --- | --- |
| `DATABASE_URL` | 필수 | — | PostgreSQL 연결 문자열 |
| `BLOB_ROOT` | 필수 | — | 불변 로컬 Blob Store 루트 |
| `SYNC_DEVICE_ID` | 필수 | — | 허용된 기기 UUID |
| `SYNC_DEVICE_TOKEN` | 필수 | — | 32자 이상의 기기 Bearer 토큰 |
| `LISTEN_ADDR` | 선택 | `:8443` | gRPC 수신 주소 |
| `TLS_CERT_FILE` | 운영 필수 | — | TLS 인증서 체인 |
| `TLS_KEY_FILE` | 운영 필수 | — | TLS 개인 키 |
| `ALLOW_INSECURE` | 선택 | `false` | 로컬 개발에서만 TLS 비활성화 |
| `DEV_BOOTSTRAP` | 선택 | `false` | 개발용 사용자·기기·폴더 생성 |
| `SYNC_USER_ID` | bootstrap 시 | — | 개발 사용자 UUID |
| `SYNC_FOLDER_ID` | bootstrap 시 | — | 개발 폴더 UUID |

서버는 `MAX_FILE_SIZE_BYTES`, `MAX_FOLDER_LIVE_SIZE_BYTES`,
`MAX_USER_LIVE_SIZE_BYTES`, `MAX_PENDING_UPLOAD_SIZE_BYTES`,
`MAX_CHUNK_SIZE_BYTES`도 지원합니다. 청크 크기는 4 MiB를 넘을 수 없습니다.

에이전트 환경 변수:

| 변수 | 필수 | 기본값 | 설명 |
| --- | --- | --- | --- |
| `SYNC_FOLDER_ID` | 필수 | — | 서버에서 허용한 폴더 UUID |
| `SYNC_DEVICE_TOKEN` | 필수 | — | 32자 이상의 기기 Bearer 토큰 |
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

서버는 마이그레이션을 자동 적용하지 않습니다. 새 스키마가 필요한 서버 버전을
실행하기 전에 `sync-migrate`를 별도 실행해야 합니다.

### 현재 범위

현재 구현은 Phase 1 백업 기반입니다. 이식 가능한 경로 검증, 안정된 스냅샷
해싱, 영속 클라이언트 작업, 재개 가능한 전체 파일 업로드, 불변 버전과 tombstone,
멱등 커밋, 변경 커서, 인증된 gRPC 전송, Codex·Claude worktree 탐지와 사용자
선택을 포함합니다. 복원 오케스트레이션, 다중 기기 등록, 안전 유예 정책,
Garbage Collection은 후속 범위입니다.
