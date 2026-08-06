# ampqa TODO (local scratchpad — not tracked in git)

## Measure how long AMP operations take (operation-latency tracking)

Source: Hari, 2026-08-06 — "endpoint creation is taking too long. How do I debug
this?" (https://yugabyte.slack.com/archives/C0BE0G1N9ND/p1786041632135389?thread_ts=1785997064.161129&cid=C0BE0G1N9ND)

Today an operation being slow is an anecdote someone notices on the dev cluster.
Nothing measures it, so there is no answer to "since when", "how slow is normal",
or "which build regressed it".

Idea: a perf-tier test (e.g. `perf/ops_latency`) that times each control-plane
operation end-to-end against a fresh stack and reports every duration as a gated
datapoint, so operation latency gets the same portal history / baselines /
per-version averages the benchmarks already have:

- project create / delete
- branch create (from empty parent), branch fork (from a seeded parent — size the
  seed so copy-vs-CoW behaviour is visible), branch delete
- endpoint create (cold: image pull + volume; warm: repeat on the same node),
  endpoint ready-to-serve (create → first successful SQL), endpoint delete
- promote/upgrade path timings where reachable (rolling update of a standing
  deployment is meko-devops territory — maybe just record what the controller
  reports)

Measure at two layers when they differ: the API call duration (what the caller
waits on the synchronous call) vs time-to-actually-usable (task finished /
endpoint answers SQL) — Hari's complaint is the second one.

Building blocks already there:
- datapoints + baselines + portal perf history (`core.Datapoint`, gate helpers in
  suites/perf)
- the metrics sampler (apps/metrics, meko#623) can run during the ops loop so a
  slow create correlates with what the store/pods were doing
- steps are already timed per test — but step durations are not datapoints, so
  they have no history/gating; the ops test should emit real datapoints

Open questions:
- run it N times per operation and report median + CV (single-shot ops are noisy
  — image pull cache state dominates cold create)
- where the controller itself records per-task durations (amp_registry tasks table
  has timestamps?) — if the product already knows, the test should read and report
  the product's own numbers too, not only observe from outside
- alerting: a gated baseline on median create time is the cheap version of "tell
  me when it regresses"
