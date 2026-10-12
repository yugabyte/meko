#!/usr/bin/env python3
"""REJ Outdoors' "My Orders" page, served from the demo's main branch.

  python3 app.py            then open http://127.0.0.1:8080

It runs exactly ycp_demo.QUERY, the query `show` diagnoses and fixes, and shows how
long it took. The page refreshes itself, so when `show` ships the fix to main the
badge goes from red to green with nobody touching the browser. Times are measured
from this machine, so after the fix they are mostly the VPN round trip.
"""

import html
import os
import re
import shutil
import subprocess
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ycp_demo as d  # noqa: E402

PORT = int(os.environ.get("APP_PORT", "8080"))
REFRESH_S = 5  # each refresh is one run of a slow query on a 0.5 vCPU endpoint; don't hammer it
ADDR = None


def run(sql):
    """One psql session: rows of the last statement, and its client-measured ms."""
    host, port = ADDR.rsplit(":", 1)
    out = subprocess.run(
        [
            shutil.which("psql") or "psql",
            f"host={host} port={port} user=cloud_admin dbname=postgres sslmode=disable",
            "-AtXq",
            "-F",
            "\t",
            "-c",
            "\\timing on",
            "-c",
            sql,
        ],
        capture_output=True,
        text=True,
        timeout=60,
        env={**os.environ, "PGPASSWORD": d.pg_password(), "PGCONNECT_TIMEOUT": "10"},
    )
    if out.returncode:
        raise RuntimeError(out.stderr.strip().splitlines()[-1])
    lines = out.stdout.splitlines()
    ms = next((float(m.group(1)) for l in lines if (m := re.match(r"Time: ([\d.]+) ms", l))), None)
    return [l.split("\t") for l in lines if l and not l.startswith("Time:")], ms


def page():
    customer, _ = run(f"SELECT name, city FROM customers WHERE id = {d.CUSTOMER_ID}")
    orders, ms = run(d.QUERY)  # the page's real query, timed
    ids = ",".join(o[0] for o in orders) or "0"
    items, _ = run(
        f"SELECT i.order_id, min(p.name), count(*) FROM order_items i JOIN products p "
        f"ON p.id = i.product_id WHERE i.order_id IN ({ids}) GROUP BY 1"
    )
    what = {i[0]: i[1] + (f" + {int(i[2]) - 1} more" if int(i[2]) > 1 else "") for i in items}
    name, city = customer[0] if customer else ("Customer", "")
    slow = ms is None or ms > 100
    badge = (
        f'<div class="badge {"slow" if slow else "fast"}">'
        f"{'🐢' if slow else '⚡'} orders loaded in <b>{ms:,.0f} ms</b></div>"
        if ms is not None
        else ""
    )
    rows = "".join(
        f"<tr><td>#{html.escape(o[0])}</td><td>{html.escape(o[1][:10])}</td>"
        f"<td>{html.escape(what.get(o[0], ''))}</td>"
        f"<td><span class='st {html.escape(o[2])}'>{html.escape(o[2])}</span></td>"
        f"<td class='num'>${float(o[3]):,.2f}</td></tr>"
        for o in orders
    )
    return f"""<!doctype html><html><head><meta charset="utf-8">
<meta http-equiv="refresh" content="{REFRESH_S}">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>My Orders · {d.STORE}</title>
<style>
:root {{ --ink:#1d2a24; --muted:#6b7a72; --line:#e6ece8; --bg:#f6f8f4; --brand:#2f6b4f; }}
* {{ box-sizing:border-box }}
body {{ margin:0; font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; background:var(--bg); color:var(--ink) }}
header {{ background:var(--brand); color:#fff; padding:14px 24px; display:flex; align-items:center; gap:12px }}
header b {{ font-size:19px; letter-spacing:.3px }} header span {{ opacity:.8 }}
main {{ max-width:900px; margin:28px auto; padding:0 16px }}
h1 {{ font-size:24px; margin:0 0 4px }} .sub {{ color:var(--muted); margin-bottom:18px }}
.badge {{ display:inline-block; padding:10px 18px; border-radius:999px; font-size:20px; margin-bottom:18px }}
.slow {{ background:#fde8e6; color:#a3271b }} .fast {{ background:#e3f5ea; color:#1d6b3e }}
table {{ width:100%; border-collapse:collapse; background:#fff; border:1px solid var(--line); border-radius:10px; overflow:hidden }}
th, td {{ padding:10px 14px; border-bottom:1px solid var(--line); text-align:left }}
th {{ font-size:12px; text-transform:uppercase; letter-spacing:.6px; color:var(--muted); background:#fafcfa }}
.num {{ text-align:right; font-variant-numeric:tabular-nums }}
.st {{ font-size:12px; padding:2px 8px; border-radius:6px; background:#eef2ef }}
.st.processing, .st.shipped {{ background:#e6effc; color:#1f4e9c }} .st.returned, .st.cancelled {{ background:#f3eee6; color:#7a5a1d }}
footer {{ color:var(--muted); font-size:12px; margin-top:14px }}
@media (max-width:600px) {{ td:nth-child(3), th:nth-child(3) {{ display:none }} }}
</style></head><body>
<header><b>⛰ {d.STORE}</b><span>{d.TAGLINE} · since 1987</span></header>
<main>
<h1>My Orders</h1>
<div class="sub">Signed in as {html.escape(name)} · {html.escape(city)}</div>
{badge}
<table><thead><tr><th>Order</th><th>Placed</th><th>Items</th><th>Status</th><th class="num">Total</th></tr></thead>
<tbody>{rows}</tbody></table>
<footer>Live from the <code>main</code> branch of <code>{d.cfg["DEMO_CLUSTER"]}</code> · refreshes every {REFRESH_S}s ·
time measured from this machine</footer>
</main></body></html>"""


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path not in ("/", "/index.html"):
            self.send_error(404)
            return
        try:
            body, code = page(), 200
        except Exception as e:  # noqa: BLE001 -- show the error on the page, keep serving
            body = (
                f"<!doctype html><meta http-equiv='refresh' content='{REFRESH_S}'>"
                f"<p style='font:16px sans-serif;padding:24px'>Can't reach the database: {html.escape(str(e))}</p>"
            )
            code = 503
        data = body.encode()
        self.send_response(code)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_):
        pass


def serve(block=True):
    global ADDR
    ADDR, _ = d.main_endpoint()
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(f"{d.STORE} · My Orders → http://127.0.0.1:{PORT}  (main at {ADDR})")
    if block:
        srv.serve_forever()
    else:
        import threading

        threading.Thread(target=srv.serve_forever, daemon=True).start()


if __name__ == "__main__":
    serve()
