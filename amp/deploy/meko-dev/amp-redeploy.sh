#!/usr/bin/env bash
# amp clean redeploy: point the amp-dev control plane at a given image TAG and a
# (passwordless) external YB universe, on a fresh yb_storage schema — preserving
# the tailnet URLs (amp-api/amp-ui/amp-mcp) and the OTel collector CRs.
#
# Destructive steps require CONFIRM=yes.
#   CONFIRM=yes ./amp-redeploy.sh     # full redeploy
#   ./amp-redeploy.sh                 # dry preview (skips destructive steps)
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NS="${NS:-amp-dev}"; KCTX="${KCTX:-meko-dev-eks}"
ECR="${ECR:-454529406029.dkr.ecr.us-west-2.amazonaws.com}"
TAG="${TAG:-}"; AWS_REGION="${AWS_REGION:-us-west-2}"
# chart ships in this repo, one level up from deploy/meko-dev/
CHART="${CHART:-$HERE/../helm/yb-amp-controller}"
REL="${REL:-yb-amp-controller}"
# external YB universe (auth disabled: passwordless yugabyte)
YB_HOSTPORTS="${YB_HOSTPORTS:-ybamp2-db-dfac-yb-tservers.yb-dev-amp2-db.svc.cluster.local:5433}"
YB_NS="${YB_NS:-yb-dev-amp2-db}"; YB_DB="${YB_DB:-yb_storage}"
CONFIRM="${CONFIRM:-no}"
K="kubectl --context $KCTX -n $NS"
log(){ printf '\033[1m[redeploy]\033[0m %s\n' "$*"; }
guard(){ [ "$CONFIRM" = yes ] || { log "SKIP (set CONFIRM=yes): $*"; return 1; }; }

[ -n "$TAG" ] || { log "ERROR: set TAG=<image tag> (applied to controller/endpoint/mcp/ui)"; exit 2; }

# 1. Re-mint ECR pull secret (token ~12h)
log "1. re-mint pull secret"
if guard "create pull-secret"; then
  $K create secret docker-registry pull-secret --docker-server="$ECR" --docker-username=AWS \
    --docker-password="$(aws ecr get-login-password --region "$AWS_REGION")" --dry-run=client -o yaml | $K apply -f -
fi

# 2. Ensure yb_storage DB exists on the universe (the controller does NOT auto-create it)
log "2. create database $YB_DB on universe (idempotent)"
if guard "create database"; then
  POD=$(kubectl --context "$KCTX" -n "$YB_NS" get pods -l app.kubernetes.io/name=yb-tserver -o jsonpath='{.items[0].metadata.name}')
  kubectl --context "$KCTX" -n "$YB_NS" exec "$POD" -c yb-tserver -- bash -lc \
    "Y=\$(ls /home/yugabyte/tserver/postgres/bin/ysqlsh 2>/dev/null||echo ysqlsh); \$Y -h ${YB_HOSTPORTS%%:*} -p ${YB_HOSTPORTS##*:} -U yugabyte -d yugabyte -c 'CREATE DATABASE $YB_DB' 2>&1 | grep -v 'already exists' || true"
fi

# 3. Uninstall old per-endpoint releases (they belong to the old backend)
log "3. uninstall old yb-ep-* releases"
if guard "uninstall endpoints"; then
  helm --kube-context "$KCTX" -n "$NS" list -q | grep '^yb-ep-' | xargs -r -n1 helm --kube-context "$KCTX" -n "$NS" uninstall
fi

# 4. helm upgrade the control plane -> TAG + universe, preserving all other values
log "4. helm upgrade control plane -> $TAG @ $YB_HOSTPORTS"
if guard "helm upgrade"; then
  helm --kube-context "$KCTX" -n "$NS" get values "$REL" -o yaml > /tmp/amp-vals.yaml
  python3 - "$TAG" "$YB_HOSTPORTS" "$YB_DB" <<'PY'
import re,sys
tag,hp,db=sys.argv[1],sys.argv[2],sys.argv[3]
f="/tmp/amp-vals.yaml"; s=open(f).read()
s=re.sub(r'databaseUrl: .*', f'databaseUrl: postgres://yugabyte@{hp}/{db}?sslmode=disable', s)
s=re.sub(r'ybHostPorts: .*', f'ybHostPorts: {hp}', s)
s=re.sub(r'tag: \S+', f'tag: {tag}', s)   # controller/endpoint/mcp/ui image tags
open(f,'w').write(s); print("values overridden")
PY
  # --force-conflicts: controller/ui images may have been `kubectl set image`'d
  # (field-manager "kubectl-set") -> server-side-apply conflict without it.
  helm --kube-context "$KCTX" -n "$NS" upgrade "$REL" "$CHART" -f /tmp/amp-vals.yaml --force-conflicts
fi

# 5. wait for control plane ready
log "5. wait for control plane ready"
if guard "rollout wait"; then
  for d in amp-controller amp-controller-mcp amp-controller-ui; do
    $K rollout status "deploy/$d" --timeout=300s
  done
fi

# 6. delete the old in-namespace yugabyted backend (irreversible)
log "6. delete old yugabyted backend (irreversible)"
if guard "delete old yugabyted"; then
  $K delete statefulset yugabyted --ignore-not-found
  $K delete svc yugabyted --ignore-not-found
  $K delete pvc data-yugabyted-0 --ignore-not-found
fi

# 7. verify tailnet URLs + OTel CRs preserved
log "7. verify preserved: tailnet hostnames + OTel CRs"
for s in amp-controller amp-controller-mcp amp-controller-ui; do
  echo -n "   $s -> "; $K get svc "$s" -o jsonpath='{.metadata.annotations.tailscale\.com/hostname}{"\n"}' 2>/dev/null
done
$K get opentelemetrycollectors 2>/dev/null

log "done. Run amp-integration-test.sh to validate provisioning end-to-end."
