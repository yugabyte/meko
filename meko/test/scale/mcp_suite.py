"""Drive the whole MCP tool surface against the seeded multi-account datapacks.

  surface   every one of the 25 tools, with checks: a full lifecycle on a scratch
            datapack (create -> conversations -> memories -> artifacts -> delete),
            plus the searches on seeded small, large and idle datapacks
  load      N concurrent agents for a fixed time, each using its datapack owner's PAT,
            running a weighted tool mix over the active datapacks (Zipf: a few are hot)

Talks MCP streamable HTTP (JSON-RPC over POST) with each datapack owner's Meko PAT. Writes
one JSON object per call to --out and prints a per-tool summary.
"""

import argparse
import asyncio
import base64
import json
import os
import random
import statistics
import time
import uuid

import httpx
import numpy as np

import workload as w

MCP_URL = os.environ.get("MCP_URL", "http://meko-mcp-server:8000/mcp")


class ToolError(Exception):
    pass


class Mcp:
    """One MCP client. Stateless HTTP, so every call is one POST."""

    def __init__(self, http: httpx.AsyncClient, pat: str):
        self.http, self.pat, self.n = http, pat, 0

    async def rpc(self, method: str, params: dict) -> dict:
        self.n += 1
        r = await self.http.post(
            MCP_URL,
            json={"jsonrpc": "2.0", "id": self.n, "method": method, "params": params},
            headers={
                "Authorization": f"Bearer {self.pat}",
                "Accept": "application/json, text/event-stream",
            },
        )
        if r.status_code != 200:
            raise ToolError(f"HTTP {r.status_code}: {r.text[:200]}")
        body = r.text
        if "data:" in body[:200]:
            body = [ln[5:] for ln in body.splitlines() if ln.startswith("data:")][-1]
        msg = json.loads(body)
        if "error" in msg:
            raise ToolError(f"JSON-RPC error: {msg['error']}")
        return msg["result"]

    async def call(self, tool: str, **args) -> object:
        res = await self.rpc("tools/call", {"name": tool, "arguments": args})
        text = "".join(c.get("text", "") for c in res.get("content", []) if c.get("type") == "text")
        try:
            out = json.loads(text) if text else None
        except json.JSONDecodeError:
            out = text
        if res.get("isError") or (isinstance(out, dict) and out.get("error")):
            raise ToolError(f"{tool}: {str(out or text)[:300]}")
        return out


def load_pats() -> dict:
    """MCP_PATS_FILE holds "<user_id> <token>" lines, one Meko PAT per seeded account."""
    pats = {}
    with open(os.environ["MCP_PATS_FILE"]) as f:
        lines = f.read().splitlines()
    for line in lines:
        parts = line.split()
        if len(parts) == 2:
            pats[parts[0]] = parts[1]
    return pats


def conv_id(conv) -> str:
    """conversation_create returns the new conversation's id under "id"."""
    if isinstance(conv, dict):
        return conv.get("id") or conv.get("conversation_id") or ""
    return ""


class Recorder:
    def __init__(self, path: str, label: str, call_timeout: float):
        self.f, self.label, self.rows, self.call_timeout = open(path, "a"), label, [], call_timeout

    async def timed(self, tool: str, coro, **tags):
        t = time.perf_counter()
        err, out = None, None
        try:
            # A hard deadline for the whole call: httpx's timeout only bounds the gap
            # between bytes, and a slow search can keep the stream alive for an hour.
            out = await asyncio.wait_for(coro, self.call_timeout)
        except Exception as e:  # recorded, not raised: one failing tool must not stop the run
            err = f"{type(e).__name__}: {e}"[:400]
        row = {
            "label": self.label,
            "tool": tool,
            "ms": (time.perf_counter() - t) * 1000,
            "ok": err is None,
            "error": err,
            "ts": time.time(),
            **tags,
        }
        self.f.write(json.dumps(row) + "\n")
        self.f.flush()
        self.rows.append(row)
        return out, err

    def summary(self) -> None:
        by = {}
        for r in self.rows:
            by.setdefault((r["tool"], r.get("cls", "")), []).append(r)
        print(f"label={self.label} calls={len(self.rows)}")
        for (tool, cls), rs in sorted(by.items()):
            ms = sorted(r["ms"] for r in rs if r["ok"])

            def q(p, ms=ms):
                return round(ms[min(len(ms) - 1, int(p * (len(ms) - 1)))], 1) if ms else None

            errs = [r["error"] for r in rs if not r["ok"]]
            print(
                json.dumps(
                    {
                        "tool": tool,
                        "class": cls,
                        "n": len(rs),
                        "errors": len(errs),
                        "p50_ms": q(0.5),
                        "p95_ms": q(0.95),
                        "p99_ms": q(0.99),
                        "first_error": errs[0] if errs else None,
                    }
                )
            )


