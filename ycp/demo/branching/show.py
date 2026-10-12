"""The presentation: `ycp_demo.py show`.

Six acts, all real and all through YCP:
  1. The problem    -- a slow query on main, and its plan
  2. The agent      -- Claude reads the plan and proposes one fix per hypothesis
  3. Fork           -- one desired-state write forks a full copy of main per fix
  4. The race       -- every fix tested on its own copy, in parallel
  5. Clean up       -- the copies vanish; main is still untouched
  6. Ship it        -- only the proven fix is applied to main (--no-ship skips it)
"""

import concurrent.futures as cf
import json
import re
import shutil
import subprocess
import sys
import threading
import time

import ycp_demo as d

# ------------------------------------------------------------------ terminal

TTY = sys.stdout.isatty()


def sgr(code):
    return (lambda s: f"\x1b[{code}m{s}\x1b[0m") if TTY else (lambda s: s)


bold, dim, green, yellow, red, cyan, magenta, inv = (
    sgr("1"),
    sgr("2"),
    sgr("32"),
    sgr("33"),
    sgr("31"),
    sgr("36"),
    sgr("35"),
    sgr("7"),
)
SPIN = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
W = min(shutil.get_terminal_size((100, 30)).columns, 100) - 2
PAUSE = False


def visible_len(s):
    return len(re.sub(r"\x1b\[[0-9;]*m", "", s))


def pad(s, n):
    return s + " " * max(0, n - visible_len(s))


def beat(seconds=1.2):
    if PAUSE:
        input(dim("  ⏎ "))
        sys.stdout.write("\x1b[1A\x1b[2K")
    else:
        time.sleep(seconds)


def act(n, title, subtitle):
    print()
    print("  " + inv(bold(f" ACT {n} ")) + "  " + bold(title))
    print("  " + dim(subtitle))
    print()


def typeout(text, delay=0.012):
    if not TTY:
        print(text)
        return
    for ch in text:
        sys.stdout.write(ch)
        sys.stdout.flush()
        if ch not in "\x1b[0123456789;m":
            time.sleep(delay)
    print()


class Board:
    """A block of lines redrawn in place."""

    def __init__(self):
        self.height = 0
        self.last = None

    def draw(self, lines):
        if not TTY:
            # Piped output: print a frame only when something other than a timer changed.
            key = re.sub(r"[\d.]+s\b|[" + SPIN + "]", "", "\n".join(lines))
            if key != self.last and lines:
                print("\n".join(lines) + "\n")
            self.last = key
            return
        if self.height:
            sys.stdout.write(f"\x1b[{self.height}A")
        for line in lines:
            sys.stdout.write("\x1b[2K" + line + "\n" if TTY else line + "\n")
        if TTY and len(lines) < self.height:
            for _ in range(self.height - len(lines)):
                sys.stdout.write("\x1b[2K\n")
            sys.stdout.write(f"\x1b[{self.height - len(lines)}A")
        sys.stdout.flush()
        self.height = len(lines)


def spinner_call(label, fn):
    """Run fn in a thread with a live spinner and timer; return (result, seconds)."""
    box, t0 = {}, time.time()

    def go():
        try:
            box["v"] = fn()
        except BaseException as e:  # noqa: BLE001 -- re-raised on the main thread
            box["e"] = e

    th = threading.Thread(target=go, daemon=True)
    th.start()
    board, i = Board(), 0
    while th.is_alive():
        board.draw([f"  {cyan(SPIN[i % len(SPIN)])} {label}  {dim(f'{time.time() - t0:4.1f}s')}"])
        i += 1
        time.sleep(0.08)
    board.draw([])
    if "e" in box:
        raise box["e"]
    return box["v"], time.time() - t0


# ------------------------------------------------------------------ live inventory


