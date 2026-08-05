"""Tests for the endpoint supervisor's pure logic.

`supervisor.py` had no tests, and every defect found in it was caught by a
linter, a reviewer or a hand-run script rather than by anything in the repo:

  * the LFC sized from `statvfs`, which on an emptyDir reports the whole node
    filesystem, letting the cache grow past the volume's sizeLimit into a pod
    eviction;
  * operator overrides appended as duplicate toml keys, which `toml_edit`
    rejects outright, so the pageserver would not have started at all;
  * an `isinstance` form ruff rejects, whose obvious fix breaks on any
    interpreter older than 3.10.

Each of those has a case here. These also serve as the parity oracle for the
Rust port in `compute/endpoint_supervisor`: the same inputs and expectations are
asserted on both sides, so the rewrite can be shown to agree with what ships
before it takes over PID 1.

The module is imported by path with its resource-discovery env pointed at
fixtures, so nothing here touches a real cgroup, database or volume.
"""

import importlib.util
import pathlib
import sys

import pytest

SUPERVISOR = pathlib.Path(__file__).resolve().parents[1] / "supervisor.py"


def load_supervisor(monkeypatch, tmp_path, **env):
    """Import supervisor.py fresh with `env` applied.

    Module-level constants (ENDPOINT_VCPU, OVERRIDES, ...) are resolved at
    import, so each case gets its own module object rather than mutating a
    shared one.
    """
    # str() everywhere: setenv rejects a PosixPath, and this repo's pytest config
    # turns that warning into a failure.
    monkeypatch.setenv("BRANCH_ID", str(env.pop("BRANCH_ID", "br-test")))
    # Default to a cgroup root that does not exist, so a case opts in to cgroup
    # values explicitly instead of inheriting the host's.
    monkeypatch.setenv("CGROUP_ROOT", str(env.pop("CGROUP_ROOT", tmp_path / "no-cgroup")))
    monkeypatch.setenv("RUN_DIR", str(env.pop("RUN_DIR", tmp_path)))
    for k, v in env.items():
        if v is None:
            monkeypatch.delenv(k, raising=False)
        else:
            monkeypatch.setenv(k, str(v))
    spec = importlib.util.spec_from_file_location(f"sup_{len(sys.modules)}", SUPERVISOR)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def cgroup_v2(tmp_path, *, cpu_max=None, memory_max=None):
    d = tmp_path / "cg2"
    d.mkdir(parents=True, exist_ok=True)
    if cpu_max is not None:
        (d / "cpu.max").write_text(cpu_max + "\n")
    if memory_max is not None:
        (d / "memory.max").write_text(memory_max + "\n")
    return d


def settings_map(mod, settings=None):
    return {s["name"]: s["value"] for s in (settings or mod.derived_pg_settings())}


# --------------------------------------------------------------------------
# Quantity parsing
# --------------------------------------------------------------------------

@pytest.mark.parametrize(
    "text,want",
    [
        ("4Gi", 4 * 1024**3),
        ("512Mi", 512 * 1024**2),
        ("98831908Ki", 98831908 * 1024),
        ("2G", 2_000_000_000),
        ("1073741824", 1073741824),
        (" 8Gi ", 8 * 1024**3),
        # Unparseable input yields None so the caller skips the setting rather
        # than sizing a cache off a typo.
        ("", None),
        ("nonsense", None),
        (None, None),
    ],
)
def test_parse_memory_quantity(monkeypatch, tmp_path, text, want):
    mod = load_supervisor(monkeypatch, tmp_path)
    assert mod.parse_memory_quantity(text) == want


@pytest.mark.parametrize(
    "text,want",
    [("2", 2.0), ("1500m", 1.5), ("500m", 0.5), ("0.5", 0.5), ("", None), ("x", None), (None, None)],
)
def test_parse_cpu_quantity(monkeypatch, tmp_path, text, want):
    mod = load_supervisor(monkeypatch, tmp_path)
    assert mod.parse_cpu_quantity(text) == want


# --------------------------------------------------------------------------
# Resource discovery
# --------------------------------------------------------------------------

