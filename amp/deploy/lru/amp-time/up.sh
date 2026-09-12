#!/usr/bin/env bash
# Applies amp-time for every LRU. Idempotent.
#
#   KUBE_CONTEXT=meko_dev_eks ./up.sh
set -euo pipefail

KUBE_CONTEXT=${KUBE_CONTEXT:-meko_dev_eks}
NS=${LRU_NS:-amp-lru}
HERE=$(cd "$(dirname "$0")" && pwd)

k() { kubectl --context "$KUBE_CONTEXT" "$@"; }

k apply -k "$HERE"
# The ConfigMap names are stable, so an edit to the vendored package does not
# change the pod spec. The package is copied in by an init container, so nothing
# picks it up without a restart.
k -n "$NS" rollout restart deploy/amp-time
k -n "$NS" rollout status deploy/amp-time --timeout=600s
k -n "$NS" get ingress -l '!lru' -o custom-columns=NAME:.metadata.name,HOST:.status.loadBalancer.ingress[0].hostname
