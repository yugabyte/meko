#!/usr/bin/env bash
#
# The operator path on a REAL cluster: two namespaces, two Helm releases, no kind.
#
#   <ycp namespace>    the control plane — api_server --control-plane-mode plus its
#                      bundled Postgres, from the `meko` chart in ycp mode.
#   <meko namespace>   the data plane — the agent, and everything the agent then
#                      brings up from the desired state registration seeded.
#
# The only manual step is registering the environment. After that the agent enrolls
# and reconciles; nothing here applies a workload or POSTs desired state.
#
# Differences from ycp-helm-bootstrap.sh (same flow, kind):
#   - images come from ECR, so they must be pushed first and built for the cluster's
#     architecture (GKE/EKS nodes are amd64; this laptop is not),
#   - both namespaces need an ECR pull Secret, because GKE has no AWS IAM,
#   - the storage class comes from the cloud preset rather than kind's local-path.
#
# It refuses to touch a namespace it did not create, so it cannot walk over
# somebody else's release in a shared cluster.
#
# Usage:
#   ./ycp/test/local/ycp-on-cluster.sh --context gke_yugabyte_us-west1_itest-dev
#   ./ycp/test/local/ycp-on-cluster.sh --context <ctx> --ycp-ns ycp --meko-ns meko \
#       --tag bootstrap-7c8776b41
#   ./ycp/test/local/ycp-on-cluster.sh --context <ctx> --delete    # tear both down
#
#     ./ycp/test/local/ycp-on-cluster.sh --context <ctx> --all \
#         --agent-tag custom-abc123
#
#   --storage-class is needed on any cluster without the cloud preset's default: the gcp
#   preset asks for pd-ssd, and a GKE cluster that only has standard-rwo / premium-rwo
#   fails the install with the class it wanted and the ones it found.
#     ./ycp/test/local/ycp-on-cluster.sh --context <ctx> --all --storage-class standard-rwo
#
# Environment:
#   AWS_PROFILE      profile with Meko ECR access (meko-dev here) — required.
#   MEKO_CHART       the chart directory (default: this checkout's meko/deploy/helm/meko)
#   MEKO_MCP_IMAGE   meko-mcp-server ref to seed; default = newest tag in ECR
#   OPENAI_API_KEY   written into the data-plane Secret when set
#
set -euo pipefail

CTX=""
YCP_NS="${YCP_NS:-ycp}"
MEKO_NS="${MEKO_NS:-meko}"
TAG="${TAG:-}"
AGENT_TAG="${AGENT_TAG:-latest}"
# The control-plane image, when it does not live in the ECR the cloud preset points
# at. meko/api_server publishes to Quay on demand (meko-control-plane-image.yml), and
# nothing publishes it to ECR at all, so a control plane built from a branch is
# normally a Quay ref — which needs the registry and repository overridden, not just
# the tag. Empty leaves the preset's registry alone and --tag supplies the tag.
YCP_IMAGE="${YCP_IMAGE:-}"
# The pinned data-plane release (meko/api_server/internal/service/dataplane_release.go).
# One tag per component: they are versioned separately, so a single shared value
# renders references that cannot be pulled.
MEKO_API_TAG="${MEKO_API_TAG:-console-943437724}"
MEKO_MCP_TAG="${MEKO_MCP_TAG:-2.1.0-20260731-7b3fdd3}"
MEKO_UI_TAG="${MEKO_UI_TAG:-1.0.0-20260729-bf56db7}"
CLOUD="${CLOUD:-gcp}"          # which cloud preset: gcp | aws
# Storage class for the control plane's bundled Postgres. Empty keeps the cloud
# preset's (pd-ssd on GKE, gp2-csi on AWS); override for a cluster that has neither,
# which the chart otherwise fails on by design rather than guessing.
STORAGE_CLASS="${STORAGE_CLASS:-}"
ENVPRESET="${ENVPRESET:-dev}"  # which env preset: dev | prod
DELETE=0
# No default. Installing a data plane is an operator's decision — registering an
# environment and attaching an agent to it are two deliberate acts, and a tool that
# does both because it was run is a tool that installs an agent nobody asked for.
STAGE=""                       # ycp | agent | all, chosen explicitly
ENV_NAME="${ENV_NAME:-}"
# The chart moved into this repo (a179f8c2a1); MEKO_CHART overrides for a checkout
# that still has it elsewhere.
HERE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MEKO_CHART="${MEKO_CHART:-$(cd "$HERE_DIR/../../.." && pwd)/meko/deploy/helm/meko}"
ECR=161908593886.dkr.ecr.us-east-2.amazonaws.com
ECR_REGION=us-east-2
PULL_SECRET=ecr-registry-creds   # the name values-gcp.yaml already uses
CREDS_SECRET=meko-data-plane
ADMIN_KEY="${ADMIN_KEY:-}"
PORT="${PORT:-18083}"
TIMEOUT="${TIMEOUT:-600s}"
# Marks the namespaces this script creates. Nothing is applied to a namespace that
# exists without it — a shared cluster is exactly where "assume it's ours" goes wrong.
OWNER_LABEL=meko.dev/created-by=ycp-on-cluster
# Helm release names are derived from the namespaces, not fixed. Helm releases are
# namespace-scoped, but some of what the chart renders is not: the api-server's
# CRD-creator ClusterRole is named "<release>-api-server-crd-creator", so two
# releases both called `ycp` in different namespaces of one cluster fight over it.
# A cluster that already runs a YCP — meko-dev-eks does — is exactly where a second
# one for testing gets installed.