def probe_datapacks() -> dict:
    """One seeded datapack per class, the same ones probe.py uses first."""
    out = {}
    for i in range(20000):
        c = w.size_class(i)
        if c not in out and (c == "idle" or w.sizes(i)[1] > 0):
            out[c] = i
        if len(out) == 3:
            break
    return out


async def surface(http: httpx.AsyncClient, pats: dict, rec: Recorder, conv_wait: float) -> None:
    run = uuid.uuid4().hex[:8]
    t = rec.timed

    # Seeded datapacks: the searches the scale test is about, on each size class.
    first_cid = None
    probes = probe_datapacks()
    if not probes:
        raise SystemExit("surface: no seeded probe datapacks found; seed before running")
    for cls, i in probes.items():
        dp = str(w.datapack_id(i))
        mcp = Mcp(http, pats[str(w.user_of(i))])
        conv, _ = await t(
            "conversation_create",
            mcp.call("conversation_create", datapack_id=dp, title=f"surface {run}"),
            cls=cls,
        )
        cid = conv_id(conv)
        first_cid = cid  # the last probe's: same account as the mcp client that outlives the loop
        for tool, args in (
            ("memory_search", dict(query="what topics does this datapack cover", limit=10)),
            ("knowledgebase_search", dict(query="synthetic chunk", limit=10)),
            ("context_search", dict(query="what do you know about this")),
            ("memory_get_all", {}),
            ("datapack_describe", dict(include_status=True)),
            ("conversation_list", dict(limit=20)),
        ):
            await t(tool, mcp.call(tool, conversation_id=cid, datapack_id=dp, **args), cls=cls)

    # The scratch datapack belongs to the last probe datapack's account, whose
    # conversation id datapack_create needs ("from the stored chat").
    # A scratch datapack, taken through every write tool and then deleted.
    created, err = await t(
        "datapack_create",
        mcp.call("datapack_create", name=f"onemil-surface-{run}", conversation_id=first_cid),
        cls="scratch",
    )
    if err:
        return
    dp = created.get("datapack_id") or created.get("id")
    conv, err = await t(
        "conversation_create",
        mcp.call("conversation_create", datapack_id=dp, title="surface"),
        cls="scratch",
    )
    cid = conv_id(conv)
    if err or not cid:
        return

    def call(tool, **a):
        return t(tool, mcp.call(tool, conversation_id=cid, datapack_id=dp, **a), cls="scratch")

    await call("datapack_update", description=f"surface run {run}")
    await call(
        "conversation_add_message",
        input="I prefer tea over coffee and my name is Ada.",
        output="Noted: you prefer tea, and your name is Ada.",
    )
    await call("conversation_get", include_messages=True)
    await call("conversation_update", title="surface (renamed)")
    await call(
        "memory_add", text="Ada prefers tea over coffee. Ada works on distributed databases."
    )

    # Extraction is asynchronous: wait for memories to appear before reading them back.
    deadline, memories = time.time() + conv_wait, []
    while time.time() < deadline:
        got, err = await call("memory_get_all")
        memories = (got or {}).get("memories", got) if isinstance(got, (dict, list)) else []
        if isinstance(memories, list) and memories:
            break
        await asyncio.sleep(3)
    ids = [m.get("id") or m.get("memory_id") for m in memories if isinstance(m, dict)]
    await call("memory_search", query="what does Ada drink", limit=5)
    if ids:
        await call("memory_get_by_id", memory_id=ids[0])
        await call("memory_update", memory_id=ids[0], text="Ada prefers green tea over coffee.")
        await call("memory_promote", memory_ids=ids[:1])
        await call("memory_delete_by_id", memory_id=ids[-1])
    else:
        rec.rows.append(
            {
                "label": rec.label,
                "tool": "memory_add",
                "ms": conv_wait * 1000,
                "ok": False,
                "error": f"no memories extracted within {conv_wait}s",
                "cls": "scratch",
            }
        )
    await call("knowledgebase_search", query="green tea", limit=5)
    await call("context_search", query="what does Ada drink")

    put, _ = await call(
        "artifact_put",
        filename="notes.txt",
        content_type="text/plain",
        content_base64=base64.b64encode(b"surface artifact").decode(),
    )
    if put:
        await call("artifact_get", content_hash=put.get("content_hash"))
    await call(
        "track_token_usage",
        name="surface",
        input_tokens=120,
        output_tokens=40,
        total_tokens=160,
        model="gpt-4.1-nano-2025-04-14",
    )
    await call("memory_delete_all")
    # A document id that does not exist: exercises the delete path without touching seeded data.
    await call("knowledgebase_delete_document", document_id=str(uuid.uuid4()))
    await call("conversation_delete")
    await call("datapack_delete")

    # Last, because at 1M datapacks it can take down the api-server replica serving it:
    # the unpaginated list, every datapack the account owns in one response.
    await t("datapack_list", mcp.call("datapack_list", conversation_id=first_cid), cls="account")


