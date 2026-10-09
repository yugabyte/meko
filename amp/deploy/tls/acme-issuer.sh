#!/usr/bin/env bash
#
# A publicly-trusted certificate for the endpoint DNS zone, from Let's Encrypt:
# an ACME ClusterIssuer solving DNS-01, and (by default) the one wildcard
# Certificate every compute in the zone mounts.
#
# The alternative to this script is `deploy/tls/trust-root.sh`, whose `amp-ca`
# ClusterIssuer signs per-endpoint certificates for nothing and in no time — but
# a client has to be given the CA bundle out of band. Use that inside a network
# you control, and this where the client is somebody else's psql.
#
# Idempotent. See controller/architecture/ac-compute-tls.md and runbooks/tls.md.
#
# Why DNS-01, and why a wildcard:
#
#   * HTTP-01 and TLS-ALPN-01 both need the ACME server to reach the name it is
#     validating on 80 or 443. An endpoint answers the POSTGRES protocol on a
#     port of a shared load balancer, so there is nothing there to solve with.
#   * Let's Encrypt allows 50 NEW certificates per registered domain per week
#     (renewals exempt, so it caps CREATES rather than fleet size). In BYOC the
#     zone belongs to the customer, so that budget is theirs alone and
#     ISSUE_WILDCARD=0 with --deploy-compute-tls-mode=issue is a reasonable
#     choice below that create rate. The wildcard is still the default because it
#     needs one order per zone however fast endpoints are created.
#     Its cost is a shared private key: any endpoint pod that is compromised
#     leaks the key for every name in the zone. Where that matters, issue
#     per-endpoint certificates from a private CA instead and let clients trust
#     the CA — the trade is a trust distribution problem for a blast-radius one.
#   * ISSUE_WILDCARD=0 skips the certificate and leaves just the issuer, for a
#     deployment small enough to want --deploy-compute-tls-mode=issue against it.
#
# Env:
#   ZONE                  (required) the endpoint DNS zone, e.g. db.example.com.
#                         Must be the zone the controller's --ingress-zone-* uses,
#                         or the names it publishes are not in the certificate.
#   ACME_EMAIL            (required) registration contact for expiry warnings.
#   NS                    namespace the wildcard Secret is created in — the one
#                         endpoints deploy into (default: amp).
#   ACME_ENV              staging (default) | prod. STAGING FIRST: prod has the
#                         rate limit above and a failed-order limit besides, and
#                         staging issues from an untrusted root that otherwise
#                         behaves identically.
#   ISSUER_NAME           ClusterIssuer name (default: amp-acme-<ACME_ENV>).
#   SECRET_NAME           the wildcard Secret (default: amp-endpoints-wildcard-tls).
#   ISSUE_WILDCARD        1 (default) | 0 — see above.
#   DNS_PROVIDER          route53 (default) | cloud-dns.
#   AWS_REGION            route53: region for the STS/Route53 calls (us-east-1).
#   AWS_HOSTED_ZONE_ID    route53: pin the solver to one zone. Optional but
#                         recommended — without it cert-manager needs
#                         route53:ListHostedZonesByName to find it.
#   AWS_ROLE_ARN          route53: role cert-manager assumes (IRSA). Leave empty
#                         to use the ambient credentials of its ServiceAccount.
#   GCP_PROJECT           cloud-dns: project holding the zone.
set -euo pipefail

ZONE="${ZONE:?ZONE is required, e.g. ZONE=db.example.com}"
ACME_EMAIL="${ACME_EMAIL:?ACME_EMAIL is required}"
NS="${NS:-amp}"
ACME_ENV="${ACME_ENV:-staging}"
ISSUE_WILDCARD="${ISSUE_WILDCARD:-1}"
DNS_PROVIDER="${DNS_PROVIDER:-route53}"
AWS_REGION="${AWS_REGION:-us-east-1}"
AWS_HOSTED_ZONE_ID="${AWS_HOSTED_ZONE_ID:-}"
AWS_ROLE_ARN="${AWS_ROLE_ARN:-}"
GCP_PROJECT="${GCP_PROJECT:-}"
SECRET_NAME="${SECRET_NAME:-amp-endpoints-wildcard-tls}"

say() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
die() {
  printf '\033[1;31merror: %s\033[0m\n' "$*" >&2
  exit 1
}

command -v kubectl >/dev/null 2>&1 || die "kubectl not found on PATH"
kubectl get crd clusterissuers.cert-manager.io >/dev/null 2>&1 ||
  die "cert-manager is not installed; run deploy/tls/trust-root.sh first"

case "${ACME_ENV}" in
staging) ACME_SERVER="https://acme-staging-v02.api.letsencrypt.org/directory" ;;
prod) ACME_SERVER="https://acme-v02.api.letsencrypt.org/directory" ;;
*) die "ACME_ENV must be staging or prod, got '${ACME_ENV}'" ;;
esac
ISSUER_NAME="${ISSUER_NAME:-amp-acme-${ACME_ENV}}"

