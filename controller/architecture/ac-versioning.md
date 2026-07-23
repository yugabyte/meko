# ac — Versioning

**Status:** Draft
**Last updated:** 2026-07-22

How the amp controller bundle is versioned: one version, from a single source,
reported by every component (`ac-apis.md`, `ac-k8s-deploy.md`). Covers both the
Go controller and the bundled Rust `controller`.

## Version format

The version is a **hybrid CalVer/SemVer**: `MAJOR.YYMMDD.PATCH` (e.g.
`0.260714.0`).

- **`MAJOR`** — kept at `0` for now; bumped only for a deliberate epoch change.
- **`YYMMDD`** — the release date (the CalVer component). It occupies the SemVer
  *minor* slot, so the string is still a valid three-part SemVer that
  parses/sorts normally.
- **`PATCH`** — a same-day counter: `0` for the day's first release, `+1` for
  each re-cut on that date (`0.260722.0` → `0.260722.1` for a second release the
  same day).

Every **build** appends the short commit: the CI image/binary version is
`<VERSION>-<short-commit>` (e.g. `0.260722.0-1bc8b3d`) — dashes only, so it drops
straight into an image tag / URL. The `VERSION` file holds **only the base**
`MAJOR.YYMMDD.PATCH`; the commit id lives on the build, not in the file. It stays
SemVer-shaped, so the image tag and sorting work normally.

## Single source of truth

The repo-root **`VERSION`** file (e.g. `0.260714.0`) is the only place the
release number lives. [`scripts/version.sh`](../../scripts/version.sh) reads it
and derives every other form; the Makefile and deploy scripts call it, so the
binary, image tag, Helm chart, and `controller` never drift.

```
scripts/version.sh                 # 0.260722.0            (base version from VERSION)
scripts/version.sh binary-version  # 0.260722.0-1bc8b3d    (-ldflags value)
scripts/version.sh image-tag       # 0.260722.0-1bc8b3d   (container tag)
scripts/version.sh sync            # stamp Chart.yaml + controller Cargo.toml/Cargo.lock
scripts/version.sh bump date       # 0.260714.0 -> 0.260722.0 (today), then sync
```

## Build identity

`binary-version` / `image-tag` always append `-<short-commit>`, so every build
is uniquely identifiable in `/version`, logs, and image tags and traces straight
to its exact commit — `0.260722.0-1bc8b3d`. A `-dirty` marker is added for an
unclean **local** tree; `RELEASE=1` asserts a clean build and omits it (CI trees
are clean, so it is a no-op there — the commit id is always present). The suffix
is Docker-tag-safe (`.`/`-`, no `+`).

## Where the version shows up

- **Controller binary** — `controller/internal/version` (`Version`, `Commit`,
  `BuildDate`), stamped via `-ldflags -X` from the Makefile (`controller-bundle{,-only}`,
  and the plain `controller` dev target). Any out-of-Makefile build (e.g. a manual
  cross-compile) should reuse `make print-controller-ldflags` so it stamps from the
  same source. Logged at startup (`amp controller starting`).
- **REST API** — `GET /api/v1/version` returns
  `{version, git_commit, build_date, controller?}`. The
  `controller` field is the bundled binary's `--version`, captured once
  at startup (empty when no managed controller runs).
- **UI** — the `AppLayout` footer shows `amp v<version>`; its tooltip carries the
  commit, build date, and controller version.
- **Container image** — built and pushed as `<VERSION>-<short-commit>` (e.g.
  `0.260722.0-1bc8b3d`). Deploy scripts default the controller/endpoint tags to
  `scripts/version.sh image-tag`.
- **Helm chart** — `Chart.yaml` `version`/`appVersion` are stamped from `VERSION`
  by `version.sh sync`.

## controller

The `controller` is a vendored, Neon-derived component with its own
git-based versioning (`project_git_version!`/`project_build_tag!`), which reads
the `GIT_VERSION`/`BUILD_TAG` env vars at compile time. The bundle build stamps
these from the same `VERSION` file (Makefile: `BUILD_TAG=v<VERSION>`,
`GIT_VERSION=<commit>`), and its `--version` (a `CLI_VERSION` const) prefers the
stamped `BUILD_TAG` — so `controller --version` reports the same release
identity as the controller, and that string is what the controller surfaces in
the `controller` field of the `GET /api/v1/version` response.

## Compute / endpoint binaries

The compute image's Rust binaries — `yb-pageserver`, `yb-safekeeper`,
`yb-compute-ctl`, `yb-endpoint-storage` — report the **same** version from the
same `VERSION` file, so the endpoint image and the controller never disagree. A
`build.rs` in the shared `utils` crate stamps the `VERSION` semver into the
`AMP_VERSION` compile-time constant (via `cargo:rustc-env` +
`cargo:rerun-if-changed`, so it can't go stale through the endpoint image's
BuildKit `target` cache mount), and `utils::amp_version()` combines it with the
build's git commit into `<VERSION>-<short-commit>` (e.g. `0.260722.0-1bc8b3d`,
with `-dirty` for a modified tree) — the same shape as the controller stamp
above. Every binary logs it at startup; `yb-pageserver`/`yb-safekeeper` also
surface it through `--version`.

The commit still comes from the `GIT_VERSION` env (passed by the image build)
via `project_git_version!`; the operational `BUILD_TAG` (the image tag, used for
the extension-download S3 paths and the `build_tag` metric label) is unchanged.
Because these binaries are Neon-derived, this is the same
`project_git_version!`/`project_build_tag!` machinery the `controller` uses — the
`utils` `build.rs` just adds the `VERSION`-file semver on top.

## Releasing — human-triggered, CI-performed

A version is minted **only when someone decides to release** — not on every
merge. Most days that is zero times, some days more than once. The bump is done
by CI, never by hand.

Trigger the **`Release — bump VERSION`** workflow
(`.github/workflows/version-bump.yml`, `workflow_dispatch`) with a bump part:

- **`date`** (default) — the normal release: rolls `YYMMDD` to today and resets
  `PATCH` to `0`. If `VERSION` is already dated today, it rolls `PATCH` instead
  (`0.260722.0` → `0.260722.1`), so a second release the same day moves forward.
- **`patch`** — re-cut the current date (a hotfix on an existing release).
- **`major`** — bump the epoch; reserved for a deliberate reset.

The workflow runs `scripts/version.sh bump <part>` (which also syncs the chart +
`controller` Cargo.toml/Cargo.lock; `make version-bump-*` are thin wrappers) on a
`release/<version>` branch, **opens a PR to `main`, and enables auto-merge** — so
main's "require a pull request" rule is honoured with no branch-protection bypass
(needs "Allow auto-merge" enabled). **The merge commit is the release commit.**
The `amp-build` job (SCM polling on `main`) builds it and pushes
`<VERSION>-<short-commit>` to quay — the Makefile stamps the commit, so the
released tag identifies the exact code. (amp-build should build the exact commit
it triggered on, not a moving `main` head.)

Between releases nothing new is published; a local/dev build off the current
`VERSION` is still `<VERSION>-<short-commit>[-dirty]`, uniquely identifiable.