API_URL = os.environ.get("API_URL", "http://meko-api-server/api/v1")
TERMINAL = ("COMPLET", "INDEXED", "SUCCE", "DONE", "FAIL", "ERROR")


def doc_text(i: int, k: int) -> bytes:
    """A few KB of plain text, distinct per document, so each yields several chunks."""
    lines = [
        f"Document {k} of imported datapack {i}, section {s}: "
        + " ".join(f"term{(i * 31 + k * 7 + s * 3 + t) % 997}" for t in range(40))
        for s in range(40)
    ]
    return "\n".join(lines).encode()


async def upload(http: httpx.AsyncClient, pat: str, dp: str, name: str, body: bytes) -> dict:
    r = await http.post(
        f"{API_URL}/datapacks/{dp}/knowledge-bases/upload",
        headers={"Authorization": f"Bearer {pat}"},
        files={"file": (name, body, "text/plain")},
    )
    if r.status_code >= 300:
        raise ToolError(f"upload HTTP {r.status_code}: {r.text[:200]}")
    data = r.json()
    return data.get("data", data)


async def doc_status(http: httpx.AsyncClient, pat: str, dp: str, key: str) -> str:
    r = await http.get(
        f"{API_URL}/datapacks/{dp}/knowledge-bases/document-status",
        params={"document_key": key},
        headers={"Authorization": f"Bearer {pat}"},
    )
    if r.status_code >= 300:
        return f"HTTP {r.status_code}"
    data = r.json()
    data = data.get("data", data)
    return str(data.get("status") or data.get("document_status") or data)[:80]


def own_range(user: str) -> range:
    """Where to look for one of an account's datapacks."""
    if user == str(w.account_user_id(w.BIG_ACCOUNT)):
        return range(w.BIG_ACCOUNT_START, w.BIG_ACCOUNT_START + w.BIG_ACCOUNT_SIZE)
    return range(2_000_000)


def import_owners(pats: dict, a) -> list:
    """(user, one of its seeded datapacks) for each importing account: the 50k
    account with --big, else the first --accounts accounts to own a seeded datapack."""
    if a.big:
        user = str(w.account_user_id(w.BIG_ACCOUNT))
        return [(user, next(i for i in own_range(user) if str(w.user_of(i)) == user))]
    owners, seen = [], set()
    for i in range(1_000_000):
        user = str(w.user_of(i))
        if user in pats and user not in seen:
            seen.add(user)
            owners.append((user, i))
            if len(owners) == a.accounts:
                break
    return owners


RATE_LIMITED = ("429", "RATE_LIMITED", "rate limit")


