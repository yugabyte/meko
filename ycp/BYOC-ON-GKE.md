# Standing up a BYOC data plane on GKE, start to finish

One environment, on a real cluster, from nothing. The whole Meko stack — YugabyteDB with
the `mage` and `pg_dist_rag` extensions, then `api-server`, `mcp-server`, `rag-worker`,
`inference-gateway` and `ui` — comes up because an environment was **registered**. No
`kubectl apply`, and no desired state POSTed by hand.

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

## 1. Build and push both images

```bash
cd ~/code/meko
export AWS_PROFILE=meko-dev
TAG="mekostack-$(git rev-parse --short HEAD)"
ECR=161908593886.dkr.ecr.us-east-2.amazonaws.com

podman build --platform linux/amd64 -f meko/api_server/Dockerfile -t "$ECR/meko/meko-api-server:$TAG" meko/api_server
podman build --platform linux/amd64 -f ycp/Dockerfile.agent  -t "$ECR/meko/meko-agent:$TAG"      ycp

# Check the architecture before pushing. An arm64 image pulls cleanly and then dies with
# `exec format error`, which reads like a broken build rather than a wrong platform.
for n in meko-api-server meko-agent; do
  podman inspect "$ECR/meko/$n:$TAG" --format "$n: {{.Os}}/{{.Architecture}}"
done

aws ecr get-login-password --region us-east-2 |
  podman login --username AWS --password-stdin "$ECR"
for n in meko-api-server meko-agent; do podman push "$ECR/meko/$n:$TAG"; done
```

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
  `itest-mcs`.

**Re-running is safe.** It refreshes the ECR token, re-registers, and restarts the agent so
it picks up the freshly minted enrollment token. That last part is load-bearing — see
[re-running](#re-running-and-why-the-agent-has-to-restart).

It takes about 4 minutes on a warm node and prints, at the end, the admin secret, the
addresses, and a `curl` that replaces the environment's desired state.

## 3. What you should see

```
==> checking the fleet API reports the stack healthy
    state=PROVISIONING
      meko-agent         1/1 v1.4.2
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

# What the agent provisioned, and the loop.
kubectl --context $C -n $NS get all,cm -l meko.dev/stack=true
kubectl --context $C -n $NS logs -f deploy/meko-agent
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

The UI does **not** get a LoadBalancer by default. Ask for one in desired state:

```bash
# expose: {"ui": "internal"} — VPC-only. "external" is a public address, and see the
# warning below before reaching for it.
KEY=$(kubectl --context $C -n ycp-anijhawan get secret meko-admin-api-key -o jsonpath='{.data.admin-api-key}' | base64 -d)
YCP=http://$(kubectl --context $C -n ycp-anijhawan get svc meko-api-server-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

# Desired state is REPLACED, not merged: read it back, edit, POST the whole set.
curl -s -H "X-Meko-Admin-Secret: $KEY" "$YCP/api/v1/environments/itest-mcs/desired" |
  python3 -c 'import json,sys
r = json.load(sys.stdin)["data"]["resources"]
next(x for x in r if x["kind"] == "meko-stack")["spec"]["expose"] = {"ui": "internal"}
print(json.dumps({"resources": r}))' |
  curl -s -X POST -H "X-Meko-Admin-Secret: $KEY" -H 'content-type: application/json' \
    --data-binary @- "$YCP/api/v1/environments/itest-mcs/desired"
```

Off the VPN, port-forward instead — that needs no exposure at all:

```bash
kubectl --context $C -n $NS get svc meko-ui  # no LoadBalancer needed for this
kubectl --context $C -n $NS port-forward svc/meko-ui 5000:5000
```

## 5. Drive it

Registering is the only step the API *requires*. Afterwards, desired state is the control
surface, and it is replaced rather than merged:

```bash
# Withdraw everything. The agent garbage-collects the stack on its next tick, and the
# environment goes back to READY with just the agent.
curl -s -X POST -H "X-Meko-Admin-Secret: $KEY" -H 'content-type: application/json' \
  -d '{"resources":[]}' "$YCP/api/v1/environments/itest-mcs/desired"
```

Two things the GC deliberately leaves behind, because destroying them is an operator's
decision and not a side effect of an edit:

- the database's **PersistentVolumeClaim** (`data-meko-yugabytedb-0`),
- the Secrets the agent generated (`meko-database`, `meko-mcp-server-db`,
  `meko-rag-worker-db`). Leave them and a fresh stack reuses the old password; delete them
  and it mints a new one.

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

Or leave it out and drop the component, which reaches `READY` honestly:
`"disable": ["inference-gateway"]` in the stack spec.

### Healthy is not the same as working

The api-server answering `/api/v1/health` with 200 proves it can reach its database, and
nothing more. **`api_server` does not migrate on boot** — it migrates only when
`-migrate-only` is passed. The agent gives it a `migrate` init container for exactly this
reason; without one the stack comes up entirely green and every real request fails with
`relation "meko_system.users" does not exist`. If the UI loads and then 401s with
`dev account setup failed`, check that init container.

### The ECR token expires in ~12 hours

Running pods are fine — their images are cached on the node — but any eviction or restart
after that lands in `ImagePullBackOff`. Re-run the script to refresh
`ecr-registry-creds`; it does nothing else destructive.

### Re-running, and why the agent has to restart

Registering an environment is an **upsert**: it mints a fresh enrollment token and clears
the stored status-token hash. So a second run invalidates the credentials the running
agent holds. The Deployment spec is unchanged, so `helm upgrade` restarts nothing, and a
`secretKeyRef` env var is read once at start — leaving the agent 401ing on every call with
a token that no longer matches, its re-enrol failing silently.

The script now deletes `meko-agent-state` and restarts the agent after installing, so this
is handled. Worth knowing anyway, because there is **no endpoint to re-issue an enrollment
token**: an agent that gets into this state can only be recovered by deregistering and
re-registering its environment (tracked in
[meko#401](https://github.com/yugabyte/meko/issues/401)).

### A StatefulSet blocks its own replacement

A pod that can never become Ready — `InvalidImageName`, `ImagePullBackOff` — stops the
rolling update that would replace it. Correcting `images.database` therefore does **not**
self-heal; delete the pod:

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
than anywhere you are looking. The agent refuses unparseable references now and names the
field, but write them out in full.

---

## Security posture of this setup

It is a **dev** posture, and worth being explicit about before anyone points anything
public at it:

- **The data-plane api-server does not authenticate.** With the Cognito settings unset it
  logs `auth disabled – using dev account for all requests` and serves every caller as a
  dev account. Configure `COGNITO_REGION` / `COGNITO_POOL_ID` / `COGNITO_CLIENT_ID` /
  `COGNITO_CLIENT_SECRET` / `COGNITO_DOMAIN` in the `meko-data-plane` Secret to change
  that.
- **So `expose: external` puts an unauthenticated Meko, and a live YugabyteDB, on the
  internet.** `internal` is VPC-only and is what the control plane itself uses. Prefer it.
- **The console has no auth of its own** beyond the admin secret, and that secret is a
  fleet-admin credential. `--console-embed-admin-secret` writes it into the page so the
  console stops asking — reasonable where the console is already the trust boundary
  (internal LoadBalancer, tailnet), wrong anywhere a browser can wander in.
- **The UI's Cognito settings are baked into its image at build time** (`VITE_COGNITO_*`),
  so a data plane authenticates against whatever pool its image was built for. Only the
  runtime allow-list in `meko_ui/server/index.ts` is settable from here.
