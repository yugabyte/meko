#!/usr/bin/env bash
#
# Run an externalized-extension end-to-end test against a LOCAL yb_amp_local
# deploy — the local-deploy twin of the SMOKE_EXTENSIONS block in
# deploy/smoke-test.sh (which runs the same check against a GKE cluster).
#
# Local deploy runs the compute as a bare PG18 process (compute-ctl), so
# there is no Kubernetes ImageVolume: the Docker/local deployers ignore
# endpoint.extensions (compute/architecture/extensions.md). But the "mount" is
# only two PG18 GUCs, and `yb_amp_local endpoint create` already accepts
# arbitrary GUCs via --pg-setting. So this driver reproduces the chart's
# behaviour locally:
#
#   1. Build the extension image with the real docker pipeline
#      (deploy/extensions/build.sh -> quay.io/.../amp-ext-<name>:<version>).
#   2. Extract its FROM-scratch tree (share/ + lib/) to a host temp dir.
#   3. Bring up a throwaway yb_amp_local cluster (compute -> pageserver /
#      safekeeper -> YugabyteDB).
#   4. Create an endpoint whose extension_control_path / dynamic_library_path
#      point at the extracted tree, start it.
#   5. CREATE EXTENSION and run a real query, then tear the cluster down.
#
# The docker-built .so loads into the local compute because both compile from
# the same compute/vendor/postgres-v18 submodule (matching PG_MODULE_MAGIC) —
# the ABI-anchor guarantee the build pipeline is built around.
#
# Usage:
#   test/integration/compute/extensions.sh                  # pgvector, release
#   test/integration/compute/extensions.sh --ext pgvector   # explicit
#   test/integration/compute/extensions.sh --ext hexabridge # needs AMP_EXT_GH_TOKEN
#   test/integration/compute/extensions.sh --debug          # debug cluster build
#   test/integration/compute/extensions.sh --registry my    # override image registry
#   test/integration/compute/extensions.sh --cache          # layer-cache the base build
#   test/integration/compute/extensions.sh --keep           # leave the cluster up
#
# Honoured env vars: YB_HOST, YB_PORT (the backing YugabyteDB; default
# 127.0.0.1:5433), INTEGRATION_PORT_BASE (pin the cluster's port block),
# YB_AMP_REPO_DIR, YB_DBNAME. For a private-source extension (hexabridge),
# AMP_EXT_GH_TOKEN must be set — build.sh passes it through as the gh_token
# BuildKit secret to fetch the source (see deploy/extensions/build.sh).
#
# Requires: docker, python3 + PyYAML (the manifest parse), a YugabyteDB, and a
# built Postgres (pg_install/v18 — this script runs ./build.sh if it is
# missing). Heavy: the first run builds the ABI-anchor PG18 base image in
# docker (several minutes). Primarily a manual/local driver; the opt-in
# `amp_extensions_local` CI suite (amp-on-demand-checks.yml) runs it with
# --cache so the base restores from the shared registry layer cache instead of
# a cold rebuild.
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: test/integration/compute/extensions.sh [options]

Options:
  --ext <name>        extension to test (default: pgvector). Must be a row in
                      deploy/extensions/extensions.yaml.
  --debug             build/run the cluster binaries in debug (default: release)
  --release           build/run the cluster binaries in release (the default)
  --registry <reg>    override the image registry from extensions.yaml
  --cache             layer-cache the ABI-anchor PG18 base build via build.sh
                      --cache (buildx docker-container builder + a registry login
                      required; see deploy/extensions/build.sh)
  --keep              leave the cluster + db up for postmortem (default: tear down)
  -h, --help          show this help

Honoured env vars: YB_HOST, YB_PORT (default 127.0.0.1:5433),
INTEGRATION_PORT_BASE, YB_AMP_REPO_DIR, YB_DBNAME.
EOF
}

# The amp tree root. Derived from this script's own location rather than
# `git rev-parse --show-toplevel`, which returns the repository root — not the
# same directory once amp/ is a subdirectory of a larger repo.
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$REPO"