while [ $# -gt 0 ]; do
  case "$1" in
    --context)  CTX="$2"; shift 2 ;;
    --ycp-ns)   YCP_NS="$2"; shift 2 ;;
    --meko-ns)  MEKO_NS="$2"; shift 2 ;;
    --tag)      TAG="$2"; shift 2 ;;
    --cloud)    CLOUD="$2"; shift 2 ;;
    --storage-class) STORAGE_CLASS="$2"; shift 2 ;;
    --env)      ENVPRESET="$2"; shift 2 ;;
    --env-name) ENV_NAME="$2"; shift 2 ;;
    # Staged: bring the control plane up on its own, look at it, then attach a data
    # plane. Both stages are idempotent, so --ycp-only followed by no flag at all is
    # the same end state as one combined run.
    --ycp-only)   STAGE=ycp; shift ;;
    --agent-only) STAGE=agent; shift ;;
    --all)        STAGE=all; shift ;;
    --agent-tag)  AGENT_TAG="$2"; shift 2 ;;
    --ycp-image)  YCP_IMAGE="$2"; shift 2 ;;
    --admin-key)  ADMIN_KEY="$2"; shift 2 ;;
    --delete)   DELETE=1; shift ;;
    -h|--help)  sed -n '2,34p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

log() { printf '\n==> %s\n' "$*"; }
die() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
k() { kubectl --context "$CTX" "$@"; }
h() { helm --kube-context "$CTX" "$@"; }

[ -n "$STAGE" ] || die "pick a stage explicitly — this installs into a real cluster:
    --ycp-only     the control plane (register environments in it yourself afterwards)
    --agent-only   register an environment and attach an agent to it
    --all          both, in one run
  With --delete, the same flags scope what is removed; --delete alone removes the
  data plane only."
[ -n "$CTX" ] || die "--context is required. Never guessed: this applies workloads to whatever it names.
  Available:
$(kubectl config get-contexts -o name | sed 's/^/    /')"
for bin in kubectl helm curl python3 aws; do
  command -v "$bin" >/dev/null 2>&1 || die "$bin is required but not on PATH"
done
CHART="$MEKO_CHART"
[ -d "$CHART" ] || die "no chart at $CHART — set MEKO_CHART to a meko checkout's deploy/helm/meko"
k version >/dev/null 2>&1 ||
  die "cannot reach $CTX. For a GKE context this is usually expired auth: gcloud auth login"

