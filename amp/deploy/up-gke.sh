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
#   4. the backing YugabyteDB, per FOUNDATION:
#      yugabyted (default) a single-node StatefulSet + headless Service, or with
#                          TLS=1 the certificate trust root (deploy/tls/trust-root.sh)
#                          and the upstream `yugabyte` chart on cert-manager certificates
#      operator            an operator-managed universe, via foundation/
#   5. the yb_storage database (the controller connects to it at startup), plus the
#      amp_storage role in TLS mode
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
#   FOUNDATION=operator deploy/up-gke.sh                          # operator-managed universe instead of yugabyted
#   TLS=1 deploy/up-gke.sh                                        # TLS + certificate auth (see TLS below)
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
CONTROLLER_IMAGE_REPO="${CONTROLLER_IMAGE_REPO:-amp-controller}"
ENDPOINT_IMAGE_REPO="${ENDPOINT_IMAGE_REPO:-amp-endpoint}"
UI_IMAGE_REPO="${UI_IMAGE_REPO:-amp-controller-ui}"
MCP_IMAGE_REPO="${MCP_IMAGE_REPO:-amp-mcp}"
AGENT_IMAGE_REPO="${AGENT_IMAGE_REPO:-ycp-reconciler-agent}"  # reconcile-agent (YCP_ENABLED=1)

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

# How the backing YugabyteDB is brought up.
#   yugabyted (default) the single-node StatefulSet below. RF-1, no rolling
#                       upgrade, no backups — fine for a dev/CI stack.
#   operator            a real universe managed by the YugabyteDB Kubernetes
#                       Operator, via the foundation/ deployer. Sets
#                       YB_HOST_PORTS/DB_URL from its output, so the address
#                       comes from the one place that knows it rather than being
#                       duplicated here.
# The default is deliberately unchanged: the operator path carries a ~5 CPU
# yugaware pod and tens of minutes of install, and is exercised by its own
# on-demand workflow rather than by every deploy.
FOUNDATION="${FOUNDATION:-yugabyted}"
FOUNDATION_SPEC="${FOUNDATION_SPEC:-${REPO_ROOT}/../foundation/examples/amp.yaml}"

# yugabyted settings (FOUNDATION=yugabyted only).
YB_IMAGE="${YB_IMAGE:-yugabytedb/yugabyte:latest}"
YB_STORAGE_SIZE="${YB_STORAGE_SIZE:-20Gi}"
# Stable in-cluster address of the yugabyted pod (StatefulSet headless Service DNS).
YB_FQDN="yugabyted-0.yugabyted.${NS}.svc.cluster.local"

# TLS. Off by default (encryption costs roughly 5% CPU and many deployments sit
# on a private network). With TLS=1:
#   - deploy/tls/trust-root.sh stands up (or verifies) cert-manager and
#     trust-manager, the `amp-ca` ClusterIssuer and the `amp-ca-bundle` trust
#     Bundle, and labels this namespace so the CA lands here as a ConfigMap;
#     TLS_ISSUER_KIND / TLS_ISSUER_NAME / TLS_ISSUER_GROUP select an existing
#     issuer instead of the bootstrapped one (see that script for TLS_CA_FILE).
#   - YugabyteDB is the upstream `yugabyte` chart with cert-manager certificates
#     rather than the yugabyted StatefulSet: TLS on YSQL and on the tserver RPC
#     port, client certificates required on RPC, and certificate authentication
#     for the storage role (`hostssl ... cert`).
#   - The controller and every endpoint it deploys present certificates of their
#     own (CN = their YB role), issued by the same issuer and verified against the
#     same CA bundle, so no password travels.
#   - TLS_PERMISSIVE=1 (default) keeps plaintext accepted beside TLS
#     (allow_insecure_connections=true, a trailing `host all all all trust` HBA
#     rule): the transition shape controller/architecture/ac-endpoint-tls.md
#     describes, for a deployment whose endpoints have not all been re-rendered.
#     TLS_PERMISSIVE=0 refuses plaintext.
TLS="${TLS:-0}"
TLS_PERMISSIVE="${TLS_PERMISSIVE:-1}"
TLS_ISSUER_KIND="${TLS_ISSUER_KIND:-ClusterIssuer}"
TLS_ISSUER_NAME="${TLS_ISSUER_NAME:-}"
TLS_ISSUER_GROUP="${TLS_ISSUER_GROUP:-cert-manager.io}"
# The chart-deployed YugabyteDB (TLS=1 only).
YB_CHART_VERSION="${YB_CHART_VERSION:-2026.1.0}"
YB_CHART_IMAGE_TAG="${YB_CHART_IMAGE_TAG:-}"              # empty => the chart's appVersion
# standard-rwo is a GKE PD class; this is the GKE script, so it names one.
YB_STORAGE_CLASS="${YB_STORAGE_CLASS:-standard-rwo}"
YB_RELEASE="yugabyte"
# The storage role endpoints log in as, by certificate CN.
YB_STORAGE_ROLE="${YB_STORAGE_ROLE:-amp_storage}"
if [ "${TLS}" = "1" ]; then
  # The chart's per-pod certificates carry the pod FQDNs (oldNamingStyle), so
  # clients that verify the server name connect to the pod, not the Service.
  YB_FQDN="yb-tserver-0.yb-tservers.${NS}.svc.cluster.local"
