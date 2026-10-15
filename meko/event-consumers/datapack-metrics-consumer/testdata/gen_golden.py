"""Record what the MCP server's inline bookkeeping writes for a fixed sequence of searches.

The search.completed handler must leave the four bookkeeping tables exactly as the inline code in
meko-mcp-server (`_record_daily_activity`, `_record_token_savings`, `_submit_audit_log`) does. This runs
that code against real tables and writes the sequence and the rows it produced to
testdata/search_completed.json, which internal/handlers/parity_test.go replays through the handler.

It also records the events the producer stores for each search. It runs the MCP server's own
`_emit_search_completed` through the real `events.emit`, flushes the queue into a scratch events table, and reads
the stored JSON back (so envelope building, JSON normalisation and the `occurred_at` stamp are all the
producer's), not a copy of them. That needs meko-mcp-server at or after the batching producer.

Run it from a meko-mcp-server checkout with its virtualenv, against a database where it may create and
drop a `meko_system` schema (the legacy SQL names that schema, so it cannot be renamed). It refuses to run
if the schema already exists:

    .venv/bin/python /path/to/meko/meko/event-consumers/datapack-metrics-consumer/testdata/gen_golden.py \\
        --dsn postgresql://postgres@127.0.0.1:5432/postgres \\
        --mcp-server . > /path/to/meko/meko/event-consumers/datapack-metrics-consumer/testdata/search_completed.json

Regenerate it when the inline bookkeeping changes, and review the diff: it is the contract the handler
is held to.
"""

import argparse
import json
import os
import pathlib
import sys
import types
from unittest.mock import patch

import psycopg
from psycopg_pool import ConnectionPool

HERE = pathlib.Path(__file__).resolve().parent
SCHEMA_SQL = (HERE.parent / "internal" / "testdb" / "bookkeeping.sql").read_text()

DP_A = "11111111-1111-4111-8111-111111111111"
DP_B = "22222222-2222-4222-8222-222222222222"
CONV_1 = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1"
CONV_2 = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2"
CONV_3 = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3"
USER_1 = "cccccccc-cccc-4ccc-8ccc-ccccccccccc1"
USER_2 = "cccccccc-cccc-4ccc-8ccc-ccccccccccc2"


def search(datapack, conv, user, sent, raw, high=0, mid=0, low=0, audited=True, kind="hybrid"):
    return {
        "search_type": kind,
        "datapack_id": datapack,
        "conversation_id": conv,
        "user_id": user,
        "tokens_sent": sent,
        "tokens_if_raw": raw,
        "hits_high": high,
        "hits_mid": mid,
        "hits_low": low,
        "quota_metric": "context_search",
        "quota_audited": audited,
    }


# Each entry is one context_search call. tokens_sent is what the tool computes as hybrid_tokens_sent, and
# the token-savings write only happens when it is above zero, as in context_search.
SEARCHES = [
    search(DP_A, CONV_1, USER_1, 1910, 12000, 3, 1, 0),
    search(DP_A, CONV_1, USER_1, 640, 12000, 1, 2, 1),
    search(DP_A, CONV_2, USER_2, 0, 0),  # a zero-result search still counts as a query
    search(DP_B, CONV_3, USER_2, 2500, 9000, 2, 0, 0),
    search("", CONV_1, "", 800, 9000, audited=False),  # stdio: no datapack, no user
    search(DP_A, "not-a-uuid", USER_1, 700, 12000, 1, 0, 0),  # daily count and audit only
    search(
        DP_A, CONV_1, "", 300, 12000, 0, 0, 1, audited=False
    ),  # no user: the log row has a NULL user
    search(
        DP_B, CONV_3, USER_1, 500, 200, 1, 0, 0
    ),  # sent more than the baseline: log keeps it, summary clamps
    search(DP_A, CONV_1, USER_1, 100, 12000, 1, 0, 0),
    search(DP_A, CONV_1, USER_1, 100, 12000, 1, 0, 0),
    search(DP_A, CONV_1, USER_1, 100, 12000, 1, 0, 0),  # the same key three times in one batch
    search(
        DP_A, CONV_2, "bogus", 400, 12000, 0, 1, 0, audited=False
    ),  # an invalid user id is stored as NULL
]


# Events per flush: more than one, so a flush is a multi-row INSERT, and not a divisor of len(SEARCHES).
FLUSH_BATCH = 5