class Watch:
    """Polls the YCP inventory in the background; records when each id first went ready."""

    def __init__(self):
        self.inv, self.ready_at, self.gone_at, self.stop = {}, {}, {}, False
        self.t0 = time.time()
        threading.Thread(target=self.loop, daemon=True).start()

    def loop(self):
        while not self.stop:
            try:
                inv = d.inventory()
                now = time.time()
                for k, r in inv.items():
                    if r.get("state") == "ready":
                        self.ready_at.setdefault(k, now)
                for k in self.inv.keys() - inv.keys():  # released, even with nothing on screen
                    self.gone_at.setdefault(k, now)
                self.inv = inv
            except BaseException:  # noqa: BLE001 -- a missed poll is just a stale frame
                pass
            time.sleep(0.7)

    def state(self, rid):
        r = self.inv.get(rid)
        return None if r is None else (r.get("state") or "?")


def tree(w, hyps, phase, t_start, frame):
    """Render main and its forks. phase: fork | drop."""
    spin = cyan(SPIN[frame % len(SPIN)])
    main_ep = w.inv.get(f"endpoint/{d.CLUSTER}/main/rw", {}).get("attrs", {})
    lines = [
        f"  {bold(d.cfg['DEMO_CLUSTER'])}  {dim('·  ' + (('managed · ' + d.cfg['YCP_REGION']) if d.MANAGED else d.cfg['YCP_ENV']) + '  ·  via YCP')}",
        f"  {green('●')} {bold('main')}  {dim('source · ' + main_ep.get('address', ''))}",
    ]
    for i, h in enumerate(hyps):
        last = i == len(hyps) - 1
        b_id, e_id = f"branch/{d.CLUSTER}/{h['branch']}", f"endpoint/{d.CLUSTER}/{h['branch']}/rw"
        bs, es = w.state(b_id), w.state(e_id)
        elbow, rail = ("└─", "  ") if last else ("├─", "│ ")
        if phase == "drop":
            # Dropped when YCP accepts the delete (the console removes it then); the
            # compute is released when the agent reports the branch gone, a tick or more later.
            dropped = h.get("dropped_at")
            if not dropped:
                b = f"{spin} {yellow('dropping')}  {time.time() - t_start:4.1f}s"
            elif bs is None:
                gone = w.gone_at.setdefault(b_id, time.time())
                b = dim(
                    f"✖ dropped  {dropped - t_start:4.1f}s  ·  compute released {gone - t_start:4.1f}s"
                )
            else:
                b = (
                    dim(f"✖ dropped  {dropped - t_start:4.1f}s  ·  ")
                    + f"{spin} {dim('releasing compute')}"
                )
            lines.append(f"  {dim(elbow)} {pad(h['branch'], 30)} {b}")
            continue
        if bs == "ready":
            b = green(f"✔ forked   {w.ready_at[b_id] - t_start:4.1f}s") + dim(
                f"  at LSN {w.inv[b_id].get('attrs', {}).get('fork_lsn', '?')}"
            )
        elif bs is None:
            b = f"{spin} {dim('declared')} {time.time() - t_start:4.1f}s"
        else:
            b = f"{spin} {yellow(bs)} {time.time() - t_start:4.1f}s"
        if es == "ready" and h.get("warm_at"):
            e = green(f"✔ compute ready {w.ready_at[e_id] - t_start:4.1f}s") + green(
                f"  ✔ cache warm {h['warm_at'] - t_start:4.1f}s"
            )
        elif es == "ready" and h.get("warm_err"):
            e = green(f"✔ compute ready {w.ready_at[e_id] - t_start:4.1f}s") + dim("  · cache cold")
        elif es == "ready":
            e = (
                green(f"✔ compute ready {w.ready_at[e_id] - t_start:4.1f}s")
                + f"  {spin} {yellow('warming cache')}"
            )
        elif bs != "ready":
            e = dim("· waiting for its branch")
        else:
            e = f"{spin} {yellow('starting compute')} {dim('0.5 vCPU')} {time.time() - t_start:4.1f}s"
        lines.append(f"  {dim(elbow)} {pad(bold(h['branch']), 30)} {b}")
        lines.append(f"  {dim(rail)}   {dim('└─ rw')}  {e}")
    return lines


# ------------------------------------------------------------------ the agent

