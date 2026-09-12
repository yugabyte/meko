from __future__ import annotations

import json
import math
import sqlite3
import threading
import time
import zlib
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Iterable

import psycopg
import psycopg.conninfo
from psycopg.rows import dict_row

COLLECTOR_TAG = "/* amp-time:collector */"
COLLECTOR_MARKER = "amp_time_collector"
MANIFEST_FILE = "amp-time.manifest.json"
SNAPSHOT_PREFIX = "amp-time.out."
DATABASE_FILE = "amp-time.sqlite3"
MAX_TIMESLICE_RANGE = 200_000
_RANGE_EDGE_ALLOWANCE = 1_024
DML_OPERATIONS = ("SELECT", "INSERT", "UPDATE", "DELETE")
OPERATION_RATE_KEYS = {
    operation: f"{operation.lower()}s_per_sec" for operation in DML_OPERATIONS
}
OPERATION_MEAN_KEYS = {
    operation: f"{operation.lower()}_mean_exec_ms" for operation in DML_OPERATIONS
}


def utc_now() -> datetime:
    return datetime.now(timezone.utc)


def iso_utc(value: datetime) -> str:
    return value.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


def _parsed_utc(value: str) -> datetime:
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None or parsed.utcoffset() is None:
        raise ValueError("timeslice timestamp must include a UTC offset")
    return parsed.astimezone(timezone.utc)


def _safe_float(value: Any) -> float:
    try:
        return float(value or 0)
    except (TypeError, ValueError):
        return 0.0


def _safe_int(value: Any) -> int:
    try:
        return int(value or 0)
    except (TypeError, ValueError):
        return 0


def statement_key(row: dict[str, Any]) -> tuple[str, str, str, bool]:
    return (
        str(row.get("dbid") or ""),
        str(row.get("userid") or ""),
        str(row.get("queryid") or ""),
        bool(row.get("toplevel", True)),
    )


def statement_operation(query: Any) -> str:
    """Return the top-level DML operation represented by a PGSS query."""
    text = str(query or "")
    length = len(text)
    index = 0
    depth = 0
    first_word = True
    with_clause = False

    while index < length:
        char = text[index]
        if char.isspace():
            index += 1
            continue
        if text.startswith("--", index):
            newline = text.find("\n", index + 2)
            index = length if newline < 0 else newline + 1
            continue
        if text.startswith("/*", index):
            end = text.find("*/", index + 2)
            index = length if end < 0 else end + 2
            continue
        if char == "'":
            index += 1
            while index < length:
                if text[index] != "'":
                    index += 1
                elif index + 1 < length and text[index + 1] == "'":
                    index += 2
                else:
                    index += 1
                    break
            continue
        if char == '"':
            index += 1
            while index < length:
                if text[index] != '"':
                    index += 1
                elif index + 1 < length and text[index + 1] == '"':
                    index += 2
                else:
                    index += 1
                    break
            continue
        if char == "$":
            tag_end = text.find("$", index + 1)
            if tag_end >= 0:
                tag = text[index : tag_end + 1]
                if tag == "$$" or tag[1:-1].replace("_", "a").isalnum():
                    closing = text.find(tag, tag_end + 1)
                    if closing >= 0:
                        index = closing + len(tag)
                        continue
        if char == "(":
            depth += 1
            index += 1
            continue
        if char == ")":
            depth = max(0, depth - 1)
            index += 1
            continue
        if char.isalpha() or char == "_":
            end = index + 1
            while end < length and (text[end].isalnum() or text[end] in {"_", "$"}):
                end += 1
            word = text[index:end].upper()
            if depth == 0:
                if first_word:
                    first_word = False
                    if word in {"SELECT", "INSERT", "UPDATE", "DELETE"}:
                        return word
                    if word != "WITH":
                        return "OTHER"
                    with_clause = True
                elif with_clause and word in {"SELECT", "INSERT", "UPDATE", "DELETE"}:
                    return word
            index = end
            continue
        index += 1
    return "OTHER"


def add_derived_rate_totals(document: dict[str, Any]) -> dict[str, Any]:
    """Backfill operation rates, mean execution times, and row rates."""
    totals = document.setdefault("totals", {})
    if not isinstance(totals, dict):
        totals = {}
        document["totals"] = totals
    duration = max(_safe_float(document.get("duration_seconds")), 0.001)
    derived_keys = set(OPERATION_RATE_KEYS.values()) | set(OPERATION_MEAN_KEYS.values())
    if any(key not in totals for key in derived_keys):
        rows = [row for row in document.get("rows", []) if isinstance(row, dict)]
        operation_calls = {operation: 0 for operation in DML_OPERATIONS}
        operation_total_ms = {operation: 0.0 for operation in DML_OPERATIONS}
        for row in rows:
            operation = str(row.get("command") or statement_operation(row.get("query"))).upper()
            if operation in operation_calls:
                calls = _safe_int(row.get("calls"))
                operation_calls[operation] += calls
                operation_total_ms[operation] += (
                    _safe_float(row.get("total_ms"))
                    if row.get("total_ms") is not None
                    else _safe_float(row.get("mean_ms")) * calls
                )
        for operation, calls in operation_calls.items():
            totals.setdefault(OPERATION_RATE_KEYS[operation], round(calls / duration, 4))
            totals.setdefault(
                OPERATION_MEAN_KEYS[operation],
                round(operation_total_ms[operation] / calls, 4) if calls else 0.0,
            )
    totals.setdefault("rows_per_sec", round(_safe_int(totals.get("rows")) / duration, 4))
    return document


