#!/usr/bin/env bash
set -euo pipefail

# If you save this in your path under the name "cargo-zclippy" (or whatever
# name you like), then you can run it as "cargo zclippy" from the shell prompt.
#
# If your text editor has rust-analyzer integration, you can also use this new
# command as a replacement for "cargo check" or "cargo clippy" and see clippy
# warnings and errors right in the editor.
# In vscode, this setting is Rust-analyzer>Check On Save:Command

# NB: the CI runs the full feature powerset, so, it catches slightly more errors
# at the expense of longer runtime. This script is used by developers, so, don't
# do that here.

# Clippy args (inlined from the former .neon_clippy_args):
# * `-A unknown_lints` – do not warn about unknown lint suppressions
#                        that people with newer toolchains might use
# * `-D warnings`      - fail on any warnings (`cargo` returns non-zero exit status)
# * `-D clippy::todo`  - don't let `todo!()` slip into `main`
CLIPPY_COMMON_ARGS="--locked --workspace --all-targets -- -A unknown_lints -D warnings -D clippy::todo"
exec cargo clippy --all-features $CLIPPY_COMMON_ARGS
