# Deploying YCP from scratch

A runbook for someone starting with nothing: no cluster, no registry, no
credentials. It covers what you actually need, in dependency order, and the failure
modes that are easy to hit.

> **What YCP is.** The control plane is `api_server` run with
> `--control-plane-mode`; the data plane runs `meko-agent` from this directory.
> Both are deployed by the `meko` Helm chart at
> [`meko/deploy/helm/meko`](../meko/deploy/helm/meko).
>
> This document gets you to the point where that chart can work: code that builds,
> images in a registry, and a cluster allowed to pull them. The install itself is
> [`meko/deploy/helm/meko/YCP-RUNBOOK.md`](../meko/deploy/helm/meko/YCP-RUNBOOK.md),
> which is written from a real bring-up and picks up exactly where this leaves off.

- For what the pieces *are*, read [`ARCHITECTURE.md`](./ARCHITECTURE.md) first.
- For per-flag/env reference, see [`DEPLOY.md`](./DEPLOY.md). This document is the
  "start here" path; DEPLOY.md is the reference.

---

## 0. Pick how far you need to go

Each level is a superset of the one above it. Don't set up a registry to answer
"does this thing work" — level 1 answers that in one command.

| Level | What it proves | Needs | Time |
|-------|----------------|-------|------|
| **1 — laptop** | It compiles and the unit tests pass | Go 1.25+ | ~1 min |
| **2 — throwaway cluster** | Images build; the control plane migrates, serves and enrolls an agent; the agent survives a restart; a commanded environment brings up what it is told to | + `kind`, `kubectl`, `curl`, `python3`, and docker **or** podman | ~5 min |
| **2.5 — the real stack, locally** | The same, with the private `meko-mcp-server` image customers run | + AWS credentials for the Meko ECR | ~15 min (image pull) |
| **2.75 — the operator path** | Register in YCP, run the Helm command YCP prints, and Meko comes up on its own | + `helm`, a meko-devops checkout | ~15 min |
| **2.9 — on a real cluster** | The same, two namespaces on a cluster you already have (`itest-dev`, `meko-dev-eks`), images pulled from ECR | + a cluster and ECR push access | ~20 min |
| **3 — a real cluster** | A shared, reachable YCP other people can use | + a registry, image publishing, a cluster | ~1 hr first time |

## Level 1 — on your laptop

```bash
cd ycp && go build ./... && go test ./...
```

There is nothing to `go run` here: the agent needs an in-cluster Kubernetes API to
read facts from, and the control plane lives in `api_server/`. To run the control
plane against a local Postgres, see the `api_server` dev loop in the root
`AGENTS.md` and add `--control-plane-mode`.

## Level 2 — a throwaway cluster, no registry needed

This is the fastest honest check, and it is the same thing CI runs:

```bash
cd ycp
./test/e2e/run.sh
```

It builds the api-server and agent images from source, loads them into a `kind`
cluster it creates, and drives the whole bring-up: the control-plane migrations run
against a plain Postgres, `/api/v1/health` comes up, the fleet API refuses an
unauthenticated call, an environment is registered through the real API bound to an
`api_key_id`, the agent checks in with that identity, reaches `READY` with real
cluster facts, and **restarts and checks in again**. Then it *commands* the
environment — one POST of desired state — and asserts the agent acts on it: it
brings up a second declared database, reports it back as a component, and
garbage-collects it when withdrawn.
Then it deletes the cluster (`YCP_E2E_KEEP=1` to keep it and poke around).

Everything it runs uses public images, so it needs no registry credentials. That is
also its limit: it never starts `meko-mcp-server`, which is private. For that, see
level 2.5.

It pins every `kubectl` call to the `kind-ycp-e2e` context and refuses to run if
that context isn't the cluster it just created, so it cannot touch whatever your
kubeconfig currently points at.

If this passes, the code and manifests are fine and anything that fails at level 3
is environmental.

## Level 2.5 — the real Meko stack, locally

Level 2 proves the loop with images built from this repo. This proves it with the
images customers actually run — `meko-mcp-server` over a database the agent
provisions — on the same throwaway cluster:

```bash
YCP_E2E_KEEP=1 ./test/e2e/run.sh          # leaves the cluster up
AWS_PROFILE=meko-dev ./test/local/meko-stack.sh
```

It creates an ECR pull Secret from your AWS credentials, writes `OPENAI_API_KEY`
into a data-plane Secret if you have one exported, then declares the stack as
desired state and waits for it: a `database` and an `mcp-server`. Add
`MEKO_RAG_IMAGE=…` to declare a `rag-worker` too. When it finishes it prints the
port-forwards for the console and the MCP endpoint.