async def with_backoff(rec: Recorder, tool: str, make, **tags):
    """Call make() until it is not rate-limited, as an importer would. Each 429 is
    recorded as its own row, so the rate limit shows up in the results."""
    delay = 2.0
    for _ in range(12):
        out, err = await rec.timed(tool, make(), **tags)
        if not err or not any(x in err for x in RATE_LIMITED):
            return out, err
        rec.rows[-1]["tool"] = f"{tool}_429"
        await asyncio.sleep(delay + random.random())
        delay = min(delay * 2, 30.0)
    return out, err


async def import_run(http, pats: dict, rec: Recorder, a) -> None:
    """Create N datapacks, spread over the importing accounts, fill each with M
    memories and K documents, and time every stage to "indexed". Another account
    uploads one probe document every 30 s throughout, to measure how long a small
    tenant waits behind the import."""
    owners = import_owners(pats, a)
    owner_ids = {u for u, _ in owners}
    probe_owner = next(u for u in sorted(pats) if u not in owner_ids)
    run = uuid.uuid4().hex[:6]
    clients = {}
    for user, i in owners:
        mcp = Mcp(http, pats[user])
        boot, _ = await rec.timed(
            "conversation_create",
            mcp.call("conversation_create", datapack_id=str(w.datapack_id(i))),
            cls="import",
        )
        clients[user] = (mcp, conv_id(boot))
    start = {"import": "start", "accounts": len(owners), "concurrency": a.concurrency}
    print(json.dumps(start), flush=True)
    sem = asyncio.Semaphore(a.concurrency)
    docs, datapacks = [], []
    t_start = time.time()

    async def one(n: int) -> None:
        user = owners[n % len(owners)][0]
        mcp, boot_cid = clients[user]
        async with sem:
            made, err = await with_backoff(
                rec,
                "datapack_create",
                lambda: mcp.call(
                    "datapack_create", name=f"import-{run}-{n:05d}", conversation_id=boot_cid
                ),
                cls="import",
            )
            dp = (made or {}).get("datapack_id") if isinstance(made, dict) else None
            if err or not dp:
                return
            conv, _ = await rec.timed(
                "conversation_create", mcp.call("conversation_create", datapack_id=dp), cls="import"
            )
            cid = conv_id(conv)
            if not cid:
                return
            for m in range(a.memories):
                await rec.timed(
                    "memory_add",
                    mcp.call(
                        "memory_add",
                        datapack_id=dp,
                        conversation_id=cid,
                        text=f"Imported fact {m} for datapack {n}: item {n * 1000 + m}.",
                    ),
                    cls="import",
                )
            for k in range(a.docs):
                res, err = await with_backoff(
                    rec,
                    "kb_upload",
                    lambda k=k: upload(http, pats[user], dp, f"doc-{k}.txt", doc_text(n, k)),
                    cls="import",
                )
                if not err and isinstance(res, dict) and res.get("document_key"):
                    docs.append((dp, res["document_key"], time.time(), user))
            datapacks.append((dp, cid, user))

    async def probe_small_tenant(stop: asyncio.Event) -> None:
        dp = str(
            next(
                w.datapack_id(i) for i in own_range(probe_owner) if str(w.user_of(i)) == probe_owner
            )
        )
        k = 0
        while not stop.is_set():
            res, err = await rec.timed(
                "kb_upload",
                upload(http, pats[probe_owner], dp, f"probe-{run}-{k}.txt", doc_text(999999, k)),
                cls="probe",
            )
            if not err and isinstance(res, dict) and res.get("document_key"):
                t0 = time.time()
                while not stop.is_set() and time.time() - t0 < a.index_wait:
                    st = await doc_status(http, pats[probe_owner], dp, res["document_key"])
                    if any(x in st.upper() for x in TERMINAL):
                        rec.rows.append(
                            {
                                "label": rec.label,
                                "tool": "time_to_indexed",
                                "cls": "probe",
                                "ms": (time.time() - t0) * 1000,
                                "ok": "FAIL" not in st.upper() and "ERROR" not in st.upper(),
                                "error": st,
                                "ts": time.time(),
                            }
                        )
                        break
                    await asyncio.sleep(5)
            k += 1
            await asyncio.sleep(30)

    stop = asyncio.Event()
    prober = asyncio.create_task(probe_small_tenant(stop))
    await asyncio.gather(*(one(n) for n in range(a.datapacks)))
    t_loaded = time.time()
    print(
        json.dumps(
            {
                "import": "loaded",
                "datapacks": len(datapacks),
                "docs": len(docs),
                "secs": round(t_loaded - t_start, 1),
            }
        ),
        flush=True,
    )

    # Documents: poll each until it reaches a terminal status.
    pending = list(docs)
    deadline = time.time() + a.index_wait
    while pending and time.time() < deadline:
        still = []
        for dp, key, t0, user in pending:
            st = await doc_status(http, pats[user], dp, key)
            if any(x in st.upper() for x in TERMINAL):
                rec.rows.append(
                    {
                        "label": rec.label,
                        "tool": "time_to_indexed",
                        "cls": "import",
                        "ms": (time.time() - t0) * 1000,
                        "ok": "FAIL" not in st.upper() and "ERROR" not in st.upper(),
                        "error": st,
                        "ts": time.time(),
                    }
                )
            else:
                still.append((dp, key, t0, user))
        pending = still
        await asyncio.sleep(10)
    for dp, key, t0, _ in pending:
        rec.rows.append(
            {
                "label": rec.label,
                "tool": "time_to_indexed",
                "cls": "import",
                "ms": a.index_wait * 1000,
                "ok": False,
                "error": "not indexed before --index-wait",
                "ts": time.time(),
            }
        )
    t_indexed = time.time()

    # Memories: extraction runs in the background; count what landed.
    got = 0
    for dp, cid, user in datapacks:
        res, _ = await rec.timed(
            "memory_get_all",
            clients[user][0].call("memory_get_all", datapack_id=dp, conversation_id=cid),
            cls="import",
        )
        mems = (res or {}).get("memories", res) if isinstance(res, (dict, list)) else []
        got += len(mems) if isinstance(mems, list) else 0
    stop.set()
    await prober
    print(
        json.dumps(
            {
                "import": "done",
                "datapacks": len(datapacks),
                "docs": len(docs),
                "docs_unindexed": len(pending),
                "memories_added": len(datapacks) * a.memories,
                "memories_found": got,
                "load_secs": round(t_loaded - t_start, 1),
                "index_secs": round(t_indexed - t_loaded, 1),
            }
        ),
        flush=True,
    )


