# Deploying YCP from scratch

A runbook for someone starting with nothing: no cluster, no registry, no
credentials. It covers what you actually need, in dependency order, and the failure
modes that are easy to hit.

> **What YCP is.** The control plane is `api_server` run with
> `--control-plane-mode`; the data plane runs `meko-agent` from this directory.
> Both are deployed by the `meko` Helm chart in
> [meko-devops](https://github.com/yugabyte/meko-devops).
>
> This document gets you to the point where that chart can work: code that builds,
> images in a registry, and a cluster allowed to pull them. The install itself is
> [`charts/meko/YCP-RUNBOOK.md`](https://github.com/yugabyte/meko-devops/blob/main/charts/meko/YCP-RUNBOOK.md),
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
| **2 — throwaway cluster** | Images build; the control plane migrates, serves and enrolls an agent; the agent survives a restart | + `kind`, `kubectl`, `curl`, `python3`, and docker **or** podman | ~5 min |
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
unauthenticated call, an environment is registered through the real API, the agent
enrolls with the token that mints, reaches `READY` with real cluster facts, and then
restarts **without re-enrolling**. Then it deletes the cluster (`YCP_E2E_KEEP=1` to
keep it and poke around).

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

Meko uses ECR at `161908593886.dkr.ecr.us-east-2.amazonaws.com` under `meko/`:

| Repository | Binary | Dockerfile | Published by |
|---|---|---|---|
| `meko/meko-agent` | BYOC pull agent | `ycp/Dockerfile.agent` | `ycp-images.yml` |
| `meko/dpmcp` | per-datapack MCP server | `ycp/Dockerfile.dpmcp` | `ycp-images.yml` |
| `meko/meko-api-server` | the control plane | `api_server/Dockerfile` | **nobody — hand-built** |

**ECR does not create repositories on push** — unlike most registries. Create them
first, once per account:

```bash
for r in meko/meko-agent meko/dpmcp meko/meko-api-server; do
  aws ecr create-repository --region us-east-2 --repository-name "$r" \
    --image-tag-mutability MUTABLE
done
```

`MUTABLE` is required, not cosmetic: publishing re-pushes the `latest` tag every
time, which `IMMUTABLE` rejects.

## B. Publishing images

[`.github/workflows/ycp-images.yml`](../.github/workflows/ycp-images.yml) builds
and pushes the agent and dpmcp images on every merge to `main` that touches
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
              "arn:aws:ecr:us-east-2:161908593886:repository/meko/dpmcp"]}]}
```

Until the role exists the workflow builds the images and **skips the push with a
warning** rather than failing, so wiring this up later never breaks `main`.

The control plane's image has no workflow, so it is built and pushed by hand:

```bash
aws ecr get-login-password --region us-east-2 \
  | docker login --username AWS --password-stdin 161908593886.dkr.ecr.us-east-2.amazonaws.com
R=161908593886.dkr.ecr.us-east-2.amazonaws.com/meko
TAG=$(date +%Y%m%d)
docker build --platform linux/amd64 -f api_server/Dockerfile -t $R/meko-api-server:$TAG api_server
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

Agents point at the prefixed URL: `agent.ycpUrl=https://devcloud.yugabyte.com/ycp`
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
[`charts/meko/YCP-RUNBOOK.md`](https://github.com/yugabyte/meko-devops/blob/main/charts/meko/YCP-RUNBOOK.md):
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

**Agent logs `enroll -> 401`**
Token mismatch or expiry. Enrollment tokens last 30 minutes. Confirm the agent's
`ENROLLMENT_TOKEN` matches the minted one and that `ENV_NAME` matches the
registered environment's id (the slug of the name it was registered under).

**Agent exits with `cannot read agent state` or `enrolled but could not persist
agent state`**
Its ServiceAccount is missing `get`/`create`/`update` on secrets in its own
namespace. Both are deliberately fatal — see DEPLOY.md.

**`kubectl exec` into a pod gives no shell**
Expected for the agent and dpmcp: `gcr.io/distroless/static:nonroot`, no shell, no
package manager. Debug via logs and port-forward. (The api-server image is Alpine
and does have one.)

**Datapack pods can't pull `dpmcp`**
Known gap. The agent creates those Deployments itself and sets no
`imagePullSecrets`, and the credential refresher only visits a fixed namespace
list — so the dynamically created `ycp-dp-*` namespaces have no credentials.
`DPMCP_IMAGE` therefore still defaults to a public placeholder.

## Known gaps

- **`dpmcp` from a private registry** — as above; needs pull-secret plumbing in the
  datapack namespaces, or a publicly pullable copy (which real BYOC needs anyway).
- **No CI build for the control-plane image** — `meko/meko-api-server` is the one
  YCP image nothing publishes automatically.
- **No console authentication** beyond the admin secret on the fleet routes. Keep it
  on an internal LB, a tailnet, or port-forward.

See [`STATUS.md`](./STATUS.md) for the current state of the dev deployment and what
is open.
