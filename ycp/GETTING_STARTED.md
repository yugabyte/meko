# Deploying this YCP reference implementation from scratch

A runbook for someone starting with nothing: no cluster, no registry, no
credentials. It covers what you actually need, in dependency order, and the
failure modes that are easy to hit.

> **Which YCP is this?** Production YCP is `api_server` run with
> `--control-plane-mode`, deployed by the `meko` Helm chart in `meko-devops`
> (`helm … -f values-ycp.yaml`). What you deploy here is `cmd/ycp` — a minimal
> reference implementation of the same contract: same API surface, same enrollment
> handshake, no Cognito/S3/Langfuse dependencies. That makes it the right thing for
> the e2e, for developing an agent against, and for a standalone demo — and the
> wrong thing to point real tenants at. If you want the production control plane,
> use the chart.

- For what the pieces *are*, read [`ARCHITECTURE.md`](./ARCHITECTURE.md) first.
- For per-flag/env reference and the existing cluster variants, see
  [`DEPLOY.md`](./DEPLOY.md). This document is the "start here" path; DEPLOY.md is
  the reference.

---

## 0. Pick how far you need to go

Each level is a superset of the one above it. Don't set up a registry to answer
"does this thing work" — level 1 answers that in one command.

| Level | What it proves | Needs | Time |
|-------|----------------|-------|------|
| **1 — laptop** | The control plane and console run | Go 1.25+ | ~1 min |
| **2 — throwaway cluster** | Images build; the real deploy works end to end; the agent enrolls | + `docker`, `kind`, `kubectl`, `curl` | ~5 min |
| **3 — a real cluster** | A shared, reachable YCP other people can use | + a registry, image publishing, a cluster | ~1 hr first time |

## Level 1 — run it on your laptop

```bash
cd ycp
go run ./cmd/ycp -addr :8080          # console at http://localhost:8080
```

State is in-memory. To exercise the real enrollment handshake locally, pre-arm a
token so an agent can complete it:

```bash
YCP_SEED_ENV_NAME=itest-mcs YCP_SEED_ENV_TOKEN=mko_enr_local_demo go run ./cmd/ycp
```

Nothing else is required at this level — no cluster, no registry, no credentials.

## Level 2 — a throwaway cluster, no registry needed

This is the fastest honest check, and it is the same thing CI runs:

```bash
cd ycp
./test/e2e/run.sh
```

It builds all three images from source, loads them into a `kind` cluster it
creates, applies the real `deploy/ycp-demo.yaml` through the `test/e2e` overlay,
and asserts every workload rolls out, `/healthz` returns `ok`, the agent completes
the enrollment handshake, and the environment appears on the fleet API. Then it
deletes the cluster (`YCP_E2E_KEEP=1` to keep it and poke around).

It pins every `kubectl` call to the `kind-ycp-e2e` context and refuses to run if
that context isn't the cluster it just created, so it cannot touch whatever your
kubeconfig currently points at.

If this passes, the code and manifests are fine and anything that fails at level 3
is environmental.

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

Meko uses ECR at `161908593886.dkr.ecr.us-east-2.amazonaws.com` under `meko/`.
Three repositories, one per binary:

| Repository | Binary | Dockerfile |
|---|---|---|
| `meko/ycp` | control plane + console | `Dockerfile` |
| `meko/meko-agent` | BYOC pull agent | `Dockerfile.agent` |
| `meko/dpmcp` | per-datapack MCP server | `Dockerfile.dpmcp` |

**ECR does not create repositories on push** — unlike most registries. Create
them first, once per account:

```bash
for r in meko/ycp meko/meko-agent meko/dpmcp; do
  aws ecr create-repository --region us-east-2 --repository-name "$r" \
    --image-tag-mutability MUTABLE
done
```

`MUTABLE` is required, not cosmetic: publishing re-pushes the `latest` tag every
time, which `IMMUTABLE` rejects.

## B. Publishing images

[`.github/workflows/ycp-images.yml`](../.github/workflows/ycp-images.yml) builds
and pushes all three on every merge to `main` that touches `ycp/**`, tagging each
with the commit SHA and `latest`. PRs build but never push. You can also trigger
it by hand with `gh workflow run ycp-images.yml --ref main`.

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

`ecr-push-policy.json` — push rights on those three repos and nothing else
(`GetAuthorizationToken` has to be `*`; ECR does not scope it):

```json
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":"ecr:GetAuthorizationToken","Resource":"*"},
 {"Effect":"Allow","Action":["ecr:BatchCheckLayerAvailability","ecr:InitiateLayerUpload",
   "ecr:UploadLayerPart","ecr:CompleteLayerUpload","ecr:PutImage","ecr:BatchGetImage",
   "ecr:DescribeRepositories","ecr:DescribeImages"],
  "Resource":["arn:aws:ecr:us-east-2:161908593886:repository/meko/ycp",
              "arn:aws:ecr:us-east-2:161908593886:repository/meko/meko-agent",
              "arn:aws:ecr:us-east-2:161908593886:repository/meko/dpmcp"]}]}
```

