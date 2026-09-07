#!/usr/bin/env bash
# Re-vendor the amp controller's OpenAPI spec and regenerate the client.
#
# amp/controller and meko/ycp are separate Go modules that must not cross-import,
# so the shared artifact is the spec file rather than the generated Go. That copy
# only stays current if someone remembers to make it, which is what this script
# and the CI check that runs it are for.
#
# With --check, copy and regenerate into a scratch tree and fail if the result
# differs from what is committed, leaving the working tree untouched.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../../.." && pwd)
src=$root/amp/controller/internal/api/openapi_spec.yaml

check=false
[[ ${1:-} == --check ]] && check=true

if [[ ! -f $src ]]; then
  echo "error: $src not found" >&2
  exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

out=$here
$check && out=$tmp

cp "$src" "$out/openapi_spec.yaml"

# The config's `output` beats the -o flag and is relative to the working
# directory, so rewrite it to an absolute path rather than relying on either.
# Always via $tmp — writing it back into $here would truncate the real config.
cfg=$tmp/oapi-codegen.yaml
sed "s#^output:.*#output: $out/ampclient.gen.go#" "$here/oapi-codegen.yaml" > "$cfg"

# From the module, so the oapi-codegen pinned in ycp/go.mod is the one that runs.
(cd "$root/ycp" && go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen \
  -config "$cfg" "$out/openapi_spec.yaml")

if ! $check; then
  exit 0
fi

stale=false
for f in openapi_spec.yaml ampclient.gen.go; do
  if ! diff -u "$here/$f" "$out/$f"; then
    stale=true
  fi
done
if $stale; then
  echo "error: the vendored amp spec or its generated client is stale." >&2
  echo "       Run ycp/internal/ampclient/sync-spec.sh and commit the result." >&2
  exit 1
fi
echo "vendored amp spec and generated client are in sync"