def test_cgroup_v2_is_preferred_over_env(monkeypatch, tmp_path):
    """The reason the cgroup is read at all: after an in-place resize the env
    still describes the size the pod started with."""
    cg = cgroup_v2(tmp_path, cpu_max="600000 100000", memory_max=str(12 * 1024**3))
    mod = load_supervisor(
        monkeypatch, tmp_path, CGROUP_ROOT=cg, ENDPOINT_CPU="2", ENDPOINT_MEMORY="4Gi"
    )
    assert mod.ENDPOINT_VCPU == 6.0
    assert mod.ENDPOINT_MEMORY_BYTES == 12 * 1024**3


def test_env_is_the_fallback_without_a_cgroup_limit(monkeypatch, tmp_path):
    mod = load_supervisor(monkeypatch, tmp_path, ENDPOINT_CPU="4", ENDPOINT_MEMORY="8Gi")
    assert mod.ENDPOINT_VCPU == 4.0
    assert mod.ENDPOINT_MEMORY_BYTES == 8 * 1024**3


def test_cgroup_v2_unlimited_reads_as_unset(monkeypatch, tmp_path):
    cg = cgroup_v2(tmp_path, cpu_max="max 100000", memory_max="max")
    mod = load_supervisor(monkeypatch, tmp_path, CGROUP_ROOT=cg)
    assert mod.cgroup_cpu_limit() is None
    assert mod.cgroup_memory_limit_bytes() is None


def test_cgroup_v1_fallback_and_sentinel(monkeypatch, tmp_path):
    d = tmp_path / "cg1"
    (d / "memory").mkdir(parents=True)
    (d / "cpu").mkdir(parents=True)
    (d / "memory" / "memory.limit_in_bytes").write_text("4294967296\n")
    (d / "cpu" / "cpu.cfs_quota_us").write_text("200000\n")
    (d / "cpu" / "cpu.cfs_period_us").write_text("100000\n")
    mod = load_supervisor(monkeypatch, tmp_path, CGROUP_ROOT=d)
    assert mod.cgroup_memory_limit_bytes() == 4 * 1024**3
    assert mod.cgroup_cpu_limit() == 2.0

    (d / "memory" / "memory.limit_in_bytes").write_text("9223372036854771712\n")
    mod = load_supervisor(monkeypatch, tmp_path, CGROUP_ROOT=d)
    assert mod.cgroup_memory_limit_bytes() is None, "the no-limit sentinel is not a real limit"


# --------------------------------------------------------------------------
# Derived Postgres settings
# --------------------------------------------------------------------------

def test_derived_settings_match_the_deployed_four_core_shape(monkeypatch, tmp_path):
    """Pinned against a live 4-vCPU endpoint (8Gi memory, 12Gi volume)."""
    cg = cgroup_v2(tmp_path, cpu_max="400000 100000", memory_max=str(8 * 1024**3))
    mod = load_supervisor(
        monkeypatch, tmp_path, CGROUP_ROOT=cg, ENDPOINT_DATA_VOLUME_SIZE="12Gi"
    )
    got = settings_map(mod)
    assert got["shared_buffers"] == "4096MB"
    assert got["yb_amp.max_file_cache_size"] == "12288MB"
    assert got["yb_amp.file_cache_size_limit"] == "11059MB"
    assert got["max_parallel_workers"] == "4"
    assert got["max_parallel_workers_per_gather"] == "2"
    assert got["max_parallel_maintenance_workers"] == "2"
    assert got["autovacuum_max_workers"] == "3"
    # Postmaster ceilings: fixed, so the live values above stay reload-only.
    assert got["max_worker_processes"] == "64"
    assert got["autovacuum_worker_slots"] == "16"


def test_derived_settings_match_the_deployed_eight_core_shape(monkeypatch, tmp_path):
    """Pinned against a live 8-vCPU endpoint (16Gi memory, 23Gi volume)."""
    cg = cgroup_v2(tmp_path, cpu_max="800000 100000", memory_max=str(16 * 1024**3))
    mod = load_supervisor(
        monkeypatch, tmp_path, CGROUP_ROOT=cg, ENDPOINT_DATA_VOLUME_SIZE="23Gi"
    )
    got = settings_map(mod)
    assert got["shared_buffers"] == "8192MB"
    assert got["yb_amp.max_file_cache_size"] == "23552MB"
    assert got["yb_amp.file_cache_size_limit"] == "21196MB"
    assert got["max_parallel_workers"] == "8"
    assert got["max_parallel_workers_per_gather"] == "4"
    assert got["autovacuum_max_workers"] == "4"


