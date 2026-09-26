# AGENTS.md

Guidance for AI coding agents working in this repository.

Work is organised one directory per project, with shared components at the top level,
so **the guide you need is the one covering the tree you are editing**:

| Editing | Read |
|---|---|
| `amp/**` | [`amp/AGENTS.md`](amp/AGENTS.md) — AMP's own layout, build and test commands |
| `meko/**` | [`meko/AGENTS.md`](meko/AGENTS.md) — layout, build/run/test/lint commands, architecture notes, per-component guides |
| `ycp/**` | this file, plus [`ycp/ARCHITECTURE.md`](ycp/ARCHITECTURE.md) and [`ycp/DEPLOY.md`](ycp/DEPLOY.md) |
| `foundation/**` | [`foundation/ARCHITECTURE.md`](foundation/ARCHITECTURE.md), plus [`## foundation/`](#foundation--shared-top-level) below |

## Reading paths in this repo

Paths in root-level files — this one, and everything in `.claude/skills/` — are
relative to the **repository root**, so they carry the `meko/` or `amp/` prefix. Commands
quoted inside `meko/AGENTS.md` or `amp/AGENTS.md` are relative to that file's own
directory. Both are correct; check which altitude you are reading before copying a path.

## Required one-time git config

```bash
git config log.follow true
```

Both trees were moved into subdirectories by rename commits, and `git log` does no
rename detection without this. **Don't conclude a file has no history from a short
`git log`** — check the config first. `git blame` is unaffected. Directory-level log
can't be recovered; use a file, or an old SHA (all pre-move SHAs still resolve).

## `ycp/` — shared, top-level

The BYOC **data plane** for YCP (Yugabyte Control Plane): the pull agent
(`cmd/reconcile-agent`), the wire types both sides share (`internal/api`), and the
kind e2e. Its own `go.mod`. The control plane itself is
`meko/api_server --control-plane-mode`.

It lives at the root rather than under `meko/` because it is shared with amp. One
agent serves both, built on the backend-agnostic `reconcile` engine and picking its
backend from `ENV_KIND`: it drives a REST API — the amp controller, or meko's own
`api_server` — to converge an environment to desired state, and never applies a
manifest itself. It is that environment's sole YCP client — see
[`amp/controller/architecture/ac-ycp.md`](amp/controller/architecture/ac-ycp.md).

Its typed client for that REST API is generated from a **copy** of the controller's
`openapi_spec.yaml` vendored at `ycp/internal/ampclient/`, because the two are separate
Go modules that must not cross-import. Editing either side means re-running
`ycp/internal/ampclient/sync-spec.sh` in the same PR — a stale copy is self-consistent
and compiles, so only that check notices. The obligation is repeated for spec editors in
[`amp/controller/AGENTS.md`](amp/controller/AGENTS.md).

An **environment** is the unit YCP manages — one namespace running a Meko stack. Its
workloads come from a chart, not from the control plane: `yb-amp-controller` for an amp
environment, the `meko` chart for a meko one, and the agent is a component of that
release. Registration declares nothing of its own; the agent then converges whatever
desired state holds — for meko, `agent` and `datapack` resources through the
api-server's REST API, the same shape amp's agent uses against its controller. See
[`ycp/ARCHITECTURE.md`](ycp/ARCHITECTURE.md) and [`ycp/DEPLOY.md`](ycp/DEPLOY.md), and
[`ycp/BYOC-ON-GKE.md`](ycp/BYOC-ON-GKE.md) to stand one up on a real cluster.

```bash
# unit tests need nothing running
(cd ycp && go test ./...)

# control-plane smoke test on kind: builds the api-server image, stands it up, and
# asserts rollout -> health -> base path -> auth gate -> register -> install hint.
# Needs kind + docker or podman; no registry credentials. It does NOT cover an agent
# converging anything — every image the agent would drive is private.
(cd ycp && ./test/e2e/run.sh)

# the reconcile loop itself, on a cluster the e2e left up (YCP_E2E_KEEP=1): the
# chart-installed stack plus the reconcile-agent converging an agent and its
# datapacks, and collecting a withdrawn one. Needs Meko ECR credentials.
(cd ycp && AWS_PROFILE=meko-dev ./test/local/meko-reconcile.sh)

# the same on a REAL cluster, no kind: YCP in one namespace, a BYOC data plane in
# another.
(cd ycp && AWS_PROFILE=meko-dev ./test/local/ycp-on-cluster.sh \
   --context <ctx> --all --tag <api-server tag> --agent-tag <agent tag>)
```

The reconcile-agent image publishes to Quay on merge to `main`
(`.github/workflows/ycp-reconciler-agent-image.yml`).

## `foundation/` — shared, top-level

The YugabyteDB universe amp and meko run on, stood up with the **YugabyteDB Kubernetes
Operator** instead of a hand-rolled `yugabyted` StatefulSet. Its own `go.mod`, a CLI under
`cmd/foundation`, and unit tests that need no cluster.

It lives at the root, like `ycp/`, because it is shared with meko. How it is wired and
why — the object graph, the ordering constraints, the address contract, what is
deliberately absent — is [`foundation/ARCHITECTURE.md`](foundation/ARCHITECTURE.md). The
bullets below are the things a reader would otherwise re-derive the hard way.

`examples/amp.yaml` deploys the **unpublished build `2026.1.3.0-b21`**, for the 15-kind
CRD bundle and for `tls.extraSANs` (see the TLS note below). There is no special chart source for that: `releases.yugabyte.com` is public
for reads and helm fetches a chart from a URL, so it is `chartSource: path` with the
tarball's URL in `yba.chart` — nothing to download first, no credentials. The defaults
stay on the newest published line under `repo`, the only pairing that works with nothing
else set. The spec file is the deployer's **only** input — deploying a different build
means a different spec, which is what the foundation workflow's `chart_version` dispatch
writes for itself.

**The spec file is the source of truth and an update is a re-apply of it.** `render` emits the
CRs and applies nothing, which is what makes a change reviewable as a manifest diff and the
output consumable by GitOps. Omission is deletion: removing a key deletes the object on the
next `apply`. Nothing wraps the CR in a REST API or a task system, deliberately: it is already a declarative surface with
server-side apply and a status the operator reconciles level-triggered.

```bash
cd foundation
go test -race ./...                                        # no cluster needed
go run ./cmd/foundation validate -c examples/amp.yaml      # offline: parse + the rules YBA enforces late
go run ./cmd/foundation render -c examples/amp.yaml -o manifests/   # applies nothing
go run ./cmd/foundation up      -c examples/amp.yaml       # CRDs, yugaware, universe, databases
go run ./cmd/foundation apply   -c examples/amp.yaml       # converge after editing the spec
eval "$(go run ./cmd/foundation connstr -c examples/amp.yaml --shell)"
go run ./cmd/foundation down    -c examples/amp.yaml
```

`down` leaves the namespace and the CRDs alone by default, and both defaults are safety
properties rather than conveniences: the namespace is shared with the consumer's own pods,
and the CRDs are cluster-scoped, so deleting them takes every other foundation on the
cluster with them.

Things a reader will otherwise re-derive the hard way:

- **The Spec IS the rendered body.** Every universe default this deployer owns is applied
  at parse time (`Spec.universeDefaults`), so the `YBUniverse` entry's `Spec` and the
  rendered body are the same map and anything reading the Spec sees what the cluster gets.
  When those defaults were applied at render time instead, `sslMode` read the raw map, missed
  `enableClientToNodeEncrypt`, fell back to the CRD's `true` and emitted `sslmode=require`
  against a universe rendered with TLS off. `TestTheSpecIsTheRenderedBody` compares the
  two key sets so a default cannot drift back. The one exception is the provider's
  `kubeNamespace`, which defaults from `s.Namespace` — overridden by `--namespace` *after*
  `Load` — so `providerSpec` fills it in at render time on a deep copy.
- **Every CR is a top-level key naming its KIND**, whose value is `{name, spec}` — `name` is
  `metadata.name`, `spec` is the CR's spec body VERBATIM, and a list under the key declares
  several objects of one kind. `YBProvider` and `YBUniverse` are two such keys and nothing
  more (only `YBUniverse` is required), so any of the bundle's kinds — 12 at chart 2026.1.1,
  15 at 2026.1.2, which `examples/amp.yaml` now deploys — is reachable with no code change. An UPPERCASE-initial key is a kind and everything else must be a Spec field,
  which is what keeps `KnownFields` on for the settings — with free-form keys a mistyped
  `namespaec:` would become a CR rather than an error. **Key order is apply order** and
  teardown is its reverse, so `Load` walks a `yaml.Node` rather than decoding into a map.
  Nothing is modelled as Go structs, so every field the pinned release has is reachable and a
  new operator release needs no code change. Nothing re-checks them against the CRD's schema
  either: `kubectl apply` sends strict field validation, so the API server rejects an
  undeclared field, a wrong type and a bad enum on its own —

  ```
  .spec.numNodez: field not declared in schema
  spec.cloudInfo.kubernetesProvider: Unsupported value: "gke-typo": supported values: ...
  ```

  A copy of that schema in this repo would have to be regenerated on every chart bump to
  say the same thing slightly earlier, which is not a trade worth making. What `Validate`
  does check is only what no schema can express — YBA's own rules, which it enforces
  asynchronously inside the operator with nothing written back to the CR.
- **A `YBProvider` is optional, and its storage class wins.** The operator has an
  auto-provider, and a universe can name one somebody else registered — but a Kubernetes
  provider is what carries the DB image registry and the per-zone storage class, neither
  of which the auto-provider can infer, and on itest-dev both are non-default (`quay.io/yugabyte/yugabyte-itest`, `yb-standard`). The
  zone's `kubernetesStorageClass` takes precedence over the universe's own, per the CRD.
  The pull secret is on neither: see the bullet below.
- **Three storage classes can be in play and they are not the same one.** The provider's
  zone (which wins for the universe's PVCs), the universe's own volume blocks, and
  `yba.values.yugaware.storageClass` for yugaware's PVC — a different class again, and
  unset it silently takes the cluster default. Preflight collects all three, because an
  `Immediate`-binding class provisions the disk first and zone-pins the ~4.5 CPU yugaware
  pod before it can schedule.
- **`placementInfo` is declared, and its zone codes must be ones the provider defines.**
  YBA rejects a mismatch as a failed task tens of minutes into a bring-up, and it is a
  cross-reference between two halves of the same file, so `Validate` checks it offline.
  Whether the per-zone counts add up, and which replication factors are supported, are YBA's
  own rules and are deliberately NOT mirrored here — a copy goes stale and starts rejecting
  specs YBA would take. itest-dev is single-zone, so the example places all three RF3 nodes
  in `us-west1-b`.
- **`apply` prunes.** Anything carrying `app.kubernetes.io/managed-by=meko-foundation` in the
  namespace that the spec no longer declares is deleted, after the universe settles. The
  candidate kinds come from `kubectl api-resources`, not from a list in Go, so nothing here
  needs keeping in step with the bundle. The label is the ownership boundary — a CR somebody
  else created is never touched — and `--no-prune` opts out.
- **The MAGE gflag is seeded at CREATE, and drags a second gflag with it.**
  `ysql_yb_enable_mage` is a start-time gflag on both the master and the tserver — the
  extension will not install without it and turning it on later is a rolling restart — and
  it is a *preview* flag, which YBA refuses unless `allowed_preview_flags_csv` names it.
  That CSV is itself an ordinary gflag and must list **every** preview flag in use, so
  adding a second one means rewriting the value to carry both rather than adding a key.
  Nothing here validates that: which flags are preview is YB's to decide and moves between
  releases, so YBA is the authority and a mirror of its list in this repo would go stale.
- **YBA needs EXACTLY ONE customer or the operator refuses every CR**, and the chart
  creates it from a **post-install** hook gated on `yugaware.defaultUser.enabled`, which
  the chart defaults *off*. Two consequences. Setting the `defaultUser` fields without the
  flag does nothing — the whole Job template is behind it — so `HelmSets` forces the flag
  on. And because it is post-install, a release that already exists without a customer
  never gets one: every later `helm upgrade --install` is an upgrade, the hook does not
  run, and helm reports success. The operator then throws in `getOperatorCustomer`
  *before* it can write a status, so the CR sits with an **empty** status and the only
  evidence is a stack trace in the yugaware log — indistinguishable from a slow bring-up,
  which is why `checkYBACustomer` reads the count over YBA's own unauthenticated
  `customer_count` and fails the install instead. A second customer (the console can
  create one) breaks it the same way.
- **`status` has no `observedGeneration`.** Right after an apply the operator has not looked
  at the new spec and `universeState` still reads `Ready` from the *previous* generation, so
  a wait on state alone reports an edit as applied before it starts. `ybuniverse.Settled`
  therefore also requires no `status.actions` entry to be `queued` or `running`.
- **`YB_HOST_PORTS` is the ONLY address the deploy path hands compute, and both lanes come
  off it.** The YSQL pool uses it as given (5433); the direct-RPC lane *derives* its
  address by appending 9100 to that host, because nothing can configure it — the deploy
  path has no way to set `tserver_rpc_addrs` (`endpoint_supervisor` writes only
  `host_ports`, `user`, `dbname`, `sslmode`, `sslrootcert`, `password_env`). That is why
  the Service the CR asks for carries **both** client ports.
- **It must stay a Service name, never a per-pod list.** A full move renames the tserver
  pods, and the compute daemons cannot pick up a new list — the thin client re-resolves its
  configured strings on recycle but never re-reads config, and the DSN reaches endpoint
  pods as an env var, which Kubernetes never updates in a running container. The address
  has to be a name whose membership changes underneath it.
- **Both default addresses are intra-cluster only.** The headless tserver Service and the
  tserver-RPC ClusterIP are unreachable from another cluster — a ClusterIP is a virtual
  address existing only as kube-proxy rules on its own nodes, so no amount of VPC peering
  routes to it. A cross-cluster foundation needs an internal LoadBalancer (the same
  `serviceEndpoints` entry with `type: LoadBalancer` plus the cloud's internal-LB
  annotations) or an MCS clusterset name, which supplies discovery only and still needs
  flat pod-level connectivity. A clusterset name additionally needs
  `multicluster.createServiceExports`, which rewrites node broadcast addresses so the smart
  driver's per-pod stage resolves cross-cluster, and a `ServiceExport` for the Service —
  which is `exportService: true` on the entry, from chart 2026.1.3. Both routes are a spec
  edit and no code change; `examples/amp.yaml` carries the export commented out. Either way
  the address goes wherever the consumer is configured, not in this spec.
- **The spec carries no address knob at all.** The address is the Service the chart created
  in the namespace just deployed into, which `checkTServerService` verified. Which address
  a *consumer* dials is a property of the consumer's topology, not of the universe, so it
  belongs where the consumer is configured — `redeploy.sh` reads `YB_HOST_PORTS`/`DB_URL`
  from env, the install hint has `YB=<host>:5433` — and a single field on the foundation
  could not express two consumers in two places anyway.
- **The universe lands wherever the provider zones' `kubeNamespace` says** — that is the
  field YBA obeys, and when no zone sets one YBA invents a namespace
  (`yb-<customer code>-<universe>-<suffix>`) and the tserver Service ends up somewhere no
  consumer predicted. `providerSpec` therefore defaults it to the foundation's namespace,
  so co-location is the default. A zone naming its own is honoured and is how you put the
  universe elsewhere; `Spec.UniverseNamespace` is then what the DSN and every
  universe-facing lookup use (`kubectlUniverse`), while the CRs stay in the foundation's
  namespace because that is the one the operator watches. The zones must agree on one
  namespace — those lookups run in exactly one, and a per-zone address is a different
  feature.
- **The DB image's pull secret is YBA's to place, not ours.** YBA derives it from its own
  (`yba.values.image.pullSecret`) and propagates it into the namespace it deploys nodes
  into. That is the only thing that can work: the `YBProvider` CRD has no field for
  pull-secret *content* — only `kubeConfigSecret` — so a CR can name a Secret but never
  put one in a namespace YBA created. Consequences: the spec sets no
  `kubernetesOverrides.Image.pullSecretName` (naming one risks naming something absent
  beside the DB pods), preflight checks only yugaware's own, and the robot behind that
  secret needs pull access to the DB image repo as well as the YBA one.
- **The tserver Service is created by the CHART, and the SPEC is what asks for it** — a
  `kubernetesOverrides.serviceEndpoints` entry (`type: ClusterIP`, `scope: Namespaced`,
  ports `tcp-ysql-port: 5433` and `tcp-rpc-port: 9100`) plus `enableLoadBalancer: true`.
  **The deployer applies only what the spec says**; nothing about the universe is filled
  in on its behalf, so the CRD's own defaults decide anything omitted — and
  `enableLoadBalancer` defaults FALSE, which renders no endpoints at all. `Validate`
  therefore REFUSES a spec that leaves the contract out, reporting every problem at once:
  without that Service the deployer cannot finish either, so there is nothing to warn
  about. It has no opinion on encryption — a spec may say nothing about either flag — but
  `SSLMode` has to answer for the universe that WILL exist, so `CRDEncryptionDefault`
  mirrors the CRD's `true` and an absent flag reads as on. That is the one CRD default
  this module copies, and a chart bump re-checks it. `up` and `apply` warn when a spec
  inherited it rather than said it, and REFUSE when it disagrees with a universe that
  already exists — the old deployer wrote both flags `false`, so a migrating spec has to
  write them explicitly or claim TLS against a plaintext database.
  **Migrating is spec-first: add the block, apply with the old deployer, then upgrade.** The chart
  supplies the selector, so there is nothing to guess. Its NAME, though, is not ours to
  predict: a Namespaced endpoint is called
  `<commonLabels."app.kubernetes.io/part-of" | "namespaced">-<endpoint name>`, and YBA
  always sets that label — to the universe name it generated, suffix included. So the real
  Service is `foundation-1954916679-yb-tserver-conn`, and it is found by the
  `serviceName=yb-tserver-conn,scope=Namespaced` labels, which the chart writes from
  values the CR sets. Three consequences: `enableLoadBalancer` is forced true
  because the chart gates that whole template on it (it is *not* a request for public
  exposure — overriding the endpoint list replaces the chart's three LoadBalancer
  defaults); `scope: Namespaced` needs `oldNamingStyle` off, which is YBA's choice and
  absent from the CRD's `kubernetesOverrides` schema, so `checkTServerService` verifies the
  result instead of assuming it; and the Service carries
  `helm.sh/resource-policy: keep`, so `helm uninstall` will not remove it — its lifecycle
  belongs to the universe and YBA reclaims it, which the foundation CI asserts.

`foundation/ybuniverse` is pure — no I/O, no binaries, no Kubernetes client — because the
amp controller ships helm but no kubectl and the BYOC agent is distroless-static, so a
consumer that manages a foundation has to speak the k8s REST API directly.

The operator path is **not** the default anywhere yet: `amp/deploy/up.sh`,
`amp/deploy/up-gke.sh`, `meko/deploy/helm/meko/scripts/deploy-gke.sh` and
`ycp/cmd/agent` all still bring up their own single-node `yugabyted`. That is a decision,
not an oversight — YBA is far too heavy for kind, so the operator path soaks in exactly one
place first: `/ci amp_deploy_gke_foundation` and its nightly
(`.github/workflows/amp-foundation-gke.yml`), which deploys `up-gke.sh` with
`FOUNDATION=operator` and runs the agent-driven reconcile smoke against the universe.

That universe is **encrypted**, and on this lane TLS is not an input: both encryption
flags are create-time only, so the spec decides and `up-gke.sh` reads the shape back from
`foundation validate --json` rather than being told. It needs chart **2026.1.3** — the
node certificates have to name the endpoint Service, which is the only address consumers
get and is in none of the SANs the chart computes for itself, and
`kubernetesOverrides.tls.extraSANs` is what adds it. The CA is a `YBCertificate` pointing
at the same `amp-ca` root the amp endpoints already use, so nothing new is trusted.

## Repo-wide conventions

### Prerequisites

An **authenticated GitHub CLI (`gh`)** is required — this repo is private, so
unauthenticated access silently returns the wrong thing rather than failing. Check
with `gh auth status`; if it is missing, stop and have the user run `gh auth login`
(in Claude Code, `! gh auth login` so the output lands in the conversation). Do not
work around it by fetching GitHub URLs with a web fetcher or asking the user to paste
output by hand.

### Branch naming

Branches are **`<username>/<kebab-topic>`** or **`feature/<kebab-topic>`**.
`.claude/hooks/check-branch-name.sh` blocks a non-conforming `git push`, and the
`branch-names-require-prefix` ruleset enforces the weaker half remotely. Full rules
and the hook's test suite: [`meko/AGENTS.md`](meko/AGENTS.md).

### Tracking references

Commit subjects and PR titles carry `[MEKO-<num>]` for meko work or `[#<issue>]` for
AMP work, and **omit the tag entirely** when there is genuinely no ticket — never
invent a placeholder or guess a number.

#### `[#N]` in commits that predate the monorepo merge means an *amp* number

AMP's 69 open issues were transferred out of `yugabyte/amp` and **renumbered**, so a
`[#N]` tag on any commit from before the merge refers to amp's old sequence, not to
`#N` in this repo. amp's numbering reached **#447** (issues and pull requests share one
sequence), so treat any `[#N]` with **N ≤ 447** on a pre-merge commit as an amp number
and resolve it before assuming it points here.

| amp numbers | where they are now |
|---|---|
| the 69 **open** issues, scattered over `#5`–`#440` | moved here as `#432`–`#500` — [mapping table](https://github.com/yugabyte/meko/issues/393#issuecomment-5139709911) |
| the 175 **closed** issues, up to `#433` | left in `yugabyte/amp`; still resolve there |
| pull requests | left in `yugabyte/amp` |

The ranges overlap, which is the trap: `#432`–`#500` here *are* the migrated amp issues,
but everything below that is meko's own. A live example — `[#244]`, the tag on AMP's
ImageVolume work:

- amp#244 "control_plane: support Postgres extensions via k8s ImageVolume" → now
  [meko#474](https://github.com/yugabyte/meko/issues/474)
- meko#244 is "Add discord link to Meko app and remove Slack link" — an unrelated meko PR

Old `yugabyte/amp` issue URLs redirect to the new location, so prefer pasting a full URL
over a bare `#N`.

### Prose discipline — write for the reader, not for volume

AI-written text runs long: PR descriptions that narrate the diff, comments that restate
the line below them, architecture docs and runbooks padded with editorial framing. It
reads as thorough and costs real review time — reviewers spend their budget deleting it
instead of checking the change, and some have stopped reviewing architecture-doc diffs
altogether. Every extra sentence is also one more claim that has to stay true as the code
moves, which is why the long version is the *harder* one to maintain.

There are no word counts here. Judge each piece by what a reader who already knows this
repo needs:

- **Code comments** — the *why*, when it isn't obvious from the code: an invariant, an
  ordering constraint, a workaround and its cause. Not a restatement of the line below,
  not section captions, not narration of the change you just made.
- **PR descriptions** — the motivation (a reviewer who doesn't know why is the expensive
  case), a high-level summary sized to the change, roughly a line per file, and whatever
  the reader must *act* on: new config variables, deploy or migration steps. Not a
  paragraph per file, not a narration of the diff.
- **Architecture docs and runbooks** — how the system is wired today, why it's built that
  way, and what an operator does. Cut restated context and the history of what the code
  used to do.
- **Commit messages** — subject, plus the why when the why isn't obvious.

The test for a sentence: would a reader who knows this repo do or believe anything
differently without it? If not, cut it.

The `commit` skill re-checks this at commit time — guidance that lives only here is
remembered at the start of a task and gone by the end of one.

### Reviewing

[`REVIEW.md`](REVIEW.md) routes to the right guide — [`amp/REVIEW.md`](amp/REVIEW.md)
or [`meko/REVIEW.md`](meko/REVIEW.md). Each project keeps its own because the concerns
differ; both are titled `# PR review guide`, so check which tree the PR touches.

### On-demand CI (`/ci` directives)

Some AMP suites are too heavy for every PR and are opt-in: add a `/ci <suite>` line to
the **PR description**, before opening it — editing the description afterwards fires
nothing.

| Directive | Runs |
|---|---|
| `amp_integration_slow` | slow integration scenario suite. Also automatic when the diff touches `amp/test/integration/compute/slow/` |
| `amp_generate_image` | controller container image build |
| `amp_extensions_local` | local-deploy extension regression test (`amp/test/integration/compute/extensions.sh --regress`): builds the real extension image and runs each externalized extension's own upstream regression suite against a throwaway `yb_amp_local` cluster. Also automatic when the diff touches `amp/deploy/extensions/`, and nightly on `main`. The lighter twin of `amp_deploy_gke` for extension-pipeline changes |
| `amp_deploy_gke` | build + push four images to Quay, deploy to the itest-dev GKE cluster, smoke test. Also nightly on `main` |
| `amp_deploy_gke_foundation` | the only suite that runs the **operator-managed foundation**, and the only one that runs an **encrypted** universe end to end: deploys with `FOUNDATION=operator` so `up-gke.sh` calls the `foundation/` deployer instead of its yugabyted StatefulSet, asserts the node certificates name the endpoint Service and that the session really is TLS, runs the agent-driven reconcile smoke against that universe, then tears it down and asserts no cluster-scoped RBAC leaked. Its own workflow and cron (03:17 UTC). Also nightly on `main` |

They are all `amp_`-prefixed because they belong to AMP; there is nothing equivalent for
meko yet. Full contract (including `amp_deploy_gke_extensions` and what makes a change
correctness-sensitive): [`amp/docs/ci-directives.md`](amp/docs/ci-directives.md).

### Agent skills (`.claude/skills/`)

Pointers only; each skill is the authority on its own subject.

| Skill | Read it before |
|---|---|
| `commit` | any commit — it gates the companions (docs, tests, generated files, setup) that must travel in the same commit, and the prose trim above |
| `git-push` | pushing a branch, or refreshing an existing PR |
| `open-pr` | opening a new PR |
| `merge-pr` | merging |
| `create-amp-issue` | filing or updating a GitHub issue for AMP work |
| `resolve-pr-comments` | closing out a review — resolving addressed threads, and 👍/👎 on bot findings so the reviewer improves |
| `integration-before-corruption-claim` | claiming data loss or corruption in `amp/` — run the integration suite first |
| `amp-mcp-tools` | working on the amp controller's MCP tools. A **symlink** to `amp/controller/mcp/skills/amp-mcp-tools`, so it stays in step with the packaged skill bundle |
| `amp-lru` | adding, changing or retiring an amp LRU (an always-on real workload on amp with its own pool, pods, checker and dashboard) — the shape every LRU follows, the portal row, the hourly sync. Also read `amp/deploy/lru/README.md` for the shape and [`amp/docs/lru-environment.md`](amp/docs/lru-environment.md) for what is deployed where today |
| `amp-report-service` | working with the amp QA report portal — triaging runs/results, perf history, known issues, launching runs/builds/fleet deploys via the `yb` CLI or the portal's MCP server |

All skills are repo-wide and live at the root, including the AMP-specific ones — there is
no per-tree `.claude/`. The two `amp`-scoped entries above only apply when you are editing
`amp/**`, but they are indexed here so a single list stays authoritative.
