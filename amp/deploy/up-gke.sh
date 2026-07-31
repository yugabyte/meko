#!/usr/bin/env bash
#
# Bring up the whole amp stack on GKE, from scratch, using GCR/Artifact Registry
# instead of ECR. This is the Google-Cloud-native sibling of deploy/up.sh: no
# `aws`, no dshubin_bot, no ~12h ECR pull-secret dance. In order it:
#
#   1. (optional, BUILD=1) build + push the four images to GCR:
#      amp/controller, amp/endpoint, amp/controller-ui, amp/mcp
#   2. namespace
#   3. a pull secret, per PULL_SECRET / GCR_KEY_FILE / PULL_SECRET_DOCKERCONFIGJSON:
#      none by default (same-project GKE pulls from GCR via the node service
#      account — needs roles/artifactregistry.reader for Artifact Registry, or
#      storage.objectViewer for classic gcr.io buckets); one minted from a GCP SA
#      key; one minted from a raw docker config.json (e.g. a CI Quay secret); or an
#      EXISTING named secret (bring your own)
#   4. a single-node YugabyteDB (yugabyted) StatefulSet + headless Service
#   5. the yb_storage database (the controller connects to it at startup)
#   6. the amp controller (+ MCP + UI) via the yb-amp-controller Helm chart
#
# Idempotent: every step is create-or-apply / `helm upgrade --install`, so
# re-running converges. Re-running does NOT wipe YugabyteDB data (the StatefulSet
# keeps its PVC).
#
# Prereqs: kubectl (context on the target GKE cluster), helm, gcloud, docker.
#
# Usage:
#   deploy/up-gke.sh
#   CLUSTER=gke_yugabyte_us-west1_itest-dev NS=amp deploy/up-gke.sh
#   BUILD=1 TAG=$(date +%Y%m%d-%H%M%S) deploy/up-gke.sh          # build + push, then deploy
#   REGISTRY=us-west1-docker.pkg.dev/yugabyte/amp BUILD=1 deploy/up-gke.sh
#   PULL_SECRET=my-gcr-secret deploy/up-gke.sh                    # bring your own existing secret
#   PULL_SECRET=gcr-pull-secret GCR_KEY_FILE=./gcr-sa-key.json deploy/up-gke.sh  # mint from a GCP SA key
#   PULL_SECRET=quay-pull-secret PULL_SECRET_DOCKERCONFIGJSON="$(cat cfg.json)" deploy/up-gke.sh  # mint from a docker config.json
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHART_DIR="${SCRIPT_DIR}/helm/yb-amp-controller"

# --- configuration (override via environment) ---
# Namespace: NS wins, then AMP_NAMESPACE, then the default.
NS="${NS:-${AMP_NAMESPACE:-amp}}"
CLUSTER="${CLUSTER:-}"                               # kube-context to switch to; empty = current
REGISTRY="${REGISTRY:-gcr.io/yugabyte}"             # base registry + project/repo path (no trailing slash)
TAG="${TAG:-}"                                       # image tag; empty => latest (or a timestamp if BUILD=1)
BUILD="${BUILD:-0}"                                  # 1 => build + push all images first

# Image repo *names* under REGISTRY. Full ref is ${REGISTRY}/${repo}:${TAG}.
CONTROLLER_IMAGE_REPO="${CONTROLLER_IMAGE_REPO:-amp/controller}"
ENDPOINT_IMAGE_REPO="${ENDPOINT_IMAGE_REPO:-amp/endpoint}"
UI_IMAGE_REPO="${UI_IMAGE_REPO:-amp/controller-ui}"
MCP_IMAGE_REPO="${MCP_IMAGE_REPO:-amp/mcp}"

