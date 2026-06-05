#!/usr/bin/env bash
# Load 100 M rows into big_table on the target. Uses chunked INSERT to
# avoid one giant transaction (which would blow up WAL/PS memory on ybLight).
# Usage: ./big-load.sh ybLight|aurora [rows] [chunk]
#   rows  : total rows to load (default 100000000)
#   chunk : rows per INSERT statement (default 1000000)

set -euo pipefail
target="${1:?usage: $0 ybLight|aurora [rows] [chunk]}"; shift || true
rows="${1:-100000000}"; shift || true
chunk="${1:-1000000}"; shift || true

source ~/benchmark/scripts/_env.sh "$target"

DBNAME=bigbench

# (Re)create DB
$PSQL -d $PGDATABASE -c "DROP DATABASE IF EXISTS $DBNAME;" >/dev/null
$PSQL -d $PGDATABASE -c "CREATE DATABASE $DBNAME;" >/dev/null

# Schema
$PSQL -d $DBNAME -v ON_ERROR_STOP=1 -f ~/benchmark/sql/big-load-schema.sql

n_chunks=$(( (rows + chunk - 1) / chunk ))
echo "[load] target=$target rows=$rows chunk=$chunk → $n_chunks chunks"
start_ts=$(date +%s)
for ((i=0; i<n_chunks; i++)); do
  lo=$(( i*chunk + 1 ))
  hi=$(( (i+1)*chunk ))
  [[ $hi -gt $rows ]] && hi=$rows
  $PSQL -d $DBNAME -v ON_ERROR_STOP=1 -c "
    INSERT INTO big_table
    SELECT g, (g * 7) % 10000 + 1, g, repeat(md5(g::text), 5)::bytea, now()
    FROM generate_series($lo, $hi) g;
  " >/dev/null
  elapsed=$(($(date +%s) - start_ts))
  pct=$(( (i+1) * 100 / n_chunks ))
  printf "  chunk %3d/%d  rows=[%d..%d]  elapsed=%ds  (%d%%)\n" \
    "$((i+1))" "$n_chunks" "$lo" "$hi" "$elapsed" "$pct"
done

echo "[load] ANALYZE..."
$PSQL -d $DBNAME -c "ANALYZE big_table;" >/dev/null
echo "[load] done in $(( $(date +%s) - start_ts ))s"

# Report sizes
$PSQL -d $DBNAME -c "
SELECT pg_size_pretty(pg_total_relation_size('big_table')) AS total,
       pg_size_pretty(pg_relation_size('big_table'))       AS heap,
       pg_size_pretty(pg_indexes_size('big_table'))        AS indexes,
       (SELECT count(*) FROM big_table)                    AS rows;"
