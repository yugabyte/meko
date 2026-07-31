# ac — Versioning

**Status:** Draft
**Last updated:** 2026-07-23

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
`<VERSION>-<short-commit>` (e.g. `0.260723.0-71fe562d7`) — dashes only, so it
drops straight into an image tag / URL. It stays SemVer-shaped, so the image tag
and sorting work normally.

## Single source of truth — git tags

Releases are **git tags** (`0.YYMMDD.PATCH`, e.g. `0.260723.0`), minted by CI —
there is **no version file to bump and no release commit on `main`**. The tag
sits on the exact commit the images were built from, so:

- the release list = the repo's tag list;
- `git checkout 0.260723.0` = the exact released source;
- the same-day `PATCH` counter = the existing `0.YYMMDD.*` tags (no other state).

The repo-root `VERSION` file is **not a version source** and is never bumped or
committed to. It survives only as the build-time delivery mechanism: CI stamps
the computed base into the *workspace* copy before building, and
[`scripts/version.sh`](../../scripts/version.sh) / the Makefile read it from
there, so the binary, image tag, Helm chart, and `controller` all report the
computed release version. (For local dev builds it is a placeholder base;
`version.sh` still appends `-<short-commit>[-dirty]` so any local build is
uniquely identifiable.)

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

## Releasing — automatic, tag-driven (Jenkins `amp-build`)

Every merge to `main` that changes **product code** mints a release, fully
automatically — no manual runs, no PRs, no GitHub permissions. The whole flow
lives in the `amp-build` Jenkins job
(`qa-services/amp-report-service/jenkins/pipelines/pipeline_amp_build.groovy`):

```
merge to main
  → pollSCM (≤15 min)
  → gate: product code changed since the last release tag?
      compute/**, controller/**, the postgres submodule, Cargo.lock,
      rust-toolchain.toml, Makefile, postgres.mk, build.sh, scripts/build.py
      — excluding *.md, *_test.go, tests/ dirs, testdata/
      (docs/test-only merges are a ~1-min no-op, no version minted)
  → version computed on the fly: 0.YYMMDD.PATCH
      YYMMDD = HEAD commit date · PATCH = next free among the 0.YYMMDD.* tags
  → build → push quay.io/yugabyte/amp-{controller,endpoint,pg}:<version>-<short-commit>
  → push the git tag 0.YYMMDD.PATCH onto the built commit
```

Properties:

- **The tag is pushed only after all images land** on quay — a failed build
  never burns a version; the tag and the images always exist as a pair (a re-run
  repairs a missing tag idempotently).
- **No double builds**: polling fires only on new commits (the tag push doesn't
  move the branch head), the gate diffs against the tag on HEAD, the quay probe
  skips already-published deterministic tags, and builds are serialized.
- **First release** under this scheme: `0.260723.0` → images
  `…:0.260723.0-71fe562d7` + tag `0.260723.0` on `71fe562d7`.
- Old-date hotfixes fit without redesign: branch from the tag and build that
  branch with an explicit version.
- Test-run builds are separate: `custom-<short-commit>`, deterministic per
  commit, deduped the same way.

## Target: the release status page

This pipeline is the **foundation for a release status page**: every release is
a stable, machine-readable triple — **git tag** (source), **quay images**
(artifacts), and, next, **test results** attached to that same version. The
status page's row is "release `0.260723.0` — images ✓ — regression ✓/✗ — perf
✓/✗", built from:

- **releases** — the repo's `0.*` tags (GitHub API) or quay tag list;
- **artifacts** — quay `amp-*:<version>-<short-commit>` manifests;
- **test status** — the amp report portal's runs for that version (the portal
  already records `versions.product` per run and groups report pages by it), and
  a GitHub commit status posted back onto the tagged commit so the status is
  visible on the GitHub side too.
