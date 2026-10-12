"""Time GET /datapacks for accounts of different sizes: the unpaged stream, one
page, a full walk of the pages, the name filters, and /datapacks/count.

    MCP_PATS_FILE=pats API_URL=http://localhost:18080/api/v1 python listbench.py
"""

import argparse
import collections
import json
import os
import time

import httpx

import workload as w

TIMEOUT = 30.0


def accounts() -> dict[str, int]:
    """The big account, the largest regular one, and a median one, by user id."""
    sizes = collections.Counter(w.account_of(i) for i in range(1_000_000))
    ranked = sorted(sizes.items(), key=lambda kv: -kv[1])
    median = ranked[len(ranked) // 2]
    return {
        str(w.account_user_id(w.BIG_ACCOUNT)): w.BIG_ACCOUNT_SIZE,
        str(w.account_user_id(ranked[0][0])): ranked[0][1],
        str(w.account_user_id(median[0])): median[1],
    }


def emit(**kv) -> None:
    print(json.dumps(kv), flush=True)


def timed_get(c: httpx.Client, size: int, test: str, path: str, params=None):
    t = time.perf_counter()
    try:
        r = c.get(path, params=params)
        ms = (time.perf_counter() - t) * 1000
        emit(test=test, size=size, status=r.status_code, ms=round(ms, 1), bytes=len(r.content))
        return r.json() if r.status_code == 200 else None
    except Exception as e:  # noqa: BLE001 - a timeout is a result here
        ms = round((time.perf_counter() - t) * 1000, 1)
        emit(test=test, size=size, error=type(e).__name__, ms=ms)
        return None


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("--repeat", type=int, default=3)
    a = p.parse_args()
    api = os.environ.get("API_URL", "http://localhost:18080/api/v1")
    with open(os.environ["MCP_PATS_FILE"]) as f:
        pats = dict(line.split() for line in f if line.strip())

    for user, size in accounts().items():
        c = httpx.Client(
            base_url=api, headers={"Authorization": f"Bearer {pats[user]}"}, timeout=TIMEOUT
        )

        def get(test, path, params=None, c=c, size=size):
            return timed_get(c, size, test, path, params)

        for _ in range(a.repeat):
            get("count", "/datapacks/count")
            get("page_100", "/datapacks", {"limit": 100})
            get("page_100_total", "/datapacks", {"limit": 100, "include_total": "true"})
            get("page_100_by_name", "/datapacks", {"limit": 100, "sort": "name"})
            get("q_substring", "/datapacks", {"limit": 100, "q": "42"})
            get("name_exact", "/datapacks", {"limit": 10, "name": "no-such-datapack"})
        get("stream_all", "/datapacks")

        # Walk every page at the maximum page size.
        t = time.perf_counter()
        rows = pages = 0
        cursor = None
        while True:
            params = {"limit": 1000}
            if cursor:
                params["cursor"] = cursor
            body = get("walk_page", "/datapacks", params)
            if body is None:
                break
            data = body["data"]
            rows += len(data["items"])
            pages += 1
            if not data["has_more"]:
                break
            cursor = data["next_cursor"]
        ms = round((time.perf_counter() - t) * 1000, 1)
        emit(test="walk_all", size=size, rows=rows, pages=pages, ms=ms)


if __name__ == "__main__":
    main()
