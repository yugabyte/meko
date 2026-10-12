"""The resize presentation: `ycp_demo.py scale`.

A query loop runs on main through ONE held session the whole time. Claude reads the
endpoint's cgroup and decides; every resize is a YCP desired-state write that amp
applies in place (Kubernetes pods/resize: no restart, the session survives).

  1. Under load   -- a CPU-bound report on a 0.5 vCPU endpoint, latency live
  2. Diagnose     -- Claude reads the plan and the CPU metrics: throttled, not I/O-bound
  3. Scale up     -- live; the latency line drops mid-stream, same session
  4. Quiet        -- the load stops; Claude sees an idle endpoint and scales it down

Only CPU is scaled for speed. A running Postgres cannot use memory it is given live:
shared_buffers is fixed at start and nothing re-reads the new size, so memory only
moves with CPU (amp's floor is 8 GiB per vCPU) and never below 4Gi, where
shared_buffers already sits at 2 GB.
"""

import json
import os
import re
import shutil
import subprocess
import threading
import time

import show as S
import ycp_demo as d

from show import bold, cyan, dim, green, red, yellow

E = f"{d.CLUSTER}/main/rw"
START_CPU = "0.5"
# The store's revenue-by-category report over the latest 1M orders (~2M order lines):
# a primary-key range scan, then pure CPU.
QUERY = (
    "SELECT p.category, count(*) AS units, sum(i.qty * i.unit_price) AS revenue "
    "FROM order_items i JOIN products p ON p.id = i.product_id "
    f"WHERE i.order_id > {d.ORDERS - 1_000_000} GROUP BY p.category ORDER BY revenue DESC"
)
TOP_MS = 3000
SIZES = ["0.25", "0.5", "1", "2"]
TICKS = "▁▂▃▄▅▆▇█"


def gi(q):
    """A Kubernetes memory quantity in GiB."""
    m = re.match(r"^(\d+(?:\.\d+)?)(Gi|Mi|G|M)?$", str(q or "0"))
    if not m:
        return 0
    n, unit = float(m.group(1)), m.group(2)
    return {"Gi": n, "Mi": n / 1024, "G": n * 1e9 / 2**30, "M": n * 1e6 / 2**30}.get(
        unit, n / 2**30
    )


def memory_for(cpu, live_gi=0):
    # Memory only ever grows. Kubernetes refuses to set a limit below current usage, and
    # page cache counts, so a shrink after a heavy query sticks in ResizeError while YCP's
    # inventory reports the size that was asked for. amp's floor is 8 GiB per vCPU.
    return f"{max(4, round(float(cpu) * 8), int(-(-live_gi // 1)))}Gi"


# ------------------------------------------------------------------ the endpoint


def attrs():
    return d.inventory()[f"endpoint/{E}"]["attrs"]


def resize(cpu):
    mem = memory_for(cpu, live_memory_gi())
    d.update(E, resources={"cpu": cpu, "memory": mem})
    return mem


_ADDR = None


def addr():
    global _ADDR
    if _ADDR is None:
        _ADDR, _ = d.main_endpoint()
    return _ADDR


def cgroup():
    """The endpoint container's live cgroup, read through Postgres (cloud_admin is a
    superuser in the endpoint, and Postgres runs in that container). No kubectl, so the
    same code works on a managed account."""
    files = {"max": "cpu.max", "stat": "cpu.stat", "mem": "memory.max"}
    sql = (
        "SELECT json_build_object("
        + ", ".join(f"'{k}', pg_read_file('/sys/fs/cgroup/{f}')" for k, f in files.items())
        + ")"
    )
    return json.loads(d.psql(addr(), sql))


def live_cpu():
    quota, period = cgroup()["max"].split()
    return float("inf") if quota == "max" else int(quota) / int(period)


def live_memory_gi():
    """The container's actual memory limit, which can differ from what YCP last asked for."""
    v = cgroup()["mem"].strip()
    return 0 if v == "max" else int(v) / 2**30


