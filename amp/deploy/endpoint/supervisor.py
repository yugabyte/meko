#!/usr/bin/env python3
"""yb-amp single-container endpoint supervisor.

Brings up, in one container, the services that make up a yb-amp endpoint
and supervises them (basic monitor + restart-with-backoff). Runs as PID 1
under tini.

Services (in start order):
  1. endpoint_storage   - LFC scratch blobs           (YugabyteDB-backed)
  2. safekeeper         - WAL                          (YugabyteDB-backed)
  3. yb-pageserver      - page service                 (YugabyteDB-backed)
  4. compute_ctl        - supervises Postgres v18 + the `yb_amp` extension

No control plane runs here: it is a shared, cluster-level service
plane that lives in the controller layer. This container assumes the `yb_storage`
schema already exists: the controller provisions it on startup, and it is the
only thing that does. There is no self-provisioning mode.

Config comes from environment variables (delivered via a ConfigMap + Secret
in the Helm chart). See the chart's values.yaml / README for the contract.
"""

import hashlib
import json
import os
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

# ---------------------------------------------------------------------------
# Config (environment contract — matches the Helm ConfigMap keys)
# ---------------------------------------------------------------------------

RUN_DIR = Path(os.environ.get("RUN_DIR", "/data"))
BIN_DIR = Path(os.environ.get("BIN_DIR", "/usr/local/bin"))
PG_DISTRIB_DIR = os.environ.get("PG_DISTRIB_DIR", "/usr/local")


def env(name, default=None):
    v = os.environ.get(name)
    return v if v not in (None, "") else default


def env_int(name, default):
    v = env(name)
    return int(v) if v is not None else default


def log(msg):
    print(f"[supervisor] {msg}", flush=True)


# ---------------------------------------------------------------------------
# Resource introspection — the inputs every derived setting is sized from.
#
# The cgroup is preferred over the env values on purpose: the controller
# resizes endpoints *in place* (the `pods/resize` subresource, see
# `classifyResize` in controller/internal/deploy/kubernetes.go), which updates
# the cgroup limits of a running container without restarting it and without
# re-rendering the ConfigMap. After such a resize ENDPOINT_CPU / ENDPOINT_MEMORY
# still carry the values the pod started with, so anything derived from them
# would silently describe the old size. The env values remain the fallback for
# environments with no cgroup limit (plain Docker, local runs).
# ---------------------------------------------------------------------------

CGROUP_ROOT = Path(os.environ.get("CGROUP_ROOT", "/sys/fs/cgroup"))

_MEM_SUFFIXES = {
    "k": 1000, "K": 1000, "m": 1000**2, "M": 1000**2, "g": 1000**3, "G": 1000**3,
    "t": 1000**4, "T": 1000**4,
    "Ki": 1024, "Mi": 1024**2, "Gi": 1024**3, "Ti": 1024**4,
}


def parse_memory_quantity(s):
    """Kubernetes memory quantity ("512Mi", "4Gi", "2G", "1073741824") -> bytes.

    Returns None on anything unparseable so callers can fall back rather than
    size a cache off a typo.
    """
    if s is None:
        return None
    s = str(s).strip()
    if not s:
        return None
    for suffix in ("Ki", "Mi", "Gi", "Ti"):
        if s.endswith(suffix):
            try:
                return int(float(s[: -len(suffix)]) * _MEM_SUFFIXES[suffix])
            except ValueError:
                return None
    if s[-1] in _MEM_SUFFIXES:
        try:
            return int(float(s[:-1]) * _MEM_SUFFIXES[s[-1]])
        except ValueError:
            return None
    try:
        return int(float(s))
    except ValueError:
        return None


def parse_cpu_quantity(s):
    """Kubernetes CPU quantity ("2", "1500m", "0.5") -> float cores, or None."""
    if s is None:
        return None
    s = str(s).strip()
    if not s:
        return None
    try:
        if s.endswith("m"):
            return float(s[:-1]) / 1000.0
        return float(s)
    except ValueError:
        return None


def _read_first_line(path):
    try:
        return path.read_text().strip()
    except OSError:
        return None


def cgroup_memory_limit_bytes():
    """The container's memory ceiling from cgroup v2/v1, or None if unlimited.

    cgroup v2 writes the literal "max" when no limit is set; v1 reports a
    sentinel near 2^63, so anything absurdly large is treated as unlimited.
    """
    raw = _read_first_line(CGROUP_ROOT / "memory.max")
    if raw is None:
        raw = _read_first_line(CGROUP_ROOT / "memory" / "memory.limit_in_bytes")
    if raw in (None, "max"):
        return None
    try:
        val = int(raw)
    except ValueError:
        return None
    # Treat the "no limit" sentinels (and anything past a petabyte) as unset.
    return val if 0 < val < (1 << 50) else None


def cgroup_cpu_limit():
    """The container's CPU quota in cores from cgroup v2/v1, or None."""
    raw = _read_first_line(CGROUP_ROOT / "cpu.max")
    if raw is not None:
        parts = raw.split()
        if len(parts) == 2 and parts[0] != "max":
            try:
                quota, period = int(parts[0]), int(parts[1])
                if quota > 0 and period > 0:
                    return quota / period
            except ValueError:
                return None
        return None
    quota = _read_first_line(CGROUP_ROOT / "cpu" / "cpu.cfs_quota_us")
    period = _read_first_line(CGROUP_ROOT / "cpu" / "cpu.cfs_period_us")
    try:
        if quota is not None and period is not None:
            q, p = int(quota), int(period)
            if q > 0 and p > 0:
                return q / p
    except ValueError:
        pass
    return None


def volume_bytes(path):
    """Total size of the filesystem backing `path`, or None if unavailable."""
    try:
        st = os.statvfs(path)
    except OSError:
        return None
    total = st.f_blocks * st.f_frsize
    return total if total > 0 else None


