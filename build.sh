#!/usr/bin/env bash
# Thin wrapper around scripts/build.py — the one build entry point for
# yb-light (handles the Postgres submodule, headers, and debug/release/tar
# builds). Run `./build.sh --help` for modes.
set -euo pipefail
DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
exec python3 "${DIR}/scripts/build.py" "$@"
