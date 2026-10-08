#!/usr/bin/env bash
#
# external-dns for one endpoint DNS zone: publishes the DNSEndpoint records the
# endpoint releases render, for the nlb_* and ingress_* modes alike.
# Run once per zone the controller is configured with
# (--deploy-ingress-zone-internal / --deploy-ingress-zone-external).
#
# Idempotent. See controller/architecture/ac-ingress.md ("Dependencies outside
# the controller") and ac-k8s-deploy.md ("Published names").
#
# What it pins, and why each is not the chart's default:
#
#   * sources: [crd] only. The records are stated outright as DNSEndpoint objects;
#     a service or ingress source would also publish anything else in the
#     namespace that happens to carry an external-dns annotation.
#   * policy: sync. upsert-only never deletes, so every retired endpoint would
#     leave its name resolving to a load balancer that is gone, or to whichever
#     endpoint's address the cloud hands out next.
#   * One release per zone, filtered by domain AND zone id. The internal zone's
#     names must never reach the public one, and a domain filter alone matches a
#     public and a private hosted zone with the same name.
#   * namespaced: true. The DNSEndpoints live in the controller's namespace, so
#     external-dns needs a Role there and nothing cluster-wide.
#
# Not done here: the credentials. external-dns needs write access to the zone,
# and that is the zone owner's to grant — on BYOC, the customer. Pass the
# ServiceAccount annotation that binds it (IRSA on EKS, Workload Identity on GKE)
# in SA_ANNOTATION. The DNSEndpoint CRD is cluster-scoped and installed from the
# chart's crds/ on the first install only.
#
# Env:
#   NS              namespace the controller and its endpoints run in (default amp)
#   FAMILY          internal | external — names the release (required)
#   ZONE            the zone's domain, e.g. internal.pg.example.com (required)
#   ZONE_ID         the hosted zone's id: Route 53 zone id, or Cloud DNS zone name (required)
#   PROVIDER        aws | google (default aws)
#   GOOGLE_PROJECT  the Cloud DNS project (PROVIDER=google)
#   SA_ANNOTATION   "key=value" on the ServiceAccount, e.g.
#                   eks.amazonaws.com/role-arn=arn:aws:iam::123456789012:role/amp-external-dns
#   TXT_OWNER_ID    ownership marker on every record (default amp-<NS>-<FAMILY>).
#                   Must be unique per controller deployment sharing the zone, or
#                   one deployment's sync deletes the other's records.
#   RENDER=1        print the manifests instead of installing
set -euo pipefail

NS="${NS:-amp}"
FAMILY="${FAMILY:?FAMILY=internal|external is required}"
ZONE="${ZONE:?ZONE (the zone domain) is required}"
ZONE_ID="${ZONE_ID:?ZONE_ID (the hosted zone id) is required}"
PROVIDER="${PROVIDER:-aws}"
TXT_OWNER_ID="${TXT_OWNER_ID:-amp-${NS}-${FAMILY}}"
SA_ANNOTATION="${SA_ANNOTATION:-}"
CHART_VERSION="${CHART_VERSION:-1.23.0}"

case "${FAMILY}" in internal|external) ;; *)
  echo "!! FAMILY must be internal or external (got '${FAMILY}')" >&2; exit 1 ;;
esac

args=(
  --namespace "${NS}"
  --repo https://kubernetes-sigs.github.io/external-dns/
  --version "${CHART_VERSION}"
  --set "namespaced=true"
  --set "sources={crd}"
  --set "policy=sync"
  --set "registry=txt"
  --set "txtOwnerId=${TXT_OWNER_ID}"
  --set "domainFilters={${ZONE}}"
  --set "provider.name=${PROVIDER}"
  --set "extraArgs={--zone-id-filter=${ZONE_ID}}"
)
case "${PROVIDER}" in
  aws) ;;
  google)
    : "${GOOGLE_PROJECT:?GOOGLE_PROJECT is required for PROVIDER=google}"
    args+=(--set "extraArgs={--zone-id-filter=${ZONE_ID},--google-project=${GOOGLE_PROJECT}}")
    ;;
  *)
    echo "!! PROVIDER must be aws or google (got '${PROVIDER}'); see the external-dns chart for others" >&2
    exit 1 ;;
esac
if [ -n "${SA_ANNOTATION}" ]; then
  # --set-string, and the dots in the key escaped: the key is an annotation name.
  key="${SA_ANNOTATION%%=*}"
  args+=(--set-string "serviceAccount.annotations.${key//./\\.}=${SA_ANNOTATION#*=}")
fi

release="external-dns-${FAMILY}"
if [ "${RENDER:-0}" = "1" ]; then
  exec helm template "${release}" external-dns --include-crds "${args[@]}"
fi
echo ">> installing ${release} for ${ZONE} (${PROVIDER} zone ${ZONE_ID}) in ${NS}"
helm upgrade --install "${release}" external-dns --wait "${args[@]}"
