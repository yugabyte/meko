#!/usr/bin/env python3
"""Build yb-amp binaries + compute extensions — the one build entry point.

Replaces the manual `git submodule update` + `make` dance from the README.
It handles a fresh checkout, the PostgreSQL-headers prerequisite, and the
debug/release/tarball builds, and keeps the vendored Postgres in sync.

Modes (pick one; default is debug):
    (no flag)        debug build   (cargo dev profile -> target/debug)
    --release        release build (-> target/release)
    --release-tar    release build + package a shippable tarball
                     (/tmp/yb-amp-bundle-<date>-<sha>.tar.gz)
    --headers        only install the PostgreSQL server headers, so a bare
                     `cargo build` / `cargo check` / IDE can find them (the
                     postgres_ffi crate generates bindings from them).

--headers and --pg-only build part of a tree rather than selecting one, so
either may be combined with --release: `--pg-only --release` is how the
integration suite's --release mode gets its PostgreSQL and nothing else.

The mode selects one build type for everything: the Rust amp stack (cargo dev
vs release profile), the compute extensions, and the PostgreSQL they are built
against. A debug build gets an assertion-enabled PostgreSQL, which is what the
integration suites run against; --release gets one without. The two live in
separate directories (pg_install-debug/ vs pg_install-release/, see the
Makefile), so building one never clobbers the other. Both are suffixed: the
unsuffixed pg_install/ held a release tree before the split, and the type is
baked into <build dir>/<v>/config.status, so naming neither type after it keeps
a leftover from being reused. A tree configured by hand is still wiped and
reconfigured.

PostgreSQL submodule handling (every mode):
    ybAmp builds against a patched PostgreSQL tree vendored as the
    `compute/vendor/postgres-v18` git submodule. This script checks it out if it is
    missing, and when the gitlink recorded in HEAD differs from the commit in
    the working tree (e.g. a rebase/pull bumped it) it re-syncs and rebuilds
    PostgreSQL from source. The cached install is reused (PG_INSTALL_CACHED=1,
    only the C extensions recompiled) when it matches on both halves of its key:
    the submodule commit it was built from (.pg_commit) and the makefiles that
    produced it (.pg_makefiles — Makefile + postgres.mk). Without the second, a
    contrib module added to postgres.mk, or a configure flag edited into either
    file, ships a stale Postgres.
"""

import argparse
import datetime
import hashlib
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


def submodule_in_tree():
    """SUBMODULE as git's object tree records it, i.e. relative to the repo root.

    git submodule pathspecs and `git -C` are relative to the cwd, but `HEAD:<path>`
    never is. When the amp tree is a subdirectory of a larger repo the two differ, so
    ask git for the prefix rather than assuming either layout.
    """
    return (git_out("rev-parse", "--show-prefix") or "") + SUBMODULE


PG_VERSION = "v18"


def make_pg_dirs(build_type):
    """Where make puts this build type's trees: (install root, build dir).

    Asking make keeps a single derivation of these paths. Re-deriving them here
    would diverge the moment POSTGRES_INSTALL_DIR is overridden (the Makefile
    honours it, and so do we): make would install to the override while the
    checks below read the default, find no pg_config there, and reconfigure
    PostgreSQL from scratch on every build. The suites read the same target — see
    test/integration/compute/pg_tree.sh.
    """
    res = subprocess.run(
        ["make", "-s", "print-pg-dirs", f"BUILD_TYPE={build_type}"],
        cwd=ROOT, stdout=subprocess.PIPE, text=True, check=True,
    )
    # Keyed, not positional: a mixed-up pair here would have wipe_pg_trees delete
    # the wrong tree. KeyError is the right outcome if make stops printing one.
    dirs = dict(
        line.split("=", 1) for line in res.stdout.splitlines() if "=" in line
    )
    return Path(dirs["POSTGRES_INSTALL_DIR"]), Path(dirs["BUILD_DIR"])