# ------------------------------------------------------------------------- delete

if [ "$DELETE" = "1" ]; then
  # Scoped by the same stage flags as install, because "delete the data plane" must
  # not take the control plane with it. Deleting a namespace is not recoverable by
  # re-running anything, so the default here is the narrow half: --delete on its own
  # removes the DATA plane only. Removing the control plane is --ycp-only --delete,
  # and both together is an explicit --all --delete.
  case "$STAGE" in
    ycp)   DELETE_NS=("$YCP_NS"); scope="the control plane" ;;
    all)   DELETE_NS=("$MEKO_NS" "$YCP_NS"); scope="both halves" ;;
    *)     DELETE_NS=("$MEKO_NS"); scope="the data plane" ;;
  esac
  log "removing $scope from $CTX: ${DELETE_NS[*]}"
  for ns in "${DELETE_NS[@]}"; do
    if k get ns "$ns" -o jsonpath="{.metadata.labels['meko\.dev/created-by']}" 2>/dev/null | grep -q ycp-on-cluster; then
      k delete ns "$ns" --wait=false
      echo "    deleting namespace $ns"
    else
      echo "    skipping $ns — not created by this script (or absent)"
    fi
  done
  exit 0
fi

[ -n "$TAG" ] || die "--tag is required: the api-server image tag in ECR to deploy.
  Build and push one from this checkout with:
    export AWS_PROFILE=meko-dev; TAG=bootstrap-\$(git rev-parse --short HEAD)
    podman build --platform linux/amd64 -f meko/api_server/Dockerfile \\
      -t $ECR/meko/meko-api-server:\$TAG meko/api_server
    podman push $ECR/meko/meko-api-server:\$TAG
  --platform linux/amd64 matters: the nodes are amd64 and this laptop may not be.
  The reconcile-agent is not built here — it publishes to Quay; see --agent-tag."

# Only the derived default is timestamped. --env-name has to stay the environment id so
# --agent-only can attach to a control plane that is already running one.
ENV_NAME="${ENV_NAME:-$MEKO_NS-$(printf '%s' "$CTX" | tr -c 'a-z0-9' '-' | tr -s '-' | sed 's/^-//;s/-$//' | tail -c 20)-$(date +%s)}"
ENV_NAME="$(printf '%s' "$ENV_NAME" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-' | tr -s '-' | sed 's/^-//;s/-$//')"

PF_PID=""
cleanup() {
  rc=$?
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  if [ "$rc" -ne 0 ]; then
    log "diagnostics"
    k -n "$YCP_NS" get pods 2>/dev/null | head -8 || true
    if k get ns "$MEKO_NS" >/dev/null 2>&1; then
      k -n "$MEKO_NS" get pods,statefulsets,deployments 2>/dev/null | head -16 || true
      k -n "$MEKO_NS" logs deploy/meko-reconcile-agent --tail=30 2>/dev/null | head -40 || true
    fi
  fi
  exit "$rc"
}
trap cleanup EXIT

log "target"
echo "    context:    $CTX"
echo "    ycp ns:     $YCP_NS   (control plane)"
echo "    meko ns:    $MEKO_NS  (agent + the stack it brings up)"
echo "    image tag:  $TAG"
echo "    presets:    values-$CLOUD.yaml + values-$ENVPRESET.yaml"

# --------------------------------------------------------------------- namespaces
#
# Created here, labeled, and never adopted: a namespace that already exists without
# our label belongs to someone else, and this is a shared cluster.

# Only the namespaces this stage needs: --ycp-only should not leave an empty `meko`
# namespace behind for a data plane that may never be installed.
case "$STAGE" in
  ycp)   STAGE_NS=("$YCP_NS") ;;
  agent) STAGE_NS=("$MEKO_NS") ;;
  *)     STAGE_NS=("$YCP_NS" "$MEKO_NS") ;;
esac

