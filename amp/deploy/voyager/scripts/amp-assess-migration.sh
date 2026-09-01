#!/bin/bash
# AMP assess driver. Replaces `yb-voyager assess-migration` for PG → AMP.
#
# AMP's compute is PostgreSQL, so voyager's YugabyteDB compatibility analysis
# (unsupported datatypes, PL/pgSQL, query constructs, colocation, sharding)
# does not apply. This reports complexity LOW and a vCPU recommendation matched
# to the source's own capacity, then writes the same yugabyted control-plane
# rows the AMP controller already reads.
#
# Usage:
#   amp-assess-migration.sh <pg_connection_string> <schema_list> <output_dir>
#   amp-assess-migration.sh --psql-env <schema_list> <output_dir>
#
# The image entrypoint assess.sh uses --psql-env so SOURCE_USER / SOURCE_DB
# are passed as discrete libpq fields, not interpolated into a URI.

set -euo pipefail

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
SCRIPT_NAME=$(basename "$0")

PAYLOAD_VERSION="1.8-amp.1"
VOYAGER_VERSION="amp-assess-1.0"
# AMP provisions on vCPU, but the payload's MemoryPerInstance is surfaced to the
# sizing agent (GET /migrations/{id}/assessment), so derive it from the detected
# vCPU count at 4 GB/vCPU rather than reporting a flat constant. DEFAULT_MEMORY_GB
# is the fallback used only when the source vCPU count cannot be determined.
MEMORY_GB_PER_VCPU=4
DEFAULT_MEMORY_GB=16

HELP_TEXT="
Usage: $SCRIPT_NAME <pg_connection_string> <schema_list> <output_dir>
       $SCRIPT_NAME --psql-env <schema_list> <output_dir>

Arguments:
  pg_connection_string   Source PostgreSQL URI, e.g.
                         'postgresql://username@hostname:port/dbname'
  --psql-env             Use PGHOST / PGPORT / PGUSER / PGDATABASE / PGPASSWORD
                         instead of a URI (what assess.sh does).
  schema_list            Pipe-separated schemas, e.g. 'public|sales'.
                         Recorded on the control-plane row; not otherwise used.
  output_dir             Directory for the local copy of the assessment report.

Environment:
  PGPASSWORD                  Source database password.
  PGHOST / PGPORT / PGUSER / PGDATABASE
                              Required with --psql-env.
  YB_VOYAGER_MIGRATION_UUID   Required. AMP migration.id, keys the control-plane rows.
  CONTROL_PLANE_TYPE          'yugabyted' to write control-plane rows. Anything
                              else (or unset) writes only the local report.
  YUGABYTED_DB_CONN_STRING    Required when CONTROL_PLANE_TYPE=yugabyted.
