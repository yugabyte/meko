#!/usr/bin/env bash
#
# Pins the on-disk layout contract of the co-located topology: a
# timeline's pageserver + safekeeper live under the endpoint that owns
# them (`endpoints/<endpoint>/{pageserver,safekeeper}`), and the old
# top-level `pageserver_<id>/` + `safekeepers/sk<id>/` dirs are gone.
#
# Asserts on the shared cluster (main is already up), then drives a
# self-contained branch → endpoint create → destroy cycle to check that
# the branch's pair appears under its endpoint and is torn down on
# `endpoint stop --destroy` (the deprovision path).
#
# Run via test/e2e/run.sh — it provides $NEON, $YB_LIGHT_REPO_DIR, $TID.

set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

step() { echo; printf "%(%H:%M:%S)T === %s ===\n" -1 "$*"; }
fail() { echo "FAIL — $*"; exit 1; }

R=$YB_LIGHT_REPO_DIR
LAYOUT_BRANCH=layout_b
LAYOUT_EP=ep_layout
LAYOUT_PG_PORT=55560

step "main timeline's pair is co-located under endpoints/main/"
[[ -f $R/endpoints/main/pageserver/pageserver.toml ]] \
    || fail "missing endpoints/main/pageserver/pageserver.toml"
[[ -f $R/endpoints/main/pageserver/identity.toml ]] \
    || fail "missing endpoints/main/pageserver/identity.toml"
[[ -f $R/endpoints/main/pageserver/pageserver.pid ]] \
    || fail "missing endpoints/main/pageserver/pageserver.pid"
[[ -f $R/endpoints/main/safekeeper/safekeeper.pid ]] \
    || fail "missing endpoints/main/safekeeper/safekeeper.pid"

step "no legacy top-level pageserver_*/ or safekeepers/ dirs"
shopt -s nullglob
legacy_ps=("$R"/pageserver_*)
shopt -u nullglob
[[ ${#legacy_ps[@]} -eq 0 ]] || fail "found legacy top-level dirs: ${legacy_ps[*]}"
[[ ! -d $R/safekeepers ]] || fail "found legacy top-level safekeepers/ dir"

step "branch $LAYOUT_BRANCH (no new on-disk dirs yet)"
"$NEON" timeline branch --tenant-id "$TID" --branch-name "$LAYOUT_BRANCH"
[[ ! -e $R/endpoints/$LAYOUT_EP ]] \
    || fail "endpoints/$LAYOUT_EP should not exist before endpoint create"

step "endpoint create $LAYOUT_EP provisions the branch's pair under its folder"
"$NEON" endpoint create "$LAYOUT_EP" --tenant-id "$TID" \
    --branch-name "$LAYOUT_BRANCH" --pg-version 17 --pg-port "$LAYOUT_PG_PORT" >/dev/null
[[ -f $R/endpoints/$LAYOUT_EP/pageserver/pageserver.toml ]] \
    || fail "missing endpoints/$LAYOUT_EP/pageserver/pageserver.toml"
[[ -f $R/endpoints/$LAYOUT_EP/safekeeper/safekeeper.pid ]] \
    || fail "missing endpoints/$LAYOUT_EP/safekeeper/safekeeper.pid"
# pgdata is only laid down on `endpoint start`, not at create time.
[[ ! -e $R/endpoints/$LAYOUT_EP/pgdata ]] \
    || fail "pgdata should not exist before endpoint start"
# The branch's pageserver got a distinct id from main's.
main_id=$(grep -oE 'id *= *[0-9]+' "$R/endpoints/main/pageserver/identity.toml" | grep -oE '[0-9]+')
br_id=$(grep -oE 'id *= *[0-9]+' "$R/endpoints/$LAYOUT_EP/pageserver/identity.toml" | grep -oE '[0-9]+')
[[ $main_id != "$br_id" ]] || fail "branch pageserver id $br_id collides with main's"

step "endpoint stop --destroy tears the pair down (deprovision)"
"$NEON" endpoint stop "$LAYOUT_EP" --destroy >/dev/null
[[ ! -e $R/endpoints/$LAYOUT_EP ]] \
    || fail "endpoints/$LAYOUT_EP should be gone after --destroy"

step "Final verdict"
echo "PASS — co-located layout contract holds (main pair, branch provisioning, deprovision)"
