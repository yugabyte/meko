#!/usr/bin/env bash
# Drive microbench.sql N times against a Postgres-compatible URL, capture
# psql \timing output, then parse a best-of-N table.
#
# Usage:
#   benchmark/scripts/microbenchmarks/run_microbench.sh <psql_url> [runs=3] [outdir=/tmp/microbench]
#
# Example:
#   benchmark/scripts/microbenchmarks/run_microbench.sh 'postgresql://cloud_admin@127.0.0.1:55432/postgres'
#
# Picks `psql` from $PSQL if set, else pg_install/v18/bin/psql, else $PATH.

set -euo pipefail

URL="${1:?usage: $0 <psql_url> [runs=3] [outdir=/tmp/microbench]}"
RUNS="${2:-3}"
OUTDIR="${3:-/tmp/microbench}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SQL="$SCRIPT_DIR/microbench.sql"
PARSE="$SCRIPT_DIR/parse_results.py"

if [[ -z "${PSQL:-}" ]]; then
    if [[ -x "$SCRIPT_DIR/../../../pg_install/v18/bin/psql" ]]; then
        PSQL="$SCRIPT_DIR/../../../pg_install/v18/bin/psql"
    else
        PSQL="psql"
    fi
fi

mkdir -p "$OUTDIR"
for i in $(seq 1 "$RUNS"); do
    out="$OUTDIR/run${i}.txt"
    echo "##### RUN $i -> $out #####" >&2
    "$PSQL" "$URL" -X -q -f "$SQL" 2>&1 \
        | grep -E '^===|^Time:' \
        > "$out"
done

python3 "$PARSE" "$OUTDIR"/run*.txt
