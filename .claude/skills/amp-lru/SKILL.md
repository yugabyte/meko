---
name: amp-lru
description: Create, change or retire an amp LRU (long-running universe) — a real workload that runs around the clock on amp in its own pool and pods, reports metrics to the shared LRU Grafana, exports a health verdict, and is registered on the amp report portal for the hourly sync. Use when asked to add an LRU, a new always-on workload, a soak on amp-dev, or to register, edit, retire or troubleshoot one; and before touching anything under amp/deploy/lru/.
---

# amp LRU

An LRU is a standing environment on amp that never gets torn down, runs a real
application workload 24x7, and gets an hourly verdict on the report portal. Every
LRU has the same shape so the portal, the sync job and a person on call can treat
them alike. The authority on the shape is `amp/deploy/lru/README.md`; read it first.
`amp/docs/lru-environment.md` is the companion: which cluster and namespaces, which
four LRUs exist, how a verdict reaches the portal, and what has already gone wrong.
Read it before touching a running LRU, and when picking this up without context.

## Where things are

| Piece | Where | Owner of changes |
|---|---|---|
| The LRU itself: pool, pods, checker, dashboard | `amp/deploy/lru/<name>/` in yugabyte/meko | you, one directory per LRU |
| Shared Prometheus + Grafana + namespace | `amp/deploy/lru/monitoring/` in yugabyte/meko | rarely changes; applies to every LRU |
| Portal row, list, detail, `yb amp lru`, MCP tools | yugabyte/qa-services `amp-report-service`, `yb-cli` | already built; you register rows, you do not change code |
| Hourly sync job | yugabyte/meko-devops `jenkins/amp/scripts/lru-sync.py`, `pipeline_amp_lru_sync.groovy` | already built; only edit to add a new verdict type |

## Rules every LRU follows

1. **Its own pool.** `lru-<name>` on the amp deployment, one cluster, branch `main`,
   one `read_write` endpoint. Created by the LRU's `up.sh` through the controller
   API, idempotently: look up by name, create only if missing.
2. **Its own pods, shared namespace `amp-lru`.** Every resource name is prefixed
   `<name>-` (dashes), every object labelled `lru: <name>` (underscores allowed).
   `kubectl get all -l lru=<name>` must return the whole LRU and nothing else.
3. **Endpoint by alias.** An `ExternalName` Service `<name>-endpoint` that `up.sh`
   points at `yb-ep-<id>.amp-dev.svc.cluster.local`. Manifests never carry an
   endpoint id.
4. **Metrics by annotation.** Pods to scrape set `prometheus.io/scrape: "true"` and
   name metrics ports `metrics` or `metrics-<x>`. Nothing in `monitoring/` is
   edited. Series get `job` (pod label `app`), `lru`, `component` (port name).
5. **No environment ids in tracked files.** Resolve an endpoint id from the
   controller by pool name (`amp_endpoint_info{default="true",pool="lru-<name>"}` in a
   dashboard, the controller API in a checker). Never sed one into a tracked
   manifest: it goes stale and leaves the checkout dirty.
6. **A checker.** One pod, `checker/check.py`, evaluating the LRU's rules every
   minute against `http://prometheus:9090` and exporting on `:9100`:
   `lru_check_status{lru,check}` (1 ok, 0.5 warn, 0 fail, -1 unreadable),
   `lru_check_value{lru,check}`, `lru_overall_status{lru}` — the minimum over
   every rule, unreadable ones included, so a rule that cannot run degrades the
   verdict instead of vanishing from it. Every series carries
   `lru="<name>"`. Check names end in their unit: `_per_s`, `_ratio`, `_s`,
   `_bytes`, `_total`, `_up`/`_reachable`. The portal formats them from that.
7. **A dashboard as a ConfigMap.** `dashboard.json` generated into a ConfigMap
   labelled `grafana_dashboard: "1"`, uid `lru-<name-with-dashes>`, title
   `LRU: <name>`. Every Temporal/app/checker query scoped with `lru="$lru"` where
   `$lru` is a hidden constant variable. Endpoint queries filter on
   `endpoint="$endpoint"`, a hidden variable resolved from
   `amp_endpoint_info{default="true",pool="lru-<name>"}`. The `default` filter is
   load-bearing: a fork of the branch puts a second endpoint in the same pool,
   and without it the hidden variable can latch onto the fork.
8. **`up.sh`, `README.md`, `FINDINGS.md`.** `up.sh` idempotent and driven by env
   vars with defaults (controller, namespace, pool, sizes, kube context). README
   says what runs and how to operate it. FINDINGS is the dated log of what the
   workload has shown.
9. **No credentials anywhere.** Not in manifests, not in the portal row. Point at
   the 1Password item.
10. **Never hardcode the environment.** Cluster, cloud, namespace, URLs, intervals
   are env vars in `up.sh` or fields on the portal row. The next LRU may be on
   another cluster or cloud.

## Adding an LRU

1. `cp -r amp/deploy/lru/small_temporal amp/deploy/lru/<name>`. Replace the
   workload manifests (`temporal*.yaml`, `omes.yaml`) with yours. Rename every
   resource and label from `small-temporal`/`small_temporal` to yours, including
   in `kustomization.yaml`, `checker.yaml`, `endpoint.yaml`, `up.sh`
   (`LRU`, `POOL`, `CLUSTER` defaults and the `patch svc` / `rollout status` names).
