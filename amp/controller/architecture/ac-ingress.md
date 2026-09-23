# ac — Shared Postgres ingress

**Status:** Draft (schema, allocator, API surface, lifecycle wiring and charts landed; capacity reconciler and per-endpoint certificates outstanding)
**Last updated:** 2026-09-16

How an endpoint becomes reachable from outside the cluster **without provisioning
a load balancer of its own** ([meko#462](https://github.com/yugabyte/meko/issues/462)).
The `nlb_*` modes in [`ac-k8s-deploy.md`](ac-k8s-deploy.md) give each endpoint a
dedicated cloud load balancer; that costs one billed resource, one subnet address
per zone and about two minutes of provisioning **per database**, none of which a
quota increase fixes. The `ingress_*` modes put the endpoint on a shared one
instead.

## The shape

A **shard** is one cloud load balancer, a Traefik deployment behind it, and a
fixed range of ports. An endpoint gets one port on one shard.

```
client ──► ep-3f9a2c41.internal.byoc…:20007
              │  DNS: a record per endpoint, pointing at its shard
              ▼
           shard LB ──► traefik (≥2 replicas) ──► yb-ep-3f9a2c41 pod :55433
                          per-endpoint IngressRouteTCP + MiddlewareTCP
```

Four properties, each a deliberate choice rather than a consequence:

- **Endpoint creation never calls the cloud.** A shard provisions its whole port
  range when it is built, so creating an endpoint allocates a port and renders
  routes — all inside the cluster. Nothing mutates a load balancer that live
  connections are sitting behind, and no create waits minutes for one.
- **Every endpoint has its own DNS name** where there is a zone to publish it in,
  not a shared one. That keeps the shard
  an implementation detail: an endpoint can be moved between shards by
  re-pointing a record, without reissuing anyone's address. The name derives from
  the endpoint's **id**, never from its name or its cluster's or pool's — those
  are renameable (see `storage_pools_name_live` in `0002_resource_model`), and an address that breaks
  on a rename is a broken address.
- **TLS terminates at the pod.** Traefik forwards bytes; nothing in the path
  holds a key or can read customer traffic. This is what rules out every
  off-the-shelf hostname router, and §Routing explains why.
- **Access control stays per endpoint.** A shared load balancer would normally
  force a shared allowlist, because `loadBalancerSourceRanges` is a per-Service
  field. Traefik's `MiddlewareTCP.ipAllowList` filters per route instead.

## Routing: why the port, and not the hostname

Postgres does not open a connection with TLS. The client sends a plaintext
`SSLRequest`, the server answers `S`, and only then does the TLS handshake start.
So a proxy that routes by hostname must answer that `S` itself before it has any
idea which backend the connection is for — and the usual way to do that is to
terminate TLS, which the rule above forbids.

Hence **one port per endpoint**: the port is the discriminator, the proxy needs
to understand nothing, and the session is end-to-end encrypted by construction.
It also makes query cancellation work for free — a cancel arrives on a second
connection to the same port, so it reaches the same backend with no proxy state
involved.

Traefik can do the hostname version *without* terminating (verified: see
`amp/test/ingress-bakeoff/`), which is why it is the tier. That path would put
every endpoint on 5432 and make shard moves invisible to clients, but it is not
built: cancellation has no SNI to route on, and TLS would stop being optional.

### When there is no DNS

A zone is optional, and separate from the feature switch, because they answer
different questions: whether this deployment routes through shards, and whether
it has somewhere to publish names. A kind or CI cluster answers yes and no.

With no zone, an endpoint's address is its **shard's own host and port**. Nothing
about the routing changes — the route, the allowlist and the pod are identical,
and the endpoint answers in exactly the same place. What is given up is the
indirection: the shard stops being invisible, so moving an endpoint between
shards would change the address a client holds. On a cluster where nothing moves
that is worth nothing, and it is the difference between the functional suite
running on kind and not.

The name is still allocated and still unique — only unpublished — so a deployment
that later gets a zone has one ready rather than having to mint addresses for a
fleet that already exists.

## Allocation

`internal/ingress` owns it, deliberately outside `core.Backend` — that interface
is for user-facing resources, whereas a shard is capacity the control plane
manages on its own behalf, and this is the one place a bug hands two customers
the same address.

`0007_ingress_allocation` defines both tables. Two schema decisions carry weight
beyond their size, and `migrate_ingress_allocation_test.go` pins both:

- **`ingress_allocations.endpoint_id` has no foreign key.** Endpoints are
  hard-deleted, so a cascading reference would free the port the instant the
  endpoint went away — defeating the quarantine that stops a client with a cached
  address landing on whichever database takes the slot next. The row outlives the
  endpoint by design.
- **Uniqueness is partial, over live rows.** Unique, because two endpoints
  sharing a `(shard, port)` is a connection reaching the wrong database. Partial,
  because retiring has to give the port back.

Allocation is a single `INSERT ... SELECT`: between a read and a write a
concurrent create can take the slot, so the unique indexes are the arbiter and
the loser retries. Correctness does not depend on one writer.

**Placement** is a policy with a real trade, not a tuning detail:

| | Blast radius | Can capacity come back? |
|---|---|---|
| `spread` (default) — emptiest shard | smallest | **no** — no shard ever empties |
| `pack` — fullest shard with room | larger while partly full | yes — shards drain as endpoints retire |

Defragmenting would square that circle and is **not** done: on the port path a
move changes the port, and no saving justifies breaking a running database's
connection string. That becomes free only on the shared-port path above.

## Shard lifecycle

A shard is a Helm release of `deploy/helm/yb-amp-ingress`. The first one installs
with the control plane itself, so a deployment with ingress enabled has capacity
the moment the controller is up — no reconcile pass to wait for, and no cold start
where the first endpoint ever created blocks on a load-balancer build.

The controller registers the shard it is configured with and then waits for the
cloud: a shard is `building` until its Service has an ingress address, `ready`
once it does, and `draining` when it is being retired. **`building` is not
allocatable** — an endpoint placed there would render a DNS record whose target
is still empty.

Giving capacity back is gradual. A shard whose last endpoint retires scales its
proxies to zero first — the load balancer and the address stay, so it returns in
seconds — and only after a longer idle period is the release removed. **A
family's last ready shard is never drained**, whoever installed it: an empty
shard is the steady state of a deployment nobody is using yet, and going to zero
would mean the next endpoint waits minutes for a load balancer. Expressed as an
invariant over the table rather than a flag on the row, so it keeps holding after
the original shard is replaced.

## Config

`--deploy-ingress-enabled` is off by **default**, so this is inert on every
deployment that has not opted in, and the `ingress_*` modes are refused with
`400` rather than falling back to a dedicated load balancer — a fallback would
reinstate the per-endpoint bill without anyone choosing it.

The zones are independent of that switch, because they answer a different
question: whether there is DNS to publish names in. A kind or CI cluster routes
through shards happily and has none, and then an endpoint's address is its
shard's own `host:port` — the name is still allocated and still unique, just
unpublished.

A name minted without a zone is **unqualified** (`ep-abc12345`), and that shape
is what records the decision: publishability is read from the stored name on
every path, so a redeploy cannot reach a different answer than the create did.
The consequence worth knowing is that setting a zone on an existing fleet does
NOT retroactively publish it — those rows keep their unqualified names, and
giving them published ones means reissuing addresses. That is a migration, not a
config change.

There are two zone flags rather than one because external-dns is pinned to a zone
with `--zone-id-filter`, and the point of the internal family is that its names do
not resolve publicly. The name an endpoint is issued is stored on its allocation
row, so pointing both families at one zone is not a mistake a later config change
can correct — it means reissuing addresses customers already hold.

| Flag | Effect |
|---|---|
| `--deploy-ingress-enabled` | The opt-in. Off = the `ingress_*` modes are refused. |
| `--deploy-ingress-zone-internal` | DNS zone for internal endpoint names. Optional: empty = names unpublished, addresses are `shard:port`. |
| `--deploy-ingress-zone-external` | DNS zone for external endpoint names. Must differ from the internal zone, so an internal name can never reach a public one. |
| `--deploy-ingress-port-range` | Range each shard provisions in full; its width is how many endpoints a shard holds. |
| `--deploy-ingress-headroom` | Free ports kept per family before another shard is built. Measured across the family's ready shards, so it may exceed one shard's width; refused only if above what `--deploy-ingress-max-shards` shards can hold. |
| `--deploy-ingress-max-shards` | Ceiling on what the control plane may build unattended. |
| `--deploy-ingress-quarantine` | How long a released port stays unissued. Should exceed the record TTL. |
| `--deploy-ingress-placement` | `spread` or `pack`, per the table above. |

## Dependencies outside the controller

- **AWS Load Balancer Controller** provisions each shard's load balancer. Already
  deployed with its own IRSA role; the controller needs no cloud credentials, and
  neither does Traefik, which talks only to the Kubernetes API.
- **The Traefik CRDs** — all ten of the `traefik.io` set, not just the two this
  design renders: the provider builds an informer per kind and a failed sync on
  any one of them publishes NO configuration, so one missing CRD costs every
  route on the shard rather than the kind involved. The CONTROLLER chart carries
  them in `crds/` (from traefik-helm-chart **v37.1.2** — the chart's version
  series, not the proxy image's), which covers a **first install** of that
  release: Helm processes `crds/` on install only, skips it on `helm upgrade`,
  and omits it from `helm template` without `--include-crds`. Every other path
  therefore applies the directory explicitly — `deploy/controller/redeploy.sh`,
  `deploy/meko-dev/amp-redeploy.sh`, and the functional stack's render — because
  an existing release upgraded in place would otherwise never get them.
  A missing `ingressroutetcps` at least fails each `ingress_*` release at apply
  with `no matches for kind IngressRouteTCP` (after its slot is allocated); a
  missing `traefikservices` is silent.
- **The shard's own RBAC, kept namespaced.** The proxy reads Services,
  EndpointSlices, the `traefik.io` set — and Secrets and ConfigMaps, which it
  watches whether or not anything uses them. Nothing here terminates TLS, so the
  Secret grant buys this design nothing, and endpoint releases install into the
  same namespace, so the shared proxy can read every endpoint's password Secret.
  Granted anyway, and accepted rather than worked around: the provider's cache
  sync fails without it, Traefik has no flag to disable those two informers the
  way `--providers.kubernetescrd.disableClusterScopeResources` handles the Node
  one, and RBAC cannot restrict `list`/`watch` by name, so the only narrower
  shape is shards in a namespace of their own — which needs `allowCrossNamespace`
  and buys nothing this deployment wants. That cluster-scope flag is what keeps
  the Role namespaced at all: the Node informer is cluster-scoped, so without the
  flag the sync fails and the shard listens on all 50 ports and routes nothing.
- **external-dns**, *only when a zone is configured*, publishes the per-endpoint records from the `DNSEndpoint`
  objects each endpoint release renders. It needs its **CRD source** enabled
  (`--source=crd`) or those objects are inert, and `policy=sync` rather than
  `upsert-only` or retiring an endpoint leaks its name forever.
- **A DNS zone per family**, *when names are published at all*, so external-dns
  can be pinned per zone with `--zone-id-filter` and an internal endpoint's name
  can never be published to a public zone. A deployment with no zone publishes
  nothing and addresses endpoints by their shard, so it needs neither this nor
  external-dns.

On `meko-dev-eks` the first is healthy and the second is not: external-dns is
deployed but holds no AWS credentials by any mechanism and has failed every
reconcile since it was installed, so it has never published a record.
`amp/test/ingress-bakeoff/run.sh preflight` reports both halves.

## Not yet built

Per-endpoint certificates. TLS is passed through to the pod, so an endpoint
presents whatever certificate it already has — which does not match the name it
is published under, and `sslmode=verify-full` against that name therefore fails.
`ingress_external` is usable at `sslmode=require` and is NOT gated on this, but
an internet-facing endpoint that cannot be verified by name is a real limitation
rather than a detail; `verify-full` needs a certificate per endpoint name.

Per-listener source-IP filtering, and it needs more than the middleware. The
shard's Service is `externalTrafficPolicy: Cluster`, so kube-proxy SNATs and the
proxy sees a node address rather than the client — measured on GKE by giving a
route an `ipAllowList` of the client's own range and of `10.0.0.0/8` in turn:
under `Cluster` only the `10.0.0.0/8` rule admitted the connection, under `Local`
only the client's own did. So an allowlist written against customer addresses
would match the wrong thing until the Service is `Local`. PROXY protocol is not
the alternative it is on AWS: GCP's internal passthrough load balancer does not
offer it, which is why `proxyProtocol` in the shard chart is an AWS-shaped knob.
`Local` costs no measurable throughput (16.3k vs 16.9k tps at 16 connections,
inside this cluster's run-to-run spread) but restricts the load balancer to nodes
that host a replica, so it wants checking against replica placement before it is
turned on.

Also outstanding: the capacity reconciler's watermark and ceiling. `allowed_cidrs` is refused on the `ingress_*`
modes until that last one lands; `nlb_*` remains the mode for an endpoint that
needs an allowlist today.
