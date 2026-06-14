# Storage controller HTTP API

Source: `storage_controller/src/http.rs` (router in `make_router`, one handler
fn per route), `storage_controller/src/main.rs` (listener wiring). See
[`storage-controller.md`](storage-controller.md) for what the controller is and
how it diverges from upstream Neon.

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

Tenant-scoped routes additionally pass through a per-tenant token-bucket rate
limiter (`--tenant-rate-limit`, default 10 req/s/tenant); admin/debug routes are
exempt.

## API families

Routes group into: generic/observability (unprefixed), `/upcall/v1/...`
(control-plane upcall API), `/debug/v1/...` (test/dev/debug), `/control/v1/...`
(node / safekeeper / endpoint / tenant-describe / timeline-mapping control), and
`/v1/...` (the "virtual pageserver" tenant/timeline data-plane shim). Path
params are `:tenant_id`, `:timeline_id`, `:node_id`, `:endpoint_id`, `:id`
(safekeeper instance id) — note `:tenant_id`/`:timeline_id` were recently
renamed from the upstream `:tenant_shard_id`.

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
| POST | `/upcall/v1/re-attach` | PS startup: learn which tenants to attach (`ReAttachRequest`) |
| POST | `/upcall/v1/validate` | PS pre-deletion: confirm it still holds the latest generation (`ValidateRequest`) |
| GET | `/upcall/v1/timeline_import_status` | Read timeline-import progress (`TimelineImportStatusRequest`) |
| POST | `/upcall/v1/timeline_import_status` | Report timeline-import progress (`PutTimelineImportStatusRequest`) |

### Debug / test API (`/debug/v1`)

Scope: `Admin` unless noted. Includes the attach-hook the launcher uses to bump
generations.

| Method | Path | Purpose |
|---|---|---|
| POST | `/debug/v1/attach-hook` | Acquire/bump a generation for a tenant before attach (`AttachHookRequest`); used by the launcher |
| POST | `/debug/v1/inspect` | Inspect a tenant's attach state (`InspectRequest`) |
| POST | `/debug/v1/tenant/:tenant_id/drop` | Forget a tenant in-memory + in DB (scope `PageServerApi`) |
| POST | `/debug/v1/node/:node_id/drop` | Forget a node |
| GET | `/debug/v1/tombstone` | List node tombstones |
| DELETE | `/debug/v1/tombstone/:node_id` | Delete a node tombstone |
| POST | `/debug/v1/tenant/:tenant_id/import` | Import an existing tenant from remote storage (scope `PageServerApi`) |
| POST | `/debug/v1/tenant/:tenant_id/timeline/:timeline_id/import` | Import a timeline (`TimelineImportRequest`; ids must match URL) (scope `PageServerApi`) |
| GET | `/debug/v1/tenant` | Dump full in-memory tenant state |
| GET | `/debug/v1/tenant/:tenant_id/locate` | Resolve a tenant to its node(s) (debug-only `tenant_locate`) |
| GET | `/debug/v1/tenant/:tenant_id/timeline/:timeline_id/locate` | Resolve a tenant timeline to node(s) |
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

### Safekeeper operations (`/control/v1/safekeeper`)

Scope: `Infra` for list/get, `Admin` for upsert/scheduling.

| Method | Path | Purpose |
|---|---|---|
| GET | `/control/v1/safekeeper` | List safekeepers |
| GET | `/control/v1/safekeeper/:id` | Get one safekeeper by instance id (404 if unknown; manual-testing only) |
| POST | `/control/v1/safekeeper/:id` | Upsert a safekeeper (`SafekeeperUpsert`; id in URL must equal body, >0); 204 on success. Used by deploy scripts |
| POST | `/control/v1/safekeeper/:id/scheduling_policy` | Set a safekeeper's scheduling policy (`SafekeeperSchedulingPolicyRequest`) |

### Metadata health (`/control/v1/metadata_health`)

Scope: `Scrubber` for update, `Admin` for the lists.

| Method | Path | Purpose |
|---|---|---|
| POST | `/control/v1/metadata_health/update` | Record scrub results (`MetadataHealthUpdateRequest`) |
| GET | `/control/v1/metadata_health/unhealthy` | List unhealthy tenants |
| POST | `/control/v1/metadata_health/outdated` | List tenants not scrubbed within a window (`MetadataHealthListOutdatedRequest`) |

### Tenant control (`/control/v1/tenant`)

`describe`/`list` (scope `Scrubber`/`Admin`) wait for startup reconcile before
running; `policy` is scope `Admin`.

| Method | Path | Purpose |
|---|---|---|
| GET | `/control/v1/tenant` | List tenants (scope `Admin`) |
| GET | `/control/v1/tenant/:tenant_id` | Describe one tenant (scope `Scrubber`) |
| PUT | `/control/v1/tenant/:tenant_id/policy` | Update tenant placement/scheduling policy (`TenantPolicyRequest`) |

