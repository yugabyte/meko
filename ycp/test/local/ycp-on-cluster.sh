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
#   --reconcile picks the reconcile-agent data plane: the chart installs the stack and
#   the agent converges the environment's datapacks over the api-server. The default
#   is the meko-agent, which reconciles a seeded meko-stack into k8s.
#     ./ycp/test/local/ycp-on-cluster.sh --context <ctx> --all --reconcile \
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
# --reconcile installs the OTHER data-plane model: the chart brings the stack up and
# the reconcile-agent converges the environment's datapacks over the api-server,
# instead of the meko-agent reconciling a seeded meko-stack into k8s. The two are
# alternatives — one YCP client per environment — so this is a mode, not a flag that
# adds something.
RECONCILE=0
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
    --reconcile)  RECONCILE=1; shift ;;
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
  # Namespaces the agent created itself are labeled by the agent, not by this script,
  # and sit outside both namespaces above. They belong to the data plane, so they go
  # only when the data plane does.
  if [ "$STAGE" != ycp ]; then
    for dpns in $(k get ns -l app.kubernetes.io/managed-by=meko-agent -o name 2>/dev/null | cut -d/ -f2); do
      k delete ns "$dpns" --wait=false && echo "    deleting agent-managed namespace $dpns"
    done
  fi
  exit 0
fi

[ -n "$TAG" ] || die "--tag is required: the image tag in ECR to deploy.
  Build and push one from this checkout with:
    export AWS_PROFILE=meko-dev; TAG=bootstrap-\$(git rev-parse --short HEAD)
    for spec in meko/api_server/Dockerfile:meko/api_server:meko-api-server \\
                ycp/Dockerfile.agent:ycp:meko-agent; do
      df=\${spec%%:*}; r=\${spec#*:}; podman build --platform linux/amd64 \\
        -f \$df -t $ECR/meko/\${r#*:}:\$TAG \${r%%:*} && podman push $ECR/meko/\${r#*:}:\$TAG
    done
  --platform linux/amd64 matters: the nodes are amd64 and this laptop may not be."

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
      k -n "$MEKO_NS" logs deploy/meko-agent --tail=30 2>/dev/null | head -40 || true
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
# "desired": [] is the whole opt-in to the chart-installed model: an environment that
# declares nothing has no stack for the meko-agent to reconcile, and the control
# plane's install hint prints the reconcile-agent commands for exactly that shape.
reg_desired=""
[ "$RECONCILE" = 1 ] && reg_desired=',"desired":[]'
reg="$(api POST /api/v1/environments -H 'content-type: application/json' \
  -d "{\"name\":\"$ENV_NAME\",\"kind\":\"meko\",\"account_id\":\"22222222-2222-2222-2222-222222222222\",\"placement\":\"cloud\",\"region\":\"$ENVPRESET\",\"api_key_id\":\"$API_KEY_ID\"$reg_desired}")" ||
  die "registration failed"
ENV_ID="$(printf '%s' "$reg" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["environment"]["id"])')"

echo
echo "--------- the install command YCP printed ---------"
printf '%s' "$reg" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["install_hint"])'
echo "--------------------------------------------------"

if [ "$RECONCILE" = 1 ]; then
  api GET "/api/v1/environments/$ENV_ID/desired" | python3 -c '
import json, sys
res = json.load(sys.stdin)["data"]["resources"] or []
if res:
    sys.exit("    seeded %s — a reconcile-agent environment must declare nothing"
             % [r["kind"] for r in res])
print("    seeded nothing, as declared")
' || die "registration seeded a stack; --reconcile registers an environment that declares none"
else
api GET "/api/v1/environments/$ENV_ID/desired" | python3 -c '
import json, sys
res = json.load(sys.stdin)["data"]["resources"]
print("    seeded at registration: " + ", ".join(r["kind"] + "/" + r["name"] for r in res))
stack = next((r for r in res if r["kind"] == "meko-stack"), None)
if stack is None:
    sys.exit("    no meko-stack seeded — registration should resolve the whole release")
images = stack["spec"].get("images", {})
missing = [c for c in ("database", "api_server", "mcp_server", "rag_worker", "inference_gateway", "ui")
           if not images.get(c)]
if missing:
    sys.exit("    seeded stack has no image for %s" % ", ".join(missing))
for c in sorted(images):
    print("      %-18s %s" % (c, images[c]))
if not stack["spec"].get("image_pull_secret"):
    sys.exit("    seeded stack names no image_pull_secret — every image in it is private, "
             "so nothing would pull")
' || die "registration did not seed the stack"
fi

# --------------------------------------------------------------------------- agent

log "installing the agent into $MEKO_NS"
k -n "$MEKO_NS" create secret generic meko-agent-api-key \
  --from-literal=dev-admin-secret="$ADMIN_KEY" \
  --from-literal=dev-api-key-id="$API_KEY_ID" --dry-run=client -o yaml | k apply -f - >/dev/null

# In-cluster Service DNS, absolute (trailing dot). Both releases are in the same
# cluster, so the agent needs no external address — and the dot matters: ndots:5
# makes the four-dot relative form walk the search list first and stall.
YCP_URL="http://meko-api-server-service.$YCP_NS.svc.cluster.local.:80/api/v1"
if [ "$RECONCILE" = 1 ]; then
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
else
  h upgrade --install "$MEKO_NS" "$CHART" -n "$MEKO_NS" \
    -f "$CHART/values-$CLOUD.yaml" -f "$CHART/values-$ENVPRESET.yaml" -f "$CHART/values-agent.yaml" \
    --set agent.ycpUrl="$YCP_URL" \
    --set agent.envName="$ENV_ID" \
    --set-string agent.image.tag="$TAG" \
    --set agent.provider="$CLOUD" \
    --set ecrCredentialRefresh.enabled=false \
    --wait --timeout "$TIMEOUT" >/dev/null ||
    die "the agent install failed (see: helm --kube-context $CTX -n $MEKO_NS get all $MEKO_NS)"
fi

# ------------------------------------------------------- watch it bootstrap itself

if [ "$RECONCILE" = 1 ]; then
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
    different created_by_agent_id and is never touched.

    Tear down:   $0 --context $CTX --ycp-ns $YCP_NS --meko-ns $MEKO_NS --delete
EOF
  exit 0
fi

log "waiting for the agent to enroll"
enrolled=""
for _ in $(seq 1 45); do
  enrolled="$(k -n "$MEKO_NS" logs deploy/meko-agent --tail=-1 2>/dev/null |
                grep "enrolled: environment_id=$ENV_ID" | tail -1 || true)"
  [ -n "$enrolled" ] && break
  sleep 2
