#!/usr/bin/env python3
"""The split-screen stage: `ycp_demo.py stage`.

Opens two Chrome windows tiled side by side:
  left   the real YCP console on the demo cluster (it can't be framed: the console
         sends X-Frame-Options SAMEORIGIN). The "⟳ YCP auto-refresh" bookmarklet
         on the stage page clicks the console's own Refresh button every 5s.
  right  this page: a real terminal (xterm.js on a pseudo-terminal) that runs the
         shows, with REJ Outdoors' "My Orders" page underneath.
"""

import base64
import fcntl
import json
import os
import pty
import queue
import signal
import struct
import subprocess
import sys
import termios
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import app  # noqa: E402
import ycp_demo as d  # noqa: E402

PORT = int(os.environ.get("STAGE_PORT", "8090"))
COMMANDS = {  # what the stage buttons may run; nothing else is accepted
    "show": ["show", "--pause"],
    "scale": ["scale", "--pause"],
    "extensions": ["extensions", "--pause"],
    "preflight": ["preflight"],
    "reset": ["reset"],
}


def console_url():
    """The console's own link to the demo cluster. A managed account's links all carry the
    account's first environment, whatever region the cluster is in."""
    env = d.cfg["YCP_ENV"]
    if d.MANAGED:
        groups = (d.call("GET") or {}).get("environments") or []
        env = next((g["environment_id"] for g in groups if g.get("environment_id")), "managed")
    return f"{d.cfg['YCP_PORTAL']}/ycp/a/{d.cfg['YCP_ACCOUNT_ID']}/clusters/{env}/{d.CLUSTER}"


# Clicks the console's own Refresh button every REFRESH_MS; click the bookmark again to stop.
REFRESH_MS = 5000
# Re-applied every 10 s: a no-op when already running at this interval, and replaces a
# refresh started with a different one.
AUTO_REFRESH_JS = (
    f"(()=>{{if(window.__ycpAuto&&window.__ycpMs==={REFRESH_MS})return 'on';"
    "if(window.__ycpAuto)clearInterval(window.__ycpAuto);"
    f"window.__ycpMs={REFRESH_MS};"
    "window.__ycpAuto=setInterval(()=>{const b=[...document.querySelectorAll('button')]"
    f".find(x=>x.textContent.trim()==='Refresh');b&&b.click()}},{REFRESH_MS});"
    "if(!document.title.startsWith('⟳'))document.title='⟳ '+document.title;return 'started'})()"
)
BOOKMARKLET = (
    "javascript:(()=>{if(window.__ycpAuto){clearInterval(window.__ycpAuto);window.__ycpAuto=0;"
    "document.title=document.title.replace(/^⟳ /,'');return}"
    "window.__ycpAuto=setInterval(()=>{const b=[...document.querySelectorAll('button')]"
    f".find(x=>x.textContent.trim()==='Refresh');b&&b.click()}},{REFRESH_MS});"
    "document.title='⟳ '+document.title})()"
)


def keep_console_refreshing(stop):
    """Re-apply the auto-refresh to the console window every 10 s: idempotent, and it
    survives a reload. Needs Chrome's View > Developer > Allow JavaScript from Apple Events;
    without it, say so once and leave it to the bookmarklet."""
    url_end = "/" + d.CLUSTER
    js = AUTO_REFRESH_JS.replace('"', '\\"')  # outside the f-string: a backslash there needs 3.12
    script = f'''
    tell application "Google Chrome"
        repeat with w in (every window)
            if URL of active tab of w ends with "{url_end}" then
                return (execute (active tab of w) javascript "{js}")
            end if
        end repeat
        return "no console window"
    end tell'''
    warned = False
    while not stop.is_set():
        r = subprocess.run(["osascript", "-e", script], capture_output=True, text=True)
        if "turned off" in r.stderr and not warned:
            print(
                "console auto-refresh: Chrome blocks JavaScript from Apple Events. Turn on View > Developer >"
                " Allow JavaScript from Apple Events, or click the ⟳ YCP auto-refresh bookmarklet.",
                flush=True,
            )
            warned = True
        elif r.stdout.strip() == "started":
            print(f"console auto-refresh: on (every {REFRESH_MS // 1000} s)", flush=True)
        stop.wait(10)


