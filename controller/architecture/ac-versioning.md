# ac — Versioning

**Status:** Draft
**Last updated:** 2026-07-14

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
  each re-cut on that date.

Because it stays SemVer-shaped, the `-dev.<commit>` pre-release suffix and the
`v<...>` image tag work exactly as before.

## Single source of truth

The repo-root **`VERSION`** file (e.g. `0.260714.0`) is the only place the
release number lives. [`scripts/version.sh`](../../scripts/version.sh) reads it
and derives every other form; the Makefile and deploy scripts call it, so the
binary, image tag, Helm chart, and `controller` never drift.

```
scripts/version.sh                 # 0.260714.0            (release version)
scripts/version.sh binary-version  # 0.260714.0-dev.abc1234-dirty  (-ldflags value)
scripts/version.sh image-tag       # v0.260714.0-dev.abc1234-dirty (container tag)
scripts/version.sh sync            # stamp Chart.yaml + controller Cargo.toml/Cargo.lock
scripts/version.sh bump patch      # 0.260714.0 -> 0.260714.1, then sync
```

## Dev vs release

A build is a **pre-release by default**: `binary-version` / `image-tag` append
`-dev.<short-commit>` (plus `-dirty` when the working tree is unclean), so dev
builds are uniquely identifiable in `/version`, logs, and image tags, and never
clobber a release on the same `v0.260714.0` tag. **`RELEASE=1`** yields the clean
release version. The suffix is a SemVer-2.0 pre-release (sorts below the
release) and is Docker-tag-safe (`.`/`-`, no `+`).

## Where the version shows up

- **Controller binary** — `controller/internal/version` (`Version`, `Commit`,
  `BuildDate`), stamped via `-ldflags -X` from the Makefile
  (`controller-bundle{,-only}`). Logged at startup (`amp controller starting`).
- **REST API** — `GET /api/v1/version` returns
  `{version, git_commit, build_date, controller?}`. The
  `controller` field is the bundled binary's `--version`, captured once
  at startup (empty when no managed controller runs).
- **UI** — the `AppLayout` footer shows `amp v<version>`; its tooltip carries the
  commit, build date, and controller version.
- **Container image** — built and pushed as `v<VERSION>` (release) or
  `v<VERSION>-dev.<commit>` (dev). Deploy scripts default the controller/endpoint
  tags to `scripts/version.sh image-tag`.
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

## Releasing

1. `scripts/version.sh bump <part>` — bumps `VERSION` and syncs the chart +
   `controller` Cargo.toml/Cargo.lock (the `make version-bump-*` targets
   are thin wrappers). Under the CalVer scheme:
   - **`date`** (alias **`minor`**, `make version-bump-minor`) — the normal way
     to cut a release: rolls `YYMMDD` to today and resets `PATCH` to `0`. If
     `VERSION` is already dated today, it rolls `PATCH` instead, so the version
     always moves forward.
   - **`patch`** (`make version-bump-patch`) — re-cut the current date (a hotfix
     on top of an existing release).
   - **`major`** (`make version-bump-major`) — bump the epoch; re-stamps the
     date and resets `PATCH`. Reserved for a deliberate reset.
2. Commit the bump.
3. Build/push images with `RELEASE=1` so they tag `v<VERSION>` (clean, no `-dev`).

Between releases, every build is a `-dev.<commit>` pre-release off the current
`VERSION`.