class PgTree:
    """Where one PostgreSQL build type lives, as make reports it.

    debug and release never share a tree. The type is baked into
    <build dir>/<v>/config.status at configure time and never re-read, so one
    shared tree silently serves whichever consumer configured it first — which is
    how the integration suite lost its assertions locally (#1094).
    """

    def __init__(self, build_type):
        assert build_type in ("debug", "release"), build_type
        self.build_type = build_type
        self.install_root, self.build_dir = make_pg_dirs(build_type)
        self.install = self.install_root / PG_VERSION
        # Stamp test/integration/compute/run.sh touches after building the compute
        # extensions; removed here on a PG rebuild so its guard recompiles them
        # against the new headers (see --pg-only).
        self.ext_stamp = self.build_dir / ".yb_amp_ext.stamp"
        # Which submodule commit this install was built from, and which makefiles
        # shaped it. Together they are the cache key: the first notices a
        # header/source change the gitlink cannot (an FMGR_ABI_EXTRA bump), the
        # second notices a recipe change that bumps no commit at all.
        self.commit_stamp = self.install / ".pg_commit"
        self.makefiles_stamp = self.install / ".pg_makefiles"

    def __str__(self):
        return f"{self.install_root.name}/{PG_VERSION} ({self.build_type})"


# Binaries launched by yb_amp_local (see control_plane local_env.rs).
DAEMON_BINS = [
    "pageserver",
    "yb_amp_local",
    # yb_amp_local execs a sibling `controller` binary (see
    # local_env.rs::controller_bin / localenv.go::ControllerBin), so it must be
    # staged next to it.
    "controller",
    "safekeeper",
    "compute-ctl",
]
# Operator-side diagnostic tools (the `tools` crate, compute/tools/); they
# share the YbConnArgs connection flags and work against no-TLS and TLS clusters.
DIAG_TOOLS = ["dump-pages", "dump-wals", "reset-consistent-lsn"]
# Crates that produce the binaries above.
BUILD_CRATES = [
    "pageserver",
    "safekeeper",
    "compute_tools",
    "tools",
]
# Runtime libs not present on stock AlmaLinux/RHEL 9 (OpenSSL 1.1, ICU 60).
# See memory/bundle-runtime-libs.md. Staged top-level for the daemons and into
# the install's v18/lib/ for the walredo postgres child (LD_LIBRARY_PATH is only
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


# Stamp recording the submodule commit the install was built from. It is the
# only way to notice that a header/source change (e.g. an FMGR_ABI_EXTRA bump)
# invalidated the cached PG + every extension linked against it, since the
# gitlink and the checked-out working tree can match while the *install* is
# stale.
# The other half of the cache key. The commit stamp says which PG source; these
# say how it was built — postgres.mk's contrib set, and the PG_CFLAGS /
# PG_CONFIGURE_OPTS the Makefile hands configure. Editing either bumps no
# submodule commit, so the commit stamp alone calls a stale install fresh. CI
# keys on the same two files (amp-pr-build.yml hashFiles).
#
# File contents only: flags that reach configure from the environment or host
# (CFLAGS, WITH_SANITIZERS, the .docker_build branch) leave these bytes
# unchanged, so WITH_SANITIZERS=yes still reuses a non-sanitized install.
PG_MAKEFILES = ("Makefile", "postgres.mk")


def worktree_pg_makefiles_hash():
    """Hash of the makefiles as they stand in the checkout right now.

    Framed by name and length: concatenating the contents alone would hash the
    same whether a line sits at the end of Makefile or the start of postgres.mk.
    """
    h = hashlib.sha256()
    for name in PG_MAKEFILES:
        data = (ROOT / name).read_bytes()
        h.update(f"{name}:{len(data)}\n".encode())
        h.update(data)
    return h.hexdigest()


def built_pg_makefiles_hash(pg):
    """The makefiles hash this install was last built from, or None."""
    return (
        pg.makefiles_stamp.read_text().strip()
        if pg.makefiles_stamp.is_file() else None
    )


def pg_makefiles_changed(pg):
    """True if the makefiles differ from the ones this install was built from."""
    return built_pg_makefiles_hash(pg) != worktree_pg_makefiles_hash()


def record_pg_makefiles(pg):
    """Stamp the install with the makefiles it was just built from."""
    pg.makefiles_stamp.write_text(worktree_pg_makefiles_hash() + "\n")