# YugabyteDB connection (the external durable backend).
YB_HOST_PORTS = env("YB_HOST_PORTS", "127.0.0.1:5433")
YB_USER = env("YB_USER", "yugabyte")
YB_DBNAME = env("YB_DBNAME", "yb_storage")
YB_SSLMODE = env("YB_SSLMODE")
YB_SSLROOTCERT = env("YB_SSLROOTCERT")
# Name of the env var holding the YB password (matches YbConnConfig.password_env).
YB_PASSWORD_ENV = env("YB_PASSWORD_ENV")
if YB_PASSWORD_ENV is None and env("YB_PASSWORD") is not None:
    YB_PASSWORD_ENV = "YB_PASSWORD"

# Endpoint / compute.
# Major version only, and only used to locate the Postgres install on disk
# (`pg_install/v{N}/{bin,lib}`). The image ships v18 and nothing else, so a
# default of anything else would point PG_BIN at a directory that isn't there.
# It is also what the safekeeper create is told, as a full PgVersionId.
PG_VERSION = env("ENDPOINT_PG_VERSION", "18")
COMPUTE_PORT = env_int("COMPUTE_PORT", 55433)
COMPUTE_CTL_HTTP_PORT = env_int("COMPUTE_CTL_HTTP_PORT", 3080)
COMPUTE_PASSWORD = env("COMPUTE_PASSWORD", "cloud_admin")
SHARED_PRELOAD_LIBRARIES = env("SHARED_PRELOAD_LIBRARIES", "yb_amp")
BRANCH_ID = env("BRANCH_ID")


def _valid_uuid(s):
    """`s` if it is a well-formed UUID, else None.

    The endpoint id is interpolated into a `psql -c` statement (which takes no
    bind parameters), so it is checked before use rather than trusted.
    """
    if not s:
        return None
    try:
        return str(uuid.UUID(str(s)))
    except (ValueError, AttributeError, TypeError):
        log(f"WARNING: ENDPOINT_ID {s!r} is not a UUID; ignoring it")
        return None


# This endpoint's row in `amp_registry.endpoints`, the source of operator
# overrides. Absent in a standalone dev container, which then runs on derived
# defaults only.
ENDPOINT_ID = _valid_uuid(env("ENDPOINT_ID"))

# Operator overrides for the three config surfaces, filled in by `main` from
# `fetch_overrides()` before any config file is rendered. Module-level and
# mutable rather than fetched at import time, so importing this module (tests,
# tooling) performs no database I/O.
OVERRIDES = {"pg": {}, "pageserver": {}, "safekeeper": {}}

# --- Resources the derived settings are sized from -------------------------
# ENDPOINT_CPU / ENDPOINT_MEMORY mirror the pod's resource reservation (set by
# the chart from .Values.endpoint.resources). They are the fallback; the cgroup
# is the truth — see the note above `CGROUP_ROOT`.
ENDPOINT_MEMORY_BYTES = cgroup_memory_limit_bytes() or parse_memory_quantity(
    env("ENDPOINT_MEMORY")
)
ENDPOINT_VCPU = cgroup_cpu_limit() or parse_cpu_quantity(env("ENDPOINT_CPU"))
# The /data budget, from the chart. Unlike memory and CPU this cannot be read
# from inside the container on the emptyDir path: the kubelet enforces an
# emptyDir's sizeLimit by watching usage rather than with a filesystem quota, so
# statvfs reports the whole node filesystem there. Whatever the volume claims,
# the cache must stay inside this.
ENDPOINT_DATA_VOLUME_BYTES = parse_memory_quantity(env("ENDPOINT_DATA_VOLUME_SIZE"))

# Internal service ports.
PAGESERVER_PG_PORT = env_int("PAGESERVER_PG_PORT", 64000)
PAGESERVER_HTTP_PORT = env_int("PAGESERVER_HTTP_PORT", 9898)
PAGESERVER_GRPC_PORT = env_int("PAGESERVER_GRPC_PORT", 51051)
SAFEKEEPER_PG_PORT = env_int("SAFEKEEPER_PG_PORT", 5454)
SAFEKEEPER_HTTP_PORT = env_int("SAFEKEEPER_HTTP_PORT", 7676)
ENDPOINT_STORAGE_HTTP_PORT = env_int("ENDPOINT_STORAGE_HTTP_PORT", 9993)

LISTEN_HOST = "127.0.0.1"


# The pageserver requires `control_plane_api` to be set ("pageservers cannot
# run in isolation"), but this fork's pageserver no longer calls back to it at
# runtime (controller_upcall_client was removed), so a placeholder is fine in
# the controller-free pod. Point it at the controller's surface if you have one.
CONTROL_PLANE_API = env("CONTROL_PLANE_API", "http://localhost:6666")

# Controller coordination (opt-in). Left unset, the endpoint stays
# controller-free and self-manages (the default). The pageserver's generation is
# not part of this either way: the process claims its own from
# `amp_storage.branch_state` at startup.
CONTROLLER_URL = env("CONTROLLER_URL")
USE_CONTROLLER = CONTROLLER_URL is not None
# Listen address for the pageserver/safekeeper: 0.0.0.0 under controller
# coordination (so published host ports route in), loopback otherwise. Internal
# callers (compute, the supervisor's own probes) keep using 127.0.0.1, which a
# 0.0.0.0 listener also accepts.
BIND_HOST = "0.0.0.0" if USE_CONTROLLER else LISTEN_HOST

# Supervisor behaviour.
RESTART_BACKOFF = env_int("SUPERVISOR_RESTART_BACKOFF_SECONDS", 2)
MAX_RESTARTS = env_int("SUPERVISOR_MAX_RESTARTS", 5)
RESTART_WINDOW = env_int("SUPERVISOR_RESTART_WINDOW_SECONDS", 180)


