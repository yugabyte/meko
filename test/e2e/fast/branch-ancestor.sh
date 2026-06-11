#!/usr/bin/env bash
#
# Branch data inheritance: a branch — and a branch-of-a-branch — reads its
# ANCESTOR's committed rows via lineage-aware reads against YB, with
# nothing copied at branch time. This complements branch-endpoint.sh
# (write isolation + restart) by checking:
#
#   1. value-level inheritance — the branch returns the ancestor's exact
#      row values and a whole-table checksum, not just a matching count.
#   2. multi-level lineage — a grandchild branch (b2 off b1 off main) sees
#      data contributed by BOTH ancestors.
#   3. ancestors are immutable w.r.t. their descendants' writes.
#
# Run via test/e2e/run.sh — it provides $YB_LIGHT, $TID, $COMPUTE_PORT and a
# running shared `main` endpoint (the lineage root here).

set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

step() { echo; printf "%(%H:%M:%S)T === %s ===\n" -1 "$*"; }
fail() { echo "FAIL — $*"; exit 1; }

B1_PORT=55590
B2_PORT=55600

psql_main() { PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT" -U cloud_admin postgres -v ON_ERROR_STOP=1 -qtA "$@"; }
psql_at()   { local port=$1; shift; PGPASSWORD= psql -h 127.0.0.1 -p "$port" -U cloud_admin postgres -v ON_ERROR_STOP=1 -qtA "$@"; }

step "Seed ancestor data on main (known values + whole-table checksum)"
psql_main <<'SQL'
DROP TABLE IF EXISTS lineage;
CREATE TABLE lineage (id INT PRIMARY KEY, payload TEXT NOT NULL);
INSERT INTO lineage SELECT g, md5(g::text) FROM generate_series(1, 2000) g;
CHECKPOINT;
SQL
ANC_SUM=$(psql_main -c "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM lineage")
ANC_42=$(psql_main -c "SELECT payload FROM lineage WHERE id = 42")
[[ -n $ANC_SUM ]] || fail "could not compute ancestor checksum"

step "Branch anc_b1 off main, provision + start its endpoint"
"$YB_LIGHT" timeline branch --tenant-id "$TID" --branch-name anc_b1
"$YB_LIGHT" endpoint create ep_anc_b1 --tenant-id "$TID" --branch-name anc_b1 --pg-version 17 --pg-port "$B1_PORT" >/dev/null
"$YB_LIGHT" endpoint start ep_anc_b1 >/dev/null

step "anc_b1 inherits the ancestor's rows — exact count, value, and checksum"
B1_CNT=$(psql_at "$B1_PORT" -c "SELECT count(*) FROM lineage")
[[ $B1_CNT == 2000 ]] || fail "anc_b1 sees $B1_CNT ancestor rows, want 2000"
B1_42=$(psql_at "$B1_PORT" -c "SELECT payload FROM lineage WHERE id = 42")
[[ $B1_42 == "$ANC_42" ]] || fail "anc_b1 row 42 payload '$B1_42' != ancestor '$ANC_42'"
B1_SUM=$(psql_at "$B1_PORT" -c "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM lineage")
[[ $B1_SUM == "$ANC_SUM" ]] || fail "anc_b1 whole-table checksum mismatch vs ancestor"

step "Write more on anc_b1, then branch anc_b2 off it (two-level lineage)"
psql_at "$B1_PORT" <<'SQL'
INSERT INTO lineage SELECT g, 'b1-' || g FROM generate_series(3001, 3100) g;
CHECKPOINT;
SQL
"$YB_LIGHT" timeline branch --tenant-id "$TID" --branch-name anc_b2 --ancestor-branch-name anc_b1
"$YB_LIGHT" endpoint create ep_anc_b2 --tenant-id "$TID" --branch-name anc_b2 --pg-version 17 --pg-port "$B2_PORT" >/dev/null
"$YB_LIGHT" endpoint start ep_anc_b2 >/dev/null

step "anc_b2 inherits from BOTH ancestors (main's 2000 + anc_b1's 100)"
B2_CNT=$(psql_at "$B2_PORT" -c "SELECT count(*) FROM lineage")
[[ $B2_CNT == 2100 ]] || fail "anc_b2 sees $B2_CNT rows, want 2100 (2000 from main + 100 from anc_b1)"
B2_42=$(psql_at "$B2_PORT" -c "SELECT payload FROM lineage WHERE id = 42")
[[ $B2_42 == "$ANC_42" ]] || fail "anc_b2 row 42 payload '$B2_42' != original ancestor '$ANC_42'"
B2_B1=$(psql_at "$B2_PORT" -c "SELECT count(*) FROM lineage WHERE payload LIKE 'b1-%'")
[[ $B2_B1 == 100 ]] || fail "anc_b2 sees $B2_B1 of anc_b1's rows, want 100"

step "Ancestors are immutable w.r.t. their descendants' writes"
MAIN_CNT=$(psql_main -c "SELECT count(*) FROM lineage")
[[ $MAIN_CNT == 2000 ]] || fail "main (root) count changed to $MAIN_CNT, want 2000"
B1_FINAL=$(psql_at "$B1_PORT" -c "SELECT count(*) FROM lineage")
[[ $B1_FINAL == 2100 ]] || fail "anc_b1 count $B1_FINAL, want 2100 (2000 inherited + 100 own)"

step "Cleanup: deprovision branch endpoints"
"$YB_LIGHT" endpoint stop ep_anc_b2 --destroy >/dev/null 2>&1 || true
"$YB_LIGHT" endpoint stop ep_anc_b1 --destroy >/dev/null 2>&1 || true

step "Final verdict"
echo "PASS — branch and grandchild inherited ancestor data (values + checksums) via lineage"
