#!/usr/bin/env bash
# Brings up the small_temporal LRU: the amp side (pool, cluster, endpoint) and
# the workload manifests, on top of the shared ../monitoring base. Idempotent:
# every step looks up by name before creating.
#
#   KUBE_CONTEXT=meko_dev_eks AMP_NS=amp-dev ./up.sh
#
# Talks to the controller over a local port-forward with admin/admin.
set -euo pipefail

KUBE_CONTEXT=${KUBE_CONTEXT:-meko_dev_eks}
AMP_NS=${AMP_NS:-amp-dev}
LRU=${LRU:-small_temporal}
LRU_NS=${LRU_NS:-amp-lru}
POOL=${POOL:-lru-temporal}
CLUSTER=${CLUSTER:-temporal}
PG_VERSION=${PG_VERSION:-18}
EP_CPU=${EP_CPU:-2}
EP_MEM=${EP_MEM:-16Gi}
AUTH=${AMP_AUTH:-admin:admin}
# Not overridable: every workload, checker and amp-time manifest hardcodes this as
# cloud_admin's password, so a different value here would lock them all out.
EP_PASSWORD=cloud_admin
PORT=${PORT:-18080}
HERE=$(cd "$(dirname "$0")" && pwd)

k() { kubectl --context "$KUBE_CONTEXT" "$@"; }
api() { curl -sS -m 30 -u "$AUTH" -H 'content-type: application/json' "http://127.0.0.1:$PORT/api/v1$@"; }
jqf() { python3 -c "import sys,json; d=json.load(sys.stdin); $1"; }

k -n "$AMP_NS" port-forward svc/amp-controller "$PORT:8080" >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true' EXIT
for _ in $(seq 1 15); do curl -s -m 2 -o /dev/null "http://127.0.0.1:$PORT/healthz" && break; sleep 2; done

wait_task() {
  local id=$1 st
  for _ in $(seq 1 240); do
    st=$(api "/tasks/$id" | jqf 'print(d["status"])')
    case $st in
      succeeded|completed|done) return 0 ;;
      failed|cancelled|error) api "/tasks/$id"; echo "task $id $st" >&2; return 1 ;;
    esac
    sleep 5
  done
  echo "task $id still $st after 20m" >&2; return 1
}

POOL_ID=$(api "/storage-pools?limit=500" | jqf 'print(next((p["id"] for p in d["storage_pools"] if p["name"]=="'"$POOL"'"), ""))')
if [ -z "$POOL_ID" ]; then
  OWNER=$(api "/users" -X POST -d '{}' | jqf 'print(d["id"])')
  POOL_ID=$(api "/storage-pools" -X POST -d "{\"name\":\"$POOL\",\"owner_user_id\":\"$OWNER\"}" | jqf 'print(d["id"])')
  echo "created pool $POOL $POOL_ID (owner $OWNER)"
else
  echo "pool $POOL $POOL_ID"
fi

CLUSTER_JSON=$(api "/storage-pools/$POOL_ID/clusters?limit=500" | jqf 'import json as j; print(j.dumps(next((c for c in d.get("clusters",[]) if c["name"]=="'"$CLUSTER"'"), {})))')
if [ "$CLUSTER_JSON" = "{}" ]; then
  RESP=$(api "/storage-pools/$POOL_ID/clusters" -X POST -d "{\"name\":\"$CLUSTER\",\"pg_version\":$PG_VERSION}")
  TASK=$(echo "$RESP" | jqf 'print(d.get("id") or d.get("task",{}).get("id"))')
  wait_task "$TASK"
  CLUSTER_JSON=$(api "/storage-pools/$POOL_ID/clusters?limit=500" | jqf 'import json as j; print(j.dumps(next(c for c in d["clusters"] if c["name"]=="'"$CLUSTER"'")))')
  echo "created cluster $CLUSTER"
fi
CLUSTER_ID=$(echo "$CLUSTER_JSON" | jqf 'print(d["id"])')
BRANCH_ID=$(echo "$CLUSTER_JSON" | jqf 'print(d["default_branch"]["branch_id"])')
echo "cluster $CLUSTER $CLUSTER_ID branch main $BRANCH_ID"

EP_BASE="/storage-pools/$POOL_ID/clusters/$CLUSTER_ID/branches/$BRANCH_ID/endpoints"
EP_ID=$(api "$EP_BASE" | jqf 'print(next((e["id"] for e in d.get("endpoints",[]) if e["role"]=="read_write"), ""))')
if [ -z "$EP_ID" ]; then
  RESP=$(api "$EP_BASE" -X POST -d "{\"role\":\"read_write\",\"password\":\"$EP_PASSWORD\",\"resources\":{\"cpu\":\"$EP_CPU\",\"memory\":\"$EP_MEM\"}}")
  TASK=$(echo "$RESP" | jqf 'print(d.get("id") or d.get("task",{}).get("id"))')
  wait_task "$TASK"
  EP_ID=$(api "$EP_BASE" | jqf 'print(next(e["id"] for e in d["endpoints"] if e["role"]=="read_write"))')
  echo "created endpoint $EP_ID ($EP_CPU vCPU / $EP_MEM)"
else
  echo "endpoint $EP_ID"
fi
EP_SVC="yb-ep-$EP_ID.$AMP_NS.svc.cluster.local"
k -n "$AMP_NS" rollout status "deploy/yb-ep-$EP_ID" --timeout=600s

cat > "$HERE/lru.env" <<ENV
POOL_ID=$POOL_ID
CLUSTER_ID=$CLUSTER_ID
BRANCH_ID=$BRANCH_ID
ENDPOINT_ID=$EP_ID
ENDPOINT_SERVICE=$EP_SVC
ENDPOINT_PORT=55433
ENV
echo "wrote $HERE/lru.env"

# monitoring/up.sh, not a bare apply -k: it also creates the grafana-admin Secret
# and the cross-namespace RBAC Prometheus needs to scrape the endpoint pods.
KUBE_CONTEXT="$KUBE_CONTEXT" LRU_NS="$LRU_NS" "$HERE/../monitoring/up.sh"
k apply -k "$HERE"
# Stable in-namespace alias for the endpoint so the manifests carry no endpoint id.
# Deliberately not a kustomize resource: a bare `kubectl apply -k .` would put a
# placeholder back and nothing downstream would resolve.
k apply -f - <<YAML
apiVersion: v1
kind: Service
metadata:
  name: small-temporal-endpoint
  namespace: $LRU_NS
  labels: { lru: $LRU }
spec:
  type: ExternalName
  externalName: $EP_SVC
  ports: [{ name: postgres, port: 55433, targetPort: 55433 }]
YAML
k -n "$LRU_NS" rollout status deploy/small-temporal-server --timeout=600s
k -n "$LRU_NS" get pods -l lru="$LRU"