fi

# Exposure (GKE). Defaults to an internal LoadBalancer (private VPC IP).
#   CONTROLLER_EXPOSE     internal-lb (default) | external-lb | nodeport | none
#   ENDPOINT_SERVICE_TYPE ClusterIP (default) | NodePort | LoadBalancer
CONTROLLER_EXPOSE="${CONTROLLER_EXPOSE:-internal-lb}"
ENDPOINT_SERVICE_TYPE="${ENDPOINT_SERVICE_TYPE:-ClusterIP}"
# Endpoint data volume: a storage class backs each endpoint's /data with a PVC
# (Persistent Disk); empty (default) => an ephemeral emptyDir.
ENDPOINT_STORAGE_CLASS="${ENDPOINT_STORAGE_CLASS:-}"
ENDPOINT_DISK_SIZE="${ENDPOINT_DISK_SIZE:-10Gi}"
# Flat /data size for every endpoint whatever its CPU, bypassing the 20 GiB-per-vCPU
# policy and the floor above. Empty (default) = CPU-proportional. A GKE node on the
# default 100GB boot disk has ~43.8Gi allocatable, so a 2-vCPU endpoint's 40Gi packs
# one endpoint per node; "10Gi" fits four.
ENDPOINT_DISK_SIZE_OVERRIDE="${ENDPOINT_DISK_SIZE_OVERRIDE:-}"
# GiB of endpoint memory per vCPU: fills in the memory when a create names only
# CPU, and is the floor every request is held to. 0 (default) = the product's own
# 8, deliberately: itest-dev schedules a 2-vCPU/16Gi endpoint, so this deploy
# exercises the shape real endpoints get rather than a test-only one. Lower it
# only if this cluster stops being able to hold that.
ENDPOINT_MEMORY_GIB_PER_VCPU="${ENDPOINT_MEMORY_GIB_PER_VCPU:-0}"
# GKE internal-LB annotations (used for internal-lb controller/UI Services).
LB_ANNOTATIONS="${LB_ANNOTATIONS:-networking.gke.io/load-balancer-type=Internal,networking.gke.io/internal-load-balancer-allow-global-access=true}"
MCP_ENABLED="${MCP_ENABLED:-true}"
UI_ENABLED="${UI_ENABLED:-true}"
# Deploy the OTel collector CRs (chart default true). Set false where the
# cluster's OpenTelemetry Operator is unavailable — its admission webhook has
# failurePolicy: Fail, so a broken operator rejects the whole install even
# though the smoke doesn't exercise metrics.
OTEL_ENABLED="${OTEL_ENABLED:-true}"
# Stamped as the environment_id label on every series the collectors scrape
# (ac-metrics.md). Defaults to the namespace so an ordinary deploy still renders
# the identity processor — a collector that never renders it never checks that its
# statements parse. A YCP-driven deploy overrides this with the real environment
# id below, which is what a customer install gets from the YCP install hint.
METRICS_ENV_ID="${METRICS_ENV_ID:-${NS}}"

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
case "${FOUNDATION}" in
  yugabyted) ;;
  operator)
    command -v go >/dev/null 2>&1 || die "FOUNDATION=operator runs the foundation CLI from source; go not found on PATH"
    [ -f "${FOUNDATION_SPEC}" ] || die "FOUNDATION_SPEC=${FOUNDATION_SPEC} not found"
    # Absolutise it: the deployer runs from a subshell cd'd into foundation/, so
    # a relative path would resolve there instead of here — passing preflight
    # and then failing with a confusing "not found".
    FOUNDATION_SPEC="$(cd "$(dirname "${FOUNDATION_SPEC}")" && pwd)/$(basename "${FOUNDATION_SPEC}")"
    ;;
  *) die "invalid FOUNDATION=${FOUNDATION} (want yugabyted|operator)" ;;
