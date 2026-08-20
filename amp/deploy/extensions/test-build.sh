#!/usr/bin/env bash
# Test the yb-amp extension build pipeline.
#
# Two tiers:
#
#   (cheap, default) static checks — always run, no docker, seconds:
#     * shellcheck + `bash -n` on build.sh and this script
#     * parse extensions.yaml (python + PyYAML) and validate every entry has the
#       required fields (name/version/url/sha256) and a sane shape
#
#   (heavy, opt-in) --docker — actually build an extension image via build.sh
#     (cold, compiles the ABI-anchor PG18 base — many minutes) and assert the
#     resulting `FROM scratch` tree carries the expected control/SQL/.so files.
#     Reserved for the GKE workflow / a manual run; not for the fast unit gate.
#
# Usage:
#   deploy/extensions/test-build.sh [--docker] [NAME]
#     --docker   also build NAME (default: pgvector) and assert its image tree
#
# Requires: python3 + PyYAML always; shellcheck if present (else skipped);
# docker only with --docker.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
manifest="$here/extensions.yaml"

do_docker=false
name="pgvector"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --docker) do_docker=true; shift ;;
    -h|--help) sed -n '2,24p' "$0"; exit 0 ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *) name="$1"; shift ;;
  esac
done

fail() { echo "FAIL: $*" >&2; exit 1; }

echo ">> static: bash -n"
bash -n "$here/build.sh"
bash -n "$0"

if command -v shellcheck >/dev/null 2>&1; then
  echo ">> static: shellcheck"
  shellcheck "$here/build.sh" "$0"
else
  echo ">> static: shellcheck not installed, skipping"
fi

echo ">> static: validate $manifest"
python3 - "$manifest" <<'PY'
import re, sys, yaml
with open(sys.argv[1]) as f:
    m = yaml.safe_load(f)
assert isinstance(m, dict), "manifest must be a mapping"
for key in ("registry", "base_image"):
    assert m.get(key), f"manifest missing top-level {key!r}"
exts = m.get("extensions") or []
assert exts, "manifest has no extensions"
seen = set()
sha_re = re.compile(r"^[0-9a-f]{64}$")
for e in exts:
    name = e.get("name")
    assert name, f"extension entry missing name: {e!r}"
    assert name not in seen, f"duplicate extension {name!r}"
    seen.add(name)
    for key in ("version", "url", "sha256"):
        assert e.get(key), f"{name}: missing required {key!r}"
    assert sha_re.match(str(e["sha256"])), f"{name}: sha256 is not 64 hex chars"
    assert str(e["url"]).startswith(("http://", "https://")), f"{name}: url must be http(s)"
    # patch, if named, must exist under compute/image/patches/
    patch = e.get("patch")
    if patch:
        import os
        p = os.path.join(os.path.dirname(sys.argv[1]), "..", "..", "compute", "image", "patches", patch)
        assert os.path.isfile(p), f"{name}: patch {patch!r} not found at {p}"
    # regress, if present, drives the upstream regression suite (extensions.sh
    # --regress). driver is required and one of pgxs|script; script driver needs
    # a script path (relative to the source tree).
    reg = e.get("regress")
    if reg is not None:
        assert isinstance(reg, dict), f"{name}: regress must be a mapping"
        driver = reg.get("driver")
        assert driver in ("pgxs", "script"), f"{name}: regress.driver must be pgxs|script, got {driver!r}"
        if driver == "script":
            assert reg.get("script"), f"{name}: regress.driver=script needs a script path"
print(f"   ok: {len(exts)} extension(s): {', '.join(sorted(seen))}")
PY

if ! $do_docker; then
  echo "static checks passed (run with --docker to build + assert an image tree)"
  exit 0
fi

command -v docker >/dev/null 2>&1 || fail "--docker requires docker on PATH"

# Read the version + control basename for NAME (the control need not equal the
# package name — pgvector ships "vector"), so we know the image tag and the
# files to expect.
read -r version control < <(python3 - "$manifest" "$name" <<'PY'
import sys, yaml
with open(sys.argv[1]) as f:
    m = yaml.safe_load(f)
for e in m.get("extensions", []):
    if e.get("name") == sys.argv[2]:
        print(e["version"], e.get("control") or e["name"]); break
else:
    sys.exit(f"no extension named {sys.argv[2]!r} in manifest")
PY
)

reg="amptest"
image="${reg}/amp-ext-${name}:${version}"
echo ">> docker: building ${image} via build.sh (cold; this compiles PG18 and may take many minutes)"
"$here/build.sh" --registry "$reg" "$name"

echo ">> docker: asserting the image tree"
cid="$(docker create "$image")"
trap 'docker rm -f "$cid" >/dev/null 2>&1 || true' EXIT
tree="$(docker export "$cid" | tar -tf -)"

# Assert the control, a versioned SQL file, and the shared object all landed in
# the two GUC-referenced subdirs (extension_control_path / dynamic_library_path).
# The control file and .so are named for the control basename, but the base SQL
# file's version is the extension's own (e.g. vector--0.8.2.sql), which need NOT
# equal the image version (pg_cron ships pg_cron--1.0.sql + upgrade scripts, not
# pg_cron--1.6.7.sql) — so match any versioned SQL rather than a pinned name.
want=(
  "share/postgresql/extension/${control}.control"
  "lib/postgresql/${control}.so"
)
for w in "${want[@]}"; do
  grep -qx "$w" <<<"$tree" || fail "image ${image} is missing ${w}"
  echo "   ok: ${w}"
done
if grep -qE "^share/postgresql/extension/${control}--[0-9].*\.sql$" <<<"$tree"; then
  echo "   ok: share/postgresql/extension/${control}--<version>.sql"
else
  fail "image ${image} has no ${control}--<version>.sql install script"
fi

echo "docker build + tree assertions passed for ${image}"
