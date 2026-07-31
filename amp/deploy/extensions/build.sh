#!/usr/bin/env bash
# Build (and optionally push) the yb-amp extension ImageVolume images.
#
# Reads deploy/extensions/extensions.yaml, builds the ABI-anchor PG18 base once,
# then each extension into a minimal `FROM scratch` image the endpoint mounts as
# a Kubernetes ImageVolume. Run from anywhere; paths resolve to the repo root.
#
# Usage:
#   deploy/extensions/build.sh [--push] [--no-base-push] [--registry REG] [--base-only] [NAME ...]
#
#   --push         also `docker push` every image built (base + extensions)
#   --no-base-push with --push, push only the extension images, NOT the
#                  ABI-anchor base (the base is still built locally to compile
#                  the extensions — nothing pulls it back from the registry, so
#                  publishing it is optional). Ignored without --push.
#   --registry REG override the manifest's registry
#   --base-only    build just the ABI-anchor base image, no extensions
#   NAME ...       build only these extensions (default: all in the manifest)
#
# Requires: docker, python3 with PyYAML (pip install pyyaml).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
manifest="$here/extensions.yaml"

push=false
no_base_push=false
base_only=false
registry_override=""
declare -a only=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --push) push=true; shift ;;
    --no-base-push) no_base_push=true; shift ;;
    --base-only) base_only=true; shift ;;
    --registry) registry_override="${2:?--registry needs a value}"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *) only+=("$1"); shift ;;
  esac
done

# The manifest is the source of truth; python emits shell-safe, tab-separated
# rows so bash never has to parse YAML itself.
read_manifest() {
  python3 - "$manifest" <<'PY'
import sys, yaml
with open(sys.argv[1]) as f:
    m = yaml.safe_load(f)
print("REGISTRY\t%s" % m.get("registry", ""))
print("BASE_IMAGE\t%s" % m.get("base_image", "amp-pg18-build"))
print("PG_TAG\t%s" % m.get("pg_tag", "v18"))
def norm(v):
    if v is None:
        return ""
    if isinstance(v, bool):
        return "true" if v else "false"
    return str(v)
for e in m.get("extensions", []):
    fields = [norm(e.get(k)) for k in
              ("name","version","url","sha256","patch","prep","trusted","dockerfile")]
    print("EXT\t" + "\t".join(f.replace("\t"," ") for f in fields))
PY
}

registry=""; base_image=""; pg_tag=""
declare -a ext_rows=()
while IFS=$'\t' read -r tag rest; do
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
docker build -t "$base_local" \
  -f "$here/pg18-build.Dockerfile" "$repo_root"

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
  IFS=$'\t' read -r name version url sha256 patch prep trusted dockerfile <<<"$row"
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
