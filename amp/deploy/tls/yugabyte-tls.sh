#!/usr/bin/env bash
#
# A single-node YugabyteDB from the upstream `yugabyte` chart with cert-manager
# certificates, for a TLS-mode amp deployment: TLS on YSQL and on the tserver
# RPC port, client certificates demanded on RPC, and certificate authentication
# for the storage role. Runs deploy/tls/trust-root.sh first, then creates the
# yb_storage database and the storage role. up.sh / up-gke.sh call it with
# TLS=1; see controller/architecture/ac-endpoint-tls.md.
#
# Env:
#   NS                 namespace (required)
#   YB_CHART_VERSION   `yugabyte` chart version (default: YB_CHART_VERSION in amp/versions.txt)
#   YB_CHART_IMAGE_TAG image tag override (default: the chart's appVersion)
#   YB_STORAGE_SIZE    per-pod volume size (20Gi)
#   YB_STORAGE_CLASS   storage class; empty (default) takes the cluster's own
#   YB_STORAGE_ROLE    role endpoints log in as, by certificate CN (amp_storage)
#   TLS_PERMISSIVE     1 (default) keeps plaintext accepted beside TLS:
#                      allow_insecure_connections=true plus a trailing
#                      `host all all all trust` HBA rule; 0 refuses plaintext
#   TLS_ISSUER_KIND / TLS_ISSUER_NAME / TLS_ISSUER_GROUP
#                      passed through to trust-root.sh; empty name = bootstrap amp-ca
#   YB_RELEASE         helm release name (yugabyte). With the chart's default
#                      naming the pods are yb-master-0 / yb-tserver-0 and the
#                      tserver pod FQDN is yb-tserver-0.yb-tservers.<NS>.svc.cluster.local
#   YB_CERT_ROLLOUT    1 (default) installs the yb-cert-rollout CronJob, which
#                      restarts a StatefulSet whose certificate was renewed after
#                      its pods started (the chart copies certificates at pod
#                      start only); 0 skips it
#   YB_CERT_ROLLOUT_SCHEDULE  its cron schedule (17 * * * *)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${NS:?NS is required}"
# shellcheck source=../../scripts/versions.sh
. "${SCRIPT_DIR}/../../scripts/versions.sh"
YB_CHART_VERSION="${YB_CHART_VERSION:-$(amp_version YB_CHART_VERSION)}"
YB_CHART_IMAGE_TAG="${YB_CHART_IMAGE_TAG:-}"
YB_STORAGE_SIZE="${YB_STORAGE_SIZE:-20Gi}"
YB_STORAGE_CLASS="${YB_STORAGE_CLASS:-}"
YB_STORAGE_ROLE="${YB_STORAGE_ROLE:-amp_storage}"
TLS_PERMISSIVE="${TLS_PERMISSIVE:-1}"
TLS_ISSUER_KIND="${TLS_ISSUER_KIND:-ClusterIssuer}"
TLS_ISSUER_NAME="${TLS_ISSUER_NAME:-}"
TLS_ISSUER_GROUP="${TLS_ISSUER_GROUP:-cert-manager.io}"
YB_RELEASE="${YB_RELEASE:-yugabyte}"
YB_FQDN="yb-tserver-0.yb-tservers.${NS}.svc.cluster.local"
YB_CERT_ROLLOUT="${YB_CERT_ROLLOUT:-1}"
YB_CERT_ROLLOUT_IMAGE="${YB_CERT_ROLLOUT_IMAGE:-alpine/k8s:1.33.4}"
YB_CERT_ROLLOUT_SCHEDULE="${YB_CERT_ROLLOUT_SCHEDULE:-17 * * * *}"

say() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
die() {
  printf '\033[1;31merror: %s\033[0m\n' "$*" >&2
  exit 1
}

say "trust root (cert-manager, trust-manager, ${TLS_ISSUER_KIND}/${TLS_ISSUER_NAME:-amp-ca})"
TLS_ISSUER_KIND="${TLS_ISSUER_KIND}" TLS_ISSUER_NAME="${TLS_ISSUER_NAME}" \
  TLS_ISSUER_GROUP="${TLS_ISSUER_GROUP}" TLS_TRUST_NAMESPACES="${NS}" \
  "${SCRIPT_DIR}/trust-root.sh"