REHEARSED = [
    {
        "name": "missing-index",
        "why": "Seq scan reads 1M rows to find 20",
        "sql": "CREATE INDEX ON orders (customer_id, placed_at DESC)",
    },
    {"name": "stale-statistics", "why": "orders was never analyzed", "sql": "ANALYZE orders"},
    {
        "name": "work-mem-too-small",
        "why": "64kB work_mem may spill the sort",
        "sql": "SET work_mem = '16MB'",
    },
]

PROMPT = """You are a Postgres performance engineer. A query is slow. Below are its
EXPLAIN ANALYZE output, the table definition, table stats and work_mem.
Propose exactly {n} distinct hypotheses for why it is slow. For each, give ONE SQL
statement that FIXES it on a throwaway copy of the database: something that changes
the database or session (CREATE INDEX, ANALYZE, VACUUM, SET, ALTER ...), never a
read-only diagnostic query. Lead with the most likely cause; the others are
plausible alternatives worth ruling out.
Reply with ONLY JSON: {{"hypotheses":[{{"name":"kebab-case, max 3 words","why":"max 50 chars","sql":"..."}}]}}

{evidence}"""


def claude(prompt):
    """One `claude -p` call; returns (parsed JSON, the answer exactly as Claude wrote it)."""
    out = subprocess.run(
        ["claude", "-p", prompt, "--output-format", "json"],
        capture_output=True,
        text=True,
        timeout=90,
    )
    raw = json.loads(out.stdout)["result"].strip()
    return json.loads(re.sub(r"^```(json)?|```$", "", raw, flags=re.M).strip()), raw


def show_prompt(instructions, evidence, max_evidence_lines=10):
    """The prompt as sent: the instructions in full, the evidence head-first."""
    import textwrap

    print(f"  {bold(magenta('▸ prompt'))}  {dim('claude -p')}")
    for para in instructions.strip().split("\n\n"):
        for line in textwrap.wrap(" ".join(para.split()), W - 6):
            print(f"    {line}")
    ev = [l for l in evidence.splitlines() if l.strip()]
    print(f"    {dim('— evidence —')}")
    for line in ev[:max_evidence_lines]:
        print(f"    {dim(line[: W - 6])}")
    if len(ev) > max_evidence_lines:
        print(f"    {dim(f'… {len(ev) - max_evidence_lines} more lines')}")


def show_answer(raw, secs):
    print(f"\n  {bold(magenta('◂ response'))}  {dim(f'Claude, {secs:.0f}s')}")
    try:
        pretty = json.dumps(
            json.loads(re.sub(r"^```(json)?|```$", "", raw, flags=re.M).strip()), indent=2
        )
    except ValueError:
        pretty = raw
    for line in pretty.splitlines():
        typeout(f"    {cyan(line[: W - 6])}", delay=0.002)


def ask_claude(evidence, n):
    parsed, raw = claude(PROMPT.format(n=n, evidence=evidence))
    hyps = parsed["hypotheses"][:n]
    if not hyps or not all(h.get("sql") and h.get("name") for h in hyps):
        raise ValueError("unusable answer")
    return hyps, raw


def branch_names(hyps):
    seen = set()
    for h in hyps:
        base = re.sub(r"[^a-z0-9]+", "-", h["name"].lower()).strip("-")[:24] or "fix"
        name, k = base, 2
        while name in seen:
            name, k = f"{base}-{k}", k + 1
        seen.add(name)
        h["branch"] = name
    return hyps


# ------------------------------------------------------------------ measuring


def timed(addr, setup_sql=(), runs=5):
    """Best of `runs` executions, in ONE session that first runs setup_sql."""
    explain = f"EXPLAIN (ANALYZE, FORMAT JSON) {d.QUERY}"
    out = d.psql(addr, [*setup_sql, *[explain] * runs], statement_timeout_s=120)
    dec, i, times = json.JSONDecoder(), 0, []
    while i < len(out):
        j = out.find("[", i)
        if j < 0:
            break
        doc, i = dec.raw_decode(out, j)
        times.append(doc[0]["Execution Time"])
    return min(times)


