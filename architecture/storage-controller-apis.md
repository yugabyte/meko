# Storage controller HTTP API

Source: `storage_controller/src/http.rs` (router in `make_router`, one handler
fn per route), `storage_controller/src/main.rs` (listener wiring). See
[`storage-controller.md`](storage-controller.md) for what the controller is and
how it diverges from upstream.

## Binary, listen address, transport

The `storage_controller` binary serves a single HTTP/HTTPS API. Listen
addresses come from CLI flags, not from any config block:

- `--listen <host:port>` — plain HTTP listener (e.g. `127.0.0.1:1244`).
- `--listen-https <host:port>` — TLS listener (cert/key from `--ssl-cert-file` /
  `--ssl-key-file`, hot-reloaded on `--ssl-cert-reload-period`).
- At least one of the two is required; both may be set, serving the same router.

The pageserver/safekeeper/launcher point their `control_plane_api` (and the
`yb_light_local` wrapper its controller URL) at this address. There is no
separate "internal" port — every route below is on the one listener.

## Authentication

JWT bearer auth via `--public-key` (or `PUBLIC_KEY`). When a public key is
configured, `auth_middleware` validates every request except the allowlist
`/status`, `/live`, `/ready`, `/metrics`, `/profile/cpu`, `/profile/heap`.
Each handler then calls `check_permissions(req, scope)` for a required JWT
`Scope` (`Admin`, `Infra`, `PageServerApi`, `GenerationsApi`, `Scrubber`); a
token carrying `Admin` satisfies any scope. In `--dev` mode the public key may
be omitted, disabling incoming auth entirely.

Branch-scoped routes additionally pass through a per-branch token-bucket rate
limiter (`--tenant-rate-limit`, default 10 req/s/branch); admin/debug routes are
exempt.

## API families

Routes group into: generic/observability (unprefixed), `/upcall/v1/...`
(control-plane upcall API), `/debug/v1/...` (test/dev/debug), `/control/v1/...`
(node / safekeeper / endpoint / branch-describe / branch-mapping control), and
`/v1/...` (the "virtual pageserver" branch data-plane shim). Path
params are `:branch_id`, `:node_id`, `:endpoint_id`, `:id`
(safekeeper instance id) — note `:branch_id` was collapsed from the former
`(:tenant_id, :timeline_id)` pair (originally the upstream `:tenant_shard_id`).

### Generic / observability (no auth, no prefix)

| Method | Path | Purpose |
|---|---|---|
| GET | `/metrics` | Prometheus exposition (see [`storage-controller-metrics.md`](storage-controller-metrics.md)) |
| GET | `/status` | Liveness of the HTTP listener (k8s startup probe); always 200 |
| GET | `/live` | 200 once startup is complete, else 503 (k8s liveness probe) |
| GET | `/ready` | 200 once startup I/O / reconcile is done, else 503 (k8s readiness probe) |
| GET | `/profile/cpu` | CPU profile (pprof) |
| GET | `/profile/heap` | jemalloc heap profile |

### Upcall API (`/upcall/v1`) — control-plane upcalls

Pageserver-facing upcalls. **yb-light note:** these handlers still exist
server-side but the yb pageserver no longer calls re-attach / validate (its
`controller_upcall_client` is deleted); generations arrive via the launcher's
attach-hook instead. Scope: `GenerationsApi`.

| Method | Path | Purpose |
|---|---|---|
| POST | `/upcall/v1/re-attach` | PS startup: learn which branches to attach (`ReAttachRequest`) |
| POST | `/upcall/v1/validate` | PS pre-deletion: confirm it still holds the latest generation (`ValidateRequest`) |
| GET | `/upcall/v1/timeline_import_status` | Read timeline-import progress (`TimelineImportStatusRequest`) |
| POST | `/upcall/v1/timeline_import_status` | Report timeline-import progress (`PutTimelineImportStatusRequest`) |

### Debug / test API (`/debug/v1`)

Scope: `Admin` unless noted. Includes the attach-hook the launcher uses to bump
generations.

