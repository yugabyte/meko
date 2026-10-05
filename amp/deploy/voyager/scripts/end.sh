#!/bin/bash
# Image entrypoint for AMP's irreversible Voyager cleanup job.

set -euo pipefail

: "${EXPORT_DIR:?EXPORT_DIR is required}"

# Voyager reads the target password from the environment rather than argv. An
# empty value makes it prompt on stdin, which a headless job cannot answer.
if [[ -z "${TARGET_DB_PASSWORD:-}" ]]; then
  echo "TARGET_DB_PASSWORD is empty; refusing to prompt in a headless job" >&2
  exit 2
fi
export TARGET_DB_PASSWORD

yb-voyager end migration \
  --export-dir "$EXPORT_DIR" \
  --backup-schema-files false \
  --backup-data-files false \
  --save-migration-reports false \
  --backup-log-files false \
  --yes
