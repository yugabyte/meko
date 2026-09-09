# Deploying YCP from scratch

A runbook for someone starting with nothing: no cluster, no registry, no
credentials. It covers what you actually need, in dependency order, and the failure
modes that are easy to hit.

> **What YCP is.** The control plane is `api_server` run with
> `--control-plane-mode`; the data plane runs `cmd/reconcile-agent` from this
> directory beside a Meko stack the chart installs.
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
| **2 — throwaway cluster** | The image builds; the control plane migrates, serves, registers an environment and prints a runnable install hint | + `kind`, `kubectl`, `curl`, `python3`, and docker **or** podman | ~5 min |
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

It builds the api-server image from source, loads it into a `kind` cluster it
creates, and asserts the control plane's whole surface: the control-plane migrations
run against a plain Postgres, `/api/v1/health` comes up, the fleet API refuses an
unauthenticated call, an environment is registered through the real API bound to an
`api_key_id` and seeded with nothing, and the install hint dials the `agent_base_url`
the registration stored. It also asserts the `/ycp` base-path mount, which is how
production fronts it. Then it deletes the cluster (`YCP_E2E_KEEP=1` to keep it and
poke around).

Everything it runs uses public images, so it needs no registry credentials. That is
also its limit, and it is a real one: **it does not drive an agent.** The retired
`meko-agent` could stand a stack up from public images alone; the reconcile-agent
needs an api-server or an amp controller to converge against, and every image in the
Meko release is private. Convergence is covered by the backends' unit tests and by
level 2.9 below, on a cluster that can pull.

It pins every `kubectl` call to the `kind-ycp-e2e` context and refuses to run if
that context isn't the cluster it just created, so it cannot touch whatever your
kubeconfig currently points at.

If this passes, the control plane and its manifests are fine and anything that fails
at level 3 is environmental.

## Level 2.9 — the same flow on a real cluster, two namespaces

No kind. One namespace for the control plane, one for the data plane, on a cluster
you already have — `itest-dev`, `itest-mcs`, `meko-dev-eks`:

```bash
# once per code change: build the amd64 api-server image and push it to ECR. The
# reconcile-agent is not built here — it publishes to Quay, and
# ycp/build-reconciler-agent-image.sh is the local equivalent.
export AWS_PROFILE=meko-dev
TAG="bootstrap-$(git rev-parse --short HEAD)"
ECR=161908593886.dkr.ecr.us-east-2.amazonaws.com
podman build --platform linux/amd64 -f meko/api_server/Dockerfile \
  -t "$ECR/meko/meko-api-server:$TAG" meko/api_server
podman push "$ECR/meko/meko-api-server:$TAG"

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
  has no AWS IAM. Each release carries its own, through `global.imagePullSecrets` —
  the control plane is not involved, since it no longer hands anyone a stack.
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
| `meko/meko-api-server` | the control plane's image, and the data plane's | `meko/api_server/Dockerfile` | `meko-control-plane-image.yml` (to Quay) |

The BYOC pull agent is not here: `ycp-reconciler-agent` publishes to **Quay**
(`ycp-reconciler-agent-image.yml`), so a cluster pulls it without ECR
cross-account pull-secret setup.

**ECR does not create repositories on push** — unlike most registries. Create them
first, once per account:

```bash
for r in meko/meko-api-server; do
  aws ecr create-repository --region us-east-2 --repository-name "$r" \
    --image-tag-mutability MUTABLE
done
```

`MUTABLE` is required, not cosmetic: publishing re-pushes the `latest` tag every
time, which `IMMUTABLE` rejects.

## B. Publishing images

[`.github/workflows/ycp-reconciler-agent-image.yml`](../.github/workflows/ycp-reconciler-agent-image.yml)
builds and pushes `quay.io/yugabyte/ycp-reconciler-agent` on every merge to `main`
that touches `ycp/**`, tagging with the commit SHA; only a `main` build moves
`latest`. PRs build but never push. Trigger it by hand with
`gh workflow run ycp-reconciler-agent-image.yml --ref <branch>`.

It authenticates with a Quay robot delivered as a repo secret, and stays green with
`--no-push` when that secret is absent.

The Quay robot is a repo secret (`QUAY_AMP_TOKEN`); until it exists the workflow
builds and **skips the push with a warning** rather than failing, so wiring it up
later never breaks `main`. The control-plane image ships the same way, from
[`.github/workflows/meko-control-plane-image.yml`](../.github/workflows/meko-control-plane-image.yml)
and `meko/api_server/build-image.sh`.

Nothing pushes to ECR from CI any more — that was `ycp-images.yml`, deleted with
`cmd/agent`. The **data-plane** images the pinned release names still live there, and
are still pushed by hand:

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
aws ecr describe-images --region us-east-2 --repository-name meko/meko-api-server \
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
register an environment and install its data plane with the hint that registration
prints — the meko chart with `-f values-byoc.yaml` and `reconcileAgent.enabled=true`.

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

- **No CI push to ECR.** Both YCP images publish to Quay
  (`meko-control-plane-image.yml`, `ycp-reconciler-agent-image.yml`); the ECR
  `meko/meko-api-server` the pinned data-plane release names is pushed by hand.
- **No CI coverage of the reconcile loop.** The e2e is a control-plane smoke; nothing
  in CI converges a resource end to end.
- **No console authentication** beyond the admin secret on the fleet routes. Keep it
  on an internal LB, a tailnet, or port-forward.

See [`STATUS.md`](./STATUS.md) for the current state of the dev deployment and what
is open.
