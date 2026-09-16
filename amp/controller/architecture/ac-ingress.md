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
- **Every endpoint has its own DNS name**, not a shared one. That keeps the shard
  an implementation detail: an endpoint can be moved between shards by
  re-pointing a record, without reissuing anyone's address. The name derives from
  the endpoint's **id**, never from its name or its cluster's or pool's — those
  are renameable (see `0016_storage_pool_name_live`), and an address that breaks
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

## Allocation

`internal/ingress` owns it, deliberately outside `core.Backend` — that interface
is for user-facing resources, whereas a shard is capacity the control plane
manages on its own behalf, and this is the one place a bug hands two customers
the same address.

`0018_ingress_allocation` defines both tables. Two schema decisions carry weight
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

Enabling it needs at least one zone. An endpoint's address **is** its published
name — `Allocation.Address()` is `dns_name:port` — so a family with no zone has
nothing to hand a client, and the allocator refuses it with `ErrNotConfigured`.
Startup refuses the combination outright rather than letting it fail on every
endpoint create.

Addressing an endpoint by its shard's own `host:port` instead, so a kind or CI
cluster with no external-dns can route identically, is **not yet built** — see
the section below. Until it lands, the switch and "has a zone" are not separable.

There are two zones rather than one because external-dns is pinned to a zone with
`--zone-id-filter`, and the point of the internal family is that its names do not
resolve publicly. The name an endpoint is issued is stored on its allocation row,
so pointing both families at one zone is not a mistake a later config change can
correct — it means reissuing addresses customers already hold.

| Flag | Effect |
|---|---|
| `--deploy-ingress-enabled` | The opt-in. Off = the `ingress_*` modes are refused. |
| `--deploy-ingress-zone-internal` | DNS zone for internal endpoint names. Empty = that family cannot be allocated; at least one zone is required. |
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
- **The Traefik CRDs** (`ingressroutetcps`, `middlewaretcps` and the rest of the
  `traefik.io` set). The shard chart renders Traefik directly rather than through
  its upstream chart, which is what used to install them — so they are now a
  prerequisite, and one with two failure modes worth knowing apart. Without them
  the shard installs cleanly and its provider serves nothing, while every
  `ingress_*` endpoint release fails at apply with `no matches for kind
  IngressRouteTCP` — after its slot has been allocated. `deploy/controller/redeploy.sh`
  applies them before installing a shard.
- **external-dns** publishes the per-endpoint records from the `DNSEndpoint`
  objects each endpoint release renders. It needs its **CRD source** enabled
  (`--source=crd`) or those objects are inert, and `policy=sync` rather than
  `upsert-only` or retiring an endpoint leaks its name forever.
- **A DNS zone per family**, so external-dns can be pinned per zone with
  `--zone-id-filter` and an internal endpoint's name can never be published to a
  public zone.

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

Also outstanding: the capacity reconciler's watermark and ceiling,
**addressing an endpoint by its shard's `host:port` where there is no DNS zone**
(until then a zone is required, see Config), and per-listener source-IP
filtering. `allowed_cidrs` is refused on the `ingress_*`
modes until that last one lands; `nlb_*` remains the mode for an endpoint that
needs an allowlist today.