for ns in "${STAGE_NS[@]}"; do
  if k get ns "$ns" >/dev/null 2>&1; then
    k get ns "$ns" -o jsonpath="{.metadata.labels['meko\.dev/created-by']}" 2>/dev/null | grep -q ycp-on-cluster ||
      die "namespace $ns already exists and was not created by this script.
  Refusing to install into it — pick another with --ycp-ns / --meko-ns, or delete it
  deliberately if it is yours."
  else
    k create ns "$ns" >/dev/null
    k label ns "$ns" "$OWNER_LABEL" >/dev/null
    echo "    created namespace $ns"
  fi
done

# -------------------------------------------------------------------- pull secrets
#
# GKE has no AWS IAM, so every pod pulling from ECR needs a dockerconfigjson Secret
# in its own namespace — including the pods the AGENT creates in $MEKO_NS.

log "writing the ECR pull Secret into ${STAGE_NS[*]}"
ecr_token="$(aws ecr get-login-password --region "$ECR_REGION")" ||
  die "aws ecr get-login-password failed — set AWS_PROFILE to a profile with Meko ECR access"
for ns in "${STAGE_NS[@]}"; do
  k -n "$ns" create secret docker-registry "$PULL_SECRET" \
    --docker-server="$ECR" --docker-username=AWS --docker-password="$ecr_token" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
done
echo "    $PULL_SECRET (ECR tokens last ~12h; re-run this script to refresh)"

if [ "$STAGE" = ycp ]; then
  creds=()   # the data-plane credentials Secret lives with the agent, not here
else
creds=(--from-literal=placeholder=unused)
if [ -n "${OPENAI_API_KEY:-}" ]; then
  creds=(--from-literal=OPENAI_API_KEY="$OPENAI_API_KEY")
  echo "    $CREDS_SECRET with OPENAI_API_KEY"
else
  echo "    $CREDS_SECRET without OPENAI_API_KEY — the MCP server starts, and fails on the first embedding"
fi
k -n "$MEKO_NS" create secret generic "$CREDS_SECRET" "${creds[@]}" \
  --dry-run=client -o yaml | k apply -f - >/dev/null
fi

# ------------------------------------------------------------------- control plane

# Credentials: whatever is already in the cluster wins, and only a missing one is
# generated.
#
# This is not just convenience. `controlPlaneSecrets` renders these Secrets from
# values, so a re-run with a freshly generated dbPassword would rewrite the Secret
# while the bundled Postgres keeps the password its volume was initialised with —
# the api-server would then fail authentication against its own database, on an
# "upgrade" that changed nothing else. A regenerated admin key is milder but still
# breaks every operator's saved secret.
secretValue() { # ns name key
  k -n "$1" get secret "$2" -o "jsonpath={.data.$3}" 2>/dev/null | base64 -d 2>/dev/null || true
}
existingAdmin="$(secretValue "$YCP_NS" meko-admin-api-key admin-api-key)"
existingDB="$(secretValue "$YCP_NS" meko-api-server-db db-password)"
existingMCP="$(secretValue "$YCP_NS" meko-mcp-internal-secret secret-key)"
[ -n "$ADMIN_KEY" ] || ADMIN_KEY="$existingAdmin"
[ -n "$ADMIN_KEY" ] || ADMIN_KEY="$(python3 -c 'import secrets; print(secrets.token_urlsafe(24))')"
DB_PASSWORD="$existingDB"
[ -n "$DB_PASSWORD" ] || DB_PASSWORD="$(python3 -c 'import secrets; print(secrets.token_urlsafe(18))')"
# All three, not two: the chart renders each control-plane Secret only when its value
# is non-empty, and a missing meko-mcp-internal-secret is a secretKeyRef the pod
# cannot resolve — it sits in Init:CreateContainerConfigError until the install times
# out, which reads as a broken image rather than a missing value.
MCP_INTERNAL="$existingMCP"
[ -n "$MCP_INTERNAL" ] || MCP_INTERNAL="$(python3 -c 'import secrets; print(secrets.token_urlsafe(24))')"
if [ -n "$existingDB" ]; then
  echo "    reusing the database password already in $YCP_NS (a new one would not match the volume)"