def pg_error(msg):
    """The ERROR line of a psql failure, not the DETAIL/CONTEXT lines after it."""
    line = next((l for l in msg.splitlines() if "ERROR:" in l), msg.splitlines()[-1] if msg else "")
    return line.split("ERROR:", 1)[-1].strip()[:60]


FIX_TIMEOUT_S = 60  # a fix slower than this (VACUUM FULL on 5M rows at 0.5 vCPU) loses the race


def warm(h):
    """Read the table once on a fresh copy, so the race compares fixes, not cold caches:
    a new endpoint's first scan pulls every page from the pageserver (~10 s here)."""
    try:
        d.psql(h["addr"], d.QUERY, statement_timeout_s=120)
        h["warm_at"] = time.time()
    except Exception:  # noqa: BLE001 -- a cold copy still races, just slower
        h["warm_err"] = True


def trial(h, addr, base):
    """Apply the fix, then time the query. A session-level SET is replayed in the
    timing session; anything else persists on the branch. Two runs decide a fix that
    doesn't help; one that does gets two more, and reports its best."""
    try:
        d.psql(addr, h["sql"], statement_timeout_s=FIX_TIMEOUT_S)
    except Exception as e:  # noqa: BLE001 -- a bad fix is a result, not a crash
        msg = str(e)
        h["error"] = (
            f"fix took over {FIX_TIMEOUT_S}s" if "statement timeout" in msg else pg_error(msg)
        )
        return
    try:
        session = [h["sql"]] if re.match(r"\s*SET\s", h["sql"], re.I) else []
        ms = timed(addr, session, runs=2)
        if ms * 2 < base:
            ms = min(ms, timed(addr, session, runs=2))
        h["ms"] = max(ms, 0.001)
    except Exception as e:  # noqa: BLE001
        h["error"] = pg_error(str(e))


def fmt(ms):
    return f"{ms:.2f} ms" if ms < 1 else f"{ms:.1f} ms"


def store_intro(addr):
    """Meet the store: what is in the database, and the page that is slow."""
    rows = d.psql(
        addr,
        "SELECT relname || '|' || to_char(n_live_tup, 'FM999,999,999') || '|' || "
        "pg_size_pretty(pg_total_relation_size(relid)) FROM pg_stat_user_tables "
        "ORDER BY pg_total_relation_size(relid)",
    ).splitlines()
    total_b = int(
        d.psql(addr, "SELECT sum(pg_total_relation_size(relid)) FROM pg_stat_user_tables")
    )
    total = f"{total_b / 2**30:.1f} GB" if total_b >= 2**30 else f"{total_b / 2**20:.0f} MB"
    print(f"  {bold(d.STORE)}  {dim(d.TAGLINE)}")
    for r in rows:
        name, n, size = r.split("|")
        print(f"    {pad(name, 13)} {n:>10} rows  {dim(size)}")
    print(f"    {dim(f'{total} in all')}")
    who = d.psql(
        addr,
        f"SELECT name || ', ' || city || ' · customer since ' || to_char(joined_at, 'YYYY') "
        f"FROM customers WHERE id = {d.CUSTOMER_ID}",
    )
    print(f"\n  {bold('My Orders')} {dim('for ' + who)}")
    for r in d.psql(
        addr,
        f"SELECT to_char(placed_at, 'Mon DD YYYY') || '|' || status || '|' || total "
        f"FROM orders WHERE customer_id = {d.CUSTOMER_ID} ORDER BY placed_at DESC LIMIT 3",
    ).splitlines():
        day, status, total_ = r.split("|")
        print(f"    {dim(day)}  {pad(status, 10)} {f'${float(total_):,.2f}':>10}")
    print(f"    {dim('…')}")
    return total


def bar(ms, base, width=34):
    n = max(1, round(width * min(ms, base) / base)) if ms else 0
    return "█" * n + dim("·" * (width - n))


# ------------------------------------------------------------------ the show