def cpu_stat():
    return {k: int(v) for k, v in (l.split() for l in cgroup()["stat"].splitlines() if l.strip())}


# ------------------------------------------------------------------ the load


class Load:
    """One psql session running QUERY back to back; records (time, ms)."""

    def __init__(self, addr):
        host, port = addr.rsplit(":", 1)
        self.p = subprocess.Popen(
            [
                shutil.which("psql") or "psql",
                f"host={host} port={port} user=cloud_admin dbname=postgres sslmode=disable",
                "-AtXq",
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1,
            env={**os.environ, "PGPASSWORD": d.pg_password()},
        )
        self.lock = threading.Lock()
        self.samples, self.running, self.errors = [], False, 0
        self.pid = self.ask("SELECT pg_backend_pid()").strip()
        self.ask("SET work_mem = '64MB'")  # keep it CPU-bound: nothing spills to disk
        self.ask("\\timing on")

    def ask(self, sql):
        """Send one statement on the held session; return its last output line."""
        # psql runs SQL only at ';', but a backslash command at once: without the
        # terminator the marker would come back before the query ran.
        if not sql.startswith("\\") and not sql.rstrip().endswith(";"):
            sql += ";"
        with self.lock:
            self.p.stdin.write(sql + "\n" + "\\echo __done__\n")
            self.p.stdin.flush()
            lines = []
            for line in self.p.stdout:
                line = line.rstrip("\n")
                if line == "__done__":
                    break
                lines.append(line)
        # With \timing on the last line is "Time: ..."; callers wanting the row ask for it.
        return "\n".join(lines)

    def start(self):
        self.running = True
        threading.Thread(target=self.loop, daemon=True).start()

    def loop(self):
        while self.running:
            try:
                out = self.ask(QUERY)
                m = re.search(r"Time: ([\d.]+) ms", out)
                if m:
                    self.samples.append((time.time(), float(m.group(1))))
                else:
                    self.errors += 1
                    time.sleep(0.5)
            except Exception:  # noqa: BLE001 -- counted; the chart shows a gap
                self.errors += 1
                time.sleep(0.5)

    def stop(self):
        self.running = False
        time.sleep(1.5)

    def last_ms(self, window=3):
        recent = [ms for _, ms in self.samples[-window:]]
        return sum(recent) / len(recent) if recent else None

    def qps(self, seconds=5):
        now = time.time()
        return sum(1 for t, _ in self.samples if now - t <= seconds) / seconds


def chart(load, cpu, events, top):
    """A rolling latency line: one bar per query, newest on the right."""
    width = S.W - 34
    xs = load.samples[-width:]
    line = "".join(TICKS[min(7, int(ms / top * 7.99))] for _, ms in xs)
    cur = load.last_ms()
    color = green if cur and cur < top * 0.5 else red
    out = [
        f"  {dim('latency')}  {color(line)}{' ' * (width - len(xs))}  "
        f"{color(bold(f'{cur:6.0f} ms')) if cur else dim('   ... ms')}",
        f"  {dim('size')}     {bold(cpu + ' vCPU')}  {dim('·')}  {load.qps():.1f} queries/s  {dim('·')}  "
        f"{dim('one session, pid')} {load.pid}  {dim('·')}  errors {load.errors}",
    ]
    out += [f"  {dim('·')} {e}" for e in events[-3:]]
    return out


def watch(load, cpu, events, top, until, extra=lambda: []):
    board = S.Board()
    while not until():
        board.draw(chart(load, cpu(), events, top) + extra())
        time.sleep(0.2)
    board.draw(chart(load, cpu(), events, top) + extra())


def for_seconds(n):
    t_end = time.time() + n
    return lambda: time.time() >= t_end


# ------------------------------------------------------------------ the agent

PROMPT = """You are an autoscaler for a Postgres compute endpoint. Decide whether to
resize its CPU. Allowed sizes (vCPU): {sizes}. Current size: {cpu} vCPU.
Resizing is live: no restart, sessions survive. Memory follows CPU automatically
and cannot help a running query (shared_buffers is fixed at start), so reason
about CPU only. Scale up when the workload is CPU-starved; scale down when the
endpoint is idle, to save cost; otherwise keep the size.
Reply with ONLY JSON: {{"action":"scale_up|scale_down|keep","cpu":"<allowed size>","reason":"max 70 chars"}}

{evidence}"""


def decide(evidence, cpu, use_agent):
    """Ask Claude, showing the prompt as sent and its answer as written."""
    if use_agent and shutil.which("claude"):
        S.show_prompt(
            PROMPT.split("{evidence}")[0].format(sizes=", ".join(SIZES), cpu=cpu), evidence
        )
        print()
        try:
            (v, raw), secs = S.spinner_call(
                "Claude is deciding",
                lambda: S.claude(PROMPT.format(sizes=", ".join(SIZES), cpu=cpu, evidence=evidence)),
            )
            S.show_answer(raw, secs)
            if v.get("cpu") in SIZES:
                return v, "Claude"
        except Exception:  # noqa: BLE001 -- fall through to the rule
            pass
    return None, "rule"


def rule(throttled, used, cpu, active):
    if throttled > 0.2:
        return {
            "action": "scale_up",
            "cpu": "2",
            "reason": f"throttled in {throttled:.0%} of CPU periods",
        }
    if active == 0 and used < 0.05 * float(cpu):
        return {
            "action": "scale_down",
            "cpu": "0.25",
            "reason": "idle: no active queries, CPU near zero",
        }
    return {"action": "keep", "cpu": cpu, "reason": "within its size"}


def measure(seconds):
    a, t0 = cpu_stat(), time.time()
    time.sleep(seconds)
    b, dt = cpu_stat(), time.time() - t0
    periods = max(1, b["nr_periods"] - a["nr_periods"])
    return (
        (b["nr_throttled"] - a["nr_throttled"]) / periods,
        (b["usage_usec"] - a["usage_usec"]) / dt / 1e6,
    )


# ------------------------------------------------------------------ the show


def scale(args):
    S.PAUSE = "--pause" in args
    use_agent = "--no-agent" not in args
    addr, _ = d.main_endpoint()
    print("\n  " + bold(S.magenta("Right-size the database while it runs.")))
    print("  " + dim("Every resize is a YCP desired-state write; amp applies it in place."))

    if attrs().get("cpu") != START_CPU:
        _, secs = S.spinner_call(
            f"setting main to {START_CPU} vCPU for the start",
            lambda: (resize(START_CPU), _wait_cpu(START_CPU)),
        )
    up_since = d.psql(addr, "SELECT to_char(pg_postmaster_start_time(), 'HH24:MI:SS')")
    load = Load(addr)
    events, cur = [], {"cpu": START_CPU}
    t_show = time.time()

    try:
        # ---- act 1
        S.act(
            1,
            "Under load",
            f"Month end at {d.STORE}: finance runs revenue by category, back to back, "
            "on one session, at 0.5 vCPU.",
        )
        S.typeout("  " + cyan(QUERY))
        print()
        load.start()
        watch(load, lambda: cur["cpu"], events, TOP_MS, for_seconds(10))
        slow = load.last_ms(5)
        if slow is None:
            d.die(f"the report never completed on main ({load.errors} errors)")
        S.beat()

        # ---- act 2
        S.act(
            2,
            "Diagnose",
            "Claude reads the query plan and the endpoint's CPU metrics while the load runs.",
        )
        box = {}
        th = threading.Thread(target=lambda: box.update(m=measure(5)), daemon=True)
        th.start()
        watch(load, lambda: cur["cpu"], events, TOP_MS, lambda: not th.is_alive())
        throttled, used = box["m"]
        plan = d.psql(addr, ["SET work_mem = '64MB'", f"EXPLAIN (ANALYZE, BUFFERS) {QUERY}"])
        io = "no disk reads" if "read=" not in plan else "some disk reads"
        spill = "no spill" if "Disk:" not in plan else "spills to disk"
        cached = "read=" not in plan
        bound = cached and "Disk:" not in plan
        workers = int((re.search(r"Workers Launched: (\d+)", plan) or [0, 0])[1])
        evidence = (
            f"CPU metrics over 5s: throttled in {throttled:.0%} of CPU periods; using {used:.2f} of "
            f"{cur['cpu']} vCPU.\nActive queries: 1 (back to back), each using {1 + workers} processes "
            f"(leader + {workers} parallel worker{'s' if workers != 1 else ''}).\n"
            f"Plan: {io}, {spill}{', all buffers hit' if cached else ''}.\n"
            f"Latency: {slow:.0f} ms per run.\n\n{plan}"
        )
        print(
            f"  {dim('metrics:')} throttled in {bold(red(f'{throttled:.0%}'))} of CPU periods  {dim('·')}  "
            f"using {used:.2f} of {cur['cpu']} vCPU"
        )
        print(
            f"  {dim('plan:')}   {io}, {spill}{', every page in cache' if cached else ''}, "
            f"{1 + workers} process{'es' if workers else ''} per query  "
            f"{dim('→ CPU-bound') if bound else ''}"
        )
        v, who = decide(evidence, cur["cpu"], use_agent)
        if not v:
            v, who = (
                rule(throttled, used, cur["cpu"], 1),
                "rule (agent unavailable)" if use_agent else "rule",
            )
        print(
            f"  {dim(who + ':')} {bold(v['action'].replace('_', ' '))} → {bold(v['cpu'] + ' vCPU')}  "
            f"{dim('— ' + v['reason'])}"
        )
        S.beat()

        # ---- act 3
        up = v["cpu"] if v["action"] == "scale_up" else "2"
        if v["action"] != "scale_up":
            print(
                f"  {dim(f'(not the call made above: scaling to {up} vCPU anyway, to show a live resize)')}"
            )
        S.act(3, "Scale up, live", f"One API call: {cur['cpu']} → {up} vCPU. Watch the line.")
        t_up = time.time()
        resize(up)
        events.append(f"+{0:4.1f}s  YCP write: cpu {cur['cpu']} → {up}")
        landed = {}

        def lands():
            if "t" not in landed and abs(live_cpu() - float(up)) < 1e-6:
                landed["t"] = time.time() - t_up
                cur["cpu"] = up
                events.append(f"+{landed['t']:4.1f}s  pod resized in place  {dim('(no restart)')}")
            if "t" in landed and "ycp" not in landed and ycp_reports(up):
                landed["ycp"] = time.time() - t_up
                events.append(
                    f"+{landed['ycp']:4.1f}s  YCP reports {up} vCPU  {dim('(the console shows it)')}"
                )
            # Done once the console can show it too, and the line has had time to drop.
            return "ycp" in landed and time.time() - t_up > landed["t"] + 6

        watch(load, lambda: cur["cpu"], events, TOP_MS, lambda: lands() or time.time() - t_up > 180)
        fast = load.last_ms(5)
        same = d.psql(addr, "SELECT to_char(pg_postmaster_start_time(), 'HH24:MI:SS')") == up_since
        print(
            f"\n  {green('✔')} {slow:.0f} ms → {bold(green(f'{fast:.0f} ms'))} "
            f"({slow / fast:.1f}× faster) {landed.get('t', 0):.0f}s after the call, "
            f"in YCP after {landed.get('ycp', 0):.0f}s  {dim('·')}  "
            f"{'Postgres up since ' + up_since + ', never restarted' if same else red('Postgres restarted')}  "
            f"{dim('·')}  session pid {load.pid}, {load.errors} errors"
        )
        S.beat()

        # ---- act 4
        S.act(4, "Quiet hours", "The report finishes. Claude checks again.")
        load.stop()
        events.append(f"+{time.time() - t_up:4.1f}s  load stopped")
        th = threading.Thread(target=lambda: box.update(m=measure(6)), daemon=True)
        th.start()
        S.spinner_call("watching the idle endpoint", th.join)
        throttled, used = box["m"]
        active = int(
            d.psql(
                addr,
                "SELECT count(*) FROM pg_stat_activity WHERE state = 'active' "
                "AND backend_type = 'client backend' AND pid <> pg_backend_pid()",
            )
        )
        print(
            f"  {dim('metrics:')} using {bold(f'{used:.3f}')} of {cur['cpu']} vCPU  {dim('·')}  "
            f"active queries: {active}"
        )
        evidence = (
            f"CPU metrics over 6s: using {used:.3f} of {cur['cpu']} vCPU, throttled in {throttled:.0%} "
            f"of periods.\nActive queries: {active}. Connections are idle."
        )
        v, who = decide(evidence, cur["cpu"], use_agent)
        if not v:
            v, who = (
                rule(throttled, used, cur["cpu"], active),
                "rule (agent unavailable)" if use_agent else "rule",
            )
        print(
            f"  {dim(who + ':')} {bold(v['action'].replace('_', ' '))} → {bold(v['cpu'] + ' vCPU')}  "
            f"{dim('— ' + v['reason'])}"
        )
        down = v["cpu"] if v["action"] == "scale_down" else None
        if down:
            t_down = time.time()
            _, secs = S.spinner_call(
                f"scaling down to {down} vCPU, live", lambda: (resize(down), _wait_live_cpu(down))
            )
            cur["cpu"] = down
            _, ycp_s = S.spinner_call("waiting for YCP to report it", lambda: _wait_ycp(down))
            alive = load.ask("SELECT 'still here'").splitlines()[0]
            print(
                f"  {green('✔')} {up} → {bold(down + ' vCPU')} in {secs:.0f}s, in YCP after {secs + ycp_s:.0f}s  "
                f"{dim('·')}  "
                f"freed {float(up) - float(down):g} vCPU  {dim('·')}  "
                f"the same session answers: {bold(alive)}"
            )
    finally:
        load.running = False
        try:
            load.p.stdin.close()
        except Exception:  # noqa: BLE001
            pass

    card = [
        f"{START_CPU} → {up} vCPU live: {slow:.0f} ms → {fast:.0f} ms ({slow / fast:.1f}× faster), "
        f"{landed.get('t', 0):.0f}s after the call",
        (
            f"{up} → {down} vCPU when idle: {float(up) - float(down):g} vCPU freed"
            if down
            else "kept its size"
        ),
        f"one session the whole time · Postgres never restarted · {time.time() - t_show:.0f}s",
    ]
    width = max(len(c) for c in card) + 4
    print("\n  " + green("╭" + "─" * width + "╮"))
    for c in card:
        print("  " + green("│") + "  " + S.pad(bold(c), width - 2) + green("│"))
    print("  " + green("╰" + "─" * width + "╯") + "\n")


def _wait_cpu(cpu, timeout=120):
    t0 = time.time()
    while time.time() - t0 < timeout:
        if attrs().get("cpu") == cpu:
            return
        time.sleep(0.5)
    d.die(f"main did not reach {cpu} vCPU")


def ycp_reports(cpu):
    """Whether YCP's inventory (what the console shows) has the new size yet. The pod
    gets it first: the agent reports on its next tick, the console on its next poll."""
    try:
        return abs(float(attrs().get("cpu") or -1) - float(cpu)) < 1e-6
    except Exception:  # noqa: BLE001 -- a missed read is "not yet"
        return False


def _wait_ycp(cpu, timeout=180):
    t0 = time.time()
    while time.time() - t0 < timeout:
        if ycp_reports(cpu):
            return
        time.sleep(1)
    d.die(f"YCP never reported {cpu} vCPU")


def _wait_live_cpu(cpu, timeout=120):
    t0 = time.time()
    while time.time() - t0 < timeout:
        if abs(live_cpu() - float(cpu)) < 1e-6:
            return
        time.sleep(0.5)
    d.die(f"the endpoint did not reach {cpu} vCPU")


def main(args):
    try:
        scale(args)
    except KeyboardInterrupt:
        raise SystemExit("\ninterrupted")
