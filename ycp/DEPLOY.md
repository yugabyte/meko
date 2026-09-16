# Deploying the YCP data plane

The image built from this directory — `ycp-reconciler-agent` — and how it is
built, configured, tested and operated. For the component overview see
[`ARCHITECTURE.md`](./ARCHITECTURE.md).

**Deploying the control plane is not covered here.** It is `api_server` with
`--control-plane-mode`, installed from the `meko` Helm chart; the step-by-step is
[`meko/deploy/helm/meko/YCP-RUNBOOK.md`](../meko/deploy/helm/meko/YCP-RUNBOOK.md).
§4 of that runbook is also how an agent gets attached to it in a real cluster. What
follows is for building and debugging the agent itself.

## Prerequisites

- Go 1.25+ to build.
- For the build-and-deploy test: `kind`, `kubectl`, `curl`, `python3`, and a
  container engine — docker or podman, auto-detected (`YCP_E2E_ENGINE` overrides).

## Run locally

The agent needs an in-cluster Kubernetes API to read facts from, so it is not
useful outside a cluster — use the e2e below instead.

## Container images

Built and pushed to **Quay** on every merge to `main` by
[`.github/workflows/ycp-reconciler-agent-image.yml`](../.github/workflows/ycp-reconciler-agent-image.yml),
tagged with the commit SHA (pin this in a deployment); only a `main` build moves
`latest`. PRs build but never push. Quay rather than the platform ECR so a cluster
pulls it without cross-account pull-secret setup — the same reason the control plane
ships there.

| Dockerfile | Binary | Image |
|------------|--------|-------|
| `Dockerfile.reconcile-agent` | `cmd/reconcile-agent` (the BYOC pull agent, either kind) | `quay.io/yugabyte/ycp-reconciler-agent` |

It is `gcr.io/distroless/static:nonroot` at runtime — no shell, no package
manager, so `kubectl exec` into it will not give you a prompt. Use logs and
port-forward.

The control plane's image (`meko/meko-api-server`) is **not** built by CI; it is
still pushed by hand. The runbook's Prerequisites section says how to check that a
given tag carries the control-plane flags.

To build locally (`podman build` works the same, and is what the e2e uses when
docker is absent):

```bash
./build-reconciler-agent-image.sh --no-push
```

## Tests

Unit tests need nothing running:

```bash
go test ./...
```

The build-and-deploy test is the end-to-end one. It builds the api-server image
from source, loads it into a throwaway `kind` cluster, and asserts the surface the
control plane presents against the **real thing** (`api_server
--control-plane-mode` on a plain Postgres): every workload rolls out including the
migrate init container, `/api/v1/health` reports both databases up, the fleet API
rejects an unauthenticated call, registering an environment binds it to an
`api_key_id` and seeds no desired state, and the install hint dials the
`agent_base_url` the registration stored.

It does not cover an agent converging anything. The retired meko-agent could stand
a stack up from public images alone, which is what made that assertable without
credentials; the reconcile-agent needs an api-server or an amp controller to
converge against, so its coverage lives in `test/local/` and in the backends' own
unit tests until a fixture exists.

It deploys the control plane under a `/ycp` base path, matching how production
fronts it at `https://devcloud.yugabyte.com/ycp/`, and asserts the mount: the
console answers at `/ycp/` and `/ycp`, the API under `/ycp/api/v1`, the domain
root 404s, and `/api/v1/health` still answers there (which is also what the pod's
readiness probe uses). The prefix lives only in `$BASE` in `run.sh` and is
substituted into the manifests' `__BASE_PATH__` placeholder at apply time, so
`YCP_E2E_BASE_PATH= ./test/e2e/run.sh` tests the root mount with nothing else to
edit.

```bash
./test/e2e/run.sh                  # create, test, tear down
YCP_E2E_KEEP=1 ./test/e2e/run.sh   # leave the cluster up to poke at
```

