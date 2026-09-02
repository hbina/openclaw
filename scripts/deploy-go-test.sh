#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

printf 'deploy-go-test.sh now uses the fresh-state Docker cutover workflow.\n' >&2
exec python3 "$SCRIPT_DIR/deploy-go-docker.py" "$@"
