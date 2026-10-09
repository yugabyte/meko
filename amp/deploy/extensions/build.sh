#!/usr/bin/env bash
# Build (and optionally push) the yb-amp extension ImageVolume images.
#
# Reads deploy/extensions/extensions.yaml, builds the ABI-anchor PG18 base once,
# then each extension into a minimal `FROM scratch` image the endpoint mounts as
# a Kubernetes ImageVolume. Run from anywhere; paths resolve to the repo root.
#
# Usage:
#   deploy/extensions/build.sh [--push] [--no-base-push] [--cache] [--force] [--registry REG] [--base-only] [NAME ...]
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
#   --force        rebuild extension images even if the target tag already
#                  exists. By default an extension whose tag is already present
#                  is skipped — extension tags are immutable, so re-running the
#                  build is wasted work. "Present" is checked in the registry
#                  with --push, else in the local image store. The ABI-anchor
#                  base is always (re)built regardless.
#   --registry REG override the manifest's registry
#   --base-only    build just the ABI-anchor base image, no extensions
#   NAME ...       build only these extensions (default: all in the manifest)
#
# The tag is <version>, or <version>-r<N> for a row with `image_revision: N`
# (N >= 2). Every image is labelled org.yugabyte.amp.ext-recipe with a hash of
# the extension's own recipe: the row's build fields, its Dockerfile and its
# patch. The ABI-anchor base it builds FROM is not part of it, so a base change
# (the vendored PG, pg18-build.Dockerfile) does not invalidate a tag; one that can
# affect the extensions has to bump their image_revision by hand.
#   * With --push (the registry, whose tags are immutable): an existing tag whose
#     label differs is a build ERROR, not a skip — a recipe change without an
#     `image_revision` bump, which would otherwise leave the registry serving the
#     old recipe under a tag that looks current. This holds for every tag built
#     with the label. The tags published before it (the revision-1 pgvector,
#     pg_cron, hexabridge and age) carry none, so their recipe cannot be checked:
#     they are skipped with a warning, and bumping one's revision brings it under
#     the guard.
#   * Locally (no --push): a local tag is just a cache, so an image whose label
#     differs, or is missing, is rebuilt rather than refused.
#
# A private-source extension (e.g. hexabridge) is fetched with an auth header when
# AMP_EXT_GH_TOKEN is set in the environment: it is passed to the build as a
# BuildKit secret (id=gh_token), never a build-arg, so it stays out of image
# layers. Public extensions build fine without it.
#
# Requires: docker, python3 with PyYAML (pip install pyyaml). --cache also needs
# a buildx docker-container builder + a registry login.
set -euo pipefail

# The generic extension.Dockerfile uses BuildKit features (a `# syntax` directive
# and --mount=type=secret for the authenticated private-source fetch), so force
# the BuildKit backend on for the plain `docker build` calls too (buildx is
# already BuildKit).
export DOCKER_BUILDKIT=1

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
manifest="$here/extensions.yaml"

push=false
no_base_push=false
base_only=false
cache=false
force=false
registry_override=""
declare -a only=()
while [[ $# -gt 0 ]]; do
  case "$1" in
  --push)
    push=true
    shift
    ;;
  --no-base-push)
    no_base_push=true
    shift
    ;;
  --base-only)
    base_only=true
    shift
    ;;
  --cache)
    cache=true
    shift
    ;;
  --force)
    force=true
    shift
    ;;
  --registry)
    registry_override="${2:?--registry needs a value}"
    shift 2
    ;;
  -h | --help)
    sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d'
    exit 0
    ;;
  -*)
    echo "unknown flag: $1" >&2
    exit 2
    ;;
  *)
    only+=("$1")
    shift
    ;;
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
import hashlib, json, os
root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(sys.argv[1]))))
BUILD_FIELDS = ("name","version","image_revision","url","patch","prep","trusted","dockerfile","make_targets")
def recipe(e):
    # The extension's own recipe: the row's build fields plus the bytes of its
    # Dockerfile and patch. A comment edit in the manifest, or a runtime-only
    # field (preload, library, settings, regress), changes nothing. The ABI-anchor
    # base it builds FROM is deliberately NOT hashed (pg18-build.Dockerfile, the
    # vendored postgres-v18, Makefile, postgres.mk): every vendored-PG bump would
    # otherwise invalidate every labelled tag. A base change that can affect the
    # extensions built on it has to bump their image_revision by hand.
    h = hashlib.sha256()
    h.update(json.dumps({k: norm(e.get(k)) for k in BUILD_FIELDS}, sort_keys=True).encode())
    dockerfile = e.get("dockerfile") or "deploy/extensions/extension.Dockerfile"
    for path in [dockerfile] + (["compute/image/patches/" + e["patch"]] if e.get("patch") else []):
        with open(os.path.join(root, path), "rb") as f:
            h.update(path.encode() + b"\0" + f.read())
    return h.hexdigest()
