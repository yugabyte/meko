# Temporal LRU findings

Phase 0 log. Observed facts, dated, newest at the bottom. Inferences are marked.

## 2026-09-03 bring-up

- `up.sh` created pool `lru-temporal`, cluster `temporal` (PG 18), endpoint
  `a310d544` at 2 vCPU / 16 GiB in under 2 minutes. Endpoint pod ready on first try.
- Temporal `auto-setup` 1.29.7 created `temporal_lru` and `temporal_lru_visibility`
  and ran both schema tools as `cloud_admin` on the first boot. 39 tables, 40
  connections open. No amp-specific error during schema setup.
- Three deploy fixes needed, all on the Temporal side, none on amp: dynamic config
  file missing from the image (ConfigMap added), Nexus callback template and
  `component.callbacks.allowedAddresses` unset (every Nexus operation failed until
  set), `enableServiceLinks` collision crashing the UI.
- Omes `throughput_stress`, 4 concurrent, 2 iterations/s: in the first 4 minutes of a
  clean cycle 1262 workflows, 878 completed, 12 failed. Endpoint pod 650 mCPU /
  810 MiB, Temporal pod 530 mCPU / 395 MiB, Omes 110 mCPU.
- Temporal logged 27 `Nexus StartOperation request failed: context deadline exceeded`
  in those 4 minutes, all against its own HTTP port 7243. Inferred: single-process
  Temporal under load, not the database. Watch whether the rate grows.
- `Queue reader unable to retrieve tasks: shard status unknown` 5 times at startup
  only. Inferred: shard acquisition during boot.

## 2026-09-03 monitoring up

- Prometheus, Grafana (`http://grafana-lru.tail5955.ts.net`) and the checker run in
  `amp-lru`. 27 amp endpoint scrape targets in `amp-dev` (9 endpoints x 3 exporters).
- After 20 minutes at 4 concurrent / 2 iterations per s: endpoint commits about
  490/s on `temporal_lru`, persistence p99 16 ms, persistence error ratio 0.03 %
  (all `CurrentWorkflowConditionFailedError`, which is Temporal's expected
  already-started path), workflow failure ratio 0, service error ratio 0,
  0.73 kitchen-sink workflows completing per s. No deadlocks.
- Omes SDK client metrics (`temporal_request*`) come out with scrambled label
  values (operation, namespace, task_queue rotate between series). Inferred: an
  Omes or Go SDK label ordering bug. The dashboard uses server-side metrics instead.

## 2026-09-03 dashboard load time

- Cold visit pulled **8.3 MB** of Grafana boot assets uncompressed; the two big
  chunks alone took 5.4 s and 4.6 s over the tailnet. `GF_SERVER_ENABLE_GZIP` is
  off by default. With it on the same eight assets are **2.5 MB** and the worst
  chunk drops from 5.4 s to 1.05 s.
- Warm load was already fine and stays so: Grafana boots in 0.47 s, the first
  panel query fires at 0.85 s, all seven above-the-fold panels finish at 2.2 s.
  Panel queries run 0.7 s median, 1.2 s worst.
- Assets carry `cache-control: max-age=31536000`, so the 8 MB was a first-visit
  cost only, which is exactly when someone forms an opinion of the page.
- Server side is idle: Grafana 6 mCPU / 116 MiB against a 500 mCPU / 512 MiB
  limit, Prometheus 11 mCPU / 192 MiB against 1 core / 2 GiB, 49k series. Tailnet
  RTT from a laptop is about 90 ms.

## 2026-09-03 restructure

- Directory renamed to `small_temporal`; every resource is now `small-temporal-*`
  and labelled `lru=small_temporal`. Prometheus and Grafana moved to the shared
  `../monitoring` base and discover LRUs by pod annotation and dashboard
  ConfigMap. Same Grafana URL. The Temporal memory limit went from 4 to 8 GiB,
  since the pod sat at 99 % of 4 GiB, the shape of the two earlier OOMs.
- Omes rebuilt on the new pod as expected; the portal verdict came back `ok`
  through the new `lru="small_temporal"` selector on the first sync.

## 2026-09-04 review fixes

- `lru_overall_status` counted only rules that ran. A rule whose query returned
  nothing became ERR and dropped out of the minimum, so an unreadable endpoint
  could read as green. It is now the minimum over every rule.
- The checker resolves its endpoint from the controller by pool instead of taking
  a configured id, which removes the sed that rewrote a tracked file and the
  leading-comma PromQL error when the id was unset. New rule
  `amp_endpoint_resolved` reports when the pool has no endpoint.
- Prometheus RBAC narrowed from a cluster-wide pods read to a namespace Role in
  each of the two namespaces it discovers. Grafana's admin password moved from a
  manifest constant to a generated Secret.
- Checked against the bot's other two claims: the `sed` on `kustomization.yaml`
  was idempotent (removed anyway), and the endpoint's pull secret is
  `quay-pull-secret`, a long-lived quay credential from 2026-07-08, not a ~12 h
  ECR token.

## 2026-09-04 the endpoint alias was never patched

The LRU was down for its whole first day: Temporal could not acquire a single
shard and Omes died on every cycle, both on
`lookup small-temporal-endpoint ... no such host`. All four pods stayed `Running`,
so only the portal verdict showed it.

`svc/small-temporal-endpoint` still carried the `placeholder.amp-dev.svc.cluster.local`
that `endpoint.yaml` shipped through kustomize. `up.sh` guarded its patch on
`get svc amp-endpoint`, a different service that the else branch was supposed to
create. It is not in the namespace today, so the guard never passed and the alias
the manifests actually resolve was never touched.

The deeper problem was that the alias was a kustomize resource at all. A tracked
manifest can only carry a placeholder, so every `kubectl apply -k .` — a path
`README.md` documents for changing an Omes knob — reset the alias and would have
reproduced this outage even with the guard fixed. `up.sh` now applies the alias
imperatively with the resolved externalName and it is no longer a kustomize
resource. Nothing detects the failure on its own: the rollout status passes because
the pod is Running, and the checker resolves the endpoint from the controller API
rather than through the alias, so `amp_endpoint_resolved` stays green.

The amp side was healthy throughout: pool `lru-temporal`, endpoint
`a310d544-39a4-45c5-bfaf-b5f6d8c0b592`, its deployment 1/1 the whole time.

## Open

- 12 failed workflows per 4 minutes: confirm they are the Nexus deadline cases and not
  persistence errors.
- Endpoint memory curve over 24 h at this rate.
- `amp_storage.pages` and `wals` growth for branch `ae980ead-a32c-4404-8188-11c1a1424d86`.
- Mark's Omes command line on `vmload2a`, to align scenario and rate.