@dataclass(frozen=True)
class Sample:
    captured_at: datetime
    rows: list[dict[str, Any]]
    collection_ms: float


class PgssCollector:
    """Collect the portable subset of pg_stat_statements from one PostgreSQL server."""

    def __init__(self, dsn: str, password: str | None = None) -> None:
        self.dsn = dsn
        self.password = password
        self._conn: psycopg.Connection[dict[str, Any]] | None = None
        self._time_column: str | None = None
        self._has_toplevel = False
        self._columns: set[str] = set()

    def close(self) -> None:
        if self._conn is not None:
            try:
                self._conn.close()
            finally:
                self._conn = None
                self._time_column = None
                self._columns = set()

    def _connect(self) -> psycopg.Connection[dict[str, Any]]:
        if self._conn is not None and not self._conn.closed:
            return self._conn
        kwargs: dict[str, Any] = {
            "autocommit": True,
            "row_factory": dict_row,
            "application_name": "amp-time",
            "connect_timeout": 10,
        }
        if self.password:
            kwargs["password"] = self.password
        self._conn = psycopg.connect(self.dsn, **kwargs)
        self._detect_columns(self._conn)
        return self._conn

    def _detect_columns(self, conn: psycopg.Connection[dict[str, Any]]) -> None:
        try:
            with conn.cursor() as cur:
                # A named CTE survives PGSS normalization (leading comments do not),
                # allowing the real collector query to exclude all of its own SQL.
                cur.execute(
                    f"{COLLECTOR_TAG} WITH amp_time_probe AS (SELECT 1) "
                    "SELECT s.* FROM pg_stat_statements s CROSS JOIN amp_time_probe LIMIT 0"
                )
                columns = {item.name for item in cur.description or ()}
        except psycopg.Error as exc:
            raise RuntimeError(
                "pg_stat_statements is unavailable. Ensure it is in shared_preload_libraries "
                "and run CREATE EXTENSION pg_stat_statements in this database."
            ) from exc
        if "total_exec_time" in columns:
            self._time_column = "total_exec_time"
        elif "total_time" in columns:
            self._time_column = "total_time"
        else:
            raise RuntimeError("pg_stat_statements exposes neither total_exec_time nor total_time")
        self._has_toplevel = "toplevel" in columns
        self._columns = columns

    def _column(self, name: str, cast: str, fallback: str) -> str:
        return f"s.{name}::{cast}" if name in self._columns else fallback

    def collect(self) -> Sample:
        started = time.perf_counter()
        try:
            conn = self._connect()
            assert self._time_column is not None
            top_level = "s.toplevel" if self._has_toplevel else "TRUE"
            shared_reads = self._column("shared_blks_read", "bigint", "0::bigint")
            shared_hits = self._column("shared_blks_hit", "bigint", "0::bigint")
            wal_bytes = self._column("wal_bytes", "float8", "0::float8")
            stats_since = self._column("stats_since", "text", "NULL::text")
            minmax_stats_since = self._column("minmax_stats_since", "text", "NULL::text")
            min_exec = self._column("min_exec_time", "float8", "NULL::float8")
            max_exec = self._column("max_exec_time", "float8", "NULL::float8")
            mean_exec = self._column(
                "mean_exec_time",
                "float8",
                f"CASE WHEN s.calls > 0 THEN s.{self._time_column} / s.calls END::float8",
            )
            stddev_exec = self._column("stddev_exec_time", "float8", "NULL::float8")
            sql = f"""{COLLECTOR_TAG}
                WITH {COLLECTOR_MARKER} AS (SELECT 1 AS enabled)
                SELECT
                    s.dbid::text AS dbid,
                    s.userid::text AS userid,
                    s.queryid::text AS queryid,
                    {top_level} AS toplevel,
                    COALESCE(d.datname, '[dropped database]')::text AS dbname,
                    COALESCE(r.rolname, '[dropped role]')::text AS username,
                    s.query::text AS query,
                    s.calls::bigint AS calls,
                    s.{self._time_column}::float8 AS total_exec_time,
                    s.rows::bigint AS rows,
                    {shared_reads} AS shared_blks_read,
                    {shared_hits} AS shared_blks_hit,
                    {wal_bytes} AS wal_bytes,
                    {stats_since} AS stats_since,
                    {minmax_stats_since} AS minmax_stats_since,
                    {min_exec} AS min_exec_time,
                    {max_exec} AS max_exec_time,
                    {mean_exec} AS mean_exec_time,
                    {stddev_exec} AS stddev_exec_time
                FROM pg_stat_statements s
                LEFT JOIN pg_database d ON d.oid = s.dbid
                LEFT JOIN pg_roles r ON r.oid = s.userid
                CROSS JOIN {COLLECTOR_MARKER} internal
                WHERE internal.enabled = 1
                  AND s.query NOT LIKE '%amp_top_%'
                  AND s.query NOT LIKE '%amp_time_%'
                  AND s.query NOT LIKE '%pgss_top_%'
                  AND NOT (
                    s.query LIKE '%s.queryid::text AS queryid%'
                    AND s.query LIKE '%FROM pg_stat_statements s%'
                    AND s.query LIKE '%LEFT JOIN pg_roles r ON r.oid = s.userid%'
                  )
            """
            with conn.cursor() as cur:
                cur.execute(sql)
                rows = list(cur.fetchall())
        except Exception:
            self.close()
            raise
        elapsed_ms = (time.perf_counter() - started) * 1000
        return Sample(captured_at=utc_now(), rows=rows, collection_ms=elapsed_ms)


