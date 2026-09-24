//! Link strategy for the `libyb_thin_client` C++ shim.
//!
//! The shim (`shim/yb_thin_client.h` → `yb_thin_client.cc` in the YB tree)
//! is a *YugabyteDB build target*: it uses YB internals
//! (`ThinClientServiceProxy`, `PartitionSchema`, `pg_doc_data`) + generated
//! protos and must be built with YB's Clang-21/libc++ toolchain, so it is
//! NOT compiled here — we link the prebuilt `libyb_thin_client.so` from the
//! per-arch release tarball published to S3 (see `README.md` → Packaging).
//!
//! `shim/yb_thin_client.h` is **vendored verbatim** from the shim's release
//! (its canonical C ABI); `src/ffi.rs` mirrors it. To lock the two together,
//! `shim/VERSION` pins the shim version that ABI was vendored
//! from, and we assert the shim's shipped `version.txt` matches it. A
//! mismatch fails the build: re-vendor the header and bump the version file
//! before building against a newer shim.
//!
//! The version alone cannot tell two builds of one release apart, and an ABI
//! addition such as a new enum value exports no symbol, so linking the older
//! build succeeds. Where the shim's own `include/yb_thin_client.h` sits beside
//! its `lib/` — every extracted release tarball — the build also requires it
//! to equal the vendored header byte for byte. The deploy images stage only
//! the `.so`, fetched from the same pin in the same build, so they skip it.
//!
//! # Resolution
//!
//! 1. `YB_THIN_CLIENT_LIB_DIR`, when set — an explicit directory holding
//!    `libyb_thin_client.so`. Used by the deploy images (which stage the shim
//!    at `/opt/yb_thin_client/lib`) and by anyone pointing at a locally built
//!    shim on purpose.
//! 2. Otherwise the repo's gitignored `.shim-cache/`, keyed by the pinned
//!    version + commit + arch: reused untouched when already populated,
//!    otherwise fetched once by `scripts/fetch-yb-thin-client.sh`. This is the
//!    default, so an ordinary `cargo build` needs no setup at all.
//!
//! What is deliberately *gone* is the old silent fallback chain: `YB_BUILD_ROOT`,
//! `YB_SOURCE_ROOT/build/latest/lib`, and a hardcoded
//! `$HOME/code/hari-yugabyte-db/...` YB build directory. Those hold a
//! *developer's* shim rather than the pinned one, and because a raw build tree
//! ships no `version.txt` the guard below degraded to a warning — so an
//! unrelated local YB checkout got linked in silently, surfacing only as
//! undefined references at link time or an ABI mismatch at runtime. The
//! override above is explicit; nothing is guessed.
//!
//! There is **no stub fallback**: `libyb_thin_client.so` is a hard build
//! requirement, so a build that can't find it fails loudly rather than
//! silently producing a client that returns `Unavailable` at runtime.
use std::path::{Path, PathBuf};
use std::process::Command;

/// The shim version `shim/yb_thin_client.h` + `src/ffi.rs` were vendored
/// against. Kept in lockstep with the vendored header.
const PINNED_SHIM_VERSION: &str = include_str!("shim/VERSION");
/// The shim build the vendored ABI came from; with the version, it names the
/// published tarball (and so the cache entry).
const PINNED_SHIM_COMMIT: &str = include_str!("shim/COMMIT");
/// The ABI `src/ffi.rs` mirrors.
const VENDORED_HEADER: &str = include_str!("shim/yb_thin_client.h");

/// Repo root, derived from this crate's manifest dir
/// (`<repo>/compute/libs/yb_thin_client`).
fn repo_root() -> PathBuf {
    let manifest =
        PathBuf::from(std::env::var_os("CARGO_MANIFEST_DIR").expect("CARGO_MANIFEST_DIR"));
    manifest
        .ancestors()
        .nth(3)
        .unwrap_or(&manifest)
        .to_path_buf()
}