| Method | Path | Purpose |
|---|---|---|
| POST | `/debug/v1/attach-hook` | Acquire/bump a generation for a branch before attach (`AttachHookRequest`); used by the launcher |
| POST | `/debug/v1/inspect` | Inspect a branch's attach state (`InspectRequest`) |
| POST | `/debug/v1/branch/:branch_id/drop` | Forget a branch in-memory + in DB (scope `PageServerApi`) |
| POST | `/debug/v1/node/:node_id/drop` | Forget a node |
| GET | `/debug/v1/tombstone` | List node tombstones |
| DELETE | `/debug/v1/tombstone/:node_id` | Delete a node tombstone |
| POST | `/debug/v1/branch/:branch_id/import` | Import an existing branch from remote storage (scope `PageServerApi`) |
| POST | `/debug/v1/branch/:branch_id/import_timeline` | Import a branch (`TimelineImportRequest`; branch_id must match URL) (scope `PageServerApi`) |
| GET | `/debug/v1/branch` | Dump full in-memory branch state |
| GET | `/debug/v1/branch/:branch_id/locate` | Resolve a branch to its node(s) (debug-only `tenant_locate`) |
| GET | `/debug/v1/scheduler` | Dump scheduler state |
| POST | `/debug/v1/consistency_check` | Cross-check in-memory vs DB state |
| PUT | `/debug/v1/failpoints` | Configure failpoints (request-span only; no scope check) |

### Node operations (`/control/v1/node`)

Scope: `Infra` for register/list/status, `Admin` for config/shards.

| Method | Path | Purpose |
|---|---|---|
| POST | `/control/v1/node` | Register a pageserver node (`NodeRegisterRequest`) |
| GET | `/control/v1/node` | List nodes (sorted by id) |
| GET | `/control/v1/node/:node_id` | Describe one node |
| PUT | `/control/v1/node/:node_id/config` | Set availability / scheduling policy (`NodeConfigureRequest`; body node_id must match URL) |
| GET | `/control/v1/node/:node_id/shards` | List shard locations on a node |

### Compute-endpoint registry (`/control/v1/endpoint`)

Scope: `Admin`. Used by `yb_light_local` in place of the old per-endpoint
folder layout; the controller persists/returns records and never interprets the
endpoint `spec`.

| Method | Path | Purpose |
|---|---|---|
| POST | `/control/v1/endpoint` | Create/persist a compute-endpoint record (`ComputeEndpoint`) |
| GET | `/control/v1/endpoint` | List endpoint records |
| DELETE | `/control/v1/endpoint/:endpoint_id` | Delete an endpoint record |

### Safekeeper operations

None. In yb-light's co-located topology each branch's endpoint provisions and
owns its own single safekeeper, and the launcher creates the timeline on it
directly — the controller never contacts a safekeeper. The global safekeeper
registry (the `storage_controller.safekeepers` table, its register/list/
scheduling/heartbeat machinery, and the multi-SK `sk_set`/quorum code) was
removed in #171; the controller only records branch rows.

### Metadata health (`/control/v1/metadata_health`)

Scope: `Scrubber` for update, `Admin` for the lists.

| Method | Path | Purpose |
|---|---|---|
| POST | `/control/v1/metadata_health/update` | Record scrub results (`MetadataHealthUpdateRequest`) |
| GET | `/control/v1/metadata_health/unhealthy` | List unhealthy branches |
| POST | `/control/v1/metadata_health/outdated` | List branches not scrubbed within a window (`MetadataHealthListOutdatedRequest`) |

### Branch control (`/control/v1/branch`)

`describe`/`list` (scope `Scrubber`/`Admin`) wait for startup reconcile before
running; `policy` is scope `Admin`.

| Method | Path | Purpose |
|---|---|---|
| GET | `/control/v1/branch` | List branches (scope `Admin`) |
| GET | `/control/v1/branch/:branch_id` | Describe one branch (scope `Scrubber`) |
| PUT | `/control/v1/branch/:branch_id/policy` | Update branch placement/scheduling policy (`BranchPolicyRequest`) |

### Timeline mappings & pending inits (`/control/v1/...`)

Branch-name mapping and the pending-timeline-init recipe registry. Scope:
`Admin`. These back the `branch_name` / `pending_init` flows described in
[`storage-controller.md`](storage-controller.md).

