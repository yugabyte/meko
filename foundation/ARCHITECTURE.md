# Foundation Architecture

A **foundation** is the YugabyteDB universe amp and meko keep their durable state
in, plus everything needed to stand it up and reach it. This module brings one up
with the **YugabyteDB Kubernetes Operator**: apply the operator's CRDs, install
the `yugaware` chart with the operator enabled, apply a `YBProvider` and a
`YBUniverse`, wait, create the databases, and report one address.

It replaces a hand-rolled single-node `yugabyted` StatefulSet — RF-1, no rolling
upgrade, no backups, no lifecycle management — with a real universe, and buys the
day-2 CRD surface (Release, Backup, RestoreJob, StorageConfig, SupportBundle,
BackupSchedule) that [#729](https://github.com/yugabyte/meko/issues/729) builds
on.

It lives at the repository root rather than under `amp/` for the same reason
`ycp/` does: it is shared with meko.

## What actually runs

Nothing of ours runs in the cluster. The CLI is a short-lived process that
writes CRs and polls; the only long-lived controller in the picture is **YBA's
operator thread, inside the yugaware pod**. That is the whole reason this module
can be a CLI rather than a controller: the reconcile loop already exists and is
somebody else's.

```
   spec file
       │  foundation CLI — a short-lived process on a workstation or CI runner
       │  kubectl apply --server-side;  helm upgrade --install
       ▼
   ┌───────────────────────────────────────────────────────────────────┐
   │  FOUNDATION NAMESPACE                                             │
   │                                                                   │
   │   YBProvider        YBUniverse          ← the only thing we write │
   │        └────────┬────────┘                                        │
   │                 │ watched by                                      │
   │                 ▼                                                 │
   │   yugaware-0 ─ yugaware container ─ operator thread + task engine │
   │              ─ postgres  (YBA's own state)                        │
   │              ─ prometheus                                         │
   │                 │                                                 │
   │                 │ helm install, chart yugabytedb/yugabyte,        │
   │                 │ once per zone                                   │
   │                 ▼                                                 │
   │   UNIVERSE NAMESPACE  (this one, unless a provider zone says      │
   │                        otherwise)                                 │
   │                                                                   │
   │     yb-master-{0,1,2}          yb-tserver-{0,1,2}   StatefulSets  │
   │                                        ▲                          │
   │     <universe>-yb-tserver-conn ────────┘                          │
   │       ClusterIP, ports 5433 (YSQL) and 9100 (tserver RPC)         │
   │                        ▲                                          │
   └────────────────────────┼──────────────────────────────────────────┘
                            │  YB_HOST_PORTS / DB_URL, injected as env
                            │  at deploy time and never rewritten
              amp controller, endpoint pods, meko api-server
```

**Control flow is one-way and level-triggered.** The CLI applies a CR and then
only *reads*: the operator picks the change up, turns it into a YBA task, and YBA
runs helm against the yugabyte chart to make the StatefulSets match. Status comes
back the same way — YBA updates the task, the operator writes `status`, the CLI
polls it. Nothing here talks to YBA's REST API, and no API token exists on this
path; the operator acts under its own ServiceAccount, which is what the
ClusterRoles `rbac.create` makes are for.

**Data flow does not involve any of that.** Once the universe is up, consumers
resolve one Service name and talk to tservers directly. The control plane can be
entirely down — yugaware deleted, operator gone — and the database keeps serving.
That separation is worth knowing when triaging: a broken CR status says nothing
about whether queries are working, and vice versa.

**Two hops of indirection are deliberate.** The CLI does not create DB pods, and
does not create the Service either. YBA owns the first (so rolling upgrades,
backups and full moves are its problem, not ours) and the chart owns the second
(so the selector is never something we guess). What the CLI owns is the narrow
part nothing else does: ordering, waiting, and reporting one address.

## The shape of the thing

**The spec file is the source of truth and an update is a re-apply of it.**
`render` turns it into manifests and touches nothing; `up` and `apply` send those
to a cluster and wait. There is deliberately no REST API and no task system over
the CR — see [What is not here](#what-is-not-here).

```
foundation/
  spec.go        the Spec: parse, defaults, and the rules YBA enforces too late
  render.go      Spec -> manifests, the helm --set list, the CRD URL
  crds.go        kubectl apply --server-side of the operator CRD bundle
  yba.go         helm upgrade --install of yugaware, and the customer check
  universe.go    apply the CRs, wait for the provider, wait for the universe
  conn.go        find the tserver Service, build the DSN, create databases
  down.go        ordered teardown
  deployer.go    the converge sequence and its preflight
  runner.go      the command-runner seam every kubectl/helm call goes through
  ybuniverse/    CR rendering and status interpretation — pure, no I/O
  cmd/foundation render | validate | up | apply | status | connstr | down
```

`ybuniverse/` is a separate package and has no I/O, no binaries and no Kubernetes
client on purpose. The amp controller ships helm but no kubectl and is explicitly
"not a client-go deployer", and the BYOC agent is distroless-static — so a
consumer that manages a foundation has to speak the k8s REST API directly, and
keeping CR rendering and status interpretation free of transport is what lets the
same code serve the shelling CLI here and a REST client later.

`kubectl` and `helm` stay shelled ambient-authed binaries rather than linked SDKs
(the ampqa convention), and every invocation goes through `Runner`, so the whole
deployer is testable without a cluster.

## Who owns what on a cluster

| Object | Created by | Scope | Removed by |
|---|---|---|---|
| CRDs (15 in the bundle; we use 2) | `applyCRDs` | **cluster** | nothing, by default |
| `<ns>-yugaware` helm release | `installYBA` | namespace | `down` |
| ClusterRole/Binding `<ns>-yugaware` | the chart | **cluster** | `down`, by name |
| the single YBA customer | the chart's post-install Job | — | with the release |
| `YBProvider` / `YBUniverse` | `applyCRs` | namespace | `down`, in order |
| master/tserver pods, PVCs | **YBA**, from the CRs | universe namespace | YBA, on CR delete |
| the tserver `ClusterIP` Service | **the YugabyteDB chart** | universe namespace | YBA, with the universe |
| `yb_storage` | `ensureDatabases` | in the DB | — |

Two of those rows are the ones people get wrong.

**The CRDs are cluster-scoped and shared.** `applyCRDs` runs on every converge and
overwrites them, so the bundle installed on a cluster is whichever foundation
converged last. Two foundations pinned to different `chartVersion`s will fight;
the CI workflow's single fixed concurrency group is the mitigation. `down` leaves
them alone by default because deleting them takes every other foundation's
universe on the cluster with them.

**The tserver Service is created by the chart, not by us.** The CR carries a
`kubernetesOverrides.serviceEndpoints` entry and the chart renders the Service
from it, selector included — which removes a whole bug class, because the chart
selects on the app label *plus* `release=<helm release>` and the release label is
what scopes it to one universe. Consequences worth knowing:

- `enableLoadBalancer` is forced true because the chart gates that entire
  template on it. It is **not** a request for public exposure: overriding the
  endpoint list *replaces* the chart's three LoadBalancer defaults.
- `scope: Namespaced` needs `oldNamingStyle` off, which is YBA's choice and
  absent from the CRD's `kubernetesOverrides` schema, so `checkTServerService`
  verifies the result rather than assuming it.
- The Service carries `helm.sh/resource-policy: keep`, so `helm uninstall` will
  not remove it. Its lifecycle belongs to the universe and YBA reclaims it, which
  the CI asserts.

## The spec, and where validation lives

`provider:` and `universe:` are the `YBProvider` and `YBUniverse` **spec bodies
verbatim**, and `yba.values:` is yugaware chart values verbatim. None is a
translated subset. The alternative — a Go struct per CR, extended every time
someone needs a field nobody modelled and re-extended on every operator release —
is what this replaced. With passthrough, `chartVersion` alone decides which
release's fields are legal and a new field needs no code.

Nothing here re-implements the CRD's schema. `kubectl apply` sends strict field
validation, so the API server rejects an undeclared field, a wrong type, a bad
enum and an immutable-field change by itself:

```
.spec.numNodez: field not declared in schema
spec.cloudInfo.kubernetesProvider: Unsupported value: "gke-typo": supported values: "gke", …
```

A copy of that schema in this repo would need regenerating on every chart bump to
say the same thing slightly earlier, and would go stale between bumps. The cost
of not having it is that a passthrough typo surfaces on apply rather than in a
cluster-less test.

`Spec.Validate` therefore covers only what no schema can express — things the CRD
happily accepts and **YBA then rejects asynchronously**, often with nothing
written back to the CR:

| Rule | What happens without it |
|---|---|
| `ybSoftwareVersion` non-empty | `universeState: "Error Creating"`, action still `queued`, reason only in the yugaware log |
| `ybSoftwareVersion` is a release YBA has (`installYBA`, not `Validate` — it needs the cluster) | the same opaque `Error Creating`, from `Helm Package path not found for release` |
| `replicationFactor` ∈ {1,3,5,7}, `numNodes ≥ RF` | the CRD constrains neither |
| `placementInfo` zones exist in the provider, counts sum to `numNodes` | a failed task, tens of minutes in |
| a provider block or a `providerName` | the auto-provider cannot supply registry or storage class |
| provider zones agree on one `kubeNamespace` | this deployer's lookups run in exactly one |

### The Spec is the rendered body

Every universe default this deployer owns is applied at **parse time**
(`Spec.universeDefaults`), so `s.Universe` and the rendered `YBUniverse` spec are
the same map. Anything reading the Spec — `sslMode`, the storage-class preflight,
`Validate` — sees exactly what the cluster gets.

This is load-bearing rather than tidy. When those defaults were applied at render
time into a copy, `s.Universe` was authoritative for some fields and stale for
others, and `sslMode` read the stale one: it saw no `enableClientToNodeEncrypt`,
fell back to the CRD's default of `true`, and emitted `sslmode=require` for a
universe rendered with TLS off. `TestTheSpecIsTheRenderedBody` compares the two
key sets in both directions so a default cannot drift back.

The provider's `kubeNamespace` is the one exception, for a real reason: it
defaults from `s.Namespace`, which `--namespace` overrides *after* `Load`. So
`providerSpec` fills it in at render time, on a deep copy — `Render` must be
callable twice with the same result.

## Bring-up

`up` and `apply` share one path; the split is only about which steps are expected
to be no-ops.

1. **Preflight** — binaries on PATH, cluster reachable, helm ≥ 3.11, k8s ≥ 1.27,
   every storage class the spec names exists (and warns unless it binds
   `WaitForFirstConsumer`), every pull secret the spec names exists, and
   `yba.values.yugaware.defaultUser.password` is set.
2. **CRDs** — `kubectl apply --server-side -f <bundle for chartVersion>`, with a
   local-path override for a cluster with no egress.
3. **Namespace** — created if absent, otherwise left exactly as it is. It is
   shared with the consumer's own pods, so the `managed-by` label records only
   that a foundation lives there; it is not an ownership claim.
4. **yugaware** — `helm upgrade --install --wait`, values file first and
   `--set-string` after, because helm gives `--set` precedence over `-f` and the
   settings in `HelmSets` must not be switchable off by a values file.
5. **Customer check** — YBA must have exactly one, or the operator refuses every
   CR. See below.
6. **Provider**, then wait for it to register. Separate from the universe's wait
   because YBA rejects a universe naming an unregistered provider
   *asynchronously, in the universe's status* — a much worse place to read "the
   provider wasn't ready yet".
7. **Universe**, then wait for it to settle.
8. **Service check**, **databases**, **connection info**.

### Three ordering constraints that are not obvious

**The customer must exist before any CR is applied.** The chart creates YBA's one
customer from a **post-install hook** gated on `yugaware.defaultUser.enabled`,
which the chart defaults *off* — so setting the `defaultUser` fields alone renders
no Job at all and helm reports success. `HelmSets` forces the flag. Because it is
post-install, it never runs on an upgrade: a release that already exists without a
customer can never gain one, and every converge will report success while the
operator throws

```
OperatorUtils.getOperatorCustomer: Customer list does not have exactly one customer
```

*before* it can write a status — so the CR sits with an **empty** status and the
only evidence is in the yugaware log. `installYBA` reads YBA's own unauthenticated
`customer_count` and fails with the reason rather than letting the universe wait
time out. More than one customer breaks it the same way.

**The DB release has to exist in the yugaware image, and that image drifts.** YBA needs a
helm *package* for `ybSoftwareVersion`, not just the version string, and it only checks at
universe-create — asynchronously, with the reason in its own log. Which releases a yugaware
carries is decided by its image, pinned by the chart's `appVersion`, and **a published
chart version is mutable**: `2026.1.1` was re-cut on 2026-09-10 with `appVersion` moving
`2026.1.1.1-b2` → `2026.1.1.2-b9`, breaking a spec that had been green for days with
nothing in it changed. `checkDBRelease` lists `/opt/yugabyte/releases` in the pod and fails
before the universe is applied, naming both versions. Pinning `image.tag` would trade this
for a chart-vs-image mismatch, so the check is the fix rather than more pinning.

**`yb_storage` must exist before the consumer rolls out.** The amp controller's
`db.Connect` targets it at startup, which is why database creation is a step of
the bring-up rather than something the consumer does.

**Teardown order is the reverse, and stranding a finalizer is unrecoverable.**
Both CRs carry the operator's finalizer and only the operator can clear it, so
`down` deletes the universe, then the provider, then uninstalls yugaware. Doing it
the other way leaves a finalizer with nothing to satisfy it and the namespace
hangs in `Terminating` forever — `kubectl delete ns` is a no-op on a Terminating
namespace, so nothing recovers it.

## Namespaces

Two, and they are the same one by default.

- **The foundation's namespace** (`spec.namespace`) holds yugaware, the operator,
  both CRs — and, as every consumer in this repo deploys it, the consumer's own
  pods.
- **The universe's namespace** is the provider zones' `kubeNamespace`. That is the
  field YBA obeys, and when no zone sets one **YBA invents a namespace**
  (`yb-<customer code>-<universe>-<suffix>`) and puts the DB pods there. So
  `providerSpec` writes it into every zone, defaulted to the foundation's
  namespace, materialised in the manifest rather than left blank.

A zone naming its own namespace is honoured and is how you place the universe
elsewhere; `Spec.UniverseNamespace` is then what the DSN and every
universe-facing lookup use (`kubectlUniverse`), while the CRs stay where the
operator watches. All zones must agree on one, because those lookups run in
exactly one namespace — zones spread across several would need a per-zone
address, which is a different feature.

The DB image's pull secret is **YBA's to place**, not ours: it derives one from
its own (`yba.values.image.pullSecret`) and propagates it into the namespace it
deploys nodes into. That is the only thing that can work, because the
`YBProvider` CRD has no field for pull-secret *content* — only `kubeConfigSecret`
— so a CR can name a Secret but never put one in a namespace YBA created. The
robot behind the yugaware secret therefore needs pull access to the DB image repo
as well.

## The address contract

`connstr` emits exactly two things: `YB_HOST_PORTS` and `DB_URL`. That is the
whole interface between a foundation and its consumer.

**One address serves both compute lanes**, because the direct-RPC lane cannot be
addressed separately. The deploy path has no way to set `tserver_rpc_addrs` —
`endpoint_supervisor` writes only `host_ports`, `user`, `dbname`, `sslmode`,
`sslrootcert` and `password_env` — so `ysql_client`'s
`effective_tserver_rpc_addrs` falls through to appending 9100 to each
`host_ports` host. The Service therefore carries **both** client ports, 5433 and
9100, and the YSQL pool uses the host as given while the RPC lane derives the
same host on 9100.

**It is a Service name, never a per-pod list.** A full move renames the tserver
pods, and the compute daemons cannot pick up a new list: the thin client
re-resolves its configured strings when a connection generation recycles but
never re-reads config, and the DSN reaches endpoint pods as an env var, which
Kubernetes never updates in a running container. The address has to be a name
whose *membership* changes underneath it.

**The Service is found by label, not named.** A Namespaced-scope endpoint is
called `<commonLabels."app.kubernetes.io/part-of" | "namespaced">-<endpoint
name>`, and YBA always sets that label — to the universe name it generated,
suffix included. So the real Service is `foundation-1954916679-yb-tserver-conn`,
and `tserverServiceName` selects on `serviceName=yb-tserver-conn,scope=Namespaced`
— labels the chart writes from values the CR sets.

**`sslmode` comes from the rendered universe**, so the DSN and the CR cannot
disagree: `disable` against a TLS universe gives `no pg_hba.conf entry … no
encryption`, and `require` against a plaintext one fails just as hard.

**One Service, shared by every AZ release in the namespace.** YBA installs the
yugabyte chart once *per zone*, so a three-zone universe is three helm releases.
`scope: Namespaced` is what makes them share a single tserver Service instead of
getting one each: the chart renders it only for the owning release
(`yugabyte.should_render_service`), drops `release` and `yugabyte.io/zone` from
its selector so it matches every zone's tservers, and marks it
`helm.sh/resource-policy: keep` so a non-owning release's uninstall cannot take
it from the others. That is also the second reason every provider zone must name
the same `kubeNamespace` — split them across namespaces and you get one Service
per namespace, each fronting a subset of the universe.

**Cross-cluster: the ClusterIP is the wrong shape, and MCS is the right one.** A
ClusterIP is a virtual address existing only as kube-proxy rules on its own
nodes, so no amount of VPC peering routes to it. Two ways out, neither wired up
here:

- **An internal LoadBalancer** — the same `serviceEndpoints` override with
  `type: LoadBalancer` plus the cloud's internal-LB annotations. No code change,
  no chart change.
- **An MCS clusterset name** (`<svc>.<ns>.svc.clusterset.local`), which unions
  the endpoints of like-named Services across the ClusterSet. This is the
  **namespace sameness** model: the namespace name and the Service name must be
  identical in every cluster, which the `Namespaced` scope already gives —
  `namespaced-`/`<part-of>-` prefixing is derived from the universe, not from the
  release or the zone, so every cluster's copy lands on the same name.

  What was missing was the export. `multicluster.createServiceExports` only
  exports the headless `yb-masters`/`yb-tservers` Services, so `serviceEndpoints`
  Services stayed cluster-local and were not resolvable from peer clusters —
  clusterset DNS would resolve the name and reach a subset of the pods.
  [yugabyte/charts#228](https://github.com/yugabyte/charts/commit/54de0159e6f1b3af21d5a87cdedf7a1d701d108c)
  closes that with a per-endpoint `exportService: false` flag that emits a
  `ServiceExport` beside the endpoint's Service, same name and labels, under
  `multicluster.mcsApiVersion`. It is **not in the pinned 2026.1.1**, so reaching
  it here needs a `chartVersion` carrying it; once there it is a passthrough
  field on the CR's existing `serviceEndpoints` entry and needs no code change.

  Two caveats survive it. MCS is a spec needing an implementation (GKE MCS, AWS
  Cloud Map, Submariner, Cilium ClusterMesh), and it supplies **discovery, not
  connectivity** — pod-level reachability between the clusters has to already
  exist. And the smart driver uses the configured name only to bootstrap, then
  addresses tservers by the per-pod hostnames `yb_servers()` reports, so the YSQL
  lane can discover names the consuming cluster cannot resolve unless
  `multicluster.createServiceExports` is on as well. The direct-RPC lane is
  unaffected — it never leaves the configured address.

Either way the address is supplied where the *consumer* is configured —
`redeploy.sh`'s env, the install hint's `YB=` — not in this spec: which address a
consumer dials is a property of its topology, not of the universe, and one field
here could not express two consumers in two places.

## Readiness

`YBUniverse.status` has **no `observedGeneration`**. Right after an apply the
operator has not looked at the new spec and `universeState` still reads `Ready`
from the *previous* generation, so a wait on state alone reports an edit as
applied before it starts. `ybuniverse.Settled` therefore also requires no
`status.actions` entry to be `queued` or `running`; the operator enqueues an
action before it begins work, which is what closes the hole.

`ybuniverse.Failed` covers two shapes. An action can carry `failed` with a message
and a task UUID — the good case. But YBA can also reject a create *before* it
submits a task, leaving the action `queued` with no message while `universeState`
becomes `Error Creating`; matching only the action status would wait that out for
the full timeout. The CRD documents four states as examples with no enum, and
`Error Creating` is not among them, so the match is on the `Error` prefix.

## Day-2

Edit the spec file and `apply`. Reviewable as a manifest diff via `render`, which
is the review gate GitOps gives for free.

| Change | Field |
|---|---|
| software upgrade | `ybSoftwareVersion` (with `upgradeOption`) |
| scale out **and in** | `numNodes`, `placementInfo` |
| vertical scale | `masterResourceSpec`, `tserverResourceSpec` |
| gflags | `gFlags.masterGFlags` / `tserverGFlags` |
| disk grow | `tserverVolume` / `masterVolume` — in-place grow only |
| pause / resume | `paused` |
| helm overrides | `kubernetesOverrides` |

**Create-time only** — changing these rebuilds the universe rather than editing
it, because the CRD marks them `self == oldSelf`: `enableClientToNodeEncrypt`,
`enableNodeToNodeEncrypt`, `enableYSQLAuth`, `replicationFactor`, `rootCA`.
Changing a storage class or volume *count* is a "full move" rather than an
expand.

Both encryption flags default to **`true`** in the CRD, so a spec that says
nothing gets a universe it cannot be talked out of. `universeDefaults` writes both
as `false`: client-to-node off keeps the address contract identical to the
yugabyted path, and node-to-node off is a hard requirement, because it encrypts
the tserver RPC port and the compute hot path speaks pggate to 9100 directly with
no route for cert material. Encrypted-universe support is
[#1241](https://github.com/yugabyte/meko/issues/1241), behind the compute-side
cert work in [#1114](https://github.com/yugabyte/meko/issues/1114).

## What is not here

- **A REST API or task system over the CR.** The `YBUniverse` CR is already a
  declarative surface with server-side apply and a status the operator reconciles
  level-triggered. Wrapping it in an imperative write-ahead task system is *less*
  standard, not more.
- **A CRD schema check.** See [above](#the-spec-and-where-validation-lives).
- **TLS.** See [Day-2](#day-2).
- **Cross-cluster addressing.** See [The address contract](#the-address-contract).
- **Foundation as a YCP desired-state resource.** `ycp/reconcile`'s contract maps
  onto Kubernetes almost exactly — that is not a coincidence, it is a careful
  reimplementation of the controller contract for targets that are *not*
  Kubernetes. Pointing it at a CR runs the k8s contract on top of the k8s
  contract. Its real value is remote declaration for a cluster nobody has kubectl
  on, which is a BYOC property; when that is the requirement it will be a thin
  backend rather than a rewrite.
- **A published image.** The workflow `go run`s it from the checkout.

One trap worth stating because it is one line of code away: do **not** report a
foundation through `Backend.List()` or `StatusReport.Resources`. Anything the
backend lists is a candidate for deletion if it is not also desired, so a
foundation surfaced as a reconcile resource with no desired counterpart would make
the agent try to delete the universe the controller lives inside. Report it on
`StatusReport.Components` instead.

## Where it runs

The operator path is **not** the default anywhere. `amp/deploy/up.sh`,
`amp/deploy/up-gke.sh`, `meko/deploy/helm/meko/scripts/deploy-gke.sh` and
`ycp/cmd/agent` all still bring up their own single-node `yugabyted`. That is a
decision, not an oversight: YBA wants ~4.5 CPU for `yugaware-0` and tens of
minutes to install, which is far too heavy for kind.

So it soaks in exactly one place — `/ci amp_deploy_gke_foundation` and its nightly
([`amp-foundation-gke.yml`](../.github/workflows/amp-foundation-gke.yml)), which
deploys `up-gke.sh` with `FOUNDATION=operator` and runs the agent-driven reconcile
smoke against the universe, then tears it down and asserts nothing leaked.
`meko_foundation` in [`meko-pr-build.yml`](../.github/workflows/meko-pr-build.yml)
runs the unit tests, which need no cluster.