/// Target arch as the published tarballs name it. Uses `CARGO_CFG_TARGET_ARCH`
/// (the *target*, not the host) so a cross-build can't link the wrong shim.
fn tarball_arch() -> String {
    match std::env::var("CARGO_CFG_TARGET_ARCH").as_deref() {
        Ok("x86_64") => "x86_64".into(),
        Ok("aarch64") => "aarch64".into(),
        Ok(other) => panic!(
            "yb_thin_client: no libyb_thin_client.so is published for target arch {other}; \
             only x86_64 and aarch64 are."
        ),
        Err(e) => panic!("yb_thin_client: CARGO_CFG_TARGET_ARCH unreadable: {e}"),
    }
}

/// The cache entry for the pinned shim: `<repo>/.shim-cache/<tarball stem>`.
/// Mirrors `scripts/fetch-yb-thin-client.sh`, which populates it.
fn cache_entry(root: &Path, version: &str, commit: &str, arch: &str) -> PathBuf {
    root.join(".shim-cache").join(format!(
        "yb_thin_client-{version}-{commit}-release-linux-{arch}"
    ))
}

/// `libyb_thin_client.so`'s directory inside an extracted cache entry, or
/// `None` when the entry is absent/incomplete. The tarball's top-level
/// directory name is not part of the contract, so search for the `.so`.
fn find_shim_dir(entry: &Path) -> Option<PathBuf> {
    fn walk(dir: &Path, depth: usize) -> Option<PathBuf> {
        if depth > 6 {
            return None;
        }
        for e in std::fs::read_dir(dir).ok()?.flatten() {
            let p = e.path();
            if p.is_dir() {
                if let Some(found) = walk(&p, depth + 1) {
                    return Some(found);
                }
            } else if p.file_name().is_some_and(|n| n == "libyb_thin_client.so") {
                return p.parent().map(Path::to_path_buf);
            }
        }
        None
    }
    walk(entry, 0)
}

/// Populate the cache by running the fetch script (one download, then cached
/// for every later build). Only called on a miss.
fn fetch(root: &Path) {
    let script = root.join("scripts/fetch-yb-thin-client.sh");
    assert!(
        script.is_file(),
        "yb_thin_client: the pinned shim is not in {}/.shim-cache and the fetch script \
         {} is missing, so it cannot be downloaded. Restore the script, or pre-populate \
         the cache with the tarball's extracted contents.",
        root.display(),
        script.display()
    );
    let out = Command::new("bash")
        .arg(&script)
        .current_dir(root)
        .output()
        .unwrap_or_else(|e| panic!("yb_thin_client: running {}: {e}", script.display()));
    if !out.status.success() {
        // Both streams: a `set -e` abort can die with nothing on stderr, and
        // "failed, no output" is a miserable thing to debug.
        panic!(
            "yb_thin_client: {} failed ({}).\n--- stderr ---\n{}\n--- stdout ---\n{}",
            script.display(),
            out.status,
            String::from_utf8_lossy(&out.stderr).trim(),
            String::from_utf8_lossy(&out.stdout).trim()
        );
    }
}

/// The shim's shipped version (release tarball `version.txt`, which sits
/// next to `lib/`). `None` when absent — which should not happen for a
/// tarball-sourced shim, and is treated as an error by the caller.
fn shim_version(lib_dir: &Path) -> Option<String> {
    for p in [lib_dir.join("version.txt"), lib_dir.join("../version.txt")] {
        if let Ok(s) = std::fs::read_to_string(&p) {
            return Some(s.trim().to_string());
        }
    }
    None
}

/// The header the shim shipped with, or `None` when only the `.so` (and
/// `version.txt`) were staged.
fn shipped_header(lib_dir: &Path) -> Option<(PathBuf, String)> {
    let p = lib_dir.join("../include/yb_thin_client.h");
    std::fs::read_to_string(&p).ok().map(|s| (p, s))
}