PG_BIN = Path(PG_DISTRIB_DIR) / f"v{PG_VERSION}" / "bin"
PG_LIB = Path(PG_DISTRIB_DIR) / f"v{PG_VERSION}" / "lib"

# The Postgres-family binaries (postgres, psql, initdb) and the compute's
# walredo child link against pg_install/v{N}/lib (libpq.so.5, ...). Put it on
# the loader path for this process and everything it spawns.
_existing_ld = os.environ.get("LD_LIBRARY_PATH", "")
os.environ["LD_LIBRARY_PATH"] = str(PG_LIB) + (f":{_existing_ld}" if _existing_ld else "")


# ---------------------------------------------------------------------------
# YB connection rendering
# ---------------------------------------------------------------------------

def yb_conn_dict():
    d = {"host_ports": YB_HOST_PORTS, "user": YB_USER, "dbname": YB_DBNAME}
    if YB_PASSWORD_ENV:
        d["password_env"] = YB_PASSWORD_ENV
    if YB_SSLMODE:
        d["sslmode"] = YB_SSLMODE
    if YB_SSLROOTCERT:
        d["sslrootcert"] = YB_SSLROOTCERT
    return d


def _toml_quote(s):
    return '"' + str(s).replace("\\", "\\\\").replace('"', '\\"') + '"'


def yb_conn_inline_toml():
    """Inline TOML table form, e.g. { host_ports = "...", user = "..." }."""
    parts = [f"{k} = {_toml_quote(v)}" for k, v in yb_conn_dict().items()]
    return "{ " + ", ".join(parts) + " }"


def yb_conn_typed_json():
    """JSON object with the YbStore discriminator, for endpoint_storage."""
    d = {"type": "YbStore"}
    d.update(yb_conn_dict())
    return d


# ---------------------------------------------------------------------------
# Small HTTP + wait helpers (stdlib only)
# ---------------------------------------------------------------------------

def http_request(method, url, body=None, timeout=10):
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        raw = resp.read()
        return resp.status, (json.loads(raw) if raw else None)


def tcp_open(host, port):
    try:
        with socket.create_connection((host, port), timeout=1):
            return True
    except OSError:
        return False


def wait_for(desc, predicate, timeout, interval=0.5):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if predicate():
                return True
        except Exception:
            pass
        time.sleep(interval)
    raise TimeoutError(f"timed out waiting for {desc} ({timeout}s)")


def psql(dbname, sql=None, file=None, ignore_errors=False, args=None):
    """Run psql against the external YugabyteDB using the bundled client.

    `args` appends extra psql flags (e.g. ["-t", "-A"] for a bare single value).
    """
    cmd = [
        str(PG_BIN / "psql"),
        "-h", YB_HOST_PORTS.split(",")[0].split(":")[0],
        "-p", YB_HOST_PORTS.split(",")[0].split(":")[1] if ":" in YB_HOST_PORTS.split(",")[0] else "5433",
        "-U", YB_USER,
        "-d", dbname,
        "-v", "ON_ERROR_STOP=1",
        "--no-psqlrc",
    ]
    if args:
        cmd += list(args)
    if sql is not None:
        cmd += ["-c", sql]
    if file is not None:
        cmd += ["-f", str(file)]
    penv = dict(os.environ)
    if YB_PASSWORD_ENV and os.environ.get(YB_PASSWORD_ENV):
        penv["PGPASSWORD"] = os.environ[YB_PASSWORD_ENV]
    res = subprocess.run(cmd, env=penv, capture_output=True, text=True)
    if res.returncode != 0 and not ignore_errors:
        raise RuntimeError(f"psql failed: {res.stderr.strip()}")
    return res


# ---------------------------------------------------------------------------
# Config file rendering
# ---------------------------------------------------------------------------
# NOTE: the exact compute spec shape is verified against `yb_amp_local`-generated
# configs during bring-up testing.

def fetch_overrides():
    """Read this endpoint's operator overrides from YugabyteDB.

    Returns `{"pg": {...}, "pageserver": {...}, "safekeeper": {...}}`, empty
    objects when there is nothing to apply.

    The overrides live in `amp_registry.endpoints` — the row the controller
    already owns for this endpoint — rather than being baked into the pod's
    ConfigMap, because a ConfigMap is not re-read by a running container and env
    never hot-updates. Reading them from the database is what lets a later change
    be applied without recreating the pod.

    Failure is not fatal. An endpoint that cannot reach the registry (or has no
    ENDPOINT_ID plumbed, as in a standalone dev container) must still start on its
    derived defaults; refusing to boot because an override *might* exist would
    turn a registry blip into an outage.

    Narrow by design: one row, three columns, keyed by primary key. `amp_registry`
    is the controller's own schema and the pod only reaches it because both
    connect as the same superuser today — so the less of it this depends on, the
    cheaper a future least-privilege role is to introduce.
    """
    empty = {"pg": {}, "pageserver": {}, "safekeeper": {}}
    if not ENDPOINT_ID:
        log("no ENDPOINT_ID; skipping the override lookup (derived defaults only)")
        return empty
    # ENDPOINT_ID is validated as a UUID at parse time (see ENDPOINT_ID above), so
    # it is safe to inline here — psql's -c takes no bind parameters.
    # Two rows, one query. The branch carries the settings every compute on it
    # gets; the endpoint carries its own. `||` on jsonb takes the RIGHT operand
    # for a key both define, which is the precedence the schema documents — "an
    # endpoint's own pg_settings merge over these". LEFT JOIN so a missing branch
    # row still yields the endpoint's settings rather than nothing at all.
    #
    # This is the only carrier for user GUCs: the chart no longer ships them (it
    # keeps only the extension-derived paths it computes from the ImageVolumes it
    # mounted), so a failed read means the compute comes up on derived defaults
    # alone. That is logged loudly below rather than being fatal — an endpoint
    # that cannot reach the registry should still start.
    sql = (
        "SELECT jsonb_build_object("
        "'pg', coalesce(b.pg_settings,'{}'::jsonb) || coalesce(e.pg_settings,'{}'::jsonb),"
        "'pageserver', coalesce(e.pageserver_settings,'{}'::jsonb),"
        "'safekeeper', coalesce(e.safekeeper_settings,'{}'::jsonb)) "
        "FROM amp_registry.endpoints e "
        "LEFT JOIN amp_registry.branches b ON b.branch_id = e.branch_id "
        f"WHERE e.id = '{ENDPOINT_ID}'"
    )
    try:
        # -t (tuples only) -A (unaligned) so stdout is exactly the JSON document.
        res = psql(YB_DBNAME, sql=sql, ignore_errors=True, args=["-t", "-A"])
    except Exception as e:  # noqa: BLE001 - any failure falls back to defaults
        log(f"WARNING: override lookup failed ({e}); using derived defaults only")
        return empty
    if res.returncode != 0:
        log(f"WARNING: override lookup failed ({res.stderr.strip()}); "
            "using derived defaults only")
        return empty
    raw = res.stdout.strip()
    if not raw:
        log(f"no endpoints row for {ENDPOINT_ID}; using derived defaults only")
        return empty
    try:
        doc = json.loads(raw)
    except ValueError as e:
        log(f"WARNING: override row is not valid JSON ({e}); using derived defaults only")
        return empty
    out = {
        "pg": doc.get("pg") or {},
        "pageserver": doc.get("pageserver") or {},
        "safekeeper": doc.get("safekeeper") or {},
    }
    for surface in ("pg", "pageserver", "safekeeper"):
        if out[surface]:
            log(f"overrides[{surface}]: {sorted(out[surface])}")
    return out


