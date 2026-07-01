#!/usr/bin/env python3
"""Build yb-light binaries + compute extensions — the one build entry point.

Replaces the manual `git submodule update` + `make` dance from the README.
It handles a fresh checkout, the PostgreSQL-headers prerequisite, and the
debug/release/tarball builds, and keeps the vendored Postgres in sync.

Modes (pick one; default is debug):
    (no flag)        debug build   (cargo dev profile -> target/debug)
    --release        release build (-> target/release)
    --release-tar    release build + package a shippable tarball
                     (/tmp/yb-light-bundle-<date>-<sha>.tar.gz)
    --headers        only install the PostgreSQL server headers, so a bare
                     `cargo build` / `cargo check` / IDE can find them (the
                     postgres_ffi crate generates bindings from them).

PostgreSQL submodule handling (every mode):
    ybAmp builds against a patched PostgreSQL tree vendored as the
    `vendor/postgres-v17` git submodule. This script checks it out if it is
    missing, and when the gitlink recorded in HEAD differs from the commit in
    the working tree (e.g. a rebase/pull bumped it) it re-syncs and rebuilds
    PostgreSQL from source. When the submodule already matches and
    pg_install/v17 exists, the cached install is reused (PG_INSTALL_CACHED=1)
    and only the C extensions are recompiled. This closes the footgun where a
    bumped gitlink would otherwise ship a stale Postgres against the cache.
"""

import argparse
import datetime
import os
import shutil
import socket
import subprocess
import sys
from pathlib import Path

# Repo root = parent of this script's scripts/ dir, so the build works no
# matter where it's invoked from.
ROOT = Path(__file__).resolve().parent.parent
SUBMODULE = "vendor/postgres-v17"
PG_INSTALL = ROOT / "pg_install" / "v17"

# Binaries launched by yb_light_local (see control_plane local_env.rs).
DAEMON_BINS = [
    "yb-pageserver",
    "yb_light_local",
    "yb-safekeeper",
    "storage_controller",
    "yb-compute-ctl",
    "yb-endpoint-storage",
]
# Operator-side diagnostic tools (built by `-p pageserver`); they share the
# YbConnArgs connection flags and work against no-TLS and TLS clusters.
DIAG_TOOLS = ["yb-dump-pages", "yb-dump-wals", "yb-reset-consistent-lsn"]
# Crates that produce the binaries above.
BUILD_CRATES = [
    "pageserver",
    "control_plane",
    "safekeeper",
    "storage_controller",
    "compute_tools",
    "endpoint_storage",
]
# Runtime libs not present on stock AlmaLinux/RHEL 9 (OpenSSL 1.1, ICU 60).
# See memory/bundle-runtime-libs.md. Staged top-level for the daemons and into
# pg_install/v17/lib/ for the walredo postgres child (LD_LIBRARY_PATH is only
# that dir — see #33).
RUNTIME_LIBS = [
    "libssl.so.1.1",
    "libcrypto.so.1.1",
    "libicui18n.so.60",
    "libicuuc.so.60",
    "libicudata.so.60",
]


def say(msg):
    print(f">>> {msg}", flush=True)


def run(cmd, *, env=None):
    """Run a command from the repo root, streaming output; abort on failure."""
    print(f"+ {' '.join(cmd)}", flush=True)
    subprocess.run(cmd, cwd=ROOT, env=env, check=True)


def git_out(*args):
    """Return stripped stdout of a git command, or None if it fails."""
    res = subprocess.run(
        ["git", *args], cwd=ROOT, capture_output=True, text=True
    )
    return res.stdout.strip() if res.returncode == 0 else None


