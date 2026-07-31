//! Stamp the repo-root `VERSION` file into the binaries as `AMP_VERSION`.
//!
//! `VERSION` is the single source of truth for the product's semantic version
//! (hybrid CalVer, e.g. `0.260714.0`) — the same file the Go controller reads
//! via `scripts/version.sh`. Reading it here, in the `utils` crate that every
//! compute binary depends on, lets the pageserver/safekeeper/compute-ctl/
//! endpoint-storage all report the same version as the controller.
//!
//! Using `cargo:rustc-env` + `cargo:rerun-if-changed` means the value is
//! re-stamped whenever `VERSION` changes, with no dependency on a build arg or
//! the environment — so it can't go stale through the endpoint image's BuildKit
//! `target` cache mount the way a bare `option_env!` can.

use std::path::PathBuf;

fn main() {
    // Walk up from this crate's manifest dir until we find the repo-root VERSION
    // file, so it resolves regardless of crate depth or where cargo is invoked
    // (local build or the endpoint Docker build's `COPY . .`).
    let mut dir = PathBuf::from(
        std::env::var("CARGO_MANIFEST_DIR").expect("CARGO_MANIFEST_DIR is always set by cargo"),
    );
    let mut version = String::from("0.0.0");
    loop {
        let candidate = dir.join("VERSION");
        if candidate.is_file() {
            if let Ok(contents) = std::fs::read_to_string(&candidate) {
                let trimmed = contents.trim();
                if !trimmed.is_empty() {
                    version = trimmed.to_string();
                }
            }
            // Re-run (and re-stamp) whenever VERSION changes.
            println!("cargo:rerun-if-changed={}", candidate.display());
            break;
        }
        if !dir.pop() {
            // Hit the filesystem root without finding VERSION — keep the default.
            break;
        }
    }

    println!("cargo:rustc-env=AMP_VERSION={version}");
}
