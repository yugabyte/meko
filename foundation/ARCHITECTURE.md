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
| CRDs (12 at chart 2026.1.1, 15 at 2026.1.2; we own 2 and can apply any) | `applyCRDs` | **cluster** | nothing, by default |
| `<ns>-yugaware` helm release | `installYBA` | namespace | `down` |
| ClusterRole/Binding `<ns>-yugaware` | the chart | **cluster** | `down`, by name |
| the single YBA customer | the chart's post-install Job | — | with the release |
| `YBProvider` / `YBUniverse` | `applyCRs` | namespace | `down`, in order |
| every other CR the spec names | `applyCRs`, in document order | namespace | `down`, in reverse |
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

**Every CR is a top-level key naming its kind**, whose value is `{name, spec}`:
`name` is `metadata.name`, `spec` is the CR's spec body verbatim, and a list
under the key declares several objects of one kind. `YBProvider` and
`YBUniverse` are two such keys: no Go struct and no field list, so any kind in
the bundle is reachable the day its CRD ships. They are not *entirely*
unprivileged, and the exceptions are worth knowing — `Render` fills the
provider's `kubeNamespace`, `universeDefaults` defaults the universe body,
`Validate` requires one universe and puts any provider above it, and
`applyCRs` waits on the provider and runs the release check before the
universe. Each of those exists because the deployer has to *do* something
about that object — wait, connect, create databases — not because the kind is
modelled. `yba.values:` is yugaware chart values verbatim on the
same principle.

`name` sits beside `spec` rather than inside it because they are different
fields: `spec.name` is **required** by `KMSConfig`, `PitrConfig` and `DrConfig`
and real on `StorageConfig` and `BackupSchedule`, so a body whose `name` was
lifted into metadata would be missing a required field.

**Case is what separates the two halves of the file.** An uppercase-initial key
is a CR kind; everything else must be one of the Spec's own fields, which keeps
`KnownFields` on for the settings — without that split, a mistyped `namespaec:`
would become a CR named "namespaec" instead of a parse error. Every Spec field
is lowerCamel and every operator kind is UpperCamel, so nothing has to enumerate
kinds to tell them apart.

**Key order is apply order**, and teardown is its exact reverse. `Load` walks
the document as a `yaml.Node` rather than decoding into a map, because Go
randomises map iteration and would lose it.

The alternative — a Go struct per CR, extended every time someone needs a field
nobody modelled and re-extended on every operator release — is what this
replaced. With passthrough, `chartVersion` alone decides which release's fields
are legal and a new field needs no code.

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

The same argument rules out mirroring **YBA's** rules, not just the CRD's:
which replication factors it supports, and how it wants a placement to add up,
move between releases, so a copy here starts rejecting specs YBA would accept.
What survives is only the cross-reference nothing outside this file can make —
a placement naming a zone the provider does not define.

`Spec.Validate` therefore covers only what no schema can express — things the CRD
happily accepts and **YBA then rejects asynchronously**, often with nothing
written back to the CR:

| Rule | What happens without it |
|---|---|
| `ybSoftwareVersion` non-empty | `universeState: "Error Creating"`, action still `queued`, reason only in the yugaware log |
| `ybSoftwareVersion` is a release YBA has (`installYBA`, not `Validate` — it needs the cluster) | the same opaque `Error Creating`, from `Helm Package path not found for release` |
| `numNodes ≥ replicationFactor` | the CRD constrains neither |
| `placementInfo` zones exist in the provider | a failed task, tens of minutes in |
| exactly one `YBUniverse`, and at most one `YBProvider` | `connstr`/`ensureDatabases`/the tserver lookup each resolve one universe, and a universe references one provider |
| `YBProvider` above `YBUniverse` | keys apply top to bottom, and YBA rejects a universe whose provider has not registered |
| every entry has a `name` and a `spec` | the API server rejects an empty name while naming no key in the file |
| no `(kind, name)` twice | the API server treats the second as a server-side apply of the first, so one entry silently does nothing |
| `chartVersion` set under `chartSource: repo` | an empty one is a failed helm lookup, not "latest" |
| provider zones agree on one `kubeNamespace` | this deployer's lookups run in exactly one |

### The Spec is the rendered body

Every universe default this deployer owns is applied at **parse time**
(`Spec.universeDefaults`), so the `YBUniverse` entry's `Spec` and the rendered
`YBUniverse` spec body are the same map. Anything reading the Spec — `sslMode`, the storage-class preflight,
`Validate` — sees exactly what the cluster gets.

This is load-bearing rather than tidy. When those defaults were applied at render
time into a copy, the Spec's own copy was authoritative for some fields and
stale for others, and `sslMode` read the stale one: it saw no `enableClientToNodeEncrypt`,
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

