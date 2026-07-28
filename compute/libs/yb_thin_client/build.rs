//! Link strategy for the `libyb_thin_client` C++ shim.
//!
//! The shim (`shim/yb_thin_client.h` → `yb_thin_client.cc` in the YB tree)
//! is a *YugabyteDB build target*: it uses YB internals
//! (`PgClientServiceProxy`, `PartitionSchema`, `pg_doc_data`) + generated
//! protos and must be built with YB's Clang-21/libc++ toolchain, so it is
//! NOT compiled here — we link the prebuilt `libyb_thin_client.so` from the
//! YB build tree / release tarball. The tarball is published per-arch to S3
//! and fetched by CI and the deploy images (see `README.md` → Packaging).
//!
//! `shim/yb_thin_client.h` is **vendored verbatim** from the shim's release
//! (its canonical C ABI); `src/ffi.rs` mirrors it. To lock the two together,
//! `shim/VERSION` pins the shim version that ABI was vendored
//! from, and we assert the shim's shipped `version.txt` matches it. A
//! mismatch fails the build: re-vendor the header and bump the version file
//! before building against a newer shim.
//!
//! Resolution order for the lib dir:
//!   1. `YB_THIN_CLIENT_LIB_DIR` env var (explicit; wins).
//!   2. `YB_BUILD_ROOT`/lib, else `<YB_SOURCE_ROOT>/build/latest/lib`,
//!      else `$HOME/code/hari-yugabyte-db/build/release-clang21-dynamic-ninja/lib`.
//!
//! There is **no stub fallback**: `libyb_thin_client.so` is a hard build
//! requirement, so a build that can't find it fails loudly rather than
//! silently producing a client that returns `Unavailable` at runtime.
use std::path::{Path, PathBuf};

/// The shim version `shim/yb_thin_client.h` + `src/ffi.rs` were vendored
/// against. Kept in lockstep with the vendored header.
const PINNED_SHIM_VERSION: &str = include_str!("shim/VERSION");

fn env_dir(name: &str) -> Option<PathBuf> {
    println!("cargo:rerun-if-env-changed={name}");
    let p = PathBuf::from(std::env::var_os(name)?);
    p.is_dir().then_some(p)
}

fn shim_lib_dir() -> Option<PathBuf> {
    if let Some(d) = env_dir("YB_THIN_CLIENT_LIB_DIR") {
        return Some(d);
    }
    if let Some(build) = env_dir("YB_BUILD_ROOT") {
        return Some(build.join("lib"));
    }
    let candidates = [
        env_dir("YB_SOURCE_ROOT").map(|s| s.join("build/latest/lib")),
        std::env::var_os("HOME").map(|h| {
            PathBuf::from(h).join("code/hari-yugabyte-db/build/release-clang21-dynamic-ninja/lib")
        }),
    ];
    candidates
        .into_iter()
        .flatten()
        .find(|d| d.join("libyb_thin_client.so").is_file())
}

/// The shim's shipped version (release tarball `version.txt`, which sits
/// next to `lib/`). `None` when absent — e.g. a raw YB build dir that ships
/// no `version.txt`, where we can't verify.
fn shim_version(lib_dir: &Path) -> Option<String> {
    for p in [lib_dir.join("version.txt"), lib_dir.join("../version.txt")] {
        if let Ok(s) = std::fs::read_to_string(&p) {
            return Some(s.trim().to_string());
        }
    }
    None
}

fn main() {
    println!("cargo:rerun-if-changed=shim/yb_thin_client.h");
    println!("cargo:rerun-if-changed=shim/VERSION");

    let dir = shim_lib_dir().filter(|d| d.join("libyb_thin_client.so").is_file());
    let Some(dir) = dir else {
        panic!(
            "yb_thin_client: libyb_thin_client.so not found. It is a required build \
             dependency (no stub fallback). Set YB_THIN_CLIENT_LIB_DIR to a directory \
             containing libyb_thin_client.so (+ version.txt), or fetch the per-arch shim \
             tarball (see compute/libs/yb_thin_client/README.md → Packaging). Also tried \
             YB_BUILD_ROOT/lib, YB_SOURCE_ROOT/build/latest/lib, and \
             $HOME/code/hari-yugabyte-db/build/release-clang21-dynamic-ninja/lib."
        );
    };

    // Version guard: the linked shim must be the version the vendored ABI
    // header was cut from, or the build fails (ABI could drift).
    let pinned = PINNED_SHIM_VERSION.trim();
    match shim_version(&dir) {
        Some(found) if found != pinned => panic!(
            "yb_thin_client: linked shim is version {found}, but the vendored ABI \
             (shim/yb_thin_client.h + shim/VERSION) is {pinned}. \
             Re-vendor shim/yb_thin_client.h from the {found} release and update \
             shim/VERSION, then rebuild."
        ),
        Some(_) => {}
        None => println!(
            "cargo:warning=yb_thin_client: no version.txt beside the shim at {} — \
             cannot verify it matches the vendored ABI ({pinned})",
            dir.display()
        ),
    }
    let d = dir.display();
    // Link the shim; rpath so it resolves at runtime.
    println!("cargo:rustc-link-search=native={d}");
    println!("cargo:rustc-link-lib=dylib=yb_thin_client");
    println!("cargo:rustc-link-arg=-Wl,-rpath,{d}");
}
