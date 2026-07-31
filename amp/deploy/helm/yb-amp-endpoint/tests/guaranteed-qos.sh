#!/usr/bin/env bash
#
# Guardrail: EVERY container in the compute (yb-amp-endpoint) pod must set
# resources.requests == resources.limits for both cpu and memory, so the pod
# gets Kubernetes "Guaranteed" QoS. QoS is a pod-level property — a single
# container without a matching request==limit drops the whole pod to Burstable.
#
# This renders the chart and checks all containers, so it catches a *newly
# added* container (e.g. a sidecar) that forgot its request==limit block —
# which the per-container helm-unittest assertions cannot.
#
# Requires: helm, yq (v4+). Run: bash tests/guaranteed-qos.sh
set -euo pipefail

CHART="$(cd "$(dirname "$0")/.." && pwd)"

# Emit one "name|reqCpu|limCpu|reqMem|limMem" row per container in the pod, for a
# given render. Missing fields render as MISSING so they fail the comparison.
containers() {
  helm template qos "$CHART" --set image.tag=dev "$@" 2>/dev/null | yq eval-all '
    select(.kind == "Deployment") | .spec.template.spec.containers[] |
      .name
      + "|" + (.resources.requests.cpu    // "MISSING")
      + "|" + (.resources.limits.cpu      // "MISSING")
      + "|" + (.resources.requests.memory // "MISSING")
      + "|" + (.resources.limits.memory   // "MISSING")' -
}

check() {
  local desc="$1"; shift
  local fail=0 rows name reqc limc reqm limm
  rows="$(containers "$@")"
  if [ -z "$rows" ]; then
    echo "  ✗ [$desc] no containers found in the rendered Deployment"
    return 1
  fi
  while IFS='|' read -r name reqc limc reqm limm; do
    [ -z "$name" ] && continue
    if [ "$reqc" = MISSING ] || [ "$limc" = MISSING ] || [ "$reqm" = MISSING ] || [ "$limm" = MISSING ] \
       || [ "$reqc" != "$limc" ] || [ "$reqm" != "$limm" ]; then
      echo "  ✗ [$desc] container '$name': requests(cpu=$reqc, mem=$reqm) != limits(cpu=$limc, mem=$limm)"
      fail=1
    else
      echo "  ✓ [$desc] container '$name': cpu=$reqc memory=$reqm (request == limit)"
    fi
  done <<< "$rows"
  return "$fail"
}

echo "Guaranteed-QoS guardrail — yb-amp-endpoint pod containers:"
rc=0
# Default values, and an explicit override, so the invariant is checked for both
# the built-in defaults and a caller-supplied resource spec.
check "defaults" || rc=1
check "custom" --set endpoint.resources.cpu=1500m --set endpoint.resources.memory=8Gi || rc=1

if [ "$rc" -ne 0 ]; then
  echo
  echo "FAIL: a container is not Guaranteed-QoS shaped. Every container in the"
  echo "compute pod must set resources.requests == resources.limits (cpu+memory)."
  exit 1
fi
echo "PASS: all containers set request == limit (pod is Guaranteed QoS)."