fi

# The MCP image to seed. Without it the control plane seeds only a database, and the
# agent brings up half a stack — correct, but not what this script is here to show.
MCP_IMAGE="${MEKO_MCP_IMAGE:-}"
if [ -z "$MCP_IMAGE" ]; then
  t="$(aws ecr describe-images --repository-name meko/meko-mcp-server --region "$ECR_REGION" \
    --query 'reverse(sort_by(imageDetails,&imagePushedAt))[0].imageTags[0]' --output text 2>/dev/null || true)"
  [ -n "$t" ] && [ "$t" != "None" ] || die "could not read ECR for the MCP tag (set MEKO_MCP_IMAGE)"
  MCP_IMAGE="$ECR/meko/meko-mcp-server:$t"
fi

if [ "$STAGE" = agent ]; then
  k -n "$YCP_NS" get deploy meko-api-server >/dev/null 2>&1 ||
    die "--agent-only, but there is no control plane in $YCP_NS. Run with --ycp-only first."
  [ -n "$ADMIN_KEY" ] ||
    die "--agent-only needs the admin key: pass --admin-key, or let it be read from the
  meko-admin-api-key Secret in $YCP_NS (which is missing)."
  log "using the control plane already in $YCP_NS"
else
log "installing the control plane into $YCP_NS"
echo "    seeding new environments with: $MCP_IMAGE"
# --ycp-image wins over --tag: a full ref carries its own registry, repository and
# tag, and mixing the two would install the preset's registry with somebody else's
# tag — an image reference that resolves to nothing, or worse, to the wrong thing.
ycp_image_args=(--set-string "apiServer.image.tag=$TAG")
if [ -n "$YCP_IMAGE" ]; then
  ref="${YCP_IMAGE%:*}"; ycp_tag="${YCP_IMAGE##*:}"
  [ "$ref" != "$YCP_IMAGE" ] || die "--ycp-image needs a tag: <registry>/<repository>:<tag>"
  ycp_image_args=(
    --set-string "apiServer.image.registry=${ref%%/*}"
    --set-string "apiServer.image.repository=${ref#*/}"
    --set-string "apiServer.image.tag=$ycp_tag"
  )
  echo "    control-plane image: $YCP_IMAGE"
fi
h upgrade --install "$YCP_NS" "$CHART" -n "$YCP_NS" \
  -f "$CHART/values-$CLOUD.yaml" -f "$CHART/values-$ENVPRESET.yaml" -f "$CHART/values-ycp.yaml" \
  --set apiServer.namespace="$YCP_NS" \
  --set controlPlaneDb.namespace="$YCP_NS" \
  "${ycp_image_args[@]}" \
  --set controlPlaneSecrets.enabled=true \
  --set controlPlaneSecrets.dbPassword="$DB_PASSWORD" \
  --set controlPlaneSecrets.adminApiKey="$ADMIN_KEY" \
  --set controlPlaneSecrets.mcpInternalSecret="$MCP_INTERNAL" \
  --set apiServer.dataplane.mcpImage="$MCP_IMAGE" \
  --set apiServer.dataplane.imagePullSecret="$PULL_SECRET" \
  --set apiServer.dataplane.secretRef="$CREDS_SECRET" \
  --set ecrCredentialRefresh.enabled=false \
  ${STORAGE_CLASS:+--set global.storageClassName=$STORAGE_CLASS} \
  --wait --timeout "$TIMEOUT" >/dev/null ||
  die "the control-plane install failed (see: helm --kube-context $CTX -n $YCP_NS get all $YCP_NS)"
k -n "$YCP_NS" rollout status deploy/meko-api-server --timeout="$TIMEOUT"
fi