TLS_ISSUER_NAME="${TLS_ISSUER_NAME:-amp-ca}"

say "YugabyteDB (yugabyte chart ${YB_CHART_VERSION}, certificates from ${TLS_ISSUER_KIND}/${TLS_ISSUER_NAME})"
helm repo add yugabytedb https://charts.yugabyte.com >/dev/null 2>&1 || true
helm repo update yugabytedb >/dev/null
CA_BUNDLE_B64="$(kubectl -n "${NS}" get configmap amp-ca-bundle -o jsonpath='{.data.ca\.crt}' | base64 -w0)"
[ -n "${CA_BUNDLE_B64}" ] || die "ConfigMap amp-ca-bundle in ${NS} has no ca.crt"

# The storage role logs in by certificate; so may `yugabyte`, which is what
# lets the controller move to a certificate later without another HBA change.
# Everything else needs TLS and a password. With TLS_PERMISSIVE the trailing
# plaintext rule is what a not-yet-migrated controller and endpoints use.
HBA="hostssl yb_storage ${YB_STORAGE_ROLE} all cert,hostssl all yugabyte all cert,hostssl all all all scram-sha-256"
TLS_INSECURE=false
if [ "${TLS_PERMISSIVE}" = "1" ]; then
  HBA="${HBA},host all all all trust"
  TLS_INSECURE=true
fi
case "${TLS_ISSUER_KIND}" in
ClusterIssuer) YB_ISSUER_YAML="    useClusterIssuer: true
    clusterIssuer: ${TLS_ISSUER_NAME}" ;;
Issuer) YB_ISSUER_YAML="    useClusterIssuer: false
    issuer: ${TLS_ISSUER_NAME}" ;;
*) YB_ISSUER_YAML="    useCustomIssuer: true
    customIssuer:
      kind: ${TLS_ISSUER_KIND}
      name: ${TLS_ISSUER_NAME}
      group: ${TLS_ISSUER_GROUP}" ;;
esac
# Indented to sit under storage.master / storage.tserver, or empty so the
# chart falls back to the cluster's default class.
STORAGE_CLASS_YAML=""
if [ -n "${YB_STORAGE_CLASS}" ]; then
  STORAGE_CLASS_YAML="    storageClass: ${YB_STORAGE_CLASS}"
fi
YB_VALUES_FILE="$(mktemp)"
trap 'rm -f "${YB_VALUES_FILE}"' EXIT
cat >"${YB_VALUES_FILE}" <<YAML
replicas:
  master: 1
  tserver: 1
enableLoadBalancer: false
storage:
  master:
    count: 1
    size: ${YB_STORAGE_SIZE}
${STORAGE_CLASS_YAML}
  tserver:
    count: 1
    size: ${YB_STORAGE_SIZE}
${STORAGE_CLASS_YAML}
resource:
  master:
    requests: { cpu: 500m, memory: 1Gi }
    limits: { cpu: "1", memory: 2Gi }
  tserver:
    requests: { cpu: 500m, memory: 2Gi }
    limits: { cpu: "2", memory: 4Gi }
gflags:
  master:
    replication_factor: 1
    default_memory_limit_to_ram_ratio: 0.85
    node_to_node_encryption_use_client_certificates: "true"
  tserver:
    node_to_node_encryption_use_client_certificates: "true"
    ysql_enable_auth: "true"
    ysql_hba_conf_csv: "${HBA}"
tls:
  enabled: true
  nodeToNode: true
  clientToServer: true
  insecure: ${TLS_INSECURE}
  certManager:
    enabled: true
    bootstrapSelfsigned: false
${YB_ISSUER_YAML}
  rootCA:
    cert: "${CA_BUNDLE_B64}"
    key: ""
YAML
YB_HELM_ARGS=(upgrade --install "${YB_RELEASE}" yugabytedb/yugabyte --version "${YB_CHART_VERSION}"
  -n "${NS}" -f "${YB_VALUES_FILE}" --wait --timeout 20m)
