# meko Helm chart — GKE deploy guide

A single self-contained chart that stands up a full Meko instance
(api-server, meko-ui, meko-mcp-server, inference-gateway, rag-agent, langfuse,
provisioner) in a GKE cluster via plain `helm`.

This chart moved here from
[`yugabyte/meko-devops`](https://github.com/yugabyte/meko-devops) (`charts/meko`,
through commit `7f1428c`) so it versions alongside the components it deploys —
its 42 commits of history stay in that repo. The **AWS/EKS** path it was
additive to (`kustomize/` + `jenkins/`) also stays there, as does
[`HELM_MIGRATION_PLAN.md`](https://github.com/yugabyte/meko-devops/blob/main/HELM_MIGRATION_PLAN.md),
which has the full design and parity notes.

Validated on GKE `itest-mcs`: all components render, apply, and pull their
images from ECR; `helm template -f values-gcp.yaml` + `kubectl apply
--dry-run=server` pass for all objects.

## Layout

```
values.yaml            canonical schema + safe defaults (cloud-neutral)
values-aws.yaml        cloud preset — EKS/ALB/ACM (parity baseline)
values-gcp.yaml        cloud preset — GKE: GCE ingress + ManagedCertificate, ECR pull
values-dev.yaml        env preset — dev hosts/DB/Cognito/tags
values-prod.yaml       env preset — prod hosts/DB/Cognito/tags
values-ycp.yaml       mode preset — YCP control plane (BYOC)
values-byoc.yaml       mode preset — a self-contained data plane in ONE namespace:
                       its own YugabyteDB, and no Secrets to create by hand
values-byoc-full.yaml  opt-in overlay — moves the inference gateway onto OpenAI and
                       adds the rag text worker. AFTER values-byoc.yaml; needs an OpenAI key
```

Compose a cloud preset + an env preset at install:
`-f values-gcp.yaml -f values-dev.yaml`.

## Deploy modes

The chart deploys in one of two modes, selected by the top-level `mode` value:

| `mode` | What deploys | Where it runs |
|--------|--------------|---------------|
| `mcp` (default) | **Data plane** — the full Meko stack (api-server + meko-mcp-server + provisioner + meko-ui + inference-gateway + rag-agent + langfuse). Unchanged from before this switch existed; renders byte-identically. | A Meko tenant / data-plane cluster. |
| `ycp` | **Control plane (BYOC / YCP)** — `api-server` run with `--control-plane-mode` (the fleet registry + agent API from `byoc-control-plane.md`) plus its console on an **internal** LoadBalancer. No data-plane components render. | Inside the Meko/Aeon VPC (not a customer cluster). |

`mode: ycp` **suppresses** every data-plane component regardless of its
`<comp>.enabled` flag, drops the api-server's data-plane args/secrets (S3, OpenAI,
langfuse, SES), and turns off the public Ingress. Deploy the control plane with
the `values-ycp.yaml` preset.

> **Standing one up from nothing?** [`YCP-RUNBOOK.md`](./YCP-RUNBOOK.md) is the
> step-by-step: the five Secrets and their exact keys, attaching an agent, tailnet
> exposure, teardown, and a troubleshooting list. What follows assumes the
> prerequisites are already in place.

```bash
helm upgrade --install ycp charts/meko -n ycp-system --create-namespace \
  -f charts/meko/values-aws.yaml -f charts/meko/values-<env>.yaml \
  -f charts/meko/values-ycp.yaml \
  --set-string apiServer.image.tag=$VERSION
```