def show(args):
    global PAUSE
    PAUSE = "--pause" in args
    ship = "--no-ship" not in args
    n = 3

    addr, main_id = d.main_endpoint()
    # Copies left by a show that was killed before its clean-up ran.
    stale = sorted(
        {
            r["name"].split("/")[2]
            for r in d.desired()
            if r["kind"] == "branch" and d.mine(r["name"]) and r["name"].split("/")[2] != "main"
        }
    )
    if stale:
        spinner_call(
            f"dropping {len(stale)} copies left by an earlier run",
            lambda: (
                d.remove(*(f"{d.CLUSTER}/{b}" for b in stale)),
                d.wait([f"branch/{d.CLUSTER}/{b}" for b in stale], gone=True, timeout=300),
            ),
        )
    undone = reset_main(addr)
    if undone:
        print(dim(f"  (reset main from the last show: dropped {undone})"))
    t_show = time.time()
    print("\n" + "  " + bold(magenta("Fork the database, not the problem.")))
    print(
        "  "
        + dim("Every action below is a YCP desired-state write against a live amp environment.")
    )

    # ---- act 1
    act(
        1,
        "The problem",
        f"{d.CUSTOMER.split()[0]} opens “My Orders” on {d.STORE}. The page runs this query.",
    )
    db_size = store_intro(addr)
    print()
    typeout("  " + cyan(d.QUERY))
    stats = d.psql(
        addr,
        [
            "SELECT pg_size_pretty(pg_total_relation_size('orders'))",
            "SELECT to_char(count(*), 'FM999,999,999') FROM orders",
        ],
    ).splitlines()
    base, _ = spinner_call("timing it on main", lambda: timed(addr))
    plan = d.psql(addr, f"EXPLAIN (ANALYZE) {d.QUERY}")
    scan = next((l.strip().lstrip("-> ") for l in plan.splitlines() if "Scan" in l), "")
    hits = d.psql(addr, f"SELECT count(*) FROM ({d.QUERY}) q")
    print(f"  {bold(red(f'{base:.1f} ms'))} per call  {dim('·')}  {stats[1]} orders, {stats[0]}")
    print(
        f"  {dim('plan:')} {scan.split('(')[0].strip()}  {dim('·')}  "
        f"reads all {stats[1]} rows to return {hits}"
    )
    beat()

    # ---- act 2
    act(
        2,
        "Ask the agent",
        f"Claude reads the real plan and proposes {n} fixes. Each gets its own copy of main.",
    )
    evidence = "\n".join(
        [
            plan,
            d.psql(addr, "\\d orders"),
            d.psql(addr, "SELECT relname, n_live_tup, last_analyze FROM pg_stat_user_tables"),
            "work_mem = " + d.psql(addr, "SHOW work_mem"),
        ]
    )
    if "--no-agent" in args or not shutil.which("claude"):
        hyps, who = [dict(h) for h in REHEARSED], "rehearsed"
    else:
        show_prompt(PROMPT.split("{evidence}")[0].format(n=n), evidence)
        print()
        try:
            (hyps, raw), secs = spinner_call(
                "Claude is reading the query plan", lambda: ask_claude(evidence, n)
            )
            show_answer(raw, secs)
            who = f"Claude, {secs:.0f}s"
        except Exception:  # noqa: BLE001 -- never let the agent sink the show
            hyps, who = [dict(h) for h in REHEARSED], "rehearsed (agent unavailable)"
    hyps = branch_names(hyps)
    print(f"\n  {dim(who + ' · one copy of main per hypothesis:')}")
    for i, h in enumerate(hyps, 1):
        typeout(f"  {bold(str(i))}  {bold(h['branch'])}  {dim('— ' + h.get('why', ''))}")
    beat()

    # ---- act 3
    act(
        3,
        "Fork",
        f"One API call: {n} branches of main, each with its own compute. No data is copied.",
    )
    w = Watch()
    t_fork = time.time()
    d.add(
        [
            r
            for h in hyps
            for r in (
                {
                    "kind": "branch",
                    "name": f"{d.CLUSTER}/{h['branch']}",
                    "spec": {"parent_branch_id": main_id},
                },
                {
                    "kind": "endpoint",
                    "name": f"{d.CLUSTER}/{h['branch']}/rw",
                    "spec": d.endpoint("cluster_ip"),
                },
            )
        ]
    )
    b_ids = [f"branch/{d.CLUSTER}/{h['branch']}" for h in hyps]
    e_ids = [f"endpoint/{d.CLUSTER}/{h['branch']}/rw" for h in hyps]
    board, frame = Board(), 0
    try:
        warming = {}
        while not all(h.get("warm_at") or h.get("warm_err") for h in hyps):
            if time.time() - t_fork > 600:
                raise RuntimeError("forks did not come up within 10 minutes")
            for h in hyps:  # warm each copy the moment its compute is up
                row = w.inv.get(f"endpoint/{d.CLUSTER}/{h['branch']}/rw")
                if row and row.get("state") == "ready" and h["branch"] not in warming:
                    h["addr"] = d.endpoint_addr(row)
                    warming[h["branch"]] = threading.Thread(target=warm, args=(h,), daemon=True)
                    warming[h["branch"]].start()
            board.draw(tree(w, hyps, "fork", t_fork, frame))
            frame += 1
            time.sleep(0.1)
        board.draw(tree(w, hyps, "fork", t_fork, frame))
        forks_s = max(w.ready_at[b] for b in b_ids) - t_fork
        compute_s = max(w.ready_at[e] for e in e_ids) - t_fork
        warm_s = max((h.get("warm_at") or time.time()) for h in hyps) - t_fork
        print(
            f"\n  {green('✔')} {n} full copies of the store ({db_size}) in {bold(f'{forks_s:.1f}s')}"
            f"  {dim('·')}  compute up in {compute_s:.0f}s  {dim('·')}  caches warm in {warm_s:.0f}s"
        )
        beat()

        # ---- act 4
        act(
            4,
            "The race",
            "Every fix runs on its own copy, at the same time. Main is never touched.",
        )
        pool = cf.ThreadPoolExecutor(n)
        futs = {pool.submit(trial, h, h["addr"], base): h for h in hyps}
        board, frame, t_race = Board(), 0, time.time()
        while True:
            lines = [
                f"  {pad(dim('main (baseline)'), 30)} {bar(base, base)}  {red(f'{fmt(base):>11}')}"
            ]
            for h in hyps:
                if "ms" in h:
                    x = base / h["ms"]
                    tag = green(bold(f"{x:,.0f}× faster")) if x >= 2 else dim("no improvement")
                    lines.append(
                        f"  {pad(bold(h['branch']), 30)} {bar(h['ms'], base)}  {fmt(h['ms']):>11}  {tag}"
                    )
                elif "error" in h:
                    lines.append(f"  {pad(h['branch'], 30)} {red('fix failed: ' + h['error'])}")
                else:
                    lines.append(
                        f"  {pad(h['branch'], 30)} {cyan(SPIN[frame % len(SPIN)])} "
                        f"{dim('applying fix, measuring')}  {time.time() - t_race:4.1f}s"
                    )
            board.draw(lines)
            if all(f.done() for f in futs):
                break
            frame += 1
            time.sleep(0.1)
        pool.shutdown()
        winners = sorted((h for h in hyps if "ms" in h), key=lambda h: h["ms"])
        win = winners[0] if winners and base / winners[0]["ms"] >= 2 else None
        if win:
            print(
                f"\n  {green('▶')} root cause: {bold(win['name'])}  "
                f"{fmt(base)} → " + bold(green(fmt(win["ms"])))
            )
        beat()
    finally:
        # ---- act 5 (always: never leave copies behind)
        act(5, "Clean up", "The copies and their compute are dropped. Nothing to clean up by hand.")
        t_drop = time.time()
        d.remove(*(f"{d.CLUSTER}/{h['branch']}" for h in hyps))
        for h in hyps:
            h["dropped_at"] = time.time()
        drop_s = time.time() - t_drop
        board, frame = Board(), 0
        for _ in range(15):  # a moment on screen; release carries on in the background
            board.draw(tree(w, hyps, "drop", t_drop, frame))
            frame += 1
            time.sleep(0.1)
        print(
            f"\n  {green('✔')} dropped in {drop_s:.1f}s  {dim('·  compute is released in the background')}"
        )

    after = None
    if ship and win:
        beat()
        act(6, "Ship it", "Only the fix that won the race is applied to main.")
        # A session SET only lasts for its session, so on main it becomes a database default.
        sql = re.sub(r"^\s*SET\s", "ALTER DATABASE postgres SET ", win["sql"], flags=re.I)
        typeout("  " + cyan(sql))
        _, secs = spinner_call(
            "applying to main", lambda: d.psql(addr, sql, statement_timeout_s=300)
        )
        after = timed(addr)
        print(f"  {pad(dim('main, before'), 30)} {bar(base, base)}  {red(f'{fmt(base):>11}')}")
        print(
            f"  {pad(bold('main, after'), 30)} {bar(after, base)}  {bold(green(f'{fmt(after):>11}'))}"
            f"  {green(bold(f'{base / max(after, 0.001):,.0f}× faster'))}"
        )
        print(f"  {dim(f'applied in {secs:.1f}s')}")

    def released():
        while any(w.state(b) is not None for b in b_ids) and time.time() - t_drop < 300:
            time.sleep(0.5)

    if any(w.state(b) is not None for b in b_ids):
        spinner_call("releasing the copies' compute", released)
    w.stop = True
    release_s = max((w.gone_at.get(b) or time.time()) for b in b_ids) - t_drop
    left = sum(1 for b in b_ids if w.state(b) is not None)
    total = time.time() - t_show
    source = "from Claude" if who.startswith("Claude") else "(rehearsed)"
    card = [
        f"{n} hypotheses {source}, tested in parallel on {n} full copies of main",
        (
            f"root cause: {win['name']}  ·  {fmt(base)} → {fmt(win['ms'])}  ({base / win['ms']:,.0f}× faster)"
            if win
            else "no fix beat the baseline"
        ),
        *(
            [f"shipped to main: {fmt(base)} → {fmt(after)}  ·  only after it was proven"]
            if after
            else []
        ),
        f"forked in {forks_s:.1f}s  ·  dropped in {drop_s:.0f}s, compute released in {release_s:.0f}s  ·  "
        f"whole story {total:.0f}s  ·  {left} copies left",
    ]
    width = max(len(c) for c in card) + 4
    print("\n  " + green("╭" + "─" * width + "╮"))
    for c in card:
        print("  " + green("│") + "  " + pad(bold(c), width - 2) + green("│"))
    print("  " + green("╰" + "─" * width + "╯") + "\n")


