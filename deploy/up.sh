#!/usr/bin/env bash
#
# Bring up the whole amp stack on Kubernetes from scratch, in one shot:
#
#   1. namespace
#   2. ECR image pull secret  (aws ecr get-login-password | docker login, then a
#      k8s docker-registry secret so the cluster can pull amp/controller + amp/endpoint)
#   3. a single-node YugabyteDB (yugabyted) StatefulSet + headless Service
#   4. the yb_storage database (the controller connects to it at startup)
#   5. the amp controller (controller + managed storage_controller, Helm endpoint
#      driver) via deploy/controller/redeploy.sh
#
# then prints the URL for the controller UI.
#
# This is the from-nothing companion to deploy/controller/redeploy.sh — that
# script (re-)deploys only the controller and assumes the namespace, YugabyteDB,
# and pull secret already exist; this one creates all three first.
#
# Idempotent: every step is create-or-apply, so re-running converges. Re-running
# does NOT wipe YugabyteDB data (the StatefulSet keeps its PVC).
#
# Prereqs: kubectl (pointed at the target cluster), helm, docker, aws (with
# credentials for the ECR account), and the amp/controller + amp/endpoint images
# already pushed to ECR.
#
# Usage:  deploy/up.sh
#   override any default via env, e.g.:
#     NS=amp deploy/up.sh                       # namespace; also reads AMP_NAMESPACE, default amp
#     CONTROLLER_TAG=20260620-120000 ENDPOINT_TAG=20260620-120000 deploy/up.sh
#     CONTROLLER_EXPOSE=internal-lb deploy/up.sh   # internal LB instead of NodePort
#     ENDPOINT_SERVICE_TYPE=NodePort deploy/up.sh  # endpoints as NodePort (<nodeIP>:<nodePort>) instead of ClusterIP
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- configuration (override via environment) ---
# Namespace: NS wins, then AMP_NAMESPACE, then the default.
NS="${NS:-${AMP_NAMESPACE:-amp}}"
REGION="${REGION:-us-west-2}"
ECR="${ECR:-454529406029.dkr.ecr.us-west-2.amazonaws.com}"
PULL_SECRET="${PULL_SECRET:-pull-secret}"

# Image repo paths within the ECR registry, and the tags to deploy. If a tag is
# left unset it is auto-discovered as the most recently pushed tag in the repo.
CONTROLLER_IMAGE_REPO="${CONTROLLER_IMAGE_REPO:-amp/controller}"
ENDPOINT_IMAGE_REPO="${ENDPOINT_IMAGE_REPO:-amp/endpoint}"
CONTROLLER_TAG="${CONTROLLER_TAG:-}"
ENDPOINT_TAG="${ENDPOINT_TAG:-}"

# Single-node YugabyteDB (yugabyted).
YB_IMAGE="${YB_IMAGE:-yugabytedb/yugabyte:latest}"
YB_STORAGE_SIZE="${YB_STORAGE_SIZE:-20Gi}"
# Stable in-cluster address of the yugabyted pod (StatefulSet headless Service DNS).
YB_FQDN="yugabyted-0.yugabyted.${NS}.svc.cluster.local"