def tag(e):
    rev = int(e.get("image_revision") or 1)
    return str(e["version"]) + ("-r%d" % rev if rev > 1 else "")
for e in m.get("extensions", []):
    fields = [norm(e.get(k)) for k in
              ("name","version","url","patch","prep","trusted","dockerfile","make_targets")]
    fields += [tag(e), recipe(e)]
    print("EXT" + US + US.join(f.replace(US, " ") for f in fields))
PY
}

registry=""
base_image=""
pg_tag=""
declare -a ext_rows=()
while IFS=$'\x1f' read -r tag rest; do
  case "$tag" in
  REGISTRY) registry="$rest" ;;
  BASE_IMAGE) base_image="$rest" ;;
  PG_TAG) pg_tag="$rest" ;;
  EXT) ext_rows+=("$rest") ;;
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

$base_only && {
  echo "base-only: done"
  exit 0
}

want() {
  [[ ${#only[@]} -eq 0 ]] && return 0
  local n
  for n in "${only[@]}"; do [[ "$n" == "$1" ]] && return 0; done
  return 1
}

# Present the GitHub token to every extension build as a BuildKit secret when it
# is set. It is only consumed by the fetch of a private-source extension (the
# Dockerfile mounts it required=false); public extensions ignore it. Passed as a
# secret, never a --build-arg, so the token is never persisted into an image layer.
declare -a secret_args=()
if [[ -n "${AMP_EXT_GH_TOKEN:-}" ]]; then
  secret_args=(--secret "id=gh_token,env=AMP_EXT_GH_TOKEN")
fi

# exists reports whether the image tag is already published (--push) or already
# in the local store. Extension tags are immutable, so an existing tag means the
# build is redundant — provided it was built from the same recipe (recipe_of).
exists() {
  local img="$1"
  if $push; then
    docker manifest inspect "$img" >/dev/null 2>&1
  else
    docker image inspect "$img" >/dev/null 2>&1
  fi
}

# recipe_of prints the existing image's recipe label, or nothing when it has
# none. Reading the labels failing is NOT "no label": that would switch the guard
# off with a skip message, so it fails instead.
RECIPE_LABEL=org.yugabyte.amp.ext-recipe
recipe_of() {
  local img="$1" labels
  if $push; then
    labels=$(docker buildx imagetools inspect "$img" --format '{{ json .Image.Config.Labels }}') || {
      echo "error: cannot read the labels of ${img}" >&2
      return 1
    }
  else
    labels=$(docker image inspect "$img" --format '{{ json .Config.Labels }}') || {
      echo "error: cannot read the labels of ${img}" >&2
      return 1
    }
  fi
  python3 -c 'import json, sys; print((json.loads(sys.argv[1] or "null") or {}).get(sys.argv[2], ""))' \
    "${labels:-null}" "$RECIPE_LABEL"
}

for row in "${ext_rows[@]}"; do
  IFS=$'\x1f' read -r name version url patch prep trusted dockerfile make_targets tag recipe <<<"$row"
  want "$name" || continue

  image="${registry}/amp-ext-${name}:${tag}"
  file="$here/extension.Dockerfile"
  [[ -n "$dockerfile" ]] && file="$repo_root/$dockerfile"

  if ! $force && exists "$image"; then
    have=$(recipe_of "$image") || exit 1
    if [[ "$have" == "$recipe" ]]; then
      echo ">> ${image} already exists, same recipe — skipping (use --force to rebuild)"
      continue
    fi
    if ! $push; then
      # A local tag is a cache, not a release: rebuild it from the recipe.
      echo ">> ${image} exists locally from ${have:-an unlabelled build}; the recipe changed — rebuilding"
    elif [[ -z "$have" ]]; then
      echo ">> ${image} already exists (built before recipe labels; not verifiable) — skipping"
      continue
    else
      echo "error: ${image} exists but was built from a different recipe" >&2
      echo "  registry: ${have}" >&2
      echo "  manifest: ${recipe}" >&2
      echo "  Tags are immutable: bump ${name}'s image_revision in extensions.yaml and the" >&2
      echo "  controller catalog (catalog.go ImageRevision), so the new recipe gets a new tag." >&2
      exit 1
    fi
  fi

  echo ">> building ${image} (${file#"$repo_root"/})"
  docker build "${secret_args[@]+"${secret_args[@]}"}" -t "$image" \
    --build-arg "PG_BASE=${base_local}" \
    --build-arg "NAME=${name}" \
    --build-arg "VERSION=${version}" \
    --build-arg "URL=${url}" \
    --build-arg "PATCH=${patch}" \
    --build-arg "PREP=${prep}" \
    --build-arg "TRUSTED=${trusted}" \
    --build-arg "MAKE_TARGETS=${make_targets}" \
    --label "${RECIPE_LABEL}=${recipe}" \
    -f "$file" "$repo_root"

  if $push; then
    echo ">> pushing ${image}"
    docker push "$image"
  fi
done

echo "done"
