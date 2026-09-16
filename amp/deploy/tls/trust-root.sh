#!/usr/bin/env bash
#
# The certificate trust root every TLS-enabled amp deployment shares:
# cert-manager, trust-manager, the `amp-ca` ClusterIssuer, and the
# `amp-ca-bundle` Bundle that lands the CA as ConfigMap `amp-ca-bundle`
# (key `ca.crt`) in every namespace labelled amp.yugabyte.com/trust=true.
#
# Idempotent. up.sh / up-gke.sh run it when TLS=1; it is also safe by hand.
# See controller/architecture/ac-endpoint-tls.md and runbooks/tls.md.
#
# Env (all optional):
#   CERT_MANAGER_NS         namespace cert-manager and trust-manager run in (cert-manager).
#                           It is also cert-manager's cluster-resource namespace, so the
#                           bootstrapped CA Secret and the trust sources live here.
#   INSTALL_CERT_MANAGER    auto (default: install only when its CRDs are absent) | 1 | 0
#   INSTALL_TRUST_MANAGER   auto | 1 | 0
#   CERT_MANAGER_VERSION    chart version for an install (default: the chart's latest)
#   TRUST_MANAGER_VERSION   chart version for an install (default: the chart's latest)
#   TLS_ISSUER_NAME         bring-your-own issuer: skip the bootstrap and use this one
#   TLS_ISSUER_KIND         ClusterIssuer (default) | Issuer | a custom kind
#   TLS_ISSUER_GROUP        cert-manager.io (default)
#   TLS_CA_FILE             with a BYO issuer, the PEM CA chain clients should trust;
#                           without it the issuer's ca.secretName is read from CERT_MANAGER_NS
#   TLS_ROOT_CA_DURATION    bootstrapped root lifetime (87600h)
#   TLS_TRUST_NAMESPACES    space-separated namespaces to label for the Bundle
#
# Re-running never removes a root from the trust bundle: a root that is not
# in `amp-ca-roots` yet is appended, which is step 1 of a CA rotation.
set -euo pipefail

CERT_MANAGER_NS="${CERT_MANAGER_NS:-cert-manager}"
INSTALL_CERT_MANAGER="${INSTALL_CERT_MANAGER:-auto}"
INSTALL_TRUST_MANAGER="${INSTALL_TRUST_MANAGER:-auto}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-}"
TRUST_MANAGER_VERSION="${TRUST_MANAGER_VERSION:-}"
TLS_ISSUER_NAME="${TLS_ISSUER_NAME:-}"
TLS_ISSUER_KIND="${TLS_ISSUER_KIND:-ClusterIssuer}"
TLS_ISSUER_GROUP="${TLS_ISSUER_GROUP:-cert-manager.io}"
TLS_CA_FILE="${TLS_CA_FILE:-}"
TLS_ROOT_CA_DURATION="${TLS_ROOT_CA_DURATION:-87600h}"
TLS_TRUST_NAMESPACES="${TLS_TRUST_NAMESPACES:-}"

BOOTSTRAP_ISSUER="amp-ca-bootstrap"
ROOT_CA_NAME="amp-root-ca"
ROOTS_CONFIGMAP="amp-ca-roots"
BUNDLE_NAME="amp-ca-bundle"
TRUST_LABEL="amp.yugabyte.com/trust"

say() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
die() { printf '\033[1;31merror: %s\033[0m\n' "$*" >&2; exit 1; }

for bin in kubectl helm; do
  command -v "$bin" >/dev/null 2>&1 || die "$bin not found on PATH"
done

crd_present() { kubectl get crd "$1" >/dev/null 2>&1; }

want_install() {
  # $1 = mode, $2 = crd
  case "$1" in
    1) return 0 ;;
    0) crd_present "$2" || die "$2 is absent and installation is disabled"; return 1 ;;
    auto) ! crd_present "$2" ;;
    *) die "invalid install mode '$1' (want auto|1|0)" ;;
  esac
}

# --- cert-manager -----------------------------------------------------------
say "cert-manager (namespace ${CERT_MANAGER_NS})"
if want_install "${INSTALL_CERT_MANAGER}" certificates.cert-manager.io; then
  echo "   installing cert-manager${CERT_MANAGER_VERSION:+ ${CERT_MANAGER_VERSION}}"
  helm upgrade --install cert-manager oci://quay.io/jetstack/charts/cert-manager \
    -n "${CERT_MANAGER_NS}" --create-namespace --wait \
    ${CERT_MANAGER_VERSION:+--version "${CERT_MANAGER_VERSION}"} \
    --set crds.enabled=true \
    --set enableCertificateOwnerRef=true