def test_lfc_is_clamped_to_the_declared_budget_not_the_filesystem(monkeypatch, tmp_path):
    """Regression: an emptyDir's sizeLimit is enforced by the kubelet watching
    usage, not by a filesystem quota, so statvfs reports the whole node
    filesystem. Sizing the cache off that let it grow past sizeLimit and get the
    pod evicted for exceeding ephemeral-storage."""
    mod = load_supervisor(monkeypatch, tmp_path, ENDPOINT_DATA_VOLUME_SIZE="10Gi")
    fs_bytes = mod.volume_bytes(str(tmp_path))
    assert fs_bytes and fs_bytes > 10 * 1024**3, "fixture needs a filesystem larger than the budget"
    got = settings_map(mod)
    assert got["yb_amp.max_file_cache_size"] == "10240MB"
    assert got["yb_amp.file_cache_size_limit"] == "9216MB", "90% of the budget, not of the fs"


def test_unknown_resources_skip_their_settings(monkeypatch, tmp_path):
    """An unknown input must leave the GUC alone rather than ship a guess."""
    mod = load_supervisor(monkeypatch, tmp_path, ENDPOINT_DATA_VOLUME_SIZE=None)
    monkeypatch.setattr(mod, "ENDPOINT_MEMORY_BYTES", None)
    monkeypatch.setattr(mod, "ENDPOINT_VCPU", None)
    monkeypatch.setattr(mod, "volume_bytes", lambda _p: None)
    got = settings_map(mod)
    assert "shared_buffers" not in got
    assert "yb_amp.file_cache_size_limit" not in got
    assert "max_parallel_workers" not in got
    # The ceilings do not depend on the resources, so they are still emitted.
    assert got["max_worker_processes"] == "64"


# --------------------------------------------------------------------------
# toml rendering and the override merge
# --------------------------------------------------------------------------

@pytest.mark.parametrize(
    "value,want",
    [
        (8, "8"),
        (1.5, "1.5"),
        # bool before int: bool subclasses int, and `true` is not `1` in toml.
        (True, "true"),
        (False, "false"),
        ("none", '"none"'),
        # Nested values are refused rather than half-rendered.
        ({"a": 1}, None),
        ([1, 2], None),
    ],
)
def test_toml_scalar(monkeypatch, tmp_path, value, want):
    mod = load_supervisor(monkeypatch, tmp_path)
    assert mod.toml_scalar(value) == want


def test_override_replaces_the_derived_value_without_duplicating_the_key(monkeypatch, tmp_path):
    """Regression: toml forbids a duplicated key — `toml_edit`, which both
    daemons parse with, rejects the whole document — so an appended override
    would stop the daemon starting rather than winning. postgresql.conf is the
    opposite, which is what made this easy to get wrong."""
    cg = cgroup_v2(tmp_path, cpu_max="400000 100000")
    mod = load_supervisor(monkeypatch, tmp_path, CGROUP_ROOT=cg)
    mod.OVERRIDES.update({"pageserver": {"scale_factor": 8, "yb_payload_compression": "none"}})
    lines = mod.merge_toml("pageserver", [("scale_factor", "4"), ("branch_id", "'br'")])
    keys = [ln.split("=")[0].strip() for ln in lines]
    assert len(keys) == len(set(keys)), f"duplicate key emitted: {keys}"
    assert "scale_factor = 8" in lines
    assert "scale_factor = 4" not in lines
    assert "yb_payload_compression = \"none\"" in lines


def test_identity_keys_cannot_be_overridden(monkeypatch, tmp_path):
    """An override must not be able to point an endpoint at another branch's data
    or unbind it from the rest of its pod."""
    mod = load_supervisor(monkeypatch, tmp_path)
    mod.OVERRIDES.update(
        {"pageserver": {"branch_id": "someone-elses", "listen_pg_addr": "'0.0.0.0:1'"}}
    )
    lines = mod.merge_toml("pageserver", [("branch_id", "'mine'")])
    assert lines == ["branch_id = 'mine'"], lines


