# compute_ctl HTTP API

Source: `compute_tools/src/http/server.rs` (router), `compute_tools/src/http/routes/*`
(handlers), `compute_tools/src/http/middleware/authorize.rs` (auth),
`compute_tools/src/bin/compute_ctl.rs` (ports). See also
[`compute_ctl.md`](compute_ctl.md).

`compute_ctl` (the `compute_tools/` binary) is the per-endpoint supervisor for a
Postgres compute. It exposes its control API over **two Axum HTTP servers**, built from
the same `Server` enum and router builder in `http/server.rs`:

| Server | Flag (default) | Audience | Auth |
|---|---|---|---|
| **External** | `--external-http-port` (**3080**) | control plane, metrics scraper, autoscaler | JWT bearer (except metrics routes); see below |
| **Internal** | `--internal-http-port` (**3081**) | local processes only — the `neon` Postgres extension, `local_proxy` | none |

Both servers currently bind `Ipv6Addr::UNSPECIFIED` (`[::]`) — the "internal" server is
*conceptually* loopback-only but is not yet pinned to localhost (a `TODO` notes GitHub
runners can't bind localhost). The legacy `--http-port` flag maps to the external port
for backwards compatibility; when used, the internal port defaults to `http_port + 1`.
Port binding retries `EADDRINUSE` for ~10 s to survive fast compute recycles.

## Auth / JWT scope model

External (non-metrics) routes are wrapped by `AsyncRequireAuthorizationLayer` →
`Authorize` (`http/middleware/authorize.rs`):

- A `Bearer` JWT is validated against the **JWKS carried in the spec's
  `compute_ctl_config`**. Algorithm is **EdDSA** by default, switching to **RS256** if
  any JWK declares `RS256`. Expiry (`exp`) is checked; `nbf`/`aud` validation is off in
  the library validator (audience checked manually for admin scope only).
- Scope model (`ComputeClaimsScope`, `libs/compute_api/src/requests.rs`):
  - **`compute_ctl:admin`** → must carry the `compute` audience (`COMPUTE_AUDIENCE`);
    `compute_id` is not checked.
  - any other / no scope → the token's `compute_id` claim must equal this process's
    `--compute-id`.
- **Trusted-instance bypass:** if an `instance_id` is configured, JWT auth is skipped
  entirely (network policy is assumed to gate access).

Unmatched paths return **404**; wrong method returns **405**. All requests carry an
`X-Request-Id` (added if absent) and are traced via OpenTelemetry.

## External server routes

### Observability (unauthenticated)

| Method | Path | Purpose |
|---|---|---|
| GET | `/metrics` | Prometheus text exposition of `compute_ctl`'s own metrics (`metrics::collect()` + `compute_metrics::collect()`). Empty metric families are filtered out. See [`compute_ctl-metrics.md`](compute_ctl-metrics.md). |
| GET | `/autoscaling_metrics` | Proxies `GET /autoscaling_metrics` from the Postgres `neon` extension's metrics exporter over its Unix domain socket in pgdata, and forwards the response verbatim. Used by the autoscaling agent. |

### Lifecycle / status (JWT)

| Method | Path | Purpose |
|---|---|---|
| GET | `/status` | Returns `ComputeStatusResponse` (current `ComputeStatus`, tenant/timeline, error, timestamps). |
| POST | `/configure` | Body `ConfigurationRequest` (`{ spec }`). Parses to `ParsedSpec`; only allowed when status is `Empty` or `Running`. Sets `ConfigurationPending`, then **blocks** until the compute reaches `Running` (returns `ComputeStatusResponse`) or `Failed` (500). |
| POST | `/terminate` | Optional query `?mode=` (`TerminateMode`: fast/immediate). Allowed from `Empty`/`Running`; idempotent if already `Terminated` (201). Drives shutdown and **blocks** until `Terminated`, returning `TerminateResponse { lsn }` (the terminate flush LSN). In `lakebase_mode`, an `Empty` compute exits the process after a 5 s delay. |
| POST | `/promote` | Body `PromoteConfig` (`{ spec, wal_flush_lsn }`). Promotes a Replica to Primary; returns `PromoteState` (500 if `Failed`). |
| GET | `/liveness_probe` | Runs `pg_isready` against the compute's port. 200 `"ok"` / 500. NOTE: handler is marked "NOT ENABLED YET". |

### Health / writability (JWT)

| Method | Path | Purpose |
|---|---|---|
| POST | `/check_writability` | Requires `Running`. Writes to `public.health_check` via `checker::check_writability`. 200 `true` / 500. |

### Catalog & diagnostics (JWT)

| Method | Path | Purpose |
|---|---|---|
| GET | `/dbs_and_roles` | Catalog inventory of databases and roles (`catalog::get_dbs_and_roles`). |
| GET | `/database_schema?database=<name>` | Streams a `pg_dump` schema dump (JSON content-type) for the named DB. 404 if DB does not exist. **Param is `database`** (not `dbname`). |
| GET | `/insights` | Requires `Running`. Collected Postgres usage insights (`collect_insights`). |
| GET | `/metrics.json` | JSON snapshot of the compute's startup metrics (`state.metrics`). Distinct from Prometheus `/metrics`. |

### LFC prewarm / offload (JWT)

| Method | Path | Purpose |
|---|---|---|
| GET | `/lfc/prewarm` | Current `LfcPrewarmState`. |
| POST | `/lfc/prewarm` | Start an LFC prewarm. Optional query `?from_endpoint=<id>` to prewarm from another endpoint's saved state. 202 Accepted, or 429 if a prewarm is already running. |
| DELETE | `/lfc/prewarm` | Cancel an in-progress prewarm. 202. |
| GET | `/lfc/offload` | Current `LfcOffloadState`. |
| POST | `/lfc/offload` | Trigger an LFC offload (save cache state to endpoint_storage). 202, or 429 if already running. |

## Internal server routes

Called by local processes (the `neon` extension, `local_proxy`); no auth layer.

| Method | Path | Purpose |
|---|---|---|
| POST | `/extension_server/{*filename}` | Download a remote extension file. Optional query `?is_library=`. 412 if `--remote-ext-base-url` unset; 409 if spec has no remote-extension info; 404 if the file is unknown. |
| POST | `/extensions` | Install an extension. Body `ExtensionInstallRequest` (`{ extension, database, version }`). Requires `Running`. Returns `ExtensionInstallResponse { extension, version }` (201). Called by the `neon` extension. |
| POST | `/grants` | Add role grants. Body `SetRoleGrantsRequest` (`{ database, schema, privileges, role }`). Requires `Running`. Returns `SetRoleGrantsResponse` (201). |
| POST | `/refresh_configuration` | Postgres-initiated config refresh — nudges `compute_ctl` to re-pull its spec and reconfigure (best-effort, non-blocking). Increments `pg_cctl_pagestream_request_errors_total`. Postgres calls this when it suspects a stale/wrong pageserver. |
| POST | `/failpoints` | **`testing` feature builds only.** Body is a list of `FailpointConfig { name, actions }`. 412 if not compiled with failpoint support. |

## yb-light specifics

`compute_tools/` is upstream Neon, essentially unmodified — the API surface above is
inherited as-is. yb-light's only relevant differences are operational: a single
safekeeper means `/terminate` no longer runs a `sync-safekeepers` walproposer pass, and
`/promote` / `/configure` / `/terminate` are the contact points the (future)
`yblight-controller` uses to drive endpoints. The spec body (`ConfigurationRequest`)
carries the YB-backed pageserver/safekeeper connstrings, `(tenant_id, timeline_id)`, the
endpoint_storage address+token for LFC, and the JWKS used by this very auth layer. The
sharding types were stripped fork-wide, so a `TenantId` (not `TenantShardId`) identifies
a tenant in these payloads.
