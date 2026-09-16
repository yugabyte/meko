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
#   YB_CHART_VERSION   `yugabyte` chart version (2026.1.0)
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
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${NS:?NS is required}"
YB_CHART_VERSION="${YB_CHART_VERSION:-2026.1.0}"
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

say() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
die() { printf '\033[1;31merror: %s\033[0m\n' "$*" >&2; exit 1; }

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
  Issuer)        YB_ISSUER_YAML="    useClusterIssuer: false
    issuer: ${TLS_ISSUER_NAME}" ;;
  *)             YB_ISSUER_YAML="    useCustomIssuer: true
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

# The chart mounts a CN=yugabyte client certificate into the tserver pod, so
# admin work logs in by certificate. The storage role is a member of yugabyte
# until the controller provisions explicit grants at startup.
say "database yb_storage and role ${YB_STORAGE_ROLE}"
kubectl -n "${NS}" exec -i yb-tserver-0 -- bash -s -- "${YB_FQDN}" "${YB_STORAGE_ROLE}" <<'REMOTE'
set -euo pipefail
H="$1"; ROLE="$2"
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
has_role="$("$YSQLSH" "$CONN" -tAc "SELECT 1 FROM pg_roles WHERE rolname='$ROLE'")"
if [ "$has_role" = "1" ]; then
  echo "   role $ROLE already exists"
else
  "$YSQLSH" "$CONN" -c "CREATE ROLE \"$ROLE\" LOGIN" >/dev/null
  "$YSQLSH" "$CONN" -c "GRANT yugabyte TO \"$ROLE\"" >/dev/null
  echo "   created role $ROLE (member of yugabyte)"
fi
REMOTE

echo
if [ "${TLS_PERMISSIVE}" = "1" ]; then
  echo "YugabyteDB: ${YB_FQDN}:5433 (YSQL, TLS), tserver RPC on 9100 (TLS) — permissive:" \
       "plaintext is still accepted and client certificates are not required"
else
  echo "YugabyteDB: ${YB_FQDN}:5433 (YSQL, TLS), tserver RPC on 9100 (TLS, client certs required)"
fi
echo "CA bundle:  ConfigMap amp-ca-bundle key ca.crt in ${NS}"
echo "HBA:        ${HBA}"