def test_written_toml_has_no_duplicate_keys(monkeypatch, tmp_path):
    """End to end on the rendered files, since that is what the daemons parse."""
    cg = cgroup_v2(tmp_path, cpu_max="400000 100000")
    mod = load_supervisor(monkeypatch, tmp_path, CGROUP_ROOT=cg)
    mod.OVERRIDES.update(
        {"pageserver": {"scale_factor": 8}, "safekeeper": {"wal_cache_size_mib": 512}}
    )
    for writer, name in (
        (mod.write_pageserver_config, "pageserver.toml"),
        (mod.write_safekeeper_config, "safekeeper.toml"),
    ):
        writer(tmp_path / "d")
        body = (tmp_path / "d" / name).read_text().strip().split("\n")
        keys = [ln.split("=")[0].strip() for ln in body]
        assert len(keys) == len(set(keys)), f"{name} has duplicate keys: {keys}"


def test_scale_factor_is_omitted_when_the_cpu_is_unknown(monkeypatch, tmp_path):
    """scale_factor has no "unset" value, so a wrong one silently mis-sizes every
    pool; better to leave it out and let the daemon default apply."""
    mod = load_supervisor(monkeypatch, tmp_path)
    monkeypatch.setattr(mod, "ENDPOINT_VCPU", None)
    assert mod.scale_factor_toml_lines("pageserver") == []


# --------------------------------------------------------------------------
# Override lookup
# --------------------------------------------------------------------------

def test_no_endpoint_id_means_no_lookup(monkeypatch, tmp_path):
    mod = load_supervisor(monkeypatch, tmp_path, ENDPOINT_ID=None)
    assert mod.ENDPOINT_ID is None
    assert mod.fetch_overrides() == {"pg": {}, "pageserver": {}, "safekeeper": {}}


def test_a_malformed_endpoint_id_is_rejected(monkeypatch, tmp_path):
    """The id is interpolated into `psql -c`, which takes no bind parameters, so
    it is validated rather than trusted."""
    mod = load_supervisor(monkeypatch, tmp_path, ENDPOINT_ID="'; DROP TABLE x; --")
    assert mod.ENDPOINT_ID is None


def test_a_failed_lookup_falls_back_to_derived_defaults(monkeypatch, tmp_path):
    """A registry blip must not stop an endpoint starting."""
    mod = load_supervisor(
        monkeypatch, tmp_path, ENDPOINT_ID="3f2b1c4e-0000-4000-8000-000000000001"
    )

    class Boom:
        returncode = 1
        stdout = ""
        stderr = "connection refused"

    monkeypatch.setattr(mod, "psql", lambda *a, **k: Boom())
    assert mod.fetch_overrides() == {"pg": {}, "pageserver": {}, "safekeeper": {}}


def test_lookup_parses_the_three_surfaces(monkeypatch, tmp_path):
    mod = load_supervisor(
        monkeypatch, tmp_path, ENDPOINT_ID="3f2b1c4e-0000-4000-8000-000000000001"
    )

    class Ok:
        returncode = 0
        stderr = ""
        stdout = (
            '{"pg": {"work_mem": "64MB"}, "pageserver": {"scale_factor": 8},'
            ' "safekeeper": {"wal_cache_size_mib": 512}}\n'
        )

    monkeypatch.setattr(mod, "psql", lambda *a, **k: Ok())
    got = mod.fetch_overrides()
    assert got["pg"] == {"work_mem": "64MB"}
    assert got["pageserver"] == {"scale_factor": 8}
    assert got["safekeeper"] == {"wal_cache_size_mib": 512}


def test_lookup_merges_both_rows_with_the_endpoint_winning(monkeypatch, tmp_path):
    """The query asks the database for `b.pg_settings || e.pg_settings`; this
    pins the SQL that expresses that precedence, since a swapped operand order
    would silently make the branch win."""
    mod = load_supervisor(
        monkeypatch, tmp_path, ENDPOINT_ID="3f2b1c4e-0000-4000-8000-000000000001"
    )
    seen = {}

    class Ok:
        returncode = 0
        stderr = ""
        stdout = '{"pg": {}, "pageserver": {}, "safekeeper": {}}'

    def spy(dbname, sql=None, **kwargs):
        seen["sql"] = sql
        return Ok()

    monkeypatch.setattr(mod, "psql", spy)
    mod.fetch_overrides()
    sql = " ".join(seen["sql"].split())
    assert "coalesce(b.pg_settings,'{}'::jsonb) || coalesce(e.pg_settings,'{}'::jsonb)" in sql
    assert "LEFT JOIN amp_registry.branches b" in sql