MIX = [  # (tool, weight): an agent mostly searches and records, occasionally lists
    ("memory_search", 30),
    ("memory_add", 15),
    ("context_search", 12),
    ("knowledgebase_search", 10),
    ("conversation_add_message", 12),
    ("conversation_get", 4),
    ("conversation_list", 3),
    ("memory_get_all", 5),
    ("datapack_describe", 4),
    ("track_token_usage", 5),
]


def active_datapacks(limit: int) -> list:
    """Active datapacks in a fixed shuffled order, so the Zipf head (the hot ones)
    lands on many accounts instead of the lowest indices."""
    active = [i for i in range(limit) if w.size_class(i) != "idle"]
    random.Random(w.SEED).shuffle(active)
    return active


async def agent(
    n: int,
    http: httpx.AsyncClient,
    pats: dict,
    rec: Recorder,
    active: list,
    stop: float,
    think: float,
    rng,
    only: str = "",
) -> None:
    tools, weights = zip(*([(only, 1)] if only else MIX))
    zipf = rng.zipf(1.3, 1_000_000)
    k = 0
    while time.time() < stop:
        i = active[(int(zipf[k % len(zipf)]) - 1) % len(active)]
        k += 1
        dp, cls = str(w.datapack_id(i)), w.size_class(i)
        pat = pats.get(str(w.user_of(i)))
        if not pat:
            await asyncio.sleep(think)
            continue
        mcp = Mcp(http, pat)  # every call acts as the datapack's owner
        conv, err = await rec.timed(
            "conversation_create",
            mcp.call("conversation_create", datapack_id=dp, agent_id=f"load-{n}"),
            cls=cls,
        )
        if err:
            await asyncio.sleep(think)
            continue
        cid = conv_id(conv)
        if not cid:
            continue
        for _ in range(rng.integers(3, 9)):  # a short session on this datapack
            tool = random.choices(tools, weights)[0]
            # Every call carries the agent that created the conversation, or
            # conversation tools reject it with agent_id_mismatch. Two tools take no agent_id.
            a = dict(conversation_id=cid, datapack_id=dp)
            if tool not in ("datapack_describe", "track_token_usage"):
                a["agent_id"] = f"load-{n}"
            if tool in ("memory_search", "context_search", "knowledgebase_search"):
                a["query"] = f"what do we know about topic {rng.integers(0, 2000)}"
            elif tool == "memory_add":
                fact = rng.integers(0, 10**9)
                a["text"] = f"Agent {n} learned fact {fact} while working on datapack {i}."
            elif tool == "conversation_add_message":
                a.update(input=f"question {rng.integers(0, 10**6)}", output="an answer")
            elif tool == "track_token_usage":
                a.update(name="load", input_tokens=200, output_tokens=50, total_tokens=250)
            await rec.timed(tool, mcp.call(tool, **a), cls=cls, agent=n)
            await asyncio.sleep(think)