def toml_scalar(v):
    """Render a JSON scalar as a toml value.

    Numbers and booleans go in bare (a quoted `scale_factor` would fail to
    deserialize as f64); everything else is quoted as a string. Nested
    objects/arrays are refused rather than half-rendered — the override columns
    are documented as flat `{key: value}` maps of top-level toml keys.
    """
    # bool first: it is a subclass of int, and `true` is not `1` in toml.
    # Separate checks rather than isinstance(v, (int, float)) or the 3.10+
    # `int | float` union: the tuple form trips ruff's UP038, and the union form
    # is a TypeError on any interpreter older than 3.10 — which breaks running
    # this module outside the image (bookworm ships 3.11; dev boxes vary).
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, int):
        return repr(v)
    if isinstance(v, float):
        return repr(v)
    if isinstance(v, str):
        return _toml_quote(v)
    return None


# Keys an override may never touch: they are the pod's identity and wiring, not
# tuning. Overriding `branch_id` would point the endpoint at another branch's
# data; overriding a listen address or the YB connection would unbind it from the
# rest of the pod. Tuning knobs are open by design — this is the short deny-list.
PROTECTED_TOML_KEYS = {
    "branch_id", "pg_distrib_dir", "listen_pg_addr", "listen_http_addr",
    "listen_grpc_addr", "listen_pg", "listen_http", "safekeeper_pg_addr",
    "control_plane_api", "yb_storage_conn",
}


def merge_toml(component, base):
    """Merge derived config with operator overrides into ordered toml lines.

    `base` is an ordered list of `(key, rendered_value)` pairs the supervisor
    derived. Overrides replace a key's value in place rather than being appended:
    **toml forbids a duplicated key** (`toml_edit`, which both daemons parse
    with, rejects the document), so emitting `scale_factor` twice would stop the
    daemon from starting rather than letting the last one win. That is the
    difference from postgresql.conf, where appending really does override.
    """
    merged = list(base)
    index = {k: i for i, (k, _) in enumerate(merged)}
    for key, value in sorted(OVERRIDES.get(component, {}).items()):
        if key in PROTECTED_TOML_KEYS:
            log(f"WARNING: refusing {component} override {key!r}: "
                "identity/wiring keys are not overridable")
            continue
        rendered = toml_scalar(value)
        if rendered is None:
            log(f"WARNING: ignoring {component} override {key!r}: "
                "only scalar values are supported")
            continue
        if key in index:
            log(f"override {component}.{key} = {rendered} (replaces the derived value)")
            merged[index[key]] = (key, rendered)
        else:
            log(f"override {component}.{key} = {rendered}")
            merged.append((key, rendered))
    return [f"{k} = {v}" for k, v in merged]


def scale_factor_toml_lines(component):
    """`scale_factor = <vCPU>` for pageserver.toml / safekeeper.toml.

    Both daemons multiply their count- and byte-like defaults (YSQL pool sizes,
    write fan-out, walredo slots, materialiser concurrency, the safekeeper's WAL
    cache) by this. Tying it to the pod's vCPU makes an endpoint's storage-side
    concurrency track the CPU it actually has, instead of every endpoint sharing
    one baseline.

    Emitted only when the CPU is known: `scale_factor` has no "unset" value, and
    writing a wrong one silently mis-sizes every pool. Fractional quotas are
    passed through as-is — `scale_count` clamps each result to >= 1, so a 500m
    endpoint gets the baseline sizing rather than zero-sized pools.

    Note this is read once at startup: the pools are constructed from it, so a
    change needs the daemon restarted, not just re-rendered.
    """
    if not ENDPOINT_VCPU:
        log(f"WARNING: endpoint CPU unknown; leaving {component} scale_factor at its default")
        return []
    log(f"derived {component} scale_factor={ENDPOINT_VCPU:g} (endpoint vCPU)")
    return [("scale_factor", f"{ENDPOINT_VCPU:g}")]