# Exposure — defaults avoid slow cloud LBs (see redeploy.sh for the full menu):
#   CONTROLLER_EXPOSE     nodeport (default) | internal-lb | external-lb | none
#   ENDPOINT_SERVICE_TYPE ClusterIP (default) | NodePort | LoadBalancer
CONTROLLER_EXPOSE="${CONTROLLER_EXPOSE:-nodeport}"
ENDPOINT_SERVICE_TYPE="${ENDPOINT_SERVICE_TYPE:-ClusterIP}"
# Endpoint data volume: a storage class backs each endpoint's /data with a PVC
# (EBS/PD); empty (default) => an ephemeral emptyDir. ENDPOINT_DISK_SIZE sizes the PVC.
ENDPOINT_STORAGE_CLASS="${ENDPOINT_STORAGE_CLASS:-}"
ENDPOINT_DISK_SIZE="${ENDPOINT_DISK_SIZE:-10Gi}"
# Internal-LB annotations (used only for *-lb / LoadBalancer modes).
LB_ANNOTATIONS="${LB_ANNOTATIONS:-networking.gke.io/load-balancer-type=Internal,networking.gke.io/internal-load-balancer-allow-global-access=true}"
# Endpoint LB annotations default to LB_ANNOTATIONS only when endpoints use a LoadBalancer.
if [ "${ENDPOINT_SERVICE_TYPE}" = "LoadBalancer" ]; then
  ENDPOINT_SERVICE_ANNOTATIONS="${ENDPOINT_SERVICE_ANNOTATIONS:-${LB_ANNOTATIONS}}"
else
  ENDPOINT_SERVICE_ANNOTATIONS="${ENDPOINT_SERVICE_ANNOTATIONS:-}"
fi