def built_pg_commit(pg):
    """The submodule commit this install was last built from, or None."""
    return pg.commit_stamp.read_text().strip() if pg.commit_stamp.is_file() else None


def record_pg_commit(pg):
    """Stamp the install with the submodule commit it was just built from."""
    have = git_out("-C", SUBMODULE, "rev-parse", "HEAD")
    if have:
        pg.commit_stamp.write_text(have + "\n")


def pg_install_matches_commit(pg):
    """True if this install was built from the commit now checked out.

    Call ensure_submodule() first, so this compares against a submodule already
    at the gitlink and a bump can only surface as .pg_commit disagreeing. An
    absent install does not match. Reads only; main() does the wiping.
    """
    if not (pg.install / "bin" / "postgres").is_file():
        say(f"{pg} missing — building PostgreSQL from source...")
        return False
    have = git_out("-C", SUBMODULE, "rev-parse", "HEAD")
    built = built_pg_commit(pg)
    if built != have:
        say(f"{pg} was built from {(built or 'unknown')[:12]} but the "
            f"submodule is at {(have or 'unknown')[:12]} — rebuilding PostgreSQL")
        return False
    return True


def ensure_submodule():
    """Check compute/vendor/postgres-v18 out if it isn't already at the gitlink.

    Self-guarding, so callers need not work out whether a checkout is due.
    """
    if git_out("rev-parse", f"HEAD:{submodule_in_tree()}") == git_out(
        "-C", SUBMODULE, "rev-parse", "HEAD"
    ):
        return
    say(f"Syncing {SUBMODULE}...")
    run(["git", "submodule", "update", "--init", "--recursive", "--", SUBMODULE])


def make_env(*, cached):
    """Env for `make`: optionally mark the PG install cached so it's skipped."""
    env = os.environ.copy()
    if cached:
        env["PG_INSTALL_CACHED"] = "1"
    return env


def installed_pg_build_type(pg):
    """The build type the install actually has, or None if it isn't installed.

    The debug configure adds `--enable-cassert`; release omits it. We read the
    recorded configure line from the installed `pg_config` — the ground truth for
    how that PG was actually built, since the type is baked in at configure time
    (<build dir>/<v>/config.status).

    Kept even though each type now has its own directory, for a tree somebody
    configured by hand or with an explicit POSTGRES_INSTALL_DIR: config.status is
    never re-read, so the wrong type would otherwise be served indefinitely.
    """
    pg_config = pg.install / "bin" / "pg_config"
    if not pg_config.is_file():
        return None
    res = subprocess.run(
        [str(pg_config), "--configure"], capture_output=True, text=True
    )
    if res.returncode != 0:
        return None
    return "debug" if "--enable-cassert" in res.stdout else "release"


def wipe_pg_trees(pg, why):
    """Drop the PG build tree + install so the next build reconfigures cleanly.

    config.status is a bare file target in postgres.mk with no prerequisite on
    the makefiles, so make reuses it however the recipe changed — re-running
    `make` picks up neither a new build type nor new PG_CONFIGURE_OPTS.
    Removing both dirs is what forces configure to run again.
    """
    for p in (pg.build_dir / PG_VERSION, pg.install):
        if p.exists():
            say(f"Removing {p} — {why}")
            shutil.rmtree(p)