def write_pageserver_config(datadir):
    datadir.mkdir(parents=True, exist_ok=True)

    # How to create the branch's timeline is not configured here: the controller
    # seeds the branch's `amp_storage.branch_state` row at branch-create time,
    # and the pageserver reads its init mode off that row (a NULL
    # `ancestor_branch_id` means initdb + import, a set one means fork that
    # ancestor at `ancestor_lsn`).
    base = [
        ("pg_distrib_dir", f"'{PG_DISTRIB_DIR}'"),
        ("listen_pg_addr", f"'{BIND_HOST}:{PAGESERVER_PG_PORT}'"),
        ("listen_http_addr", f"'{BIND_HOST}:{PAGESERVER_HTTP_PORT}'"),
        ("listen_grpc_addr", f"'{BIND_HOST}:{PAGESERVER_GRPC_PORT}'"),
        # The local safekeeper this pageserver streams WAL from is always reached
        # over loopback inside the pod, regardless of the public bind address.
        ("safekeeper_pg_addr", f"'{LISTEN_HOST}:{SAFEKEEPER_PG_PORT}'"),
        ("control_plane_api", f"'{CONTROLLER_URL or CONTROL_PLANE_API}'"),
        ("yb_storage_conn", yb_conn_inline_toml()),
        ("branch_id", f"'{BRANCH_ID}'"),
    ]
    base += scale_factor_toml_lines("pageserver")
    lines = merge_toml("pageserver", base)
    (datadir / "pageserver.toml").write_text("\n".join(lines) + "\n")


def write_safekeeper_config(datadir):
    datadir.mkdir(parents=True, exist_ok=True)
    base = [
        ("branch_id", _toml_quote(BRANCH_ID)),
        ("listen_pg", _toml_quote(f"{BIND_HOST}:{SAFEKEEPER_PG_PORT}")),
        ("listen_http", _toml_quote(f"{BIND_HOST}:{SAFEKEEPER_HTTP_PORT}")),
        ("yb_storage_conn", _toml_quote(yb_conn_inline_toml())),
    ]
    base += scale_factor_toml_lines("safekeeper")
    lines = merge_toml("safekeeper", base)
    (datadir / "safekeeper.toml").write_text("\n".join(lines) + "\n")


def write_endpoint_storage_config(datadir):
    datadir.mkdir(parents=True, exist_ok=True)
    cfg = {
        "listen": f"{LISTEN_HOST}:{ENDPOINT_STORAGE_HTTP_PORT}",
        "yb_storage_conn": yb_conn_typed_json(),
    }
    path = datadir / "endpoint_storage.json"
    path.write_text(json.dumps(cfg))
    return path


def md5_role_password(password, user):
    return "md5" + hashlib.md5((password + user).encode()).hexdigest()


# Postmaster-context ceilings. Both are PGC_POSTMASTER, so they can only change
# with a Postgres restart — they are deliberately set once, generously, and left
# alone, so the *live* values beneath them (`max_parallel_workers` USERSET,
# `autovacuum_max_workers` SIGHUP) can be retuned with a reload instead. This is
# the same split PG18 itself uses for autovacuum_worker_slots vs
# autovacuum_max_workers.
MAX_WORKER_PROCESSES_CEILING = env_int("PG_MAX_WORKER_PROCESSES", 64)
AUTOVACUUM_WORKER_SLOTS_CEILING = env_int("PG_AUTOVACUUM_WORKER_SLOTS", 16)

# Fraction of the endpoint's memory given to shared_buffers.
SHARED_BUFFERS_FRACTION = 0.50
# Fraction of the data volume the LFC may actually occupy. The hard ceiling
# (`yb_amp.max_file_cache_size`) is the whole volume; this is the live limit, so
# WAL, logs and the rest of pgdata — which share this volume — keep headroom.
LFC_VOLUME_FRACTION = 0.90
# Upper bound on the LFC ceiling. The ceiling is postmaster-context and reserves
# shared memory for its chunk hash table — one entry per 1 MiB chunk, ~120 B
# each (file_cache.c: RequestAddinShmemSpace / FILE_CACHE_ENRTY_SIZE). At the
# volume sizes endpoints actually get (tens of GiB) that is single-digit MiB,
# but a very large volume would reserve hundreds of MiB of shmem for a cache
# that big is never useful, so clamp it. 128 GiB => ~16 MiB of shmem.
LFC_CEILING_MAX_MB = env_int("PG_LFC_CEILING_MAX_MB", 128 * 1024)