log "port-forwarding the control plane"
k -n "$YCP_NS" port-forward svc/meko-api-server-service "$PORT:80" >/dev/null 2>&1 &
PF_PID=$!
api() {
  local method="$1" path="$2"; shift 2
  curl -fsS -X "$method" "http://127.0.0.1:$PORT$path" -H "X-Meko-Admin-Secret: $ADMIN_KEY" "$@"
}
ready=""
for _ in $(seq 1 40); do
  ready="$(curl -fsS "http://127.0.0.1:$PORT/api/v1/health" 2>/dev/null || true)"
  [ -n "$ready" ] && break
  sleep 2
done
[ -n "$ready" ] || die "the control plane never answered /api/v1/health through the port-forward"
echo "    health -> $ready"

if [ "$STAGE" = ycp ]; then
cat <<EOF

==> PASS — YCP is running in $YCP_NS on $CTX.

    admin secret: $ADMIN_KEY
      (also in the meko-admin-api-key Secret in $YCP_NS, so a later run finds it)

    Console:   kubectl --context $CTX -n $YCP_NS port-forward svc/meko-api-server-service 18080:80
               http://127.0.0.1:18080/
    Fleet API: curl -H "X-Meko-Admin-Secret: $ADMIN_KEY" http://127.0.0.1:18080/api/v1/environments

    It has no environments yet. Attach a data plane — registers an environment and
    installs the agent into $MEKO_NS, which then brings Meko up on its own:

      $0 --context $CTX --ycp-ns $YCP_NS --meko-ns $MEKO_NS --tag $TAG --agent-only

    Newly registered environments will be seeded with:
      database/meko and mcp-server/mcp ($MCP_IMAGE)

    Tear down: $0 --context $CTX --ycp-ns $YCP_NS --meko-ns $MEKO_NS --delete
EOF
  exit 0
fi

# ----------------------------------------------------------------------- register

log "registering environment $ENV_NAME"
API_KEY_ID="$(python3 -c 'import uuid; print(uuid.uuid4())')"
# An environment declares nothing at registration — its workloads come from the
# chart, and its resources are POSTed afterwards.
reg_desired=',"desired":[]'
reg="$(api POST /api/v1/environments -H 'content-type: application/json' \
  -d "{\"name\":\"$ENV_NAME\",\"kind\":\"meko\",\"account_id\":\"22222222-2222-2222-2222-222222222222\",\"placement\":\"cloud\",\"region\":\"$ENVPRESET\",\"api_key_id\":\"$API_KEY_ID\"$reg_desired}")" ||
  die "registration failed"
ENV_ID="$(printf '%s' "$reg" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["environment"]["id"])')"

echo
echo "--------- the install command YCP printed ---------"
printf '%s' "$reg" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["install_hint"])'
echo "--------------------------------------------------"

api GET "/api/v1/environments/$ENV_ID/desired" | python3 -c '
import json, sys
res = json.load(sys.stdin)["data"]["resources"] or []
if res:
    sys.exit("    seeded %s — registration must declare nothing" % [r["kind"] for r in res])
print("    seeded nothing, as declared")
' || die "registration seeded something; it must declare nothing"

# --------------------------------------------------------------------------- agent

log "installing the agent into $MEKO_NS"
k -n "$MEKO_NS" create secret generic meko-agent-api-key \
  --from-literal=dev-admin-secret="$ADMIN_KEY" \
  --from-literal=dev-api-key-id="$API_KEY_ID" --dry-run=client -o yaml | k apply -f - >/dev/null