It creates and deletes its own cluster (`kind-ycp-e2e`) and pins every `kubectl`
call to that context, refusing to run if the context does not resolve to the kind
node it just created — so it cannot touch whatever cluster your kubeconfig
happens to point at. On failure it dumps pod state, container logs and events.

Knobs: `YCP_E2E_CLUSTER`, `YCP_E2E_TAG`, `YCP_E2E_PORT`, `YCP_E2E_TIMEOUT`,
`YCP_E2E_KEEP`, `YCP_E2E_BASE_PATH`.

Against a cluster you already have, `test/local/` drives the reconcile loop for
real: `ycp-on-cluster.sh` installs the control plane, and `meko-reconcile.sh`
registers a `meko` environment, declares an agent and datapacks, and asserts the
agent converges and GCs them.

Both CI jobs are `meko_ycp` and `meko_ycp_e2e` in
[`meko-pr-build.yml`](../.github/workflows/meko-pr-build.yml). The e2e fires on `ycp/**` and
on the `api_server` files that make up the control plane (its flags, router,
environment service/repository/handlers, console and migrations), since it builds
that image too.

## Telling an attached agent what to run

Registering an environment binds it to an API key and gives you the install hint;
declaring desired state is what tells the agent to converge something.

```bash
curl -X POST "$YCP/api/v1/environments" \
  -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H 'content-type: application/json' \
  -d '{"name":"acme-prod","kind":"meko","placement":"cloud","region":"us-east-2",
       "api_key_id":"'$(uuidgen)'"}'
```

The caller supplies a name, the kind (`meko` or `amp`), and the id of the API key the
agent will authenticate with — YBM mints it in production; direct against the control
plane any uuid works, handed to a dev-mode agent as `YCP_DEV_API_KEY_ID`.
`GET /api/v1/environments/{id}` then renders the install hint: a `helm upgrade
--install` of the meko chart with `reconcileAgent.enabled=true`, with the stack's
image tags filled in from the release pinned in
[`meko/api_server/internal/service/dataplane_release.go`](../meko/api_server/internal/service/dataplane_release.go).
**To ship a new data-plane release, bump the tags there** — hints rendered afterwards
carry the new set, and an installed release keeps what it was installed with until
someone upgrades the chart. The three stack tags are versioned per component, not per
release, so the hint prints one variable each.

**Registration seeds no desired state.** The workloads — YugabyteDB with `mage` and
`pg_dist_rag`, api-server, MCP server, RAG worker, inference gateway, UI — come from
the chart the hint installs, not from the control plane, so a freshly registered
environment has an agent enrolled and nothing declared. `POST /environments` takes a
`"desired": [...]` if you want to declare resources at registration time.

To change what an environment runs, replace its desired state. That is one call, and
the API is the only way to make it — the console reads desired state but does not edit
it:

```bash
curl -X POST "$YCP/api/v1/environments/$ENV/desired" \
  -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H 'content-type: application/json' \
  -d '{"resources":[
        {"kind":"agent","name":"ycp"},
        {"kind":"datapack","name":"ycp/team-a","spec":{"description":"team A"}}
      ]}'

curl -H "X-Meko-Admin-Secret: $ADMIN_KEY" "$YCP/api/v1/environments/$ENV/desired"   # read it back
```

The set is **replaced**, not merged: anything left out is garbage-collected on the
next tick. **The agent is the api-server's only caller and is authoritative over it** —
a datapack made by hand that desired state does not declare is deleted.

A kind this environment's agent does not serve is refused with a 400 naming the entry
(`resources[1] has kind "datapak", which a meko environment does not serve`), on both
this call and `POST /environments`. Only the **kind** is checked here — name shape is
the backend's rule, so `"<agent>/<datapack>"` is enforced a tick later as permanent
drift rather than at declare time.

### What a meko environment declares