# Pull auth. Four modes, selected by PULL_SECRET / GCR_KEY_FILE /
# PULL_SECRET_DOCKERCONFIGJSON (checked in that order when PULL_SECRET is set):
#   PULL_SECRET empty (default)         -> no pull secret; rely on the GKE node
#                                          service account to pull from GCR (Option A).
#   PULL_SECRET + GCR_KEY_FILE          -> (re)mint that docker-registry secret from
#                                          the given GCP SA JSON key (GCR/_json_key).
#   PULL_SECRET + PULL_SECRET_DOCKERCONFIGJSON
#                                       -> (re)create that secret from a raw docker
#                                          config.json ({"auths":{...}}), e.g. a Quay
#                                          robot cred passed straight from a CI secret.
#                                          Works against a namespace this script just
#                                          created (nothing need exist beforehand).
#   PULL_SECRET only (neither of above) -> use an EXISTING secret of that name in the
#                                          namespace as-is (bring your own).
PULL_SECRET="${PULL_SECRET:-}"
GCR_KEY_FILE="${GCR_KEY_FILE:-}"                     # GCP SA JSON key to mint PULL_SECRET from (optional)
PULL_SECRET_DOCKERCONFIGJSON="${PULL_SECRET_DOCKERCONFIGJSON:-}"  # raw docker config.json to mint PULL_SECRET from (optional)

# Single-node YugabyteDB (yugabyted).
YB_IMAGE="${YB_IMAGE:-yugabytedb/yugabyte:latest}"
YB_STORAGE_SIZE="${YB_STORAGE_SIZE:-20Gi}"
# Stable in-cluster address of the yugabyted pod (StatefulSet headless Service DNS).
YB_FQDN="yugabyted-0.yugabyted.${NS}.svc.cluster.local"

# Exposure (GKE). Defaults to an internal LoadBalancer (private VPC IP).
#   CONTROLLER_EXPOSE     internal-lb (default) | external-lb | nodeport | none
#   ENDPOINT_SERVICE_TYPE ClusterIP (default) | NodePort | LoadBalancer
CONTROLLER_EXPOSE="${CONTROLLER_EXPOSE:-internal-lb}"
ENDPOINT_SERVICE_TYPE="${ENDPOINT_SERVICE_TYPE:-ClusterIP}"
# Endpoint data volume: a storage class backs each endpoint's /data with a PVC
# (Persistent Disk); empty (default) => an ephemeral emptyDir.
ENDPOINT_STORAGE_CLASS="${ENDPOINT_STORAGE_CLASS:-}"
ENDPOINT_DISK_SIZE="${ENDPOINT_DISK_SIZE:-10Gi}"
# GKE internal-LB annotations (used for internal-lb controller/UI Services).
LB_ANNOTATIONS="${LB_ANNOTATIONS:-networking.gke.io/load-balancer-type=Internal,networking.gke.io/internal-load-balancer-allow-global-access=true}"
MCP_ENABLED="${MCP_ENABLED:-true}"
UI_ENABLED="${UI_ENABLED:-true}"

say()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31merror: %s\033[0m\n' "$*" >&2; exit 1; }

# Registry host (for `gcloud auth configure-docker`): gcr.io or <region>-docker.pkg.dev.
REG_HOST="${REGISTRY%%/*}"

# --- preflight ---
say "preflight"
for bin in kubectl helm gcloud docker; do
  command -v "$bin" >/dev/null 2>&1 || die "$bin not found on PATH"
done
[ -n "${CLUSTER}" ] && kubectl config use-context "${CLUSTER}"
CTX="$(kubectl config current-context 2>/dev/null || true)"
[ -n "$CTX" ] || die "no kubectl context set (pass CLUSTER=... or select one)"
case "${CONTROLLER_EXPOSE}" in
  nodeport|internal-lb|external-lb|none) ;;
  *) die "invalid CONTROLLER_EXPOSE=${CONTROLLER_EXPOSE} (want nodeport|internal-lb|external-lb|none)" ;;
esac
if [ "${BUILD}" = "1" ] && [ -z "${TAG}" ]; then TAG="$(date +%Y%m%d-%H%M%S)"; fi
TAG="${TAG:-latest}"
echo "   kube-context : ${CTX}"
echo "   namespace    : ${NS}"
echo "   registry     : ${REGISTRY}   (host ${REG_HOST})"
echo "   tag          : ${TAG}   (build=${BUILD})"
echo "   expose       : ${CONTROLLER_EXPOSE}   endpoints=${ENDPOINT_SERVICE_TYPE}"

