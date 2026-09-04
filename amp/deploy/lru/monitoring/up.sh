#!/usr/bin/env bash
# Applies the shared LRU monitoring base and makes sure the Grafana admin Secret
# exists with a generated password. Idempotent: an existing Secret is left alone,
# so re-running never rotates the password under a logged-in browser.
#
#   KUBE_CONTEXT=meko_dev_eks ./up.sh
set -euo pipefail

KUBE_CONTEXT=${KUBE_CONTEXT:-meko_dev_eks}
NS=${LRU_NS:-amp-lru}
HERE=$(cd "$(dirname "$0")" && pwd)

k() { kubectl --context "$KUBE_CONTEXT" "$@"; }

k apply -f "$HERE/namespace.yaml"
# Grafana will not start without this key, so mint it before applying the
# Deployment. An existing password is left alone: re-running must never rotate
# it under a logged-in browser.
if ! k -n "$NS" get secret grafana-admin -o jsonpath='{.data.password}' 2>/dev/null | grep -q .; then
  PW=$(openssl rand -hex 16)
  k -n "$NS" create secret generic grafana-admin \
    --from-literal=user=admin --from-literal=password="$PW" \
    --dry-run=client -o yaml | k apply -f -
  echo "grafana admin password generated; read it with:"
  echo "  kubectl -n $NS get secret grafana-admin -o jsonpath='{.data.password}' | base64 -d"
fi
k apply -k "$HERE"
# Cross-namespace RBAC, kept out of the kustomization so its namespace
# transformer cannot rewrite it into amp-lru.
k apply -f "$HERE/prometheus-rbac-amp-dev.yaml"
k -n "$NS" rollout status deploy/prometheus --timeout=300s
k -n "$NS" rollout status deploy/grafana --timeout=300s