| Kind | Name | Spec | Notes |
|------|------|------|-------|
| `agent` | `"<agent>"` | _(none)_ | A user-scoped agent. Deleting one **cascades**: its datapacks go first, because the api-server's own delete removes only the agent row and its grants, which would strand every datapack under it holding a free-tier slot. |
| `datapack` | `"<agent>/<datapack>"` | `description` | The path is the parent link — there is no `agent_id` field, and the agent named by the first segment is created first. The leaf is the datapack's name, so a rename is a different key: a delete plus a create. |

The signup agent (`meko_agent`) is excluded from all of this — never listed, never
deleted. It carries the account's share and invite grants, and a create that names no
agent lands on it, so garbage-collecting it because desired state does not mention it
would break the account.

Some refusals are **permanent**, reported as drift and never retried: a name the
api-server rejects (400), a conflict (409), a revoked or unscoped token (401/403), and
a free-tier cap (402) — that counter is lifetime and is never refunded on delete, so
retrying can only fail. Clearing a datapack's description is permanent drift for the
same reason: the api-server will not write an empty one.

Fleet-wide knobs on the control plane:

| Control-plane flag / env | Chart value | Effect |
|---|---|---|
| `--console-embed-admin-secret` / `MEKO_CONSOLE_EMBED_ADMIN_SECRET` | — | Write the admin secret into the console page so it never asks for one. **Off by default, and leave it off where the console is reachable by anyone you would not hand the fleet-admin credential to** — it grants no new capability against that control plane, since the console can already do everything the credential allows, but it does turn "can load the console" into "has the secret". Unset, the console asks in-page and keeps it in `sessionStorage`. |
| `--no-bootstrap-defaults`, `--dataplane-mcp-image`, `--dataplane-image-pull-secret`, `--dataplane-secret-ref` | `apiServer.dataplane.noBootstrapDefaults` / `.mcpImage` / `.imagePullSecret` / `.secretRef` | **Deprecated no-ops.** They configured the stack registration used to seed. Parsed and discarded; the control plane warns at boot if any is set. Still *accepted* because both chart copies render them as **args** when the matching value is set — so removing them would crash-loop a deployment whose values file sets one, with `flag provided but not defined`. Nothing in this repo sets them any more; they go once no out-of-tree values file does. |
| `--dataplane-registry`, `--dataplane-image` | _(none)_ | Deprecated no-ops with no chart path — nothing renders them. Kept only so the whole set is retired in one step. The install hint reads the pinned release directly and prints tags, never a registry. |

The data plane's own Secrets — registry credentials, and the one holding
`OPENAI_API_KEY`, the AWS keys and the Cognito settings — are the chart's business and
live in the agent's namespace. The control plane never learns either: **desired state
carries no secret material**. That is also why `api_server`'s Cognito flags gained
environment fallbacks (`COGNITO_REGION`, `COGNITO_POOL_ID`, `COGNITO_CLIENT_ID`,
`COGNITO_CLIENT_SECRET`, `COGNITO_DOMAIN`): a flag value is visible in the pod spec to
anyone who can read the namespace, so the data-plane api-server takes its credentials
from `envFrom` and only its database wiring as flags. The one flag that carries a
secret, `-db-password`, is written `$(DB_PASSWORD)` and expanded by Kubernetes from a
`secretKeyRef`.

The api-server needs no Kubernetes access: it runs with the namespace's default
ServiceAccount and never builds an in-cluster client, so it cannot fail to start for
want of RBAC.

## Seeing what ran in an amp environment

An amp environment's reconcile agent relays the controller's task log on its status
heartbeat, so operations are readable from the control plane without cluster access:

```bash
# what is running right now
curl -sS -H "X-Meko-Admin-Secret: $SECRET" -H "X-Meko-Account-ID: $ACCOUNT" \
  "$YCP/api/v1/environments/$ENV/tasks?active=true" | jq '.data.items'

# what has failed, most recent first
curl -sS -H "X-Meko-Admin-Secret: $SECRET" -H "X-Meko-Account-ID: $ACCOUNT" \
  "$YCP/api/v1/environments/$ENV/tasks?status=failed" | jq '.data.items[] | {kind, target_ref, error}'

# one operation in full
curl -sS -H "X-Meko-Admin-Secret: $SECRET" -H "X-Meko-Account-ID: $ACCOUNT" \
  "$YCP/api/v1/environments/$ENV/tasks/$TASK_ID" | jq .data
```

