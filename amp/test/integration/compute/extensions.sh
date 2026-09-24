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
#   test/integration/compute/extensions.sh --registered ext_fixtures/postgis.json
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
  --registered <file> mount a REGISTERED extension instead of a built one: a
                      registerExtension body (see test/integration/compute/
                      ext_fixtures/) naming a prebuilt third-party image and its
                      layout. Pulled, not built. Repeatable, and combinable with
                      --ext to co-mount both kinds. The controller stores the same
                      JSON; this reproduces its effect on a local compute, which
                      has no controller.

                      Run it on a host whose base matches the image's: the
                      libraries such an image bundles go on LD_LIBRARY_PATH, which
                      is searched before the system directories, so they shadow the
                      host's for EVERY module — and the first casualty is yb_amp.so,
                      not the extension. The bundled set is filtered to what the
                      host cannot already resolve, which makes a near-enough host
                      work, but the filter tracks that host's package set and is
                      not a substitute for matching bases. CI covers this path
                      through amp_deploy_gke_extensions instead, on the real
                      endpoint image with real ImageVolumes.
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
# Whether --ext was actually given. The default above cannot be told apart from
# an explicit `--ext pgvector` by value, and the parse loop shifts every
# positional away, so $* is empty by the time anything downstream could look.
EXT_EXPLICIT=0
REGISTERED_FILES=()
PROFILE=release
REGISTRY_OVERRIDE=""
CACHE=0
KEEP=0
REGRESS=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --ext) EXT_NAME="$2"; EXT_EXPLICIT=1; shift ;;
        --registered) REGISTERED_FILES+=("$2"); shift ;;
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
# --registered on its own means "no built-in extensions": the default --ext
# pgvector would otherwise silently ride along and make the run about two things.
# Keyed on whether --ext was GIVEN, not on its value: `--ext pgvector
# --registered …` is a legitimate co-mount and must keep pgvector.
if [[ ${#REGISTERED_FILES[@]} -gt 0 && $EXT_EXPLICIT == 0 ]]; then
    EXT_NAME=""
fi
IFS=',' read -ra EXT_NAMES <<< "$EXT_NAME"
if [[ -z "$EXT_NAME" ]]; then EXT_NAMES=(); fi
[[ ${#EXT_NAMES[@]} -ge 1 || ${#REGISTERED_FILES[@]} -ge 1 ]] || { echo "no --ext or --registered given" >&2; exit 2; }
# Guard the REQUESTED list, before the dependency closure below can grow it.
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
        library = e.get("library") or control
        preload = "true" if e.get("preload") else "false"
        print("%s\t%s\t%s\t%s\t%s" % (reg, e.get("version", ""), control, preload, library))
        break
else:
    sys.exit("extension %r not found in %s" % (name, sys.argv[1]))
PY
}
# 1a) Close over `requires`. An extension's control file can name another
#     extension (documentdb.control requires pg_cron and vector), and CREATE
#     EXTENSION fails with "extension X is not available" unless that one's
#     control + library are on the endpoint's paths too. In the product an engine
#     bundle brings them; here the manifest says so directly, so a bare
#     `--ext documentdb` mounts what it needs. Mounting is all this does:
#     CREATE EXTENSION still needs CASCADE to install them, which documentdb's
#     smoke below passes. Requested
#     names keep their order and stay first (the regress path drives
#     EXT_NAMES[0]), with dependencies appended, deduped.
# Skipped for a --registered-only run, which requests no built-ins.
if (( ${#EXT_NAMES[@]} > 0 )); then
_primary="${EXT_NAMES[0]}"
EXT_NAMES_STR="$(python3 - "$MANIFEST" "${EXT_NAMES[@]}" <<'INNERPY'
import sys, yaml
m = yaml.safe_load(open(sys.argv[1]))
rows = {e.get("name"): e for e in m.get("extensions", [])}
out, seen, queue = [], set(), list(sys.argv[2:])
while queue:
    n = queue.pop(0)
    if n in seen:
        continue
    if n not in rows:
        sys.exit("extension %r not found in %s" % (n, sys.argv[1]))
    seen.add(n)
    out.append(n)
    queue.extend(rows[n].get("requires") or [])
print(",".join(out))
INNERPY
)" || exit 1
IFS=',' read -ra EXT_NAMES <<< "$EXT_NAMES_STR"
[[ "${EXT_NAMES[0]}" == "$_primary" ]] || { echo "closure reordered the primary" >&2; exit 1; }
(( ${#EXT_NAMES[@]} > 1 )) && echo ">>> co-mounting: ${EXT_NAMES[*]}"
fi

declare -a EXT_VERSIONS EXT_CONTROLS EXT_PRELOADS EXT_IMAGES EXT_PREBUILT EXT_LIBRARIES
declare -a EXT_SHARE EXT_LIB EXT_LIBPATH EXT_ENVPATHS
ABI_EXEMPT=()
for _n in "${EXT_NAMES[@]}"; do
    IFS=$'\t' read -r _reg _ver _ctrl _pre _lib < <(read_row "$_n")
    [[ -n "$REGISTRY_OVERRIDE" ]] && _reg="$REGISTRY_OVERRIDE"
    [[ -n "$_ver" ]] || { echo "no version for '$_n' in $MANIFEST" >&2; exit 1; }
    EXT_VERSIONS+=("$_ver"); EXT_CONTROLS+=("$_ctrl"); EXT_PRELOADS+=("$_pre")
    EXT_LIBRARIES+=("$_lib")
    EXT_IMAGES+=("${_reg}/amp-ext-${_n}:${_ver}"); EXT_PREBUILT+=("")
    # An amp-built image is always the layout deploy/extensions produces.
    EXT_SHARE+=("share/postgresql"); EXT_LIB+=("lib/postgresql")
    EXT_LIBPATH+=(""); EXT_ENVPATHS+=("")
    echo ">>> extension=$_n version=$_ver control=$_ctrl preload=$_pre image=${_reg}/amp-ext-${_n}:${_ver}"
done

# 1b) REGISTERED extensions. In a deployment these are rows the controller stores
#     from a registerExtension call and renders into the chart; a local compute has
#     no controller, so read the same JSON body directly and apply it by hand. This
#     is the only path that carries a third-party image reference, and it carries it
#     from a fixture rather than from anything in the product.
read_registered() {
    python3 - "$1" <<'PY'
import sys, json
d = json.load(open(sys.argv[1]))
name = d.get("name") or ""
if not name:
    sys.exit("%s: no \"name\"" % sys.argv[1])
if not d.get("image"):
    sys.exit("%s: no \"image\"" % sys.argv[1])
layout = d.get("layout") or {}
env_paths = layout.get("env_paths") or {}
print("\t".join([
    name,
    d["image"],
    d.get("control") or name,
    "true" if d.get("preload") else "false",
    layout.get("share_path") or "share/postgresql",
    layout.get("lib_path") or "lib/postgresql",
    ",".join(layout.get("library_path") or []),
    ",".join("%s=%s" % (k, v) for k, v in sorted(env_paths.items())),
    ",".join(d.get("abi_extra_exempt") or []),
]))
PY
}
for _f in "${REGISTERED_FILES[@]:-}"; do
    [[ -n "$_f" ]] || continue
    # Accept a path relative to this script's directory, so the fixtures are
    # nameable as `ext_fixtures/postgis.json`.
    [[ -f "$_f" ]] || _f="$REPO/test/integration/compute/$_f"
    [[ -f "$_f" ]] || { echo "no such --registered file: $_f" >&2; exit 2; }
    IFS=$'\t' read -r _n _img _ctrl _pre _share _lib _libpath _envpaths _abi < <(read_registered "$_f")
    EXT_NAMES+=("$_n"); EXT_VERSIONS+=(""); EXT_CONTROLS+=("$_ctrl"); EXT_PRELOADS+=("$_pre")
    # Parallel to EXT_CONTROLS, so the preload below indexes both lists the same
    # way. A registration carries no separate library name: it preloads under
    # its control name.
    EXT_LIBRARIES+=("$_ctrl")
    EXT_IMAGES+=("$_img"); EXT_PREBUILT+=("$_img")
    EXT_SHARE+=("$_share"); EXT_LIB+=("$_lib")
    EXT_LIBPATH+=("$_libpath"); EXT_ENVPATHS+=("$_envpaths")
    [[ -n "$_abi" ]] && ABI_EXEMPT+=("$_abi")
    echo ">>> registered=$_n image=$_img control=$_ctrl preload=$_pre share=$_share lib=$_lib"
done

PRIMARY_EXT="${EXT_NAMES[0]}"
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
LOADER_DIRS=()
ENV_NAMES=()
for _i in "${!EXT_NAMES[@]}"; do
    _n="${EXT_NAMES[$_i]}"; _img="${EXT_IMAGES[$_i]}"
    if [[ -n "${EXT_PREBUILT[$_i]}" ]]; then
        # Registered: a prebuilt image, so there is nothing to compile. The
        # endpoint's ImageVolume would pull the same reference.
        echo ">>> pulling registered extension image ($_n): $_img"
        docker pull "$_img"
    else
        echo ">>> building extension image ($_n) via deploy/extensions/build.sh"
        BUILD_ARGS=()
        [[ -n "$REGISTRY_OVERRIDE" ]] && BUILD_ARGS+=(--registry "$REGISTRY_OVERRIDE")
        [[ $CACHE == 1 ]] && BUILD_ARGS+=(--cache)
        bash "$REPO/deploy/extensions/build.sh" "${BUILD_ARGS[@]}" "$_n"
    fi

    _dir="$(mktemp -d)"
    EXT_DIRS+=("$_dir")
    echo ">>> extracting $_img -> $_dir"
    # Export the whole filesystem rather than `docker cp`ing known subtrees: a
    # registered image also carries dirs that are neither share nor lib (the
    # system libraries it bundles). The image is FROM scratch with no
    # CMD/ENTRYPOINT and `docker create` refuses one without a command, so pass a
    # placeholder — it is only recorded in the container config, never run.
    _cid="$(docker create "$_img" /extract-only)"
    docker export "$_cid" | tar -C "$_dir" -xf - >/dev/null 2>&1 || true
    docker rm "$_cid" >/dev/null
    _share="${EXT_SHARE[$_i]}"; _lib="${EXT_LIB[$_i]}"
    [[ -d "$_dir/$_share/extension" ]] || { echo "extracted tree missing $_share/extension" >&2; exit 1; }
    [[ -d "$_dir/$_lib" ]] || { echo "extracted tree missing $_lib" >&2; exit 1; }
    CTRL_PATH_PARTS+=("$_dir/$_share")
    LIB_PATH_PARTS+=("$_dir/$_lib")
    # Dirs of system shared libraries the .so links that live only in the image.
    # In a pod these reach the loader through EXTENSION_LIBRARY_PATH, which the
    # supervisor appends to LD_LIBRARY_PATH; here the compute is a bare process,
    # so they go straight onto LD_LIBRARY_PATH below.
    if [[ -n "${EXT_LIBPATH[$_i]}" ]]; then
        IFS=',' read -ra _lps <<< "${EXT_LIBPATH[$_i]}"
        for _lp in "${_lps[@]}"; do
            [[ -d "$_dir/$_lp" ]] || { echo "extracted tree missing library_path dir $_lp" >&2; exit 1; }
            # Path only what this host CANNOT already resolve.
            #
            # LD_LIBRARY_PATH is searched before the system directories, so the
            # image's bundled libraries shadow the host's for EVERY module, not
            # just this extension. In a pod that is harmless: the endpoint image
            # and the extension image share a base, which the registration's tag
            # rule already enforces. Here they need not — a CNPG bookworm image
            # on an ubuntu runner shadows the host libcurl with a bookworm one
            # that wants libldap-2.5, and the FIRST casualty is yb_amp.so, which
            # links libcurl too and has nothing to do with this extension.
            #
            # So symlink through a filtered dir: anything ldconfig already knows
            # stays the host's, anything it does not (geos, proj, gdal, …) comes
            # from the image. On a matching base the two sets are identical, so
            # this changes nothing there and makes the run portable here.
            _filtered="$_dir/.loader/$(basename "$_lp")"
            mkdir -p "$_filtered"
            for _so in "$_dir/$_lp"/*.so*; do
                [[ -e "$_so" ]] || continue
                _base="$(basename "$_so")"
                if ldconfig -p 2>/dev/null | awk "{print \$1}" | grep -qxF "$_base"; then continue; fi
                ln -sf "$_so" "$_filtered/$_base"
            done
            echo ">>> bundled libs: $(find "$_filtered" -maxdepth 1 -type l | wc -l) of $(find "$_dir/$_lp" -maxdepth 1 -name '*.so*' | wc -l) in $_lp not resolvable on this host"
            LOADER_DIRS+=("$_filtered")
        done
    fi
    # Runtime data dirs the extension's libraries read (PROJ_DATA, GDAL_DATA).
    # Exported so every process this script starts inherits them — what the
    # chart's container env does in a pod.
    if [[ -n "${EXT_ENVPATHS[$_i]}" ]]; then
        IFS=',' read -ra _eps <<< "${EXT_ENVPATHS[$_i]}"
        for _ep in "${_eps[@]}"; do
            _k="${_ep%%=*}"; _v="${_ep#*=}"
            [[ -d "$_dir/$_v" ]] || { echo "extracted tree missing env path $_v ($_k)" >&2; exit 1; }
            export "$_k=$_dir/$_v"
            ENV_NAMES+=("$_k")
            echo ">>> $_k=$_dir/$_v"
        done
    fi
    # The library, not the control name: documentdb's one image ships
    # pg_documentdb_core.so + pg_documentdb.so, so preloading its CREATE EXTENSION
    # name is a `could not access file` FATAL. Comma-separated values are passed
    # through as-is; shared_preload_libraries takes a list.
    [[ "${EXT_PRELOADS[$_i]}" == true ]] && PRELOAD_LIBS+=("${EXT_LIBRARIES[$_i]}")
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
# Hand the bundled library dirs to the SUPERVISOR, exactly as the chart does in
# a pod, rather than exporting LD_LIBRARY_PATH here: yb_amp_local scrubs the
# environment and rebuilds it for the supervisor, and pgLibEnv owns
# LD_LIBRARY_PATH and overwrites it — an exported value never reaches the
# compute. Going through EXTENSION_LIBRARY_PATH means this run exercises the
# same append the pod path does (see append_ld_library_path in the supervisor).
# EXTENSION_ENV names the data-dir variables to forward through the same scrub.
if [[ ${#LOADER_DIRS[@]} -gt 0 ]]; then
    _ext_libs="$(IFS=:; echo "${LOADER_DIRS[*]}")"
    export EXTENSION_LIBRARY_PATH="$_ext_libs"
    echo ">>> EXTENSION_LIBRARY_PATH=$EXTENSION_LIBRARY_PATH"
fi
if [[ ${#ENV_NAMES[@]} -gt 0 ]]; then
    _ext_envs="$(IFS=,; echo "${ENV_NAMES[*]}")"
    export EXTENSION_ENV="$_ext_envs"
    echo ">>> EXTENSION_ENV=$EXTENSION_ENV"
fi

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
        grep -nE 'extension_control_path|dynamic_library_path|shared_preload_libraries|abi_extra_exempt' "$f" 2>/dev/null || true
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
# The settings the extension's own regress suite expects from the --temp-config
# it would normally start a private instance with. They go in the endpoint's
# postgresql.conf rather than a per-session PGOPTIONS because some are only read
# once, when a library loads: documentdb caches whether to emit extended-RUM
# explain output at rum library load time (index_am/rum.c reads
# DisableExtendedRumExplainPlans there and stores the callback), so a USERSET
# GUC arriving per session is too late to change it. Read here, before the
# endpoint is created; only the primary extension's are used.
declare -a REG_PG_SETTINGS=()
if [[ $REGRESS == 1 ]]; then
    while IFS= read -r _kv; do
        [[ -n "$_kv" ]] && REG_PG_SETTINGS+=(--pg-setting "$_kv")
    done < <(python3 - "$MANIFEST" "$PRIMARY_EXT" <<'REGPY'
import sys, yaml
m = yaml.safe_load(open(sys.argv[1]))
for e in m.get("extensions", []):
    if e.get("name") == sys.argv[2]:
        for kv in ((e.get("regress") or {}).get("pg_settings") or []):
            print(kv)
        break
REGPY
)
    (( ${#REG_PG_SETTINGS[@]} )) && echo ">>> regress pg_settings ($PRIMARY_EXT): $((${#REG_PG_SETTINGS[@]} / 2))"
fi

PG_SETTINGS=(
    --pg-setting "extension_control_path=$CTRL_PATH"
    --pg-setting "dynamic_library_path=$LIB_PATH"
    # amp compute defaults compute_query_id=on, so EXPLAIN (VERBOSE) prints a
    # non-deterministic "Query Identifier:" line. Upstream regress suites assume
    # the stock default (off), so restore it — otherwise AGE's expr/cypher_with
    # EXPLAIN tests diverge on a value that can't be blessed. Harmless elsewhere.
    --pg-setting "compute_query_id=off"
    "${REG_PG_SETTINGS[@]}"
)
# A registered image built against stock PG18 carries abi_extra "PostgreSQL"
# where this fork carries "yb_amp Postgres", and the server refuses it outright
# ("incompatible library: ABI mismatch") unless the module is waived. The chart
# renders the same GUC from the registration's abi_extra_exempt list. Comma-
# separated (a list of module names), like shared_preload_libraries.
if [[ ${#ABI_EXEMPT[@]} -gt 0 ]]; then
    PG_SETTINGS+=(--pg-setting "yb_amp_abi_extra_exempt_libraries=$(IFS=,; echo "${ABI_EXEMPT[*]}")")
fi
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
        emit("REG_DIR", r.get("dir"))
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
        # Where the regress Makefile lives. For a single-module extension that is
        # the tarball root, which is the default. documentdb is a multi-module
        # tree: its REGRESS sits in pg_documentdb/src/test/regress, and its ROOT
        # Makefile is a delegating one whose `%:` catch-all would swallow the
        # print target below and echo nothing — which surfaced as the misleading
        # "no REGRESS tests found in Makefile".
        REG_SRC="$SRC_DIR${REG_DIR:+/$REG_DIR}"
        [[ -f "$REG_SRC/Makefile" ]] || {
            echo ">>> FAIL $EXT_NAME: no Makefile at ${REG_DIR:-<root>} (regress.dir)" >&2; exit 1; }
        # Evaluate REGRESS + REGRESS_OPTS from the extension's own Makefile (they
        # use make functions like $(wildcard ...), so read them through make
        # rather than parse). Then strip --temp-instance/--temp-config: those
        # would spin up a PRIVATE temp server (pg_cron's Makefile does), but we
        # want the suite driven at the live amp compute.
        PG_CONFIG="$PGINST/bin/pg_config"
        # A throwaway wrapper makefile that includes the extension's own Makefile
        # (which ends in `include $(PGXS)`) and adds a target to print any var.
        WRAP="$REG_SRC/.amp_regress.mk"
        cat > "$WRAP" <<'MK'
include Makefile
amp-print-%:
	@echo '$($*)'
MK
        REGRESS_TESTS="$(make -s -C "$REG_SRC" -f "$WRAP" amp-print-REGRESS PG_CONFIG="$PG_CONFIG")"
        REGRESS_OPTS="$(make -s -C "$REG_SRC" -f "$WRAP" amp-print-REGRESS_OPTS PG_CONFIG="$PG_CONFIG")"
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
        INPUTDIR="$REG_SRC"
        OUTPUTDIR="$REG_SRC"   # pg_regress writes regression.diffs here (CWD default)
        drop_value=0
        for tok in $REGRESS_OPTS; do
            if (( drop_value )); then drop_value=0; continue; fi
            case "$tok" in
                --temp-instance=*|--temp-config=*|--port=*) ;;
                --temp-instance|--temp-config|--port) drop_value=1 ;;
                --inputdir=*) v="${tok#--inputdir=}"; [[ $v == /* ]] && INPUTDIR="$v" || INPUTDIR="$REG_SRC/$v"; KEPT_OPTS+=("$tok") ;;
                --outputdir=*) v="${tok#--outputdir=}"; [[ $v == /* ]] && OUTPUTDIR="$v" || OUTPUTDIR="$REG_SRC/$v"; KEPT_OPTS+=("$tok") ;;
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
        # A suite that ships a bin/ beside its Makefile puts it first on PATH,
        # because pg_regress shells out to `diff` to compare expected against
        # results and that is the hook suites use to normalize before comparing.
        # documentdb's bin/diff runs normalize.sed over BOTH files (so
        # non-deterministic values — now(), ids, LSNs — compare equal) and calls
        # the real diff with -Z, ignoring trailing whitespace. Upstream's own
        # Makefile does exactly this (`export PATH := $(MAKEFILE_DIR)bin:$(PATH)`);
        # driving pg_regress directly skips it, which shows up as diffs that are
        # only a timestamp or a column's padding.
        # Keyed on bin/diff specifically, not on bin/ existing: that is the only
        # program pg_regress resolves through PATH, so a suite shipping some
        # unrelated bin/ must not have it shadow the system tools for everything
        # else. Absent for pgvector, pg_cron, hexabridge and age, which keep the
        # plain diff they pass with today.
        REG_PATH="$PATH"
        if [[ -x "$REG_SRC/bin/diff" ]]; then
            REG_PATH="$REG_SRC/bin:$PATH"
            echo ">>> regress diff ($EXT_NAME): using the suite's own $REG_SRC/bin/diff"
            # amp's own log lines are not part of any extension's expected
            # output. A test that lowers client_min_messages to see the
            # extension's messages also sees the storage layer's — yb_amp's smgr
            # DEBUG lines and the endpoint's LOG lines — which no upstream
            # expected file contains. Filter them from BOTH sides rather than
            # bless them: the smgr line carries an LSN, so a blessed file would
            # freeze one run's value and fail on the next.
            #
            # bin/diff prefers normalize_modified.sed when present, its own hook
            # for "running tests on an existing cluster", which is exactly this.
            # Generated from the suite's normalize.sed each run so upstream's
            # rules stay authoritative and only the amp lines are appended.
            if [[ -f "$REG_SRC/bin/normalize.sed" ]]; then
                {
                    cat "$REG_SRC/bin/normalize.sed"
                    echo '/^DEBUG:  \[YBAMP_SMGR\]/d'
                    echo '/^LOG:  ConsoleURL not set, skipping forwarding$/d'
                } > "$REG_SRC/bin/normalize_modified.sed"
                echo ">>> regress normalize ($EXT_NAME): amp log lines filtered from both sides"
            fi
        fi
        if ! ( cd "$REG_SRC" && export PATH="$REG_PATH" && "$PG_REGRESS" \
            --bindir="$PGINST/bin" \
            --host=127.0.0.1 --port="$COMPUTE" --user=cloud_admin \
            --dbname="amp_regress_${EXT_NAME}" \
            "${KEPT_OPTS[@]}" $REGRESS_TESTS ); then
            # Copy what the run produced somewhere CI can upload it. Blessing a
            # by-design diff means committing the ACTUAL output
            # (ext_regress_expected/<name>/<test>_N.out), and the results live in
            # a mktemp dir teardown removes — reconstructing them from the printed
            # diff is how a blessed file ends up subtly wrong.
            #
            # NOT under YB_AMP_REPO_DIR: clean.sh deletes that whole directory on
            # exit, which is why the cluster logs are printed rather than uploaded.
            # A sibling survives to the upload step.
            _rd="$REPO/.integration-tests/regress-${EXT_NAME}_${RUN_ID}"
            mkdir -p "$_rd"
            [[ -d "$REG_SRC/results" ]] && cp -a "$REG_SRC/results" "$_rd/" 2>/dev/null || true
            for _f in "$OUTPUTDIR/regression.diffs" "$REG_SRC/regression.diffs"; do
                [[ -f "$_f" ]] && cp -a "$_f" "$_rd/regression.diffs" 2>/dev/null && break
            done
            echo ">>> regress artifacts for $EXT_NAME under $_rd ($(ls "$_rd" | tr '\n' ' '))"
            for diffs in "$OUTPUTDIR/regression.diffs" "$REG_SRC/regression.diffs" \
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
elif [[ "$EXT_NAME" == postgis ]]; then
    # PostGIS, mounted from the registered CloudNativePG image
    # (ext_fixtures/postgis.json) rather than anything this repo builds. Three
    # things are under test that no built-in extension exercises:
    #
    #  * the ABI waiver — the .so is built against stock PG18, so without
    #    yb_amp_abi_extra_exempt_libraries the CREATE below fails at load with
    #    "incompatible library: ABI mismatch". Reaching any geometry function
    #    proves the module loaded.
    #  * the bundled system libraries — geos/proj/gdal live only in the image.
    #  * their data dirs — ST_Transform needs proj.db, found only via PROJ_DATA.
    #    A build that resolved libproj but not its database still answers
    #    ST_AsText, so the transform is the assertion that matters.
    #
    # Values: ST_Area of the unit square is 1; EPSG:4326 -> 3857 sends longitude
    # 180 to x = 20037508.34 (half the Web Mercator world), a spec-defined
    # constant rather than a fixture.
    out=$("${PSQL[@]}" <<'SQL' 2>&1
CREATE EXTENSION IF NOT EXISTS postgis;
SELECT 'VERSION:'||extversion FROM pg_extension WHERE extname='postgis';
SELECT 'AREA:'||round(ST_Area(ST_GeomFromText('POLYGON((0 0,1 0,1 1,0 1,0 0))'))::numeric, 3);
DROP TABLE IF EXISTS amp_postgis_probe;
CREATE TABLE amp_postgis_probe (id serial PRIMARY KEY, geom geometry(Point, 4326));
INSERT INTO amp_postgis_probe (geom) VALUES
  (ST_SetSRID(ST_MakePoint(0, 0), 4326)),
  (ST_SetSRID(ST_MakePoint(180, 0), 4326));
CREATE INDEX amp_postgis_probe_gix ON amp_postgis_probe USING GIST (geom);
CHECKPOINT;
SET enable_seqscan = off;
SELECT 'NEAR:'||id FROM amp_postgis_probe
  ORDER BY geom <-> ST_SetSRID(ST_MakePoint(1, 0), 4326) LIMIT 1;
SELECT 'MERCX:'||round(ST_X(ST_Transform(geom, 3857))::numeric, 2)
  FROM amp_postgis_probe ORDER BY id DESC LIMIT 1;
SQL
)
    if echo "$out" | grep -q '^VERSION:3.6.2$' \
       && echo "$out" | grep -q '^AREA:1.000$' \
       && echo "$out" | grep -q '^NEAR:1$' \
       && echo "$out" | grep -q '^MERCX:20037508.34$'; then
        echo ">>> PASS postgis: v3.6.2 + ST_Area=1 + GiST NN + ST_Transform(4326->3857) x=20037508.34"
    else
        echo ">>> FAIL postgis checks; output:" >&2
        echo "$out" >&2
        exit 1
    fi
elif [[ "$EXT_NAME" == documentdb ]]; then
    # documentdb's control file requires documentdb_core, pg_cron,
    # tsm_system_rows and vector, and CREATE EXTENSION installs none of them
    # without CASCADE — the generic smoke below would fail on
    # `required extension "documentdb_core" is not installed`. The `requires`
    # closure above has already mounted pgvector and pg_cron for CASCADE to find.
    #
    # Past registration, it asserts the document path the gateway drives:
    # insert through documentdb_api, then query with a filter, which goes
    # through bson and the @@ operator rather than plain SQL. bsonUseEJson
    # prints the insert result as JSON instead of BSONHEX.
    out=$("${PSQL[@]}" <<'SQL' 2>&1
CREATE EXTENSION IF NOT EXISTS documentdb CASCADE;
SET documentdb_core.bsonUseEJson = on;
SELECT 'INSERT:'||documentdb_api.insert_one('amp_smoke', 'probe', '{ "_id": 1, "a": 1 }')::text;
SELECT 'INSERT:'||documentdb_api.insert_one('amp_smoke', 'probe', '{ "_id": 2, "a": 2 }')::text;
SELECT 'MATCH:'||object_id::text FROM documentdb_api.collection('amp_smoke', 'probe')
  WHERE document OPERATOR(documentdb_api_catalog.@@) '{ "a": 2 }';
SELECT 'TOTAL:'||count(*) FROM documentdb_api.collection('amp_smoke', 'probe');
SQL
)
    if [[ "$(echo "$out" | grep -c '^INSERT:{ "n" : { "$numberInt" : "1" }, "ok" : { "$numberDouble" : "1.0" } }$')" == 2 ]] \
       && echo "$out" | grep -q '^MATCH:{ "" : { "$numberInt" : "2" } }$' \
       && echo "$out" | grep -q '^TOTAL:2$'; then
        echo ">>> PASS documentdb: CREATE EXTENSION ... CASCADE + insert_one x2 + filtered find"
    else
        echo ">>> FAIL documentdb checks; output:" >&2
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