done
[ -n "$enrolled" ] || die "the agent never enrolled — kubectl -n $MEKO_NS logs deploy/meko-agent"
printf '    %s\n' "$enrolled"

log "waiting for the database the agent was never told to run"
# Found by label, not by name. The stack names its objects after the resource it was
# seeded as (meko-yugabytedb), and a hardcoded name here is exactly what broke this
# script when registration started seeding a meko-stack instead of a database.
DB=""
for _ in $(seq 1 60); do
  DB="$(k -n "$MEKO_NS" get statefulset -l meko.dev/stack=true \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  [ -n "$DB" ] && break
  sleep 2
done
[ -n "$DB" ] ||
  die "no database StatefulSet — the agent enrolled but did not act on its desired state"
echo "    StatefulSet $DB"
k -n "$MEKO_NS" rollout status "statefulset/$DB" --timeout="$TIMEOUT"

log "waiting for the Meko services (the first meko-mcp-server pull is several GB)"
# Every Deployment the stack owns, by label. The inference gateway is skipped without an
# OPENAI_API_KEY: it builds its LLM client at import, so it exits rather than degrades,
# and that is missing configuration rather than a fault in the stack.
for dep in $(k -n "$MEKO_NS" get deploy -l meko.dev/stack=true \
             -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
  case "$dep" in
    *inference-gateway)
      if [ -z "${OPENAI_API_KEY:-}" ]; then
        echo "    skipping $dep — it needs OPENAI_API_KEY to start at all"
        continue
      fi ;;
  esac
  k -n "$MEKO_NS" rollout status "deploy/$dep" --timeout="$TIMEOUT" || die "$dep never became ready"
done

log "checking the fleet API reports the stack healthy"
# Which components to require. `yugabytedb`, not `postgres`: a component reports the
# engine it is running, and the stack's engine is yugabytedb. The gateway is left out
# when there is no OPENAI_API_KEY — it builds its LLM client at import so it never
# starts, and READY means EVERY declared component is up, so READY is then unreachable
# and demanding it would fail the run over missing configuration.
WANT="meko-agent yugabytedb api-server mcp-server rag-worker ui"
[ -n "${OPENAI_API_KEY:-}" ] && WANT="$WANT inference-gateway"
for _ in $(seq 1 40); do
  envs="$(api GET /api/v1/environments 2>/dev/null || true)"
  python3 - "$ENV_ID" "$envs" "$WANT" >/dev/null 2>&1 <<'PY' && break
import json, sys
items = json.loads(sys.argv[2] or "{}").get("data", {}).get("items") or []
e = next((x for x in items if x["id"] == sys.argv[1]), None)
comps = {c["name"]: c for c in ((e or {}).get("components") or [])}
sys.exit(0 if all(w in comps and comps[w]["total"] > 0 and comps[w]["healthy"] == comps[w]["total"]
                  for w in sys.argv[3].split()) else 1)
PY
  sleep 5
done
python3 - "$ENV_ID" "${envs:-}" "$WANT" "${OPENAI_API_KEY:-}" <<'PY' || die "the fleet API does not report a healthy stack"
import json, sys
items = json.loads(sys.argv[2] or "{}").get("data", {}).get("items") or []
e = next((x for x in items if x["id"] == sys.argv[1]), None)
if e is None:
    sys.exit("%s not in the fleet" % sys.argv[1])