| Parameter | Meaning |
|---|---|
| `active=true` | shorthand for `status=pending` + `status=running` |
| `status` | repeatable, or comma-separated: `pending`, `running`, `succeeded`, `failed`. An unrecognized value is **dropped, not rejected**, so a stale client gets results rather than a 400 |
| `kind` | exact match, e.g. `resize_endpoint`, `create_branch` |
| `cluster_id` | only tasks scoped to one cluster |
| `page`, `page_size` | 1-based; default 25, max 200 |

Response fields worth knowing:

- `error` is the **controller's own** failure reason, not the agent's summary of it.
- `ycp_generation` is `null` for a task an operator or the amp UI planned directly
  rather than YCP driving it — how to tell "the console did this" from "someone went
  around the control plane".
- `reported_at` (per task) and `last_heartbeat_at` (per response) are what separate a
  task that has not moved from an agent that has stopped reporting. A stale
  `last_heartbeat_at` means the whole page is stale, whatever the statuses say.

**History depth is 30 days.** The agent forgets what it relayed when its pod restarts
and the control plane cannot reach into the data plane to re-read the log, so its own
table is the durable copy and that retention is the answer to "how far back can I ask
what ran".
Terminal tasks past the window are deleted on the next heartbeat that records one;
`pending` and `running` tasks are never pruned however old they look.

A `kind: meko` environment runs no controller and so has no task log: these routes
return an empty page rather than an error.

## Which environments a fleet call sees

The admin secret is a fleet-wide credential, so a caller presenting only it — the
console, the e2e, your curl — reads and writes every environment. That is the
default; `--require-account-header` (below) makes it a 400 instead.

`X-Meko-Account-ID` narrows a call to one account. YBM sends it on every request it
proxies, stamped from the account it has already authenticated:

```bash
curl -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H "X-Meko-Account-ID: $ACCOUNT" \
  "$YCP/api/v1/environments"                      # only $ACCOUNT's environments
```

With the header set, registration stamps the new environment with that account and
ignores any `account_id` in the body, and every other `/environments` route — read,
patch, deregister, desired state — is restricted to environments carrying it. One
that belongs to another account answers 404, the same as one that does not exist.

Environment ids come from the name, and the name is unique per account rather than
across the fleet — `(account_id, id)` is what the database enforces. So two accounts
may each register `prod` and each gets its own environment; neither can address or
overwrite the other's. Re-registering your own name is refused (409) rather than
resetting it: an environment is never replaceable by registering over it.
Deregister it if it is no longer needed, and register the replacement under a new
name.

The id an operator uses is unchanged by that. Rows are keyed internally by a uuid,
which is what the desired-resources table joins on; the fleet API's `{id}` path
segment and `--set agent.envName=` are the name-derived slug, and agent calls
carry no id at all — the environment is resolved from the api key they
authenticate with.

The one call that can now be ambiguous is an **unscoped** read of an id two accounts
both hold: it answers 409 rather than picking one. Send an account to resolve it.
That cannot happen for environments registered before the per-account index, whose
ids were unique fleet-wide — and it cannot happen at all with the header required.

Environments registered before accounts were stamped carry no account, and are
visible only to an unscoped caller.

To make the header mandatory, so no call can reach the whole fleet:

| Control-plane flag / env | Chart value | Effect |
|---|---|---|
| `--require-account-header` / `MEKO_REQUIRE_ACCOUNT_HEADER` | `apiServer.requireAccountHeader` | Answer 400 to any fleet call with no `X-Meko-Account-ID`, instead of serving it fleet-wide. **Off by default**, because it takes the unscoped path away: the console, the e2e and operator curl all authenticate with the admin secret alone. For a control plane reached only through YBM. |

