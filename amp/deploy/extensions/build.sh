#!/usr/bin/env bash
# Build (and optionally push) the yb-amp extension ImageVolume images.
#
# Reads deploy/extensions/extensions.yaml, builds the ABI-anchor PG18 base once,
# then each extension into a minimal `FROM scratch` image the endpoint mounts as
# a Kubernetes ImageVolume. Run from anywhere; paths resolve to the repo root.
#
# Usage:
#   deploy/extensions/build.sh [--push] [--no-base-push] [--cache] [--registry REG] [--base-only] [NAME ...]
#
#   --push         also `docker push` every image built (base + extensions)
#   --no-base-push with --push, push only the extension images, NOT the
#                  ABI-anchor base (the base is still built locally to compile
#                  the extensions — nothing pulls it back from the registry, so
#                  publishing it is optional). Ignored without --push.
#   --cache        cache the ~30-min ABI-anchor PG18 base build via a buildx
#                  registry layer cache (<registry>/<base_image>:buildcache):
#                  unchanged inputs restore from it in minutes instead of a cold
#                  rebuild. The base is `--load`ed locally either way, so the
#                  extension builds still `FROM` it. Only the :buildcache tag is
#                  pushed (not the base image, even without --no-base-push), so it
#                  needs a builder that can export registry cache (buildx's
#                  docker-container driver — `docker buildx create --use` or the
#                  setup-buildx-action) and registry login for the cache push. CI
#                  only; a plain local `docker` builder can't export it.
#                  Cache EXPORT is best-effort (ignore-error=true): the registry
#                  login must have PUSH access to the <base_image> repo for the
#                  cache to populate — if it can only pull, the export 401s, the
#                  build still succeeds, but every run stays cold (no speedup).
#   --registry REG override the manifest's registry
#   --base-only    build just the ABI-anchor base image, no extensions
#   NAME ...       build only these extensions (default: all in the manifest)
#
# Requires: docker, python3 with PyYAML (pip install pyyaml). --cache also needs
# a buildx docker-container builder + a registry login.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
manifest="$here/extensions.yaml"

push=false
no_base_push=false
base_only=false
cache=false
registry_override=""
declare -a only=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --push) push=true; shift ;;
    --no-base-push) no_base_push=true; shift ;;
    --base-only) base_only=true; shift ;;
    --cache) cache=true; shift ;;
    --registry) registry_override="${2:?--registry needs a value}"; shift 2 ;;
    -h|--help) sed -n '2,35p' "$0"; exit 0 ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *) only+=("$1"); shift ;;
  esac
done

# The manifest is the source of truth; python emits shell-safe rows so bash never
# has to parse YAML itself. Fields are joined with the ASCII Unit Separator (0x1f),
# NOT a tab: tab is an IFS *whitespace* char, so `read` collapses consecutive tabs
# and drops trailing ones — an extension with empty patch+prep (e.g. pg_cron) would
# have those fields swallowed and everything after them shift left. 0x1f is
# non-whitespace, so `read` preserves empty fields positionally.
read_manifest() {
  python3 - "$manifest" <<'PY'
import sys, yaml
US = "\x1f"
with open(sys.argv[1]) as f:
    m = yaml.safe_load(f)
print("REGISTRY" + US + m.get("registry", ""))
print("BASE_IMAGE" + US + m.get("base_image", "amp-pg18-build"))
print("PG_TAG" + US + m.get("pg_tag", "v18"))
def norm(v):
    if v is None:
        return ""
    if isinstance(v, bool):
        return "true" if v else "false"
    return str(v)
for e in m.get("extensions", []):
    fields = [norm(e.get(k)) for k in
              ("name","version","url","sha256","patch","prep","trusted","dockerfile")]
    print("EXT" + US + US.join(f.replace(US, " ") for f in fields))
PY
}

registry=""; base_image=""; pg_tag=""
declare -a ext_rows=()
while IFS=$'\x1f' read -r tag rest; do
  case "$tag" in
    REGISTRY)   registry="$rest" ;;
    BASE_IMAGE) base_image="$rest" ;;
    PG_TAG)     pg_tag="$rest" ;;
    EXT)        ext_rows+=("$rest") ;;
  esac
done < <(read_manifest)

[[ -n "$registry_override" ]] && registry="$registry_override"

base_local="${base_image}:local"
echo ">> building ABI-anchor base ${base_local}"
if $cache; then
  # Restore the expensive base build from a registry layer cache and refresh it.
  # --load lands the image in the local docker store so the extension builds
  # below (plain `docker build`) can still `FROM` it. Only the :buildcache tag
  # is written to the registry — the base image itself is not published here.
  #
  # ignore-error=true keeps the cache EXPORT best-effort: pushing :buildcache
  # needs write access to the base repo, and if the registry login can only pull
  # (or the repo doesn't exist yet) the export 401s — without this that aborts the
  # whole build. With it, the export just warns and the build still succeeds; the
  # cache stays empty (every run cold) until the repo is writable. cache-from is
  # always safe: a missing/unreadable cache is a miss, not an error.
  base_cache="${registry}/${base_image}:buildcache"
  docker buildx build --load \
    --cache-from "type=registry,ref=${base_cache}" \
    --cache-to "type=registry,ref=${base_cache},mode=max,ignore-error=true" \
    -t "$base_local" \
    -f "$here/pg18-build.Dockerfile" "$repo_root"
else
  docker build -t "$base_local" \
    -f "$here/pg18-build.Dockerfile" "$repo_root"
fi

if $push && ! $no_base_push; then
  base_remote="${registry}/${base_image}:${pg_tag}"
  docker tag "$base_local" "$base_remote"
  echo ">> pushing ${base_remote}"
  docker push "$base_remote"
elif $push && $no_base_push; then
  echo ">> skipping base push (--no-base-push); it is built locally only"
fi

$base_only && { echo "base-only: done"; exit 0; }

want() {
  [[ ${#only[@]} -eq 0 ]] && return 0
  local n; for n in "${only[@]}"; do [[ "$n" == "$1" ]] && return 0; done
  return 1
}

for row in "${ext_rows[@]}"; do
  IFS=$'\x1f' read -r name version url sha256 patch prep trusted dockerfile <<<"$row"
  want "$name" || continue

  image="${registry}/amp-ext-${name}:${version}"
  file="$here/extension.Dockerfile"
  [[ -n "$dockerfile" ]] && file="$repo_root/$dockerfile"

  echo ">> building ${image} (${file#"$repo_root"/})"
  docker build -t "$image" \
    --build-arg "PG_BASE=${base_local}" \
    --build-arg "NAME=${name}" \
    --build-arg "VERSION=${version}" \
    --build-arg "URL=${url}" \
    --build-arg "SHA256=${sha256}" \
    --build-arg "PATCH=${patch}" \
    --build-arg "PREP=${prep}" \
    --build-arg "TRUSTED=${trusted}" \
    -f "$file" "$repo_root"

  if $push; then
    echo ">> pushing ${image}"
    docker push "$image"
  fi
done

echo "done"
