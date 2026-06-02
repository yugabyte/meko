---
status: open
severity: medium
area: storage_controller / neon_local
opened: 2026-06-02
---

# Phase 4k — `neon_local start` wedge on fresh init

## Symptom

After a `neon_local init` + `start` against a freshly-created
`storage_controller` database, the very next API call from the
pageserver to storcon fails with:

```
pageserver API: Conflict: 1 (localhost) 500 Internal Server Error:
500 Internal Server Error tenant is broken:
db error: ERROR: relation "storage_controller.timelines" does not exist
```

The default `--start-timeout 10s` triggers cleanup before
storage_controller's diesel migrations finish on a fresh DB. Bumping
to `--start-timeout 120s` (or higher) papers over it; sleeping ~30 s
after `start` before the first `tenant create` also works.

## Why it happens

`neon_local start` brings up storcon's embedded Postgres, then storcon
itself, then the pageserver. Pageserver registration with storcon
appears to fire before storcon's migrations have finished against a
just-created DB. The pageserver looks healthy by neon_local's standards
("port is open") but its first operation against storcon hits a
schema that doesn't exist yet.

Storage_controller migrations against a freshly-dropped YB-style
database take ~60 s in practice; neon_local's health check doesn't
account for that.

## Repro

Clean slate:

```bash
export NEON_REPO_DIR=$PWD/.neon-localfs
rm -rf "$NEON_REPO_DIR"
target/release/neon_local init --config /tmp/neon-init.toml
target/release/neon_local start --start-timeout 30s     # default-ish
target/release/neon_local tenant create --tenant-id <fresh>   # ← fails
```

`start-timeout 240s` + `sleep 30` between `start` and `tenant create`
reliably unblocks; that's the current workaround.

## Fix shapes (pick one)

- **(a)** Bump `neon_local`'s default `--start-timeout` to ~120 s.
  Smallest change. Hides the underlying race but unblocks new-comers.
- **(b)** Drop the cross-service health-check coupling so
  `storage_controller` can finish migrating in parallel with
  pageserver registration retries.
- **(c)** Pre-create the storage_controller schema during
  `neon_local init` so first-`start` avoids the migration burst
  entirely.

(b) or (c) are the proper fixes; (a) is a sticky-plaster.

## Notes

- This is tracked as **Phase 4k** in `architecture/yb-pageserver.md`'s
  status table.
- Hit again 2026-06-02 during the corruption-repro local-cluster
  bring-up.
- The standard workaround is `--start-timeout 240s` + a
  `sleep 30` (or `until tenant create succeeds` poll loop)
  between `start` and the first `tenant create`.