Until the role exists the workflow builds the images and **skips the push with a
warning** rather than failing, so wiring this up later never breaks `main`.

Prefer not to use CI? Build and push by hand:

```bash
aws ecr get-login-password --region us-east-2 \
  | docker login --username AWS --password-stdin 161908593886.dkr.ecr.us-east-2.amazonaws.com
R=161908593886.dkr.ecr.us-east-2.amazonaws.com/meko
docker build --platform linux/amd64 -f Dockerfile       -t $R/ycp:dev        . && docker push $R/ycp:dev
docker build --platform linux/amd64 -f Dockerfile.agent -t $R/meko-agent:dev . && docker push $R/meko-agent:dev
```

Confirm what actually landed:

```bash
aws ecr describe-images --region us-east-2 --repository-name meko/ycp \
  --query 'reverse(sort_by(imageDetails,&imagePushedAt))[0].{tags:imageTags,pushed:imagePushedAt}'
```

## C. A cluster that can pull

The images live in a private registry, so the cluster needs authorization. This
differs by cloud and is the most common thing to get wrong:

| Cluster | What authorizes the pull |
|---|---|
| **EKS in the same AWS account** | Node IAM. Nothing to configure — this is why `deploy/ycp-dev.yaml` sets no `imagePullSecrets`. |
| **EKS in another account** | An ECR repository policy granting that account, plus node IAM. |
| **GKE / anything non-AWS** | A `dockerconfigjson` pull Secret in **every namespace** that pulls. ECR tokens expire in ~12h, so it needs periodic refresh — `meko-devops` has an `ecrCredentialRefresh` CronJob for exactly this. |
| **A customer's cluster (real BYOC)** | Has no AWS credentials at all. A private ECR image cannot work there; it needs a public image or a customer-side mirror. |

ECR has no anonymous pull. Even a wide-open repository policy still requires the
puller to call `GetAuthorizationToken` with AWS credentials.

## D. Reaching the console

The console is plain HTTP on port 8080 and has **no authentication of its own**,
so don't put it on the public internet. Three options in use:

| Option | How | Where it's used |
|---|---|---|
| Internal LoadBalancer | `networking.gke.io/load-balancer-type: Internal` — private VPC IP | `deploy/ycp-demo.yaml` |
| Tailnet | Tailscale operator annotations on a ClusterIP Service | `deploy/ycp-dev.yaml` |
| Port-forward | `kubectl port-forward svc/ycp 8080:80` | anywhere, always available |

## Deploy

With A–C satisfied, the deploy is one command. `deploy/ycp-dev.yaml` is the
image-based manifest (namespace `ycp`, console on the tailnet):

```bash
CTX=meko-dev-eks
kubectl --context $CTX apply -f deploy/ycp-dev.yaml
kubectl --context $CTX -n ycp rollout status deploy/ycp deploy/meko-agent
```

That creates: the `ycp` namespace, the `meko-agent` ServiceAccount and its
ClusterRole/Binding, a seed enrollment-token Secret, and the control-plane and
agent Deployments plus the Service.

Pick a namespace nothing else owns. On a cluster that already runs Meko, do **not**
deploy into `meko-system` — the data plane's own `meko-api-server` lives there and
the names collide.

## Verify

```bash
# the control plane is serving
kubectl --context $CTX -n ycp port-forward svc/ycp 8080:80 &
curl -s localhost:8080/healthz                      # -> ok

# the agent completed the real handshake
kubectl --context $CTX -n ycp logs deploy/ycp | grep "agent enrolled"
# agent enrolled: env=meko-dev provider=aws k8s=v1.33.13-eks-8f14419 nodes=17

# and the fleet API agrees
curl -s localhost:8080/api/v1/environments | jq '.items[] | {id, state, live, node_count}'
# { "id": "meko-dev", "state": "READY", "live": true, "node_count": 17 }
```

`live: true` with real `node_count` is the signal that matters: it means the agent
authenticated, read cluster facts through its RBAC, and reported them back. A row
that stays `live: false` means the agent never enrolled.

## Make state durable

`deploy/ycp-dev.yaml` sets no `YCP_DATABASE_URL`, so the registry is **in-memory**:
everything resets when the pod restarts. Agents re-enroll on their own (the seed
token re-arms every 10 minutes), but registrations made through the API are lost.

For durable state, point it at Postgres — `deploy/ycp-demo.yaml` has a working
in-cluster Postgres to copy:

```yaml
env:
  - { name: YCP_DATABASE_URL, value: "postgres://ycp:ycp@postgres.ycp.svc.cluster.local:5432/ycp?sslmode=disable" }
```

The schema is created and migrated on startup — there is no separate migration
step.

## Enroll a real environment

