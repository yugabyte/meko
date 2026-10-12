#!/usr/bin/env bash
# Deploy (or switch) the MCP-surface test stack.
#
#   ./deploy.sh released   today's api-server and meko-mcp-server, old memory table
#   ./deploy.sh branch     the fork branches' images, meko_mem0_memories + search routing
#
# Secrets are generated once and kept in the cluster; rerunning only swaps images
# and memory settings.
set -euo pipefail

MODE=${1:?usage: deploy.sh released|branch}
CTX=${CTX:-amiram.mizne@amp-bench.us-east-2.eksctl.io}
NS=${NS:-meko-onemil}
HERE=$(cd "$(dirname "$0")" && pwd)
K=(kubectl --context "$CTX" -n "$NS")

export REG=${REG:-161908593886.dkr.ecr.us-east-2.amazonaws.com}
export LITELLM_TAG=${LITELLM_TAG:-onemil-5f3b532-1c7595e-61473f4}
export LANGFUSE_TAG=${LANGFUSE_TAG:-3.157.0-20260928-1ae9be0}
export RAG_TAG=${RAG_TAG:-1.0.9-20260917-context-search} RAG_REPLICAS=${RAG_REPLICAS:-1}
# The chart's default is 2 GiB, below MCP's working set once it serves KB and
# context search (measured: each pod OOM-killed 6 times at 50-200 agents).
export MCP_MEMORY_LIMIT=${MCP_MEMORY_LIMIT:-6Gi}
# The Langfuse schema has to match the image: 1ae9be0 is the commit in its tag.
LANGFUSE_DIR=${LANGFUSE_DIR:-$HOME/sandbox/GitHub/meko-langfuse}
LANGFUSE_COMMIT=${LANGFUSE_COMMIT:-1ae9be0}

case $MODE in
released)
  export API_TAG=${API_TAG:-1.0.50-20260925} MCP_TAG=${MCP_TAG:-3.1.0-20260929-81a04b0}
  export API_EXTRA_ARGS=""
  memory=()
  ;;
branch)
  export API_TAG=${API_TAG:-onemil-5f3b532-1c7595e-61473f4} MCP_TAG=${MCP_TAG:-onemil-5f3b532-1c7595e-61473f4}
  # The measured thresholds: exact below a few thousand rows, ef_search 400 above.
  memory=(MEM0_COLLECTION_NAME=meko_mem0_memories)
  # Each account's PAT resolves to that account (MEKO-785); the released image has no such flag.
  export API_EXTRA_ARGS="- --dev-auth-accept-pats"
  mcp_only=(MEM0_EXACT_SEARCH_MAX_ROWS=2000 MEM0_HNSW_EF_SEARCH=400
    KB_EXACT_SEARCH_MAX_ROWS=2000 KB_HNSW_EF_SEARCH=400)
  ;;
*)
  echo "unknown mode $MODE" >&2
  exit 1
  ;;
esac

env_lines() {
  local out="" kv
  [ $# -eq 0 ] && return 0
  for kv in "$@"; do out+="- { name: ${kv%%=*}, value: \"${kv#*=}\" }"$'\n            '; done
  printf '%s' "$out"
}
# ${a[@]+...}: bash 3.2 treats an empty array as unset under set -u.
MEMORY_ENV_API=$(env_lines ${memory[@]+"${memory[@]}"})
MEMORY_ENV_MCP=$(env_lines ${memory[@]+"${memory[@]}"} ${mcp_only[@]+"${mcp_only[@]}"})
export MEMORY_ENV_API MEMORY_ENV_MCP

rand() { openssl rand -hex "${1:-24}"; }
if ! "${K[@]}" get secret stack-secrets >/dev/null 2>&1; then
  s3k=$(rand 12) s3s=$(rand) redis=$(rand) admin=$(rand)
  "${K[@]}" create secret generic stack-secrets \
    --from-literal=s3-access-key="$s3k" --from-literal=s3-secret-key="$s3s" \
    --from-literal=redis-auth="$redis" --from-literal=langfuse-admin-api-key="$admin" \
    --from-literal=mcp-internal-secret="$(rand)" \
    --from-literal=langfuse-public-key="pk-lf-onemil-$(rand 8)" --from-literal=langfuse-secret-key="sk-lf-onemil-$(rand 16)"
fi
get() { "${K[@]}" get secret stack-secrets -o jsonpath="{.data.$1}" | base64 -d; }
"${K[@]}" create secret generic langfuse-secrets \
  --from-literal=NEXTAUTH_SECRET="$(get mcp-internal-secret)" \
  --from-literal=SALT="$(get redis-auth)" \
  --from-literal=ENCRYPTION_KEY="$(openssl dgst -sha256 <<<"$(get langfuse-admin-api-key)" | awk '{print $NF}')" \
  --from-literal=REDIS_AUTH="$(get redis-auth)" \
  --from-literal=ADMIN_API_KEY="$(get langfuse-admin-api-key)" \
  --from-literal=LANGFUSE_INIT_PROJECT_PUBLIC_KEY="$(get langfuse-public-key)" \
  --from-literal=LANGFUSE_INIT_PROJECT_SECRET_KEY="$(get langfuse-secret-key)" \
  --from-literal=LANGFUSE_INIT_USER_PASSWORD="$(get s3-secret-key)" \
  --from-literal=LANGFUSE_S3_EVENT_UPLOAD_ACCESS_KEY_ID="$(get s3-access-key)" \
  --from-literal=LANGFUSE_S3_EVENT_UPLOAD_SECRET_ACCESS_KEY="$(get s3-secret-key)" \
  --from-literal=LANGFUSE_S3_MEDIA_UPLOAD_ACCESS_KEY_ID="$(get s3-access-key)" \
  --from-literal=LANGFUSE_S3_MEDIA_UPLOAD_SECRET_ACCESS_KEY="$(get s3-secret-key)" \
  --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null

schema=$(mktemp -d)
trap 'rm -rf "$schema"' EXIT
for f in yb_postgres_data.sql tracing_postgres_schema.sql; do
  git -C "$LANGFUSE_DIR" show "$LANGFUSE_COMMIT:$f" >"$schema/$f"
done
# Server-side: client-side apply would copy the 438 KB schema into an annotation (256 KB max).
"${K[@]}" create configmap langfuse-schema --from-file="$schema" --dry-run=client -o yaml | "${K[@]}" apply --server-side -f - >/dev/null

envsubst '${REG} ${API_TAG} ${MCP_TAG} ${LITELLM_TAG} ${LANGFUSE_TAG} ${RAG_TAG} ${RAG_REPLICAS} ${MCP_MEMORY_LIMIT} ${API_EXTRA_ARGS} ${MEMORY_ENV_API} ${MEMORY_ENV_MCP}' \
  <"$HERE/stack.yaml" | "${K[@]}" apply -f -
"${K[@]}" wait --for=condition=complete job/stack-init --timeout=15m
for d in litellm redis minio langfuse meko-api-server meko-mcp-server rag-agent-text; do
  "${K[@]}" rollout status "deploy/$d" --timeout=10m
done
echo "stack up: mode=$MODE api=$API_TAG mcp=$MCP_TAG"