EXT_NAME=pgvector
PROFILE=release
REGISTRY_OVERRIDE=""
CACHE=0
KEEP=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --ext) EXT_NAME="$2"; shift ;;
        --debug) PROFILE=debug ;;
        --release) PROFILE=release ;;
        --registry) REGISTRY_OVERRIDE="$2"; shift ;;
        --cache) CACHE=1 ;;
        --keep) KEEP=1 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
    esac
    shift
done

command -v docker >/dev/null 2>&1 || { echo "docker not found on PATH" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "python3 not found on PATH" >&2; exit 1; }

MANIFEST="$REPO/deploy/extensions/extensions.yaml"
PGINST="$REPO/pg_install/v18"

# 1) Resolve the extension's manifest row: the image tag is the single source
#    of truth in extensions.yaml, so read it the same way build.sh does rather
#    than reconstruct it here. Emits: registry, version, control, preload.
read_row() {
    python3 - "$MANIFEST" "$EXT_NAME" <<'PY'
import sys, yaml
m = yaml.safe_load(open(sys.argv[1]))
name = sys.argv[2]
reg = m.get("registry", "")
for e in m.get("extensions", []):
    if e.get("name") == name:
        control = e.get("control") or name
        preload = "true" if e.get("preload") else "false"
        print("%s\t%s\t%s\t%s" % (reg, e.get("version", ""), control, preload))
        break
else:
    sys.exit("extension %r not found in %s" % (name, sys.argv[1]))
PY
}
IFS=$'\t' read -r REGISTRY VERSION CONTROL PRELOAD < <(read_row)
[[ -n "$REGISTRY_OVERRIDE" ]] && REGISTRY="$REGISTRY_OVERRIDE"
[[ -n "$VERSION" ]] || { echo "no version for '$EXT_NAME' in $MANIFEST" >&2; exit 1; }
IMAGE="${REGISTRY}/amp-ext-${EXT_NAME}:${VERSION}"

echo ">>> extension=$EXT_NAME version=$VERSION control=$CONTROL preload=$PRELOAD image=$IMAGE"

# 2) Build the extension image with the real docker pipeline (no --push). This
#    also builds the ABI-anchor PG18 base image the first time (slow).
echo ">>> building extension image via deploy/extensions/build.sh"
BUILD_ARGS=()
[[ -n "$REGISTRY_OVERRIDE" ]] && BUILD_ARGS+=(--registry "$REGISTRY_OVERRIDE")
[[ $CACHE == 1 ]] && BUILD_ARGS+=(--cache)
bash "$REPO/deploy/extensions/build.sh" "${BUILD_ARGS[@]}" "$EXT_NAME"

# 3) Extract the FROM-scratch tree (share/ + lib/) to a host temp dir. The
#    image root is exactly /share/postgresql/extension + /lib/postgresql
#    (extension.Dockerfile), so after this EXT_DIR holds share/ and lib/.
#    The image is FROM scratch with no CMD/ENTRYPOINT, and `docker create`
#    refuses one ("no command specified") without a command; pass a placeholder.
#    It is only recorded in the container config, never run — we just docker cp
#    the layers out and delete it — so the binary need not exist in the image.
EXT_DIR="$(mktemp -d)"
echo ">>> extracting $IMAGE -> $EXT_DIR"
CID="$(docker create "$IMAGE" /extract-only)"
docker cp "$CID:/share" "$EXT_DIR/"
docker cp "$CID:/lib" "$EXT_DIR/"
docker rm "$CID" >/dev/null
[[ -d "$EXT_DIR/share/postgresql/extension" ]] || { echo "extracted tree missing share/postgresql/extension" >&2; exit 1; }
[[ -d "$EXT_DIR/lib/postgresql" ]] || { echo "extracted tree missing lib/postgresql" >&2; exit 1; }

