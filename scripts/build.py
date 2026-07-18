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

The mode selects the build type for the Rust amp stack (cargo dev vs release
profile) and the compute extensions. PostgreSQL itself is ALWAYS built in
release mode (for now) — a debug PG (-O0 + asserts) makes the walredo child
needlessly slow, and the PG build type is baked into build/<v>/config.status,
so a cached debug PG is detected and rebuilt in release regardless of mode.

PostgreSQL submodule handling (every mode):
    ybAmp builds against a patched PostgreSQL tree vendored as the
    `compute/vendor/postgres-v18` git submodule. This script checks it out if it is
    missing, and when the gitlink recorded in HEAD differs from the commit in
    the working tree (e.g. a rebase/pull bumped it) it re-syncs and rebuilds
    PostgreSQL from source. When the submodule already matches and
    pg_install/v18 exists, the cached install is reused (PG_INSTALL_CACHED=1)
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
SUBMODULE = "compute/vendor/postgres-v18"
PG_VERSION = "v18"
PG_INSTALL = ROOT / "pg_install" / PG_VERSION
BUILD_DIR = ROOT / "build"
# Stamp test/integration/compute/run.sh touches after building the compute extensions;
# removed here on a PG rebuild so run.sh's guard recompiles them against the new
# headers (see --pg-only).
EXT_STAMP = BUILD_DIR / ".yb_amp_ext.stamp"
# PostgreSQL is always built in release mode (see module docstring). Only the
# compute extensions and the Rust amp stack follow the selected debug/release.
PG_BUILD_TYPE = "release"

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
# pg_install/v18/lib/ for the walredo postgres child (LD_LIBRARY_PATH is only
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


# Stamp recording the submodule commit pg_install/v18 was built from. It is the
# only way to notice that a header/source change (e.g. an FMGR_ABI_EXTRA bump)
# invalidated the cached PG + every extension linked against it, since the
# gitlink and the checked-out working tree can match while the *install* is
# stale. Shared with test/integration/compute/run.sh, which reads/writes the same file.
PG_COMMIT_STAMP = PG_INSTALL / ".pg_commit"


def built_pg_commit():
    """The submodule commit pg_install/v18 was last built from, or None."""
    return PG_COMMIT_STAMP.read_text().strip() if PG_COMMIT_STAMP.is_file() else None


def record_pg_commit():
    """Stamp pg_install with the submodule commit it was just built from."""
    have = git_out("-C", SUBMODULE, "rev-parse", "HEAD")
    if have:
        PG_COMMIT_STAMP.write_text(have + "\n")


def sync_submodule_if_needed():
    """Check out / re-sync compute/vendor/postgres-v18. Return True if PG needs a rebuild.

    PG must be rebuilt when the working tree doesn't match the gitlink (a bump,
    or a fresh/absent checkout), when pg_install/v18 was never built, or when
    the cached install was built from a DIFFERENT submodule commit than the one
    now checked out. The last case is the one a bare gitlink/working-tree check
    misses: after a commit bumps the submodule and the tree is re-synced, the
    gitlink and working tree agree while the install still holds the old PG (and
    old headers), so a header change like FMGR_ABI_EXTRA would otherwise ship a
    stale ABI to every extension.
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
        say("pg_install/v18 missing — building PostgreSQL from source...")
        run(["git", "submodule", "update", "--init", "--recursive", "--", SUBMODULE])
        return True
    built = built_pg_commit()
    if built != have:
        say(f"pg_install/v18 was built from {(built or 'unknown')[:12]} but the "
            f"submodule is at {have[:12]} — rebuilding PostgreSQL")
        return True
    say(f"{SUBMODULE} up to date ({have[:12]}) — reusing cached pg_install/v18")
    return False


def make_env(*, cached):
    """Env for `make`: optionally mark the PG install cached so it's skipped."""
    env = os.environ.copy()
    if cached:
        env["PG_INSTALL_CACHED"] = "1"
    return env


def pg_install_is_release():
    """True if the cached pg_install is a *release* PG build.

    The debug configure adds `--enable-cassert` (and builds at -O0); release
    omits it. We read the recorded configure line from the installed
    `pg_config` — the ground truth for how the cached PG was actually built,
    since the type is baked in at configure time (build/<v>/config.status).
    Returns False if PG isn't installed.
    """
    pg_config = PG_INSTALL / "bin" / "pg_config"
    if not pg_config.is_file():
        return False
    res = subprocess.run(
        [str(pg_config), "--configure"], capture_output=True, text=True
    )
    return res.returncode == 0 and "--enable-cassert" not in res.stdout