Nothing in the script deploys anything itself — it makes one `POST` of desired
state and watches the agent do the work, which is the point.

Two things to know:

- **The MCP image is large** (~11 GB uncompressed), so the first pull dominates
  the run. It is pulled by the node from ECR rather than side-loaded, because
  `kind load` would write it twice.
- **The ECR token lasts ~12 hours.** A cluster kept longer needs the script
  re-run (it only refreshes the Secret) before the next image pull.

Without AWS credentials this level is not available, and level 2 is the honest
check — see [`ARCHITECTURE.md`](./ARCHITECTURE.md#what-the-agent-runs)
for what else can be declared.

## Level 2.6 — the whole stack, from a registration and nothing else

Level 2.5 *commands* a stack, the way an operator used to have to. This one registers
an environment with a name and lets enrollment do the rest, which is the path a
customer actually takes:

```bash
YCP_E2E_KEEP=1 ./test/e2e/run.sh          # leaves the cluster up
AWS_PROFILE=meko-dev OPENAI_API_KEY=sk-… ./test/local/full-stack.sh
```

No desired state is POSTed anywhere in it. It patches the control plane with the two
data-plane Secret names, registers a fresh `full-stack-<timestamp>` environment (a
new name every run, since registering over an existing one is refused), deploys a
second agent into its own namespace, and waits while the agent brings up YugabyteDB
with `mage` and `pg_dist_rag`, then api-server, mcp-server, rag-worker,
inference-gateway and ui.

It asserts the things that would otherwise be believed rather than checked: that
registration seeded all six images, that the agent was *handed* a stack at enrollment
and persisted it (so a restart mid-outage needs no control-plane call), that both
extensions and the `dist_rag` schema really exist in the `meko` database, and that
every component reports healthy to the fleet API.

Beyond level 2.5's two caveats (large image, 12-hour ECR token):

- **`OPENAI_API_KEY` is not optional for the gateway.** It builds its LangChain client
  at startup, so without a key it exits rather than degrades. The script says so and
  drops it from the assertions rather than reporting a red component for missing
  configuration — but then the environment stays `PROVISIONING`, correctly, because a
  declared component is not running.
- **It needs real capacity.** YugabyteDB alone requests 2Gi.

## Level 2.75 — the operator path, end to end

Levels 2 and 2.5 drive the API directly. This one does what an operator does and
nothing else: stand up YCP, register an environment in it, and run the Helm
command YCP printed. The agent enrolls itself and brings Meko up from the desired
state registration seeded — no `kubectl apply`, no POST of desired state.

```bash
AWS_PROFILE=meko-dev ./test/local/ycp-helm-bootstrap.sh
```

It needs a **meko-devops checkout** for the chart (`MEKO_DEVOPS=…`, default
`~/code/meko-devops`), because that is where the `meko` chart lives. It creates
its own kind cluster (`ycp-helm`), so it does not disturb the e2e's.

It also tests the install hint, which is the one instruction an operator is given:
a hint that has drifted from the chart is not cosmetic, it is a support question.

What it proves, in order:

```
--------- the install command YCP printed (install_hint) ---------
...
    seeded at registration: database/meko, mcp-server/mcp
==> helm upgrade --install meko-agent (chart: .../charts/meko)
    enrolled: environment_id=helm-cell
==> waiting for the database the agent was never told to run
    /ping -> {"status":"ok"}
    /mcp initialize -> Meko MCP 3.2.4
    state=READY components: meko-agent 1/1, postgres 1/1, mcp-server 1/1
```

The chart needs the widened agent RBAC (`patch`, `statefulsets`, `configmaps`) —
without it the agent enrolls and then 403s on everything it tries to reconcile.

## Level 2.9 — the same flow on a real cluster, two namespaces

No kind. One namespace for the control plane, one for the data plane, on a cluster
you already have — `itest-dev`, `itest-mcs`, `meko-dev-eks`:

```bash
# once per code change: build amd64 images and push them to ECR
export AWS_PROFILE=meko-dev
TAG="bootstrap-$(git rev-parse --short HEAD)"
ECR=161908593886.dkr.ecr.us-east-2.amazonaws.com
for spec in meko/api_server/Dockerfile:meko/api_server:meko-api-server \
            ycp/Dockerfile.agent:ycp:meko-agent; do
  df="${spec%%:*}"; r="${spec#*:}"; ctx="${r%%:*}"; name="${r#*:}"
  podman build --platform linux/amd64 -f "$df" -t "$ECR/meko/$name:$TAG" "$ctx"
  podman push "$ECR/meko/$name:$TAG"
done

# then install both halves. --all is required: the script refuses to guess a stage,
# because registering an environment and attaching an agent to it are two deliberate
# acts. --storage-class is needed wherever the cloud preset's default is absent (the
# gcp preset asks for pd-ssd; check `kubectl get storageclass`).
./test/local/ycp-on-cluster.sh \
  --context gke_yugabyte_us-west1_itest-dev \
  --all --env-name itest-dev \
  --ycp-ns ycp --meko-ns meko \
  --storage-class standard-rwo \
  --tag "$TAG"

# and afterwards — --delete alone removes the data plane only
./test/local/ycp-on-cluster.sh --context <ctx> --ycp-ns ycp --meko-ns meko --all --delete
```

**[`BYOC-ON-GKE.md`](./BYOC-ON-GKE.md) is this level written out end to end**, with the
verification steps and everything that bit on a real run — the migrate step, the internal
LoadBalancers, re-runnability, and the failure modes worth recognising.

`--platform linux/amd64` is not optional from an Apple-silicon laptop: the nodes
are amd64 and an arm64 image fails with `exec format error` after a pull that looks
successful. The Dockerfiles cross-compile, so this builds natively rather than
running the Go toolchain under emulation.

Three things differ from kind, all of them the cluster's doing rather than YCP's:

- **Images come from ECR**, so they must be pushed first. The tag names the *code*;
  rebuild it after changing anything Go.
- **Both namespaces need an ECR pull Secret** (`ecr-registry-creds`), because GKE
  has no AWS IAM — including the namespace the *agent* creates workloads in, which
  is why the control plane is told the Secret's name
  (`apiServer.dataplane.imagePullSecret`) and passes it in the seeded desired state.
  ECR tokens last ~12 h; re-running the script refreshes them.
- **The storage class** comes from the cloud preset (`pd-ssd` on GKE) rather than
  kind's local-path provisioner.

The script refuses to install into a namespace it did not create, so it cannot walk
over somebody else's release on a shared cluster. `--delete` removes only what it
made, including the `ycp-dp-*` namespaces the agent created.

---

# Level 3 — a real cluster

Four things have to be true, in this order. Each depends on the one before it.

```
 A. a registry that holds the images
 B. something that publishes images into it
 C. a cluster that is allowed to pull from it
 D. a way to reach the console
```

## A. A registry

Meko uses ECR at `161908593886.dkr.ecr.us-east-2.amazonaws.com` under `meko/`:

| Repository | Binary | Dockerfile | Published by |
|---|---|---|---|
| `meko/meko-agent` | BYOC pull agent | `ycp/Dockerfile.agent` | `ycp-images.yml` |
| `meko/meko-api-server` | the control plane | `meko/api_server/Dockerfile` | **nobody — hand-built** |

**ECR does not create repositories on push** — unlike most registries. Create them
first, once per account:

```bash
for r in meko/meko-agent meko/meko-api-server; do
  aws ecr create-repository --region us-east-2 --repository-name "$r" \
    --image-tag-mutability MUTABLE
done
```

`MUTABLE` is required, not cosmetic: publishing re-pushes the `latest` tag every
time, which `IMMUTABLE` rejects.

## B. Publishing images

[`.github/workflows/ycp-images.yml`](../.github/workflows/ycp-images.yml) builds
and pushes the agent image on every merge to `main` that touches
`ycp/**`, tagging each with the commit SHA and `latest`. PRs build but never push.
You can also trigger it by hand with `gh workflow run ycp-images.yml --ref main`.

It authenticates by assuming an IAM role over GitHub's OIDC provider — no GitHub
secrets, deliberately, because creating those needs repo-admin that component
owners typically don't have, and a role ARN isn't sensitive.

Setting that up in a fresh AWS account is two calls. First register GitHub as an
identity provider (once per account):

```bash
aws iam create-open-id-connect-provider \
  --url https://token.actions.githubusercontent.com \
  --client-id-list sts.amazonaws.com \
  --thumbprint-list 6938fd4d98bab03faadb97b34396831e3780aea1
```

Then the role. Its name must match `DEFAULT_AWS_ROLE_ARN` in the workflow
(`meko-github-actions-ecr-push`), or override that with an `AWS_ROLE_ARN` Actions
variable:

```bash
aws iam create-role --role-name meko-github-actions-ecr-push \
  --assume-role-policy-document file://trust-policy.json
aws iam put-role-policy --role-name meko-github-actions-ecr-push \
  --policy-name ecr-push-ycp --policy-document file://ecr-push-policy.json
```

`trust-policy.json` — scoped so **only this repo's `main` branch** can assume it,
which keeps PR branches and forks out:

```json
{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
 "Principal":{"Federated":"arn:aws:iam::161908593886:oidc-provider/token.actions.githubusercontent.com"},
 "Action":"sts:AssumeRoleWithWebIdentity",
 "Condition":{"StringEquals":{
   "token.actions.githubusercontent.com:aud":"sts.amazonaws.com",
   "token.actions.githubusercontent.com:sub":"repo:yugabyte/meko:ref:refs/heads/main"}}}]}
```

`ecr-push-policy.json` — push rights on those repos and nothing else
(`GetAuthorizationToken` has to be `*`; ECR does not scope it):

```json
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":"ecr:GetAuthorizationToken","Resource":"*"},
 {"Effect":"Allow","Action":["ecr:BatchCheckLayerAvailability","ecr:InitiateLayerUpload",
   "ecr:UploadLayerPart","ecr:CompleteLayerUpload","ecr:PutImage","ecr:BatchGetImage",
   "ecr:DescribeRepositories","ecr:DescribeImages"],
  "Resource":["arn:aws:ecr:us-east-2:161908593886:repository/meko/meko-agent",
              "arn:aws:ecr:us-east-2:161908593886:repository/meko/meko-agent"]}]}
```

Until the role exists the workflow builds the images and **skips the push with a
warning** rather than failing, so wiring this up later never breaks `main`.

The control plane's image has no workflow, so it is built and pushed by hand:

```bash
aws ecr get-login-password --region us-east-2 \
  | docker login --username AWS --password-stdin 161908593886.dkr.ecr.us-east-2.amazonaws.com
R=161908593886.dkr.ecr.us-east-2.amazonaws.com/meko
TAG=$(date +%Y%m%d)
docker build --platform linux/amd64 -f meko/api_server/Dockerfile -t $R/meko-api-server:$TAG meko/api_server
docker push $R/meko-api-server:$TAG
```

Verify the tag carries the control-plane flags before deploying it — an image built
before they landed crash-loops on `flag provided but not defined`:

```bash
docker run --rm --entrypoint /usr/local/bin/meko-api-server $R/meko-api-server:$TAG --help \
  | grep -E 'control-plane-mode|only-controlplane-migrate'
```

Confirm what actually landed in the registry:

```bash
aws ecr describe-images --region us-east-2 --repository-name meko/meko-agent \
  --query 'reverse(sort_by(imageDetails,&imagePushedAt))[0].{tags:imageTags,pushed:imagePushedAt}'
```

## C. A cluster that can pull

The images live in a private registry, so the cluster needs authorization. This
differs by cloud and is the most common thing to get wrong:

| Cluster | What authorizes the pull |
|---|---|
| **EKS in the same AWS account** | Node IAM. Nothing to configure. |
| **EKS in another account** | An ECR repository policy granting that account, plus node IAM. |
| **GKE / anything non-AWS** | A `dockerconfigjson` pull Secret in **every namespace** that pulls. ECR tokens expire in ~12h, so it needs periodic refresh — `meko-devops` has an `ecrCredentialRefresh` CronJob for exactly this. |
| **A customer's cluster (real BYOC)** | Has no AWS credentials at all. A private ECR image cannot work there; it needs a public image or a customer-side mirror. |

ECR has no anonymous pull. Even a wide-open repository policy still requires the
puller to call `GetAuthorizationToken` with AWS credentials.

## D. Reaching the console

The console is served at the mount root by the control plane and has **no
authentication of its own** beyond the admin secret on the fleet routes, so don't
put it on the public internet. Three options in use:

| Option | How | Where |
|---|---|---|
| Internal LoadBalancer | `apiServer.lb.enabled` plus the cloud preset's annotations — private VPC IP | the default in `values-ycp.yaml` |
| Tailnet | Tailscale operator annotations via `apiServer.service.annotations` | how `meko-dev-eks` exposes it |
| Port-forward | `kubectl -n ycp-system port-forward svc/meko-api-server-service 8080:80` | anywhere, always available |

### Serving it under a subpath (`/ycp`)

To front the console on a shared domain instead of its own host — YBM devcloud
serves it at `https://devcloud.yugabyte.com/ycp/` so the browser's existing auth
tokens and session storage carry over — start the control plane with
`--base-path=/ycp` (or `MEKO_YCP_BASE_PATH=/ycp`). Everything moves under the prefix:
console at `/ycp/`, fleet API at `/ycp/api/v1/environments`, agent API at
`/ycp/api/v1/agent/*`, swagger at `/ycp/swagger/`.

The value is a **path prefix, not a URL** — `/ycp`, `ycp` and `/ycp/` all mean the
same thing, but `https://devcloud.yugabyte.com/ycp` is rejected and the server
refuses to start. That is deliberate: coercing it would mount everything under
`/https:/devcloud.yugabyte.com/ycp` where nothing is reachable, while the health
probe below kept the pod Ready — a green rollout serving 404s. A crash-loop with
the reason in the logs is the better failure.

Two things that stay put, both deliberately:

- **`GET /api/v1/health` remains reachable at the domain root** as well as under
  the prefix. Container probes and load-balancer checks hit the pod directly and
  never see the proxy that supplies the prefix. Both copies are rate-limited.
- **The console needs no separate configuration.** It derives its API prefix from
  its own URL, so it works whether the proxy forwards `/ycp` through (the
  `--base-path` case) or strips it before forwarding (leave `--base-path` unset).

Agents point at the prefixed URL: `agent.ycpUrl=https://devcloud.yugabyte.com/ycp/api/v1`
(`YCP_URL`). A trailing slash is tolerated.

One thing to know before putting the console on a shared domain: browser storage
is scoped per **origin**, not per path, so the admin secret the console holds sits
in a bucket every other page on `devcloud.yugabyte.com` can read. The console
keeps it in `sessionStorage` (tab-lifetime, never written to disk) rather than
`localStorage`, but that narrows the window without closing it — an XSS anywhere
on the domain can still read a live fleet-admin credential. Until the fleet routes
carry real per-user auth, treat that secret as exposed to the whole domain.

## Deploy

With A–C satisfied, follow
[`meko/deploy/helm/meko/YCP-RUNBOOK.md`](../meko/deploy/helm/meko/YCP-RUNBOOK.md):
namespace and Secrets, `helm upgrade --install … -f values-ycp.yaml`, verify, then
register an environment and attach an agent with `-f values-agent.yaml`.

Two things it warns about that are worth repeating, because both fail silently:
the mode preset must be the **last** `-f` file (earlier presets otherwise win and
point the release at the wrong database), and the storage class is per-cluster.

Pick a namespace nothing else owns. On a cluster that already runs Meko, do **not**
deploy into `meko-system` — the data plane's own `meko-api-server` lives there and
the names collide.

---

## Troubleshooting

Symptoms actually seen while bringing this up. The runbook has the chart-level
list; these are the ones specific to the images and the agent.

**Pods stuck `ImagePullBackOff`**
Section C — the cluster isn't authorized for the registry. On non-AWS clusters
check that a pull Secret exists *in that namespace* and hasn't expired (~12h).

**Crash-loop on `flag provided but not defined: -control-plane-mode`**
The api-server image predates the flag. Check it as in section B.

**Agent logs `enroll failed (… connection refused)` for the first few seconds**
Harmless. The agent starts before the control plane is listening and retries every
3s.

**Agent logs `ENROLL REJECTED (401)`**
No environment is bound to the key the agent authenticates with. Confirm the
registered environment's `api_key_id` matches the agent's credential (`YCP_API_KEY`
through YBM, or `YCP_DEV_API_KEY_ID` direct against the control plane) -- an
environment cannot be rebound to a different key once registered, so a mismatch
means the key was revoked or the credential is simply wrong.

**Agent logs `could not read agent state` or `could not persist agent state`**
Its ServiceAccount is missing `get`/`create`/`update` on secrets in its own
namespace. Not fatal — the state holds no credential — but a restart during a
control-plane outage then starts from nothing. See DEPLOY.md.

**`kubectl exec` into a pod gives no shell**
Expected for the agent: `gcr.io/distroless/static:nonroot`, no shell, no
package manager. Debug via logs and port-forward. (The api-server image is Alpine
and does have one.)

## Known gaps

- **No CI build for the control-plane image** — `meko/meko-api-server` is the one
  YCP image nothing publishes automatically.
- **No console authentication** beyond the admin secret on the fleet routes. Keep it
  on an internal LB, a tailnet, or port-forward.

See [`STATUS.md`](./STATUS.md) for the current state of the dev deployment and what
is open.