That is the whole command now. The namespace, `cognito.enabled: false`, the migrate
scope and the storage class all live in the presets — they are properties of the
mode and the cluster, not of the invocation (#42).

Two things about that command differ from the data-plane one and both matter:

- **`values-ycp.yaml` goes last.** Helm merges `-f` files left to right, and the env
  presets set `apiServer.migrate.scope` and the `SYSTEM_DB_`/`DB_` endpoints for a
  data plane. With the mode preset earlier, those win — and the release comes up
  against the shared data-plane YugabyteDB running the wrong migration set.
- **Its own namespace.** The data plane's api-server Deployment has the same name,
  so installing into a namespace that already runs one fails on Helm ownership
  (and force-adopting it would replace the live data plane).

#### Serving the console under a subpath

`apiServer.basePath` moves the whole api-server — console, `/api/v1`, the agent
API, swagger, the OAuth2 callback — under a URL prefix:

```bash
helm upgrade --install ycp charts/meko ... --set apiServer.basePath=/ycp
```

It exists so YCP can be fronted at `https://devcloud.yugabyte.com/ycp/`, a subpath
of the YBM domain rather than its own subdomain, which is what lets the browser's
existing auth tokens and session storage carry over.

Empty (the default) serves at the domain root, so every install that doesn't set
it is unaffected. Four things to know before you do set it:

- **Only correct if the proxy in front forwards the prefix unstripped.** If it
  strips `/ycp` before forwarding, leave `basePath` empty — the console derives
  its own API prefix from its URL and works either way.
- **It is a path prefix, not a URL.** `https://devcloud.yugabyte.com/ycp` is
  rejected and the api-server refuses to start, rather than mounting the surface
  where nothing can reach it.
- **Needs an image with `--base-path`** (`1.0.37-20260729` or newer). Older images
  ignore the env var and quietly serve at the root — check the tag the same way
  the runbook checks for `--control-plane-mode`.
- **Agents need the prefixed URL** — `--set agent.ycpUrl=https://…/ycp/api/v1`.

The health probes deliberately stay unprefixed; see Notes / gotchas.
[`YCP-RUNBOOK.md`](./YCP-RUNBOOK.md#serving-it-under-a-subpath) has the operational
detail, including what changes when you reach the console through the internal LB.

The release is self-contained: `controlPlaneDb` deploys a small Postgres for the
fleet registry and `migrate.scope: controlplane` creates its tables on pod start.
That scope exists because the system and data migration sets need YugabyteDB
(`HASH` primary keys, `SPLIT INTO … TABLETS`) while the fleet-registry set is
portable SQL. Set `controlPlaneDb.enabled=false` to point at an external database
instead.

Control-plane Secrets are a subset of the data-plane set: `meko-api-server-db`,
`meko-admin-api-key` (key `admin-api-key`, gates the fleet/console routes --
without it they return 503),
`meko-api-server-system-db`, `meko-mcp-internal-secret`, and (when
`apiServer.cognito.enabled`) `meko-api-server-cognito`. No S3/OpenAI/langfuse/SES
Secrets are needed.

The chart can render that subset for you instead of you creating them by hand —
`controlPlaneSecrets` in `values.yaml`, off by default:

```bash
  --set controlPlaneSecrets.enabled=true \
  --set controlPlaneSecrets.dbPassword="$PGPW" \
  --set controlPlaneSecrets.adminApiKey="$(openssl rand -hex 24)" \
  --set controlPlaneSecrets.mcpInternalSecret="$(openssl rand -hex 24)"
```

One `dbPassword` reaches all three Secrets that have to agree on it, which is the
step most likely to go wrong by hand. What you pass lands in Helm release state, so
it suits a dev or demo control plane; for anything else keep the Secrets
out-of-band or use a secret manager. It is `ycp`-mode only — in `mcp` mode those
same names hold the shared data-plane database's credentials, so the chart refuses
rather than overwriting them. See the block comment in `values.yaml` for the full
tradeoff, including why nothing is auto-generated.

### Data-plane Secrets (`dataPlaneSecrets`)

The data plane has the same facility, and needs it more: its database password has
to appear in **three** Secrets under **two** key spellings (`db-password` twice,
then `password` and `system_password`) *and* match what the database was
initialised with. `dataPlaneSecrets.dbPassword` writes all of them from one value,
and when `yugabyte.enabled` puts the database in this release too, both sides of
that match come from here. `values-byoc.yaml` turns it on.

The other half of "no Secrets by hand" is that the credentials a data plane does
not use are read with `optional: true` — S3, the OpenAI key, Cognito on the
api-server, the UI's analytics key. A required `secretKeyRef` with no Secret behind
it is a pod stuck in `CreateContainerConfigError`: no logs, no probe failures,
nothing naming the credential. Optional means the variable is simply unset and the
feature fails where it is called, which is a message you can read.

Three refs stay required, deliberately:

| Ref | Why not optional |
|---|---|
| `meko-api-server-db` / `-system-db` (`db-password`) | An api-server that starts without a database password is the same outage arriving later |
| `meko-mcp-internal-secret` (`secret-key`) | Absent, the data plane's two halves silently reject each other |
| `cognito-client-secret` on **meko-mcp-server** | It reads `os.environ["COGNITO_CLIENT_SECRET"]` while building the provider — absent is a `KeyError` one second into startup, not a degraded feature. A deployment with no IdP sets `mekoMcpServer.staticEnv.authProvider: meko_tkn` (PAT-only) rather than relying on a missing credential being tolerated; the ref is then not rendered at all |

### Registering a data plane with a control plane (the agent)

The **reconcile-agent** — the BYOC pull agent — is a component of *this* chart rather
than a separate one. A data-plane install opts in to enrolling itself with a YCP
control plane by turning it on:

```bash
# 1. Register the environment with YCP, bound to the agent's api key id
curl -sX POST "$YCP/api/v1/environments" \
  -d '{"name":"acme-prod","kind":"meko","region":"us-west1","placement":"cloud",
       "api_key_id":"<uuid>"}'

# 2. Store the agent's credential as a Secret in the data-plane cluster: the YBM
#    API key (key `api-key`), or against a standalone control plane the dev-mode
#    pair (`dev-admin-secret` + `dev-api-key-id`)
kubectl -n meko-system create secret generic meko-agent-api-key \
  --from-literal=api-key=<the key YBM showed once>

# 3. Deploy the data plane with the agent enabled, pointed at YCP's API base.
#    values-byoc.yaml goes LAST: it is what makes this one self-contained namespace.
helm upgrade --install meko charts/meko -n meko-system \
  -f charts/meko/values-aws.yaml -f charts/meko/values-dev.yaml \
  -f charts/meko/values-byoc.yaml \
  --set reconcileAgent.enabled=true \
  --set reconcileAgent.ycpUrl=$YCP --set reconcileAgent.envName=acme-prod \
  --set-string reconcileAgent.image.tag=$AGENT_TAG
```

YCP's own register response prints that command with everything it can know already
filled in — the namespace, the URL it was reached on, and the stack's per-component
image tags from the release it pins. Prefer it over retyping this.

### Metrics and logs (`otel`)

`templates/otel/collector.yaml` is two `OpenTelemetryCollector` CRs — they need the
OpenTelemetry Operator with the contrib distro — modelled on amp's
([`ac-metrics.md`](../../../../amp/controller/architecture/ac-metrics.md),
[`ac-logs.md`](../../../../amp/controller/architecture/ac-logs.md)):

- **`meko-metrics`** collects, each where its component is enabled:
  - `/metrics` from the api-server, MCP server, inference gateway and reconcile-agent,
    each on `<component>.metricsPort` (9464), a port named `metrics` kept off the one
    their Service and Ingress expose. The api-server's include `meko_api_rag_documents`,
    the knowledge-base backlog by status read from `dist_rag` (the rag workers export
    nothing themselves), and `meko_api_admin_*`, the admin business counters — users by
    tier, datapacks, memories, token savings, and knowledge-base documents/chunks by file
    type — read from `meko_system`/`datapacks`/`dist_rag`. These leave the cluster through
    the BYOC door like every other series.
  - the provisioner's controller-runtime `/metrics`, over HTTPS with the collector's
    token, through a ClusterRoleBinding to the provisioner's metrics-reader role;
  - Langfuse's DogStatsD metrics, on a `statsd` receiver at UDP 8125 that the web and
    worker containers are pointed at;
  - the bundled YugabyteDB's master, tserver and YSQL endpoints, with table- and
    tablet-level series dropped;
  - with `otel.podMetrics`, cAdvisor for this release's namespaces.

  It ships to `otel.remoteWriteEndpoint`, the BYOC door, or both. `otel.scrape.*`
  turns each source off.
- **`meko-logs`** is a daemonset tailing `/var/log/pods` for this release's
  namespaces only, shipping to `otel.logsEndpoint`, Datadog, or both. In the
  one-namespace BYOC shape it adds pod metadata with `k8sattributes`; across the
  hosted layout's several namespaces that would need cluster-wide pod reads, so it
  ships the file path's pod and container names alone.

`otel.byoc.enabled` turns on both off-cluster sinks together, each reading its own
Secret — `byoc-metrics-auth` and `datadog-api` — which must exist first. Metrics
carry `environment_id`/`account_id`/`source="meko"`, and logs the same identity as
Datadog tags.

Off by default, because this chart also deploys the hosted stack. The install hint
YCP prints turns it on for a BYOC data plane, and the reconcile-agent reports the
result back, so an environment whose release ships nowhere shows as `DEGRADED`.
The UI exposes no metrics; its logs ship. The MCP server's series come from its own
repository (`yugabyte/meko-mcp-server`); an image predating any source's metrics just
leaves that target down.

`helm template` sees no CRDs, so render it with `--api-versions opentelemetry.io/v1beta1`.

## Identity provider

`apiServer.cognito.enabled` and `apiServer.oidc.enabled` select who authenticates
people. Exactly one may be on; both fails the render rather than letting flag order
decide. Neither leaves the api-server in dev mode, treating every request as one
shared account — fine for a bring-up nobody else can reach, not for anything else.

**Cognito** is the default and what the hosted deployments run. It is the user
store as well as the IdP, so it serves signup, password reset, email verification
and the Cognito-brokered Google/LinkedIn logins.

**OIDC** is any issuer with a discovery document — Keycloak, Dex, Okta, Auth0,
Google. Everything but the issuer and the client is discovered. It is the right
answer for a customer cluster, where a Cognito pool in *our* AWS account is the
opposite of BYOC:

```yaml
apiServer:
  cognito: { enabled: false }
  oidc:
    enabled: true
    issuerUrl: https://keycloak.example.com/realms/meko
    clientId: meko
    # Must match the issuer's registered URI byte-for-byte; it cannot be derived
    # behind a TLS-terminating proxy, which is why it is configured.
    redirectUrl: https://meko.example.com/api/v1/auth/oidc/callback
dataPlaneSecrets:
  oidcClientSecret: ""   # empty is a public client, relying on PKCE
```

The issuer owns its users, so an OIDC deployment serves **no** signup,
password-reset, change-password or email-verification routes — those happen on the
issuer's own pages. What it serves is `/auth/oidc/login`, `/auth/oidc/callback`,
`/auth/logout` and the shared `/auth/me` surface. Meko PATs are unaffected: they
are issued and validated by this server, whichever IdP is on.

`values-byoc.yaml` runs the inference gateway on **bedrock** (which needs
`dataPlaneSecrets.awsAccessKeyId` / `.awsSecretAccessKey`) and leaves the rag workers
and the provisioner off. Appending `-f charts/meko/values-byoc-full.yaml` —
later `-f` wins, which is the whole mechanism — switches the gateway to **openai** and
adds the rag text worker, so it needs `--set dataPlaneSecrets.openaiApiKey=sk-...`,
without which the render fails rather than installing a gateway that exits at startup.
Any install with the rag workers on also needs `dataPlaneSecrets.ragAwsAccessKeyId` /
`.ragAwsSecretAccessKey`; without them the workers start but cannot read an uploaded
file from S3 or embed it with Bedrock Titan, so nothing a user uploads is processed.
The MCP server needs `dataPlaneSecrets.mcpAwsAccessKeyId` / `.mcpAwsSecretAccessKey` too, to
embed KB and conversation search queries with the same model; without them those searches
answer empty.
`provisioner` renders ClusterRoles and a CRD, which is the opposite of one namespace —
not a flag away.

`langfuse` is **on** in `values-byoc.yaml`, bundled the same way as the database: no
ClickHouse anywhere (this fork's "tracing" backend is a `clickhouse`-named Postgres
schema in the same YugabyteDB database, not a real ClickHouse deployment) and a bundled
Valkey subchart (`langfuse.valkey.managed: true`). It needs seven
`dataPlaneSecrets.langfuse*` values with no default — six are real cryptographic
material, the seventh (`langfuseInitUserEmail`) is the seeded owner's login — so the
render fails rather than shipping the exact demo password (`password123`) a HackerOne
report already turned into an RCE:

```bash
--set dataPlaneSecrets.langfuseNextauthSecret=$(openssl rand -hex 16) \
--set dataPlaneSecrets.langfuseSalt=$(openssl rand -hex 16) \
--set dataPlaneSecrets.langfuseEncryptionKey=$(openssl rand -hex 32) \
--set dataPlaneSecrets.langfuseAdminApiKey=$(openssl rand -hex 16) \
--set dataPlaneSecrets.langfuseInitUserEmail=owner@your-domain \
--set dataPlaneSecrets.langfuseInitUserPassword=$(openssl rand -hex 16) \
--set dataPlaneSecrets.langfuseInitProjectSecretKey=$(openssl rand -hex 16)
```

langfuse also needs an **S3 event-upload store** — its ingestion buffer, where every
conversation event lands before a worker reads it into the tracing store. The render
now fails without one (otherwise conversations are accepted on write and silently
dropped — MEKO-832). Point **all three** buckets at your own bucket and set its region
(the defaults are the hosted bucket in `us-east-2`, so a bare event-bucket override
still sends media/export to the hosted bucket and breaks a non-us-east-2 event bucket):

```bash
--set langfuse.config.LANGFUSE_S3_EVENT_UPLOAD_BUCKET=<your-bucket> \
--set langfuse.config.LANGFUSE_S3_MEDIA_UPLOAD_BUCKET=<your-bucket> \
--set langfuse.config.LANGFUSE_S3_BATCH_EXPORT_BUCKET=<your-bucket> \
--set langfuse.config.LANGFUSE_S3_EVENT_UPLOAD_REGION=<region> \
--set langfuse.config.LANGFUSE_S3_MEDIA_UPLOAD_REGION=<region> \
--set langfuse.config.LANGFUSE_S3_BATCH_EXPORT_REGION=<region> \
# then pick ONE auth mode:
# (a) static keys — langfuse.s3AuthMode defaults to iam-user; works on any cloud:
--set dataPlaneSecrets.langfuseS3AccessKey=AKIA… --set dataPlaneSecrets.langfuseS3SecretKey=… \
# (b) EKS IRSA — leave the keys empty and annotate the langfuse ServiceAccount with
#     the role ARN; the EKS pod-identity webhook then injects the AWS credentials.
#     (EKS only: langfuse reaches S3 via the AWS SDK, and a GKE Workload Identity
#     annotation gives a Google identity the SDK can't use for S3 — on GKE use (a).)
--set langfuse.s3AuthMode=iam-role \
--set langfuse.serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::<acct>:role/<role>
```

The render requires the event-upload store; `MEDIA_UPLOAD` / `BATCH_EXPORT` are not
required, but they default to the hosted bucket, so repoint (above) or disable them
(`--set langfuse.config.LANGFUSE_S3_BATCH_EXPORT_ENABLED=false`, and leave the media
bucket pointed at yours) rather than writing to the hosted bucket with your credentials.

**Generate these once and keep them** — in a values file of your own, or a password
manager — rather than re-running the command above on a second `helm upgrade`.
`dataplane-secrets.yaml` re-renders `langfuse-secrets` from whatever values it's given
on every upgrade; it does not read back the live Secret. A fresh `openssl rand` on a
second install silently rotates NextAuth/encryption keys that langfuse's own database
already has data under, and resets the seeded owner's password.

The agent enrolls, converges desired state, and reports status over **outbound HTTPS
only**. It is data-plane-only — `ycp` mode never renders it. Its image comes from Quay
rather than the Meko ECR, so it needs no cross-account pull secret.

**It converges resources over the api-server's REST API, not Kubernetes.** The
workloads are this chart's; what the agent manages is the `agent` and `datapack`
resources the environment declares, so it needs no cluster RBAC: a namespaced Role
covering its own state Secret, plus read-only `list` on the namespace's Deployments
and StatefulSets so the console can show which workloads are up. The field reference is
[`ycp/DEPLOY.md`](https://github.com/yugabyte/meko/blob/main/ycp/DEPLOY.md) in the
meko repo.

**The agent is the api-server's only caller and is authoritative over it.** Anything
desired state stops declaring is collected — including a datapack made by hand — so do
not point one at an api-server people use directly.

No api-server credential is needed against an api-server with no identity provider,
which authenticates nobody — the default in a BYOC bring-up. With **either** provider
on (`apiServer.cognito.enabled` or `apiServer.oidc.enabled`) every call is
authenticated, so supply `reconcileAgent.secrets.existing.mekoApiToken` (key
`meko-api-token`) — a meko PAT, which this server issues and validates whichever IdP
is on. It also decides the account the agent's datapacks belong to. Without it the
agent runs and every call to `/datapacks` and `/agents` 401s.

## Prerequisites you provide (not created by the chart)

1. **A GKE cluster** and `kubectl` context for it.
2. **Referenced Secrets** — by default the chart holds no secret material; it
   references Secrets by name. Create them per the table in
   `HELM_MIGRATION_PLAN.md §5` (e.g. `meko-api-server-db`,
   `yugabytedb-credentials`, `langfuse-secrets`, …), in the right namespaces.
   `meko-mcp-internal-secret` must hold the **same** `secret-key` in `meko-system`
   and `meko-mcp`. Or hand the job to the chart: `controlPlaneSecrets` /
   `dataPlaneSecrets` render them from values, which is what makes a `values-byoc.yaml`
   install need no `kubectl create secret` step at all.
3. **ECR pull secret** — images are pulled from AWS ECR. GKE has no AWS IAM, so
   create a dockerconfigjson Secret named `ecr-registry-creds` in **every**
   namespace (`values-gcp.yaml` sets `global.imagePullSecrets: [ecr-registry-creds]`):
   ```bash
   PW="$(AWS_PROFILE=meko-dev aws ecr get-login-password --region us-east-2)"
   for ns in meko-system meko-mcp meko-rag-workers meko-langfuse meko-ingress; do
     kubectl create secret docker-registry ecr-registry-creds -n "$ns" \
       --docker-server=161908593886.dkr.ecr.us-east-2.amazonaws.com \
       --docker-username=AWS --docker-password="$PW"
   done
   ```
   ECR auth tokens expire ~12h, so this Secret goes stale. Two ways to keep it fresh:

   **(a) Static key (simple, has a long-lived credential).** The
   `ecrCredentialRefresh` CronJob (in `values-gcp.yaml`) re-mints the pull Secret
   every 6h; it needs an `aws-ecr-refresher-creds` Secret
   (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`) in `meko-system` holding an IAM
   user key with ECR-read on the registry. NOTE: SSO/temporary creds (e.g. the
   `meko-dev` profile) will NOT work here — a standing CronJob needs a *static* key.

   **(b) GKE → AWS Workload Identity federation (recommended; no static key).**
   AWS federates the GKE cluster's OIDC issuer, and the CronJob assumes an IAM
   role via a projected ServiceAccount token — nothing long-lived stored.

   ```bash
   # 1. GKE cluster OIDC issuer (must be publicly reachable so AWS can fetch JWKS)
   ISSUER=$(kubectl get --raw /.well-known/openid-configuration | jq -r .issuer)
   #    e.g. https://container.googleapis.com/v1/projects/<proj>/locations/<loc>/clusters/<cluster>

   # 2. Register it as an AWS IAM OIDC provider (audience sts.amazonaws.com)
   aws iam create-open-id-connect-provider --url "$ISSUER" \
     --client-id-list sts.amazonaws.com

   # 3. IAM role trusting ONLY the refresher KSA, with ECR read
   #    trust policy condition:
   #      "<issuer-no-scheme>:sub" = "system:serviceaccount:meko-system:ecr-token-refresher"
   #      "<issuer-no-scheme>:aud" = "sts.amazonaws.com"
   aws iam create-role --role-name meko-ecr-refresher \
     --assume-role-policy-document file://trust.json
   aws iam attach-role-policy --role-name meko-ecr-refresher \
     --policy-arn arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly
   ```

   Then the CronJob pod projects an `sts.amazonaws.com`-audience token and sets
   `AWS_ROLE_ARN` + `AWS_WEB_IDENTITY_TOKEN_FILE`, so `aws ecr get-login-password`
   does `AssumeRoleWithWebIdentity` — no key. Chart wiring to add (TODO): a
   `ecrCredentialRefresh.awsRoleArn` value that, when set, mounts a projected
   token volume (`serviceAccountToken`, audience `sts.amazonaws.com`) + those two
   env vars and drops the `awsCredentialsSecret` envFrom. (The Secret-based path
   stays the default/fallback.)

   The same role can back **container image pulls** without the pull Secret at all
   if you later move images to Artifact Registry, or via the kubelet ECR
   credential-provider — but that's beyond this chart.
4. **DNS + TLS** for the GKE ingresses (GCE + Google ManagedCertificate):
   set real GKE hostnames in `values-<env>.yaml` (defaults are the AWS hosts),
   point DNS at the ingress IP, and for the mcp-proxy **wildcard** host upload a
   Google SSL cert and set `provisioner.mcpProxy.ingress.gke.preSharedCert`
   (ManagedCertificate can't do wildcards).
5. **Backing services reachable from the cluster**: YugabyteDB (app + system +
   langfuse DBs), a `valkey` release in `meko-langfuse`, Cognito (or another IdP
   — Cognito is AWS-specific), and Bedrock/OpenAI access.

## One-shot bring-up script

`scripts/deploy-gke.sh` automates the entire flow end-to-end (ECR pull secret,
optional throwaway YugabyteDB via the yugabytedb chart + its disk gflags,
databases + extensions, reference Secrets, and a single-step chart install — the
DB migration runs as an initContainer). Validated on GKE `itest-mcs`:

```bash
CONTEXT=gke_yugabyte_us-west1_itest-mcs ./charts/meko/scripts/deploy-gke.sh
# real login: pass COGNITO_CLIENT_SECRET=... and register the callback (see below)
```

It's a demo/bootstrap path (throwaway YB, dummy Secrets). For production, wire a
real DB + Secrets and do the manual install below.

## Install

```bash
gcloud container clusters get-credentials <cluster> --region <region> --project <project>

# (after the prereq Secrets exist)
helm upgrade --install meko charts/meko -n meko-system --create-namespace \
  -f charts/meko/values-gcp.yaml -f charts/meko/values-<env>.yaml \
  --set-string apiServer.image.tag=$VERSION \
  --dry-run=server        # review; drop --dry-run to apply
```

The api-server DB migration runs as an **initContainer** on the Deployment
(`apiServer.migrate.enabled`, scope via `apiServer.migrate.scope`): it blocks the
main container until migrations succeed and works under `helm --wait` / Argo CD.
Disable it (`--set apiServer.migrate.enabled=false`) if the DB isn't reachable yet.
`apiServer.migrate.hashPartitions` sets the partition count of each hash-partitioned
data table that a pending migration creates (default 128). A table that already
exists keeps its count. The 128 bounds each partition's HNSW index, so lower it only
where the data stays small, such as a test stack: with fewer partitions, vector
search slows down as the data grows.

Deploy a subset with the `<comp>.enabled` toggles, e.g.
`--set langfuse.enabled=false`.

## Verify

```bash
kubectl -n meko-system get pods
kubectl -n meko-mcp get pods
kubectl get managedcertificate -A          # TLS provisioning status
kubectl -n meko-system logs deploy/meko-api-server
```

## Testing a chart change

No CI runs in this repo, so these are by hand. Both are fast:

```bash
# Lint every preset combination.
for c in aws gcp; do for e in dev prod; do for m in "" ycp agent; do
  helm lint charts/meko -f charts/meko/values-$c.yaml -f charts/meko/values-$e.yaml \
    ${m:+-f charts/meko/values-$m.yaml} \
    --set-string apiServer.image.tag=t --set-string mekoUi.image.tag=t \
    --set-string mekoMcpServer.image.tag=t --set-string inferenceGateway.image.tag=t \
    --set-string provisioner.image.tag=t --set-string ragAgent.text.image.tag=t \
    --set-string ragAgent.conversations.image.tag=t \
    --set-string ragAgent.pdf.image.tag=t --set-string agent.image.tag=t \
    --set agent.ycpUrl=http://x --set agent.envName=e
done; done; done

# Unit tests (charts/meko/tests/*_test.yaml)
helm plugin install https://github.com/helm-unittest/helm-unittest   # once
helm unittest charts/meko
```

**For anything touching a shared template, diff the render against `main`.** The
data plane is live, and most changes here are meant to be invisible to it — "the
mcp renders are byte-identical" is a claim worth checking rather than assuming:

```bash
git worktree add /tmp/meko-chart-base origin/main
diff <(helm template t /tmp/meko-chart-base/charts/meko -f … ) \
     <(helm template t charts/meko                      -f … )
```

## Notes / gotchas

- **AWS parity**: `-f values-aws.yaml` renders byte-identical to the current
  kustomize manifests (verified dev+prod). GKE-only wiring (GCE ingress,
  imagePullSecrets, ManagedCertificate) is gated on `global.cloud`/ingress class.
- **rag-agent**: text and conversations need a `rag-worker` node pool
  (`node_type=rag-worker`) by default; the PDF worker needs a GPU pool
  (`node_type=gpu` + `cloud.google.com/gke-accelerator`) only while
  `ragAgent.gpu.enabled` is true (the default) — set it false on a cluster with
  no GPU pool and PDF ingestion still runs, on CPU, rather than staying
  Pending. text/conversations' `nodeSelector` isn't gated the same way; null it
  out explicitly if the target cluster has no `rag-worker` pool either.
- **CRD**: `mekouseraccounts.saas.mekodata.ai` is templated with
  `helm.sh/resource-policy: keep` — `helm uninstall` will NOT remove it (protects
  tenant data). Delete manually if you truly want it gone.
- **`apiServer.basePath` does not move the health probes.** `ingress.healthcheckPath`
  stays `/api/v1/health` under any prefix, and that is correct: kubelet reaches the
  pod directly and never passes through the proxy that supplies the prefix, so the
  api-server keeps serving health at the domain root as well. Prefixing the probe
  would fail the readiness gate forever. `tests/base-path_test.yaml` pins this.
- **Open item (§6.7)**: the CRD schema has no `langfuseImage` field though the
  sample CR sets one — k8s prunes it. Decide: add to schema or drop from CR.