def force_pg_release_reconfigure():
    """Drop the PG build tree + install so the next build reconfigures release.

    The build type is baked into build/<v>/config.status, and the release
    configure omits `--enable-depend`, so simply re-running `make` with a new
    BUILD_TYPE won't switch a debug tree to release — the stale config.status
    would be reused. Removing both dirs forces a clean release configure.
    """
    for p in (BUILD_DIR / PG_VERSION, PG_INSTALL):
        if p.exists():
            say(f"Removing {p} to force a clean release PostgreSQL rebuild")
            shutil.rmtree(p)


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

    # pg_install/v18/ — postgres binaries the compute spawns + the yb_amp .so
    # extensions. Without a matching tree the compute loads a wire-incompatible
    # yb_amp.so. copytree(symlinks=True) preserves the tree's symlinks.
    say(f"Staging pg_install/v18/ ({_du(PG_INSTALL)})...")
    (bundle / "pg_install").mkdir()
    shutil.copytree(PG_INSTALL, bundle / "pg_install" / "v18", symlinks=True)
    for lib in RUNTIME_LIBS:
        shutil.copy2(f"/usr/lib64/{lib}", bundle / "pg_install" / "v18" / "lib" / lib)

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
    grp.add_argument(
        "--pg-only",
        action="store_true",
        help="only (re)build PostgreSQL when the submodule commit drifted; skip "
             "the Rust workspace + extension build (used by run.sh)",
    )
    args = ap.parse_args()

    if args.release_tar:
        mode, build_type, target_dir = "release-tar", "release", ROOT / "target/release"
    elif args.release:
        mode, build_type, target_dir = "release", "release", ROOT / "target/release"
    elif args.headers:
        mode, build_type, target_dir = "headers", "debug", None
    elif args.pg_only:
        mode, build_type, target_dir = "pg-only", "debug", None
    else:
        mode, build_type, target_dir = "debug", "debug", ROOT / "target/debug"

    say(f"Mode: {mode} (amp stack + extensions BUILD_TYPE={build_type}; "
        f"PostgreSQL always {PG_BUILD_TYPE})")
    rebuild_pg = sync_submodule_if_needed()

    pg_target = f"postgres-install-{PG_VERSION}"
    headers_target = f"postgres-headers-install-{PG_VERSION}"
    ext_target = f"yb-amp-pg-ext-{PG_VERSION}"

    if mode == "headers":
        # postgres-headers-install-% installs into pg_install/v18/include so a
        # bare cargo build/check can generate the postgres_ffi bindings. Headers
        # are build-type-independent, so this mode never wipes/rebuilds PG.
        say(f"Installing PostgreSQL server headers ({PG_BUILD_TYPE})...")
        run(
            ["make", f"BUILD_TYPE={PG_BUILD_TYPE}", headers_target],
            env=make_env(cached=False),
        )
        say("Headers installed under pg_install/v18/include/. `cargo check` will work now.")
        return

    # PostgreSQL must be a release build regardless of mode. If the cache is a
    # debug build (or a partially-configured debug tree), wipe it so the build
    # below reconfigures release — the type is baked into config.status.
    if not pg_install_is_release():
        if PG_INSTALL.exists() or (BUILD_DIR / PG_VERSION).exists():
            say("Cached pg_install is not a release build — rebuilding PostgreSQL in release mode")
            force_pg_release_reconfigure()
        rebuild_pg = True

    # Build (or rebuild) PostgreSQL in release *before* cargo: postgres_ffi's
    # build script generates bindings from the installed server headers
    # (pg_install/v18/include), and postgres-install installs those headers.
    # Skipped when the cached PG is already release and the submodule matches.
    if rebuild_pg:
        say(f"Building PostgreSQL {PG_VERSION} from source ({PG_BUILD_TYPE})...")
        run(
            ["make", f"BUILD_TYPE={PG_BUILD_TYPE}", pg_target],
            env=make_env(cached=False),
        )
        record_pg_commit()
        # PG headers just moved (struct layout, FMGR_ABI_EXTRA, ...). The PGXS
        # extension build has no header-dependency tracking, so an incremental
        # `make` would relink stale .o files into an ABI-mismatched .so. Wipe the
        # extension build tree so it recompiles clean against the freshly
        # installed headers.
        ext_build = BUILD_DIR / f"pgxn-{PG_VERSION}"
        if ext_build.exists():
            say(f"PostgreSQL rebuilt — extensions will recompile against new {PG_VERSION} headers")
            shutil.rmtree(ext_build)
        # In --pg-only we don't build the extensions (run.sh does); drop its
        # stamp so run.sh's guard notices and clean-rebuilds them.
        if mode == "pg-only":
            EXT_STAMP.unlink(missing_ok=True)

    if mode == "pg-only":
        say("--pg-only: PostgreSQL is current; skipping Rust + extension build.")
        return

    say(f"Building {mode} binaries (yb-pageserver + supporting bins)...")
    cargo = ["cargo", "build"] + (["--release"] if build_type == "release" else [])
    for crate in BUILD_CRATES:
        cargo += ["-p", crate]
    run(cargo)

    # `yb_amp.so` + walredo/utils extensions are loaded by the compute postgres;
    # their wire protocol must match these binaries, and they link the Rust
    # walproposer-lib (which rolls every commit), so recompile them every run.
    # They follow the selected build type. PG is already built (release) above,
    # so PG_INSTALL_CACHED=1 keeps this step from touching it.
    say(f"Building compute extensions (compute/pgxn/yb_amp/*.so for {PG_VERSION}, {build_type})...")
    run(
        ["make", f"BUILD_TYPE={build_type}", ext_target],
        env=make_env(cached=True),
    )

    if mode != "release-tar":
        say(f"Build complete ({mode}). Binaries in {target_dir}/, extensions in pg_install/v18/.")
        say("Re-run with --release-tar to package a shippable bundle.")
        return

    stage_and_package(target_dir)


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as e:
        print(f"\nbuild failed: command exited {e.returncode}", file=sys.stderr)
        sys.exit(e.returncode)