[ -n "${YB_CHART_IMAGE_TAG}" ] && YB_HELM_ARGS+=(--set "Image.tag=${YB_CHART_IMAGE_TAG}")
echo ">> helm ${YB_HELM_ARGS[*]}"
helm "${YB_HELM_ARGS[@]}"
# helm --wait returns while a gflag change is still rolling the pods; the role
# step below needs a tserver that answers.
kubectl -n "${NS}" rollout status statefulset/yb-master --timeout=20m
kubectl -n "${NS}" rollout status statefulset/yb-tserver --timeout=20m
# Both halves are needed: the rollout wait proves the pods are Ready, this
# proves the SQL port is listening, which lags pod-Ready. Without it the
# CREATE DATABASE below hits `connection refused` under `set -e` and takes the
# whole deploy with it.
say "waiting for YSQL"
# By certificate, like every other admin query here: the HBA this script installs
# matches an SSL connection on `hostssl ... cert` and stops there, so a plaintext
# or cert-less probe is refused even in permissive mode.
ysql_ok() {
  kubectl -n "${NS}" exec -i yb-tserver-0 -- bash -s -- "${YB_FQDN}" <<'REMOTE' >/dev/null 2>&1
set -euo pipefail
H="$1"
YSQLSH="$(command -v ysqlsh || echo /home/yugabyte/tserver/bin/ysqlsh)"
CONN="host=$H port=5433 user=yugabyte dbname=yugabyte sslmode=verify-full sslrootcert=/root/.yugabytedb/root.crt sslcert=/root/.yugabytedb/yugabytedb.crt sslkey=/root/.yugabytedb/yugabytedb.key"
"$YSQLSH" "$CONN" -tAc "select 1"
REMOTE
}
ysql_ready=0
for _ in $(seq 1 60); do
  if ysql_ok; then
    ysql_ready=1
    break
  fi
  sleep 5
done
[ "${ysql_ready}" = "1" ] || die "YSQL never came up on yb-tserver-0 after the rollout"

if [ "${YB_CERT_ROLLOUT}" = "1" ]; then
  say "certificate rollout CronJob (${YB_CERT_ROLLOUT_SCHEDULE})"
  # A renewed Certificate's notBefore is later than the pods that still carry
  # the old files, which is the whole test. Masters before tservers, each
  # rollout waited out, so a renewal of both never restarts them together.
  kubectl -n "${NS}" apply -f - <<YAML