else
  echo "   present (CRDs found); not touching it"
  # Without this flag a deleted Certificate leaves its Secret behind, and
  # nothing else cleans it up: DeleteEndpoint is a bare helm uninstall and no
  # controller code touches cert-manager objects. A warning rather than a
  # failure because an existing cert-manager is not this script's to reconfigure
  # — but the leak is real, so say so.
  if ! kubectl -n "${CERT_MANAGER_NS}" get deploy cert-manager \
      -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null \
      | grep -q 'enable-certificate-owner-ref'; then
    echo "   note: cert-manager runs without --enable-certificate-owner-ref;" \
         "certificate Secrets outlive their Certificates unless the owner deletes them"
  fi
fi
kubectl -n "${CERT_MANAGER_NS}" rollout status deploy/cert-manager-webhook --timeout=180s >/dev/null

# --- trust-manager ----------------------------------------------------------
say "trust-manager (namespace ${CERT_MANAGER_NS})"
if want_install "${INSTALL_TRUST_MANAGER}" bundles.trust.cert-manager.io; then
  echo "   installing trust-manager${TRUST_MANAGER_VERSION:+ ${TRUST_MANAGER_VERSION}}"
  # helmCert: the webhook's serving certificate is rendered by the chart rather
  # than issued by cert-manager and injected by cainjector, which a locked-down
  # cert-manager install may not be allowed to do (cainjector then lacks RBAC on
  # validatingwebhookconfigurations and the webhook is never trusted).
  helm upgrade --install trust-manager oci://quay.io/jetstack/charts/trust-manager \
    -n "${CERT_MANAGER_NS}" --wait \
    ${TRUST_MANAGER_VERSION:+--version "${TRUST_MANAGER_VERSION}"} \
    --set app.trust.namespace="${CERT_MANAGER_NS}" \
    --set app.webhook.tls.helmCert.enabled=true
else
  echo "   present (CRDs found); not touching it"
fi
kubectl -n "${CERT_MANAGER_NS}" rollout status deploy/trust-manager --timeout=180s >/dev/null
# The webhook's caBundle is written a little after the pod is ready — by Helm
# here, since app.webhook.tls.helmCert.enabled=true above supplies the
# certificate directly rather than leaving it to cainjector. A Bundle applied
# before it lands is rejected with "certificate signed by unknown authority".
for _ in $(seq 1 60); do
  ca="$(kubectl get validatingwebhookconfiguration trust-manager -o jsonpath='{.webhooks[0].clientConfig.caBundle}' 2>/dev/null || true)"
  [ -n "${ca}" ] && break
  sleep 2
done
[ -n "${ca:-}" ] || die "trust-manager webhook has no CA bundle injected yet; retry in a minute"

# --- issuer -----------------------------------------------------------------
CA_PEM="$(mktemp)"
trap 'rm -f "${CA_PEM}"' EXIT

if [ -z "${TLS_ISSUER_NAME}" ]; then
  TLS_ISSUER_NAME="amp-ca"
  TLS_ISSUER_KIND="ClusterIssuer"
  TLS_ISSUER_GROUP="cert-manager.io"
  say "ClusterIssuer amp-ca (bootstrapped from a self-signed root)"
  kubectl apply -f - <<YAML
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: ${BOOTSTRAP_ISSUER}
  namespace: ${CERT_MANAGER_NS}
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ${ROOT_CA_NAME}
  namespace: ${CERT_MANAGER_NS}
spec:
  isCA: true
  commonName: AMP Root CA
  secretName: ${ROOT_CA_NAME}
  duration: ${TLS_ROOT_CA_DURATION}
  renewBefore: 8760h
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: ${BOOTSTRAP_ISSUER}
    kind: Issuer
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: amp-ca
spec:
  ca:
    secretName: ${ROOT_CA_NAME}
YAML
  kubectl -n "${CERT_MANAGER_NS}" wait certificate/"${ROOT_CA_NAME}" --for=condition=Ready --timeout=180s >/dev/null
  kubectl wait clusterissuer/amp-ca --for=condition=Ready --timeout=60s >/dev/null
  kubectl -n "${CERT_MANAGER_NS}" get secret "${ROOT_CA_NAME}" -o jsonpath='{.data.ca\.crt}' | base64 -d > "${CA_PEM}"