say()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31merror: %s\033[0m\n' "$*" >&2; exit 1; }

# --- preflight ---
say "preflight"
for bin in kubectl helm docker aws; do
  command -v "$bin" >/dev/null 2>&1 || die "$bin not found on PATH"
done
CTX="$(kubectl config current-context 2>/dev/null || true)"
[ -n "$CTX" ] || die "no kubectl context set"
aws sts get-caller-identity >/dev/null 2>&1 || die "no usable AWS credentials (needed for ECR login)"
echo "   kube-context : ${CTX}"
echo "   namespace    : ${NS}"
echo "   registry     : ${ECR}  (region ${REGION})"
echo "   yugabyted    : ${YB_IMAGE}"

# --- 1. namespace ---
say "namespace ${NS}"
kubectl create namespace "${NS}" --dry-run=client -o yaml | kubectl apply -f -

# --- 2. ECR pull secret ---
# Log docker in to ECR (lets local `docker pull` work too) and mint/refresh a
# k8s docker-registry secret from the same token so the cluster can pull.
say "ECR pull secret '${PULL_SECRET}'"
ECR_TOKEN="$(aws ecr get-login-password --region "${REGION}")"
docker login --username AWS --password-stdin "${ECR}" <<<"${ECR_TOKEN}" >/dev/null
kubectl -n "${NS}" create secret docker-registry "${PULL_SECRET}" \
  --docker-server="${ECR}" \
  --docker-username=AWS \
  --docker-password="${ECR_TOKEN}" \
  --dry-run=client -o yaml | kubectl apply -f -

# --- 3. single-node YugabyteDB (yugabyted) ---
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

# --- 4. the yb_storage database ---
# The controller's db.Connect targets yb_storage at startup, so it must exist
# before the controller rolls out. yugabyted binds YSQL to its advertise address
# (the pod FQDN), so connect there rather than loopback.
say "database yb_storage"
kubectl -n "${NS}" exec -i yugabyted-0 -- bash -s <<'REMOTE'
set -euo pipefail
H="$(hostname -f)"
# Exactly "1" when the DB exists, empty when absent (-tA = tuples-only/unaligned).
# A genuine connection failure aborts here (set -e) rather than being misread as
# "absent" and triggering a spurious CREATE.
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

# --- 5. resolve image tags (most-recent in ECR unless pinned) ---
discover_tag() {
  aws ecr describe-images --repository-name "$1" --region "${REGION}" \
    --query 'reverse(sort_by(imageDetails[?imageTags],&imagePushedAt))[0].imageTags[0]' \
    --output text 2>/dev/null || true
}
if [ -z "${CONTROLLER_TAG}" ]; then
  CONTROLLER_TAG="$(discover_tag "${CONTROLLER_IMAGE_REPO}")"
  { [ -n "${CONTROLLER_TAG}" ] && [ "${CONTROLLER_TAG}" != "None" ]; } || CONTROLLER_TAG=latest
fi
if [ -z "${ENDPOINT_TAG}" ]; then
  ENDPOINT_TAG="$(discover_tag "${ENDPOINT_IMAGE_REPO}")"
  { [ -n "${ENDPOINT_TAG}" ] && [ "${ENDPOINT_TAG}" != "None" ]; } || ENDPOINT_TAG=latest
fi

# --- 6. the amp controller ---
say "amp controller (controller=${CONTROLLER_IMAGE_REPO}:${CONTROLLER_TAG}  endpoint=${ENDPOINT_IMAGE_REPO}:${ENDPOINT_TAG})"
NS="${NS}" \
PULL_SECRET="${PULL_SECRET}" \
ECR="${ECR}" \
CONTROLLER_IMAGE_REPO="${CONTROLLER_IMAGE_REPO}" \
ENDPOINT_IMAGE_REPO="${ENDPOINT_IMAGE_REPO}" \
CONTROLLER_TAG="${CONTROLLER_TAG}" \
ENDPOINT_TAG="${ENDPOINT_TAG}" \
YB_HOST_PORTS="${YB_FQDN}:5433" \
DB_URL="postgres://yugabyte@${YB_FQDN}:5433/yb_storage?sslmode=disable" \
CONTROLLER_EXPOSE="${CONTROLLER_EXPOSE}" \
ENDPOINT_SERVICE_TYPE="${ENDPOINT_SERVICE_TYPE}" \
ENDPOINT_SERVICE_ANNOTATIONS="${ENDPOINT_SERVICE_ANNOTATIONS}" \
ENDPOINT_STORAGE_CLASS="${ENDPOINT_STORAGE_CLASS}" \
ENDPOINT_DISK_SIZE="${ENDPOINT_DISK_SIZE}" \
LB_ANNOTATIONS="${LB_ANNOTATIONS}" \
  "${SCRIPT_DIR}/controller/redeploy.sh"

# --- done ---
say "stack is up in namespace '${NS}'"
echo
echo "  Controller UI / API  (UI at /, REST API under /api/v1):"
case "${CONTROLLER_EXPOSE}" in
  nodeport)
    NP="$(kubectl -n "${NS}" get svc amp-controller-np -o jsonpath='{.spec.ports[0].nodePort}' 2>/dev/null || true)"
    # Prefer an ExternalIP (matches the controller's own node-IP resolver), else InternalIP.
    NODEIP="$(kubectl get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null | awk '{print $1}' || true)"
    [ -n "${NODEIP}" ] || NODEIP="$(kubectl get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null | awk '{print $1}' || true)"
    if [ -n "${NODEIP}" ] && [ -n "${NP}" ]; then
      echo "    http://${NODEIP}:${NP}   (NodePort — reachable at any node IP)"
    else
      echo "    NodePort still settling — check: kubectl -n ${NS} get svc amp-controller-np"
    fi
    ;;
  internal-lb|external-lb)
    LBIP="$(kubectl -n "${NS}" get svc amp-controller-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [ -n "${LBIP}" ]; then
      echo "    http://${LBIP}:8080   (${CONTROLLER_EXPOSE})"
    else
      echo "    ${CONTROLLER_EXPOSE} IP still provisioning — watch: kubectl -n ${NS} get svc amp-controller-lb -w"
    fi
    ;;
  *)
    echo "    (no external Service — port-forward only)"
    ;;
esac
echo
echo "  From anywhere with kubectl access (works from outside the VPC too):"
echo "    kubectl -n ${NS} port-forward svc/amp-controller 8080:8080"
echo "    then open http://localhost:8080"
echo
echo "  Endpoints will be created as ${ENDPOINT_SERVICE_TYPE} Services (psql via Service DNS in-cluster,"
echo "  or: kubectl -n ${NS} port-forward svc/<endpoint> 55433:55433)."
