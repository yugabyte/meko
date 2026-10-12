#!/usr/bin/env bash
# Build the scale test's images on the cluster with Kaniko and push them to the
# dev ECR. Each build context is streamed to the pod as a tarball, so nothing is
# built or emulated locally. Pushes use the image-builder service account
# (infra/builder-irsa.yaml).
#
#   - LiteLLM, patched for mem0 on Bedrock (meko-test/litellm)
#   - api-server from this checkout
#   - meko-mcp-server: the released image named by MCP_BASE plus this branch's
#     changed modules (MCP_FILES, relative to MCP_DIR) and a meko-mem0 wheel
#
#   MCP_DIR=... MEM0_DIR=... LITELLM_DIR=... MCP_BASE=<tag> MCP_FILES="src/a.py ..." \
#     ./build-images.sh <tag>
set -euo pipefail

TAG=${1:?usage: build-images.sh <tag>}
CTX_NAME=${CTX:-amiram.mizne@amp-bench.us-east-2.eksctl.io}
NS=${NS:-meko-onemil}
REG=${REG:-161908593886.dkr.ecr.us-east-2.amazonaws.com}
HERE=$(cd "$(dirname "$0")" && pwd)
MEKO_ROOT=$(cd "$HERE/../../.." && pwd)
K=(kubectl --context "$CTX_NAME" -n "$NS")

"${K[@]}" create configmap kaniko-docker-config --from-literal=config.json='{"credsStore":"ecr-login"}' \
  --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null

# kaniko <name> <context dir> <destination> [dockerfile]
kaniko() {
  local name=$1 dir=$2 dest=$3 dockerfile=${4:-Dockerfile}
  local overrides
  overrides=$(
    cat <<JSON
{"spec":{"serviceAccountName":"image-builder","nodeSelector":{"workload":"meko-onemil-app"},
 "tolerations":[{"key":"dedicated","operator":"Equal","value":"meko-onemil","effect":"NoSchedule"}],
 "containers":[{"name":"kaniko","image":"gcr.io/kaniko-project/executor:v1.23.2","stdin":true,"stdinOnce":true,
  "args":["--context=tar://stdin","--dockerfile=$dockerfile","--destination=$dest"],
  "env":[{"name":"AWS_REGION","value":"us-east-2"}],
  "resources":{"requests":{"cpu":"2","memory":"4Gi"}},
  "volumeMounts":[{"name":"dc","mountPath":"/kaniko/.docker"}]}],
 "volumes":[{"name":"dc","configMap":{"name":"kaniko-docker-config"}}]}}
JSON
  )
  tar -C "$dir" -czf - . | "${K[@]}" run "kaniko-$name" --rm -i --restart=Never \
    --image=gcr.io/kaniko-project/executor:v1.23.2 --overrides="$overrides" | grep -E "Pushed|error|ERROR"
}

if [ -n "${LITELLM_DIR:-}" ]; then
  kaniko litellm "$LITELLM_DIR" "$REG/meko-onemil/litellm:$TAG"
fi

# API_DIR builds another checkout's api-server, e.g. a branch combining open PRs.
kaniko api "${API_DIR:-$MEKO_ROOT/api_server}" "$REG/meko/meko-api-server:$TAG"

# MCP_DIR unset: the api-server (and LiteLLM) only.
[ -z "${MCP_DIR:-}" ] && exit 0
MCP_DIR=${MCP_DIR:?path to a meko-mcp-server checkout}
MEM0_DIR=${MEM0_DIR:?path to a meko-mem0 checkout}
MCP_BASE=${MCP_BASE:?released meko-mcp-server tag the branch is based on}
MCP_FILES=${MCP_FILES:?the branch files that differ from MCP_BASE}
ctx=$(mktemp -d)
trap 'rm -rf "$ctx"' EXIT
(cd "$MEM0_DIR" && uv build -q --wheel --out-dir "$ctx")
for f in $MCP_FILES; do mkdir -p "$ctx/$(dirname "$f")" && cp "$MCP_DIR/$f" "$ctx/$f"; done
cat >"$ctx/Dockerfile" <<DOCKERFILE
FROM $REG/meko/meko-mcp-server:$MCP_BASE
USER root
COPY src/ /app/src/
COPY *.whl /tmp/
RUN uv pip install --python /app/.venv/bin/python --no-deps /tmp/*.whl && rm /tmp/*.whl \\
 && chown -R app:app /app/src
USER app
# uv run --frozen would re-sync the venv to uv.lock and put the locked meko-mem0 back.
ENTRYPOINT ["/app/.venv/bin/python", "src/server.py"]
DOCKERFILE
kaniko mcp "$ctx" "$REG/meko/meko-mcp-server:$TAG"