| Method | Path | Purpose |
|---|---|---|
| GET | `/control/v1/branch/:branch_id/timeline` | Describe a branch (scope `Scrubber`) |
| PUT | `/control/v1/branch/:branch_id/branch_name` | Set a branch's branch name (`SetTimelineBranchNameRequest`) |
| POST | `/control/v1/branch/:branch_id/register` | Register a branch (start_lsn, optional `branch_name` set in the same write — folds the separate branch_name call) (`TimelineRegisterRequest`) |
| PUT | `/control/v1/branch/:branch_id/drop` | Drop a branch + all its endpoints; mark branch deleted; idempotent |
| GET | `/control/v1/timeline_branch_names` | List live (non-deleted) branch-name mappings |
| POST | `/control/v1/branch/:branch_id/pending_init` | Record a pending branch-init recipe (`PendingTimelineInitRequest`) |
| DELETE | `/control/v1/branch/:branch_id/pending_init` | Delete a pending-init recipe |
| GET | `/control/v1/pending_timeline_inits` | List all pending inits (cluster-wide) |

### Virtual-pageserver branch data plane (`/v1`)

The `/v1/...` routes present the cluster to shard-naive clients as a single
"virtual pageserver", proxying to the node holding the branch. All wait for
startup reconcile and are rate-limited per branch; scope `PageServerApi` (or
`Admin`).

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/branch` | Create a branch (`BranchCreateRequest`); 201 |
| DELETE | `/v1/branch/:branch_id` | Delete a branch (retry-wrapped 202→404→200 semantics) |
| PUT | `/v1/branch/:branch_id/time_travel_remote_storage` | Time-travel remote storage (`travel_to` / `done_if_after` query params) |
| POST | `/v1/branch/:branch_id/timeline` | Create a branch (`TimelineCreateRequest`); 201 |
| DELETE | `/v1/branch/:branch_id/timeline` | Delete a branch (retry-wrapped deletion) |
| PUT | `/v1/branch/:branch_id/archival_config` | Set branch archival config (`TimelineArchivalConfigRequest`) |
| PUT | `/v1/branch/:branch_id/detach_ancestor` | Detach ancestor (`detach_behavior` query param) |
| POST | `/v1/branch/:branch_id/block_gc` | Block GC on a branch |
| POST | `/v1/branch/:branch_id/unblock_gc` | Unblock GC on a branch |
| POST | `/v1/branch/:branch_id/lsn_lease` | Acquire an LSN lease (`LsnLeaseRequest`) |
| PUT | `/v1/branch/:branch_id/mark_invisible` | Passthrough to shard zero |
| GET | `/v1/branch/:branch_id` | Passthrough: branch detail from shard zero |
| GET | `/v1/branch/:branch_id/*` | Wildcard passthrough: any branch GET on the pageserver (must be last route) |

## yb-light specifics / removed-vs-upstream

- **Single-branch / launcher-managed scheduling.** Attachment is decided at
  pageserver spawn time by the launcher (identity + generation baked into
  `pageserver.toml`); the controller does not push attachments. The scheduler
  survives only to pick the intent node at branch create — hence
  `/debug/v1/scheduler` is a dump-only endpoint and `tenant_locate` is debug-only.
- **No shard split / migrate / drain / fill / leadership routes.** Verified
  absent from `make_router`: there is no `/control/v1/branch/:branch_id/shard_split`,
  no `/control/v1/branch/:branch_id/migrate`, no node `drain`/`fill`/`cancel`, and no
  leadership/step-down/`/control/v1/step_down` route. These were removed with
  the pageserver reconciler (~8.4K lines, #16). Node lifecycle is limited to
  register / list / describe / config / drop / tombstone.
- **No safekeeper surface at all.** The global safekeeper registry + the
  multi-SK membership/quorum/`sk_set` machinery were removed (#171): each
  branch's endpoint owns its single co-located safekeeper and the launcher
  creates the timeline on it directly, so the controller never contacts a
  safekeeper.
- **Upcall handlers are vestigial.** `/upcall/v1/re-attach` and
  `/upcall/v1/validate` are still routed but unused by the yb pageserver.