esac
case "${TLS}" in 0|1) ;; *) die "TLS must be 0 or 1, got '${TLS}'" ;; esac
case "${TLS_PERMISSIVE}" in 0|1) ;; *) die "TLS_PERMISSIVE must be 0 or 1, got '${TLS_PERMISSIVE}'" ;; esac
if [ "${TLS}" = "1" ] && [ "${FOUNDATION}" != "yugabyted" ]; then
  # deploy/tls/ replaces the yugabyted StatefulSet with the upstream chart; the
  # operator-managed universe carries its own certificate configuration.
  die "TLS=1 needs FOUNDATION=yugabyted, got '${FOUNDATION}'"
fi
if [ "${BUILD}" = "1" ] && [ -z "${TAG}" ]; then TAG="$(date +%Y%m%d-%H%M%S)"; fi
TAG="${TAG:-latest}"
echo "   kube-context : ${CTX}"
echo "   namespace    : ${NS}"
echo "   registry     : ${REGISTRY}   (host ${REG_HOST})"
echo "   tag          : ${TAG}   (build=${BUILD})"
echo "   expose       : ${CONTROLLER_EXPOSE}   endpoints=${ENDPOINT_SERVICE_TYPE}"
echo "   foundation   : ${FOUNDATION}"
if [ "${TLS}" = "1" ]; then
  echo "   tls          : on (permissive=${TLS_PERMISSIVE}, issuer=${TLS_ISSUER_KIND}/${TLS_ISSUER_NAME:-amp-ca}, chart yugabyte ${YB_CHART_VERSION})"
else
  echo "   tls          : off"
fi

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

# --- 4. the backing YugabyteDB ---
if [ "${FOUNDATION}" = "operator" ]; then
  # An operator-managed universe, stood up by the foundation/ deployer: CRDs,
  # the yugaware chart with the Kubernetes operator enabled, a YBUniverse CR,
  # the tserver Service and yb_storage. It takes the namespace this script
  # already created, so the amp pods stay co-located with the DB.
  say "YugabyteDB (operator-managed universe via foundation/)"
  FOUNDATION_ARGS=(-c "${FOUNDATION_SPEC}" --namespace "${NS}")
  [ -n "${CLUSTER}" ] && FOUNDATION_ARGS+=(--kube-context "${CLUSTER}")
  ( cd "${REPO_ROOT}/../foundation" && go run ./cmd/foundation up "${FOUNDATION_ARGS[@]}" )

  # Read the addresses back from the deployer rather than duplicating them: it
  # owns the Service name, the port and the DSN shape, and a second copy here is
  # a second thing to keep in step. Captured before eval so a failure is not
  # swallowed by the command substitution.
  say "resolving foundation connection details"
  _foundation_conn="$(cd "${REPO_ROOT}/../foundation" \
    && go run ./cmd/foundation connstr "${FOUNDATION_ARGS[@]}" --shell)" \
    || die "foundation connstr failed; the universe may not be Ready"
  eval "${_foundation_conn}"
  [ -n "${YB_HOST_PORTS:-}" ] || die "foundation connstr produced no YB_HOST_PORTS"
  [ -n "${DB_URL:-}" ] || die "foundation connstr produced no DB_URL"
  echo "   YB_HOST_PORTS : ${YB_HOST_PORTS}"
  echo "   DB_URL        : ${DB_URL}"
elif [ "${TLS}" = "1" ]; then
  # TLS: the trust root, then the upstream chart with cert-manager certificates,
  # then yb_storage and the storage role — all in deploy/tls/yugabyte-tls.sh, so
  # up.sh deploys the same database. It does section 5's work too.
  NS="${NS}" YB_CHART_VERSION="${YB_CHART_VERSION}" YB_CHART_IMAGE_TAG="${YB_CHART_IMAGE_TAG}" \
  YB_STORAGE_SIZE="${YB_STORAGE_SIZE}" YB_STORAGE_CLASS="${YB_STORAGE_CLASS}" \
  YB_STORAGE_ROLE="${YB_STORAGE_ROLE}" YB_RELEASE="${YB_RELEASE}" TLS_PERMISSIVE="${TLS_PERMISSIVE}" \
  TLS_ISSUER_KIND="${TLS_ISSUER_KIND}" TLS_ISSUER_NAME="${TLS_ISSUER_NAME}" TLS_ISSUER_GROUP="${TLS_ISSUER_GROUP}" \
    "${SCRIPT_DIR}/tls/yugabyte-tls.sh"
  # The chart mounts the controller's certificate and the CA bundle at these
  # paths; the endpoints get the storage role and a certificate each.
  YB_HOST_PORTS="${YB_FQDN}:5433"
  DB_URL="postgres://yugabyte@${YB_FQDN}:5433/yb_storage?sslmode=verify-full&sslrootcert=/etc/amp/tls/ca/ca.crt&sslcert=/etc/amp/tls/client/tls.crt&sslkey=/etc/amp/tls/client/tls.key"
