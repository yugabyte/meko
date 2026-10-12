# Branching demo on YCP

Seeds a database with a slow query, forks one branch per hypothesis in a single
desired-state write, tests each fix on its own branch in parallel, prints the
comparison, and drops the branches. Every change goes through the YBM public API
(YCP desired state), so the console shows it happening.

```bash
python3 ycp_demo.py preflight   # every check must say ok
python3 ycp_demo.py setup       # once: cluster, main, 0.5-core writer, the store (~6 min at 2 vCPU)
python3 ycp_demo.py stage       # split screen in Chrome: YCP console left, terminal + store app right
python3 ycp_demo.py show --pause   # the presentation (see TALK_TRACK.md)
python3 ycp_demo.py scale --pause  # encore: Claude resizes main live, up under load, down when idle
python3 ycp_demo.py extensions --pause  # try pgvector on a copy of main; main never gets it
python3 ycp_demo.py run         # the same steps, plain output
python3 ycp_demo.py reset       # make main slow again (show does this itself)
python3 ycp_demo.py teardown    # when done
```

`show` asks Claude (the `claude` CLI) to read main's plan and propose the fixes,
and falls back to rehearsed ones if it can't. `--no-agent` skips Claude entirely.

## The store

**REJ Outdoors**, a fictional outdoor-gear co-op (a nod to REI), about 1.5 GB:

| Table | Rows | What it holds |
|---|---|---|
| `customers` | 50,000 | names, emails, 24 cities in 14 countries |
| `products` | 2,000 | SKUs across Outerwear, Footwear, Packs, Camping, Climbing, Accessories |
| `orders` | 5,000,000 | two years of orders, mostly delivered; totals match their lines |
| `order_items` | ~10,000,000 | 1–3 lines per order |

Customer 4242 is **Aman Nijhawan** from Seattle. His "My Orders" page (`app.py`) runs
`ycp_demo.QUERY`, which is slow because `orders` has no index on `customer_id`. The
resize show runs finance's revenue-by-category report over the latest 1M orders.
`DEMO_ORDERS` changes the size before `setup`.

## The stage

`stage` opens two Chrome windows, each half the screen. The console can't be
embedded in a page (it sends `X-Frame-Options: SAMEORIGIN`), hence two windows.

- **Left:** the YCP console on `demo-shop`. The stage presses the console's own
  Refresh every 5 s, which needs Chrome's **View > Developer > Allow JavaScript from
  Apple Events** (turn it off after the demo). Without it, drag **⟳ YCP
  auto-refresh** from the stage's top bar to the bookmarks bar and click it on the
  console window.
- **Right:** a real terminal running the shows (buttons in the top bar; Enter
  advances an act), with Aman's My Orders page underneath. It refreshes every 5 s,
  so it turns from 🐢 to ⚡ when the fix ships.

## One-time setup

- **VPN on.** Debug endpoints have no load balancer and are reached by pod IP.
- **`gcloud auth login`**, for `kubectl` on itest-dev: the script looks up pod IPs
  and preflight reads two cluster settings.
- **`psql`** on `PATH`.
- **Secrets in `~/.ycp-demo/` (mode 700), never in this directory:**
  - `api_key`: a YBM API key for the account (or export `YCP_API_KEY`).
  - `pg_password`: the `cloud_admin` password. `setup` generates it when absent;
    an existing cluster needs the password it was created with.

Defaults target BYOC `itest-dev-portal-2` / `test-pool/demo-shop`; override any of
`CONFIG` in `ycp_demo.py` with an environment variable of the same name.
`DEMO_PROFILE` switches to an AMP Managed account on devcloud, which takes its own
API key and password from `~/.ycp-demo/managed/`:

- `managed-east`: the region `gurushant-dev-1` serves, on devcloud's infra cluster.
  Needs `tsh login` to devcloud Teleport; endpoints are reached by pod IP over the VPN.
- `managed`: us-west1, which is not reachable from the VPN, so endpoints get a
  public load balancer open to this machine only.

## What preflight checks, and why

| Check | Without it |
|---|---|
| tserver Service has `sessionAffinity: ClientIP` | Commits hang on `SyncRep` for minutes, then reads fail with `Read error` ([#1493](https://github.com/yugabyte/meko/issues/1493)). A redeploy of the universe drops the setting. |
| `RECONCILE_INTERVAL` set below 10s | A fork takes two ticks to show. Use `2s`: shorter multiplies duplicate delete tasks ([#1744](https://github.com/yugabyte/meko/issues/1744)). |
| A write on `main` commits | Catches both of the above, and a down VPN. |

Preflight prints the fix for each failure. Both cluster settings are hand edits
that the next `helm upgrade` or universe redeploy reverts, so run preflight on the
day of the demo.

## Expected timings (2 s tick, 0.5 vCPU, the 1.5 GB store)

| Step | Time |
|---|---|
| Claude proposes the fixes | ~10 s |
| Fork 3 full copies of the store | ~5–7 s |
| 3 endpoints ready | ~30–50 s (image pull ~23 s on a node without it, disk ~6 s, Postgres ~5 s) |
| Drop 3 copies | ~5–8 s |
| Ship the index to main | ~20–25 s at 0.5 vCPU (CPU-bound; ~4 s at 2 vCPU) |
| Whole `show` | ~1.5 min without pauses |
| Resize lands after the YCP write (`scale`) | ~2–3 s |
| Enable pgvector on the copy (`extensions`) | the copy's compute redeploys, ~30–60 s |

Each run also leaves a few failed duplicate tasks in the environment's task
history; the operations themselves succeed ([#1744](https://github.com/yugabyte/meko/issues/1744)).

## AMP dashboards in Grafana

`grafana/adapt_amp_dashboards.py OUT_DIR` rewrites the `amp-monitoring` chart's
dashboards for a Grafana that holds many BYOC environments in one Prometheus: it
adds Account and Environment variables and filters every query by
`environment_id`. Import the output through Grafana's dashboard API.

## For the audience

- The YBM console polls every 30 s once nothing looks in flight, and not at all in a
  background tab. Keep it in front and click **Refresh** after each step.
- The planted problems are a missing index (the real one, about 300× faster),
  stale statistics and a tiny `work_mem` (decoys).
- Every endpoint password in the environment is readable through `GET /desired`
  ([#1661](https://github.com/yugabyte/meko/issues/1661)). Use an API key you can revoke.
