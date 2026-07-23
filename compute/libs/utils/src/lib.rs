//! `utils` is intended to be a place to put code that is shared
//! between other crates in this repository.
#![deny(clippy::undocumented_unsafe_blocks)]

pub mod abort;
pub mod backoff;

/// `Lsn` type implements common tasks on Log Sequence Numbers
pub mod lsn;
/// SeqWait allows waiting for a future sequence number to arrive
pub mod seqwait;

/// A simple Read-Copy-Update implementation.
pub mod simple_rcu;

/// append only ordered map implemented with a Vec
pub mod vec_map;

pub mod bin_ser;

// helper functions for creating and fsyncing
pub mod crashsafe;

// common authentication routines
pub mod auth;

// utility functions and helper traits for unified unique id generation/serialization etc.
pub mod id;

// utility functions to obtain reachable IP addresses in PS/SK nodes.
pub mod ip_address;

mod hex;
pub use hex::Hex;

// definition of the Generation type for pageserver attachment APIs
pub mod generation;

// common log initialisation routine
pub mod logging;

pub mod lock_file;
pub mod pid_file;

// Utility for binding TcpListeners with proper socket options.
pub mod tcp_listener;

// Default signal handling
pub mod signals;

pub mod fs_ext;

pub mod measured_stream;

pub mod serde_percent;
pub mod serde_regex;
pub mod serde_system_time;

pub mod pageserver_feedback;

pub mod postgres_client;

pub mod tracing_span_assert;

pub mod tokio_dump;

pub mod leaky_bucket;
pub mod rate_limit;

/// Simple once-barrier and a guard which keeps barrier awaiting.
pub mod completion;

/// Reporting utilities
pub mod error;

/// async timeout helper
pub mod timeout;

pub mod span;
pub mod sync;

pub mod failpoint_support;

pub mod yielding_loop;

pub mod zstd;

pub mod env;

pub mod poison;

pub mod toml_edit_ext;

pub mod circuit_breaker;

pub mod try_rcu;

pub mod guard_arc_swap;

pub mod elapsed_accum;

#[cfg(target_os = "linux")]
pub mod linux_socket_ioctl;

pub mod metrics_collector;

// Re-export used in macro. Avoids adding git-version as dep in target crates.
#[doc(hidden)]
pub use git_version;

/// This is a shortcut to embed git sha into binaries and avoid copying the same build script to all packages
///
/// we have several cases:
/// * building locally from git repo
/// * building in CI from git repo
/// * building in docker (either in CI or locally)
///
/// One thing to note is that .git is not available in docker (and it is bad to include it there).
/// When building locally, the `git_version` is used to query .git. When building on CI and docker,
/// we don't build the actual PR branch commits, but always a "phantom" would be merge commit to
/// the target branch -- the actual PR commit from which we build from is supplied as GIT_VERSION
/// environment variable.
///
/// We ended up with this compromise between phantom would be merge commits vs. pull request branch
/// heads due to old logs becoming more reliable (github could gc the phantom merge commit
/// anytime) in #4641.
///
/// To avoid running buildscript every recompilation, we use rerun-if-env-changed option.
/// So the build script will be run only when GIT_VERSION envvar has changed.
///
/// Why not to use buildscript to get git commit sha directly without procmacro from different crate?
/// Caching and workspaces complicates that. In case `utils` is not
/// recompiled due to caching then version may become outdated.
/// git_version crate handles that case by introducing a dependency on .git internals via include_bytes! macro,
/// so if we changed the index state git_version will pick that up and rerun the macro.
///
/// Note that with git_version prefix is `git:` and in case of git version from env its `git-env:`.
///
/// #############################################################################################
/// TODO this macro is not the way the library is intended to be used, see <https://github.com/neondatabase/neon/issues/1565> for details.
/// We used `cachepot` to reduce our current CI build times: <https://github.com/neondatabase/cloud/pull/1033#issuecomment-1100935036>
/// Yet, it seems to ignore the GIT_VERSION env variable, passed to Docker build, even with build.rs that contains
/// `println!("cargo:rerun-if-env-changed=GIT_VERSION");` code for cachepot cache invalidation.
/// The problem needs further investigation and regular `const` declaration instead of a macro.
#[macro_export]
macro_rules! project_git_version {
    ($const_identifier:ident) => {
        // this should try GIT_VERSION first only then git_version::git_version!
        const $const_identifier: &::core::primitive::str = {
            const __COMMIT_FROM_GIT: &::core::primitive::str = $crate::git_version::git_version! {
                prefix = "",
                fallback = "unknown",
                args = ["--abbrev=40", "--always", "--dirty=-modified"] // always use full sha
            };

            const __ARG: &[&::core::primitive::str; 2] = &match ::core::option_env!("GIT_VERSION") {
                ::core::option::Option::Some(x) => ["git-env:", x],
                ::core::option::Option::None => ["git:", __COMMIT_FROM_GIT],
            };

            $crate::__const_format::concatcp!(__ARG[0], __ARG[1])
        };
    };
}