fn main() {
    println!("cargo:rerun-if-changed=shim/yb_thin_client.h");
    println!("cargo:rerun-if-changed=shim/VERSION");
    println!("cargo:rerun-if-changed=shim/COMMIT");

    let pinned = PINNED_SHIM_VERSION.trim();
    let commit = PINNED_SHIM_COMMIT.trim();
    let root = repo_root();

    // 1. Explicit override, when set. An empty or `.so`-less directory is an
    //    error rather than a quiet fall-through to the cache: someone who set
    //    the variable meant to link *that* shim, and silently linking a
    //    different one is the failure mode this whole scheme exists to kill.
    println!("cargo:rerun-if-env-changed=YB_THIN_CLIENT_LIB_DIR");
    let dir = match std::env::var_os("YB_THIN_CLIENT_LIB_DIR") {
        Some(v) => {
            let d = PathBuf::from(v);
            assert!(
                d.join("libyb_thin_client.so").is_file(),
                "yb_thin_client: YB_THIN_CLIENT_LIB_DIR is set to {} but there is no \
                 libyb_thin_client.so there. Point it at a directory containing the shim \
                 (+ version.txt), or unset it to use the pinned shim in .shim-cache/.",
                d.display()
            );
            d
        }
        // 2. Default: the pinned shim in the repo cache. Cache-first — an
        //    already-extracted shim is reused with no download and no
        //    re-extraction, so only the first build after a pin bump goes out
        //    to the network.
        None => {
            let entry = cache_entry(&root, pinned, commit, &tarball_arch());
            match find_shim_dir(&entry) {
                Some(d) => d,
                None => {
                    fetch(&root);
                    find_shim_dir(&entry).unwrap_or_else(|| {
                        panic!(
                            "yb_thin_client: fetch reported success but no \
                             libyb_thin_client.so is under {}. Remove that directory and \
                             rebuild.",
                            entry.display()
                        )
                    })
                }
            }
        }
    };

    // Version guard: the linked shim must be the version the vendored ABI
    // header was cut from, or the build fails (ABI could drift). A missing
    // version.txt is now fatal too — the old chain treated it as "can't tell"
    // and warned, which is exactly how an unrelated local build tree used to
    // get linked in unnoticed.
    match shim_version(&dir) {
        Some(found) if found != pinned => panic!(
            "yb_thin_client: linked shim is version {found}, but the vendored ABI \
             (shim/yb_thin_client.h + shim/VERSION) is {pinned}. \
             Re-vendor shim/yb_thin_client.h from the {found} release and update \
             shim/VERSION, then rebuild."
        ),
        Some(_) => {}
        None => panic!(
            "yb_thin_client: no version.txt beside the shim at {}, so it cannot be checked \
             against the vendored ABI ({pinned}). A release tarball always ships one; an \
             unpinned YB build tree does not, and linking that is what this check exists to \
             prevent. Unset YB_THIN_CLIENT_LIB_DIR to use the pinned shim, or delete the \
             .shim-cache entry and rebuild to re-fetch.",
            dir.display(),
        ),
    }

    if let Some((path, shipped)) = shipped_header(&dir) {
        assert!(
            shipped == VENDORED_HEADER,
            "yb_thin_client: the linked shim's header {} differs from the vendored \
             shim/yb_thin_client.h, though both claim version {pinned}. They are different \
             builds, and src/ffi.rs mirrors the vendored one. Pin shim/COMMIT to the build \
             the header was vendored from, or re-vendor the header (and src/ffi.rs) from \
             this one.",
            path.display()
        );
    }

    let d = dir.display();
    // Link the shim. `rustc-link-arg` is scoped to THIS crate's own link
    // products — cargo does not propagate link args to a dependent's
    // binaries.
    println!("cargo:rustc-link-search=native={d}");
    println!("cargo:rustc-link-lib=dylib=yb_thin_client");
    println!("cargo:rustc-link-arg=-Wl,--enable-new-dtags");
    println!("cargo:rustc-link-arg=-Wl,-rpath,{d}");

    // Publish the resolved directory as build metadata so dependents can set
    // their OWN rpath. Requires `links = "yb_thin_client"` in Cargo.toml;
    // cargo then exposes this to every direct dependent's build script as
    // `DEP_YB_THIN_CLIENT_LIB_DIR`. The daemon crates read it and emit their
    // own `-Wl,-rpath`, so `pageserver` / `safekeeper` and their test
    // binaries resolve the shim unaided — no `LD_LIBRARY_PATH` needed for a
    // bare `cargo test` or a hand-launched `yb_amp_local`.
    //
    // `LD_LIBRARY_PATH` still wins where it is set (run.sh, the deploy
    // images, CI): the linker emits DT_RUNPATH, which the loader consults
    // AFTER LD_LIBRARY_PATH. So a stale baked-in developer path can never
    // shadow a deliberately configured one.
    println!("cargo:lib-dir={d}");
}