# --- 1. (optional) build + push images to GCR ---
if [ "${BUILD}" = "1" ]; then
  say "gcloud docker auth for ${REG_HOST}"
  gcloud auth configure-docker "${REG_HOST}" --quiet

  say "build + push controller (${REGISTRY}/${CONTROLLER_IMAGE_REPO}:${TAG})"
  make -C "${REPO_ROOT}" controller-bundle BUILD_TYPE=release
  docker build -t "${REGISTRY}/${CONTROLLER_IMAGE_REPO}:${TAG}" \
    -f "${SCRIPT_DIR}/controller/Dockerfile" "${REPO_ROOT}"
  docker push "${REGISTRY}/${CONTROLLER_IMAGE_REPO}:${TAG}"

  say "build + push endpoint (${REGISTRY}/${ENDPOINT_IMAGE_REPO}:${TAG})"
  docker build -t "${REGISTRY}/${ENDPOINT_IMAGE_REPO}:${TAG}" \
    -f "${SCRIPT_DIR}/endpoint/Dockerfile" "${REPO_ROOT}"
  docker push "${REGISTRY}/${ENDPOINT_IMAGE_REPO}:${TAG}"

  if [ "${UI_ENABLED}" = "true" ]; then
    say "build + push UI (${REGISTRY}/${UI_IMAGE_REPO}:${TAG})"
    docker build -t "${REGISTRY}/${UI_IMAGE_REPO}:${TAG}" "${REPO_ROOT}/amp_ui"
    docker push "${REGISTRY}/${UI_IMAGE_REPO}:${TAG}"
  fi

  if [ "${MCP_ENABLED}" = "true" ]; then
    say "build + push MCP (${REGISTRY}/${MCP_IMAGE_REPO}:${TAG})"
    docker build -t "${REGISTRY}/${MCP_IMAGE_REPO}:${TAG}" \
      -f "${REPO_ROOT}/controller/mcp/Dockerfile" "${REPO_ROOT}/controller/mcp"
    docker push "${REGISTRY}/${MCP_IMAGE_REPO}:${TAG}"
  fi
fi

# --- 2. namespace ---
say "namespace ${NS}"
kubectl create namespace "${NS}" --dry-run=client -o yaml | kubectl apply -f -

# --- 3. pull secret ---
# See the PULL_SECRET / GCR_KEY_FILE / PULL_SECRET_DOCKERCONFIGJSON modes in the
# config block above. The mint paths run against the namespace created just above,
# so nothing has to exist beforehand (the CI case: a fresh per-run namespace).
if [ -z "${PULL_SECRET}" ]; then
  say "pull secret: none (relying on the GKE node service account for GCR pulls)"
elif [ -n "${GCR_KEY_FILE}" ]; then
  [ -f "${GCR_KEY_FILE}" ] || die "GCR_KEY_FILE=${GCR_KEY_FILE} not found"
  say "GCR pull secret '${PULL_SECRET}' (minting from ${GCR_KEY_FILE})"
  kubectl -n "${NS}" create secret docker-registry "${PULL_SECRET}" \
    --docker-server="${REG_HOST}" \
    --docker-username=_json_key \
    --docker-password="$(cat "${GCR_KEY_FILE}")" \
    --docker-email="amp@yugabyte.com" \
    --dry-run=client -o yaml | kubectl apply -f -
elif [ -n "${PULL_SECRET_DOCKERCONFIGJSON}" ]; then
  say "pull secret '${PULL_SECRET}' (creating from PULL_SECRET_DOCKERCONFIGJSON)"
  # Route the config.json through a temp file rather than --from-literal so the
  # credential never lands in argv / a process listing.
  _dcj="$(mktemp)"
  printf '%s' "${PULL_SECRET_DOCKERCONFIGJSON}" > "${_dcj}"
  kubectl -n "${NS}" create secret generic "${PULL_SECRET}" \
    --type=kubernetes.io/dockerconfigjson \
    --from-file=.dockerconfigjson="${_dcj}" \
    --dry-run=client -o yaml | kubectl apply -f -
  rm -f "${_dcj}"
else
  say "pull secret: using existing '${PULL_SECRET}' in namespace ${NS}"
  kubectl -n "${NS}" get secret "${PULL_SECRET}" >/dev/null 2>&1 \
    || die "pull secret '${PULL_SECRET}' not found in namespace ${NS} (create it, pass GCR_KEY_FILE=<key> or PULL_SECRET_DOCKERCONFIGJSON=<json> to mint it, or unset PULL_SECRET to use the node SA)"
fi

# --- 4. single-node YugabyteDB (yugabyted) ---
say "YugabyteDB (single-node yugabyted)"
kubectl apply -f - <<YAML
apiVersion: v1
kind: Service
metadata:
  name: yugabyted
  namespace: ${NS}
  labels: { app: yugabyted }
