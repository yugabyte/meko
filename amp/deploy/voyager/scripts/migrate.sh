#!/bin/bash
# Image entrypoint for the AMP migrate job.
#
# The controller's VoyagerJobRunner starts the job container with SOURCE_* /
# TARGET_* / EXPORT_DIR / PARALLEL_JOBS / YB_VOYAGER_MIGRATION_UUID env (see
# controller/architecture/voyager/ac-voyager.md §5.2). This script runs the
# stock yb-voyager export/import sequence. It does not replace those commands
# the way assess.sh replaces yb-voyager assess-migration.

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)

: "${YB_VOYAGER_MIGRATION_UUID:?YB_VOYAGER_MIGRATION_UUID is required}"
: "${SOURCE_HOST:?SOURCE_HOST is required}"
: "${SOURCE_PORT:?SOURCE_PORT is required}"
: "${SOURCE_USER:?SOURCE_USER is required}"
: "${SOURCE_DB:?SOURCE_DB is required}"
: "${EXPORT_DIR:?EXPORT_DIR is required}"
: "${TARGET_HOST:?TARGET_HOST is required}"
: "${TARGET_PORT:?TARGET_PORT is required}"
: "${TARGET_USER:?TARGET_USER is required}"
: "${TARGET_DB:?TARGET_DB is required}"

SOURCE_DB_TYPE="${SOURCE_DB_TYPE:-postgresql}"
SOURCE_SCHEMAS="${SOURCE_SCHEMAS:-public}"
PARALLEL_JOBS="${PARALLEL_JOBS:-4}"

# yb-voyager reads these; do not put them on the argv.
export SOURCE_DB_PASSWORD="${SOURCE_DB_PASSWORD:-}"
export TARGET_DB_PASSWORD="${TARGET_DB_PASSWORD:-}"

# Every phase runs with --start-clean because a retry reuses this migration's
# resources. The controller keys one PVC per migration (voyager-exp-<id>) and
# mounts it at $EXPORT_DIR, so a re-run finds the prior attempt's yb-voyager
# state already there; without --start-clean, `export schema`/`export data`
# refuse ("data directory is not empty, use --start-clean flag"). The target is
# likewise provisioned once per migration, so a re-run's target may hold a prior
# attempt's partial objects/rows. --start-clean resets both: export re-runs from
# scratch, `import schema` drops the target schema (--yes auto-confirms) and
# re-imports, and `import data --truncate-tables` clears the tables before
# re-loading — so an attempt is a full, deterministic redo, not a resume. It is
# harmless on a first, clean run: export ignores it on an empty dir, and dropping
# an empty target schema is a no-op.
# The assess job shares this migration's export dir, so its assessment report
# sits at $EXPORT_DIR/assessment/reports/migration_assessment_report.json —
# exactly where `export schema` looks by default. Left to its own devices it
# would parse that report and apply YugabyteDB colocation/sharding
# recommendations to the exported DDL, which is meaningless (and undesirable)
# for a PostgreSQL-compatible yugabytedb-amp target. Skip that step explicitly
# rather than relying on our lean payload happening to carry no recommendations.
echo "Exporting schema from ${SOURCE_HOST}:${SOURCE_PORT}/${SOURCE_DB}..."
yb-voyager export schema \
  --source-db-type "$SOURCE_DB_TYPE" \
  --source-db-host "$SOURCE_HOST" --source-db-port "$SOURCE_PORT" \
  --source-db-user "$SOURCE_USER" --source-db-name "$SOURCE_DB" \
  --source-db-schema "$SOURCE_SCHEMAS" \
  --skip-colocation-recommendations true \
  --export-dir "$EXPORT_DIR" --start-clean true --yes

echo "Ensuring target database ${TARGET_DB} exists..."
PGPASSWORD="$TARGET_DB_PASSWORD" psql -X \
  -h "$TARGET_HOST" -p "$TARGET_PORT" -U "$TARGET_USER" -d postgres \
  -v ON_ERROR_STOP=1 -v target_db="$TARGET_DB" \
  -f "$SCRIPT_DIR/amp-create-target-db.psql"

echo "Importing schema into ${TARGET_HOST}:${TARGET_PORT}/${TARGET_DB}..."
yb-voyager import schema \
  --target-db-type yugabytedb-amp \
  --target-db-host "$TARGET_HOST" --target-db-port "$TARGET_PORT" \
  --target-db-user "$TARGET_USER" --target-db-name "$TARGET_DB" \
  --export-dir "$EXPORT_DIR" --start-clean true --yes

echo "Exporting data from ${SOURCE_HOST}:${SOURCE_PORT}/${SOURCE_DB}..."
yb-voyager export data \
  --source-db-type "$SOURCE_DB_TYPE" \
  --source-db-host "$SOURCE_HOST" --source-db-port "$SOURCE_PORT" \
  --source-db-user "$SOURCE_USER" --source-db-name "$SOURCE_DB" \
  --source-db-schema "$SOURCE_SCHEMAS" \
  --export-dir "$EXPORT_DIR" --start-clean true --yes

echo "Importing data into ${TARGET_HOST}:${TARGET_PORT}/${TARGET_DB}..."
yb-voyager import data \
  --target-db-type yugabytedb-amp \
  --target-db-host "$TARGET_HOST" --target-db-port "$TARGET_PORT" \
  --target-db-user "$TARGET_USER" --target-db-name "$TARGET_DB" \
  --parallel-jobs "$PARALLEL_JOBS" \
  --export-dir "$EXPORT_DIR" --start-clean true --truncate-tables true --yes

echo "Finalizing schema (indexes, triggers) on the target..."
yb-voyager finalize-schema-post-data-import \
  --target-db-type yugabytedb-amp \
  --target-db-host "$TARGET_HOST" --target-db-port "$TARGET_PORT" \
  --target-db-user "$TARGET_USER" --target-db-name "$TARGET_DB" \
  --export-dir "$EXPORT_DIR" --yes

echo "Done."