else
  say "issuer ${TLS_ISSUER_KIND}/${TLS_ISSUER_NAME} (${TLS_ISSUER_GROUP}, bring-your-own)"
  if [ -n "${TLS_CA_FILE}" ]; then
    [ -f "${TLS_CA_FILE}" ] || die "TLS_CA_FILE=${TLS_CA_FILE} not found"
    cp "${TLS_CA_FILE}" "${CA_PEM}"
  else
    # A CA issuer names its Secret; the CA chain to trust is its tls.crt (or
    # ca.crt when the issuer's CA is itself an intermediate with a chain).
    case "${TLS_ISSUER_KIND}" in
      ClusterIssuer) secret="$(kubectl get clusterissuer "${TLS_ISSUER_NAME}" -o jsonpath='{.spec.ca.secretName}' 2>/dev/null || true)" ;;
      Issuer)        secret="$(kubectl -n "${CERT_MANAGER_NS}" get issuer "${TLS_ISSUER_NAME}" -o jsonpath='{.spec.ca.secretName}' 2>/dev/null || true)" ;;
      *)             secret="" ;;
    esac
    [ -n "${secret}" ] || die "cannot derive the CA to trust from ${TLS_ISSUER_KIND}/${TLS_ISSUER_NAME}; pass TLS_CA_FILE"
    kubectl -n "${CERT_MANAGER_NS}" get secret "${secret}" -o jsonpath='{.data.ca\.crt}' | base64 -d > "${CA_PEM}" || true
    if ! grep -q 'BEGIN CERTIFICATE' "${CA_PEM}"; then
      kubectl -n "${CERT_MANAGER_NS}" get secret "${secret}" -o jsonpath='{.data.tls\.crt}' | base64 -d > "${CA_PEM}"
    fi
  fi
fi
grep -q 'BEGIN CERTIFICATE' "${CA_PEM}" || die "no CA certificate to distribute"

# --- trust sources + Bundle ---------------------------------------------------
say "trust bundle ${BUNDLE_NAME}"
# Fingerprint-based containment, so re-running with the same root is a no-op
# and a new root is appended rather than replacing the old one (CA rotation
# step 1). Falls back to plain text containment without openssl.
fingerprint() {
  if command -v openssl >/dev/null 2>&1; then
    openssl x509 -in "$1" -noout -fingerprint -sha256 2>/dev/null | cut -d= -f2
  else
    tr -d '\n' < "$1"
  fi
}
existing="$(kubectl -n "${CERT_MANAGER_NS}" get configmap "${ROOTS_CONFIGMAP}" -o jsonpath='{.data.ca\.crt}' 2>/dev/null || true)"
ROOTS_PEM="$(mktemp)"
trap 'rm -f "${CA_PEM}" "${ROOTS_PEM}"' EXIT
if [ -n "${existing}" ]; then
  printf '%s\n' "${existing}" > "${ROOTS_PEM}"
  new_fp="$(fingerprint "${CA_PEM}")"
  have=0
  # Walk the existing bundle one certificate at a time.
  awk 'BEGIN{n=0} /BEGIN CERTIFICATE/{n++} {print > ("'"${ROOTS_PEM}"'." n)}' "${ROOTS_PEM}"
  for part in "${ROOTS_PEM}".*; do
    [ -f "$part" ] || continue
    if [ "$(fingerprint "$part")" = "${new_fp}" ]; then have=1; fi
    rm -f "$part"
  done
  if [ "${have}" = 1 ]; then
    echo "   root already in ${ROOTS_CONFIGMAP}"
  else
    echo "   appending a new root to ${ROOTS_CONFIGMAP} (both roots are trusted until the old one is removed)"
    cat "${CA_PEM}" >> "${ROOTS_PEM}"
  fi
else
  cp "${CA_PEM}" "${ROOTS_PEM}"
fi
kubectl -n "${CERT_MANAGER_NS}" create configmap "${ROOTS_CONFIGMAP}" \
  --from-file=ca.crt="${ROOTS_PEM}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

kubectl apply -f - <<YAML
apiVersion: trust.cert-manager.io/v1alpha1
kind: Bundle
metadata:
  name: ${BUNDLE_NAME}
spec:
  sources:
    - configMap:
        name: ${ROOTS_CONFIGMAP}
        key: ca.crt
  target:
    configMap:
      key: ca.crt
    namespaceSelector:
      matchLabels:
        ${TRUST_LABEL}: "true"
YAML

# --- namespaces -------------------------------------------------------------
for ns in ${TLS_TRUST_NAMESPACES}; do
  say "namespace ${ns}: label ${TRUST_LABEL}=true"
  kubectl create namespace "${ns}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl label namespace "${ns}" "${TRUST_LABEL}=true" --overwrite >/dev/null
  for _ in $(seq 1 30); do
    kubectl -n "${ns}" get configmap "${BUNDLE_NAME}" >/dev/null 2>&1 && break
    sleep 2
  done
  kubectl -n "${ns}" get configmap "${BUNDLE_NAME}" >/dev/null 2>&1 \
    || die "trust-manager did not sync ${BUNDLE_NAME} into ${ns}"
  echo "   ${BUNDLE_NAME} present"
done

echo
echo "issuer: ${TLS_ISSUER_KIND}/${TLS_ISSUER_NAME} (${TLS_ISSUER_GROUP})"
echo "trust:  ConfigMap ${BUNDLE_NAME} key ca.crt in every namespace labelled ${TRUST_LABEL}=true"