2. Write the rules in `checker/check.py`: keep `q()`, `band()`, the exporter and
   the `lru` label; replace the body of `check_all()`. Keep the unit suffixes.
3. Write `dashboard.json`. Scope every query to `lru="$lru"`; keep the hidden
   `lru` constant and `endpoint` variables from the Temporal one. Set `uid` and
   `title`.
4. Delete `FINDINGS.md` contents and start a fresh dated log.
5. `KUBE_CONTEXT=meko_dev_eks ./up.sh`. It applies `../monitoring` first, so the
   shared base is always current.
6. Verify: `kubectl -n amp-lru get all -l lru=<name>`; in Prometheus
   `up{lru="<name>"}` is 1 for every pod; `lru_overall_status{lru="<name>"}` exists;
   the dashboard appears at `https://grafana-lru.tail5955.ts.net/d/lru-<name>`.
7. Register the row (below). The next hourly sync picks it up with no pipeline
   change; `yb amp lru sync <name> --yes` runs it now.
8. Open a PR against `main` with the new directory only.

## The portal row

The portal stores one row per LRU and one verdict per hour. Nothing else. Metrics
stay in Grafana; the detail page embeds the dashboard.

```bash
yb amp lru list
yb amp lru get <name>
yb amp lru create --name <name> --kind <kind> --owner <you> --data ./lru.json --yes
yb amp lru update <name> --data ./patch.json --yes      # shallow merge
yb amp lru update <name> --set owner=<who> --yes
yb amp lru retire <name> --yes                         # stops the sync, keeps history
yb amp lru delete <name> --hard --yes                  # only for a mistake
yb amp lru checks <name> --since 24h
yb amp lru sync <name> --yes                           # run the hourly check now
yb amp lru report <name> --status ok|not_ok|error --yes   # manual verdict
```

MCP tools on `https://amp-qa.dev.yugabyte.com/mcp`: `list_lrus`, `get_lru`,
`list_lru_checks`, `create_lru`, `update_lru`, `retire_lru`, `delete_lru`,
`sync_lru`, `report_lru_check`. Writes only when the user asks for them.

`lru.json`, the `data` object, for an LRU on this cluster:

```json
{
  "description": "one sentence a stranger understands",
  "k8s": { "provider": "eks", "cluster": "meko_dev_eks", "region": "us-east-2",
           "namespace": "amp-lru", "credential_id": "meko-jenkins-deploy" },
  "amp": { "controller_url": "http://amp-controller.amp-dev.svc.cluster.local:8080",
           "pool": "lru-<name>", "cluster": "<cluster>", "endpoint_id": "<uuid>" },
  "foundation": { "yba_url": "http://a0ea6ee2d58194693870037cb841bdbe-1826583179.us-east-2.elb.amazonaws.com",
                  "universe": "amp2-db", "namespace": "yb-dev-amp2-db" },
  "verdict": { "type": "prometheus-kubectl", "url": "http://prometheus:9090",
               "exec_target": "deploy/<name>-<some-pod>", "selector": "lru=\"<name>\"" },
  "dashboard_url": "https://grafana-lru.tail5955.ts.net/d/lru-<name>",
  "checks_expected_every": "1h",
  "links": [ { "label": "Grafana", "url": "..." }, { "label": "amp UI", "url": "..." } ],
  "access": "kubectl -n amp-lru get all -l lru=<name>\nmanifests: amp/deploy/lru/<name>"
}
```

Unknown keys round-trip verbatim, so an LRU on another cloud adds what it needs.
`verdict.type` may also be `prometheus-http`, `http-json`, `ssh-script` or
`manual`; see `lru-sync.py` for what each expects. The sync job authenticates to
EKS clusters only, so an LRU elsewhere needs a credential bound in
`pipeline_amp_lru_sync.groovy` first.

## Verdict and staleness

`ok` when `lru_overall_status` is 1 or 0.5 (warnings listed), `not_ok` when 0
(failing rules listed), `error` when the sync could not read the LRU. A row with
no verdict for twice `checks_expected_every` shows `stale` and outranks any old
green. A stale badge means the sync job or the checker is down, not the workload.

## Troubleshooting

- Badge `stale`, workload fine: is `amp-lru-sync` registered and green in Jenkins?
  Is the checker pod up? `yb amp lru sync <name> --yes` and read the build log.
- Badge `error`: the sync could not exec or query. `exec_target` names a pod
  that exists? Prometheus has `lru_overall_status{lru="<name>"}`?
- Dashboard empty in the portal frame: reader not on Tailscale, or the ConfigMap
  lacks the `grafana_dashboard=1` label.
- Prometheus not scraping a pod: the annotation, and the port name starting with
  `metrics`.
- Two LRUs of the same kind showing each other's data: a query without
  `lru="$lru"`.

## What not to do

- Do not add scrape targets or dashboards to `monitoring/`. The LRU carries them.
- Do not reap `amp-lru` or any `lru-*` pool as cleanup.
- Do not edit dashboards in the Grafana UI; the next apply overwrites them.
- Do not store metrics in the portal. It holds rows and verdicts only.
