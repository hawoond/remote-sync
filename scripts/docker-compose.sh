#!/bin/sh

set -eu

script_directory=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(CDPATH= cd -- "$script_directory/.." && pwd)
environment_file=${REMOTE_SYNC_ENV_FILE:-"$repository_root/.env"}

case "$environment_file" in
  /*) ;;
  *) environment_file="$(pwd)/$environment_file" ;;
esac

if [ ! -f "$environment_file" ]; then
  printf '%s\n' \
    "docker-compose: $environment_file does not exist" \
    "Run ./scripts/init-env.sh first." >&2
  exit 2
fi

database_mode=$(
  sed -n 's/^[[:space:]]*DATABASE_MODE[[:space:]]*=[[:space:]]*//p' \
    "$environment_file" |
    tail -n 1 |
    tr -d "\"'"
)

case "$database_mode" in
  bundled)
    compose_file="$repository_root/docker-compose.yml"
    ;;
  external)
    compose_file="$repository_root/docker-compose.external-db.yml"
    ;;
  *)
    printf '%s\n' \
      "docker-compose: DATABASE_MODE must be bundled or external in $environment_file" >&2
    exit 2
    ;;
esac

if [ "$#" -eq 0 ]; then
  set -- up -d --build
fi

exec docker compose \
  --env-file "$environment_file" \
  -f "$compose_file" \
  "$@"