# 4) The compute needs a built Postgres (+ the yb_amp extension) to run. build.sh
#    is the single entry point that produces pg_install/v18 and the yb_amp .so.
#    This has to happen BEFORE the cargo build below: postgres_ffi's build script
#    generates bindings against pg_install/v18's server headers (it reads
#    pg_config / include/postgresql/server), so a missing pg_install/v18 fails the
#    cluster-binary build with "'c.h' file not found" rather than building it. The
#    CI PG-install cache is keyed on amp/Makefile, so a Makefile change is a cache
#    miss and pg_install/v18 is absent here on the first run after such a change.
if [[ ! -x "$PGINST/bin/postgres" || ! -f "$PGINST/lib/postgresql/yb_amp.so" ]]; then
    echo ">>> pg_install/v18 missing — building Postgres + yb_amp extension (./build.sh)"
    bash "$REPO/build.sh"
fi

# 5) Build the binaries yb_amp_local spawns at runtime (it does not build them
#    itself — a stale one surfaces as baffling runtime mismatches). The four
#    server binaries are cargo targets; yb_amp_local and the controller are Go
#    (controller/local/cmd, controller/cmd), built separately below. Mirrors run.sh.
CLUSTER_BINS=(--bin pageserver --bin safekeeper
              --bin compute-ctl)
echo ">>> building cluster binaries ($PROFILE)..."
if [[ $PROFILE == release ]]; then
    cargo build --release "${CLUSTER_BINS[@]}"
    YB_AMP="$REPO/target/release/yb_amp_local"
    _PROFILE_DIR=release
else
    cargo build "${CLUSTER_BINS[@]}"
    YB_AMP="$REPO/target/debug/yb_amp_local"
    _PROFILE_DIR=debug
fi
# yb_amp_local and the controller it spawns are Go binaries, not cargo targets.
( cd "$REPO/controller" && go build -o "$REPO/target/$_PROFILE_DIR/yb_amp_local" ./local/cmd/yb_amp_local )
( cd "$REPO/controller" && go build -o "$REPO/target/$_PROFILE_DIR/controller" ./cmd/controller )

# 6) Run-scoped, isolated cluster identifiers (mirrors run.sh / pg_regress.sh).
RUN_ID="${EPOCHSECONDS:-$(date +%s)}_$$"
export YB_HOST="${YB_HOST:-127.0.0.1}"
export YB_PORT="${YB_PORT:-5433}"
export YB_DBNAME="${YB_DBNAME:-yb_storage_extensions_${RUN_ID}}"
export YB_AMP_REPO_DIR="${YB_AMP_REPO_DIR:-$REPO/.integration-tests/extensions_${RUN_ID}}"
# The cluster finds Postgres at runtime here; the compute (and our psql) need its libs.
export POSTGRES_DISTRIB_DIR="$REPO/pg_install"
export LD_LIBRARY_PATH="$PGINST/lib:${LD_LIBRARY_PATH:-}"

# Pick a free port block (base..base+700): SC=base, PS pg/http/grpc=+300/+400/+500,
# SK=+100/+200, compute=+700.
pick_base() {
    local b
    for b in "${INTEGRATION_PORT_BASE:-53000}" 54000 55000 56000 57000 58000; do
        local busy=0 p
        for p in "$b" $((b+300)) $((b+400)) $((b+700)); do
            ss -ltn 2>/dev/null | grep -q ":${p}\b" && { busy=1; break; }
        done
        [[ $busy == 0 ]] && { echo "$b"; return 0; }
    done
    echo "no free port block found" >&2; return 1
}
BASE=$(pick_base)
COMPUTE=$((BASE + 700))
export COMPUTE_PORT="$COMPUTE"