The seed token in the manifest is for the demo. A genuine environment is
registered through the API, which mints a one-time token:

```bash
curl -sX POST localhost:8080/api/v1/environments \
  -H 'content-type: application/json' \
  -d '{"name":"acme-prod","placement":"cloud","region":"us-east-2"}' | jq
```

The `201` response carries `enrollment_token` — returned **once**, never persisted
in plaintext, and valid for **30 minutes**. Give it to that environment's agent:

```bash
kubectl -n <ns> create secret generic ycp-tokens --from-literal=enroll-token=<token>
```

The agent needs `YCP_URL`, `ENV_NAME` (matching the registered name),
`ENROLLMENT_TOKEN`, and a ServiceAccount with the ClusterRole from
`deploy/ycp-dev.yaml`. Everything it sends is outbound: agent → YCP and agent →
its own in-cluster k8s API. YCP never dials the agent, so the environment needs no
inbound firewall rules.

Its full env surface, with defaults:

| Variable | Default | Purpose |
|---|---|---|
| `YCP_URL` | `http://ycp:8080` | control plane base URL |
| `ENV_NAME` | `itest-mcs` | must match the registered environment |
| `ENROLLMENT_TOKEN` | _(none)_ | the one-time token |
| `ENV_PLACEMENT` | `cloud` | `cloud` \| `onprem` |
| `ENV_PROVIDER` | `gke` | reported as a cluster fact |
| `ENV_REGION` | `us-west1` | reported as a cluster fact |
| `MANAGED_NS_PREFIX` | `ycp-dp-` | prefix for namespaces it creates per datapack |
| `DPMCP_IMAGE` | `postgrest/postgrest:v12.2.3` | image run per datapack |
| `DP_DB_URL` | _(none)_ | data-plane DSN; empty → datapacks get a namespace only |
| `AGENT_VERSION` | `v1.4.2` | reported as a cluster fact |

Note those defaults are demo values, not sensible production ones — set
`ENV_NAME`, `ENV_PROVIDER` and `ENV_REGION` explicitly.

## Tear down

```bash
kubectl --context $CTX delete -f deploy/ycp-dev.yaml
```

The ClusterRole and ClusterRoleBinding are cluster-scoped and go with it. If the
Service was a cloud LoadBalancer, deleting it releases the forwarding rule too.

---

## Troubleshooting

Symptoms actually seen while bringing this up:

**`agent enrolled` never appears; agent logs `enroll failed (enroll -> 500)`**
A server-side error in YCP. Check `kubectl logs deploy/ycp` for `enroll failed:` —
it logs the cause. One historical case: a fresh Postgres whose seeded row had NULL
columns that failed to scan. Fixed, and the startup schema backfills old rows.

**`enroll failed (… connection refused)` for the first few seconds**
Harmless. The agent starts before the control plane is listening and retries every
3s.

**Agent logs `enroll -> 401`**
Token mismatch or expiry. Enrollment tokens last 30 minutes; the demo seed re-arms
every 10. Confirm the agent's `ENROLLMENT_TOKEN` matches the seeded/minted one and
that `ENV_NAME` matches the registered environment.

**Pods stuck `ImagePullBackOff`**
Section C — the cluster isn't authorized for the registry. On non-AWS clusters
check that a pull Secret exists *in that namespace* and hasn't expired (~12h).

**`kubectl exec` into a pod gives no shell**
Expected. All three images are `gcr.io/distroless/static:nonroot` — no shell, no
package manager. Debug via logs and port-forward.

**Helm chart's `ycp` mode crash-loops with `flag provided but not defined: -control-plane-mode`**
The `meko-devops` chart's control-plane mode runs `api_server --control-plane-mode`,
not this binary. An api-server image built before that flag landed won't accept it.
Either rebuild api-server or deploy `deploy/ycp-dev.yaml`, which uses `meko/ycp`
and doesn't depend on it.

**Datapack pods can't pull `dpmcp`**
Known gap. The agent creates those Deployments itself and sets no
`imagePullSecrets`, and the credential refresher only visits a fixed namespace
list — so the dynamically created `ycp-dp-*` namespaces have no credentials.
`DPMCP_IMAGE` therefore still defaults to a public placeholder.

## Known gaps

- **`dpmcp` from a private registry** — as above; needs pull-secret plumbing in the
  datapack namespaces, or a publicly pullable copy (which real BYOC needs anyway).
- **No console authentication** — keep it on an internal LB, a tailnet, or
  port-forward.
- **This is not the production control plane.** YCP proper is `api_server` run with
  `--control-plane-mode`, deployed by the `meko` Helm chart in `meko-devops`
  (`-f values-ycp.yaml`). `cmd/ycp` here is the minimal reference implementation of
  the same contract — same API surface, same enrollment handshake — which is what
  makes it useful for the e2e and for a standalone demo. Use it to exercise or
  develop against the contract; use the chart to run the real thing.