def sync_submodule_if_needed():
    """Check out / re-sync vendor/postgres-v17. Return True if PG needs a rebuild.

    PG must be rebuilt when the working tree doesn't match the gitlink (a bump,
    or a fresh/absent checkout) or when pg_install/v17 was never built.
    """
    want = git_out("rev-parse", f"HEAD:{SUBMODULE}")
    have = git_out("-C", SUBMODULE, "rev-parse", "HEAD")
    pg_built = (PG_INSTALL / "bin" / "postgres").is_file()

    if want != have:
        where = have[:12] if have else "absent"
        say(f"{SUBMODULE} gitlink ({want[:12]}) != working tree ({where})")
        say("Syncing submodule and rebuilding PostgreSQL from source...")
        run(["git", "submodule", "update", "--init", "--recursive", "--", SUBMODULE])
        return True
    if not pg_built:
        say("pg_install/v17 missing — building PostgreSQL from source...")
        run(["git", "submodule", "update", "--init", "--recursive", "--", SUBMODULE])
        return True
    say(f"{SUBMODULE} up to date ({have[:12]}) — reusing cached pg_install/v17")
    return False


def make_env(*, cached):
    """Env for `make`: optionally mark the PG install cached so it's skipped."""
    env = os.environ.copy()
    if cached:
        env["PG_INSTALL_CACHED"] = "1"
    return env


def stage_and_package(target_dir):
    """Stage the release bundle and pack it into a versioned tarball."""
    sha = git_out("rev-parse", "--short", "HEAD")
    date = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d")
    bundle = Path(os.environ.get("BUNDLE_DIR", "/tmp/yb-light-bundle"))
    tarball = Path(
        os.environ.get("TARBALL", f"/tmp/yb-light-bundle-{date}-{sha}.tar.gz")
    )

    say(f"Staging bundle at {bundle}")
    # Single staging dir, reused across builds — wiped each run so the dev box
    # doesn't accumulate ~1.4 GB per build.
    if bundle.exists():
        shutil.rmtree(bundle)
    (bundle / "lib").mkdir(parents=True)
    (bundle / "tools").mkdir(parents=True)

    for name in DAEMON_BINS:
        shutil.copy2(target_dir / name, bundle / name)
    for name in DIAG_TOOLS:
        shutil.copy2(target_dir / name, bundle / "tools" / name)
    for lib in RUNTIME_LIBS:
        shutil.copy2(f"/usr/lib64/{lib}", bundle / "lib" / lib)

    # pg_install/v17/ — postgres binaries the compute spawns + the neon .so
    # extensions. Without a matching tree the compute loads a wire-incompatible
    # neon.so. copytree(symlinks=True) preserves the tree's symlinks.
    say(f"Staging pg_install/v17/ ({_du(PG_INSTALL)})...")
    (bundle / "pg_install").mkdir()
    shutil.copytree(PG_INSTALL, bundle / "pg_install" / "v17", symlinks=True)
    for lib in RUNTIME_LIBS:
        shutil.copy2(f"/usr/lib64/{lib}", bundle / "pg_install" / "v17" / "lib" / lib)

    pg_commit = git_out("-C", SUBMODULE, "rev-parse", "HEAD")
    head = git_out("rev-parse", "HEAD")
    subject = git_out("log", "-1", "--pretty=%s")
    now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC")
    (bundle / "REVISION.txt").write_text(
        f"Build host: {socket.gethostname()}\n"
        f"Build date: {now}\n"
        f"Commit:     {head}\n"
        f"Subject:    {subject}\n"
        f"PG commit:  {pg_commit}\n\n"
        "yb_light_local launches yb-pageserver (see local_env.rs::pageserver_bin).\n"
    )

    say("Bundle staged:")
    run(["ls", "-la", str(bundle)])

    # `-C bundle .` so it unpacks without a leading bundle/ component.
    say(f"Packaging tarball {tarball} ...")
    run(["tar", "-czf", str(tarball), "-C", str(bundle), "."])

    say("Bundle tarball ready:")
    run(["ls", "-lh", str(tarball)])
    print()
    say("Ship it to the deployment host, e.g.:")
    print(f"    scp {tarball} <host>:~/yb-light/builds/")
    print(f"    ssh <host> 'mkdir -p ~/yb-light/builds/{date}-{sha} \\")
    print(f"        && tar -xzf ~/yb-light/builds/{tarball.name} \\")
    print(f"             -C ~/yb-light/builds/{date}-{sha}'")


