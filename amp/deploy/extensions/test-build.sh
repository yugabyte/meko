#!/usr/bin/env bash
# Test the yb-amp extension build pipeline.
#
# Two tiers:
#
#   (cheap, default) static checks — always run, no docker, seconds:
#     * shellcheck + `bash -n` on build.sh and this script
#     * parse extensions.yaml (python + PyYAML) and validate every entry has the
#       required fields (name/version/url) and a sane shape
#     * every URL a row fetches (its `url`, and any https URL in its `prep`) is
#       pinned in downloads_checksum.txt, which `scripts/safe_curl.sh check`
#       validates
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
amp_root="$(cd "$here/../.." && pwd)"
checksums="$amp_root/downloads_checksum.txt"

do_docker=false
name="pgvector"
while [[ $# -gt 0 ]]; do
  case "$1" in
  --docker)
    do_docker=true
    shift
    ;;
  -h | --help)
    sed -n '2,24p' "$0"
    exit 0
    ;;
  -*)
    echo "unknown flag: $1" >&2
    exit 2
    ;;
  *)
    name="$1"
    shift
    ;;
  esac
done

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

echo ">> static: bash -n"
bash -n "$here/build.sh"
bash -n "$0"

if command -v shellcheck >/dev/null 2>&1; then
  echo ">> static: shellcheck"
  shellcheck "$here/build.sh" "$0" "$amp_root/scripts/safe_curl.sh"
else
  echo ">> static: shellcheck not installed, skipping"
fi

echo ">> static: validate $checksums"
sh "$amp_root/scripts/safe_curl.sh" check

echo ">> static: validate $manifest"
python3 - "$manifest" "$checksums" <<'PY'
import re, sys, yaml
with open(sys.argv[1]) as f:
    m = yaml.safe_load(f)
pinned = set()
with open(sys.argv[2]) as f:
    for line in f:
        fields = line.split()
        if len(fields) == 2 and not fields[0].startswith("#"):
            pinned.add(fields[1])
assert isinstance(m, dict), "manifest must be a mapping"
for key in ("registry", "base_image"):
    assert m.get(key), f"manifest missing top-level {key!r}"
