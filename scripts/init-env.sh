#!/bin/sh

set -eu

script_directory=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(CDPATH= cd -- "$script_directory/.." && pwd)
output_path="$repository_root/.env"
database_mode=""

usage() {
  printf '%s\n' \
    "Usage: $0 [--database bundled|external] [--output PATH]" \
    "" \
    "Without --database, an interactive terminal is required."
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --database)
      [ "$#" -ge 2 ] || {
        printf '%s\n' "init-env: --database requires a value" >&2
        exit 2
      }
      database_mode=$2
      shift 2
      ;;
    --output)
      [ "$#" -ge 2 ] || {
        printf '%s\n' "init-env: --output requires a path" >&2
        exit 2
      }
      output_path=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf '%s\n' "init-env: unknown argument $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [ -e "$output_path" ]; then
  printf '%s\n' "init-env: $output_path already exists; it was not changed" >&2
  exit 1
fi

if [ -z "$database_mode" ]; then
  if [ ! -t 0 ]; then
    printf '%s\n' \
      "init-env: database selection is required; use --database bundled or --database external" >&2
    exit 2
  fi
  printf '%s\n' \
    "Choose the PostgreSQL deployment:" \
    "  1) Bundled PostgreSQL container" \
    "  2) External PostgreSQL"
  printf '%s' "Selection [1-2]: "
  IFS= read -r selection
  case "$selection" in
    1) database_mode=bundled ;;
    2) database_mode=external ;;
    *)
      printf '%s\n' "init-env: invalid database selection" >&2
      exit 2
      ;;
  esac
fi

case "$database_mode" in
  bundled)
    database_url=""
    ;;
  external)
    database_url=${DATABASE_URL:-}
    if [ -z "$database_url" ] && [ -t 0 ]; then
      printf '%s' "External PostgreSQL DATABASE_URL: "
      terminal_settings=$(stty -g)
      restore_terminal() {
        stty "$terminal_settings" 2>/dev/null || true
      }
      trap 'restore_terminal; exit 130' HUP INT TERM
      stty -echo
      if ! IFS= read -r database_url; then
        restore_terminal
        trap - HUP INT TERM
        printf '\n%s\n' "init-env: could not read DATABASE_URL" >&2
        exit 2
      fi
      restore_terminal
      trap - HUP INT TERM
      printf '\n'
    fi
    if [ -z "$database_url" ]; then
      printf '%s\n' \
        "init-env: set DATABASE_URL when selecting an external database" >&2
      exit 2
    fi
    case "$database_url" in
      *'
'*|*"$(printf '\r')"*)
        printf '%s\n' "init-env: DATABASE_URL must fit on one line" >&2
        exit 2
        ;;
    esac
    ;;
  *)
    printf '%s\n' \
      "init-env: database must be bundled or external" >&2
    exit 2
    ;;
esac

random_hex() {
  byte_count=$1
  if [ ! -r /dev/urandom ]; then
    printf '%s\n' "init-env: /dev/urandom is unavailable" >&2
    exit 1
  fi
  od -An -N "$byte_count" -tx1 /dev/urandom | tr -d ' \n'
}

random_uuid() {
  value=$(random_hex 16)
  case "$(printf '%s' "$value" | cut -c 17)" in
    0|1|2|3) variant=8 ;;
    4|5|6|7) variant=9 ;;
    8|9|a|b) variant=a ;;
    c|d|e|f) variant=b ;;
  esac
  printf '%s-%s-%s-%s-%s' \
    "$(printf '%s' "$value" | cut -c 1-8)" \
    "$(printf '%s' "$value" | cut -c 9-12)" \
    "4$(printf '%s' "$value" | cut -c 14-16)" \
    "$variant$(printf '%s' "$value" | cut -c 18-20)" \
    "$(printf '%s' "$value" | cut -c 21-32)"
}

single_quote() {
  escaped=$(printf '%s' "$1" | sed "s/'/\\\\'/g")
  printf "'%s'" "$escaped"
}

postgres_password=$(random_hex 24)
sync_user_id=$(random_uuid)
sync_device_id=$(random_uuid)
sync_folder_id=$(random_uuid)
sync_device_token=$(random_hex 32)

temporary_path="${output_path}.tmp.$$"
umask 077
trap 'rm -f "$temporary_path"' EXIT HUP INT TERM

{
  printf '%s\n' \
    "DATABASE_MODE=$database_mode" \
    "COMPOSE_PROJECT_NAME=remote-sync" \
    "" \
    "POSTGRES_DB=remote_sync" \
    "POSTGRES_USER=remote_sync" \
    "POSTGRES_PASSWORD=$postgres_password" \
    "POSTGRES_BIND_ADDRESS=127.0.0.1" \
    "POSTGRES_PORT=5432"
  if [ "$database_mode" = external ]; then
    printf 'DATABASE_URL=%s\n' "$(single_quote "$database_url")"
  else
    printf '%s\n' "DATABASE_URL="
  fi
  printf '%s\n' \
    "" \
    "SYNC_USER_ID=$sync_user_id" \
    "SYNC_DEVICE_ID=$sync_device_id" \
    "SYNC_FOLDER_ID=$sync_folder_id" \
    "SYNC_DEVICE_TOKEN=$sync_device_token" \
    "" \
    "REMOTE_SYNC_BIND_ADDRESS=127.0.0.1" \
    "REMOTE_SYNC_PORT=8443" \
    "REMOTE_SYNC_IMAGE=remote-sync:local" \
    "ALLOW_INSECURE=true" \
    "DEV_BOOTSTRAP=true" \
    "" \
    "MAX_FILE_SIZE_BYTES=10737418240" \
    "MAX_FOLDER_LIVE_SIZE_BYTES=1099511627776" \
    "MAX_USER_LIVE_SIZE_BYTES=1099511627776" \
    "MAX_PENDING_UPLOAD_SIZE_BYTES=21474836480" \
    "MAX_CHUNK_SIZE_BYTES=1048576"
} >"$temporary_path"

if [ -e "$output_path" ]; then
  printf '%s\n' "init-env: $output_path appeared during setup; it was not changed" >&2
  exit 1
fi
mv "$temporary_path" "$output_path"
trap - EXIT HUP INT TERM

printf '%s\n' \
  "Wrote $output_path with database mode: $database_mode" \
  "Start the stack with: ./scripts/docker-compose.sh"
