#!/usr/bin/env bash
#
# End-to-end verification of the co-located pageserver/safekeeper
# topology: a branch gets a NEW pageserver + safekeeper pair, provisioned
# under the branch's endpoint at `endpoint create` (not at `timeline
# branch`), and the branch endpoint serves
#
#   1. pre-branch-point data — via lineage-aware reads against the
#      ANCESTOR's rows in YB (nothing is copied at branch time), and
#   2. post-branch writes — isolated from the ancestor in both
#      directions.
#
# Run via test/e2e/run.sh — it provides $YB_LIGHT, $YB_LIGHT_REPO_DIR,
# $COMPUTE_PORT, $TID and a fresh cluster + running `main` endpoint.

set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

step() { echo; printf "%(%H:%M:%S)T === %s ===\n" -1 "$*"; }

BRANCH_PG_PORT=55532

psql_main()   { PGPASSWORD= psql -h 127.0.0.1 -p "$COMPUTE_PORT"   -U cloud_admin postgres -v ON_ERROR_STOP=1 -qtA "$@"; }
psql_branch() { PGPASSWORD= psql -h 127.0.0.1 -p "$BRANCH_PG_PORT" -U cloud_admin postgres -v ON_ERROR_STOP=1 -qtA "$@"; }

step "Seed pre-branch data on main"
# The branch compute reads these seed transactions' CLOG (SLRU) state
# straight from the pageserver, so the run also covers early-window
# CLOG-delta persistence.
psql_main <<'SQL'
DROP TABLE IF EXISTS branch_seed;
CREATE TABLE branch_seed (id INT PRIMARY KEY, val TEXT NOT NULL);
INSERT INTO branch_seed SELECT g, 'pre-branch-' || g FROM generate_series(1, 1000) g;
CHECKPOINT;
SQL
PRE_COUNT=$(psql_main -c "SELECT count(*) FROM branch_seed")
[[ $PRE_COUNT == 1000 ]] || { echo "FAIL — seed count $PRE_COUNT != 1000"; exit 1; }

step "Create branch b1 (records the branch point; PS/SK come up at endpoint create)"
"$YB_LIGHT" timeline branch --tenant-id "$TID" --branch-name b1

step "Write post-branch data on main (must NOT be visible on b1)"
psql_main -c "INSERT INTO branch_seed SELECT g, 'post-branch-main-' || g FROM generate_series(1001, 1100) g" >/dev/null

step "Create + start endpoint on b1 (provisions pageserver 2 + safekeeper 2)"
"$YB_LIGHT" endpoint create ep_b1 --tenant-id "$TID" --branch-name b1 \
    --pg-version 17 --pg-port "$BRANCH_PG_PORT" >/dev/null
"$YB_LIGHT" endpoint start ep_b1 >/dev/null

step "Branch sees pre-branch data via lineage reads"
B1_PRE=$(psql_branch -c "SELECT count(*) FROM branch_seed WHERE val LIKE 'pre-branch-%'")
[[ $B1_PRE == 1000 ]] || { echo "FAIL — branch sees $B1_PRE pre-branch rows, want 1000"; exit 1; }

step "Branch does NOT see main's post-branch writes"
B1_POST_MAIN=$(psql_branch -c "SELECT count(*) FROM branch_seed WHERE val LIKE 'post-branch-main-%'")
[[ $B1_POST_MAIN == 0 ]] || { echo "FAIL — branch sees $B1_POST_MAIN of main's post-branch rows, want 0"; exit 1; }

step "Branch writes are isolated from main"
psql_branch -c "INSERT INTO branch_seed SELECT g, 'post-branch-b1-' || g FROM generate_series(2001, 2100) g" >/dev/null
B1_OWN=$(psql_branch -c "SELECT count(*) FROM branch_seed WHERE val LIKE 'post-branch-b1-%'")
[[ $B1_OWN == 100 ]] || { echo "FAIL — branch sees $B1_OWN of its own rows, want 100"; exit 1; }
MAIN_B1=$(psql_main -c "SELECT count(*) FROM branch_seed WHERE val LIKE 'post-branch-b1-%'")
[[ $MAIN_B1 == 0 ]] || { echo "FAIL — main sees $MAIN_B1 branch rows, want 0"; exit 1; }
MAIN_TOTAL=$(psql_main -c "SELECT count(*) FROM branch_seed")
[[ $MAIN_TOTAL == 1100 ]] || { echo "FAIL — main total $MAIN_TOTAL, want 1100"; exit 1; }

step "Branch survives a pageserver restart (open-existing + lineage rebuild)"
"$YB_LIGHT" pageserver stop --id 2
"$YB_LIGHT" pageserver start --id 2 >/dev/null
B1_AFTER_RESTART=$(psql_branch -c "SELECT count(*) FROM branch_seed")
[[ $B1_AFTER_RESTART == 1100 ]] || { echo "FAIL — branch sees $B1_AFTER_RESTART rows after PS restart, want 1100 (1000 pre + 100 own)"; exit 1; }

step "Cleanup: stop branch endpoint (branch PS/SK keep running; suite teardown stops them)"
"$YB_LIGHT" endpoint stop ep_b1 --mode immediate >/dev/null 2>&1 || true

step "Final verdict"
echo "PASS — branch endpoint served lineage reads, isolated writes, and survived a restart"
