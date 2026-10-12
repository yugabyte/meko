# Talk track: `ycp_demo.py show --pause`

About 3 minutes. `--pause` waits for Enter after each act, so you set the pace.
Have two windows: the terminal (big font, dark theme) and the YBM console on the
`demo-shop` cluster page.

## Before you go on

```bash
cd ycp/demo/branching
python3 ycp_demo.py stage         # opens the split screen; leave it running
```

Click **preflight** on the stage (every line ok) and **⟳ YCP auto-refresh** on the
console window.

Run one `show --no-agent` beforehand: it pulls the endpoint image onto the nodes,
which takes about 20 s off the live run.

## The acts

**Opening.** "This is a real amp environment, running in a customer's Kubernetes
cluster, managed from our cloud console through YCP. Nothing on screen is mocked."

Point at the bottom right: "REJ Outdoors, an outdoor-gear co-op: 50,000
customers, 5 million orders, 1.5 GB. This is Aman's My Orders page. Watch the
badge: it takes about half a second, every time she looks."

**Act 1: The problem.** A customer-history query takes ~90 ms because it reads all 5
million rows to return 20.
> "Every DBA has three guesses here. Normally you try them one at a time, on
> staging, and hope staging looks like prod."

**Act 2: Ask the agent.** Claude reads the real plan and writes one fix per
hypothesis. Read the three out.
> "An agent's guesses are only safe to try if being wrong costs nothing. So we
> don't let it near main."

**Act 3: Fork.** One API call. Point at the LSN: all copies fork from the same
point, in about 4 s, with no data copied. Switch to the console and click
**Refresh**: the branches are there.
> "That's three full copies of a 5-million-row table. Nothing was copied, and each
> copy gets its own compute."

Compute takes about 30 s. Use that time to explain copy-on-write branching, or
show the console.

**Act 4: The race.** All three fixes run at once, each on its own copy. The index
wins by three orders of magnitude; the other two do nothing.
> "The agent was wrong twice, and it didn't matter."

**Act 5: Clean up.** The copies disappear in seconds.
> "No staging environment to tear down, and main still hasn't been touched."

**Act 6: Ship it.** Only the winning fix is applied to main, and the same query
drops from ~90 ms to ~0.05 ms on main itself.
> "The agent had three ideas. It tested all of them on copies, and only the one
> that was proven reached production."

Point at Aman's page: within 5 s its badge turns from 🐢 ~490 ms to ⚡ ~15 ms
(the rest is the VPN round trip from this laptop).

The next `show` puts main back to slow on its own; `--no-ship` skips this act.

## Encore: `ycp_demo.py scale --pause`

About a minute. A revenue report runs back to back on main through **one held
session**, and its latency is drawn live.

**Act 1: Under load.** About 880 ms per run at 0.5 vCPU.

**Act 2: Diagnose.** The endpoint's CPU metrics show it throttled in 100% of CPU periods,
and the plan has no disk reads and no spill. Claude concludes it is CPU-bound and
picks a size.
> "It isn't guessing. It's reading the same counters an SRE would."

**Act 3: Scale up, live.** One API call. The pod is resized in place about 2 s
later and the latency line drops mid-stream (~880 → ~420 ms at 1 vCPU).
> "Same session, same PID, zero errors. Postgres didn't restart."

**Act 4: Quiet hours.** The load stops, usage falls to about 1% of a core, and
Claude scales down to 0.25 vCPU. The held session still answers.
> "It gives the capacity back when nobody's using it."

Don't promise memory scaling. A live resize changes the pod's limits, but a
running Postgres can't use new memory: `shared_buffers` is fixed at start and
nothing re-reads the size. Memory rises with CPU only because amp requires 8 GiB
per vCPU, and it never drops below 4Gi, where `shared_buffers` already sits.

## If someone asks "isn't this what Neon does?"

Partly. Neon's MCP server has a query-tuning tool that tests an index on one
temporary branch, and Neon publishes guides for giving parallel coding agents a
branch each. What this adds: competing hypotheses raced side by side, each on its
own branch, driven through a control plane into the customer's own cluster, with
the storage on YugabyteDB.

## If something goes wrong

| Symptom | What to do |
|---|---|
| Claude takes over ~60 s or answers badly | The show falls back to the rehearsed hypotheses and says so. Carry on. |
| Compute stays "starting" past 60 s | The image is being pulled onto a new node. Talk through it; the next run is faster. |
| A fix fails | It shows as `fix failed` on that copy. That's the point: it failed on a copy. |
| Ctrl-C | The copies are still dropped. |
| A write hangs at the start | The tserver Service has lost `ClientIP` ([#1493](https://github.com/yugabyte/meko/issues/1493)). Preflight prints the fix. |