### Timeline mappings & pending inits (`/control/v1/...`)

Branch-name mapping and the pending-timeline-init recipe registry. Scope:
`Admin`. These back the `branch_name` / `pending_init` flows described in
[`storage-controller.md`](storage-controller.md).

| Method | Path | Purpose |
|---|---|---|
| GET | `/control/v1/tenant/:tenant_id/timeline/:timeline_id` | Describe a timeline (scope `Scrubber`) |
| PUT | `/control/v1/tenant/:tenant_id/timeline/:timeline_id/branch_name` | Set a timeline's branch name (`SetTimelineBranchNameRequest`) |
| POST | `/control/v1/tenant/:tenant_id/timeline/:timeline_id/register` | Register a timeline (start_lsn + sk_set) (`TimelineRegisterRequest`) |
| PUT | `/control/v1/tenant/:tenant_id/timeline/:timeline_id/drop` | Drop a branch + all its endpoints; mark timeline deleted; idempotent |
| GET | `/control/v1/timeline_branch_names` | List live (non-deleted) branch-name mappings |
| POST | `/control/v1/tenant/:tenant_id/timeline/:timeline_id/pending_init` | Record a pending timeline-init recipe (`PendingTimelineInitRequest`) |
| DELETE | `/control/v1/tenant/:tenant_id/timeline/:timeline_id/pending_init` | Delete a pending-init recipe |
| GET | `/control/v1/tenant/:tenant_id/pending_timeline_inits` | List pending inits for a tenant |

### Virtual-pageserver tenant/timeline data plane (`/v1`)

The `/v1/...` routes present the cluster to shard-naive clients as a single
"virtual pageserver", proxying to the node holding the tenant. All wait for
startup reconcile and are rate-limited per tenant; scope `PageServerApi` (or
`Admin`).

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/tenant` | Create a tenant (`TenantCreateRequest`); 201 |
| DELETE | `/v1/tenant/:tenant_id` | Delete a tenant (retry-wrapped 202→404→200 semantics) |
| PUT | `/v1/tenant/:tenant_id/time_travel_remote_storage` | Time-travel remote storage (`travel_to` / `done_if_after` query params) |
| POST | `/v1/tenant/:tenant_id/timeline` | Create a timeline (`TimelineCreateRequest`); 201 |
| DELETE | `/v1/tenant/:tenant_id/timeline/:timeline_id` | Delete a timeline (retry-wrapped deletion) |
| PUT | `/v1/tenant/:tenant_id/timeline/:timeline_id/archival_config` | Set timeline archival config (`TimelineArchivalConfigRequest`) |
| PUT | `/v1/tenant/:tenant_id/timeline/:timeline_id/detach_ancestor` | Detach ancestor (`detach_behavior` query param) |
| POST | `/v1/tenant/:tenant_id/timeline/:timeline_id/block_gc` | Block GC on a timeline |
| POST | `/v1/tenant/:tenant_id/timeline/:timeline_id/unblock_gc` | Unblock GC on a timeline |
| POST | `/v1/tenant/:tenant_id/timeline/:timeline_id/lsn_lease` | Acquire an LSN lease (`LsnLeaseRequest`) |
| PUT | `/v1/tenant/:tenant_id/timeline/:timeline_id/mark_invisible` | Passthrough to shard zero |
| GET | `/v1/tenant/:tenant_id` | Passthrough: tenant detail from shard zero |
| GET | `/v1/tenant/:tenant_id/*` | Wildcard passthrough: any tenant/timeline GET on the pageserver (must be last route) |

## yb-light specifics / removed-vs-upstream

- **Single-tenant / launcher-managed scheduling.** Attachment is decided at
  pageserver spawn time by the launcher (identity + generation baked into
  `pageserver.toml`); the controller does not push attachments. The scheduler
  survives only to pick the intent node at tenant create — hence
  `/debug/v1/scheduler` is a dump-only endpoint and `tenant_locate` is debug-only.
- **No shard split / migrate / drain / fill / leadership routes.** Verified
  absent from `make_router`: there is no `/control/v1/tenant/:id/shard_split`,
  no `/control/v1/tenant/:id/migrate`, no node `drain`/`fill`/`cancel`, and no
  leadership/step-down/`/control/v1/step_down` route. These were removed with
  the pageserver reconciler (~8.4K lines, #16). Node lifecycle is limited to
  register / list / describe / config / drop / tombstone.
- **No safekeeper membership-switch surface.** The multi-SK membership-change
  endpoints were stripped (commit `1a9ca7ec1`); only list/get/upsert/
  scheduling-policy remain.
- **Upcall handlers are vestigial.** `/upcall/v1/re-attach` and
  `/upcall/v1/validate` are still routed but unused by the yb pageserver.
