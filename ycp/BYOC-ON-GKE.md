# Standing up a BYOC data plane on GKE, start to finish

One environment, on a real cluster, from nothing. The whole Meko stack — YugabyteDB with
the `mage` and `pg_dist_rag` extensions, then `api-server`, `mcp-server`, `rag-worker`,
`inference-gateway` and `ui` — comes up from the `meko` chart, with a reconcile-agent
enrolled beside it that converges the agents and datapacks the environment declares.
No `kubectl apply`.

This is [`GETTING_STARTED.md`](./GETTING_STARTED.md)'s level 2.9 written out with
everything that bit during a real run. For what the pieces are, read
[`ARCHITECTURE.md`](./ARCHITECTURE.md); for the spec field reference,
[`DEPLOY.md`](./DEPLOY.md).

Verified 2026-08-03 on `gke_yugabyte_us-west1_itest-mcs`.

---

## What you need

| | |
|---|---|
| A cluster | `kubectl config get-contexts`. The nodes are **amd64** — that matters below. |
| `AWS_PROFILE` | A profile with Meko ECR read access; `meko-dev` here. Every image in the release is private. |
| `MEKO_DEVOPS` | A [meko-devops](https://github.com/yugabyte/meko-devops) checkout for the `meko` chart. Defaults to `~/code/meko-devops`. |
| Tools | `kubectl`, `helm`, `podman` (or docker), `curl`, `python3`, `aws`. |
| `OPENAI_API_KEY` | Optional, but see [the gateway](#the-gateway-needs-a-key-or-it-does-not-start). |

GKE auth expires and cannot be refreshed non-interactively — if `kubectl` starts failing
with `Reauthentication failed`, run `gcloud auth login`.

## 1. Build and push the api-server image

```bash
cd ~/code/meko
export AWS_PROFILE=meko-dev
TAG="mekostack-$(git rev-parse --short HEAD)"
ECR=161908593886.dkr.ecr.us-east-2.amazonaws.com

podman build --platform linux/amd64 -f meko/api_server/Dockerfile -t "$ECR/meko/meko-api-server:$TAG" meko/api_server

# Check the architecture before pushing. An arm64 image pulls cleanly and then dies with
# `exec format error`, which reads like a broken build rather than a wrong platform.
podman inspect "$ECR/meko/meko-api-server:$TAG" --format "{{.Os}}/{{.Architecture}}"

aws ecr get-login-password --region us-east-2 |
  podman login --username AWS --password-stdin "$ECR"
podman push "$ECR/meko/meko-api-server:$TAG"
```

The **reconcile-agent** is not built here: it publishes to Quay
(`ycp-reconciler-agent-image.yml`), and `ycp/build-reconciler-agent-image.sh` is the
local equivalent. The script pulls whatever tag `reconcileAgent.image.tag` names.

`--platform linux/amd64` is not optional from an Apple-silicon laptop. The Dockerfiles
cross-compile, so this builds natively rather than emulating the Go toolchain.

The tag names the *code*. Rebuild and re-push after changing anything Go, or you will
deploy the previous build and spend a while wondering why a fix had no effect.

## 2. Install both halves

```bash
cd ~/code/meko/ycp
AWS_PROFILE=meko-dev ./test/local/ycp-on-cluster.sh \
  --context gke_yugabyte_us-west1_itest-mcs \
  --all --env-name itest-mcs \
  --ycp-ns ycp-anijhawan --meko-ns itest-mcs \
  --storage-class standard-rwo \
  --tag "$TAG"
```

Four flags worth understanding rather than copying:

- **`--all`** is required. The script refuses to guess a stage, because installing a data
  plane means registering an environment and attaching an agent to it — two deliberate
  acts. `--ycp-only` brings up just the control plane; `--agent-only` attaches to one that
  is already there.
- **`--storage-class`** is needed wherever the cloud preset's default is absent. The `gcp`
  preset asks for `pd-ssd`; `itest-mcs` has `standard-rwo` and `premium-rwo`, so the
  control-plane install fails with the class it wanted and the ones it found. Check with
  `kubectl get storageclass`.
- **`--ycp-ns` / `--meko-ns`** name the two namespaces. The script only ever touches
  namespaces it created (it labels them), so it cannot walk over someone else's release on
  a shared cluster.
- **`--env-name`** is the environment id. It is the slug of the name, so `itest-mcs` stays
  `itest-mcs`. Left unset, the derived default carries a run timestamp so repeat runs do
  not collide.

**Re-running refreshes the ECR token.** Registration itself is not repeatable — see
[re-running](#re-running-and-registration).

It takes about 4 minutes on a warm node and prints, at the end, the admin secret, the
addresses, and a `curl` that replaces the environment's desired state.

## 3. What you should see

```
==> checking the fleet API reports the stack healthy
    state=PROVISIONING
      yugabytedb         1/1 2026.1.0.0-b53
      api-server         1/1 console-943437724
      mcp-server         1/1 2.1.0-20260731-7b3fdd3
      rag-worker         1/1 1.0.7-20260703-2669cba
      inference-gateway  0/1 0.1.0-20260730-ae46721
      ui                 1/1 1.0.0-20260729-bf56db7
==> PASS
```

`PROVISIONING` rather than `READY` is **correct** without an `OPENAI_API_KEY`: `READY`
means every declared component is up, and the gateway cannot start without a key.

Check the things that are easy to assume:

```bash
C=gke_yugabyte_us-west1_itest-mcs
NS=itest-mcs

# The extensions, which are the point of the MAGE build.
PW=$(kubectl --context $C -n $NS get secret meko-database -o jsonpath='{.data.password}' | base64 -d)
kubectl --context $C -n $NS exec meko-yugabytedb-0 -- env PGPASSWORD="$PW" bash -c \
  'bin/ysqlsh -h $(hostname) -p 5433 -U yugabyte -d meko -tAc "select extname from pg_extension order by extname"'
# mage  pg_dist_rag  pg_stat_statements  plpgsql  vector

# The api-server's schema. A 200 from /api/v1/health does NOT prove this.
kubectl --context $C -n $NS logs deploy/meko-api-server -c migrate | tail -3
# "Migrated data schema to version 3" / "Migrated system schema to version 9"

# The reconcile loop. A tick that converges and reports cleanly is silent, so an
# idle log after "enrolled" is the healthy state.
kubectl --context $C -n $NS logs -f deploy/meko-reconcile-agent
```

## 4. Reach it

Both halves get an **internal** LoadBalancer — a private address, reachable over the
corporate VPN. Read the addresses back rather than trusting any written down: they are
reallocated whenever a Service is recreated.

```bash
kubectl --context $C -n ycp-anijhawan get svc meko-api-server-lb \
  -o jsonpath='console: http://{.status.loadBalancer.ingress[0].ip}{"\n"}'
kubectl --context $C -n $NS get svc meko-ui \
  -o jsonpath='ui:      http://{.status.loadBalancer.ingress[0].ip}:5000{"\n"}'
```

The console needs the admin secret (`X-Meko-Admin-Secret`), which the script prints and
which also lives in the cluster:

```bash
kubectl --context $C -n ycp-anijhawan get secret meko-admin-api-key \
  -o jsonpath='{.data.admin-api-key}' | base64 -d
```

The UI's Service type is the chart's: set `mekoUi.service.type=LoadBalancer` (and the
internal-LB annotation for your cloud) at install time. Off the VPN, port-forward
instead — that needs no exposure at all:

```bash
kubectl --context $C -n $NS port-forward svc/meko-ui 5000:5000
```

## 5. Drive it

Registering binds the environment to an API key and gives you the install hint.
Desired state is the control surface afterwards, and it is replaced rather than merged:

```bash
KEY=$(kubectl --context $C -n ycp-anijhawan get secret meko-admin-api-key -o jsonpath='{.data.admin-api-key}' | base64 -d)
YCP=http://$(kubectl --context $C -n ycp-anijhawan get svc meko-api-server-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

# Declare an agent and two datapacks under it.
curl -s -X POST -H "X-Meko-Admin-Secret: $KEY" -H 'content-type: application/json' \
  -d '{"resources":[
        {"kind":"agent","name":"itest"},
        {"kind":"datapack","name":"itest/team-a","spec":{"description":"team A"}},
        {"kind":"datapack","name":"itest/team-b","spec":{"description":"team B"}}
      ]}' "$YCP/api/v1/environments/itest-mcs/desired"

# Withdraw everything. The agent collects both datapacks and then the agent on its
# next tick — deleting an agent cascades its datapacks first, because the api-server's
# own delete would strand them.
curl -s -X POST -H "X-Meko-Admin-Secret: $KEY" -H 'content-type: application/json' \
  -d '{"resources":[]}' "$YCP/api/v1/environments/itest-mcs/desired"
```

**The agent is authoritative over this api-server.** A datapack created by hand that
desired state does not declare is collected on the next tick — the signup agent
(`meko_agent`) and anything attributed to it are the sole exception, excluded from the
model entirely because they carry the account's share and invite grants.

The stack's own storage is untouched by any of this: the workloads and their
PersistentVolumeClaims belong to the Helm release, so withdrawing desired state
removes datapacks, not the database holding them.

## Teardown

```bash
./test/local/ycp-on-cluster.sh --context $C \
  --ycp-ns ycp-anijhawan --meko-ns itest-mcs --all --delete
```

`--delete` alone removes only the data plane; `--all --delete` takes the control plane
too. It refuses to delete a namespace it did not create.

---

## Things that will bite

### The gateway needs a key or it does not start

`inference-gateway` builds its LangChain client while the app is being constructed, so a
missing `OPENAI_API_KEY` is fatal rather than degrading: the process exits with
`openai.OpenAIError: Missing credentials` and the component reports 0/1 forever. The MCP
server is laxer about the same key — it starts and fails on the first embedding.

```bash
kubectl --context $C -n $NS create secret generic meko-data-plane \
  --from-literal=OPENAI_API_KEY=sk-… --dry-run=client -o yaml | kubectl --context $C apply -f -
kubectl --context $C -n $NS rollout restart deploy/meko-inference-gateway
```

Or leave it out and drop the component: `--set inferenceGateway.enabled=false`.

### Healthy is not the same as working

The api-server answering `/api/v1/health` with 200 proves it can reach its database, and
nothing more. **`api_server` does not migrate on boot** — it migrates only when
`-migrate-only` is passed. The chart gives it a `migrate` init container for exactly this
reason; without one the stack comes up entirely green and every real request fails with
`relation "meko_system.users" does not exist`. If the UI loads and then 401s with
`dev account setup failed`, check that init container.

### The ECR token expires in ~12 hours

Running pods are fine — their images are cached on the node — but any eviction or restart
after that lands in `ImagePullBackOff`. Re-run the script to refresh
`ecr-registry-creds`; it does nothing else destructive.

### Re-running and registration

Registration is INSERT-only. A second run against the same `--env-name` is refused with
409 `ENVIRONMENT_EXISTS`, and the name stays claimed for the life of the row whether or
not the environment was later deregistered. Nothing is rebound, so an agent already
attached to that environment keeps its identity and needs no restart. Use `--agent-only`
to bring an agent back up against an environment that is already registered; to stand up
a separate one, pass a name that is not taken.

### A StatefulSet blocks its own replacement

A pod that can never become Ready — `InvalidImageName`, `ImagePullBackOff` — stops the
rolling update that would replace it. A corrected image tag therefore does **not**
self-heal on `helm upgrade`; delete the pod:

```bash
kubectl --context $C -n $NS delete pod meko-yugabytedb-0
```

Deployments do not have this problem. Only the database is a StatefulSet.

### A half-initialised yugabyted cannot be restarted out of

`yugabyted` applies `YSQL_PASSWORD` only on first install. If its first start dies
part-way — a full node is the one seen in practice, failing inside
`first_install_init_auth` with `insufficient disk space` — auth ends up on with the
default password while everything else uses the generated one, and every restart repeats
it. The symptom is `password authentication failed for user "yugabyte"` from a database
that is plainly up. Check `kubectl logs <db-pod> --previous`, then discard the volume,
which **destroys its data**:

```bash
kubectl --context $C -n $NS delete pvc data-meko-yugabytedb-0 --wait=false
kubectl --context $C -n $NS delete pod meko-yugabytedb-0
```

### Placeholders in a copied spec reach the pod

An image reference is applied as written. A snippet pasted with `…`, `<registry>` or
`<tag>` still in it produces `InvalidImageName`, and the reason lives in pod status rather
than anywhere you are looking. Registration's install hint prints the three stack tags as
shell variables for this reason — fill them in rather than editing mid-command.

---

## Security posture of this setup

It is a **dev** posture, and worth being explicit about before anyone points anything
public at it:

- **The data-plane api-server does not authenticate.** With the Cognito settings unset it
  logs `auth disabled – using dev account for all requests` and serves every caller as a
  dev account. Configure `COGNITO_REGION` / `COGNITO_POOL_ID` / `COGNITO_CLIENT_ID` /
  `COGNITO_CLIENT_SECRET` / `COGNITO_DOMAIN` in the `meko-data-plane` Secret to change
  that.
- **So a public LoadBalancer on it puts an unauthenticated Meko, and a live YugabyteDB,
  on the internet.** Use an internal LB (VPC-only, what the control plane itself uses) or
  port-forward.
- **The console has no auth of its own** beyond the admin secret, and that secret is a
  fleet-admin credential. `--console-embed-admin-secret` writes it into the page so the
  console stops asking — reasonable where the console is already the trust boundary
  (internal LoadBalancer, tailnet), wrong anywhere a browser can wander in.
- **The UI's Cognito settings are baked into its image at build time** (`VITE_COGNITO_*`),
  so a data plane authenticates against whatever pool its image was built for. Only the
  runtime allow-list in `meko_ui/server/index.ts` is settable from here.