# In-cluster Service DNS, absolute (trailing dot). Both releases are in the same
# cluster, so the agent needs no external address — and the dot matters: ndots:5
# makes the four-dot relative form walk the search list first and stall.
YCP_URL="http://meko-api-server-service.$YCP_NS.svc.cluster.local.:80/api/v1"
# values-byoc.yaml LAST: it is what makes this one self-contained namespace — the
# stack plus its own YugabyteDB, and nothing outside it.
h upgrade --install "$MEKO_NS" "$CHART" -n "$MEKO_NS" \
  -f "$CHART/values-$CLOUD.yaml" -f "$CHART/values-$ENVPRESET.yaml" -f "$CHART/values-byoc.yaml" \
  --set-string apiServer.image.tag="$MEKO_API_TAG" \
  --set-string mekoMcpServer.image.tag="$MEKO_MCP_TAG" \
  --set-string mekoUi.image.tag="$MEKO_UI_TAG" \
  --set reconcileAgent.enabled=true \
  --set reconcileAgent.ycpUrl="$YCP_URL" \
  --set reconcileAgent.envName="$ENV_ID" \
  --set-string reconcileAgent.image.tag="$AGENT_TAG" \
  --set ecrCredentialRefresh.enabled=false \
  --wait --timeout "$TIMEOUT" >/dev/null ||
  die "the data-plane install failed (see: helm --kube-context $CTX -n $MEKO_NS get all $MEKO_NS).
  If the reconcile-agent image cannot be pulled, quay.io/yugabyte/ycp-reconciler-agent:$AGENT_TAG
  may never have been pushed — the workflow skips the push without QUAY_AMP_TOKEN.
  Build and push one with ycp/build-reconciler-agent-image.sh, then pass --agent-tag."

# ------------------------------------------------------- watch it bootstrap itself

log "waiting for the reconcile-agent to enroll"
enrolled=""
for _ in $(seq 1 60); do
  enrolled="$(k -n "$MEKO_NS" logs deploy/meko-reconcile-agent --tail=-1 2>/dev/null |
                grep "enrolled env=" | tail -1 || true)"
  [ -n "$enrolled" ] && break
  sleep 2
done
[ -n "$enrolled" ] ||
  die "the reconcile-agent never enrolled — kubectl -n $MEKO_NS logs deploy/meko-reconcile-agent"
printf '    %s\n' "$enrolled"

log "declaring a datapack and waiting for it to converge"
api POST "/api/v1/environments/$ENV_ID/desired" -H 'content-type: application/json' \
  -d '{"resources":[{"kind":"datapack","name":"smoke","spec":{"description":"declared by YCP"}}]}' \
  >/dev/null || die "POST desired failed"

converged=""
for _ in $(seq 1 90); do
  converged="$(api GET "/api/v1/environments/$ENV_ID" | python3 -c '
import json, sys
e = json.load(sys.stdin)["data"]
for r in json.loads(e.get("resources") or "[]"):
  if r.get("id", "").endswith("smoke"):
      print(r.get("state", ""), r.get("error", ""))
' 2>/dev/null | head -1 || true)"
  case "$converged" in ready*) break ;; esac
  sleep 2
done
case "$converged" in
  ready*) echo "    datapack smoke converged" ;;
  *) die "the datapack never converged (last reported: ${converged:-nothing}).
  kubectl --context $CTX -n $MEKO_NS logs -f deploy/meko-reconcile-agent" ;;
esac

cat <<EOF

==> PASS — $ENV_ID registered in $YCP_NS declaring nothing, the chart installed the
  stack in $MEKO_NS, and the reconcile-agent converged the datapack YCP declared.

  admin secret: $ADMIN_KEY

  Console:     kubectl --context $CTX -n $YCP_NS port-forward svc/meko-api-server-service 18080:80
  Agent loop:  kubectl --context $CTX -n $MEKO_NS logs -f deploy/meko-reconcile-agent
  Datapacks:   kubectl --context $CTX -n $MEKO_NS port-forward svc/meko-api-server-service 18001:80
               curl http://127.0.0.1:18001/api/v1/datapacks

  Declare more (desired state is REPLACED, so send the whole set):
    curl -X POST http://127.0.0.1:$PORT/api/v1/environments/$ENV_ID/desired \\
      -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H 'content-type: application/json' \\
      -d '{"resources":[{"kind":"datapack","name":"smoke"},{"kind":"datapack","name":"team-b"}]}'

  Withdraw one and it is garbage-collected. A datapack made through the UI carries a
  the account's signup agent, which the reconciler keeps out of its inventory
    entirely, so it is never collected.

  Tear down:   $0 --context $CTX --ycp-ns $YCP_NS --meko-ns $MEKO_NS --delete
EOF