# Rendered into the solver list item below, so every line here is indented to
# that item's depth. A `- ` list entry under `solvers:` must be indented at least
# as far as the key, which is why the item starts at six spaces and this at eight.
case "${DNS_PROVIDER}" in
route53)
  solver="        dns01:
          route53:
            region: ${AWS_REGION}"
  [ -n "${AWS_HOSTED_ZONE_ID}" ] && solver="${solver}
            hostedZoneID: ${AWS_HOSTED_ZONE_ID}"
  [ -n "${AWS_ROLE_ARN}" ] && solver="${solver}
            role: ${AWS_ROLE_ARN}"
  ;;
cloud-dns)
  [ -n "${GCP_PROJECT}" ] || die "DNS_PROVIDER=cloud-dns needs GCP_PROJECT"
  solver="        dns01:
          cloudDNS:
            project: ${GCP_PROJECT}"
  ;;
*) die "DNS_PROVIDER must be route53 or cloud-dns, got '${DNS_PROVIDER}'" ;;
esac

say "ClusterIssuer ${ISSUER_NAME} (Let's Encrypt ${ACME_ENV}, DNS-01 via ${DNS_PROVIDER})"
# dnsZones, not dnsNames: the solver is selected per name being validated, and a
# wildcard order validates `_acme-challenge.<zone>` — which matches the zone
# selector and not a `*.<zone>` name selector.
kubectl apply -f - <<YAML
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: ${ISSUER_NAME}
spec:
  acme:
    server: ${ACME_SERVER}
    email: ${ACME_EMAIL}
    privateKeySecretRef:
      name: ${ISSUER_NAME}-account-key
    solvers:
      - selector:
          dnsZones:
            - "${ZONE}"
${solver}
YAML
kubectl wait clusterissuer/"${ISSUER_NAME}" --for=condition=Ready --timeout=120s >/dev/null ||
  die "the issuer did not register with Let's Encrypt; check its status and cert-manager's log"

if [ "${ISSUE_WILDCARD}" != "1" ]; then
  say "done (issuer only)"
  cat <<EOF

  Point the controller at it per endpoint:

    --deploy-compute-tls-mode=issue \\
    --deploy-compute-tls-issuer-name=${ISSUER_NAME} \\
    --deploy-compute-tls-cert-duration= \\
    --deploy-compute-tls-include-service-dns=false

  Mind the 50-certificates-per-week limit on ACME_ENV=prod.
EOF
  exit 0
fi

# The apex alongside the wildcard: `*.<zone>` does not match `<zone>` itself, and
# a deployment that ever publishes an endpoint at the apex would find the
# certificate does not cover it. One extra name costs nothing in the same order.
say "wildcard Certificate *.${ZONE} -> Secret ${NS}/${SECRET_NAME}"
kubectl get namespace "${NS}" >/dev/null 2>&1 || kubectl create namespace "${NS}"
# No duration: Let's Encrypt fixes the lifetime at 90 days and only warns that a
# request for another was ignored. renewBefore is cert-manager's own, and 30 days
# is what leaves room to notice a renewal that is failing.
kubectl apply -f - <<YAML
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: amp-endpoints-wildcard
  namespace: ${NS}
spec:
  secretName: ${SECRET_NAME}
  dnsNames:
    - "*.${ZONE}"
    - "${ZONE}"
  usages:
    - server auth
  renewBefore: 720h
  privateKey:
    algorithm: ECDSA
    size: 256
    rotationPolicy: Always
  issuerRef:
    kind: ClusterIssuer
    name: ${ISSUER_NAME}
    group: cert-manager.io
YAML

# Minutes, not seconds: a DNS-01 order waits for the TXT record to propagate and
# for Let's Encrypt to see it.
say "waiting for issuance (DNS-01 propagation; several minutes is normal)"
kubectl -n "${NS}" wait certificate/amp-endpoints-wildcard --for=condition=Ready --timeout=900s ||
  die "issuance did not complete; kubectl -n ${NS} describe certificate amp-endpoints-wildcard"

say "done"
cat <<EOF

  Deploy endpoints against it:

    --deploy-compute-tls-mode=existing-secret \\
    --deploy-compute-tls-secret=${SECRET_NAME}

  or, via the controller chart:

    --set controller.endpoint.computeTlsMode=existing-secret \\
    --set controller.endpoint.computeTlsSecret=${SECRET_NAME}

  Clients then connect with sslmode=verify-full and the system trust store:

    psql "host=ep-<id>.${ZONE} port=<port> user=cloud_admin dbname=postgres sslmode=verify-full"
EOF
if [ "${ACME_ENV}" = "staging" ]; then
  cat <<EOF

  NOTE: this is the STAGING root, which no client trusts. Re-run with
  ACME_ENV=prod once an endpoint has verified against it with
  sslmode=verify-ca and the staging root supplied as sslrootcert.
EOF
fi