def rows(cur, query):
    cur.execute(query)
    return [
        [str(v) if v is not None and not isinstance(v, (int, float, str)) else v for v in r]
        for r in cur.fetchall()
    ]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dsn", required=True)
    ap.add_argument("--mcp-server", required=True, help="path to a meko-mcp-server checkout")
    args = ap.parse_args()

    # The producer is off by default, and its background flusher must not race the explicit flushes below.
    os.environ["MEKO_EVENTS_EMIT"] = "true"
    os.environ["MEKO_EVENTS_FLUSH_INTERVAL_S"] = "60"
    sys.path.insert(0, str(pathlib.Path(args.mcp_server) / "src"))
    import events  # noqa: E402  (the MCP server's producer)
    import tools  # noqa: E402  (needs the MCP server's virtualenv)

    with psycopg.connect(args.dsn, autocommit=True) as admin:
        if admin.execute("SELECT 1 FROM pg_namespace WHERE nspname = 'meko_system'").fetchone():
            sys.exit("refusing to run: this database already has a meko_system schema")
        admin.execute("CREATE SCHEMA meko_system")
        try:
            admin.execute(SCHEMA_SQL)
            # Only the columns the producer writes; the consumer's own tests use the real table definition.
            admin.execute(
                "CREATE TABLE meko_system.events (shard smallint NOT NULL,"
                " created_at timestamptz NOT NULL DEFAULT clock_timestamp(), id uuid NOT NULL, type text NOT NULL,"
                " event_schema_version smallint NOT NULL DEFAULT 1, payload jsonb NOT NULL,"
                " PRIMARY KEY (shard, created_at, id))"
            )
            pool = ConnectionPool(args.dsn, min_size=1, max_size=2, open=True)
            ctx = types.SimpleNamespace(lifespan_context={"system_pool": pool})

            # Flushed a few at a time, so the producer's multi-row INSERT is what stores them: one at a time
            # would only ever exercise a one-row statement.
            for start in range(0, len(SEARCHES), FLUSH_BATCH):
                chunk = SEARCHES[start : start + FLUSH_BATCH]
                for s in chunk:
                    emitted = tools._emit_search_completed(
                        ctx,
                        search_type=s["search_type"],
                        datapack_id=s["datapack_id"],
                        conversation_id=s["conversation_id"],
                        user_id=s["user_id"],
                        tokens_sent=s["tokens_sent"],
                        tokens_if_raw=s["tokens_if_raw"],
                        hits_high=s["hits_high"],
                        hits_mid=s["hits_mid"],
                        hits_low=s["hits_low"],
                        quota_audited=s["quota_audited"],
                    )
                    if emitted is not True:
                        sys.exit(f"the producer did not queue the event for {s}")
                stored = events.flush_now()
                if stored != len(chunk):
                    sys.exit(f"the producer stored {stored} of {len(chunk)} events in one flush")
                for s in chunk:
                    tools._record_daily_activity(ctx, s["datapack_id"], s["search_type"])
                    if s["tokens_sent"] > 0:
                        tools._record_token_savings(
                            ctx,
                            s["datapack_id"],
                            s["conversation_id"],
                            "trace",
                            s["user_id"],
                            s["search_type"],
                            s["tokens_sent"],
                            s["tokens_if_raw"],
                            s["hits_high"],
                            s["hits_mid"],
                            s["hits_low"],
                        )
                    if s["quota_audited"]:
                        tools._submit_audit_log(ctx, s["user_id"], s["quota_metric"])
            tools._audit_executor.shutdown(wait=True)  # the audit write is asynchronous
            events.shutdown(timeout=5)
            with admin.cursor() as cur:
                cur.execute("SELECT payload FROM meko_system.events ORDER BY created_at, id")
                recorded = [r[0] for r in cur.fetchall()]
            # The clock. It is fixed so the file does not change on every run; the tests that replay these
            # payloads set a real time, because the legacy code wrote CURRENT_DATE.
            for payload in recorded:
                payload["occurred_at"] = "2000-01-01T00:00:00+00:00"
            pool.close()

            with admin.cursor() as cur:
                golden = {
                    "datapack_daily_activity": rows(
                        cur,
                        "SELECT datapack_id, CASE WHEN activity_date = CURRENT_DATE THEN 'TODAY' ELSE activity_date::text END,"
                        " search_type, query_count FROM meko_system.datapack_daily_activity ORDER BY 1, 2, 3",
                    ),
                    "token_savings_log": rows(
                        cur,
                        "SELECT datapack_id, user_id, conversation_id, source, tokens_sent, tokens_if_raw, tokens_saved"
                        " FROM meko_system.token_savings_log ORDER BY 1, 3, 2 NULLS FIRST, 4, 5, 6",
                    ),
                    "token_savings_summary": rows(
                        cur,
                        "SELECT datapack_id, source, request_count, tokens_saved_total, tokens_if_raw_total,"
                        " score_high_total, score_mid_total, score_low_total"
                        " FROM meko_system.token_savings_summary ORDER BY 1, 2",
                    ),
                    "free_tier_audit_log": rows(
                        cur,
                        "SELECT user_id, metric_name FROM meko_system.free_tier_audit_log ORDER BY 1, 2",
                    ),
                }
        finally:
            admin.execute("DROP SCHEMA meko_system CASCADE")

    json.dump({"searches": SEARCHES, "events": recorded, "golden": golden}, sys.stdout, indent=2)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