spec:
  clusterIP: None
  publishNotReadyAddresses: true
  selector: { app: yugabyted }
  ports:
    - { name: ysql, port: 5433, targetPort: 5433 }
    - { name: master-ui, port: 7000, targetPort: 7000 }
    - { name: tserver-ui, port: 9000, targetPort: 9000 }
    - { name: yugabyted-ui, port: 15433, targetPort: 15433 }
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: yugabyted
  namespace: ${NS}
  labels: { app: yugabyted }
spec:
  serviceName: yugabyted
  replicas: 1
  selector:
    matchLabels: { app: yugabyted }
  template:
    metadata:
      labels: { app: yugabyted }
    spec:
      terminationGracePeriodSeconds: 60
      securityContext:
        fsGroup: 1000
      containers:
        - name: yugabyted
          image: ${YB_IMAGE}
          imagePullPolicy: IfNotPresent
          command: ["bash", "-c"]
          args:
            - |
              exec bin/yugabyted start --background=false --base_dir=/data --advertise_address="\$(hostname -f)" --tserver_flags=ysql_enable_auth=false
          ports:
            - { name: ysql, containerPort: 5433 }
            - { name: master-ui, containerPort: 7000 }
            - { name: tserver-ui, containerPort: 9000 }
            - { name: yugabyted-ui, containerPort: 15433 }
          volumeMounts:
            - { name: data, mountPath: /data }
          resources:
            requests:
              cpu: "500m"
              memory: "2Gi"
          readinessProbe:
            exec:
              command:
                - bash
                - -c
                - 'bin/ysqlsh -h "\$(hostname -f)" -p 5433 -U yugabyte -d yugabyte -c "select 1" >/dev/null 2>&1'
            initialDelaySeconds: 20
            periodSeconds: 10
            timeoutSeconds: 5
            failureThreshold: 60
          livenessProbe:
            tcpSocket: { port: 5433 }
            initialDelaySeconds: 90
            periodSeconds: 20
            failureThreshold: 6
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: ${YB_STORAGE_SIZE}
YAML

echo ">> waiting for yugabyted to be ready (this can take a few minutes on first start)..."
kubectl -n "${NS}" rollout status statefulset/yugabyted --timeout=900s

# --- 5. the yb_storage database ---
# The controller's db.Connect targets yb_storage at startup, so it must exist
# before the controller rolls out. yugabyted binds YSQL to its advertise address
# (the pod FQDN), so connect there rather than loopback.
say "database yb_storage"
kubectl -n "${NS}" exec -i yugabyted-0 -- bash -s <<'REMOTE'
set -euo pipefail
H="$(hostname -f)"
# Exactly "1" when the DB exists, empty when absent (-tA = tuples-only/unaligned).
exists="$(bin/ysqlsh -h "$H" -p 5433 -U yugabyte -d yugabyte -tAc \
  "SELECT 1 FROM pg_database WHERE datname='yb_storage'")"
if [ "$exists" = "1" ]; then
  echo "   yb_storage already exists"
elif out="$(bin/ysqlsh -h "$H" -p 5433 -U yugabyte -d yugabyte \
            -c "CREATE DATABASE yb_storage" 2>&1)"; then
  echo "   created yb_storage"
elif printf '%s' "$out" | grep -q "already exists"; then
  # PG has no CREATE DATABASE IF NOT EXISTS; tolerate a racy create.
  echo "   yb_storage already exists"
else
  printf '   CREATE DATABASE yb_storage failed: %s\n' "$out" >&2
  exit 1
fi
REMOTE

# --- 6. the amp controller (+ MCP + UI) via Helm ---
say "amp controller (Helm: controller=${REGISTRY}/${CONTROLLER_IMAGE_REPO}:${TAG})"
DB_URL="postgres://yugabyte@${YB_FQDN}:5433/yb_storage?sslmode=disable"