def restore_slow(addr):
    """Put back the planted problems a shipped fix removed. Returns what it dropped."""
    idx = d.psql(
        addr,
        "SELECT string_agg(quote_ident(indexname), ', ') FROM pg_indexes "
        "WHERE tablename = 'orders' AND indexname <> 'orders_pkey'",
    )
    if idx:
        d.psql(addr, f"DROP INDEX {idx}")
    # Any database-level setting a fix shipped goes; the planted work_mem comes back.
    d.psql(
        addr, ["ALTER DATABASE postgres RESET ALL", "ALTER DATABASE postgres SET work_mem = '64kB'"]
    )
    return idx


def reset_main(addr):
    """Start from the slow main at DEMO_CPU, whatever an earlier show left. Returns what
    restore_slow dropped."""
    undone = restore_slow(addr)
    # The resize show leaves main scaled down; copies start at DEMO_CPU, so main has to
    # match or the race credits a fix with the extra CPU.
    import scale

    if abs(scale.live_cpu() - float(d.cfg["DEMO_CPU"])) > 1e-6:
        spinner_call(
            f"setting main back to {d.cfg['DEMO_CPU']} vCPU",
            lambda: (scale.resize(d.cfg["DEMO_CPU"]), scale._wait_live_cpu(d.cfg["DEMO_CPU"])),
        )
    return undone


def reset(_args):
    addr, _ = d.main_endpoint()
    idx = restore_slow(addr)
    d.say(f"main reset: dropped {idx or 'no'} index(es); slow query is {fmt(timed(addr))} again")


def main(cmd, args):
    try:
        {"show": show, "reset": reset}[cmd](args)
    except KeyboardInterrupt:
        sys.exit("\ninterrupted; branches were dropped")
