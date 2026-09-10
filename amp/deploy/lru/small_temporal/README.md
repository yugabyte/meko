# small_temporal

A Temporal server whose persistence store is an amp endpoint, loaded around the
clock by Omes, Temporal's own load generator, at a deliberately small rate. It is
the first LRU and the reference for the shape described in [`../README.md`](../README.md).

Temporal is a workflow engine that keeps every workflow's state and history in a
relational database, so it is a database-heavy OLTP application: hot small-row
updates, one visibility table with dozens of indexes, multi-column GIN indexes.
Omes ships ready-made scenarios; nobody here writes workflow code.

## What runs

Namespace `amp-lru`, every object labelled `lru: small_temporal`.

| Object | What |
|---|---|
| `small-temporal-server` (Deployment) | `temporalio/auto-setup`: all Temporal services in one process. gRPC on `small-temporal-frontend:7233`, HTTP on `:7243`, Prometheus on `:8000`. Creates `temporal_lru` and `temporal_lru_visibility` on the endpoint and runs the schema tooling on first boot as `cloud_admin`. |
| `small-temporal-ui` (Deployment) | `temporalio/ui`, tailnet `http://small-temporal.tail5955.ts.net`. |
| `small-temporal-omes` (Deployment) | `golang:1.26`; clones and builds Omes at pod start, then loops `run-scenario-with-worker`. Metrics on `:9090` (load generator) and `:9091` (worker process). |
| `small-temporal-checker` (Deployment) | `checker/check.py`: 12 health rules a minute, exported with `lru="small_temporal"`. |
| `small-temporal-endpoint` (ExternalName) | points at this LRU's `yb-ep-<id>` Service in `amp-dev`; set by `up.sh`. |
| `small-temporal-dynamicconfig` (ConfigMap) | Temporal dynamic config the image no longer ships. Without the Nexus callback template and allowed-address rule every Nexus operation in `throughput_stress` fails. |
| `small-temporal-dashboard` (ConfigMap) | key `small-temporal-dashboard.json` (the sidecar names the file on disk after the key, and all LRUs share one directory), labelled `grafana_dashboard=1`; the shared Grafana shows it as "LRU: small_temporal". |

amp side: pool `lru-temporal`, cluster `temporal`, branch `main` (PG 18), one
`read_write` endpoint at 2 vCPU / 16 GiB, on the shared `amp2-db` foundation.
Portal row: `small_temporal` at https://amp-qa.dev.yugabyte.com/lru.

`enableServiceLinks: false` on every pod is required: the UI Service would
otherwise inject `TEMPORAL_UI_PORT=tcp://...` and crash the UI.

## Bring up, change, pause

```bash
KUBE_CONTEXT=meko_dev_eks ./up.sh
```

Idempotent. Finds or creates the pool, cluster and endpoint through the
controller API (`admin:admin` over a local port-forward), applies `../monitoring`
then this directory, and points `small-temporal-endpoint` at the endpoint. Ids
land in `lru.env` (gitignored). Controller, namespace, pool, sizes and kube
context are env vars with defaults.

`small-temporal-endpoint`, the ExternalName alias every pod resolves, is the one
object `up.sh` applies outside kustomize. It has to be: it carries the endpoint id,
so a tracked manifest could only ship a placeholder, and a bare `kubectl apply -k .`
would then reset the alias and take the whole LRU down. Re-run `up.sh` to repoint it.

No endpoint id is stored in a tracked file: the checker asks the controller which endpoint
belongs to pool `lru-temporal` and re-resolves every five minutes, so recreating
the endpoint needs no redeploy. The dashboard resolves it the same way, from
`amp_endpoint_info{pool="lru-temporal"}`; if you run `up.sh` with a different
`POOL`, change that query in `dashboard.json` to match.

Workload knobs are the `small-temporal-omes-run` ConfigMap: `OMES_SCENARIO`,
`OMES_MAX_CONCURRENT`, `OMES_MAX_ITERATIONS_PER_SECOND`, `OMES_CYCLE_DURATION`.
Edit `omes.yaml`, `kubectl apply -k .`, then
`kubectl -n amp-lru rollout restart deploy/small-temporal-omes`. A restart
rebuilds Omes, which takes a few minutes.

```bash
kubectl -n amp-lru scale deploy/small-temporal-omes --replicas=0   # stop the load, keep the data
kubectl delete -k .                                                  # remove the workload, keep the pool
kubectl -n amp-lru delete svc small-temporal-endpoint                # the alias, which delete -k does not own
```

Nothing here deletes the pool, cluster or endpoint. Do that on purpose through the
amp UI or API, and retire the portal row with `yb amp lru retire small_temporal --yes`.

## Health rules

`checker/check.py`, thresholds in one dict. Each rule is a PromQL query against
the shared Prometheus scoped to `lru="small_temporal"`, or one HTTP probe:

| Rule | Warn | Fail |
|---|---|---|
| temporal_reachable | | metrics port not answering |
| omes_up | | Omes pod not scraped |
| workflows_completing_per_s | below 0.2 | below 0.01 |
| workflow_failure_ratio | above 1 % | above 5 % |
| service_error_ratio | above 0.5 % | above 2 % |
| persistence_error_ratio | above 0.1 % | above 1 % |
| persistence_p99_s | above 50 ms | above 250 ms |
| amp_endpoint_resolved | | the controller has no endpoint for this pool |
| amp_endpoint_up | | postgres-exporter down |
| endpoint_commits_per_s | below 1 | below 0.1 |
| endpoint_deadlocks_total | recorded only | |
| omes_iterations_per_s | below 0.02 | 0 |

Two rules subtract expected noise: `service_error_ratio` ignores
`context_Canceled`, `serviceerror_Canceled`, `WorkflowExecutionAlreadyStarted`
and `NotFound`; `persistence_error_ratio` ignores
`CurrentWorkflowConditionFailedError`, which is Temporal's normal already-started
path. A rule that cannot run at all counts as a failure, not as absent:
`lru_overall_status` is the minimum over every rule including the unreadable
ones, because reporting green on no evidence is worse than reporting an error.

Thresholds are first guesses; `FINDINGS.md` tracks what the workload really
does.

## Look at it

- Dashboard: https://grafana-lru.tail5955.ts.net/d/lru-small-temporal
- Temporal UI: http://small-temporal.tail5955.ts.net
- amp UI: http://amp-ui.tail5955.ts.net/multitenancy?pool=lru-temporal&db=temporal

```bash
kubectl -n amp-lru get all -l lru=small_temporal
kubectl -n amp-lru logs deploy/small-temporal-omes -f
kubectl -n amp-lru exec deploy/small-temporal-server -- temporal operator cluster health --address small-temporal-frontend:7233
kubectl -n amp-dev exec deploy/yb-ep-<id> -c endpoint -- /usr/local/v18/bin/psql "postgresql://cloud_admin:cloud_admin@127.0.0.1:55433/temporal_lru"
```
