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
#   test/integration/compute/extensions.sh --ext pgvector,age # co-mount + smoke both
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
  --ext <name>[,<name>...]
                      extension(s) to test (default: pgvector). Each must be a
                      row in deploy/extensions/extensions.yaml. A comma-separated
                      list co-mounts several on one endpoint (e.g. pgvector,age)
                      and runs each one's smoke in turn. --regress needs a single
                      extension.
  --regress           run the extension's OWN upstream regression suite against
                      the mounted ImageVolume (see the row's `regress:` block)
                      instead of the built-in smoke assertions.
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
REGRESS=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --ext) EXT_NAME="$2"; shift ;;
        --regress) REGRESS=1 ;;
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

# --ext accepts a comma-separated list to co-mount several extensions on ONE
# endpoint (e.g. --ext pgvector,age) — the local-deploy twin of opting an
# endpoint into extensions:[...]. The built-in smoke runs each extension's own
# assertions in turn against the shared compute. --regress stays single-extension
# (it drives one upstream pg_regress/script suite).
IFS=',' read -ra EXT_NAMES <<< "$EXT_NAME"
[[ ${#EXT_NAMES[@]} -ge 1 && -n "${EXT_NAMES[0]}" ]] || { echo "no --ext given" >&2; exit 2; }
PRIMARY_EXT="${EXT_NAMES[0]}"
if [[ $REGRESS == 1 && ${#EXT_NAMES[@]} -gt 1 ]]; then
    echo "--regress runs a single extension's upstream suite; got --ext $EXT_NAME" >&2
    exit 2
fi

MANIFEST="$REPO/deploy/extensions/extensions.yaml"
PGINST="$REPO/pg_install/v18"

# 1) Resolve each extension's manifest row: the image tag is the single source
#    of truth in extensions.yaml, so read it the same way build.sh does rather
#    than reconstruct it here. Emits (per name): registry, version, control, preload.
read_row() {
    python3 - "$MANIFEST" "$1" <<'PY'
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
declare -a EXT_VERSIONS EXT_CONTROLS EXT_PRELOADS EXT_IMAGES
for _n in "${EXT_NAMES[@]}"; do
    IFS=$'\t' read -r _reg _ver _ctrl _pre < <(read_row "$_n")
    [[ -n "$REGISTRY_OVERRIDE" ]] && _reg="$REGISTRY_OVERRIDE"
    [[ -n "$_ver" ]] || { echo "no version for '$_n' in $MANIFEST" >&2; exit 1; }
    EXT_VERSIONS+=("$_ver"); EXT_CONTROLS+=("$_ctrl"); EXT_PRELOADS+=("$_pre")
    EXT_IMAGES+=("${_reg}/amp-ext-${_n}:${_ver}")
    echo ">>> extension=$_n version=$_ver control=$_ctrl preload=$_pre image=${_reg}/amp-ext-${_n}:${_ver}"
done
# Legacy single-extension aliases: the --regress path below drives just the
# primary extension and its script driver does CREATE EXTENSION $CONTROL.
EXT_NAME="$PRIMARY_EXT"
CONTROL="${EXT_CONTROLS[0]}"

# 2+3) Build each extension image via the real docker pipeline (no --push; the
#      first build also compiles the ABI-anchor PG18 base — slow) and extract its
#      FROM-scratch tree (share/ + lib/) to a per-extension host temp dir. The
#      image root is exactly /share/postgresql/extension + /lib/postgresql
#      (extension.Dockerfile). The image is FROM scratch with no CMD/ENTRYPOINT,
#      and `docker create` refuses one ("no command specified") without a
#      command; pass a placeholder — it is only recorded in the container config,
#      never run (we just docker cp the layers out and delete it). Accumulate the
#      two PG path GUCs ($system/$libdir kept literal — PG expands them — the
#      extracted dirs COLON-joined on) and, for any preload extension, its
#      shared_preload_libraries name.
declare -a EXT_DIRS
CTRL_PATH_PARTS=('$system')
LIB_PATH_PARTS=('$libdir')
PRELOAD_LIBS=()
for _i in "${!EXT_NAMES[@]}"; do
    _n="${EXT_NAMES[$_i]}"; _img="${EXT_IMAGES[$_i]}"
    echo ">>> building extension image ($_n) via deploy/extensions/build.sh"
    BUILD_ARGS=()
    [[ -n "$REGISTRY_OVERRIDE" ]] && BUILD_ARGS+=(--registry "$REGISTRY_OVERRIDE")
    [[ $CACHE == 1 ]] && BUILD_ARGS+=(--cache)
    bash "$REPO/deploy/extensions/build.sh" "${BUILD_ARGS[@]}" "$_n"

    _dir="$(mktemp -d)"
    EXT_DIRS+=("$_dir")
    echo ">>> extracting $_img -> $_dir"
    _cid="$(docker create "$_img" /extract-only)"
    docker cp "$_cid:/share" "$_dir/"
    docker cp "$_cid:/lib" "$_dir/"
    docker rm "$_cid" >/dev/null
    [[ -d "$_dir/share/postgresql/extension" ]] || { echo "extracted tree missing share/postgresql/extension" >&2; exit 1; }
    [[ -d "$_dir/lib/postgresql" ]] || { echo "extracted tree missing lib/postgresql" >&2; exit 1; }
    CTRL_PATH_PARTS+=("$_dir/share/postgresql")
    LIB_PATH_PARTS+=("$_dir/lib/postgresql")
    [[ "${EXT_PRELOADS[$_i]}" == true ]] && PRELOAD_LIBS+=("${EXT_CONTROLS[$_i]}")
done

# 4) The compute needs a built Postgres (+ the yb_amp extension) to run. build.sh
#    is the single entry point that produces pg_install/v18 and the yb_amp .so.
#    This has to happen BEFORE the cargo build below: postgres_ffi's build script
#    generates bindings against pg_install/v18's server headers (it reads
#    pg_config / include/postgresql/server), so a missing pg_install/v18 fails the
#    cluster-binary build with "'c.h' file not found" rather than building it. The
#    CI PG-install cache is keyed on the PG submodule rev, amp/Makefile, amp/postgres.mk
#    and amp/compute/pgxn/** — the last so a change to the yb_amp .so (e.g. the
#    pagestream wire command) busts the cache and rebuilds it against the fresh
#    pageserver, rather than reusing a stale .so that speaks the old protocol.
if [[ ! -x "$PGINST/bin/postgres" || ! -f "$PGINST/lib/postgresql/yb_amp.so" ]]; then
    echo ">>> pg_install/v18 missing — building Postgres + yb_amp extension (./build.sh)"
    bash "$REPO/build.sh"
fi

# 5) Build the binaries yb_amp_local spawns at runtime (it does not build them
#    itself — a stale one surfaces as baffling runtime mismatches). The four
#    server binaries are cargo targets; yb_amp_local and the controller are Go
#    (controller/local/cmd, controller/cmd), built separately below. Mirrors run.sh.
CLUSTER_BINS=(--bin pageserver --bin safekeeper
              --bin compute-ctl
              # yb_amp_local runs the three daemons under this.
              --bin endpoint-supervisor)
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

# Low end of the OS ephemeral (dynamic) port range; mirrors the Rust harness's
# ephemeral_floor (test/integration/compute/cluster.rs).
ephemeral_floor() {
    local lo=""
    if [[ -r /proc/sys/net/ipv4/ip_local_port_range ]]; then
        lo=$(cut -f1 /proc/sys/net/ipv4/ip_local_port_range 2>/dev/null)
    fi
    if [[ $lo =~ ^[0-9]+$ ]] && (( lo > 10000 )); then echo "$lo"; else echo 32768; fi
}

# Pick a free port block (base..base+700): SC=base, SK=+100/+200,
# PS pg/http/grpc=+300/+400/+500, compute=+700.
#
# The whole block must sit BELOW the ephemeral floor, like pg_regress.sh and the
# Rust harness's pick_port_base. `ss` sees only listeners, so a port up there can
# be taken between this probe and the bind by an outbound connection — including
# our own daemons' connections to YB, which is how a CI run lost the controller's
# base port on :55000 with an "address already in use" bind.
pick_base() {
    local floor b p busy
    floor=$(ephemeral_floor)
    for b in "${INTEGRATION_PORT_BASE:-18000}" 19000 20000 21000 22000 23000; do
        (( b + 700 < floor )) || continue
        busy=0
        # Every port the block actually binds. Probing only some of them let a
        # busy safekeeper or pageserver-grpc port through, and the daemon then
        # died on "Address already in use" instead of this moving to the next block.
        for p in "$b" $((b+100)) $((b+200)) $((b+300)) $((b+400)) $((b+500)) $((b+700)); do
            ss -ltn 2>/dev/null | grep -q ":${p}\b" && { busy=1; break; }
        done
        [[ $busy == 0 ]] && { echo "$b"; return 0; }
    done
    echo "no free port block below the ephemeral floor ($floor)" >&2; return 1
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
        # Keep the extracted tree(s) too: the endpoint's extension_control_path/
        # dynamic_library_path point into them, so wiping them here would leave the
        # kept cluster unable to restart or CREATE EXTENSION.
        echo ">>> --keep: cluster left up (compute 127.0.0.1:${COMPUTE:-?}, db=$YB_DBNAME)"
        echo "    tear down: YB_DBNAME=$YB_DBNAME YB_AMP_REPO_DIR=$YB_AMP_REPO_DIR COMPUTE_PORT=${COMPUTE:-} bash test/integration/compute/clean.sh"
    else
        for _d in "${EXT_DIRS[@]:-}"; do [[ -n "$_d" ]] && rm -rf "$_d"; done
        [[ -n "${SRC_DIR:-}" ]] && rm -rf "$SRC_DIR"
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

# 7) Create the endpoint with the two path GUCs pointing at the extracted tree(s).
#    $system / $libdir stay literal (PG expands them); the extracted dirs are
#    interpolated by the shell. These are Postgres path lists, so entries are
#    COLON-separated (like the chart's `join ":"` in configmap.yaml) — a comma
#    makes PG read "$system,<dir>" as one element and FATAL on the macro name.
#    extension_control_path is the sharedir BASE — PG18 appends /extension itself.
#    With several co-mounted extensions every extracted dir is on the path.
CTRL_PATH="$(IFS=:; echo "${CTRL_PATH_PARTS[*]}")"
LIB_PATH="$(IFS=:; echo "${LIB_PATH_PARTS[*]}")"
PG_SETTINGS=(
    --pg-setting "extension_control_path=$CTRL_PATH"
    --pg-setting "dynamic_library_path=$LIB_PATH"
    # amp compute defaults compute_query_id=on, so EXPLAIN (VERBOSE) prints a
    # non-deterministic "Query Identifier:" line. Upstream regress suites assume
    # the stock default (off), so restore it — otherwise AGE's expr/cypher_with
    # EXPLAIN tests diverge on a value that can't be blessed. Harmless elsewhere.
    --pg-setting "compute_query_id=off"
)
# A preload extension must also be added to shared_preload_libraries; the base is
# the supervisor's DEFAULT_SHARED_PRELOAD_LIBRARIES (endpoint_supervisor's
# main.rs), which local and deployed endpoints share. That GUC is comma-separated
# (a list of library names), unlike the path GUCs above. pgvector/age need no
# preload; pg_cron/hexabridge do — union whatever the co-mounted set requires.
if [[ ${#PRELOAD_LIBS[@]} -gt 0 ]]; then
    SPL="$(IFS=,; echo "${PRELOAD_LIBS[*]}")"
    PG_SETTINGS+=(--pg-setting "shared_preload_libraries=yb_amp,$SPL")
fi
yb endpoint create ext --branch-id "$BR" --pg-version 18 --pg-port "$COMPUTE" "${PG_SETTINGS[@]}"
yb endpoint start ext -t 600s

# 8) Assert end-to-end via the Postgres we built (its psql must match the
#    compute's major version). Connect as cloud_admin over loopback (pg_hba
#    trusts it, no password). -tA prints raw values for exact-match grepping.
PSQL=("$PGINST/bin/psql" -h 127.0.0.1 -p "$COMPUTE" -U cloud_admin -d postgres -tA -v ON_ERROR_STOP=1)

# --regress: run the extension's own upstream regression suite against the live
# compute (the local-deploy analogue of pg_regress.sh for core Postgres, #608),
# instead of the hand-written smoke below. The suite ships in the extension's
# SOURCE tree — which the FROM-scratch ImageVolume deliberately does not carry —
# so re-fetch the source the same way extension.Dockerfile does (URL + sha256 +
# optional gh_token/prep/patch from the manifest). The already-built ImageVolume
# is what the compute loads; the fetched tree only supplies sql/expected.
if [[ "$REGRESS" == 1 ]]; then
    # Pull the row's regress config + the fetch inputs, as shell assignments.
    eval "$(python3 - "$MANIFEST" "$EXT_NAME" <<'PY'
import sys, yaml, shlex
m = yaml.safe_load(open(sys.argv[1]))
name = sys.argv[2]
for e in m.get("extensions", []):
    if e.get("name") == name:
        r = e.get("regress") or {}
        def emit(k, v): print("%s=%s" % (k, shlex.quote(str(v if v is not None else ""))))
        emit("REG_DRIVER", r.get("driver"))
        emit("REG_SCRIPT", r.get("script"))
        emit("EXT_URL", e.get("url"))
        emit("EXT_SHA256", e.get("sha256"))
        emit("EXT_PATCH", e.get("patch"))
        emit("EXT_PREP", e.get("prep"))
        break
else:
    sys.exit("extension %r not found in %s" % (name, sys.argv[1]))
PY
)"
    [[ -n "$REG_DRIVER" ]] || { echo "$EXT_NAME has no regress: block in $MANIFEST; nothing to run with --regress" >&2; exit 2; }

    # Re-fetch + unpack the source into a run-scoped dir (cleaned by finish()).
    SRC_DIR="$(mktemp -d)"
    fetch_source() {
        local tgz; tgz="$(mktemp)"
        local auth=()
        # hexabridge lives in a private repo; the token turns into a bearer
        # header exactly as extension.Dockerfile's gh_token secret does. Public
        # sources (pgvector, pg_cron) fetch with no header.
        [[ -n "${AMP_EXT_GH_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer $AMP_EXT_GH_TOKEN")
        echo ">>> fetching source: $EXT_URL"
        curl -fsSL "${auth[@]}" "$EXT_URL" -o "$tgz"
        echo "$EXT_SHA256  $tgz" | sha256sum -c -
        tar -xzf "$tgz" --strip-components=1 -C "$SRC_DIR"
        rm -f "$tgz"
        ( cd "$SRC_DIR"
          [[ -n "$EXT_PREP" ]] && sh -eu -c "$EXT_PREP"
          [[ -n "$EXT_PATCH" ]] && patch --batch --fuzz=0 -p1 < "$REPO/compute/image/patches/$EXT_PATCH"
          true )
    }

    # Overlay amp-specific expected-output variants (by-design diffs blessed in
    # test/integration/compute/ext_regress_expected/<name>/) onto the fetched
    # tree. Empty until a real diff is blessed, so a clean tree is a genuine green.
    overlay_expected() {
        local destdir="$1"
        local src="$REPO/test/integration/compute/ext_regress_expected/$EXT_NAME"
        [[ -d "$src" ]] || return 0
        local n; n=$(find "$src" -type f ! -name README.md | wc -l)
        [[ "$n" -gt 0 ]] || return 0
        echo ">>> overlaying $n amp expected variant(s): $src -> $destdir"
        mkdir -p "$destdir"
        # Copy everything except the directory's README.md.
        ( cd "$src" && find . -type f ! -name README.md -print0 \
            | while IFS= read -r -d '' f; do
                mkdir -p "$destdir/$(dirname "$f")"; cp -a "$f" "$destdir/$f"
              done )
    }

    fetch_source

    if [[ "$REG_DRIVER" == pgxs ]]; then
        # Evaluate REGRESS + REGRESS_OPTS from the extension's own Makefile (they
        # use make functions like $(wildcard ...), so read them through make
        # rather than parse). Then strip --temp-instance/--temp-config: those
        # would spin up a PRIVATE temp server (pg_cron's Makefile does), but we
        # want the suite driven at the live amp compute.
        PG_CONFIG="$PGINST/bin/pg_config"
        # A throwaway wrapper makefile that includes the extension's own Makefile
        # (which ends in `include $(PGXS)`) and adds a target to print any var.
        WRAP="$SRC_DIR/.amp_regress.mk"
        cat > "$WRAP" <<'MK'
include Makefile
amp-print-%:
	@echo '$($*)'
MK
        REGRESS_TESTS="$(make -s -C "$SRC_DIR" -f "$WRAP" amp-print-REGRESS PG_CONFIG="$PG_CONFIG")"
        REGRESS_OPTS="$(make -s -C "$SRC_DIR" -f "$WRAP" amp-print-REGRESS_OPTS PG_CONFIG="$PG_CONFIG")"
        rm -f "$WRAP"
        [[ -n "$REGRESS_TESTS" ]] || { echo ">>> FAIL $EXT_NAME: no REGRESS tests found in Makefile" >&2; exit 1; }
        # Reproduce the extension's "installcheck-existing" (run against a live
        # server) from its REGRESS_OPTS by dropping only what pins a PRIVATE temp
        # instance: --temp-instance, its --temp-config, and the --port that goes
        # with it, so the harness's own --host/--port (the live amp compute,
        # below) win. AGE bakes the whole trio into REGRESS_OPTS
        # (--temp-instance/--port=61958/--temp-config age_regression.conf); its
        # surviving --port otherwise overrode ours and pg_regress dialed a dead
        # port. Everything else is KEPT — in particular --dbname: pg_cron's and
        # age's expected output, and pg_cron's CREATE EXTENSION guard, hardcode
        # the Makefile's contrib_regression, so dropping it breaks them. Handle
        # both "--flag=value" and space-separated "--flag value" forms (the
        # latter needs the next token dropped too, or its value orphans into a
        # bogus positional test name).
        KEPT_OPTS=()
        INPUTDIR="$SRC_DIR"
        OUTPUTDIR="$SRC_DIR"   # pg_regress writes regression.diffs here (CWD default)
        drop_value=0
        for tok in $REGRESS_OPTS; do
            if (( drop_value )); then drop_value=0; continue; fi
            case "$tok" in
                --temp-instance=*|--temp-config=*|--port=*) ;;
                --temp-instance|--temp-config|--port) drop_value=1 ;;
                --inputdir=*) v="${tok#--inputdir=}"; [[ $v == /* ]] && INPUTDIR="$v" || INPUTDIR="$SRC_DIR/$v"; KEPT_OPTS+=("$tok") ;;
                --outputdir=*) v="${tok#--outputdir=}"; [[ $v == /* ]] && OUTPUTDIR="$v" || OUTPUTDIR="$SRC_DIR/$v"; KEPT_OPTS+=("$tok") ;;
                *) KEPT_OPTS+=("$tok") ;;
            esac
        done
        overlay_expected "$INPUTDIR/expected"
        PG_REGRESS="$PGINST/lib/postgresql/pgxs/src/test/regress/pg_regress"
        echo ">>> running pg_regress ($EXT_NAME): tests=[$REGRESS_TESTS] opts=[${KEPT_OPTS[*]:-}]"
        # pg_regress creates its own throwaway database on the compute (amp
        # supports CREATE DATABASE — see pg_regress.sh / #119). The endpoint's
        # extension_control_path/dynamic_library_path GUCs are server-wide, so
        # the extension resolves in that database too.
        #
        # On a diff, pg_regress writes regression.diffs (under --outputdir if the
        # Makefile set one, else the CWD) but that file is neither in the cluster
        # log dir nor uploaded, so print it here — it's the only way to see what
        # diverged (and thus what to bless under ext_regress_expected/) from CI.
        if ! ( cd "$SRC_DIR" && "$PG_REGRESS" \
            --bindir="$PGINST/bin" \
            --host=127.0.0.1 --port="$COMPUTE" --user=cloud_admin \
            --dbname="amp_regress_${EXT_NAME}" \
            "${KEPT_OPTS[@]}" $REGRESS_TESTS ); then
            for diffs in "$OUTPUTDIR/regression.diffs" "$SRC_DIR/regression.diffs" \
                         "$INPUTDIR/regression.diffs"; do
                [[ -f "$diffs" ]] || continue
                echo ">>> regression.diffs ($EXT_NAME) — $diffs:"
                cat "$diffs"
                break
            done
            echo ">>> FAIL $EXT_NAME: pg_regress reported diffs" >&2
            exit 1
        fi
        echo ">>> PASS $EXT_NAME: upstream pg_regress suite green"
    elif [[ "$REG_DRIVER" == script ]]; then
        # A custom runner (hexabridge's scripts/regression.sh): it drives psql
        # itself over PG* env and diffs against its own saved outputs, so just
        # point it at the live compute. CREATE EXTENSION first (the runner
        # assumes it exists; hexabridge's is superuser-only, and cloud_admin is
        # the compute superuser).
        [[ -n "$REG_SCRIPT" ]] || { echo ">>> FAIL $EXT_NAME: regress.driver=script but no script path" >&2; exit 1; }
        "${PSQL[@]}" -c "CREATE EXTENSION IF NOT EXISTS $CONTROL" >/dev/null
        # Run the suite as role "postgres", the role its reference CI uses.
        # dbms_aqadm.create_queue_table maps a bare queue-table name to the
        # current_user's schema (dbms_aq._parse_name), so the tests that read a
        # queue table back as the hardcoded postgres.<table> only resolve when
        # current_user is postgres. amp's superuser is cloud_admin, so add a
        # peer superuser role for the run (auth is disabled on the local compute).
        "${PSQL[@]}" -c "DO \$\$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='postgres') THEN CREATE ROLE postgres SUPERUSER LOGIN; END IF; END \$\$" >/dev/null
        overlay_expected "$SRC_DIR/test/pg"
        echo ">>> running $REG_SCRIPT ($EXT_NAME) against compute 127.0.0.1:$COMPUTE"
        ( cd "$SRC_DIR"
          export PATH="$PGINST/bin:$PATH"
          export PGHOST=127.0.0.1 PGPORT="$COMPUTE" PGUSER=postgres PGDATABASE=postgres
          # Match the extension's own CI environment for deterministic output.
          export PGOPTIONS="-c timezone=Etc/UTC -c lc_messages=C" LC_ALL=C
          # hexabridge's dbms_crypto tests need OpenSSL legacy providers; its CI
          # ships the config under .github/. Use it when present.
          [[ -f .github/openssl-legacy.cnf ]] && export OPENSSL_CONF="$PWD/.github/openssl-legacy.cnf"
          bash "$REG_SCRIPT" )
        echo ">>> PASS $EXT_NAME: upstream regression suite green"
    else
        echo ">>> FAIL $EXT_NAME: unknown regress.driver=$REG_DRIVER" >&2
        exit 1
    fi

    echo ">>> OK ($EXT_NAME regression)"
    exit 0
fi

# Built-in smoke assertions for ONE extension against the live compute. Runs
# once per co-mounted extension (all share this endpoint). $PSQL/$COMPUTE are the
# cluster globals; the extension name + its control basename come in as args.
smoke_ext() {
  local EXT_NAME="$1" CONTROL="$2"
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
    # hexabridge is HexaCluster's Oracle-compat layer: a preload extension (its
    # _PG_init registers a dbms_scheduler background worker, so it was added to
    # shared_preload_libraries above) whose source lives in a PRIVATE repo
    # (build.sh fetched it
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
elif [[ "$EXT_NAME" == age ]]; then
    # Apache AGE (openCypher graph). Non-preload: load it per session with
    # LOAD 'age' (no shared_preload_libraries). Beyond registration, exercise the
    # graph path end-to-end in one session: create a graph, a cypher() CREATE that
    # writes a vertex (through the storage stack after CHECKPOINT), then a cypher()
    # MATCH that reads the property back — asserting the value round-trips.
    out=$("${PSQL[@]}" <<'SQL' 2>&1
CREATE EXTENSION IF NOT EXISTS age;
LOAD 'age';
SET search_path = ag_catalog, "$user", public;
SELECT create_graph('amp_age_probe');
SELECT * FROM cypher('amp_age_probe', $$ CREATE (:Person {name: 'ada'}) $$) AS (v agtype);
CHECKPOINT;
SELECT * FROM cypher('amp_age_probe', $$ MATCH (p:Person) RETURN p.name $$) AS (name agtype);
SQL
)
    if echo "$out" | grep -q '^"ada"$'; then
        echo ">>> PASS age: LOAD 'age' + create_graph + cypher CREATE/MATCH round-trip (ada)"
    else
        echo ">>> FAIL age cypher query; output:" >&2
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
}

# Run the smoke assertions for each co-mounted extension in turn.
for _i in "${!EXT_NAMES[@]}"; do
    smoke_ext "${EXT_NAMES[$_i]}" "${EXT_CONTROLS[$_i]}"
done

echo ">>> OK (${EXT_NAMES[*]})"
# teardown (or --keep) handled by the finish() EXIT trap
