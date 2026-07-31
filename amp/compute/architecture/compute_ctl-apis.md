# compute_ctl HTTP API

Source: `compute/compute_tools/src/http/server.rs` (router),
`compute/compute_tools/src/http/routes/*` (handlers),
`compute/compute_tools/src/http/middleware/authorize.rs` (auth),
`compute/compute_tools/src/bin/yb-compute-ctl.rs` (ports),
`compute/libs/compute_api/src/{requests,responses}.rs` (payload types). See also
[`compute_ctl.md`](compute_ctl.md).

`compute_ctl` exposes its control API over **two Axum HTTP servers**, built from the
same `Server` enum and router builder in `http/server.rs`:

| Server | Flag (default) | Audience | Auth |
|---|---|---|---|
| **External** | `--external-http-port` (**3080**) | control plane, metrics scraper, autoscaler | JWT bearer (except metrics routes); see below |
| **Internal** | `--internal-http-port` (**3081**) | local processes only — the `yb_amp` Postgres extension, `local_proxy` | none |

Both servers bind `Ipv6Addr::UNSPECIFIED` (`[::]`) — the "internal" server is
*conceptually* loopback-only but is not pinned to localhost (a `TODO` notes GitHub
runners can't bind localhost). `--http-port` is an alias for `--external-http-port`:
it conflicts with both explicit port flags, and is consulted only in
`--lakebase-mode`, where it sets the external port and the internal port becomes
`http_port + 1`; outside lakebase mode the explicit flags/defaults are used.

Port binding retries `EADDRINUSE` at 200 ms intervals for up to ~10 s (50
attempts): during a fast compute recycle a predecessor `compute_ctl` may still be
releasing the port, and failing the first bind would turn a transient hand-off into
a hard startup failure. Any other bind error (or exhaustion) panics — a compute
that can't serve its control API is useless.

## Auth / JWT scope model

External (non-metrics) routes are wrapped by `AsyncRequireAuthorizationLayer` →
`Authorize` (`http/middleware/authorize.rs`):

- A `Bearer` JWT is validated against the **JWKS carried in the spec's
  `compute_ctl_config`**. Algorithm is **EdDSA** by default, switching to **RS256**
  if any JWK declares `RS256`. Expiry (`exp`) is checked; `nbf` and library-level
  `aud` validation are off (the audience is checked manually for the admin scope
  only), and no claims are marked required at the library level.
- Every key in the JWKS is tried in turn until one verifies the token.
- Scope model (`ComputeClaims` / `ComputeClaimsScope`,
  `compute/libs/compute_api/src/requests.rs`):
  - **`compute_ctl:admin`** → the token must carry the `compute` audience
    (`COMPUTE_AUDIENCE`); `compute_id` is not checked. Missing/invalid audience →
    401.
  - any other / no scope → the token's `compute_id` claim must equal this
    process's `--compute-id`; missing or mismatched → 403.
- **Trusted-instance bypass:** if an `instance_id` is configured (the `INSTANCE_ID`
  env var), JWT auth is skipped entirely — network policy is assumed to gate access
  to the external port in such deployments.
- A missing/malformed `Authorization` header is 400; a token that fails
  verification is 401.

Unmatched paths return **404**; wrong method returns **405**. Every request gets an
`X-Request-Id` (generated if absent), which is propagated to the response and into
logs; requests are traced via OpenTelemetry (`tower_otel`).

Errors are JSON: `{"error": "<message>"}` (`GenericAPIError`). Handlers that
require a specific compute status return **412 Precondition Failed** with
`invalid compute status: <status>` when it doesn't hold.

## External server routes

### Observability (unauthenticated)

| Method | Path | Purpose |
|---|---|---|
| GET | `/metrics` | Prometheus text exposition of `compute_ctl`'s own metrics (`metrics::collect()` + `compute_metrics::collect()`, empty families filtered out). See [`compute_ctl-metrics.md`](compute_ctl-metrics.md). |
| GET | `/autoscaling_metrics` | Proxies `GET /autoscaling_metrics` from the `yb_amp` extension's in-Postgres metrics exporter, reached over its Unix domain socket in pgdata (`yb_amp-communicator.socket`), and forwards status/content-type/body verbatim. Used by the autoscaling agent. |

### Lifecycle / status (JWT)

| Method | Path | Purpose |
|---|---|---|
| GET | `/status` | `ComputeStatusResponse`: `start_time`, `tenant` (always `null`), `timeline` (the `branch_id`, when a spec is loaded), `status`, `last_active`, `error`. |
| POST | `/configure` | Body `ConfigurationRequest` = `{ spec, compute_ctl_config }` (the handler consumes only `spec`; a spec that fails `ParsedSpec` validation is 400). Allowed only from `empty` or `running` (else 412). Sets `configuration_pending`, then **blocks** until the compute reaches `running` (200 with `ComputeStatusResponse`) or `failed` (500). |
| POST | `/terminate` | Optional query `?mode=fast|immediate` (`TerminateMode`, default `fast`). If already `terminated`, returns **201** with the previous response. Allowed from `empty`/`running` (else 412). In `--lakebase-mode`, terminating an `empty` compute returns 200 and exits the process after a 5 s grace so the response can flush. Otherwise sets `termination_pending_*`, signals Postgres, **blocks** until `terminated`, and returns `TerminateResponse { lsn }` — `lsn` is always `null` (nothing populates `terminate_flush_lsn`; the durable LSN lives on the safekeeper, whose WAL is in YugabyteDB — [#58](https://github.com/yugabyte/amp/issues/58)). `fast` keeps the process alive ~30 s after Postgres exits so the control plane can collect the result; `immediate` exits after ~300 ms. |
| POST | `/promote` | Body `PromoteConfig` = `{ spec, wal_flush_lsn }`. 400 if the spec fails validation. Promotes a Replica to Primary (`compute_promote.rs`); returns `PromoteState` — 200 for `not_promoted`/`completed {...timings}`, 500 for `failed { error }`. Repeated calls attach to the single in-flight promotion and return its result. |
| GET | `/liveness_probe` | Runs `pg_isready` against the compute's Postgres port. 200 `"ok"` / 500. The handler comment marks it "NOT ENABLED YET" — no deployment wires it up as an actual probe. |

### Health / writability (JWT)

| Method | Path | Purpose |
|---|---|---|
| POST | `/check_writability` | Requires `running` (else 412). Upserts a row into `public.health_check` (`checker.rs`); a `DISK_FULL` SQL error is tolerated. 200 `true` / 500. |

### Catalog & diagnostics (JWT)

| Method | Path | Purpose |
|---|---|---|
| GET | `/dbs_and_roles` | `CatalogObjects { roles, databases }` (`catalog::get_dbs_and_roles`). |
| GET | `/database_schema?database=<name>` | Streams a `pg_dump --schema-only` dump (served with a JSON content-type header even though the body is SQL text). 404 if the database does not exist. **The query param is `database`.** |
| GET | `/insights` | Requires `running` (else 412). Top-100 `pg_stat_statements` rows (excluding `cloud_admin`) as JSON. |
| GET | `/metrics.json` | JSON snapshot of the compute's startup metrics (`ComputeMetrics`: `wait_for_spec_ms`, `sync_sk_check_ms`, `sync_safekeepers_ms` — always 0, there is no sync-safekeepers pass — `pageserver_connect_micros`, `basebackup_ms`/`_bytes`, `start_postgres_ms`, `config_ms`, `total_startup_ms`, extension-download stats). Distinct from Prometheus `/metrics`. |

### LFC prewarm / offload (JWT)

| Method | Path | Purpose |
|---|---|---|
| GET | `/lfc/prewarm` | Current `LfcPrewarmState` (`not_prewarmed` / `prewarming` / `completed {counts, timings}` / `skipped` / `failed` / `cancelled`). |
| POST | `/lfc/prewarm` | Start an LFC prewarm from endpoint_storage. Optional `?from_endpoint=<id>` to prewarm from another endpoint's saved state. 202 Accepted, or 429 if a prewarm is already running. |
| DELETE | `/lfc/prewarm` | Cancel an in-progress prewarm. 202. |
| GET | `/lfc/offload` | Current `LfcOffloadState`. |
| POST | `/lfc/offload` | Save LFC state to endpoint_storage. 202, or 429 if an offload is already running. |

## Internal server routes

Called by local processes (the `yb_amp` extension finds this port via the
`yb_amp.extension_server_port` GUC; `local_proxy` also calls it); no auth layer.

| Method | Path | Purpose |
|---|---|---|
| POST | `/extension_server/{*filename}` | Download a remote extension file into PG's lib/share dirs. Optional query `?is_library=true`. 412 if `--remote-ext-base-url` is unset; 409 if the spec carries no `remote_extensions` index; 404 if the file is not in the index; 200 on success. |
| POST | `/extensions` | Install/upgrade an extension in a database. Body `ExtensionInstallRequest` = `{ extension, database, version }`. Requires `running` (else 412). Returns 201 with `ExtensionInstallResponse { extension, version }`. |
| POST | `/grants` | Grant schema privileges to a role. Body `SetRoleGrantsRequest` = `{ database, schema, privileges, role }`. Requires `running` (else 412). Returns 201 with `SetRoleGrantsResponse` echoing the grant. |
| POST | `/failpoints` | **`testing` feature builds only** (the route is compiled out otherwise). Body is a list of `FailpointConfig { name, actions }`; the extra action `exit` kills the process. 412 if the binary lacks failpoint support. |

## yb-amp specifics

The spec payloads carried by `/configure` and `/promote` address storage with a
single **`BranchId`** (no shard or tenant/timeline identifiers) and carry the
YB-backed pageserver/safekeeper connection info, the endpoint_storage address +
token for LFC state, and the JWKS this very auth layer verifies against. Because
the single safekeeper's WAL is durable in YugabyteDB the moment it is flushed,
`/terminate` has no flush-LSN handshake to perform — hence the always-`null` `lsn`
above. In the dev cluster, `yb_amp_local` drives exactly three of these routes:
`GET /status` (start poll), `POST /terminate?mode=immediate` (stop), and the
metrics routes for scraping, minting itself a `compute_ctl:admin`-scoped JWT
(`controller/local/launch/endpoint.go`).