def stage_and_package(target_dir, pg):
    """Stage the release bundle and pack it into a versioned tarball."""
    sha = git_out("rev-parse", "--short", "HEAD")
    date = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d")
    bundle = Path(os.environ.get("BUNDLE_DIR", "/tmp/yb-amp-bundle"))
    tarball = Path(
        os.environ.get("TARBALL", f"/tmp/yb-amp-bundle-{date}-{sha}.tar.gz")
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
    # Always staged under `pg_install/`, whatever tree it was built in: that is
    # the layout the deployment host expects.
    say(f"Staging pg_install/v18/ from {pg} ({_du(pg.install)})...")
    (bundle / "pg_install").mkdir()
    shutil.copytree(pg.install, bundle / "pg_install" / "v18", symlinks=True)
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
        "yb_amp_local launches pageserver (see local_env.rs::pageserver_bin).\n"
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
    print(f"    scp {tarball} <host>:~/yb-amp/builds/")
    print(f"    ssh <host> 'mkdir -p ~/yb-amp/builds/{date}-{sha} \\")
    print(f"        && tar -xzf ~/yb-amp/builds/{tarball.name} \\")
    print(f"             -C ~/yb-amp/builds/{date}-{sha}'")


def _du(path):
    res = subprocess.run(["du", "-sh", str(path)], capture_output=True, text=True)
    return res.stdout.split()[0] if res.returncode == 0 else "?"


def main():
    ap = argparse.ArgumentParser(
        description="Build yb-amp binaries + compute extensions.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    # Two independent axes: WHICH build type, and HOW MUCH of it to build.
    # --pg-only --release is the integration suite asking for a release
    # PostgreSQL tree and nothing else, so they cannot share one group.
    typ = ap.add_mutually_exclusive_group()
    typ.add_argument("--release", action="store_true", help="release build")
    typ.add_argument(
        "--release-tar",
        action="store_true",
        help="release build + shippable tarball",
    )
    part = ap.add_mutually_exclusive_group()
    part.add_argument(
        "--headers",
        action="store_true",
        help="only install PostgreSQL server headers (for cargo check / IDE)",
    )
    part.add_argument(
        "--pg-only",
        action="store_true",
        help="only (re)build PostgreSQL when the submodule commit drifted; skip "
             "the Rust workspace + extension build (used by run.sh)",
    )
    args = ap.parse_args()

    if args.release_tar and (args.headers or args.pg_only):
        ap.error("--release-tar packages a full build; "
                 "it cannot be combined with --headers / --pg-only")

    build_type = "release" if (args.release or args.release_tar) else "debug"
    if args.headers:
        mode, target_dir = "headers", None
    elif args.pg_only:
        mode, target_dir = "pg-only", None
    elif args.release_tar:
        mode, target_dir = "release-tar", ROOT / f"target/{build_type}"
    else:
        mode, target_dir = build_type, ROOT / f"target/{build_type}"

    pg = PgTree(build_type)
    pg_make = f"BUILD_TYPE={pg.build_type}"
    # Point every child at the tree we resolved: postgres_ffi's build script reads
    # this to find the server headers it generates bindings from, so without it a
    # --release build (in any mode but --pg-only) would compile the Rust workspace
    # against the debug headers while installing the extensions into the release
    # tree.
    #
    # Only when it differs from the path that build script already infers when the
    # variable is unset. It declares cargo:rerun-if-env-changed on it, so setting
    # it to the very path cargo would have used anyway is not a no-op — it flips
    # set/unset against a bare `cargo build` (or rust-analyzer) and recompiles
    # postgres_ffi and every dependent on each alternation.
    if pg.install_root != ROOT / "pg_install-debug":
        os.environ["POSTGRES_INSTALL_DIR"] = str(pg.install_root)

    say(f"Mode: {mode} (BUILD_TYPE={build_type}; PostgreSQL {pg})")
    # Ahead of the mode dispatch: --headers builds none of PostgreSQL but still
    # configures from the submodule source.
    ensure_submodule()

    pg_target = f"postgres-install-{PG_VERSION}"
    headers_target = f"postgres-headers-install-{PG_VERSION}"
    ext_target = f"yb-amp-pg-ext-{PG_VERSION}"

    if mode == "headers":
        # postgres-headers-install-% installs into the tree's v18/include so a
        # bare cargo build/check can generate the postgres_ffi bindings. Headers
        # are build-type-independent, so this mode never wipes/rebuilds PG — it
        # runs while a local cluster is up, and a wipe would strip bin/postgres
        # and every contrib .so out from under it.
        say(f"Installing PostgreSQL server headers ({pg})...")
        run(["make", pg_make, headers_target], env=make_env(cached=False))
        say(f"Headers installed under {pg.install}/include/. "
            "`cargo check` will work now.")
        return

    # Below the --headers return, which does none of this work.
    rebuild_pg = not pg_install_matches_commit(pg)

    # Asked here, not inside the check above, so the wipe cannot be shadowed by
    # whichever trigger fires first: a rebuild that keeps config.status picks up
    # no new configure flags, yet record_pg_makefiles() would stamp it as if it
    # had.
    if pg_makefiles_changed(pg):
        wipe_pg_trees(pg, "postgres.mk/Makefile changed since this install was built")
        rebuild_pg = True

    # The tree must actually BE the type we asked for. It can only be the other
    # one if it was configured before the per-type trees existed, or by hand —
    # and the type is baked into config.status, so only removing both dirs
    # re-runs configure.
    if installed_pg_build_type(pg) != pg.build_type:
        if pg.install.exists() or (pg.build_dir / PG_VERSION).exists():
            wipe_pg_trees(pg, f"it is not a {pg.build_type} build")
        rebuild_pg = True

    if not rebuild_pg:
        say(f"{SUBMODULE} up to date — reusing cached {pg}")

    # Build (or rebuild) PostgreSQL in release *before* cargo: postgres_ffi's
    # build script generates bindings from the installed server headers
    # (<install>/v18/include), and postgres-install installs those headers.
    # Skipped when the cached PG is already release and the submodule matches.
    if rebuild_pg:
        say(f"Building PostgreSQL {PG_VERSION} from source ({pg})...")
        run(["make", pg_make, pg_target], env=make_env(cached=False))
        record_pg_commit(pg)
        record_pg_makefiles(pg)
        # PG headers just moved (struct layout, FMGR_ABI_EXTRA, ...). The PGXS
        # extension build has no header-dependency tracking, so an incremental
        # `make` would relink stale .o files into an ABI-mismatched .so. Wipe the
        # extension build tree so it recompiles clean against the freshly
        # installed headers.
        ext_build = pg.build_dir / f"pgxn-{PG_VERSION}"
        if ext_build.exists():
            say(f"PostgreSQL rebuilt — extensions will recompile against new {PG_VERSION} headers")
            shutil.rmtree(ext_build)
        # In --pg-only we don't build the extensions (run.sh does); drop its
        # stamp so run.sh's guard notices and clean-rebuilds them.
        if mode == "pg-only":
            pg.ext_stamp.unlink(missing_ok=True)

    if mode == "pg-only":
        say("--pg-only: PostgreSQL is current; skipping Rust + extension build.")
        return

    say(f"Building {mode} binaries (pageserver + supporting bins)...")
    cargo = ["cargo", "build"] + (["--release"] if build_type == "release" else [])
    for crate in BUILD_CRATES:
        cargo += ["-p", crate]
    run(cargo)

    # yb_amp_local and controller are Go binaries (controller/), no longer cargo
    # crates. Build both into the cargo target dir so the DAEMON_BINS copy (and
    # yb_amp_local's next-to-itself controller lookup) resolve them.
    run(["go", "build", "-C", "controller", "-o", str(target_dir / "yb_amp_local"),
         "./local/cmd/yb_amp_local"])
    run(["go", "build", "-C", "controller", "-o", str(target_dir / "controller"),
         "./cmd/controller"])

    # `yb_amp.so` + walredo/utils extensions are loaded by the compute postgres;
    # their wire protocol must match these binaries, and they link the Rust
    # walproposer-lib (which rolls every commit), so recompile them every run.
    # They follow the selected build type. PG is already built (release) above,
    # so PG_INSTALL_CACHED=1 keeps this step from touching it.
    say(f"Building compute extensions (compute/pgxn/yb_amp/*.so for {PG_VERSION}, {build_type})...")
    run(
        ["make", pg_make, ext_target],
        env=make_env(cached=True),
    )

    if mode != "release-tar":
        say(f"Build complete ({mode}). Binaries in {target_dir}/, extensions in {pg.install}/.")
        say("Re-run with --release-tar to package a shippable bundle.")
        return

    stage_and_package(target_dir, pg)


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as e:
        print(f"\nbuild failed: command exited {e.returncode}", file=sys.stderr)
        sys.exit(e.returncode)
    except OSError as e:
        # e.g. a partial checkout missing a file the makefiles hash reads.
        print(f"\nbuild failed: {e}", file=sys.stderr)
        sys.exit(1)
