# LRUs: long-running universes on amp

An LRU is an environment that is never torn down, runs a real workload around the
clock on amp, and gets an hourly verdict on the report portal. This directory holds
every LRU and the one monitoring stack they share. The portal side (list, detail,
`yb amp lru`, MCP tools) lives in `yugabyte/qa-services`; the hourly sync job lives
in `yugabyte/meko-devops`.

```
amp/deploy/lru/
  README.md          this file: how every LRU is built and wired
  monitoring/        shared: namespace, one Prometheus, one Grafana. Applied once.
  small_temporal/    one LRU: Temporal server + Omes load, small rate
  <next>/            the next LRU, same shape
```

## The shape every LRU has

The same seven pieces, so the sync job, the portal and a person on call can treat
them alike. Nothing about a particular cluster or workload is hardcoded anywhere:
it is a field on the portal row, an env var with a default in `up.sh`, or a label.

1. **Its own storage pool on amp.** Pool `lru-<name>`, one cluster, branch `main`,
   one `read_write` endpoint sized for the workload. Created by the LRU's `up.sh`
   through the controller API, idempotently. The pool is what keeps one LRU's data
   and load accounting separate from another's on the same amp deployment.
2. **Its own pods, in the shared `amp-lru` namespace.** The workload server, its
   load generator, and anything else it needs, each a Deployment. Every resource
   name is prefixed with the LRU name (`small-temporal-server`,
   `small-temporal-omes`) and every object carries the label `lru: <name>`, so two
   LRUs never collide and `kubectl get all -l lru=<name>` is the whole thing.
3. **An `ExternalName` Service for its endpoint**, `<name>-endpoint`, which
   `up.sh` points at the `yb-ep-<id>` Service in `amp-dev`. Manifests refer to
   that name and never to an endpoint id.
4. **Metrics by annotation.** A pod that wants scraping sets
   `prometheus.io/scrape: "true"` and names its metrics ports `metrics` or
   `metrics-<x>`. The shared Prometheus discovers it and labels every series with
   `job` (pod label `app`), `lru` (pod label `lru`) and `component` (port name).
   No scrape config is edited when an LRU is added.
5. **No environment ids in tracked files.** An endpoint id changes whenever the
   endpoint is recreated. The checker and the dashboard resolve theirs from the
   controller by pool name instead of carrying it, so nothing rewrites a tracked
   file and a fresh checkout applies cleanly.
6. **A checker that turns metrics into a verdict.** A small pod that evaluates
   the LRU's health rules once a minute and exports `lru_check_status{lru,check}`
   (1 ok, 0.5 warn, 0 fail, -1 unreadable), `lru_check_value{lru,check}` and
   `lru_overall_status{lru}`, the minimum over every rule including the
   unreadable ones — a rule that cannot run is evidence of a problem, not
   evidence of health. The rules are the LRU's own; the contract is the
   metric names and the `lru` label, which is what the hourly sync reads.
   Check names carry their unit as a suffix (`_per_s`, `_ratio`, `_s`, `_bytes`,
   `_total`, `_up`) so the portal can format them.
7. **A dashboard as a ConfigMap.** `dashboard.json` shipped as a ConfigMap
   labelled `grafana_dashboard: "1"`. The shared Grafana's sidecar copies it in;
   no Grafana config changes. Every query is scoped with `lru="$lru"` where `$lru`
   is a hidden constant variable, so two LRUs of the same kind do not mix.

## The shared monitoring base

`monitoring/` is applied once and rarely changes:

- **Prometheus** at `http://prometheus:9090` in the namespace. Two scrape jobs:
  `lru` (pod discovery in `amp-lru` by the annotation above) and `amp-endpoint`
  (every amp endpoint pod in `amp-dev`, labelled `endpoint=<id>`, so any LRU's
  dashboard can show its endpoint's Postgres, pageserver and safekeeper). 30 days
  or 15 GB on a 20 GiB volume.
  Applied with `monitoring/up.sh`, which also generates the Grafana admin
  password into a Secret on first run and never rotates it afterwards.
- **Grafana** at `https://grafana-lru.tail5955.ts.net`, one for all LRUs, folder
  "LRU" with one dashboard per LRU. Anonymous viewer behind the tailnet, log in
  as `admin` with the password in the `grafana-admin` Secret to edit, HTTPS through a Tailscale Ingress so the report portal
  can embed dashboards in an iframe, gzip on. Dashboards are provisioned from
  ConfigMaps; a change made in the UI is lost, edit the JSON.

## How an LRU reaches the report portal

The portal knows an LRU as one row: name, kind, owner, and a `data` object with
everything about where it runs (`k8s`, `amp`, `foundation`, `verdict`,
`dashboard_url`, `links`, `access`, `checks_expected_every`). It stores that row
and one verdict per hour, nothing else. Metrics stay here.

The hourly Jenkins job `amp-lru-sync` lists active rows and, for each, reads the
verdict the way `data.verdict` says. For an LRU here that is
`{"type":"prometheus-kubectl","url":"http://prometheus:9090","exec_target":"deploy/<name>-omes","selector":"lru=\"<name>\""}`:
exec into one of the LRU's pods, query the shared Prometheus for
`lru_overall_status{selector}`, post `ok`, `not_ok` or `error` with the failing
rule names and the values. The portal shows the badge, the history, and the
dashboard in a frame. A row that gets no verdict for twice its expected interval
shows `stale`.

Registering, editing and retiring rows: `yb amp lru --help`, or the MCP tools
`list_lrus`, `get_lru`, `create_lru`, `update_lru`, `retire_lru`, `sync_lru`,
`report_lru_check` on `https://amp-qa.dev.yugabyte.com/mcp`.

## Adding an LRU

The `amp-lru` skill in this repo walks through it. In short: copy
`small_temporal/`, rename every resource and label, replace the workload
manifests, write `checker/check.py` rules for it, write `dashboard.json` with
`lru="$lru"` in every query, run `up.sh`, register the row with
`yb amp lru create`, and the next hourly sync picks it up.

## Ground rules

- Never put credentials in a manifest or a portal row. Point at 1Password.
- The `amp-lru` namespace and every `lru-*` pool are not test leftovers. Do not
  reap them as cleanup.
- One LRU is one directory. Shared changes go in `monitoring/` and get applied to
  every LRU at once, so keep them rare.