/// This is a shortcut to embed build tag into binaries and avoid copying the same build script to all packages
#[macro_export]
macro_rules! project_build_tag {
    ($const_identifier:ident) => {
        const $const_identifier: &::core::primitive::str = {
            const __ARG: &[&::core::primitive::str; 2] = &match ::core::option_env!("BUILD_TAG") {
                ::core::option::Option::Some(x) => ["build_tag-env:", x],
                ::core::option::Option::None => ["build_tag:", ""],
            };

            $crate::__const_format::concatcp!(__ARG[0], __ARG[1])
        };
    };
}

/// Re-export for `project_git_version` macro
#[doc(hidden)]
pub use const_format as __const_format;

/// The product's semantic version, stamped at build time from the repo-root
/// `VERSION` file (hybrid CalVer, e.g. `0.260714.0`) — the same single source
/// of truth the Go controller reads. See this crate's `build.rs`. Shared by
/// every compute binary so they report the same version as the controller.
pub const AMP_VERSION: &str = env!("AMP_VERSION");

// The build's git commit, embedded via the same macro the binaries use. Its
// `.git` `include_bytes!` dependency makes `utils` recompile when the commit
// changes, so `amp_version()` below never goes stale through a build cache.
project_git_version!(GIT_COMMIT);

/// The full product version every compute binary reports: the `VERSION`-file
/// semver with the build's short git commit appended (`-dirty` for a modified
/// tree), e.g. `0.260714.0-9d2c3eea5`. Mirrors the Go controller's
/// `<version>-<commit>` stamp so both control-plane and compute agree.
pub fn amp_version() -> String {
    format_version(AMP_VERSION, GIT_COMMIT)
}

/// Combine the semver with the raw `project_git_version!` string. Kept separate
/// from [`amp_version`] (which feeds it the build-time consts) so the parsing is
/// unit-testable. `git_raw` is `"git-env:<40-char sha>"` for CI/Docker builds
/// that pass the `GIT_VERSION` env, or `"git:<sha>[-modified]"` for a local
/// `.git` build; the `-modified` suffix becomes `-dirty`.
fn format_version(semver: &str, git_raw: &str) -> String {
    let raw = git_raw
        .split_once(':')
        .map(|(_, sha)| sha)
        .unwrap_or(git_raw);
    let dirty = raw.ends_with("-modified") || raw.ends_with("-dirty");
    let sha = raw.trim_end_matches("-modified").trim_end_matches("-dirty");
    let short = &sha[..sha.len().min(9)];
    format!("{semver}-{short}{}", if dirty { "-dirty" } else { "" })
}

/// Same as `assert!`, but evaluated during compilation and gets optimized out in runtime.
#[macro_export]
macro_rules! const_assert {
    ($($args:tt)*) => {
        const _: () = assert!($($args)*);
    };
}

#[cfg(test)]
mod amp_version_tests {
    use super::format_version;

    #[test]
    fn ci_env_full_sha_is_shortened() {
        assert_eq!(
            format_version(
                "0.260714.0",
                "git-env:9d2c3eea5d3de8912d0cac9f66547b045f2b5eb5"
            ),
            "0.260714.0-9d2c3eea5"
        );
    }

    #[test]
    fn local_modified_tree_is_marked_dirty() {
        assert_eq!(
            format_version("0.260714.0", "git:9d2c3eea5d3de8912-modified"),
            "0.260714.0-9d2c3eea5-dirty"
        );
    }

    #[test]
    fn missing_git_falls_back_to_unknown() {
        assert_eq!(
            format_version("0.260714.0", "git:unknown"),
            "0.260714.0-unknown"
        );
    }

    #[test]
    fn handles_raw_sha_without_prefix() {
        assert_eq!(format_version("1.2.3", "abcdef1234567"), "1.2.3-abcdef123");
    }
}