def _du(path):
    res = subprocess.run(["du", "-sh", str(path)], capture_output=True, text=True)
    return res.stdout.split()[0] if res.returncode == 0 else "?"


def main():
    ap = argparse.ArgumentParser(
        description="Build yb-light binaries + compute extensions.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    grp = ap.add_mutually_exclusive_group()
    grp.add_argument("--release", action="store_true", help="release build")
    grp.add_argument(
        "--release-tar",
        action="store_true",
        help="release build + shippable tarball",
    )
    grp.add_argument(
        "--headers",
        action="store_true",
        help="only install PostgreSQL server headers (for cargo check / IDE)",
    )
    args = ap.parse_args()

    if args.release_tar:
        mode, build_type, target_dir = "release-tar", "release", ROOT / "target/release"
    elif args.release:
        mode, build_type, target_dir = "release", "release", ROOT / "target/release"
    elif args.headers:
        mode, build_type, target_dir = "headers", "debug", None
    else:
        mode, build_type, target_dir = "debug", "debug", ROOT / "target/debug"

    say(f"Mode: {mode} (BUILD_TYPE={build_type})")
    rebuild_pg = sync_submodule_if_needed()

    if mode == "headers":
        # postgres-headers-install-% installs into pg_install/v17/include so a
        # bare cargo build/check can generate the postgres_ffi bindings.
        say("Installing PostgreSQL server headers...")
        run(
            ["make", f"BUILD_TYPE={build_type}", "postgres-headers-install-v17"],
            env=make_env(cached=False),
        )
        say("Headers installed under pg_install/v17/include/. `cargo check` will work now.")
        return

    say(f"Building {mode} binaries (yb-pageserver + supporting bins)...")

    # postgres_ffi's build script generates bindings from the PostgreSQL server
    # headers (pg_install/v17/include). On a fresh checkout or after a submodule
    # bump those headers do not exist yet, and they are only installed by the
    # neon-pg-ext-v17 step *below* (after this cargo build) -- so bindgen would
    # fail with "'c.h' file not found". When a (re)build is needed, install the
    # headers first (cheap, headers-only, idempotent) so the Rust build finds them.
    if rebuild_pg:
        say("Installing PostgreSQL server headers first (for postgres_ffi bindgen)...")
        run(
            ["make", f"BUILD_TYPE={build_type}", "postgres-headers-install-v17"],
            env=make_env(cached=False),
        )

    cargo = ["cargo", "build"] + (["--release"] if build_type == "release" else [])
    for crate in BUILD_CRATES:
        cargo += ["-p", crate]
    run(cargo)

    # `neon.so` + walredo/utils extensions are loaded by the compute postgres;
    # their wire protocol must match these binaries. We always recompile the
    # extensions (they link the Rust walproposer-lib, which rolls every commit);
    # when rebuild_pg is set we also drop PG_INSTALL_CACHED so the
    # postgres-install-v17 prereq recompiles PostgreSQL from the synced source.
    say("Building compute extensions (pgxn/yb_amp/*.so for v17)...")
    run(
        ["make", f"BUILD_TYPE={build_type}", "neon-pg-ext-v17"],
        env=make_env(cached=not rebuild_pg),
    )

    if mode != "release-tar":
        say(f"Build complete ({mode}). Binaries in {target_dir}/, extensions in pg_install/v17/.")
        say("Re-run with --release-tar to package a shippable bundle.")
        return

    stage_and_package(target_dir)


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as e:
        print(f"\nbuild failed: command exited {e.returncode}", file=sys.stderr)
        sys.exit(e.returncode)