def derived_pg_settings():
    """Resource-derived Postgres GUCs, as a list of spec setting dicts.

    Every value here is a *default*: `render_compute_spec` appends
    COMPUTE_EXTRA_SETTINGS_JSON afterwards, and compute_ctl keeps the last
    occurrence of a name, so an explicit override always wins.
    """
    settings = []

    if ENDPOINT_MEMORY_BYTES:
        # shared_buffers is PGC_POSTMASTER: this is fixed for the life of the
        # postmaster. An in-place memory *increase* leaves it merely undersized
        # (safe); a *decrease* can leave it above what the cgroup now allows,
        # which is the one case that genuinely requires a Postgres restart.
        shared_buffers_mb = max(
            16, int(ENDPOINT_MEMORY_BYTES * SHARED_BUFFERS_FRACTION) // (1024 * 1024)
        )
        settings.append(
            {"name": "shared_buffers", "value": f"{shared_buffers_mb}MB", "vartype": "string"}
        )
        log(
            f"derived shared_buffers={shared_buffers_mb}MB "
            f"({int(SHARED_BUFFERS_FRACTION * 100)}% of {ENDPOINT_MEMORY_BYTES} bytes)"
        )
    else:
        log("WARNING: endpoint memory unknown (no cgroup limit, no ENDPOINT_MEMORY); "
            "leaving shared_buffers at the Postgres default")

    # --- Local File Cache -------------------------------------------------
    # The cache file lives in pgdata on the data volume, so the volume — not
    # memory — is what bounds it. max_file_cache_size is PGC_POSTMASTER but only
    # costs shared memory for its chunk hash table (~one entry per MiB, see
    # RequestAddinShmemSpace in pgxn/yb_amp/file_cache.c), so pinning it to the
    # whole volume is cheap and means every later resize is a SIGHUP away.
    # Take the smaller of what the filesystem reports and the budget the chart
    # declared. On an emptyDir the two differ enormously — statvfs sees the whole
    # node disk while the kubelet will evict the pod once /data passes
    # sizeLimit — so the declared budget is what actually bounds the cache.
    vol = volume_bytes(RUN_DIR)
    if vol and ENDPOINT_DATA_VOLUME_BYTES and ENDPOINT_DATA_VOLUME_BYTES < vol:
        log(f"data volume: filesystem reports {vol // (1024 * 1024)}MB but the declared "
            f"budget is {ENDPOINT_DATA_VOLUME_BYTES // (1024 * 1024)}MB (emptyDir sizeLimit "
            f"or PVC size); sizing the cache off the budget")
        vol = ENDPOINT_DATA_VOLUME_BYTES
    elif vol and not ENDPOINT_DATA_VOLUME_BYTES:
        log("WARNING: ENDPOINT_DATA_VOLUME_SIZE is unset; sizing the LFC off statvfs, which "
            "on an emptyDir reports the whole node filesystem and can exceed its sizeLimit")
    if vol:
        vol_mb = vol // (1024 * 1024)
        ceiling_mb = min(vol_mb, LFC_CEILING_MAX_MB)
        lfc_limit_mb = max(1, min(int(vol_mb * LFC_VOLUME_FRACTION), ceiling_mb))
        settings.append(
            {"name": "yb_amp.max_file_cache_size", "value": f"{ceiling_mb}MB",
             "vartype": "string"}
        )
        settings.append(
            {"name": "yb_amp.file_cache_size_limit", "value": f"{lfc_limit_mb}MB",
             "vartype": "string"}
        )
        log(f"derived LFC: ceiling={ceiling_mb}MB live_limit={lfc_limit_mb}MB "
            f"({int(LFC_VOLUME_FRACTION * 100)}% of the {vol_mb}MB {RUN_DIR} volume"
            + (f", ceiling clamped to {LFC_CEILING_MAX_MB}MB)" if ceiling_mb < vol_mb else ")"))
    else:
        log(f"WARNING: could not stat the {RUN_DIR} volume; LFC stays disabled")

    # --- Background / parallel workers ------------------------------------
    # Ceilings are postmaster-context; the values under them are USERSET or
    # SIGHUP, so they can be retuned without bouncing Postgres.
    settings.append(
        {"name": "max_worker_processes", "value": str(MAX_WORKER_PROCESSES_CEILING),
         "vartype": "integer"}
    )
    settings.append(
        {"name": "autovacuum_worker_slots", "value": str(AUTOVACUUM_WORKER_SLOTS_CEILING),
         "vartype": "integer"}
    )
    if ENDPOINT_VCPU:
        vcpu = max(1, int(ENDPOINT_VCPU))
        max_parallel_workers = min(vcpu, MAX_WORKER_PROCESSES_CEILING)
        per_gather = max(2, vcpu // 2)
        maintenance = max(2, vcpu // 4)
        autovacuum = min(8, max(3, vcpu // 2), AUTOVACUUM_WORKER_SLOTS_CEILING)
        for name, value in (
            ("max_parallel_workers", max_parallel_workers),
            ("max_parallel_workers_per_gather", per_gather),
            ("max_parallel_maintenance_workers", maintenance),
            ("autovacuum_max_workers", autovacuum),
        ):
            settings.append({"name": name, "value": str(value), "vartype": "integer"})
        log(f"derived workers from {vcpu} vCPU: max_parallel_workers={max_parallel_workers} "
            f"per_gather={per_gather} maintenance={maintenance} autovacuum={autovacuum}")
    else:
        log("WARNING: endpoint CPU unknown (no cgroup quota, no ENDPOINT_CPU); "
            "leaving the parallel-worker GUCs at their Postgres defaults")

    return settings


def render_compute_spec(specfile):
    settings = [
        {"name": "fsync", "value": "off", "vartype": "bool"},
        {"name": "wal_level", "value": "logical", "vartype": "enum"},
        {"name": "wal_log_hints", "value": "on", "vartype": "bool"},
        {"name": "port", "value": str(COMPUTE_PORT), "vartype": "integer"},
        {"name": "listen_addresses", "value": "0.0.0.0", "vartype": "string"},
        {"name": "max_connections", "value": "100", "vartype": "integer"},
        {"name": "shared_buffers", "value": "128MB", "vartype": "string"},
        {"name": "max_wal_senders", "value": "10", "vartype": "integer"},
        {"name": "max_replication_slots", "value": "10", "vartype": "integer"},
        {"name": "password_encryption", "value": "md5", "vartype": "enum"},
        {"name": "restart_after_crash", "value": "off", "vartype": "bool"},
        {"name": "synchronous_standby_names", "value": "walproposer", "vartype": "string"},
        {"name": "shared_preload_libraries", "value": SHARED_PRELOAD_LIBRARIES, "vartype": "string"},
        {"name": "yb_amp.safekeepers", "value": f"{LISTEN_HOST}:{SAFEKEEPER_PG_PORT}", "vartype": "string"},
        {"name": "yb_amp.branch_id", "value": BRANCH_ID, "vartype": "string"},
        {"name": "yb_amp.pageserver_connstring",
         "value": f"host={LISTEN_HOST} port={PAGESERVER_PG_PORT}", "vartype": "string"},
    ]
    # Resource-derived defaults come after the static block and before the
    # operator settings: `as_pg_settings` writes these in order into
    # postgresql.conf, where the last occurrence of a name wins. Full precedence:
    #   static default < derived < chart extension paths < branch < endpoint
    # the last two both arriving from the registry (see fetch_overrides).
    settings.extend(derived_pg_settings())

    for k, v in json.loads(env("COMPUTE_EXTRA_SETTINGS_JSON", "{}")).items():
        settings.append({"name": k, "value": str(v), "vartype": "string"})

    # User GUCs, read live from the registry, land last of all: this is the
    # branch's baseline with the endpoint's own layered over it (fetch_overrides
    # merges the two rows with `||`, the endpoint winning, as the schema
    # documents). COMPUTE_EXTRA_SETTINGS_JSON above no longer carries any user
    # settings — the chart reduced it to the extension paths it derives from the
    # ImageVolumes it mounted — so nothing here is competing with a second copy.
    for k, v in sorted(OVERRIDES.get("pg", {}).items()):
        settings.append({"name": k, "value": str(v), "vartype": "string"})
        log(f"override pg.{k} = {v}")

    spec = {
        "format_version": 1.0,
        "timestamp": "2024-01-01T00:00:00.000Z",
        "operation_uuid": "00000000-0000-0000-0000-000000000000",
        "suspend_timeout_seconds": -1,
        "cluster": {
            "cluster_id": "yb-amp-endpoint",
            "name": "yb-amp-endpoint",
            "state": "running",
            "roles": [{
                "name": "cloud_admin",
                "encrypted_password": md5_role_password(COMPUTE_PASSWORD, "cloud_admin"),
                "options": None,
            }],
            "databases": [],
            "settings": settings,
        },
        "delta_operations": [],
        "skip_pg_catalog_updates": True,
        "mode": "Primary",
        "storage_branch_id": BRANCH_ID,
        "pageserver_connstring": f"host={LISTEN_HOST} port={PAGESERVER_PG_PORT}",
        "safekeeper_connstrings": [f"{LISTEN_HOST}:{SAFEKEEPER_PG_PORT}"],
    }
    config = {
        "spec": spec,
        "compute_ctl_config": {"jwks": {"keys": []}},
    }
    specfile.write_text(json.dumps(config))
    return specfile


# ---------------------------------------------------------------------------
# Service supervision
# ---------------------------------------------------------------------------

class Service:
    def __init__(self, name, argv, cwd, health=None, extra_env=None):
        self.name = name
        self.argv = [str(a) for a in argv]
        self.cwd = str(cwd)
        self.health = health  # callable -> bool, or None
        self.extra_env = extra_env or {}
        self.proc = None
        self.restarts = []  # monotonic timestamps

    def start(self):
        penv = dict(os.environ)
        penv.update(self.extra_env)
        log(f"starting {self.name}: {' '.join(self.argv)}")
        self.proc = subprocess.Popen(self.argv, cwd=self.cwd, env=penv)

    def is_alive(self):
        return self.proc is not None and self.proc.poll() is None

    def wait_healthy(self, timeout):
        if self.health is None:
            return
        wait_for(f"{self.name} healthy", self.health, timeout)
        log(f"{self.name} is healthy")

    def record_restart(self):
        now = time.monotonic()
        self.restarts = [t for t in self.restarts if now - t < RESTART_WINDOW]
        self.restarts.append(now)
        return len(self.restarts)


class Supervisor:
    def __init__(self):
        self.services = []
        self.shutting_down = threading.Event()

    def add(self, svc):
        self.services.append(svc)

    def shutdown(self, signum=None, frame=None):
        if self.shutting_down.is_set():
            return
        log(f"shutdown (signal {signum}); stopping services")
        self.shutting_down.set()
        for svc in reversed(self.services):
            if svc.is_alive():
                log(f"  SIGTERM {svc.name}")
                try:
                    svc.proc.terminate()
                except Exception:
                    pass
        deadline = time.monotonic() + 30
        for svc in reversed(self.services):
            if svc.proc is None:
                continue
            remaining = max(0, deadline - time.monotonic())
            try:
                svc.proc.wait(timeout=remaining)
            except Exception:
                log(f"  SIGKILL {svc.name}")
                try:
                    svc.proc.kill()
                except Exception:
                    pass

    def monitor(self):
        """Block, restarting any crashed service. Exit non-zero if a service
        exceeds its restart budget so the pod is restarted by k8s/tini."""
        while not self.shutting_down.is_set():
            for svc in self.services:
                if self.shutting_down.is_set():
                    break
                if svc.is_alive():
                    continue
                rc = svc.proc.returncode if svc.proc else None
                log(f"{svc.name} exited (rc={rc})")
                count = svc.record_restart()
                if count > MAX_RESTARTS:
                    log(f"{svc.name} exceeded {MAX_RESTARTS} restarts in "
                        f"{RESTART_WINDOW}s; failing the pod")
                    self.shutdown()
                    return 1
                log(f"restarting {svc.name} in {RESTART_BACKOFF}s "
                    f"({count}/{MAX_RESTARTS} within {RESTART_WINDOW}s)")
                time.sleep(RESTART_BACKOFF)
                if not self.shutting_down.is_set():
                    svc.start()
            time.sleep(1)
        return 0


# ---------------------------------------------------------------------------
# Bring-up
# ---------------------------------------------------------------------------


def main():
    global BRANCH_ID

    sup = Supervisor()
    signal.signal(signal.SIGTERM, sup.shutdown)
    signal.signal(signal.SIGINT, sup.shutdown)

    if BRANCH_ID is None:
        BRANCH_ID = os.urandom(16).hex()
        log(f"no BRANCH_ID supplied; using fresh branch {BRANCH_ID}")
    else:
        log(f"serving branch {BRANCH_ID}")

    es_dir = RUN_DIR / "endpoint_storage"
    sk_dir = RUN_DIR / "safekeeper"
    ps_dir = RUN_DIR / "pageserver"
    compute_pgdata = RUN_DIR / "pgdata"

    # Operator overrides, before anything is rendered: they feed all three
    # config surfaces (compute spec, pageserver.toml, safekeeper.toml).
    OVERRIDES.update(fetch_overrides())

    # 1. endpoint_storage
    es_cfg = write_endpoint_storage_config(es_dir)
    es = Service(
        "endpoint_storage",
        [BIN_DIR / "yb-endpoint-storage", es_cfg],
        cwd=es_dir,
        extra_env={"RUST_LOG": "info"},
        health=lambda: tcp_open(LISTEN_HOST, ENDPOINT_STORAGE_HTTP_PORT),
    )

    # 2. safekeeper
    write_safekeeper_config(sk_dir)
    sk = Service(
        "safekeeper",
        [BIN_DIR / "yb-safekeeper", "-D", sk_dir],
        cwd=sk_dir,
        health=lambda: http_request("GET", f"http://{LISTEN_HOST}:{SAFEKEEPER_HTTP_PORT}/v1/status")[0] == 200,
    )

    # 3. pageserver (runs initdb for a Bootstrap recipe, or forks the ancestor
    #    for a Branch recipe, on first start)
    write_pageserver_config(ps_dir)
    ps = Service(
        "yb-pageserver",
        [BIN_DIR / "yb-pageserver", "-D", ps_dir],
        cwd=ps_dir,
        health=lambda: http_request("GET", f"http://{LISTEN_HOST}:{PAGESERVER_HTTP_PORT}/v1/status")[0] == 200,
    )

    for svc in (es, sk, ps):
        sup.add(svc)
        svc.start()
        svc.wait_healthy(timeout=120)

    # Wait for the pageserver's branch to come up Active and read its start LSN.
    ps_tl = f"http://{LISTEN_HOST}:{PAGESERVER_HTTP_PORT}/v1/branch/{BRANCH_ID}"

    def timeline_active():
        st, body = http_request("GET", ps_tl)
        return st == 200 and body and body.get("state") == "Active"

    wait_for("pageserver branch Active", timeline_active, timeout=180)
    _, tl = http_request("GET", ps_tl)
    start_lsn = tl.get("last_record_lsn", "0/0")
    log(f"pageserver branch Active at LSN {start_lsn}")

    # Create the branch on the safekeeper.
    sk_create = {
        "branch_id": BRANCH_ID,
        # ENDPOINT_PG_VERSION is set by the controller from the branch's
        # recorded version, so trust it rather than re-reading the branch row.
        # Only the major survives the trip: the safekeeper keeps this in memory
        # for the greeting check, which compares major versions, and the durable
        # `pg_version` is the controller-seeded one it reads back on restart.
        "pg_version": int(PG_VERSION) * 10000,
        "start_lsn": start_lsn,
    }
    try:
        http_request("POST", f"http://{LISTEN_HOST}:{SAFEKEEPER_HTTP_PORT}/v1/branch", body=sk_create)
        log("created branch on safekeeper")
    except urllib.error.HTTPError as e:
        # 409/already-exists is fine on restart.
        if e.code not in (409,):
            log(f"safekeeper create_branch returned {e.code}: {e.read()[:200]}")

    # 4. compute_ctl + postgres
    spec = render_compute_spec(RUN_DIR / "compute_spec.json")
    compute = Service(
        "compute_ctl",
        [
            BIN_DIR / "yb-compute-ctl",
            "--pgdata", compute_pgdata,
            "-C", f"postgresql://cloud_admin@{LISTEN_HOST}:{COMPUTE_PORT}/postgres",
            "-b", PG_BIN / "postgres",
            "--compute-id", "yb-amp-endpoint",
            "--config", spec,
            "--external-http-port", str(COMPUTE_CTL_HTTP_PORT),
            "--internal-http-port", str(COMPUTE_CTL_HTTP_PORT + 1),
            "--dev",
        ],
        cwd=RUN_DIR,
        health=lambda: tcp_open(LISTEN_HOST, COMPUTE_PORT),
    )
    sup.add(compute)
    compute.start()

    # The compute spec sets skip_pg_catalog_updates, so the cloud_admin password
    # from the spec is never applied: loopback works via the `trust` pg_hba rules
    # but any non-loopback client (published port, k8s service, port-forward) hits
    # the `host all all all md5` rule with no stored password and fails. Set the
    # password over the local trust connection so COMPUTE_PASSWORD works remotely.
    def _set_compute_password():
        wait_for("compute accepting connections",
                 lambda: tcp_open(LISTEN_HOST, COMPUTE_PORT), timeout=180)
        esc = COMPUTE_PASSWORD.replace("'", "''")
        cmd = [
            str(PG_BIN / "psql"), "-h", LISTEN_HOST, "-p", str(COMPUTE_PORT),
            "-U", "cloud_admin", "-d", "postgres",
            "-v", "ON_ERROR_STOP=1", "--no-psqlrc",
            "-c", f"ALTER ROLE cloud_admin WITH PASSWORD '{esc}'",
        ]
        res = None
        for _ in range(60):
            res = subprocess.run(cmd, capture_output=True, text=True)
            if res.returncode == 0:
                # LOAD-BEARING: controller's Docker deployer (controller/internal/deploy/docker.go)
                # waits for this exact line as the endpoint's readiness signal.
                # Keep the two in sync.
                log("set cloud_admin password for remote auth")
                return
            time.sleep(1)
        log(f"WARNING: could not set cloud_admin password: "
            f"{(res.stderr or '').strip()[:200]}")
    # Run in the background so the monitor loop starts supervising immediately:
    # _set_compute_password waits for the compute (bounded by timeouts), and we
    # don't want that wait to delay restarting a sibling that crashes meanwhile.
    threading.Thread(target=_set_compute_password, daemon=True).start()

    log("all services started; entering monitor loop")
    rc = sup.monitor()
    sys.exit(rc)


if __name__ == "__main__":
    main()
