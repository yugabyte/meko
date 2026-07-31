//! Bake an rpath to the `libyb_thin_client` shim into this crate's binaries
//! and test executables.
//!
//! `yb_thin_client`'s own build script cannot do this for us: a build script's
//! `cargo:rustc-link-arg` applies only to its own package's link products, and
//! cargo does not propagate link args to a dependent's binaries. So without
//! this file, `yb-safekeeper` and `cargo test -p safekeeper` link against
//! `libyb_thin_client.so` with no way to find it at run time, and fail with
//! `error while loading shared libraries` unless the caller exports
//! `LD_LIBRARY_PATH`.
//!
//! The directory arrives via cargo's `links` metadata channel:
//! `yb_thin_client` declares `links = "yb_thin_client"` and emits
//! `cargo:lib-dir=<dir>`, which cargo hands to direct dependents' build
//! scripts as `DEP_YB_THIN_CLIENT_LIB_DIR`. Reading it here keeps the
//! path-resolution logic (env override, pinned version/commit/arch,
//! `.shim-cache` fetch) in exactly one place.
//!
//! Unsuffixed `rustc-link-arg` covers binaries *and* test targets, which is
//! the point — the daemon and its live-DB tests both need it.
//!
//! `LD_LIBRARY_PATH` still takes precedence where it is set (`run.sh`, the
//! deploy images, CI): this emits DT_RUNPATH, which the loader searches after
//! `LD_LIBRARY_PATH`. A developer's baked-in path can therefore never shadow
//! a deliberately configured one in a container.

fn main() {
    // Absent only if the dependency graph changes such that yb_thin_client is
    // no longer a direct dependency; then there is no shim to point at and
    // nothing to do.
    if let Some(dir) = std::env::var_os("DEP_YB_THIN_CLIENT_LIB_DIR") {
        let dir = dir.to_string_lossy();
        // `--enable-new-dtags` is load-bearing: without it the linker emits
        // DT_RPATH, which the loader searches BEFORE `LD_LIBRARY_PATH` — a
        // baked-in developer path would then shadow the one a deploy image
        // sets. DT_RUNPATH is searched after, so the container always wins.
        println!("cargo:rustc-link-arg=-Wl,--enable-new-dtags");
        println!("cargo:rustc-link-arg=-Wl,-rpath,{dir}");
    }
    println!("cargo:rerun-if-env-changed=DEP_YB_THIN_CLIENT_LIB_DIR");
}