def _child_setup():
    # Make the pty the child's controlling terminal, or a typed Ctrl-C is just a byte.
    fcntl.ioctl(0, termios.TIOCSCTTY, 0)
    # A stage started in the background by a non-interactive shell runs with SIGINT
    # ignored, which children inherit, and then Python installs no KeyboardInterrupt:
    # ■ stop would do nothing until the 90 s kill, and the show's clean-up never runs.
    signal.signal(signal.SIGINT, signal.SIG_DFL)


class Run:
    """One command on a pseudo-terminal; output fans out to every connected viewer."""

    def __init__(self):
        self.proc, self.master, self.viewers, self.lock = None, None, [], threading.Lock()
        self.size = (40, 110)

    def start(self, name):
        with self.lock:
            if self.proc and self.proc.poll() is None:
                return False
            master, slave = pty.openpty()
            self._winsize(slave)
            self.proc = subprocess.Popen(
                [sys.executable, "-u", os.path.join(HERE, "ycp_demo.py"), *COMMANDS[name]],
                stdin=slave,
                stdout=slave,
                stderr=slave,
                cwd=HERE,
                start_new_session=True,
                preexec_fn=_child_setup,
                env={
                    **os.environ,
                    "TERM": "xterm-256color",
                    "COLUMNS": str(self.size[1]),
                    "LINES": str(self.size[0]),
                },
            )
            os.close(slave)
            self.master = master
            self.broadcast(
                f"\x1b[2J\x1b[H\x1b[2m$ ycp_demo.py {' '.join(COMMANDS[name])}\x1b[0m\r\n".encode()
            )
            threading.Thread(target=self.pump, args=(master,), daemon=True).start()
            return True

    def pump(self, master):
        while True:
            try:
                data = os.read(master, 4096)
            except OSError:
                break
            if not data:
                break
            self.broadcast(data)
        with self.lock:
            if self.master == master:
                self.master = None
            os.close(master)
        self.broadcast(b"\r\n\x1b[2m[finished]\x1b[0m\r\n")

    def broadcast(self, data):
        for q in list(self.viewers):
            q.put(data)

    def write(self, data):
        with self.lock:
            if self.master is not None and self.proc and self.proc.poll() is None:
                os.write(self.master, data)

    def stop(self):
        """Ctrl-C first, so a show's clean-up drops its copies; kill it if that hangs."""
        if not (self.proc and self.proc.poll() is None):
            return False
        proc = self.proc
        os.killpg(proc.pid, signal.SIGINT)

        def reap():
            try:
                proc.wait(timeout=90)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                self.broadcast(
                    b"\r\n\x1b[31m[killed: the next show drops any copies it left]\x1b[0m\r\n"
                )

        threading.Thread(target=reap, daemon=True).start()
        return True

    def resize(self, rows, cols):
        self.size = (rows, cols)
        with self.lock:
            if self.master is not None:
                self._winsize(self.master)
                if self.proc and self.proc.poll() is None:
                    os.killpg(self.proc.pid, signal.SIGWINCH)

    def _winsize(self, fd):
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", self.size[0], self.size[1], 0, 0))


RUN = Run()

