from __future__ import annotations

import gzip
import json
import logging
import mimetypes
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, unquote, urlparse

from amp_time import __version__
from amp_time.collector import CollectorService, TimesliceStore, build_latency_distribution
from amp_time.inspector import InspectionError, LiveInspector


def web_dir() -> Path:
    return Path(__file__).resolve().parent / "web"


class AmpHTTPRequestHandler(BaseHTTPRequestHandler):
    service: CollectorService
    store: TimesliceStore
    inspector: LiveInspector

    def log_message(self, fmt: str, *args: object) -> None:
        logging.getLogger("amp_time.http").info("%s - %s", self.address_string(), fmt % args)

    def _bytes(self, payload: bytes, content_type: str, status: int = 200) -> None:
        compressed = False
        if len(payload) >= 1024 and "gzip" in self.headers.get("Accept-Encoding", "").lower():
            candidate = gzip.compress(payload, compresslevel=5)
            if len(candidate) < len(payload):
                payload = candidate
                compressed = True
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
        self.send_header("Referrer-Policy", "no-referrer")
        self.send_header("X-Frame-Options", "DENY")
        if compressed:
            self.send_header("Content-Encoding", "gzip")
            self.send_header("Vary", "Accept-Encoding")
        self.end_headers()
        try:
            self.wfile.write(payload)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _json(self, value: object, status: int = 200) -> None:
        self._bytes(
            json.dumps(value, separators=(",", ":"), ensure_ascii=False).encode("utf-8"),
            "application/json; charset=utf-8",
            status,
        )

    def _static(self, name: str) -> None:
        if not name or "/" in name or ".." in name:
            self._json({"error": "not found"}, 404)
            return
        path = web_dir() / name
        if not path.is_file():
            self._json({"error": "not found"}, 404)
            return
        content_type = mimetypes.guess_type(str(path))[0] or "application/octet-stream"
        if content_type.startswith("text/") or content_type == "application/javascript":
            content_type += "; charset=utf-8"
        self._bytes(path.read_bytes(), content_type)

    def _request_json(self) -> dict[str, object]:
        try:
            size = int(self.headers.get("Content-Length", "0"))
        except ValueError as exc:
            raise InspectionError("Invalid request length.") from exc
        if size <= 0 or size > 16_384:
            raise InspectionError("Inspection requests must be between 1 byte and 16 KiB.")
        try:
            value = json.loads(self.rfile.read(size).decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise InspectionError("Inspection request is not valid JSON.") from exc
        if not isinstance(value, dict):
            raise InspectionError("Inspection request must be a JSON object.")
        return value

    def _stored_statement(self, request: dict[str, object]) -> dict[str, object]:
        slice_id = str(request.get("slice_id") or "")
        queryid = str(request.get("queryid") or "")
        dbid = str(request.get("dbid") or "")
        userid = str(request.get("userid") or "")
        toplevel = request.get("toplevel")
        dbname = str(request.get("dbname") or "")
        username = str(request.get("username") or "")
        if not slice_id or not queryid or not dbid or not userid or not dbname or not isinstance(toplevel, bool):
            raise InspectionError("slice_id, dbid, userid, queryid, toplevel, and dbname are required.")
        document = type(self).store.read(slice_id)
        if document is None:
            raise InspectionError("The selected timeslice is no longer available.")
        candidates = [
            row
            for row in document.get("rows", [])
            if isinstance(row, dict)
            and str(row.get("queryid") or "") == queryid
            and str(row.get("dbid") or "") == dbid
            and str(row.get("userid") or "") == userid
            and bool(row.get("toplevel", True)) is toplevel
            and str(row.get("dbname") or "") == dbname
            and (not username or str(row.get("username") or "") == username)
        ]
        if len(candidates) != 1:
            raise InspectionError("The selected statement could not be resolved uniquely.")
        return candidates[0]

    def do_GET(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        path = unquote(parsed.path)
        if path in ("/", "/index.html"):
            html = (web_dir() / "index.html").read_text(encoding="utf-8")
            self._bytes(
                html.replace("__VERSION__", f"v{__version__}").encode("utf-8"),
                "text/html; charset=utf-8",
            )
            return
        if path in ("/app.js", "/style.css"):
            self._static(path[1:])
            return
        if path == "/api/manifest":
            query = parse_qs(parsed.query)
            after = str(query.get("after", [""])[0] or "")
            self._json(
                {
                    "format_version": 2,
                    "entries": type(self).store.entries(after_id=after or None),
                    "entry_state": type(self).store.entry_state(),
                    "status": type(self).service.status(),
                }
            )
            return
        if path.startswith("/api/slices/"):
            slice_id = path[len("/api/slices/") :]
            if not slice_id or "/" in slice_id or ".." in slice_id:
                self._json({"error": "invalid slice id"}, 400)
                return
            value = type(self).store.read(slice_id)
            if value is None:
                self._json({"error": "timeslice not found"}, 404)
            else:
                self._json(value)
            return
        if path == "/api/health":
            status = type(self).service.status()
            self._json(status, 200 if status["state"] in ("starting", "live") else 503)
            return
        self._json({"error": "not found"}, 404)

    def do_POST(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        path = unquote(parsed.path)
        if path != "/api/inspect":
            self._json({"error": "not found"}, 404)
            return
        try:
            request = self._request_json()
            statement = self._stored_statement(request)
            with ThreadPoolExecutor(max_workers=2, thread_name_prefix="amp-time-inspection") as pool:
                inspection_future = pool.submit(type(self).inspector.inspect, statement)
                history_future = pool.submit(
                    type(self).store.statement_history,
                    dbid=str(statement.get("dbid") or ""),
                    userid=str(statement.get("userid") or ""),
                    queryid=str(statement.get("queryid") or ""),
                    toplevel=bool(statement.get("toplevel", True)),
                )
                inspection = inspection_future.result()
                inspection["latency"] = build_latency_distribution(history_future.result())
            self._json(inspection)
        except InspectionError as exc:
            self._json({"error": "live inspection failed", "detail": str(exc)}, 422)
        except Exception:
            logging.getLogger("amp_time.http").exception("Unexpected live inspection failure")
            self._json(
                {
                    "error": "live inspection failed",
                    "detail": "An unexpected local server error occurred while inspecting the statement.",
                },
                500,
            )


def make_server(
    *,
    host: str,
    port: int,
    service: CollectorService,
    store: TimesliceStore,
    inspector: LiveInspector,
) -> ThreadingHTTPServer:
    AmpHTTPRequestHandler.service = service
    AmpHTTPRequestHandler.store = store
    AmpHTTPRequestHandler.inspector = inspector
    return ThreadingHTTPServer((host, port), AmpHTTPRequestHandler)