The steps are a named list rather than a chain of calls, because aborting in the
middle leaves a foundation that *looks* finished — universe `Ready`, every pod
healthy — while a later step never ran. A bring-up that died at the universe wait
never created `yb_storage`, and the omission surfaced much later as the amp controller
failing to open its state store. An aborted converge therefore returns an
`IncompleteError` naming the steps it skipped, and the CLI adds the command that
resumes it: every step is create-or-apply, so `apply` picks up from where the
failure stopped.

1. **Preflight** — binaries on PATH, cluster reachable, helm ≥ 3.11, k8s ≥ 1.27,
   every storage class the spec names exists (and warns unless it binds
   `WaitForFirstConsumer`), every pull secret the spec names exists,
   `yba.values.yugaware.defaultUser.password` is set, and any local `yba.chart`
   or `crdPath` is actually there.
2. **CRDs** — `kubectl apply --server-side -f <bundle for chartVersion>`, with a
   local-path override for a cluster with no egress.

   ### Where the chart and the CRDs come from

   `chartSource` decides what `yba.chart` means, and whether `chartVersion` is
   resolved at all:

   | `chartSource` | `yba.chart` is | `chartVersion` |
   |---|---|---|
   | `repo` (default) | a chart name in charts.yugabyte.com | **required** — the published version helm resolves, and the CRD git ref |
   | `path` | anything helm takes: a `.tgz`, an unpacked chart directory, or a **URL** | the CRD git ref only, and `crdPath` overrides even that |

   An **unpublished build** has no published chart, so it comes from
   `releases.yugabyte.com` — which is just a URL under `path`. The bucket is
   public for reads and helm fetches a chart from a URL itself, so there is
   nothing to download first and no credentials involved. There is
   deliberately **no separate source for it**: a second enum value would be a
   second name for the same thing, and one existed briefly before being
   removed (`Validate` still names the replacement for a spec that uses it).
   The bucket denies LIST, so a key that does not exist answers **403 rather
   than 404** — a 403 means the version is wrong, not that you lack access.

   A build tagged in `yugabyte/charts` resolves as a git ref too, so the CRDs
   come from the same version and `crdPath` is unnecessary; set it when the
   build is untagged, or the cluster has no egress.

   **A chart bump can move images the pull secret does not cover.** 2026.1.2
   switched postgres and prometheus from Docker Hub to
   `quay.io/yugabyte/hardened-*`, which the itest robot cannot pull — the
   yugaware pod sits in `Init:ImagePullBackOff` and the install burns the full
   `ybaTimeout` before saying so. `examples/amp.yaml` overrides both back.

   2026.1.2 also added **Perf Advisor**, a subchart enabled by default that
   pulls under its own `imagePullSecretName` (`yugabyte-k8s-pull-secret`,
   which nothing here creates). `helm --wait` waits for every workload in the
   release, so its pod alone times the install out with yugaware up and
   healthy. The spec turns it off — a foundation has no use for YBA's
   query-tuning UI.

   **Render the chart to find these, do not diff `values.yaml`.** Rendering is
   authoritative and offline, and it sees subcharts, which a values diff does
   not — that miss cost a 40-minute run on its own:

   ```
   helm template t ./yugaware -f <your yba.values> | grep -oE '^ *image: .*' | sort -u
   ```

   Then check each image is pullable with the secret this cluster has, tag
   included: the public prometheus repo prefixes versions with `v` and the
   hardened one does not, so moving only the registry resolves to a tag that
   does not exist.

   This is what `examples/amp.yaml` deploys: `2026.1.2.0-b137`, for the
   15-kind bundle — `KMSConfig`, `TelemetryProvider` and `UniverseKeyRotation`
   are not in 2026.1.1's 12, and installing their CRDs is not enough on its
   own, since reconciling one needs a yugaware whose operator knows the kind.
   A full build is also **immutable**, unlike a published line, which is what
   `checkDBRelease` exists to catch drifting.

   The **defaults** stay on the newest published line under `repo`, because
   that is the only pairing that works with nothing else set; deploying a
   build is a choice a spec makes explicitly.

   **The spec file is the only input.** There are no chart flags: the
   foundation workflow's `chart_version` dispatch input curls the tarball and
   writes a spec with three values replaced, then points `FOUNDATION_SPEC` at
   it. Leave its `db_version` empty and the chart's own `appVersion` is used,
   because the release a yugaware has is decided by its image and the chart
   pins that — naming only a chart would otherwise fail `checkDBRelease`
   twenty minutes in. Leave its
   `db_version` empty and the chart's own `appVersion` is used, because the
   release a yugaware has is decided by its image and the chart pins that —
   so the chart is the authority on what the universe may ask for, and naming
   only a chart would otherwise fail `checkDBRelease` twenty minutes in.

   A build that is **local** instead goes through `path` plus `crdPath` — the two halves of a downloaded release,
   each of which helm and kubectl already accept as a file, a directory or a
   URL. Nothing about the layout is assumed: the chart is identified by the
   `Chart.yaml` inside it, not by its path, and a `crdPath` directory is applied
   **non-recursively**. A leading `~` is expanded, because a YAML value is not a
   shell word and nothing downstream expands it; anything else relative resolves
   against the process CWD.
