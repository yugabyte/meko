---
status: done
severity: high
area: yb_light_local
opened: 2026-06-11
---

# Bundled runtime libs are invisible to the walredo child

## Symptom

On a freshly deployed bundle, every walredo launch fails at exec time:

```
.../builds/2026-06-09-dde78d810/pg_install/v17/bin/postgres:
error while loading shared libraries: libicui18n.so.60:
cannot open shared object file: No such file or directory
```

PS reads this as a redo failure (`write to walredo stdin: Broken pipe` /
`read walredo stdout: early eof`) and fail-stops with
`ABORT: MATERIALIZER REDO ERROR`. Burned a day of the task-28 corruption
hunt on 2026-06-11: five SIGABRTs (04:22–07:20) that looked like
corruption repros were all this
([see 28](28_walredo-corruption-catch-me-if-you-can.md), Update
2026-06-11).

## Root cause

`scripts/yb-light-ship.sh` ships the runtime libs missing on stock
AlmaLinux/RHEL 9 (OpenSSL 1.1, ICU 60) into the bundle's **top-level
`lib/`** only. That works for the daemons — `yb_light_local` launches
them with `LD_LIBRARY_PATH` via the child-env allowlist
(`control_plane/src/background_process.rs`) — but the walredo postgres
child is spawned by the PS with `env_clear()` and
`LD_LIBRARY_PATH=<pg_lib_dir>` only
(`pageserver/src/walredo/process.rs:79`), i.e. `pg_install/v17/lib/`,
where the bundled libs are not. The PS daemon runs fine; its walredo
child can't load.

It goes unnoticed on hosts where an older full build or system packages
happen to provide the libs; it bites on the first walredo launch from a
clean bundle (manual `pg_install/v17/lib` symlinks were the 2026-06-11
stopgap on the repro box).

## Fix sketch

In `yb-light-ship.sh`, stage the same five libs into
`pg_install/v17/lib/` as well (copy, not symlink — rsync `-avzh`
without `--links` semantics across hosts is friendlier to plain files),
right after the existing top-level `lib/` staging loop. Keep the
top-level copies for the daemons.

Alternative considered: make `walredo/process.rs` append the parent's
`LD_LIBRARY_PATH` instead of `env_clear()`-then-pg_lib_dir. Rejected
for now — bigger upstream divergence, and a scrubbed walredo env is a
deliberate part of the seccomp/sandbox posture.

## Acceptance

- Fresh bundle on a host with no system OpenSSL 1.1 / ICU 60: walredo
  postgres launches (e.g. `LD_LIBRARY_PATH=<bundle>/pg_install/v17/lib
  ldd <bundle>/pg_install/v17/bin/postgres` shows no `not found`).
- Task-28 repro recipe runs past the first materializer wave without
  the libicu ABORT.

## Resolution

`yb-light-ship.sh` now stages the five runtime libs into
`pg_install/v17/lib/` in addition to the top-level `lib/`. The lib list
is a single `RUNTIME_LIBS` array so the two staging sites can't drift.
The pg_install copy runs right after the `rsync -a pg_install/v17`
staging, so the target dir is guaranteed to exist.

Verified locally: `cp` dereferences the `/usr/lib64/*.so.*` symlinks
into self-contained regular files, and their names match the
`postgres` binary's `NEEDED` entries exactly (`libssl.so.1.1`,
`libcrypto.so.1.1`, `libicui18n.so.60`, `libicuuc.so.60`, with
`libicudata.so.60` pulled in transitively by the ICU libs). The
clean-host `ldd`/task-28 acceptance is confirmed on the next bundle
deploy — not shipped from here per the no-auto-ship rule.

## Notes / links

- memory/bundle-runtime-libs.md — the original daemon-side variant of
  the same gotcha.
- Evidence log: `ps-stack.log.crash-20260611-072330-rel16414-blk4135568.gz`
  on the repro box.