PAGE = """<!doctype html><html><head><meta charset="utf-8"><title>YCP demo stage</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/css/xterm.min.css">
<script src="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/lib/xterm.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/@xterm/addon-fit@0.10.0/lib/addon-fit.min.js"></script>
<style>
html, body { margin:0; height:100%; background:#0d1117; color:#c9d1d9; font:13px -apple-system,BlinkMacSystemFont,sans-serif }
body { display:flex; flex-direction:column }
.bar { display:flex; gap:8px; align-items:center; padding:8px 12px; background:#161b22; border-bottom:1px solid #30363d }
.bar b { margin-right:auto; font-size:14px; color:#e6edf3 }
button, .bm { background:#21262d; color:#e6edf3; border:1px solid #30363d; border-radius:6px; padding:5px 10px;
              font:inherit; cursor:pointer; text-decoration:none }
button.go { background:#238636; border-color:#2ea043 }
.bm { border-style:dashed }
#term { flex:62; min-height:0; padding:6px 0 0 8px }
#app { flex:38; min-height:0; border:0; border-top:1px solid #30363d; width:100%; background:#fff }
</style></head><body>
<div class="bar"><b>ycp demo stage</b>
  <a class="bm" href="__BOOKMARKLET__" title="Drag to the bookmarks bar, then click it on the console window">⟳ YCP auto-refresh</a>
  <button onclick="run('preflight')">preflight</button>
  <button class="go" onclick="run('show')">▶ branching show</button>
  <button class="go" onclick="run('scale')">▶ resize show</button>
  <button class="go" onclick="run('extensions')">▶ extensions</button>
  <button onclick="run('reset')">reset main</button>
  <button onclick="fetch('/stop', {method: 'POST'})">■ stop</button>
</div>
<div id="term"></div>
<iframe id="app" src="http://127.0.0.1:__APP_PORT__/"></iframe>
<script>
const term = new Terminal({fontSize: 15, fontFamily: 'Menlo, Monaco, monospace', cursorBlink: true,
  theme: {background: '#0d1117'}, scrollback: 5000});
const fit = new FitAddon.FitAddon(); term.loadAddon(fit); term.open(document.getElementById('term'));
const sendSize = () => { fit.fit(); fetch(`/resize?rows=${term.rows}&cols=${term.cols}`, {method: 'POST'}); };
window.addEventListener('resize', sendSize); sendSize();
term.onData(d => fetch('/input', {method: 'POST', body: d}));
const es = new EventSource('/stream');
es.onmessage = e => term.write(Uint8Array.from(atob(e.data), c => c.charCodeAt(0)));
function run(cmd) { sendSize(); fetch('/run?cmd=' + cmd, {method: 'POST'}).then(r => r.ok || r.text().then(t => term.write('\\r\\n' + t + '\\r\\n'))); term.focus(); }
term.writeln('Pick a show above. In a show, press Enter to move to the next act; Ctrl-C stops it and still drops the copies.');
</script></body></html>"""


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/":
            body = (
                PAGE.replace("__BOOKMARKLET__", BOOKMARKLET.replace('"', "&quot;")).replace(
                    "__APP_PORT__", str(app.PORT)
                )
            ).encode()
            self._send(200, body, "text/html; charset=utf-8")
        elif u.path == "/stream":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()
            q = queue.Queue()
            RUN.viewers.append(q)
            try:
                while True:
                    try:
                        data = q.get(timeout=15)
                        self.wfile.write(b"data: " + base64.b64encode(data) + b"\n\n")
                    except queue.Empty:
                        self.wfile.write(b": keepalive\n\n")
                    self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                pass
            finally:
                RUN.viewers.remove(q)
        else:
            self.send_error(404)

    def do_POST(self):
        # Any page open in the browser can POST to localhost; only the stage itself may
        # start a show or type into its terminal.
        if self.headers.get("Origin") != f"http://127.0.0.1:{PORT}":
            return self._send(403, b"forbidden")
        u = urlparse(self.path)
        qs = parse_qs(u.query)
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        if u.path == "/run":
            name = (qs.get("cmd") or [""])[0]
            if name not in COMMANDS:
                return self._send(400, b"unknown command")
            ok = RUN.start(name)
            return self._send(200 if ok else 409, b"" if ok else b"a show is already running")
        if u.path == "/stop":
            return self._send(200 if RUN.stop() else 409, b"")
        if u.path == "/input":
            RUN.write(body)
            return self._send(204, b"")
        if u.path == "/resize":
            RUN.resize(int(qs["rows"][0]), int(qs["cols"][0]))
            return self._send(204, b"")
        self.send_error(404)

    def _send(self, code, body, ctype="text/plain"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def log_message(self, *_):
        pass


def tile_windows(stage_url):
    """Two Chrome windows, each half the screen: console left, stage right."""
    script = f'''
    tell application "Finder" to set {{_x, _y, w, h}} to bounds of window of desktop
    set half to w div 2
    tell application "Google Chrome"
        set c to make new window
        set URL of active tab of c to "{console_url()}"
        set bounds of c to {{0, 25, half, h}}
        set s to make new window
        set URL of active tab of s to "{stage_url}"
        set bounds of s to {{half, 25, w, h}}
        activate
    end tell'''
    subprocess.run(["osascript", "-e", script], check=False, capture_output=True)


def main(args):
    app.serve(block=False)
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    url = f"http://127.0.0.1:{PORT}/"
    print(f"stage → {url}\nconsole → {console_url()}")
    if "--no-open" not in args:
        tile_windows(url)
        print(
            "Opened two Chrome windows. On the console window, click the ⟳ YCP auto-refresh bookmarklet "
            "(drag it from the stage's top bar to your bookmarks bar first)."
        )
    stop = threading.Event()
    threading.Thread(target=keep_console_refreshing, args=(stop,), daemon=True).start()
    print("Ctrl-C to stop.")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