def _counter_delta(current: float, previous: float) -> tuple[float, bool]:
    if current >= previous:
        return current - previous, False
    return current, True


def build_timeslice(previous: Sample, current: Sample, source: dict[str, str]) -> dict[str, Any]:
    """Subtract cumulative PGSS counters and return one browser-ready timeslice."""
    duration = max((current.captured_at - previous.captured_at).total_seconds(), 0.001)
    previous_by_key = {statement_key(row): row for row in previous.rows}
    delta_rows: list[dict[str, Any]] = []
    reset_count = 0

    for row in current.rows:
        old = previous_by_key.get(statement_key(row))
        current_stats_since = str(row.get("stats_since") or "")
        old_stats_since = str(old.get("stats_since") or "") if old is not None else ""
        epoch_changed = bool(old is not None and current_stats_since and current_stats_since != old_stats_since)
        if old is None or epoch_changed:
            calls = _safe_int(row.get("calls"))
            total_ms = _safe_float(row.get("total_exec_time"))
            rows = _safe_int(row.get("rows"))
            shared_blks_read = _safe_int(row.get("shared_blks_read"))
            shared_blks_hit = _safe_int(row.get("shared_blks_hit"))
            wal_bytes = _safe_float(row.get("wal_bytes"))
            reset = epoch_changed
        else:
            calls_raw, calls_reset = _counter_delta(
                _safe_int(row.get("calls")), _safe_int(old.get("calls"))
            )
            total_raw, time_reset = _counter_delta(
                _safe_float(row.get("total_exec_time")),
                _safe_float(old.get("total_exec_time")),
            )
            rows_raw, rows_reset = _counter_delta(
                _safe_int(row.get("rows")), _safe_int(old.get("rows"))
            )
            reads_raw, reads_reset = _counter_delta(
                _safe_int(row.get("shared_blks_read")), _safe_int(old.get("shared_blks_read"))
            )
            hits_raw, hits_reset = _counter_delta(
                _safe_int(row.get("shared_blks_hit")), _safe_int(old.get("shared_blks_hit"))
            )
            wal_raw, wal_reset = _counter_delta(
                _safe_float(row.get("wal_bytes")), _safe_float(old.get("wal_bytes"))
            )
            calls = int(calls_raw)
            total_ms = float(total_raw)
            rows = int(rows_raw)
            shared_blks_read = int(reads_raw)
            shared_blks_hit = int(hits_raw)
            wal_bytes = float(wal_raw)
            reset = (
                calls_reset
                or time_reset
                or rows_reset
                or reads_reset
                or hits_reset
                or wal_reset
            )
        if calls <= 0 and total_ms <= 0:
            continue
        if reset:
            reset_count += 1
        delta_rows.append(
            {
                "dbid": str(row.get("dbid") or ""),
                "userid": str(row.get("userid") or ""),
                "queryid": str(row.get("queryid") or ""),
                "dbname": row.get("dbname") or "",
                "username": row.get("username") or "",
                "toplevel": bool(row.get("toplevel", True)),
                "query": row.get("query") or "",
                "command": statement_operation(row.get("query")),
                "calls": calls,
                "calls_per_sec": round(calls / duration, 4),
                "total_ms": round(total_ms, 4),
                "mean_ms": round(total_ms / calls, 4) if calls else 0.0,
                "rows": rows,
                "rows_per_call": round(rows / calls, 4) if calls else 0.0,
                "shared_blks_read": shared_blks_read,
                "shared_blks_hit": shared_blks_hit,
                "shared_hit_pct": round(
                    shared_blks_hit / (shared_blks_hit + shared_blks_read) * 100, 4
                )
                if shared_blks_hit + shared_blks_read
                else None,
                "wal_bytes": round(wal_bytes, 2),
                "stats_since": current_stats_since or None,
                "minmax_stats_since": str(row.get("minmax_stats_since") or "") or None,
                "pgss_calls": _safe_int(row.get("calls")),
                "pgss_total_ms": round(_safe_float(row.get("total_exec_time")), 4),
                "pgss_min_ms": (
                    round(_safe_float(row.get("min_exec_time")), 4)
                    if row.get("min_exec_time") is not None
                    else None
                ),
                "pgss_max_ms": (
                    round(_safe_float(row.get("max_exec_time")), 4)
                    if row.get("max_exec_time") is not None
                    else None
                ),
                "pgss_mean_ms": (
                    round(_safe_float(row.get("mean_exec_time")), 4)
                    if row.get("mean_exec_time") is not None
                    else None
                ),
                "pgss_stddev_ms": (
                    round(_safe_float(row.get("stddev_exec_time")), 4)
                    if row.get("stddev_exec_time") is not None
                    else None
                ),
                "counter_reset": reset,
            }
        )

    total_time = sum(float(row["total_ms"]) for row in delta_rows)
    total_calls = sum(int(row["calls"]) for row in delta_rows)
    total_rows = sum(int(row["rows"]) for row in delta_rows)
    total_shared_reads = sum(int(row["shared_blks_read"]) for row in delta_rows)
    total_shared_hits = sum(int(row["shared_blks_hit"]) for row in delta_rows)
    total_wal_bytes = sum(float(row["wal_bytes"]) for row in delta_rows)
    operation_calls = {
        operation: sum(int(row["calls"]) for row in delta_rows if row["command"] == operation)
        for operation in DML_OPERATIONS
    }
    operation_total_ms = {
        operation: sum(
            float(row["total_ms"]) for row in delta_rows if row["command"] == operation
        )
        for operation in DML_OPERATIONS
    }
    for row in delta_rows:
        row["time_pct"] = round((float(row["total_ms"]) / total_time * 100), 4) if total_time else 0.0
    delta_rows.sort(key=lambda row: (float(row["total_ms"]), int(row["calls"])), reverse=True)

    slice_id = current.captured_at.strftime("%Y%m%d_%H%M%S_%f")
    return {
        "format_version": 1,
        "slice_id": slice_id,
        "start_utc": iso_utc(previous.captured_at),
        "end_utc": iso_utc(current.captured_at),
        "duration_seconds": round(duration, 4),
        "collection_ms": round(current.collection_ms, 2),
        "source": source,
        "totals": {
            "statements": len(delta_rows),
            "calls": total_calls,
            "calls_per_sec": round(total_calls / duration, 4),
            "total_ms": round(total_time, 4),
            "rows": total_rows,
            "rows_per_sec": round(total_rows / duration, 4),
            "selects_per_sec": round(operation_calls["SELECT"] / duration, 4),
            "inserts_per_sec": round(operation_calls["INSERT"] / duration, 4),
            "updates_per_sec": round(operation_calls["UPDATE"] / duration, 4),
            "deletes_per_sec": round(operation_calls["DELETE"] / duration, 4),
            "select_mean_exec_ms": round(
                operation_total_ms["SELECT"] / operation_calls["SELECT"], 4
            )
            if operation_calls["SELECT"]
            else 0.0,
            "insert_mean_exec_ms": round(
                operation_total_ms["INSERT"] / operation_calls["INSERT"], 4
            )
            if operation_calls["INSERT"]
            else 0.0,
            "update_mean_exec_ms": round(
                operation_total_ms["UPDATE"] / operation_calls["UPDATE"], 4
            )
            if operation_calls["UPDATE"]
            else 0.0,
            "delete_mean_exec_ms": round(
                operation_total_ms["DELETE"] / operation_calls["DELETE"], 4
            )
            if operation_calls["DELETE"]
            else 0.0,
            "shared_blks_read": total_shared_reads,
            "shared_blks_hit": total_shared_hits,
            "wal_bytes": round(total_wal_bytes, 2),
            "counter_resets": reset_count,
        },
        "rows": delta_rows,
    }