The agent API is unaffected either way — it resolves the environment from the
api-key-id header and never needs an account header.

## Operating an attached agent

```bash
kubectl -n $ANS logs deploy/meko-reconcile-agent      # the pull loop
kubectl -n $ANS get secret reconcile-agent-state      # its persisted identity
```

A healthy start — first or restarted; enrollment is an idempotent check-in the
agent repeats at every startup:

```
reconcile-agent starting: kind=meko env=meko-dev ycp=http://…
driving the meko api-server at http://meko-api-server-service:80/api/v1
enrolled env=meko-dev id=meko-dev uuid=…
```

**A converging loop then logs nothing.** Every tick that reconciles cleanly and
reports is silent, so an idle log is the healthy state and not a stuck agent —
what it did is visible in the environment's inventory on `GET
/api/v1/environments/{id}`, not in its own output. Only a failure speaks:
`reconcile: fetch desired`, `reconcile: tick`, `reconcile: report`.

Failure modes worth knowing:

| Symptom | Cause |
|---|---|
| `collect components (reporting status anyway): … forbidden` | the agent's Role lacks `list` on `apps/deployments,statefulsets` in its namespace, so the components tab stays empty. Logged **once** and not retried — every tick would refuse identically — so upgrade the release to a chart carrying the grant and restart the agent. Non-fatal either way: the convergence report the environment's state rides on still lands. |
| `could not read agent state` / `could not persist agent state` | the ServiceAccount lacks `get`/`create`/`update` on secrets in its namespace. Not fatal — the state holds no credential, and the check-in re-establishes identity from the API key — but a restart during a control-plane outage then starts from nothing. |
| `environment changed (…) — discarding persisted identity` | `ENV_NAME` or `ENV_KIND` no longer matches what the state Secret recorded. Expected after an operator re-points the agent; unexpected otherwise, and it means the next enroll starts from scratch. |
| `ENROLL REJECTED (401)`, repeating | the API key was revoked. Redeploy with a fresh key Secret pointed at a newly registered environment — an environment cannot be rebound to a different key once registered. |
| `enroll: kind mismatch`, then exit | `ENV_KIND` disagrees with the kind the environment was registered as. Fatal by design: it can never succeed, and resuming a persisted identity past it would converge an environment whose enrolment was refused. |
| `DESIRED-STATE REJECTED (401)` | as above, on the desired-state fetch. The agent keeps converging toward the last desired state it read and skips garbage collection, since it cannot know what an operator withdrew from a response it never got. |
| `reconcile: fetch desired: …`, repeating | the control plane is unreachable or erroring. Same fallback: converge to last-good, no GC. A data plane survives a control plane being deleted outright. |
| `MEKO_API_TOKEN is unset: calling … unauthenticated` | fine against an api-server without Cognito, which authenticates nobody. With Cognito on, every call will 401 and the resources will read `Failed`. |
| `amp task feed unavailable: the controller ignored updated_since` | the reconcile agent is newer than the amp controller it drives. Task reporting stays off for the life of the agent (inventory is unaffected); restart it against a controller built from this repo's `main` or later. |
| `collect amp tasks (reporting inventory anyway)` | the controller's task API is erroring. Deliberately non-fatal: the environment's state and inventory still land, and the unreported tasks are re-sent on a later tick. |
| `status accepted but the task delta was NOT stored` | the control plane took the report but could not store the tasks, so the delta is held for the next tick rather than committed. Persisting means the control plane's task write is failing. |
| a resource row reads permanent drift and never retries | the backend refused it for a reason retrying cannot change — a name the api-server rejects, a free-tier cap (the counter is lifetime and never refunded), a revoked token, or an update the api-server will not write (clearing a datapack description). Fix the desired state; the agent will not keep hammering it. |
| a `datapack` row stays `Pending` | its `agent` parent has not converged yet. Children are gated on the parent existing, so the agent row's own reason is the one to read. |

The agent's credential is the operator-provided `meko-agent-api-key` Secret
(`reconcile-agent-state` holds only the identity its last check-in confirmed).
Deleting the agent's release removes neither — delete the namespace, and `DELETE
/api/v1/environments/{id}` to drop the binding; its response carries the
`api_key_id` so YBM can revoke the key.

Troubleshooting the **stack** — the database, api-server, mcp-server and the rest —
is the chart's business now that the agent no longer installs it: see
[`meko/deploy/helm/meko/YCP-RUNBOOK.md`](../meko/deploy/helm/meko/YCP-RUNBOOK.md).

### In-cluster addresses carry a trailing dot

Every in-cluster Service address handed to the agent — the chart's default
`MEKO_API_URL`, the DSNs it composes — is written
`svc.ns.svc.cluster.local**.**`, absolute.

Kubernetes puts `ndots:5` in every pod's resolv.conf, and `meko.ycp-agent.svc.cluster.local`
has four dots, so a resolver treats it as *relative*: it tries the whole search list
first (`…svc.cluster.local.ycp-agent.svc.cluster.local`, and so on) before asking for
the name as written. Go's resolver does this, and the bogus lookups alone were enough
to exhaust a 10-second connect budget. The symptom is a service that is plainly
healthy — `1/1`, reachable by ClusterIP, instant for the same name written with a
trailing dot — while every connection the agent makes times out. If you write one of
these addresses by hand (`reconcileAgent.mekoApiUrl`, `reconcileAgent.ycpUrl`), the
short form (`meko`) and the absolute form both work; the four-dot relative form is
the one to avoid.

## Configuration reference

One agent, either kind. **`ycp-reconciler-agent`** converges an environment's declared
resources over a REST API — the amp controller for `ENV_KIND=amp`, meko's api-server
for `meko` — and never applies a manifest. The `reconcileAgent.*` chart values set
every variable below, and the install hint on `GET /environments/{id}` prints the
whole command.

| Env | Default | Purpose |
|-----|---------|---------|
| `YCP_URL` | `http://ycp:8080/api/v1` | The API base agent routes hang off: YBM's pass-through (`…/api/public/v1/accounts/<id>/ycp`), or the control plane's own `/api/v1` (behind `--base-path`: `https://devcloud.yugabyte.com/ycp/api/v1`); a trailing slash is trimmed. |
| `ENV_NAME` | _(release namespace)_ | Environment name, and what the state Secret's re-point detection compares against. Must match the registered environment's id. |
| `YCP_API_KEY` | _(unset)_ | The YBM API key every call authenticates with. Read from a Secret, never a command line. Required unless both dev-mode vars are set. |
| `YCP_DEV_ADMIN_SECRET` / `YCP_DEV_API_KEY_ID` | _(unset)_ | Dev mode against a standalone control plane (no YBM): sent as the `X-Meko-Admin-Secret` / `X-Meko-Api-Key-ID` headers instead of the bearer. Both must be set. |
| `ENV_PLACEMENT` | `cloud` | Reported placement. |
| `AGENT_VERSION` | _(image tag)_ | Version string reported in status. The chart sets it from the image tag. |
| `ENV_KIND` | `amp` | Which backend to run. `amp` drives the amp controller; `meko` drives meko's api-server. The enrollment carries it, and a mismatch with the environment's registered kind is fatal — it can never succeed, and resuming a persisted identity through it would converge an environment whose enrolment was refused. |
| `MEKO_API_URL` | `http://meko-api-server-service:80/api/v1` | The api-server to converge against, **including** its API path (a `--base-path` prefix sits before `/api/v1`). The chart defaults it to the release's own api-server by FQDN. |
| `MEKO_API_TOKEN` | _(unset)_ | An MCP token with the read and write scopes. Not needed against an api-server running **without** Cognito, which authenticates nobody — the BYOC shape. With Cognito on it is required, and picks the account the agent's datapacks belong to. |