clean() { bash "$REPO/test/integration/compute/clean.sh" >/dev/null 2>&1 || true; }
# On failure, dump the cluster's logs to stdout BEFORE teardown. The launcher
# writes each daemon's combined stdout/stderr to <dataDir>/<name>.log and the
# compute to <endpointDir>/compute.log (controller/local proc.go, endpoint.go),
# all under YB_AMP_REPO_DIR — and the teardown below (or a --keep-less exit)
# deletes that dir, so a CI "upload logs" step finds nothing. Printing them here
# puts a compute_ctl / postgres startup error right in the job output, which is
# the only way to see why e.g. `endpoint start` timed out waiting on compute_ctl.
dump_logs() {
    local d="${YB_AMP_REPO_DIR:-}"
    [[ -n "$d" && -d "$d" ]] || return 0
    echo ">>> FAILED — dumping cluster logs under $d"
    local f
    while IFS= read -r f; do
        echo "===== ${f#"$d"/} ====="
        tail -n 120 "$f" 2>/dev/null || true
    done < <(find "$d" -type f -name '*.log' 2>/dev/null | sort)
    # The generated postgresql.conf shows whether the extension GUCs landed.
    while IFS= read -r f; do
        echo "===== ${f#"$d"/} (extension GUCs) ====="
        grep -nE 'extension_control_path|dynamic_library_path|shared_preload_libraries' "$f" 2>/dev/null || true
    done < <(find "$d" -type f -name 'postgresql.conf' 2>/dev/null | sort)
}
# Tear down on ANY exit (success, set -e failure, or signal) unless --keep, so a
# failed bring-up never leaks a half-up cluster or the extracted tree. clean.sh
# is scoped to this run's db + dir.
finish() {
    local rc=$?
    trap - EXIT INT TERM HUP
    [[ $rc -ne 0 ]] && dump_logs
    if [[ $KEEP == 1 ]]; then
        # Keep EXT_DIR too: the endpoint's extension_control_path/dynamic_library_path
        # point into it, so wiping it here would leave the kept cluster unable to
        # restart or CREATE EXTENSION.
        echo ">>> --keep: cluster left up (compute 127.0.0.1:${COMPUTE:-?}, db=$YB_DBNAME)"
        echo "    tear down: YB_DBNAME=$YB_DBNAME YB_AMP_REPO_DIR=$YB_AMP_REPO_DIR COMPUTE_PORT=${COMPUTE:-} bash test/integration/compute/clean.sh"
    else
        [[ -n "${EXT_DIR:-}" ]] && rm -rf "$EXT_DIR"
        echo ">>> tearing down"; clean
    fi
    exit "$rc"
}
trap finish EXIT INT TERM HUP
clean   # clear any leftover state for this (db, dir) up front

echo ">>> bringing up yb-amp cluster (db=$YB_DBNAME base=$BASE compute=$COMPUTE)"
yb() { "$YB_AMP" "$@"; }
yb init --yb-dbname "$YB_DBNAME" --yb-host "$YB_HOST" --yb-port "$YB_PORT" --port-base "$BASE"
yb start
# A cluster is a new lineage; its default branch is the root. The CLI prints
# "branch <id> successfully created" — scrape the 2nd token for the branch id.
BR=$(yb cluster create ext --pg-version 18 | awk '/successfully created/{print $2; exit}')
[[ -n "$BR" ]] || { echo "cluster create produced no branch id" >&2; exit 1; }
echo ">>> branch: $BR"

# 7) Create the endpoint with the two path GUCs pointing at the extracted tree.
#    $system / $libdir stay literal (PG expands them); the extracted dir is
#    interpolated by the shell. These are Postgres path lists, so entries are
#    COLON-separated (like the chart's `join ":"` in configmap.yaml) — a comma
#    makes PG read "$system,<dir>" as one element and FATAL on the macro name.
#    extension_control_path is the sharedir BASE — PG18 appends /extension itself.
PG_SETTINGS=(
    --pg-setting "extension_control_path=\$system:$EXT_DIR/share/postgresql"
    --pg-setting "dynamic_library_path=\$libdir:$EXT_DIR/lib/postgresql"
)
# A preload extension must also be added to shared_preload_libraries; the local
# base sets it to just "yb_amp" (postgresqlconf.go). That GUC is comma-separated
# (a list of library names), unlike the path GUCs above. pgvector needs no preload.
if [[ "$PRELOAD" == true ]]; then
    PG_SETTINGS+=(--pg-setting "shared_preload_libraries=yb_amp,$CONTROL")
