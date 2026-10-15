# Deploying YCP from scratch

Step-by-step for standing up the **Yugabyte Control Plane** (`mode: ycp` — `api-server`
run with `--control-plane-mode`) and attaching an agent to it. Written from a real
bring-up on `meko-dev-eks`; every command here was run.

For the chart as a whole, and the data-plane (`mode: mcp`) deploy, see
[`README.md`](./README.md).

> The presets now carry everything that used to be a pile of `--set` flags
> ([#42](https://github.com/yugabyte/meko-devops/issues/42)): the namespace, the
> migrate scope, Cognito off, the storage class, and — for the agent — the whole
> "switch off seven data-plane components" list. What is left is what genuinely varies
> per release: the image tag, and the credentials (which the chart can now render for
> you, §1).

## Prerequisites

- `kubectl` and `helm`, plus a checkout of this repo. **The chart is not published
  anywhere** — there is no `meko` Helm repo, so it is always installed from a path.
- **An api-server image carrying the flags.** `1.0.35-20260728` or newer for
  control-plane mode; `1.0.37-20260729` or newer if you also intend to set
  `apiServer.basePath` (see [Serving it under a subpath](#serving-it-under-a-subpath)).
  Nothing in CI builds api-server images, so check rather than assume:

  ```bash
  R=161908593886.dkr.ecr.us-east-2.amazonaws.com
  docker run --rm --entrypoint /usr/local/bin/meko-api-server $R/meko/meko-api-server:$TAG --help \
    | grep -E 'control-plane-mode|only-controlplane-migrate|base-path'
  ```

  A missing `--base-path` is the quiet one: the flag is unset via env var, so the
  pod starts fine and serves at the root while the proxy sends everything to
  `/ycp` — every request 404s and nothing in the logs says why.

  An older image fails one of two ways: no `--control-plane-mode` and the pod
  crash-loops on `flag provided but not defined`; no `-only-controlplane-migrate`
  and the migrate init container does.
- **Registry pull authorization.** EKS in the same AWS account: node IAM, nothing to
  do. GKE or cross-account: a `dockerconfigjson` Secret in every namespace that
  pulls, refreshed (ECR tokens last ~12h) — see `ecrCredentialRefresh` in
  `values.yaml`.

## 1. Namespace and Secrets

Pick a namespace of its own. **Not `meko-system`**: the data plane's api-server
Deployment has the same name, so Helm refuses to adopt it — and forcing it would
replace the live data plane.

```bash
NS=ycp-system
kubectl create namespace $NS
```

Five Secrets. Either let the chart render them (simplest, and it cannot get the
shared DB password wrong) or create them by hand.

**Option A — the chart renders them.** Generate the values here, where you can see
and save them, and pass them at install time:

```bash
PGPW=$(openssl rand -hex 16)          # or something trivial for a throwaway dev instance
ADMIN_KEY=$(openssl rand -hex 24)     # save this — see below
MCP_SECRET=$(openssl rand -hex 24)

SECRET_ARGS=(
  --set controlPlaneSecrets.enabled=true
  --set controlPlaneSecrets.dbPassword="$PGPW"
  --set controlPlaneSecrets.adminApiKey="$ADMIN_KEY"
  --set controlPlaneSecrets.mcpInternalSecret="$MCP_SECRET"
)
```

One `dbPassword` reaches all three Secrets that must agree on it. The values end up
in Helm release state (`helm get values ycp` shows them), which is fine for a dev or
demo control plane and a judgement call otherwise — Option B keeps them out of it.
Nothing is auto-generated on purpose: a chart-generated password would have to be
read back with `lookup` on every upgrade, and `lookup` sees nothing under `helm
template` or a client dry-run, so it would silently rotate and lock you out.

**Option B — by hand.** Also the path to use on a release whose Secrets already
exist: Helm will not adopt objects it did not create, so enabling Option A on top of
hand-made Secrets fails with `invalid ownership metadata`.

```bash
PGPW=$(openssl rand -hex 16)          # or something trivial for a throwaway dev instance

kubectl -n $NS create secret generic ycp-postgres              --from-literal=password="$PGPW"
kubectl -n $NS create secret generic meko-api-server-db        --from-literal=db-password="$PGPW"
kubectl -n $NS create secret generic meko-api-server-system-db --from-literal=db-password="$PGPW"
kubectl -n $NS create secret generic meko-mcp-internal-secret  --from-literal=secret-key="$(openssl rand -hex 24)"
kubectl -n $NS create secret generic meko-admin-api-key        --from-literal=admin-api-key="$(openssl rand -hex 24)"
```

| Secret | Key | Used for |
|---|---|---|
| `ycp-postgres` | `password` | the bundled Postgres' own password |
| `meko-api-server-db` | `db-password` | api-server → data DB |
| `meko-api-server-system-db` | `db-password` | api-server → system DB (the fleet registry) |
| `meko-mcp-internal-secret` | `secret-key` | the internal validate endpoint |
| `meko-admin-api-key` | `admin-api-key` | gates the fleet/console routes |

**The three DB passwords must be the same value.** Postgres takes its password from
`ycp-postgres`; api-server takes its own from the two `*-db` Secrets. They are
separate Secrets pointing at one database. Option A exists mostly to make this
impossible to get wrong — one `dbPassword`, fanned out. Get it wrong by hand and the
api-server starts, passes its probes, and fails every query.

Keep the admin key somewhere — every fleet API call needs it and it cannot be
recovered from the chart. Add `meko-api-server-cognito` too if you set
`apiServer.cognito.enabled=true`.

## 2. Install

```bash
helm upgrade --install ycp charts/meko -n $NS \
  -f charts/meko/values-aws.yaml \
  -f charts/meko/values-dev.yaml \
  -f charts/meko/values-ycp.yaml \
  "${SECRET_ARGS[@]}" \
  --set-string apiServer.image.tag=$TAG \
  --set-string apiServer.config.MEKO_BYOC_METRICS_URL=$BYOC_METRICS_URL \
  --wait --timeout=300s
```

Drop `"${SECRET_ARGS[@]}"` if you took Option B. Everything else that used to be a
`--set` now lives in the presets: `values-ycp.yaml` supplies the namespace
(`ycp-system`, which `controlPlaneDb` inherits) and turns Cognito off, and
`values-aws.yaml` names a storage class that exists on our EKS clusters.

Two things still bite:

- **`BYOC_METRICS_URL` is the fleet's BYOC metrics door**, and nothing stops you
  leaving it out: the binary then defaults to the devcloud door, prints it in every
  install hint, and marks environments shipping to their own fleet's door DEGRADED.
  Devcloud is `https://byoc.devcloud.yugabyte.com/api/v1/otlp/metrics`, prod
  `https://byoc.cloud.yugabyte.com/api/v1/otlp/metrics`.
- **`values-ycp.yaml` must come last.** Helm merges `-f` files left to right, and the
  env presets set `apiServer.migrate.scope` and the `SYSTEM_DB_`/`DB_` endpoints for a
  data plane. Put the mode preset earlier and those win — the release comes up
  silently pointed at the shared data-plane YugabyteDB running the wrong migration
  set. No error, just the wrong database.

Storage used to bite too: `values-aws.yaml` asked for `auto-ebs-sc` (EKS Auto Mode),
which `meko-dev-eks` does not have, and a PVC naming a missing class sits `Pending`
forever with nothing obvious on the StatefulSet. The preset now says `gp2-csi`, and a
class the target cluster lacks fails the install outright, listing the ones it has.

What lands: the api-server Deployment, its ClusterIP Service, an internal
LoadBalancer, a ConfigMap, a ServiceAccount, and the `ycp-postgres` StatefulSet +
Service + PVC. Nothing outside `$NS` — the tenants-namespace Role a data-plane
release needs is not rendered in this mode.

## 3. Verify

```bash
kubectl -n $NS get pods                                 # meko-api-server + ycp-postgres, both Running
kubectl -n $NS logs deploy/meko-api-server -c migrate   # "schema migrated" purpose=controlplane
kubectl -n $NS port-forward svc/meko-api-server-service 8080:80 &

K=$(kubectl -n $NS get secret meko-admin-api-key -o jsonpath='{.data.admin-api-key}' | base64 -d)
curl -s  localhost:8080/api/v1/health                                        # all DB checks ok
curl -so /dev/null -w '%{http_code}\n' localhost:8080/api/v1/environments     # 401 — expected
curl -s -H "X-Meko-Admin-Secret: $K" localhost:8080/api/v1/environments       # 200
```

A few `database ping failed … connection refused` lines in the migrate container are
normal — it starts before Postgres is accepting connections and retries.

The console is served at `/`. It prompts once for the admin secret and keeps it in
browser storage for the tab.

### Serving it under a subpath

To front the console on a shared domain instead of its own host — YBM devcloud
serves it at `https://devcloud.yugabyte.com/ycp/` so the browser's existing auth
tokens and session storage carry over — set `apiServer.basePath`:

```bash
helm upgrade --install ... --set apiServer.basePath=/ycp
# or, with scripts/deploy-ycp-ybm.sh:  BASE_PATH=/ycp BYOC_METRICS_URL=... ./deploy-ycp-ybm.sh
```

Everything moves under the prefix: console at `/ycp/`, fleet API at
`/ycp/api/v1/environments`, agent API at `/ycp/api/v1/agent/*`. Three things worth
knowing before you set it:

- **Only set it if the proxy in front forwards `/ycp` through unstripped.** If the
  proxy strips the prefix instead, leave `basePath` empty — the console derives its
  own API prefix from its URL and handles either arrangement.
- **The probes do not move.** `/api/v1/health` stays served at the domain root
  because kubelet reaches the pod directly and never passes through the proxy. The
  chart leaves `ingress.healthcheckPath` alone for that reason.
- **Reaching it directly changes.** Through the internal LB or a port-forward the
  console is now at `:8080/ycp/`, and `/` returns 404. Every `curl` in this runbook
  needs the prefix too.

Agents must point at the prefixed URL — `--set reconcileAgent.ycpUrl=https://…/ycp/api/v1`.
The base path itself must be a plain path prefix, not a URL; the api-server
refuses to start otherwise rather than mounting somewhere unreachable.

## 4. Register an environment and attach an agent

```bash
KEY_ID=$(uuidgen | tr 'A-Z' 'a-z')
curl -sX POST localhost:8080/api/v1/environments \
  -H "X-Meko-Admin-Secret: $K" -H 'content-type: application/json' \
  -d '{"name":"meko-dev","kind":"meko","account_id":"yugabyte","placement":"cloud","region":"us-east-2","api_key_id":"'$KEY_ID'"}'
```

The environment is bound to that `api_key_id`. Through YBM the id belongs to a real
YBM API key the agent sends as a bearer; direct against the control plane (this
runbook's shape) the agent runs in dev mode, sending the admin secret + the id as
headers.

`GET /api/v1/environments/<id>` then renders an install hint with the namespace, this
control plane's URL and the stack's image tags already filled in. Run that. It looks
like this:

```bash
ANS=meko-dp
kubectl create namespace $ANS
kubectl -n $ANS create secret generic meko-agent-api-key \
  --from-literal=dev-admin-secret="$K" --from-literal=dev-api-key-id="$KEY_ID"

# The database: an RF3 Foundation in the same namespace (chart README, "The BYOC
# database"). YBA takes 20-40 minutes.
helm upgrade --install meko-foundation-crd foundation/deploy/helm/meko-foundation-crd --wait
helm upgrade --install fdn foundation/deploy/helm/meko-foundation -n $ANS \
  --set "imagePullSecrets[0].name=quay-registry-creds" --wait
kubectl -n $ANS apply -f foundation/examples/meko-foundation-cr.yaml
kubectl -n $ANS wait foundation/foundation --for=jsonpath='{.status.phase}'=Ready --timeout=60m
YB_HOST=$(kubectl -n $ANS get foundation foundation -o jsonpath='{.status.address.ysql_host_ports}')

helm upgrade --install meko charts/meko -n $ANS \
  -f charts/meko/values-aws.yaml \
  -f charts/meko/values-dev.yaml \
  -f charts/meko/values-byoc.yaml \
  --set-string database.host="${YB_HOST%:*}" \
  --set-string apiServer.image.tag=$API_TAG \
  --set-string mekoMcpServer.image.tag=$MCP_TAG \
  --set-string mekoUi.image.tag=$UI_TAG \
  --set reconcileAgent.enabled=true \
  --set reconcileAgent.ycpUrl=http://meko-api-server-service.$NS.svc.cluster.local.:80/api/v1 \
  --set reconcileAgent.envName=meko-dev \
  --set-string reconcileAgent.image.tag=$AGENT_TAG \
  --wait
```

This installs the whole data plane, agent included — the stack's workloads are the
chart's, not the control plane's. `values-byoc.yaml` goes **last**: it is what
collapses the release into one self-contained namespace, and an earlier preset
otherwise wins.

**One pull credential.** Every image a BYOC release pulls privately names
`quay.io/yugabyte`: the stack per component, set in `values-byoc.yaml`, and the
Foundation's yugaware and DB images. (The meko-ui proxy sidecar is `nginx:1.27-alpine`
from Docker Hub and needs no credential.) So the env preset is no longer what makes them
resolvable, and the namespace needs one `docker-registry` Secret rather than that plus a
rotating ECR token.

That Secret is `quay-registry-creds`, named in `global.imagePullSecrets`, as the
yugaware pull secret in `meko-foundation-cr.yaml`, and as
`yugabyte.Image.pullSecretName` in `values-byoc-bundled-db.yaml` — the chart's list never
reaches a subchart. The install hint creates it. Node IAM is not a substitute here the way it was for ECR on EKS: it
authorizes nothing on quay.io.

**The stack tags now match.** `meko/jenkins` versions every component at one release,
so the three the hint prints carry the same value. They are still printed separately:
the pin can name different tags, and a hint that assumed otherwise would be wrong the
moment it did.

`reconcileAgent.envName` must match the registered environment's id — the slug of the
name you registered. A mismatch, or an `ENV_KIND` that disagrees with how the
environment was registered, is fatal to the agent rather than retried.

Success:

```
reconcile-agent starting: kind=meko env=meko-dev ycp=http://…
driving the meko api-server at http://meko-api-server-service.meko-dp.svc.cluster.local.:80/api/v1
enrolled env=meko-dev id=meko-dev uuid=…
```

and then **nothing**: a tick that converges and reports cleanly is silent, so an idle
log is the healthy state.

Then `GET /api/v1/environments` shows it `state=READY live=true` with real
`node_count` — that combination is the signal the whole chain works: the agent
authenticated, read cluster facts through its RBAC, and reported them back.

## Optional: expose the console on a tailnet

The internal LoadBalancer is reachable only inside the VPC. Where a Tailscale
operator is installed, `apiServer.service.annotations` puts the console on the
tailnet — chart-managed, so it survives upgrades:

```bash
helm upgrade --install ycp charts/meko -n $NS \
  -f charts/meko/values-aws.yaml -f charts/meko/values-dev.yaml -f charts/meko/values-ycp.yaml \
  --set-string apiServer.image.tag=$TAG \
  --set-string apiServer.config.MEKO_BYOC_METRICS_URL=$BYOC_METRICS_URL \
  --set-string apiServer.service.annotations."tailscale\.com/expose"=true \
  --set-string apiServer.service.annotations."tailscale\.com/hostname"=ycp \
  --set-string apiServer.service.annotations."tailscale\.com/tags"=tag:k8s-meko
```

Use `--set-string`: annotation values must be strings, and a bare `=true` becomes a
bool the API server rejects. (The template quotes them anyway, so this is belt and
braces.)

The operator provisions a `ts-<service>-*` proxy in the `tailscale` namespace. Read
the hostname it registered from that pod's state Secret:

```bash
POD=$(kubectl -n tailscale get pods -o name | grep ts-meko-api-server | head -1 | cut -d/ -f2)
kubectl -n tailscale get secret "$POD" -o jsonpath='{.data.device_fqdn}' | base64 -d
```

Nothing about the console is authenticated beyond the admin secret on the fleet
routes, so keep it on the tailnet or an internal LB — never a public Ingress.

That applies with more force under `apiServer.basePath` on a shared domain: browser
storage is scoped per origin, not per path, so the admin secret the console holds is
readable by every other page served from that host. The console keeps it in
`sessionStorage` (tab-lifetime, off disk) rather than `localStorage`, which narrows
the window without closing it. Treat the secret as exposed to the whole domain until
the fleet routes carry real per-user auth.

## Tear down

```bash
helm uninstall meko -n $ANS && kubectl delete namespace $ANS
helm uninstall ycp  -n $NS  && kubectl delete namespace $NS
```

The Tailscale operator reaps its proxy once the annotated Service goes. Deleting the
data-plane namespace destroys the stack's PVCs — including the YugabyteDB holding
every datapack in it — and deleting the control-plane namespace destroys the
`ycp-postgres` PVC and with it the fleet registry. There is no other copy of either.

**Uninstalling without deleting the namespace, then reinstalling, is the one case
where Option A can bite.** A StatefulSet's PVC outlives both the release and the
Secret, and Postgres keeps whatever password it was initialised with in its data
directory. Reinstall with a freshly generated `dbPassword` and the database still
wants the old one, so api-server authenticates against it and fails — with a healthy
looking Postgres pod. Reuse the same `dbPassword`, or delete the PVC
(`kubectl -n $NS delete pvc data-ycp-postgres-0`) and start clean.

**Delete the namespaces, not just the releases.** The agent's credential is the
operator-created `meko-agent-api-key` Secret, and Helm does not own it —
`helm uninstall` alone leaves it in the cluster, and the control plane keeps
honouring the identity. Deregistering the environment
(`DELETE /api/v1/environments/{id}`) is the other half: it drops the binding and
returns the `api_key_id` so the key can be revoked in YBM.

## Troubleshooting

**Pod crash-loops with `flag provided but not defined: -control-plane-mode`** — the
api-server image predates the flag. Same for `-only-controlplane-migrate` on the
migrate container. Check the image as in Prerequisites.

**`migrate` init container fails with `current transaction is expired or aborted …
multi-statement queries issued via the simple query protocol`** — only on a control
plane pointed at an external YugabyteDB; the bundled `controlPlaneDb` is Postgres and
never hits this. golang-migrate sends a migration file as one simple-query batch, so
the server runs it as a single implicit transaction, and YSQL cannot retry a statement
inside one (yugabyte-db#21833) — a backfill touching rows an agent is heartbeating
loses the race. The pod sits in `Init:CrashLoopBackOff`; `kubectl -n $NS logs
deploy/meko-api-server -c migrate` has the statement.

The failed attempt is recorded **dirty**, so once the migration itself is fixed every
later boot still fails, now with `Dirty database version N`. Nothing clears that on
its own. Read the version, reset it to the one below, restart:

```bash
psql "$SYSTEM_DB_URL" -c \
  'SELECT version, dirty FROM meko_system.schema_migrations_controlplane'
psql "$SYSTEM_DB_URL" <<'SQL'
DELETE FROM meko_system.schema_migrations_controlplane;
INSERT INTO meko_system.schema_migrations_controlplane (version, dirty) VALUES (<N-1>, false);
SQL
kubectl -n $NS rollout restart deploy/meko-api-server
```

The table holds a **single row** — golang-migrate truncates and re-inserts it at
every step, so it is a current-state pointer and the `DELETE` loses no history.
Re-applying from there is safe: every statement in the set is guarded (`IF NOT
EXISTS`, and the state CHECK is added only when absent), so the versions that did
land are no-ops the second time.

A data plane can wedge the same way, but do not copy the statement above verbatim: only
the control-plane set tracks inside `meko_system`. The system set uses
`schema_migrations_system` and the data set plain `schema_migrations`, both wherever
the search path puts them (`getPostgresConfig` in `internal/db/connect.go`).

The data set recovers one case by itself: a dirty **version 1** is cleared at the next
start and migration 1 is rerun, since 000001 is written to tolerate that (MEKO-831). A
dirty version above 1, and the system and control-plane sets, still need the manual
reset above, with the table and version that apply.

**`ycp-postgres-0` stuck `Pending`** — the storage class does not exist. The install
now refuses up front when the class is missing and lists the ones the cluster has, so
this should only appear on a release created before that check: `kubectl get sc` and
pass a real one via `controlPlaneDb.storage.storageClass`.

**Helm refuses to install: "invalid ownership metadata"** — Helm will not adopt an
object it did not create. Two causes: the namespace already runs something with the
same object names, almost always a data-plane `meko-api-server` in `meko-system` (use
a different namespace); or you turned on `controlPlaneSecrets` for a release whose
Secrets were made with `kubectl create secret` (delete those first — and the DB one
only while the database's password can still be reset, i.e. before its volume
exists).

**Fleet endpoints return 503** — the admin secret is empty server-side. Note the
difference from a *missing* Secret: the Deployment's `secretKeyRef` is not optional,
so a Secret that does not exist leaves the pod in `CreateContainerConfigError` and
nothing serves at all. 503 means the Secret exists and its `admin-api-key` is blank.
That is why `controlPlaneSecrets` renders nothing for an empty value instead of
rendering an empty Secret — a pod that will not start is easier to diagnose than a
console that loads and then refuses every call.

**Fleet endpoints return 401** — wrong admin secret in the request.

**Agent logs `ENROLL REJECTED (401)` repeatedly** — no environment is bound to the
agent's key id: the key was revoked, or the `meko-agent-api-key` Secret carries the
wrong one. Update the Secret with the correct id and restart the agent -- an
environment cannot be rebound to a different key once registered, so a revoked key
means registering a new environment for the replacement.

**Agent logs `enroll -> 500`** — a server-side error; `kubectl logs
deploy/meko-api-server` shows the cause on an `enroll failed:` line.

**Agent exits on `enroll: environment kind mismatch`** — `ENV_KIND` disagrees with
the kind the environment was registered as. Fatal by design; register a new
environment with the right kind, or point the agent at the matching one.

**Agent reports a resource `drift`, `rejected` or `blocked` and never retries** —
the api-server refused it for a reason retrying cannot change, and the state says
which. `rejected` is your declaration: a name it will not take (400) or a conflict
(409) — rewrite it, and the row's `error` says what it wants. `blocked` is a revoked
or unscoped token (401/403), or the free-tier cap (402), whose counter is lifetime
and is never refunded on delete — no rewrite clears either. `drift` is a verb that
does not exist, so nothing you declare changes it.

**`kubectl exec` into the agent gives no shell** — it is
`gcr.io/distroless/static:nonroot`. Use logs and port-forward. (The api-server image
is Alpine and does have one.)