"

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ] || [ $# -lt 3 ]; then
    echo "$HELP_TEXT"
    [ $# -lt 3 ] && exit 1
    exit 0
fi

SOURCE_CONN=""
if [ "$1" = "--psql-env" ]; then
    : "${PGHOST:?PGHOST is required with --psql-env}"
    : "${PGPORT:?PGPORT is required with --psql-env}"
    : "${PGUSER:?PGUSER is required with --psql-env}"
    : "${PGDATABASE:?PGDATABASE is required with --psql-env}"
else
    SOURCE_CONN="$1"
fi
SCHEMA_LIST="$2"
OUTPUT_DIR="$3"

if [ -z "${YB_VOYAGER_MIGRATION_UUID:-}" ]; then
    echo "ERROR: YB_VOYAGER_MIGRATION_UUID must be set." >&2
    exit 1
fi

WRITE_CONTROL_PLANE=false
if [ "${CONTROL_PLANE_TYPE:-}" = "yugabyted" ]; then
    if [ -z "${YUGABYTED_DB_CONN_STRING:-}" ]; then
        echo "ERROR: YUGABYTED_DB_CONN_STRING must be set when CONTROL_PLANE_TYPE=yugabyted." >&2
        exit 1
    fi
    WRITE_CONTROL_PLANE=true
fi

# Start clean. A re-assessment reuses this migration's shared export volume
# (the controller keys one PVC/named volume per migration and mounts it at
# EXPORT_DIR), so a prior attempt's assessment output is already here. Drop it
# and regenerate from scratch, mirroring the --start-clean the migrate phases
# use: without this a re-run layers onto stale files (and a stock
# `yb-voyager assess-migration` would refuse the dirty dir outright). Scoped to
# assessment/ so a re-assessment never disturbs a migrate's exported
# schema/data living elsewhere on the same volume.
rm -rf "${OUTPUT_DIR:?}/assessment"
mkdir -p "$OUTPUT_DIR/assessment/reports"
OUTPUT_DIR=$( cd "$OUTPUT_DIR" && pwd )
REPORT_PATH="$OUTPUT_DIR/assessment/reports/migration_assessment_report.json"

src() {
    if [ -n "$SOURCE_CONN" ]; then
        psql -X -q -tA "$SOURCE_CONN" "$@"
    else
        psql -X -q -tA -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" "$@"
    fi
}
cp_()  { psql -X -q -tA "$YUGABYTED_DB_CONN_STRING" "$@"; }

# --- source facts -------------------------------------------------------------

echo "Collecting source database facts..."
FACTS=$(src -v ON_ERROR_STOP=on -f "$SCRIPT_DIR/amp-source-facts.psql")
if [ -z "$FACTS" ]; then
    echo "ERROR: amp-source-facts.psql returned no row." >&2
    exit 1
fi
IFS='|' read -r IS_SUPERUSER HAS_READ_ROLE HAS_READ_EXEC \
                MAX_WORKER_PROCESSES MAX_PARALLEL_WORKERS \
                DB_NAME DB_VERSION SERVER_ADDR SERVER_PORT DB_SIZE <<< "$FACTS"

echo "  database=$DB_NAME version=$DB_VERSION"

# --- control plane: bootstrap and the IN PROGRESS row -------------------------

# Mirrors voyager: the started row is written before any assessment work, and
# the completed row gets the next invocation_sequence rather than updating it.
if [ "$WRITE_CONTROL_PLANE" = true ]; then
    echo "Bootstrapping control-plane schema..."
    cp_ -v ON_ERROR_STOP=on -f "$SCRIPT_DIR/amp-controlplane-bootstrap.psql" > /dev/null

    # psql interpolates :'var' only for script input, never for -c.
    SEQ=$(cp_ -v ON_ERROR_STOP=on -v uuid="$YB_VOYAGER_MIGRATION_UUID" <<'SQL'
SELECT coalesce(MAX(invocation_sequence), 0) + 1
  FROM ybvoyager_visualizer.ybvoyager_visualizer_metadata
 WHERE migration_uuid = :'uuid' AND migration_phase = 1;
SQL
)

    # host_ip/port record the source we connected to. Prefer the connection's
    # own PGHOST/PGPORT — always set in --psql-env mode (what the controller
    # uses) and correct behind a unix socket where inet_server_addr() is NULL.
    # Fall back to the server-reported address/port from the facts query only
    # for raw-URI invocations, where the shell has no PGHOST/PGPORT to read.
    SOURCE_ADDR="${PGHOST:-$SERVER_ADDR}"
    SOURCE_PORT="${PGPORT:-$SERVER_PORT}"
    HOST_IP_JSON="{\"SourceDBIP\":\"${SOURCE_ADDR}\"}"
    LOCAL_IP=$(hostname -i 2>/dev/null | awk '{print $1}' || true)
    [ -z "$LOCAL_IP" ] && LOCAL_IP="127.0.0.1"
    DISK_AVAIL=$(df -Pk "$OUTPUT_DIR" 2>/dev/null | awk 'NR==2 {print $4 * 1024}' || true)
    [ -z "$DISK_AVAIL" ] && DISK_AVAIL=0
    VOYAGER_INFO_JSON=$(printf '{"IP":"%s","OperatingSystem":"%s","DiskSpaceAvailable":%s,"ExportDirectory":"%s"}' \
        "$LOCAL_IP" "$(uname -s | tr 'A-Z' 'a-z')" "$DISK_AVAIL" "$OUTPUT_DIR")

    insert_cp_row() {   # $1 = invocation_sequence, $2 = status, $3 = payload
        cp_ -v ON_ERROR_STOP=on \
            -v uuid="$YB_VOYAGER_MIGRATION_UUID" -v seq="$1" -v status="$2" -v payload="$3" \
            -v mdir="$OUTPUT_DIR" -v dbname="$DB_NAME" -v schemas="$SCHEMA_LIST" \
            -v hostip="$HOST_IP_JSON" -v port="$SOURCE_PORT" -v dbver="$DB_VERSION" \
            -v vinfo="$VOYAGER_INFO_JSON" <<'SQL' > /dev/null
INSERT INTO ybvoyager_visualizer.ybvoyager_visualizer_metadata (
    migration_uuid, migration_phase, invocation_sequence, migration_dir,
    database_name, schema_name, host_ip, port, db_version, payload,
    voyager_info, db_type, status, invocation_timestamp
) VALUES (
    :'uuid', 1, :'seq'::int, :'mdir', :'dbname', :'schemas', :'hostip',
    :'port'::int, :'dbver', nullif(:'payload', ''), :'vinfo', 'postgresql', :'status', now()
)
ON CONFLICT (migration_uuid, migration_phase, invocation_sequence) DO UPDATE SET
    migration_dir = EXCLUDED.migration_dir,
    database_name = EXCLUDED.database_name,
    schema_name = EXCLUDED.schema_name,
    host_ip = EXCLUDED.host_ip,
    port = EXCLUDED.port,
    db_version = EXCLUDED.db_version,
    payload = EXCLUDED.payload,
    voyager_info = EXCLUDED.voyager_info,
    db_type = EXCLUDED.db_type,
    status = EXCLUDED.status,
    invocation_timestamp = EXCLUDED.invocation_timestamp;
SQL
    }

    # Voyager's own control plane emits no failure event (ac-voyager.md "Known
    # gaps"): a crash between the IN PROGRESS and COMPLETED rows would otherwise
    # leave this run stuck at IN PROGRESS forever, since failure is only inferred
    # from the job exit code. Record a terminal FAILED row on any abnormal exit
    # so the read path sees a terminal state. Best-effort — it must never mask
    # the original failure or alter its exit code.
    STARTED=false
    record_failure_on_exit() {
        local code=$?
        trap - EXIT
        if [ "$code" -ne 0 ] && [ "$STARTED" = true ]; then
            echo "Assessment failed (exit $code); recording FAILED status row..." >&2
            insert_cp_row "$(( SEQ + 1 ))" "FAILED" "" || true
        fi
        exit "$code"
    }
    trap record_failure_on_exit EXIT

    echo "Recording assessment start (invocation_sequence=$SEQ)..."
    insert_cp_row "$SEQ" "IN PROGRESS" ""
    STARTED=true
fi

# --- vCPU detection -----------------------------------------------------------
#
#   exact  read the source host's real CPU limits through pg_read_file
#   high   invert a vCPU-derived GUC. PostgreSQL ships max_worker_processes and
#          max_parallel_workers at exactly 8, so a value above 8 means something
#          sized them from the machine, using the documented formulas
#          max_worker_processes = GREATEST(vCPU*2,8) and
#          max_parallel_workers = GREATEST(vCPU/2,8).
#   -1     no signal. The caller decides what to do.

VCPUS=-1
CONFIDENCE="unknown"
DETECTION_METHOD="none"

count_cpu_list() {   # "0-3,8,10-11" -> 7
    local total=0 part lo hi
    [ -z "$1" ] && { echo 0; return; }
    local IFS=','
    local -a parts
    read -ra parts <<< "$1"
    for part in "${parts[@]}"; do
        if [[ "$part" == *-* ]]; then
            lo=${part%-*}; hi=${part#*-}
            total=$(( total + hi - lo + 1 ))
        else
            total=$(( total + 1 ))
        fi
    done
    echo "$total"
}

if [ "$IS_SUPERUSER" = "on" ] || { [ "$HAS_READ_ROLE" = "true" ] && [ "$HAS_READ_EXEC" = "true" ]; }; then
    echo "Reading source host CPU limits..."
    # We are past the capability gate (superuser, or pg_read_server_files +
    # EXECUTE on pg_read_file), so a failure here is not a permission problem —
    # it is something unexpected (a bad read, a regex/SQL error). Capture and
    # surface stderr instead of discarding it with 2>/dev/null, then still
    # degrade to the parameter-inference tier rather than failing the run.
    OS_CAP_ERR=$(mktemp)
    if OS_CAPACITY=$(src -v ON_ERROR_STOP=on -f "$SCRIPT_DIR/amp-source-os-capacity.psql" 2>"$OS_CAP_ERR"); then
        IFS='|' read -r CPU_MAX CPUS_ALLOWED CPUINFO_COUNT <<< "$OS_CAPACITY"

        DETECTED=$(count_cpu_list "$CPUS_ALLOWED")
        METHOD="Cpus_allowed_list"
        if [ "$DETECTED" -eq 0 ] && [ -n "${CPUINFO_COUNT:-}" ]; then
            DETECTED=$CPUINFO_COUNT
            METHOD="/proc/cpuinfo"
        fi

        QUOTA=${CPU_MAX%% *}
        PERIOD=${CPU_MAX##* }
        if [ -n "$QUOTA" ] && [ "$QUOTA" != "max" ] && [ "${PERIOD:-0}" -gt 0 ] 2>/dev/null; then
            QUOTA_CPUS=$(( (QUOTA + PERIOD - 1) / PERIOD ))
            if [ "$QUOTA_CPUS" -gt 0 ] && { [ "$DETECTED" -eq 0 ] || [ "$QUOTA_CPUS" -lt "$DETECTED" ]; }; then
                DETECTED=$QUOTA_CPUS
                METHOD="cgroup cpu.max"
            fi
        fi

        if [ "$DETECTED" -gt 0 ]; then
            VCPUS=$DETECTED
            CONFIDENCE="exact"
            DETECTION_METHOD="$METHOD"
        fi
    else
        echo "  host read failed; falling back to parameter inference" >&2
        if [ -s "$OS_CAP_ERR" ]; then
            sed 's/^/    /' "$OS_CAP_ERR" >&2
        fi
    fi
    rm -f "$OS_CAP_ERR"
fi

# Two GUCs are checked, in order, because neither is universally usable:
#   - max_parallel_workers only exists from PG 9.6 (older sources report 0 via
#     the facts coalesce), and the two can be tuned independently, so we fall
#     back to the second when the first is at its default.
#   - Both ship at a default of 8, and the sizing formulas are
#     max_worker_processes = GREATEST(vCPU*2, 8) and
#     max_parallel_workers  = GREATEST(vCPU/2, 8). A source with <= 4 vCPU that
#     never tuned these is therefore indistinguishable from the default floor:
#     both GUCs read exactly 8, fail the `> 8` test, and fall through to
#     -1/unknown. That is intentional — we report "unknown" and let the caller
#     decide rather than guess a small vCPU count we cannot actually confirm.
if [ "$VCPUS" -le 0 ]; then
    if [ "${MAX_WORKER_PROCESSES:-0}" -gt 8 ]; then
        VCPUS=$(( MAX_WORKER_PROCESSES / 2 ))
        CONFIDENCE="high"
        DETECTION_METHOD="max_worker_processes/2"
    elif [ "${MAX_PARALLEL_WORKERS:-0}" -gt 8 ]; then
        VCPUS=$(( MAX_PARALLEL_WORKERS * 2 ))
        CONFIDENCE="high"
        DETECTION_METHOD="max_parallel_workers*2"
    fi
fi

if [ "$VCPUS" -le 0 ]; then
    echo "  could not determine source vCPU count; reporting -1"
else
    echo "  source vCPUs: $VCPUS (confidence=$CONFIDENCE via $DETECTION_METHOD)"
fi

# --- payload ------------------------------------------------------------------

# json_build_object, not jsonb_build_object: jsonb reorders keys, json preserves
# insertion order so the payload matches Go's marshal order field for field.
if [ "$VCPUS" -gt 0 ]; then
    RECOMMENDED_MEMORY_GB=$(( VCPUS * MEMORY_GB_PER_VCPU ))
    SIZING_REASONING="Recommended AMP compute matched to source capacity: ${VCPUS} vCPU / ${RECOMMENDED_MEMORY_GB} GB (confidence: ${CONFIDENCE}, via ${DETECTION_METHOD})."
    FAILURE_REASONING=""
else
    RECOMMENDED_MEMORY_GB=$DEFAULT_MEMORY_GB
    SIZING_REASONING="Source vCPU count could not be determined; VCPUsPerInstance is reported as -1."
    FAILURE_REASONING="Unable to determine source vCPU count from the source database."
fi

echo "Building assessment payload..."
PAYLOAD=$(src -v ON_ERROR_STOP=on \
    -v pver="$PAYLOAD_VERSION" -v vver="$VOYAGER_VERSION" \
    -v vcpus="$VCPUS" -v memgb="$RECOMMENDED_MEMORY_GB" \
    -v dbsize="$DB_SIZE" -v reasoning="$SIZING_REASONING" -v failure="$FAILURE_REASONING" \
    <<'SQL'
-- PG → AMP is PostgreSQL → PostgreSQL, so YugabyteDB-specific fields
-- (schema/conversion analysis, colocation/sharding, multi-node cluster sizing,
-- YB import-time modelling) are omitted. What remains is what the AMP controller
-- actually reads: complexity, source size, and the vCPU/memory recommendation.
-- The vCPU detection method and confidence are surfaced in the notes below.
SELECT json_build_object(
    'PayloadVersion', :'pver',
    'VoyagerVersion', :'vver',
    'MigrationComplexity', 'LOW',
    'MigrationComplexityExplanation',
        'YugabyteDB AMP is a PostgreSQL-compatible compute, so a PostgreSQL source needs no schema conversion. This assessment reports sizing only; it does not analyse the schema for incompatibilities.',
    'SourceSizeDetails', json_build_object(
        'TotalDBSize', :'dbsize'::bigint),
    'Sizing', json_build_object(
        'SizingRecommendation', json_build_object(
            'NumNodes', 1,
            'VCPUsPerInstance', :'vcpus'::int,
            'MemoryPerInstance', :'memgb'::int),
        'FailureReasoning', :'failure'),
    'GeneralNotes', json_build_array(
        'Generated by AMP assess scripts, not by yb-voyager assess-migration. Schema compatibility, datatypes, extensions and query constructs were not analysed.'),
    'SizingNotes', json_build_array(:'reasoning'),
    'Notes', json_build_array(:'reasoning')
);
SQL
)

printf '%s\n' "$PAYLOAD" > "$REPORT_PATH"
echo "Assessment report written to $REPORT_PATH"

# --- control plane: the COMPLETED row -----------------------------------------

if [ "$WRITE_CONTROL_PLANE" = true ]; then
    echo "Recording assessment completion (invocation_sequence=$(( SEQ + 1 )))..."
    insert_cp_row "$(( SEQ + 1 ))" "COMPLETED" "$PAYLOAD"
fi

echo "Done."
