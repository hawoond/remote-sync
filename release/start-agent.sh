#!/bin/sh

set -eu

script_directory=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
config_file=${REMOTE_SYNC_CONFIG:-"$script_directory/remote-sync.env"}

if [ ! -f "$config_file" ]; then
  printf '%s\n' \
    "Remote Sync configuration was not found:" \
    "  $config_file" \
    "" \
    "Copy remote-sync.env.example to remote-sync.env, edit the connection values, and start again." >&2
  exit 2
fi

chmod 600 "$config_file" 2>/dev/null || true
set -a
# shellcheck disable=SC1090
. "$config_file"
set +a

exec "$script_directory/sync-agent" "$@"