fi
yb endpoint create ext --branch-id "$BR" --pg-version 18 --pg-port "$COMPUTE" "${PG_SETTINGS[@]}"
yb endpoint start ext -t 600s

# 8) Assert end-to-end via the Postgres we built (its psql must match the
#    compute's major version). Connect as cloud_admin over loopback (pg_hba
#    trusts it, no password). -tA prints raw values for exact-match grepping.
PSQL=("$PGINST/bin/psql" -h 127.0.0.1 -p "$COMPUTE" -U cloud_admin -d postgres -tA -v ON_ERROR_STOP=1)

if [[ "$EXT_NAME" == pgvector ]]; then
    # CREATE EXTENSION + store real vector data + a nearest-neighbour query that
    # exercises the type end-to-end (insert -> CHECKPOINT through the storage
    # stack -> ORDER BY <-> read back). An HNSW index makes the query go through
    # pgvector's index access method (build + ANN scan), not just the type
    # operators — enable_seqscan=off forces the planner onto it despite the tiny
    # table. Same assertions as deploy/smoke-test.sh: NN of [3,3,3] is [2,2,2]
    # (id=2) at sqrt(3)=1.732; the literal <-> = 5.196.
    out=$("${PSQL[@]}" <<'SQL' 2>&1
CREATE EXTENSION IF NOT EXISTS vector;
DROP TABLE IF EXISTS items;
CREATE TABLE items (id serial PRIMARY KEY, embedding vector(3));
INSERT INTO items (embedding) VALUES ('[1,1,1]'), ('[2,2,2]'), ('[9,9,9]');
CREATE INDEX items_embedding_hnsw ON items USING hnsw (embedding vector_l2_ops);
CHECKPOINT;
SET enable_seqscan = off;
SELECT id || '|' || round((embedding <-> '[3,3,3]')::numeric, 3)
  FROM items ORDER BY embedding <-> '[3,3,3]' LIMIT 1;
SELECT round(('[1,2,3]'::vector <-> '[4,5,6]'::vector)::numeric, 3);
SQL
)
    if echo "$out" | grep -q '^2|1.732$' && echo "$out" | grep -q '^5.196$'; then
        echo ">>> PASS pgvector: NN([3,3,3])=id2 dist=1.732 + literal <-> =5.196"
    else
        echo ">>> FAIL pgvector data query; output:" >&2
        echo "$out" >&2
        exit 1
    fi