comps = {c["name"]: c for c in (e.get("components") or [])}
print("    state=%s" % e["state"])
for c in (e.get("components") or []):
    print("      %-18s %d/%d %s%s" % (c["name"], c["healthy"], c["total"],
                                      c.get("version") or "",
                                      "  ERROR: " + c["error"] if c.get("error") else ""))
missing = [w for w in sys.argv[3].split()
           if w not in comps or comps[w]["total"] == 0 or comps[w]["healthy"] != comps[w]["total"]]
if missing:
    sys.exit("not reported healthy: %s" % ", ".join(missing))
if not sys.argv[4]:
    print("    (PROVISIONING is expected without OPENAI_API_KEY: the gateway is declared "
          "but cannot start. Set it to reach READY.)")
elif e["state"] != "READY":
    sys.exit("state=%s, want READY" % e["state"])
PY

# The images to print in the example below, read back from what registration actually
# seeded, so the command matches what is running. A hand-written example drifts from the
# pinned release, and a partial one is worse: a placeholder that reaches a pod spec
# produces InvalidImageName rather than an error.
seeded_images="$(api GET "/api/v1/environments/$ENV_ID/desired" | python3 -c '
import json, sys
res = json.load(sys.stdin)["data"]["resources"]
stack = next((r for r in res if r["kind"] == "meko-stack"), None)
i = (stack or {}).get("spec", {}).get("images", {})
for k in ("database", "api_server", "mcp_server", "rag_worker", "inference_gateway", "ui"):
    print(i.get(k, ""))
' 2>/dev/null || true)"
# Defaulted, because `set -u` turns an empty read into "unbound variable" and kills the
# run AFTER everything succeeded — which is exactly what happened when this block sat in
# the wrong branch.
DB_IMAGE="$(printf '%s\n' "$seeded_images" | sed -n 1p)"; DB_IMAGE="${DB_IMAGE:-<database image>}"
API_IMAGE="$(printf '%s\n' "$seeded_images" | sed -n 2p)"; API_IMAGE="${API_IMAGE:-<api_server image>}"
MCP_IMAGE="$(printf '%s\n' "$seeded_images" | sed -n 3p)"; MCP_IMAGE="${MCP_IMAGE:-<mcp_server image>}"
RAG_IMAGE="$(printf '%s\n' "$seeded_images" | sed -n 4p)"; RAG_IMAGE="${RAG_IMAGE:-<rag_worker image>}"
GW_IMAGE="$(printf '%s\n' "$seeded_images" | sed -n 5p)"; GW_IMAGE="${GW_IMAGE:-<inference_gateway image>}"
UI_IMAGE="$(printf '%s\n' "$seeded_images" | sed -n 6p)"; UI_IMAGE="${UI_IMAGE:-<ui image>}"

cat <<EOF

==> PASS — $ENV_ID registered in $YCP_NS, agent installed into $MEKO_NS, Meko up on its own.

    admin secret: $ADMIN_KEY
      (generated for this install; also in the meko-admin-api-key Secret in $YCP_NS)

    Console:     kubectl --context $CTX -n $YCP_NS port-forward svc/meko-api-server-service 18080:80
                 http://127.0.0.1:18080/
    MCP server:  kubectl --context $CTX -n $MEKO_NS port-forward svc/meko-mcp-server 18000:8000
                 then POST http://127.0.0.1:18000/mcp
    Agent loop:  kubectl --context $CTX -n $MEKO_NS logs -f deploy/meko-agent
    The stack:   kubectl --context $CTX -n $MEKO_NS get all,cm -l meko.dev/stack=true

    Change what it runs. Desired state is REPLACED, so send the whole set — and the
    console reads it but does not edit it, so this is the way:
      curl -X POST http://127.0.0.1:$PORT/api/v1/environments/$ENV_ID/desired \\
        -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H 'content-type: application/json' \\
        -d '{"resources":[{"kind":"meko-stack","name":"meko","spec":{
              "images":{"database":"$DB_IMAGE",
                        "api_server":"$API_IMAGE",
                        "mcp_server":"$MCP_IMAGE",
                        "rag_worker":"$RAG_IMAGE",
                        "inference_gateway":"$GW_IMAGE",
                        "ui":"$UI_IMAGE"},
              "image_pull_secret":"$PULL_SECRET",
              "secret_ref":"$CREDS_SECRET",
              "disable":["inference-gateway"]}}]}'

    (disable drops a component; the gateway is dropped above because it needs an
    OPENAI_API_KEY in $CREDS_SECRET to start at all. Withdraw everything with
    '{"resources":[]}' and the agent garbage-collects the stack.)

    Tear down:   $0 --context $CTX --ycp-ns $YCP_NS --meko-ns $MEKO_NS --delete
EOF
