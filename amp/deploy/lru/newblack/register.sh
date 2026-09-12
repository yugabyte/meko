#!/usr/bin/env bash
# Registers (or refreshes) the newblack row on the amp report portal, building the
# data object from lru.env so no endpoint id is ever tracked in git. Run ./up.sh
# first: it writes lru.env.
#
#   ./register.sh            # create if missing, otherwise update
#   DRY_RUN=1 ./register.sh  # print the row and stop
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
LRU=${LRU:-newblack}
OWNER=${OWNER:-spilshchikov}
KIND=${KIND:-commerce-checkout}
YBA_URL=${YBA_URL:-http://a0ea6ee2d58194693870037cb841bdbe-1826583179.us-east-2.elb.amazonaws.com}
YBA_UNIVERSE=${YBA_UNIVERSE:-amp2-db}
YBA_NAMESPACE=${YBA_NAMESPACE:-yb-dev-amp2-db}

[ -f "$HERE/lru.env" ] || { echo "no lru.env; run ./up.sh first" >&2; exit 1; }
# shellcheck disable=SC1091
. "$HERE/lru.env"

ROW=$(ENDPOINT_ID="$ENDPOINT_ID" LRU="$LRU" YBA_URL="$YBA_URL" \
  YBA_UNIVERSE="$YBA_UNIVERSE" YBA_NAMESPACE="$YBA_NAMESPACE" python3 - <<'PY'
import json
import os

lru = os.environ["LRU"]
print(json.dumps({
    "description": (
        "The anonymised NewBlack checkout workload: the customer's own schema and "
        "SQL mix, replayed at production query weights against one amp endpoint."
    ),
    "k8s": {"provider": "eks", "cluster": "meko_dev_eks", "region": "us-east-2",
            "namespace": "amp-lru", "credential_id": "meko-jenkins-deploy"},
    "amp": {"controller_url": "http://amp-controller.amp-dev.svc.cluster.local:8080",
            "pool": f"lru-{lru}", "cluster": lru, "endpoint_id": os.environ["ENDPOINT_ID"]},
    "foundation": {"yba_url": os.environ["YBA_URL"], "universe": os.environ["YBA_UNIVERSE"],
                   "namespace": os.environ["YBA_NAMESPACE"]},
    "verdict": {"type": "prometheus-kubectl", "url": "http://prometheus:9090",
                "exec_target": f"deploy/{lru}-checker", "selector": f'lru="{lru}"'},
    "dashboard_url": f"https://grafana-lru.tail5955.ts.net/d/lru-{lru}",
    "checks_expected_every": "1h",
    "links": [
        {"label": "Grafana", "url": f"https://grafana-lru.tail5955.ts.net/d/lru-{lru}"},
        {"label": "amp-time", "url": f"https://amp-time-{lru.replace('_', '-')}.tail5955.ts.net"},
        {"label": "workload source", "url": "https://github.com/yugabyte/commerce-loadtest"},
    ],
    "access": (
        f"kubectl -n amp-lru get all -l lru={lru}\n"
        f"manifests: amp/deploy/lru/{lru}\n"
        f"driver log: kubectl -n amp-lru logs deploy/{lru}-bomber -c bomber"
    ),
}, indent=2))
PY
)

if [ -n "${DRY_RUN:-}" ]; then echo "$ROW"; exit 0; fi

echo "$ROW" > "$HERE/lru.json"
if yb amp lru get "$LRU" >/dev/null 2>&1; then
  yb amp lru update "$LRU" --data "$HERE/lru.json" --yes
else
  yb amp lru create --name "$LRU" --kind "$KIND" --owner "$OWNER" --data "$HERE/lru.json" --yes
fi
yb amp lru sync "$LRU" --yes
yb amp lru get "$LRU"