else
# The yugabyted body below is deliberately left unindented: its manifest is a
# heredoc whose YAML terminator must sit at column 0, so indenting the branch
# would mean converting it to <<- and re-tabbing ~80 lines of YAML. Not worth
# obscuring the diff for.
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

# The yugabyted pod's own FQDN is the only address there is here.
YB_HOST_PORTS="${YB_FQDN}:5433"
DB_URL="postgres://yugabyte@${YB_FQDN}:5433/yb_storage?sslmode=disable"
fi

# --- 6. the amp controller (+ MCP + UI) via Helm ---
say "amp controller (Helm: controller=${REGISTRY}/${CONTROLLER_IMAGE_REPO}:${TAG})"
if [ "${TLS}" = "1" ]; then
  ENDPOINT_YB_USER="${YB_STORAGE_ROLE}"
else
  ENDPOINT_YB_USER="yugabyte"
fi

HELM_SET=(
  "controller.endpoint.ybUser=${ENDPOINT_YB_USER}"
  "controller.image.repository=${REGISTRY}/${CONTROLLER_IMAGE_REPO}"
  "controller.image.tag=${TAG}"
  "controller.endpoint.image.repository=${REGISTRY}/${ENDPOINT_IMAGE_REPO}"
  "controller.endpoint.image.tag=${TAG}"
  "controller.endpoint.ybHostPorts=${YB_HOST_PORTS}"
  "controller.endpoint.service.type=${ENDPOINT_SERVICE_TYPE}"
  "controller.endpoint.storageClass=${ENDPOINT_STORAGE_CLASS}"
  "controller.endpoint.diskSize=${ENDPOINT_DISK_SIZE}"
  "controller.endpoint.diskSizeOverride=${ENDPOINT_DISK_SIZE_OVERRIDE}"
  "controller.endpoint.memoryGiBPerVCPU=${ENDPOINT_MEMORY_GIB_PER_VCPU}"
  "controller.service.type=${CONTROLLER_EXPOSE}"
  # cert-manager is owned outside this chart: deploy/tls/trust-root.sh installs
  # it in TLS mode, and a plaintext deploy needs none. Leaving the chart default
  # on would make the release a second owner and collide with an existing install.
  "mcp.enabled=${MCP_ENABLED}"
  "mcp.image.repository=${REGISTRY}/${MCP_IMAGE_REPO}"
  "mcp.image.tag=${TAG}"
  "ui.enabled=${UI_ENABLED}"
  "ui.image.repository=${REGISTRY}/${UI_IMAGE_REPO}"
  "ui.image.tag=${TAG}"
  "otel.enabled=${OTEL_ENABLED}"
  "otel.environmentId=${METRICS_ENV_ID}"
)
[ -n "${PULL_SECRET}" ] && HELM_SET+=("imagePullSecrets[0].name=${PULL_SECRET}")
if [ "${TLS}" = "1" ]; then
  HELM_SET+=(
    "tls.enabled=true"
    "tls.issuerRef.kind=${TLS_ISSUER_KIND}"
    "tls.issuerRef.name=${TLS_ISSUER_NAME:-amp-ca}"
    "tls.issuerRef.group=${TLS_ISSUER_GROUP}"
    "tls.caBundleConfigMap=amp-ca-bundle"
    "controller.endpoint.ybTlsMode=verify-full"
  )
fi