apiVersion: v1
kind: ServiceAccount
metadata:
  name: yb-cert-rollout
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: yb-cert-rollout
rules:
  - apiGroups: ["cert-manager.io"]
    resources: ["certificates"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["apps"]
    resources: ["statefulsets"]
    verbs: ["get", "list", "watch", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: yb-cert-rollout
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: yb-cert-rollout
subjects:
  - kind: ServiceAccount
    name: yb-cert-rollout
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: yb-cert-rollout
spec:
  schedule: "${YB_CERT_ROLLOUT_SCHEDULE}"
  concurrencyPolicy: Forbid
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 3
  jobTemplate:
    spec:
      backoffLimit: 0
      # Past the sum of the two --timeout=30m rollout waits below, plus slack.
      # Short of that the Job is killed mid-roll and, with backoffLimit 0, the
      # tservers keep the old certificate until the next schedule.
      activeDeadlineSeconds: 4200
      template:
        spec:
          serviceAccountName: yb-cert-rollout
          restartPolicy: Never
          containers:
            - name: rollout
              # kubectl plus a shell. Timestamps are compared as the RFC 3339
              # UTC strings the API returns, so no GNU date is needed.
              #
              # The default is a Docker Hub image, and an unauthenticated Hub
              # pull is rate-limited — on the very renewal this Job exists to
              # handle. There is no canonical mirror of a kubectl image in
              # quay.io/yugabyte today; until there is, a deployment that
              # cares should point YB_CERT_ROLLOUT_IMAGE at its own.
              image: ${YB_CERT_ROLLOUT_IMAGE}
              env:
                - { name: RELEASE, value: "${YB_RELEASE}" }
              command: ["bash", "-euo", "pipefail", "-c"]
              args:
                - |
                  newest_not_before() {
                    local newest="" nb
                    for cert in "\$@"; do
                      nb=\$(kubectl get certificate "\$cert" -o jsonpath='{.status.notBefore}' 2>/dev/null || true)
                      [ -n "\$nb" ] || continue
                      [[ "\$nb" > "\$newest" ]] && newest="\$nb"
                    done
                    echo "\$newest"
                  }
                  roll_if_stale() {
                    local sts="\$1"; shift
                    local nb oldest
                    nb=\$(newest_not_before "\$@")
                    oldest=\$(kubectl get pods -l "app=\$sts" -o jsonpath='{range .items[*]}{.status.startTime}{"\n"}{end}' | sort | head -1)
                    if [ -z "\$nb" ] || [ -z "\$oldest" ]; then
                      echo "\$sts: no certificate or no pods; nothing to do"; return
                    fi
                    if [[ ! "\$nb" > "\$oldest" ]]; then
                      echo "\$sts: pods (started \$oldest) already carry the current certificate (\$nb)"; return
                    fi
                    echo "\$sts: certificate renewed (\$nb) after the pods started (\$oldest); rolling restart"
                    kubectl rollout restart statefulset "\$sts"
                    kubectl rollout status statefulset "\$sts" --timeout=30m
                  }
                  roll_if_stale yb-master "yb-master-\${RELEASE}-tls-cert"
                  roll_if_stale yb-tserver "yb-tserver-\${RELEASE}-tls-cert" "\${RELEASE}-tls-client-cert"
YAML
fi

# The chart mounts a CN=yugabyte client certificate into the tserver pod, so
# admin work logs in by certificate.
say "database yb_storage"
kubectl -n "${NS}" exec -i yb-tserver-0 -- bash -s -- "${YB_FQDN}" <<'REMOTE'
set -euo pipefail
H="$1"
# The chart image keeps the binaries under /home/yugabyte/tserver/bin, on PATH.
YSQLSH="$(command -v ysqlsh || echo /home/yugabyte/tserver/bin/ysqlsh)"
CONN="host=$H port=5433 user=yugabyte dbname=yugabyte sslmode=verify-full sslrootcert=/root/.yugabytedb/root.crt sslcert=/root/.yugabytedb/yugabytedb.crt sslkey=/root/.yugabytedb/yugabytedb.key"
exists="$("$YSQLSH" "$CONN" -tAc "SELECT 1 FROM pg_database WHERE datname='yb_storage'")"
if [ "$exists" = "1" ]; then
  echo "   yb_storage already exists"
elif out="$("$YSQLSH" "$CONN" -c "CREATE DATABASE yb_storage" 2>&1)"; then
  echo "   created yb_storage"
elif printf '%s' "$out" | grep -q "already exists"; then
  echo "   yb_storage already exists"
else
  printf '   CREATE DATABASE yb_storage failed: %s\n' "$out" >&2
  exit 1
fi
REMOTE

# The role, from the script the operator lane uses too — that lane gets its
# database from the foundation spec and would otherwise have no role at all.
_ns="${NS}"
UNIVERSE_NS="${_ns}" NS="${_ns}" YB_HOST_PORTS="${YB_FQDN}:5433" \
  YB_STORAGE_ROLE="${YB_STORAGE_ROLE}" YB_DATABASE="yb_storage" YB_EXEC_POD="yb-tserver-0" \
  "$(dirname "${BASH_SOURCE[0]}")/yb-storage-role.sh"

echo
if [ "${TLS_PERMISSIVE}" = "1" ]; then
  echo "YugabyteDB: ${YB_FQDN}:5433 (YSQL, TLS), tserver RPC on 9100 (TLS) — permissive:" \
    "plaintext is still accepted and client certificates are not required"
else
  echo "YugabyteDB: ${YB_FQDN}:5433 (YSQL, TLS), tserver RPC on 9100 (TLS, client certs required)"
fi
echo "CA bundle:  ConfigMap amp-ca-bundle key ca.crt in ${NS}"
echo "HBA:        ${HBA}"