exts = m.get("extensions") or []
assert exts, "manifest has no extensions"
seen = set()
url_re = re.compile(r"https?://[^\s'\"]+")
lib_re = re.compile(r"^[A-Za-z0-9_+-]+$")
guc_re = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$")
shell_meta_re = re.compile(r"[;&|<>$`(){}\[\]!*?~]")
for e in exts:
    name = e.get("name")
    assert name, f"extension entry missing name: {e!r}"
    assert name not in seen, f"duplicate extension {name!r}"
    seen.add(name)
    for key in ("version", "url"):
        assert e.get(key), f"{name}: missing required {key!r}"
    # Checksums moved to downloads_checksum.txt; a leftover one here would be a
    # second pin that nothing checks.
    assert "sha256" not in e, f"{name}: sha256 belongs in downloads_checksum.txt, not here"
    assert str(e["url"]).startswith("https://"), f"{name}: url must be https"
    assert e["url"] in pinned, f"{name}: {e['url']} is not pinned in downloads_checksum.txt"
    # A prep that downloads has to go through $SAFE_CURL, and what it fetches has
    # to be pinned too — otherwise the build would fail only when it runs.
    prep = str(e.get("prep") or "")
    assert not re.search(r"\b(curl|wget)\b", prep), f"{name}: prep must fetch with `sh \"$SAFE_CURL\" fetch`, not curl/wget"
    for u in url_re.findall(prep):
        assert u in pinned, f"{name}: prep fetches {u}, which is not pinned in downloads_checksum.txt"
    # image_revision: counts rebuilds of one version from a changed recipe, and
    # makes the tag <version>-r<N> from 2 on. build.sh refuses an existing tag
    # built from another recipe, so this is what a recipe change must bump.
    rev = e.get("image_revision")
    if rev is not None:
        assert isinstance(rev, int) and not isinstance(rev, bool) and rev >= 1, \
            f"{name}: image_revision must be an integer >= 1, got {rev!r}"
    # settings: server settings an endpoint mounting it starts with, merged into
    # the compute's settings by the chart (from the catalog) and by the harness.
    settings = e.get("settings")
    if settings is not None:
        assert isinstance(settings, dict) and settings, f"{name}: settings must be a non-empty mapping"
        for k, v in settings.items():
            assert guc_re.match(str(k)), f"{name}: settings key {k!r} is not a GUC name"
            assert isinstance(v, (str, int, float)) and not isinstance(v, bool) and str(v) != "", \
                f"{name}: settings {k} must be a scalar value"
    # patch, if named, must exist under compute/image/patches/
    patch = e.get("patch")
    if patch:
        import os
        p = os.path.join(os.path.dirname(sys.argv[1]), "..", "..", "compute", "image", "patches", patch)
        assert os.path.isfile(p), f"{name}: patch {patch!r} not found at {p}"
    # library: what goes in shared_preload_libraries when it is not the control
    # name. A typo is invisible until an endpoint's postmaster refuses to start
    # with `could not access file`, so check the shape: comma-separated bare
    # library names, no path and no .so suffix.
    lib = e.get("library")
    if lib is not None:
        assert isinstance(lib, str) and lib, f"{name}: library must be a non-empty string"
        assert e.get("preload"), f"{name}: library is only meaningful with preload: true"
        for one in lib.split(","):
            assert one, f"{name}: library has an empty entry: {lib!r}"
            assert lib_re.match(one), (
                f"{name}: library entry {one!r} must be a bare library name "
                "(no path, no .so suffix)")
    # make_targets is interpolated unquoted into the build's make command line.
    mt = e.get("make_targets")
    if mt is not None:
        assert isinstance(mt, str) and mt, f"{name}: make_targets must be a non-empty string"
        assert not shell_meta_re.search(mt), (
            f"{name}: make_targets {mt!r} contains shell metacharacters")
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
        # dir: where the regress Makefile lives, relative to the tarball root. An
        # absolute path or a .. escape would read outside the fetched source.
        rdir = reg.get("dir")
        if rdir is not None:
            assert driver == "pgxs", f"{name}: regress.dir only applies to driver=pgxs"
            assert isinstance(rdir, str) and rdir, f"{name}: regress.dir must be a non-empty string"
            assert not rdir.startswith("/"), f"{name}: regress.dir must be relative, got {rdir!r}"
            assert ".." not in rdir.split("/"), f"{name}: regress.dir must not escape the source tree"
        # pg_settings become --pg-setting args on the endpoint, i.e. lines in the
        # compute's postgresql.conf. A malformed entry is a postmaster that will
        # not start, so check the shape: one key=value per list item, a
        # qualified GUC name, and no whitespace to be word-split.
        pgset = reg.get("pg_settings")
        if pgset is not None:
            assert driver == "pgxs", f"{name}: regress.pg_settings only applies to driver=pgxs"
            assert isinstance(pgset, list) and pgset, f"{name}: regress.pg_settings must be a non-empty list"
            for kv in pgset:
                assert isinstance(kv, str) and kv, f"{name}: regress.pg_settings has an empty entry"
                assert kv.count("=") >= 1, f"{name}: regress.pg_settings {kv!r} is not key=value"
                k = kv.split("=", 1)[0]
                assert k and not any(c.isspace() for c in kv), (
                    f"{name}: regress.pg_settings {kv!r} must not contain whitespace")
                assert guc_re.match(k), f"{name}: regress.pg_settings key {k!r} is not a GUC name"
# requires: other extensions that must be mounted alongside this one, or CREATE
# EXTENSION fails with `extension "X" is not available`. Checked after the loop,
# when every name is known. The controller catalog mirrors this list.
for e in exts:
    name = e["name"]
    req = e.get("requires")
    if req is None:
        continue
    assert isinstance(req, list) and req, f"{name}: requires must be a non-empty list"
    for r in req:
        assert isinstance(r, str) and r, f"{name}: requires has a non-string entry: {req!r}"
        assert r != name, f"{name}: requires itself"
        assert r in seen, (
            f"{name}: requires {r!r}, which is not an extension in this manifest "
            "(use the manifest name, not the CREATE EXTENSION name)")
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
read -r tag control library < <(
  python3 - "$manifest" "$name" <<'PY'
import sys, yaml
with open(sys.argv[1]) as f:
    m = yaml.safe_load(f)
for e in m.get("extensions", []):
    if e.get("name") == sys.argv[2]:
        # library, when set, names the shared objects the image actually ships;
        # they need not include <control>.so at all.
        rev = int(e.get("image_revision") or 1)
        tag = str(e["version"]) + ("-r%d" % rev if rev > 1 else "")
        print(tag, e.get("control") or e["name"], e.get("library") or "")
        break
else:
    sys.exit(f"no extension named {sys.argv[2]!r} in manifest")
PY
)

reg="amptest"
image="${reg}/amp-ext-${name}:${tag}"
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
# The .so is named for the control basename only when the image ships exactly
# one. documentdb's ships pg_documentdb_core.so, pg_documentdb.so and
# pg_documentdb_extended_rum.so and no documentdb.so — which is precisely why it
# sets `library` — so assert the libraries that field names instead, or this
# fails on a correctly built image.
want=("share/postgresql/extension/${control}.control")
if [ -n "$library" ]; then
  IFS=',' read -ra libs <<<"$library"
  for l in "${libs[@]}"; do want+=("lib/postgresql/${l}.so"); done
else
  want+=("lib/postgresql/${control}.so")
fi
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
