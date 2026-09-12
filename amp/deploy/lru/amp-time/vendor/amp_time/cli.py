from __future__ import annotations

import argparse
import os
from pathlib import Path

from amp_time import __version__
from amp_time.collector import (
    CollectorService,
    PgssCollector,
    TimesliceStore,
    source_from_dsn,
)
from amp_time.server import make_server
from amp_time.inspector import LiveInspector


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(
        prog="amp-time",
        description="Serve 10-second PostgreSQL pg_stat_statements timeslices in a browser.",
    )
    result.add_argument("--version", action="version", version=f"%(prog)s {__version__}")
    result.add_argument("--dsn", help="PostgreSQL DSN (or AMP_TIME_DSN / DATABASE_URL)")
    result.add_argument("--password", help="Password (or AMP_TIME_PASSWORD / PGPASSWORD)")
    result.add_argument("--interval", type=float, default=10.0, help="sample interval in seconds")
    result.add_argument("--bind", default="127.0.0.1", help="HTTP bind address")
    result.add_argument("--port", type=int, default=8765, help="HTTP port")
    result.add_argument("--data-dir", default="amp-time-data", help="persistent timeslice directory")
    result.add_argument(
        "--retention-hours",
        type=float,
        default=72.0,
        help="timeslice retention in hours; 0 keeps everything",
    )
    return result


def main() -> None:
    args = parser().parse_args()
    if args.interval <= 0:
        raise SystemExit("--interval must be greater than zero")
    if not 1 <= args.port <= 65535:
        raise SystemExit("--port must be between 1 and 65535")
    if args.retention_hours < 0:
        raise SystemExit("--retention-hours must be zero or greater")
    dsn = args.dsn or os.environ.get("AMP_TIME_DSN") or os.environ.get("DATABASE_URL")
    if not dsn:
        raise SystemExit("Provide --dsn or set AMP_TIME_DSN / DATABASE_URL")
    password = args.password or os.environ.get("AMP_TIME_PASSWORD") or os.environ.get("PGPASSWORD")
    source = source_from_dsn(dsn)
    store = TimesliceStore(Path(args.data_dir), retention_hours=args.retention_hours)
    collector = PgssCollector(dsn, password=password)
    inspector = LiveInspector(dsn, password=password)
    service = CollectorService(
        collector=collector,
        store=store,
        interval_seconds=args.interval,
        source=source,
    )
    httpd = make_server(
        host=args.bind,
        port=args.port,
        service=service,
        store=store,
        inspector=inspector,
    )
    url = f"http://{args.bind}:{args.port}/"
    target = f"{source['user']}@{source['host']}:{source['port']}/{source['dbname']}"
    print(f"amp-time {__version__}", flush=True)
    print(f"target: {target}", flush=True)
    print(f"sampling: every {args.interval:g}s (first timeslice after one interval)", flush=True)
    print(f"data: {store.database_path} (SQLite, zlib payloads)", flush=True)
    print(f"retention: {args.retention_hours:g}h", flush=True)
    print(f"UI: {url}", flush=True)
    service.start()
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        print("\nShutting down.", flush=True)
    finally:
        httpd.server_close()
        service.stop()
        store.close()


if __name__ == "__main__":
    main()