# YCP reconciliation (off by default). When YCP_ENABLED=1, deploy the
# reconcile-agent: it enrolls with the control plane at YCP_URL as environment
# YCP_ENV_ID (authenticating every call from the Secret YCP_AGENT_SECRET), then
# converges the environment to YCP's desired state over the controller's REST API
# and reports its inventory back. amp/deploy/ycp-up.sh creates the control plane,
# the environment, and the secret. The amp_deploy_gke_ycp smoke forces a fast
# reconcile interval so the create + inventory assertions resolve quickly.
#
# The agent is the SOLE YCP client for the environment and is authoritative: it
# garbage-collects anything not in desired state, so the environment is driven
# entirely through YCP, never the amp API. See amp/deploy/ycp-reconcile-smoke.sh.
YCP_ENABLED="${YCP_ENABLED:-0}"
if [ "${YCP_ENABLED}" = "1" ]; then
  : "${YCP_URL:?YCP_ENABLED=1 requires YCP_URL}"
  : "${YCP_ENV_ID:?YCP_ENABLED=1 requires YCP_ENV_ID}"
  YCP_RECONCILE_INTERVAL="${YCP_RECONCILE_INTERVAL:-${YCP_REPORT_INTERVAL:-5s}}"
  YCP_AGENT_SECRET="${YCP_AGENT_SECRET:-amp-agent-api-key}"
  YCP_PLACEMENT="${YCP_PLACEMENT:-cloud}"
  # A YCP-managed install is expected to ship telemetry to yugabyte-cloud. Neither
  # the chart nor the control plane enforces that today — both warn — so this path
  # deploys the real thing and ycp-reconcile-smoke.sh ASSERTS it, which is what keeps
  # a broken BYOC telemetry path from passing as green.
  #
  # BYOC_METRICS_URL must be the SAME value the control plane was started with
  # (--byoc-metrics-url); ycp-up.sh takes it from this same variable.
  YCP_BYOC_METRICS_URL="${YCP_BYOC_METRICS_URL:-https://byoc.devcloud.yugabyte.com/api/v1/otlp/metrics}"
  : "${YCP_BYOC_PASSWORD:?YCP_ENABLED=1 requires YCP_BYOC_PASSWORD (the BYOC metrics door password)}"
  : "${YCP_DATADOG_API_KEY:?YCP_ENABLED=1 requires YCP_DATADOG_API_KEY (the logs fan-out key)}"
  # Both are read by secretKeyRef, so they must exist before the install.
  kubectl -n "${NS}" create secret generic byoc-metrics-auth \
    --from-literal=password="${YCP_BYOC_PASSWORD}" \
    --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n "${NS}" create secret generic datadog-api \
    --from-literal=api-key="${YCP_DATADOG_API_KEY}" \
    --dry-run=client -o yaml | kubectl apply -f -
  say "YCP reconcile-agent enabled (url=${YCP_URL} env=${YCP_ENV_ID} interval=${YCP_RECONCILE_INTERVAL} image=${REGISTRY}/${AGENT_IMAGE_REPO}:${TAG})"
  HELM_SET+=(
    "agent.enabled=true"
    "agent.image.repository=${REGISTRY}/${AGENT_IMAGE_REPO}"
    "agent.image.tag=${TAG}"
    "agent.ycpUrl=${YCP_URL}"
    "agent.envName=${YCP_ENV_ID}"
    # Override the namespace default: this deploy has a real environment id, so its
    # series are attributable the way a hint-installed one's are. No account — the
    # itest cluster is not registered against a YBM account.
    "otel.environmentId=${YCP_ENV_ID}"
    "agent.placement=${YCP_PLACEMENT}"
    "agent.reconcileInterval=${YCP_RECONCILE_INTERVAL}"
    "agent.apiKeySecret.name=${YCP_AGENT_SECRET}"
    "otel.enabled=true"
    "otel.byoc.enabled=true"
    "otel.byoc.metricsEndpoint=${YCP_BYOC_METRICS_URL}"
  )
fi

# The chart declares cert-manager as a subchart, and helm refuses to render a
# chart whose declared dependency is not in charts/ even when its condition is
# false. Chart.lock pins the version; this fetches it.

# Commas escaped: helm's --set-string splits on them, so a multi-host DSN would
# arrive as separate fragments and fail the install.
# --timeout above the preflight hook's own budget (300s + pull/schedule). helm waits
# on a post-install hook for --timeout, which defaults to exactly 5m, so without this
# helm abandons the release first and reports a timeout in place of the probe's own
# verdict — and the pod's log, which is the entire diagnosis, has to be dug out by
# hand. The install hint prints the same 10m for the same reason.
HELM_ARGS=(upgrade --install yb-amp-controller "${CHART_DIR}" -n "${NS}" --timeout 10m
           --set-string "controller.databaseUrl=${DB_URL//,/\\,}")
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
