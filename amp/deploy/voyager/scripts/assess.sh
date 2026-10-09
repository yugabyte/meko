#!/bin/bash
# Image entrypoint for the AMP assess job.
#
# The controller's VoyagerJobRunner starts the job container with SOURCE_* /
# EXPORT_DIR / YB_VOYAGER_MIGRATION_UUID / CONTROL_PLANE_TYPE env (see
# controller/architecture/voyager/ac-voyager.md §5.2). This script maps those
# into the driver arguments and does not call yb-voyager assess-migration.

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)

: "${YB_VOYAGER_MIGRATION_UUID:?YB_VOYAGER_MIGRATION_UUID is required}"
: "${SOURCE_HOST:?SOURCE_HOST is required}"
: "${SOURCE_PORT:?SOURCE_PORT is required}"
: "${SOURCE_USER:?SOURCE_USER is required}"
: "${SOURCE_DB:?SOURCE_DB is required}"
: "${EXPORT_DIR:?EXPORT_DIR is required}"

# Discrete libpq knobs, not a URI: usernames like Azure's user@server and
# database names with / or ? are passed through intact.
export PGHOST="$SOURCE_HOST"
export PGPORT="$SOURCE_PORT"
export PGUSER="$SOURCE_USER"
export PGDATABASE="$SOURCE_DB"
export PGPASSWORD="${SOURCE_DB_PASSWORD:-}"

# Job env uses comma-separated schemas; the control-plane row uses pipes,
# matching voyager's schema_name convention.
SOURCE_SCHEMAS="${SOURCE_SCHEMAS:-public}"
SCHEMA_LIST="${SOURCE_SCHEMAS//,/$'|'}"

exec "$SCRIPT_DIR/amp-assess-migration.sh" --psql-env "$SCHEMA_LIST" "$EXPORT_DIR"