3. **Namespace** — created if absent, otherwise left exactly as it is. It is
   shared with the consumer's own pods, so the `managed-by` label records only
   that a foundation lives there; it is not an ownership claim.
4. **yugaware** — `helm upgrade --install --wait`, values file first and
   `--set-string` after, because helm gives `--set` precedence over `-f` and the
   settings in `HelmSets` must not be switchable off by a values file.
5. **Customer check** — YBA must have exactly one, or the operator refuses every
   CR. See below.
6. **Every CR the spec names, top to bottom.** A provider is waited for as
   soon as it is applied — which is before the universe but not necessarily
   before everything, since `Validate` only requires it above the
   `YBUniverse`, so a `Release` declared higher goes out first. Separate from
   the universe's wait because
   YBA rejects a universe naming an unregistered provider *asynchronously, in
   the universe's status*, a much worse place to read "the provider wasn't
   ready yet". The release check runs immediately before the universe.
7. **Wait** for the universe to settle, then **prune** what the spec no longer
   declares.
8. **Service check**, **databases**, **connection info**.

### Four ordering constraints that are not obvious

**Apply order is document order, and delete order is its reverse.** Nothing
sorts and no dependency table exists in Go: the spec author knows what
references what and says so by position. `Load` therefore walks a `yaml.Node`
rather than decoding into a map, which would lose it to Go's randomised
iteration. The one rule enforced is `YBProvider` above `YBUniverse`.

This is safe for everything else because the bundle ships **no admission
webhook and no conversion strategy**: a schema-valid CR applies whether or not
what it references exists, and the operator reports the problem asynchronously
in `status.message` and requeues. So a `BackupSchedule` declared below a
universe that is not up yet is accepted rather than blocking the converge, and
the recovery is to re-`apply` once it is.

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

It runs immediately before the universe is applied rather than at the end of
`installYBA`, so a `Release` key earlier in the spec has already had its chance
to import one. And when the spec declares a `Release` it **warns rather than
fails**: the probe lists what the yugaware image bundles, while a `Release`
registers one in YBA's own metadata from a URL with no guarantee it lands in
that directory, so a hard failure there would reject a correct spec. Only a
definitive answer fails a deploy, which is the same rule the other probes in
`yba.go` follow. A spec with no `Release` keeps the hard failure — the case
that caught the re-cut.

**`yb_storage` must exist before the consumer rolls out.** The amp controller's
`db.Connect` targets it at startup, which is why database creation is a step of
the bring-up rather than something the consumer does.

**Teardown order is the reverse, and stranding a finalizer is unrecoverable.**
Every CR carries the operator's finalizer and only the operator can clear it, so
`down` walks the spec backwards, then sweeps for objects it owns that the spec
no longer declares, and only then uninstalls yugaware. Doing it the other way
leaves a finalizer with nothing to satisfy it and the namespace hangs in
`Terminating` forever — `kubectl delete ns` is a no-op on a Terminating
namespace, so nothing recovers it.

For the same reason, **a CR that did not go stops the uninstall.** `down`
otherwise collects failures and carries on, so that one bad object does not
leave the release installed — but a CR still holding its finalizer is the case
where leaving the release is the *better* outcome: uninstalling removes the
only thing that can ever clear it. Leaving it is recoverable, because the
operator keeps working and `down` is idempotent, so re-running finishes the
job. Failures are still collected first, so every other CR is attempted before
it gives up.

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

`ybuniverse.FailureWatch` reads what the CR says is wrong. Its job is mostly to
**report**, not to adjudicate: without reading the CR back, a rejected spec
presents as a wait that times out with the reason left where nobody looked.