async def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("mode", choices=["surface", "load", "import"])
    ap.add_argument("--label", default="")
    ap.add_argument("--out", default="/dev/null")
    ap.add_argument("--agents", type=int, default=10)
    ap.add_argument("--minutes", type=float, default=10)
    ap.add_argument("--think", type=float, default=0.5, help="seconds between an agent's calls")
    ap.add_argument("--seeded", type=int, default=1_000_000)
    ap.add_argument(
        "--hot", type=int, default=0, help="confine load to the first N active datapacks"
    )
    ap.add_argument(
        "--only", default="", help="run a single tool from the mix instead of all of it"
    )
    ap.add_argument("--timeout", type=float, default=30, help="whole-call deadline, seconds")
    ap.add_argument("--extract-wait", type=float, default=90)
    ap.add_argument("--big", action="store_true", help="load or import the 50,000-datapack account")
    ap.add_argument("--datapacks", type=int, default=1000, help="import: datapacks to create")
    ap.add_argument("--memories", type=int, default=3, help="import: memory_add calls per datapack")
    ap.add_argument("--docs", type=int, default=1, help="import: documents per datapack")
    ap.add_argument("--concurrency", type=int, default=16, help="import: datapacks in flight")
    ap.add_argument(
        "--accounts", type=int, default=1, help="import: accounts to spread datapacks over"
    )
    ap.add_argument(
        "--index-wait", type=float, default=1800, help="import: seconds to wait for indexing"
    )
    a = ap.parse_args()

    pats = load_pats()
    rec = Recorder(a.out, a.label or a.mode, a.timeout)
    limits = httpx.Limits(max_connections=a.agents + 10, max_keepalive_connections=a.agents + 10)
    async with httpx.AsyncClient(timeout=a.timeout + 30, limits=limits) as http:
        if a.mode == "surface":
            await surface(http, pats, rec, a.extract_wait)
        elif a.mode == "import":
            await import_run(http, pats, rec, a)
        else:
            if a.big:
                big = range(w.BIG_ACCOUNT_START, w.BIG_ACCOUNT_START + w.BIG_ACCOUNT_SIZE)
                active = [i for i in big if w.size_class(i) != "idle"]
                random.Random(w.SEED).shuffle(active)
            else:
                active = active_datapacks(a.seeded)
            if a.hot:
                active = active[: a.hot]
            stop = time.time() + a.minutes * 60
            await asyncio.gather(
                *(
                    agent(
                        n,
                        http,
                        pats,
                        rec,
                        active,
                        stop,
                        a.think,
                        np.random.default_rng(n),
                        a.only,
                    )
                    for n in range(a.agents)
                )
            )
    rec.summary()
    if a.mode == "load":
        ok = [r for r in rec.rows if r["ok"]]
        secs = a.minutes * 60
        print(
            json.dumps(
                {
                    "agents": a.agents,
                    "calls": len(rec.rows),
                    "ok": len(ok),
                    "calls_per_s": round(len(rec.rows) / secs, 2),
                    "error_rate": round(1 - len(ok) / max(1, len(rec.rows)), 4),
                    "p50_all_ms": round(statistics.median(r["ms"] for r in ok), 1) if ok else None,
                }
            )
        )


if __name__ == "__main__":
    asyncio.run(main())
