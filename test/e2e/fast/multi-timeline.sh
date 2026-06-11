#!/usr/bin/env bash
#
# Two independent timelines (each its own tenant + co-located PS+SK pair)
# coexisting in the same shared yb_storage DB. Validates:
#
#   1. storage isolation — a same-named table on each timeline holds only
#      that timeline's rows; neither sees the other's data, even though
#      their pages + WAL live in the same YB blob/wal tables keyed by
#      (tenant_id, timeline_id).
#   2. independent lifecycle — dropping one timeline (`endpoint stop
#      --destroy`, which tears down its co-located PS+SK pair) leaves the
#      other timeline fully intact and serving.
#
# Each `tenant create` here also records its own pending Bootstrap init in
# the storage controller, so this doubles as coverage for concurrent
# pending inits across tenants (task 29).
#
# Run via test/e2e/run.sh — it provides $YB_LIGHT and $YB_LIGHT_REPO_DIR. This
# test uses its own tenants/endpoints and does not touch the shared `main`.

set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

step() { echo; printf "%(%H:%M:%S)T === %s ===\n" -1 "$*"; }
fail() { echo "FAIL — $*"; exit 1; }

A_PORT=55570
B_PORT=55580

new_tenant() { "$YB_LIGHT" tenant create --pg-version 17 2>&1 | awk '/successfully created/{print $2}'; }
psql_at() { local port=$1; shift; PGPASSWORD= psql -h 127.0.0.1 -p "$port" -U cloud_admin postgres -v ON_ERROR_STOP=1 -qtA "$@"; }

step "Create two independent timelines (tenant A, tenant B)"
TA=$(new_tenant); [[ -n $TA ]] || fail "tenant A create failed"
TB=$(new_tenant); [[ -n $TB ]] || fail "tenant B create failed"
echo "tenant A: $TA"
echo "tenant B: $TB"
[[ $TA != "$TB" ]] || fail "two tenant creates returned the same id"

step "Provision + start an endpoint on each (one co-located PS+SK pair per timeline)"
"$YB_LIGHT" endpoint create ep_a --tenant-id "$TA" --branch-name main --pg-version 17 --pg-port "$A_PORT" >/dev/null
"$YB_LIGHT" endpoint start ep_a >/dev/null
"$YB_LIGHT" endpoint create ep_b --tenant-id "$TB" --branch-name main --pg-version 17 --pg-port "$B_PORT" >/dev/null
"$YB_LIGHT" endpoint start ep_b >/dev/null

step "Write distinct data into the same-named table on each timeline"
psql_at "$A_PORT" <<'SQL'
DROP TABLE IF EXISTS island;
CREATE TABLE island (id INT PRIMARY KEY, owner TEXT NOT NULL);
INSERT INTO island SELECT g, 'A' FROM generate_series(1, 500) g;
CHECKPOINT;
SQL
psql_at "$B_PORT" <<'SQL'
DROP TABLE IF EXISTS island;
CREATE TABLE island (id INT PRIMARY KEY, owner TEXT NOT NULL);
INSERT INTO island SELECT g, 'B' FROM generate_series(1, 700) g;
CHECKPOINT;
SQL

step "Each timeline sees only its own rows"
A_CNT=$(psql_at "$A_PORT" -c "SELECT count(*) FROM island")
[[ $A_CNT == 500 ]] || fail "timeline A count $A_CNT, want 500"
A_FOREIGN=$(psql_at "$A_PORT" -c "SELECT count(*) FROM island WHERE owner <> 'A'")
[[ $A_FOREIGN == 0 ]] || fail "timeline A sees $A_FOREIGN non-A rows, want 0"

B_CNT=$(psql_at "$B_PORT" -c "SELECT count(*) FROM island")
[[ $B_CNT == 700 ]] || fail "timeline B count $B_CNT, want 700"
B_FOREIGN=$(psql_at "$B_PORT" -c "SELECT count(*) FROM island WHERE owner <> 'B'")
[[ $B_FOREIGN == 0 ]] || fail "timeline B sees $B_FOREIGN non-B rows, want 0"

step "Drop timeline A (deprovision its endpoint + co-located PS+SK pair)"
"$YB_LIGHT" endpoint stop ep_a --destroy >/dev/null
[[ ! -e $YB_LIGHT_REPO_DIR/endpoints/ep_a ]] || fail "endpoints/ep_a should be gone after --destroy"

step "Timeline B is unaffected by dropping A"
B_AFTER=$(psql_at "$B_PORT" -c "SELECT count(*) FROM island")
[[ $B_AFTER == 700 ]] || fail "timeline B count $B_AFTER after dropping A, want 700"
B_AFTER_OWN=$(psql_at "$B_PORT" -c "SELECT count(*) FROM island WHERE owner = 'B'")
[[ $B_AFTER_OWN == 700 ]] || fail "timeline B owns $B_AFTER_OWN after dropping A, want 700"

step "Cleanup: deprovision timeline B"
"$YB_LIGHT" endpoint stop ep_b --destroy >/dev/null 2>&1 || true

step "Final verdict"
echo "PASS — two timelines isolated in shared YB storage; dropping one left the other intact"