**A failed task is reported and nothing more.** YBA retries with a backoff, and
a retry keeps the *same* task UUID — only a re-run gets a new one — so neither
elapsed time nor a count of tasks separates a dead create from one about to
succeed. Guessing costs a working install; not guessing costs the rest of the
universe timeout, which an operator can cut short themselves once the report has
told them what broke. So the wait logs the failure when it appears, logs it
again when it changes, and carries it into the timeout alongside the pods and
events dump — with the command that reads the yugaware log on *this* install.
That StatefulSet is looked up by label rather than named, for the same reason
`yugawareStatefulSets` exists: the chart has renamed it across versions, so a
hint built from the release name sends the reader to a pod that is not there.

**One failure does end the wait: a spec YBA rejected before it made a task.**
The action stays `queued` with no UUID while `universeState` becomes
`Error Creating`, and nothing will retry it because YBA never accepted it — a
task UUID anywhere is the evidence that it did. That one has to hold for
`FailureSettleWindow` (2 minutes) first, because `universeState` has no
`observedGeneration` behind it: a reading taken just after an apply can belong
to the previous generation, and a healthy create was seen flickering through
`Error Creating` seconds before its task appeared. The CRD documents four states
as examples with no enum, and `Error Creating` is not among them, so the match is
on the `Error` prefix.

**And a CR's status is not the authority on what YBA did.** `KMSConfig.status.state`
exists and its CRD lists `InUse` among its values, but on a universe whose
encryption at rest is genuinely active it reaches `Ready` and stays there —
`yb-admin -master_addresses <masters> is_encryption_enabled` reports `ENABLED`
with the key id while the CR never advances. Verify encryption at rest against
`yb-admin`; reading it off the CR gives a false negative, which for an
encryption feature is the wrong direction to be wrong in.

## Pruning: omission is deletion

`apply` deletes every operator CR this deployer created that the spec no longer
declares. Without it, removing a key would stop *sending* an object without
removing it, and the file would stop being the source of truth — which is the
whole point of being able to declare any kind in it.

Nothing about it owns a kind. The candidate set comes from the cluster:

```
kubectl api-resources --api-group=operator.yugabyte.io --namespaced=true -o name
kubectl -n <ns> get <those> -l app.kubernetes.io/managed-by=meko-foundation -o json
```

Anything that comes back and is not in what `Render` just produced is deleted,
through the same `deleteCR` teardown uses, so it gets the same
`--wait=false`-then-poll-for-absence finalizer discipline. Only `YBUniverse` is
guaranteed rendered and so never a candidate — a provider is optional, so a
spec that drops one leaves a labelled `YBProvider` that prune reclaims like
anything else. `down` is still how a whole foundation goes.

Three things keep it from deleting more than it should:

- **The label is the ownership boundary.** Every rendered object carries
  `app.kubernetes.io/managed-by=meko-foundation` — the same label
  `down --delete-namespace` already refuses to proceed without — so a CR
  somebody else put in this shared namespace is never a candidate.
- **The query is namespace-scoped**, so one foundation cannot reach another's.
- **A discovery failure is an error, never an empty result.** "I could not list
  anything" and "there is nothing to delete" must not look the same to a delete
  loop, and the empty answer is the dangerous one: it reads as success and would
  silently stop pruning forever. An empty kind list right after `applyCRDs`
  installed them cannot be right, so it fails too.

**It runs after `waitUniverse`, not merely after the applies**, and that
position is the correctness argument rather than a detail. One edit can both
stop the universe using a `KMSConfig` and drop the key; disabling encryption at
rest is a YBA *task*, so a prune before the universe settled would delete a
config a running edit still references. Remove the key but forget the
reference and the delete is refused or the CR sits on its finalizer, which
`deleteCR` already reports by name.

`--no-prune` opts out.

**Objects predating the label are never reclaimed, and not just on the first
run.** Only `applyCRs` stamps it, and it sends only what the spec currently
declares — so a CR an earlier foundation created acquires the label the next
time an `apply` sends it, and one the spec has *already* dropped never does.
It is invisible to `ownedObjects`, so neither `prune` nor `down`'s sweep will
touch it, on any run.

That is the label doing its job rather than failing at it: the boundary is
what stops this deleting a CR somebody else put in a shared namespace, and it
cannot tell one of those from an old one of ours. The case to know about is a
kind dropped in the same edit as the upgrade — it keeps its finalizer, and
`down`'s helm uninstall then removes the operator that would have cleared it.
`kubectl delete` it by hand, or re-add the key, `apply` (which labels it), and
drop it again.

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
| anything else YBA has — a backup target, a certificate, a telemetry export, encryption at rest | a new top-level key naming the kind |

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
- **Go structs for any CR kind, the two required ones included.** The whole
  point of the top-level-key form is that a kind shipped in a future chart is
  usable the day its CRD is, with no drift check and nothing here to update.
  Generating structs from the CRD schemas would recreate exactly the per-CR type
  maintenance this module was built to replace.
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