def _percentile(values: list[float], percentile: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    position = (len(ordered) - 1) * percentile
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    fraction = position - lower
    return ordered[lower] + ((ordered[upper] - ordered[lower]) * fraction)


def _weighted_percentile(values: list[tuple[float, int]], percentile: float) -> float | None:
    weighted = sorted((value, weight) for value, weight in values if weight > 0)
    total_weight = sum(weight for _, weight in weighted)
    if not weighted or total_weight <= 0:
        return None
    target = max(1.0, percentile * total_weight)
    cumulative = 0
    for value, weight in weighted:
        cumulative += weight
        if cumulative >= target:
            return value
    return weighted[-1][0]


def build_latency_distribution(samples: list[dict[str, Any]]) -> dict[str, Any]:
    """Summarize retained per-timeslice mean_exec_time samples for one statement."""
    usable = [
        sample
        for sample in samples
        if _safe_int(sample.get("calls")) > 0 and _safe_float(sample.get("mean_ms")) >= 0
    ]
    usable.sort(key=lambda sample: str(sample.get("end_utc") or ""))
    values = [_safe_float(sample.get("mean_ms")) for sample in usable]
    total_calls = sum(_safe_int(sample.get("calls")) for sample in usable)
    total_ms = sum(_safe_float(sample.get("total_ms")) for sample in usable)

    if not values:
        return {
            "basis": "call_weighted_timeslice_mean_exec_time",
            "coverage": "all_calls_in_retained_timeslices",
            "exact_execution_percentiles": False,
            "samples": 0,
            "calls": 0,
            "start_utc": None,
            "end_utc": None,
            "min_ms": None,
            "p50_ms": None,
            "p90_ms": None,
            "p99_ms": None,
            "max_ms": None,
            "avg_ms": None,
            "bins": [],
        }

    minimum = min(values)
    maximum = max(values)
    bin_count = 1 if minimum == maximum else min(16, max(5, round(math.sqrt(len(values)))))
    width = (maximum - minimum) / bin_count if bin_count > 1 else 0.0
    bins = [
        {
            "lower_ms": minimum + (index * width) if width else minimum,
            "upper_ms": minimum + ((index + 1) * width) if width else maximum,
            "timeslices": 0,
            "calls": 0,
        }
        for index in range(bin_count)
    ]
    for sample, value in zip(usable, values):
        index = 0 if width == 0 else min(int((value - minimum) / width), bin_count - 1)
        bins[index]["timeslices"] += 1
        bins[index]["calls"] += _safe_int(sample.get("calls"))

    def rounded(value: float | None) -> float | None:
        return round(value, 4) if value is not None else None

    return {
        "basis": "call_weighted_timeslice_mean_exec_time",
        "coverage": "all_calls_in_retained_timeslices",
        "exact_execution_percentiles": False,
        "samples": len(usable),
        "calls": total_calls,
        "start_utc": usable[0].get("start_utc"),
        "end_utc": usable[-1].get("end_utc"),
        "min_ms": rounded(minimum),
        "p50_ms": rounded(_weighted_percentile(list(zip(values, [_safe_int(s.get("calls")) for s in usable])), 0.50)),
        "p90_ms": rounded(_weighted_percentile(list(zip(values, [_safe_int(s.get("calls")) for s in usable])), 0.90)),
        "p99_ms": rounded(_weighted_percentile(list(zip(values, [_safe_int(s.get("calls")) for s in usable])), 0.99)),
        "max_ms": rounded(maximum),
        "avg_ms": rounded(total_ms / total_calls) if total_calls else None,
        "bins": [
            {
                **item,
                "lower_ms": rounded(float(item["lower_ms"])),
                "upper_ms": rounded(float(item["upper_ms"])),
            }
            for item in bins
        ],
    }


class TimesliceStore:
    def __init__(self, data_dir: Path, retention_hours: float = 72.0) -> None:
        self.data_dir = data_dir.resolve()
        self.data_dir.mkdir(parents=True, exist_ok=True)
        self.retention_hours = retention_hours
        self._lock = threading.RLock()
        self.database_path = self.data_dir / DATABASE_FILE
        self._conn = sqlite3.connect(self.database_path, timeout=10, check_same_thread=False)
        self._conn.row_factory = sqlite3.Row
        self._initialize()

    @property
    def manifest_path(self) -> Path:
        return self.data_dir / MANIFEST_FILE

    def _initialize(self) -> None:
        with self._lock:
            self._conn.execute("PRAGMA journal_mode=WAL")
            self._conn.execute("PRAGMA synchronous=NORMAL")
            self._conn.execute("PRAGMA foreign_keys=ON")
            self._conn.execute("PRAGMA busy_timeout=10000")
            self._conn.executescript(
                """
                CREATE TABLE IF NOT EXISTS metadata (
                    key TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                );
                CREATE TABLE IF NOT EXISTS timeslices (
                    slice_id TEXT PRIMARY KEY,
                    start_utc TEXT NOT NULL,
                    end_utc TEXT NOT NULL,
                    duration_seconds REAL NOT NULL,
                    totals_json TEXT NOT NULL,
                    payload_zlib BLOB NOT NULL
                );
                CREATE INDEX IF NOT EXISTS timeslices_end_utc_idx
                    ON timeslices(end_utc);
                CREATE TABLE IF NOT EXISTS statement_history (
                    slice_id TEXT NOT NULL REFERENCES timeslices(slice_id) ON DELETE CASCADE,
                    dbid TEXT NOT NULL,
                    userid TEXT NOT NULL,
                    queryid TEXT NOT NULL,
                    toplevel INTEGER NOT NULL,
                    start_utc TEXT NOT NULL,
                    end_utc TEXT NOT NULL,
                    duration_seconds REAL NOT NULL,
                    calls INTEGER NOT NULL,
                    total_ms REAL NOT NULL,
                    mean_ms REAL NOT NULL,
                    PRIMARY KEY (slice_id, dbid, userid, queryid, toplevel)
                );
                CREATE INDEX IF NOT EXISTS statement_history_lookup_idx
                    ON statement_history(dbid, userid, queryid, toplevel, end_utc);
                """
            )
            with self._conn:
                self._conn.execute(
                    "INSERT OR IGNORE INTO metadata(key, value) VALUES ('storage_version', '1')"
                )
                self._conn.execute(
                    "INSERT OR IGNORE INTO metadata(key, value) VALUES ('payload_codec', 'zlib-json')"
                )
            self._migrate_legacy_json()
            self._backfill_manifest_operation_totals()

    @staticmethod
    def _compressed_document(document: dict[str, Any]) -> bytes:
        raw = json.dumps(document, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        return zlib.compress(raw, level=6)

    @staticmethod
    def _decoded_document(payload: bytes) -> dict[str, Any] | None:
        try:
            value = json.loads(zlib.decompress(payload).decode("utf-8"))
            return value if isinstance(value, dict) else None
        except (OSError, UnicodeDecodeError, json.JSONDecodeError, zlib.error):
            return None

    def _insert_document(self, document: dict[str, Any]) -> None:
        add_derived_rate_totals(document)
        slice_id = str(document["slice_id"])
        self._conn.execute(
            """
            INSERT OR REPLACE INTO timeslices
                (slice_id, start_utc, end_utc, duration_seconds, totals_json, payload_zlib)
            VALUES (?, ?, ?, ?, ?, ?)
            """,
            (
                slice_id,
                str(document["start_utc"]),
                str(document["end_utc"]),
                float(document["duration_seconds"]),
                json.dumps(document.get("totals", {}), separators=(",", ":")),
                self._compressed_document(document),
            ),
        )
        self._conn.execute("DELETE FROM statement_history WHERE slice_id = ?", (slice_id,))
        history_rows = []
        for row in document.get("rows", []):
            if not isinstance(row, dict):
                continue
            history_rows.append(
                (
                    slice_id,
                    str(row.get("dbid") or ""),
                    str(row.get("userid") or ""),
                    str(row.get("queryid") or ""),
                    int(bool(row.get("toplevel", True))),
                    str(document["start_utc"]),
                    str(document["end_utc"]),
                    float(document["duration_seconds"]),
                    _safe_int(row.get("calls")),
                    _safe_float(row.get("total_ms")),
                    _safe_float(row.get("mean_ms")),
                )
            )
        self._conn.executemany(
            """
            INSERT INTO statement_history
                (slice_id, dbid, userid, queryid, toplevel, start_utc, end_utc,
                 duration_seconds, calls, total_ms, mean_ms)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            history_rows,
        )

    def _migrate_legacy_json(self) -> None:
        migrated = self._conn.execute(
            "SELECT value FROM metadata WHERE key = 'legacy_json_migrated'"
        ).fetchone()
        if migrated is not None:
            return
        entries: list[dict[str, Any]] = []
        try:
            manifest = json.loads(self.manifest_path.read_text(encoding="utf-8"))
            if isinstance(manifest, dict):
                entries = [row for row in manifest.get("entries", []) if isinstance(row, dict)]
        except (OSError, json.JSONDecodeError):
            pass
        with self._conn:
            for entry in entries:
                path = self._snapshot_path(str(entry.get("file", "")))
                if path is None:
                    continue
                try:
                    document = json.loads(path.read_text(encoding="utf-8"))
                except (OSError, json.JSONDecodeError):
                    continue
                if isinstance(document, dict) and document.get("slice_id"):
                    self._insert_document(document)
            self._conn.execute(
                "INSERT OR REPLACE INTO metadata(key, value) VALUES ('legacy_json_migrated', ?)",
                (iso_utc(utc_now()),),
            )

    def _backfill_manifest_operation_totals(self) -> None:
        migrated = self._conn.execute(
            "SELECT value FROM metadata WHERE key = 'manifest_operation_totals_v2_backfilled'"
        ).fetchone()
        if migrated is not None:
            return
        rows = self._conn.execute(
            "SELECT slice_id, totals_json, payload_zlib FROM timeslices"
        ).fetchall()
        required_keys = set(OPERATION_RATE_KEYS.values()) | set(OPERATION_MEAN_KEYS.values())
        with self._conn:
            for row in rows:
                try:
                    totals = json.loads(row["totals_json"])
                except json.JSONDecodeError:
                    totals = {}
                if isinstance(totals, dict) and required_keys.issubset(totals):
                    continue
                document = self._decoded_document(row["payload_zlib"])
                if document is None:
                    continue
                add_derived_rate_totals(document)
                self._conn.execute(
                    "UPDATE timeslices SET totals_json = ? WHERE slice_id = ?",
                    (json.dumps(document["totals"], separators=(",", ":")), row["slice_id"]),
                )
            self._conn.execute(
                "INSERT OR REPLACE INTO metadata(key, value) "
                "VALUES ('manifest_operation_totals_v2_backfilled', ?)",
                (iso_utc(utc_now()),),
            )

    def close(self) -> None:
        with self._lock:
            self._conn.close()

    def entries(self, after_id: str | None = None) -> list[dict[str, Any]]:
        sql = (
            "SELECT slice_id, start_utc, end_utc, duration_seconds, totals_json "
            "FROM timeslices"
        )
        params: tuple[Any, ...] = ()
        if after_id:
            sql += " WHERE slice_id > ?"
            params = (after_id,)
        sql += " ORDER BY slice_id"
        with self._lock:
            rows = self._conn.execute(sql, params).fetchall()
        return self._entry_rows(rows)

    @staticmethod
    def _entry_rows(rows: Iterable[sqlite3.Row]) -> list[dict[str, Any]]:
        return [
            {
                "id": row["slice_id"],
                "start_utc": row["start_utc"],
                "end_utc": row["end_utc"],
                "duration_seconds": row["duration_seconds"],
                "totals": json.loads(row["totals_json"]),
            }
            for row in rows
        ]

    def latest_entries(self, limit: int) -> list[dict[str, Any]]:
        if isinstance(limit, bool) or limit < 1:
            return []
        with self._lock:
            rows = self._conn.execute(
                "SELECT slice_id, start_utc, end_utc, duration_seconds, totals_json "
                "FROM timeslices ORDER BY slice_id DESC LIMIT ?",
                (int(limit),),
            ).fetchall()
        return self._entry_rows(reversed(rows))

    @staticmethod
    def _bounded_range_limit(limit: int) -> int:
        if (
            isinstance(limit, bool)
            or not isinstance(limit, int)
            or not 1 <= limit <= MAX_TIMESLICE_RANGE
        ):
            raise ValueError(
                f"range limit must be between 1 and {MAX_TIMESLICE_RANGE}"
            )
        return limit

    @staticmethod
    def _validated_time_range(
        after_utc: str, through_utc: str
    ) -> tuple[datetime, datetime]:
        if (
            not isinstance(after_utc, str)
            or not isinstance(through_utc, str)
            or not after_utc
            or not through_utc
        ):
            raise ValueError("invalid timeslice range")
        try:
            after = _parsed_utc(after_utc)
            through = _parsed_utc(through_utc)
        except ValueError:
            raise ValueError("invalid timeslice range") from None
        if after >= through:
            raise ValueError("invalid timeslice range")
        return after, through

    @staticmethod
    def _range_members(
        rows: Iterable[sqlite3.Row],
        *,
        after: datetime,
        through: datetime,
        limit: int,
    ) -> tuple[list[sqlite3.Row], bool]:
        members: list[tuple[datetime, str, sqlite3.Row]] = []
        unreadable_timestamp = False
        for row in rows:
            try:
                end_at = _parsed_utc(str(row["end_utc"]))
            except ValueError:
                unreadable_timestamp = True
                continue
            if after < end_at <= through:
                members.append((end_at, str(row["slice_id"]), row))
        members.sort(key=lambda item: (item[0], item[1]))
        overflow = len(members) > limit or unreadable_timestamp
        return [item[2] for item in members[:limit]], overflow

    def entries_between(
        self, *, after_utc: str, through_utc: str, limit: int
    ) -> tuple[list[dict[str, Any]], bool]:
        """Return bounded metadata for slices whose end is in ``(after, through]``."""
        after, through = self._validated_time_range(after_utc, through_utc)
        bounded_limit = self._bounded_range_limit(limit)
        coarse_after = iso_utc(after - timedelta(seconds=1))
        coarse_through = iso_utc(through + timedelta(seconds=1))
        candidate_limit = bounded_limit + _RANGE_EDGE_ALLOWANCE + 1
        with self._lock:
            rows = self._conn.execute(
                """
                SELECT slice_id, start_utc, end_utc, duration_seconds, totals_json
                FROM timeslices
                WHERE end_utc > ? AND end_utc < ?
                ORDER BY end_utc, slice_id
                LIMIT ?
                """,
                (coarse_after, coarse_through, candidate_limit),
            ).fetchall()
        selected, overflow = self._range_members(
            rows, after=after, through=through, limit=bounded_limit
        )
        overflow = overflow or len(rows) >= candidate_limit
        return self._entry_rows(selected), overflow

    def documents_between(
        self, *, after_utc: str, through_utc: str, limit: int
    ) -> tuple[list[dict[str, Any]], bool, int]:
        """Return bounded decoded slices in ``(after, through]`` without leaking the lock."""
        after, through = self._validated_time_range(after_utc, through_utc)
        bounded_limit = self._bounded_range_limit(limit)
        coarse_after = iso_utc(after - timedelta(seconds=1))
        coarse_through = iso_utc(through + timedelta(seconds=1))
        candidate_limit = bounded_limit + _RANGE_EDGE_ALLOWANCE + 1
        with self._lock:
            rows = self._conn.execute(
                """
                SELECT slice_id, end_utc, payload_zlib
                FROM timeslices
                WHERE end_utc > ? AND end_utc < ?
                ORDER BY end_utc, slice_id
                LIMIT ?
                """,
                (coarse_after, coarse_through, candidate_limit),
            ).fetchall()
        selected, overflow = self._range_members(
            rows, after=after, through=through, limit=bounded_limit
        )
        overflow = overflow or len(rows) >= candidate_limit
        documents: list[dict[str, Any]] = []
        unreadable = 0
        for row in selected:
            document = self._decoded_document(row["payload_zlib"])
            if document is None:
                unreadable += 1
                continue
            documents.append(add_derived_rate_totals(document))
        return documents, overflow, unreadable

    def entry_state(self) -> dict[str, Any]:
        with self._lock:
            row = self._conn.execute(
                "SELECT COUNT(*) AS total, MIN(slice_id) AS oldest_id, MAX(slice_id) AS latest_id "
                "FROM timeslices"
            ).fetchone()
        return {
            "total": int(row["total"]),
            "oldest_id": row["oldest_id"],
            "latest_id": row["latest_id"],
        }

    def _snapshot_path(self, filename: str) -> Path | None:
        if (
            not filename.startswith(SNAPSHOT_PREFIX)
            or Path(filename).name != filename
            or ".." in filename
        ):
            return None
        candidate = (self.data_dir / filename).resolve()
        return candidate if candidate.parent == self.data_dir else None

    def write(self, document: dict[str, Any]) -> None:
        with self._lock:
            with self._conn:
                self._insert_document(document)
                if self.retention_hours > 0:
                    cutoff = iso_utc(utc_now() - timedelta(hours=self.retention_hours))
                    self._conn.execute("DELETE FROM timeslices WHERE end_utc < ?", (cutoff,))

    def read(self, slice_id: str) -> dict[str, Any] | None:
        with self._lock:
            row = self._conn.execute(
                "SELECT payload_zlib FROM timeslices WHERE slice_id = ?", (slice_id,)
            ).fetchone()
        if row is None:
            return None
        document = self._decoded_document(row["payload_zlib"])
        return add_derived_rate_totals(document) if document is not None else None

    def statement_history(
        self, *, dbid: str, userid: str, queryid: str, toplevel: bool
    ) -> list[dict[str, Any]]:
        with self._lock:
            rows = self._conn.execute(
                """
                SELECT slice_id, start_utc, end_utc, duration_seconds, calls, total_ms, mean_ms
                FROM statement_history
                WHERE dbid = ? AND userid = ? AND queryid = ? AND toplevel = ?
                ORDER BY end_utc
                """,
                (str(dbid), str(userid), str(queryid), int(bool(toplevel))),
            ).fetchall()
        return [dict(row) for row in rows]


def source_from_dsn(dsn: str) -> dict[str, str]:
    info = psycopg.conninfo.conninfo_to_dict(dsn)
    return {
        "host": info.get("host", "localhost"),
        "port": info.get("port", "5432"),
        "dbname": info.get("dbname", info.get("database", "")),
        "user": info.get("user", ""),
    }


class CollectorService:
    def __init__(
        self,
        *,
        collector: PgssCollector,
        store: TimesliceStore,
        interval_seconds: float,
        source: dict[str, str],
    ) -> None:
        self.collector = collector
        self.store = store
        self.interval_seconds = interval_seconds
        self.source = source
        self._lock = threading.RLock()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._state = "starting"
        self._baseline_at: datetime | None = None
        self._last_sample_at: datetime | None = None
        self._next_sample_at: datetime | None = None
        self._last_error: str | None = None

    def start(self) -> None:
        self._thread = threading.Thread(target=self._run, name="amp-time-collector", daemon=True)
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=5)
        self.collector.close()
        with self._lock:
            self._state = "stopped"

    def _redact(self, message: str) -> str:
        secrets = [self.collector.password]
        try:
            secrets.append(psycopg.conninfo.conninfo_to_dict(self.collector.dsn).get("password"))
        except Exception:
            pass
        for secret in secrets:
            if secret:
                message = message.replace(secret, "********")
        return message

    def _run(self) -> None:
        previous: Sample | None = None
        while not self._stop.is_set():
            cycle_started = time.monotonic()
            try:
                current = self.collector.collect()
                if previous is not None:
                    self.store.write(build_timeslice(previous, current, self.source))
                previous = current
                with self._lock:
                    self._state = "live"
                    self._last_error = None
                    self._last_sample_at = current.captured_at
                    if self._baseline_at is None:
                        self._baseline_at = current.captured_at
            except Exception as exc:
                previous = None
                with self._lock:
                    self._state = "error"
                    self._last_error = self._redact(f"{type(exc).__name__}: {exc}")
            wait_for = max(0.1, self.interval_seconds - (time.monotonic() - cycle_started))
            with self._lock:
                self._next_sample_at = utc_now() + timedelta(seconds=wait_for)
            self._stop.wait(wait_for)

    def status(self) -> dict[str, Any]:
        with self._lock:
            return {
                "state": self._state,
                "interval_seconds": self.interval_seconds,
                "source": self.source,
                "baseline_at": iso_utc(self._baseline_at) if self._baseline_at else None,
                "last_sample_at": iso_utc(self._last_sample_at) if self._last_sample_at else None,
                "next_sample_at": iso_utc(self._next_sample_at) if self._next_sample_at else None,
                "last_error": self._last_error,
            }