elif [[ "$EXT_NAME" == pg_cron ]]; then
    # pg_cron is a preload extension, so extensions.sh has already appended it to
    # shared_preload_libraries above (PRELOAD=true from the manifest) and the
    # background-worker scheduler is running. Assert the full functional path, not
    # just registration: CREATE EXTENSION, schedule a sub-minute job, and wait for
    # cron's own bookkeeping to report a succeeded run. The scheduler binds to one
    # db via cron.database_name (default "postgres", which this endpoint serves) —
    # the same db we connect to — so the marker row the job writes is visible here.
    out=$("${PSQL[@]}" <<'SQL' 2>&1
CREATE EXTENSION IF NOT EXISTS pg_cron;
SELECT 'CRONDB:'||current_setting('cron.database_name');
DROP TABLE IF EXISTS cron_marker;
CREATE TABLE cron_marker (id bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now());
SELECT 'JOBID:'||cron.schedule('amp_pgcron_probe', '5 seconds', $$INSERT INTO cron_marker DEFAULT VALUES$$);
SQL
)
    if ! echo "$out" | grep -q '^CRONDB:postgres$'; then
        echo ">>> FAIL pg_cron: cron.database_name is not the default postgres; output:" >&2
        echo "$out" >&2
        exit 1
    fi
    if ! echo "$out" | grep -qE '^JOBID:[0-9]+$'; then
        echo ">>> FAIL pg_cron: cron.schedule did not return a job id; output:" >&2
        echo "$out" >&2
        exit 1
    fi
    # Poll cron.job_run_details (keyed by jobid — it has NO jobname column) for a
    # completed run: the definitive "scheduler picked the job up and the command
    # succeeded" signal. Seconds-granularity scheduling (pg_cron >= 1.5; we ship
    # 1.6.7) keeps this well under a cron minute.
    jobid=$(echo "$out" | sed -n 's/^JOBID:\([0-9]*\)$/\1/p')
    deadline=$(( $(date +%s) + 120 ))
    fired=0
    while (( $(date +%s) < deadline )); do
        succeeded=$("${PSQL[@]}" -c "SELECT count(*) FROM cron.job_run_details WHERE jobid=${jobid:-0} AND status='succeeded'" 2>/dev/null || echo 0)
        if [[ "${succeeded:-0}" -gt 0 ]]; then
            fired=$("${PSQL[@]}" -c "SELECT count(*) FROM cron_marker" 2>/dev/null || echo 0)
            break
        fi
        sleep 2
    done
    # Best-effort unschedule so a --keep cluster isn't left with a firing job.
    "${PSQL[@]}" -c "SELECT cron.unschedule('amp_pgcron_probe')" >/dev/null 2>&1 || true
    if [[ "${fired:-0}" -gt 0 ]]; then
        echo ">>> PASS pg_cron: preload + CREATE EXTENSION + scheduled job fired ($fired run(s))"
    else
        echo ">>> FAIL pg_cron: job never completed a succeeded run within 120s" >&2
        echo "$out" >&2
        exit 1
    fi
elif [[ "$EXT_NAME" == hexabridge ]]; then
    # hexabridge is HexaCluster's Oracle-compat layer: a plain (non-preload)
    # function library whose source lives in a PRIVATE repo (build.sh fetched it
    # with $AMP_EXT_GH_TOKEN). CREATE EXTENSION is superuser-only (the .control
    # ships no `trusted`); cloud_admin is the compute superuser. Beyond
    # registration, call two deterministic, IMMUTABLE Oracle-compat functions so
    # the loaded .so is actually exercised: nanvl returns its first arg when it is
    # not NaN (-> 1), and utl_raw.length over the raw cast of 'AB' is 2 bytes.
    # Same assertions as deploy/smoke-test.sh's hexabridge block.
    out=$("${PSQL[@]}" <<'SQL' 2>&1
CREATE EXTENSION IF NOT EXISTS hexabridge;
SELECT 'INSTALLED:'||extversion FROM pg_extension WHERE extname='hexabridge';
SELECT 'NANVL:'||hexabridge.nanvl(1::float8, 2::float8);
SELECT 'RAWLEN:'||utl_raw.length(utl_raw.cast_to_raw('AB'));
SQL
)
    if echo "$out" | grep -q '^INSTALLED:0.1$' \
       && echo "$out" | grep -q '^NANVL:1$' \
       && echo "$out" | grep -q '^RAWLEN:2$'; then
        echo ">>> PASS hexabridge: CREATE EXTENSION (v0.1) + nanvl(1,2)=1 + utl_raw.length('AB')=2"
    else
        echo ">>> FAIL hexabridge functional checks; output:" >&2
        echo "$out" >&2
        exit 1
    fi
else
    # Generic smoke: create the extension and confirm it registered.
    out=$("${PSQL[@]}" <<SQL 2>&1
CREATE EXTENSION IF NOT EXISTS "$CONTROL";
SELECT 'INSTALLED:'||extname FROM pg_extension WHERE extname='$CONTROL';
SQL
)
    if echo "$out" | grep -q "^INSTALLED:${CONTROL}$"; then
        echo ">>> PASS $EXT_NAME: CREATE EXTENSION $CONTROL"
    else
        echo ">>> FAIL $EXT_NAME: CREATE EXTENSION $CONTROL; output:" >&2
        echo "$out" >&2
        exit 1
    fi
fi

echo ">>> OK ($EXT_NAME)"
# teardown (or --keep) handled by the finish() EXIT trap