HELM_SET=(
  "controller.image.repository=${REGISTRY}/${CONTROLLER_IMAGE_REPO}"
  "controller.image.tag=${TAG}"
  "controller.endpoint.image.repository=${REGISTRY}/${ENDPOINT_IMAGE_REPO}"
  "controller.endpoint.image.tag=${TAG}"
  "controller.endpoint.ybHostPorts=${YB_FQDN}:5433"
  "controller.endpoint.service.type=${ENDPOINT_SERVICE_TYPE}"
  "controller.endpoint.storageClass=${ENDPOINT_STORAGE_CLASS}"
  "controller.endpoint.diskSize=${ENDPOINT_DISK_SIZE}"
  "controller.service.type=${CONTROLLER_EXPOSE}"
  "mcp.enabled=${MCP_ENABLED}"
  "mcp.image.repository=${REGISTRY}/${MCP_IMAGE_REPO}"
  "mcp.image.tag=${TAG}"
  "ui.enabled=${UI_ENABLED}"
  "ui.image.repository=${REGISTRY}/${UI_IMAGE_REPO}"
  "ui.image.tag=${TAG}"
)
[ -n "${PULL_SECRET}" ] && HELM_SET+=("imagePullSecrets[0].name=${PULL_SECRET}")

HELM_ARGS=(upgrade --install yb-amp-controller "${CHART_DIR}" -n "${NS}"
           --set-string "controller.databaseUrl=${DB_URL}")
for s in "${HELM_SET[@]}"; do HELM_ARGS+=(--set "${s}"); done

# internal-lb Service annotations contain dots (--set would mis-tokenize them),
# so render them into a tiny values file and pass it with -f.
HELM_VALUES_FILE=""
cleanup() { [ -n "${HELM_VALUES_FILE}" ] && rm -f "${HELM_VALUES_FILE}"; return 0; }
trap cleanup EXIT
if [ "${CONTROLLER_EXPOSE}" = "internal-lb" ]; then
  HELM_VALUES_FILE="$(mktemp)"
  {
    echo "controller:"
    echo "  service:"
    echo "    annotations:"
    IFS=',' read -ra _ann <<<"${LB_ANNOTATIONS}" || true
    for a in "${_ann[@]}"; do [ -n "$a" ] && echo "      - \"${a}\""; done
    echo "ui:"
    echo "  service:"
    echo "    annotations:"
    for a in "${_ann[@]}"; do [ -n "$a" ] && echo "      - \"${a}\""; done
  } >"${HELM_VALUES_FILE}"
  HELM_ARGS+=(-f "${HELM_VALUES_FILE}")
fi

echo ">> helm ${HELM_ARGS[*]}"
helm "${HELM_ARGS[@]}"

echo ">> waiting for rollout..."
kubectl -n "${NS}" rollout status deploy/amp-controller --timeout=180s
[ "${UI_ENABLED}" = "true" ]  && kubectl -n "${NS}" rollout status deploy/amp-controller-ui  --timeout=180s
[ "${MCP_ENABLED}" = "true" ] && kubectl -n "${NS}" rollout status deploy/amp-controller-mcp --timeout=180s

# --- done ---
say "stack is up in namespace '${NS}'"
echo
echo "  Controller UI / API (UI at /, REST API under /api/v1):"
case "${CONTROLLER_EXPOSE}" in
  internal-lb|external-lb)
    LBIP="$(kubectl -n "${NS}" get svc amp-controller-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [ -n "${LBIP}" ]; then
      echo "    http://${LBIP}:8080   (${CONTROLLER_EXPOSE})"
    else
      echo "    ${CONTROLLER_EXPOSE} IP still provisioning — watch: kubectl -n ${NS} get svc amp-controller-lb -w"
    fi
    ;;
  nodeport)
    NP="$(kubectl -n "${NS}" get svc amp-controller-np -o jsonpath='{.spec.ports[0].nodePort}' 2>/dev/null || true)"
    NODEIP="$(kubectl get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null | awk '{print $1}' || true)"
    [ -n "${NODEIP}" ] || NODEIP="$(kubectl get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null | awk '{print $1}' || true)"
    echo "    http://${NODEIP:-<node-ip>}:${NP:-<pending>}   (NodePort — reachable at any node IP)"
    ;;
  *)
    echo "    (no external Service — port-forward only)"
    ;;
esac
echo
echo "  From anywhere with kubectl access (works from outside the VPC too):"
echo "    kubectl -n ${NS} port-forward svc/amp-controller 8080:8080     then http://localhost:8080"
[ "${UI_ENABLED}" = "true" ] && \
  echo "    kubectl -n ${NS} port-forward svc/amp-controller-ui 8081:80   then http://localhost:8081"
echo

exit 0
