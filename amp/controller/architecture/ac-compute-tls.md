# ac — The certificate a compute presents to its clients

**Status:** Implemented; not on by default. Closes the per-endpoint-certificate gap
[`ac-ingress.md`](ac-ingress.md) opened.
The operator side is [`runbooks/tls.md`](../../runbooks/tls.md#server-tls-on-the-computes-listener).
**Last updated:** 2026-09-21

A compute's Postgres listener can present a certificate that matches the name a
client was handed, so `sslmode=verify-full` succeeds. Two issuers are supported —
a private CA, or Let's Encrypt — because the two have opposite costs and a
deployment picks by who its clients are.

This is the **opposite direction** from
[`ac-endpoint-tls.md`](ac-endpoint-tls.md), which is the client certificate a pod
presents *to* YugabyteDB. The two are independent switches: an endpoint can
verify its clients' encryption without encrypting its own storage traffic, and
the reverse. Nothing here touches the YB lanes.

Off by default, and TLS stays optional on the wire when it is on:
`compute_ctl` writes a `host` rule (`PG_HBA_ALL_SCRAM`) rather than `hostssl` —
the keyword is what decides this, not the auth method beside it — so a client
that asks for TLS gets it and every plaintext client keeps working. That is what
makes it safe to turn on under a live endpoint.

## Why there was nothing to verify against

`ingress_*` endpoints get a name of their own — `ep-<id>.<zone>`, derived from the
endpoint id so a rename cannot break an address — and TLS terminates at the pod,
because Postgres' `SSLRequest` preamble means a hostname router would have to
terminate it to know where the connection is going (`ac-ingress.md` §Routing).
So the certificate has to be in the pod, and until this landed the pod had none:
`ssl` was off, and a client could reach `sslmode=require` at best — encryption
with nobody authenticated at the far end.

## The shape

```
cert-manager ──issues──► Secret (tls.crt, tls.key)
                            │  mounted /etc/amp/tls/compute (whole directory)
                            ▼
                     endpoint pod ── supervisor renders ssl = on,
                                      ssl_cert_file, ssl_key_file
                                      into the compute spec
```

Three pieces, one per layer:

- **The endpoint chart** renders the `Certificate` (or mounts a shared Secret) and
  puts `COMPUTE_SSL_CERT_FILE` / `COMPUTE_SSL_KEY_FILE` in the ConfigMap.
- **The supervisor** turns those two paths into the three GUCs, and reloads
  Postgres when the files change.
- **The controller** decides, fleet-wide, which issuance shape every endpoint
  gets (`--deploy-compute-tls-*`). Per-endpoint is deliberately not a knob: a
  client's ability to verify an address is a property of the deployment, and an
  endpoint that silently came up unverifiable would be found by whoever tried to
  connect.

### The chart derives the names, and is the only thing that could

The certificate must cover the name the client holds, which is the endpoint's
in-cluster Service DNS and, where it has one, the endpoint's published name:
`service.ingress.dnsName` for an `ingress_*` endpoint, `service.dns.name` for an
`nlb_*` one. Both are already in the
release's values, so the chart computes the SAN list itself rather than being
handed it. A second source for the name a client is given is a second thing to
keep in step, and the failure mode of drift is a certificate nobody can verify.

An unqualified allocated name (`ep-3f9a2c41`, what a deployment with no DNS zone
gets) is left out: it is not a name any client can reach and no issuer will sign
it.

### The two addresses the chart cannot derive

`dnsNames` alone covers only the modes whose address is a name the chart already
holds. Two of the four connectivity modes are not that, and both would otherwise
serve a certificate covering nothing a client dials:

| Mode | Address a client gets | How it reaches the SANs |
|---|---|---|
| `ingress_*` **with** a DNS zone | the slot's `DNSName` | the chart, at render time |
| `ingress_*` **without** a zone | the shard's own `ShardHost` | the controller, at render time — the slot is allocated *before* the deploy, so the address is already known |
| `nlb_*` **with** a DNS zone | its published name | the chart, at render time |
| `nlb_*` **without** a zone | the load balancer's address | the controller, in a **second pass** — see below |
| (any, in-cluster) | the Service DNS | the chart, when `includeServiceDns` |

An address goes in as an **IP SAN** (`ipAddresses`) when it parses as one and a
`dnsNames` entry otherwise, because X.509 keeps the two apart: an address listed
under `dnsNames` is not matched by a client dialling it, and a name under
`ipAddresses` is rejected. Get it wrong and the certificate renders, issues, and
then fails every `verify-full` — the silent shape this whole feature exists to
remove. `addComputeTLSAddress` is the one place that decides.

### The second pass, for a load balancer

A LoadBalancer's address is assigned by the cloud *after* the Service exists, so
at render time there is nothing to put in the certificate. `CreateEndpoint`
already waits for that address — it is what the endpoint reports to clients — and
`renderClientAddress` then re-renders the release with it. The same pass points
an `nlb_*` endpoint's DNS record at that address (`ac-k8s-deploy.md` §Published
names).

Re-rendered through helm rather than patched onto the `Certificate`, because helm
owns that object: a direct patch survives exactly until the next redeploy. The
values are the durable record, so every later redeploy carries the address too.

It costs a reissue and a reload, **not a restart**: only the `Certificate` spec
moves, so cert-manager reissues, the kubelet propagates the new Secret, and the
supervisor's watcher picks it up and confirms it on the wire. It is a no-op for
every endpoint whose address the chart already covers — a ClusterIP create, or
`ingress_*` with a zone — so the common path pays nothing. A failure is logged
and left: the endpoint is up and serving, and the next redeploy retries.

What it does not fix is the case where there is no address to cover yet.
Provisioning a cloud load balancer routinely outruns `lbAddressPollTimeout`, and
`CreateEndpoint` falls through rather than failing the create — so the endpoint
records the Service DNS as its client address and the certificate gets no SAN for
the load balancer, because neither existed at the time. Nothing re-polls; both are
corrected by the endpoint's next controller-driven redeploy. That is #1380, and
the certificate now rides on it rather than adding a failure of its own: the SAN
is missing exactly when the recorded address is already wrong, and both are fixed
by the same redeploy.

The address itself is stable once assigned — a load balancer's address is held for
the life of its forwarding rule, so it survives pod reschedules, node churn and
Service updates, and changes only if the Service is deleted and recreated, which
is a redeploy anyway.

## The two issuers, and why the shapes differ

`--deploy-compute-tls-mode` picks one.

**`issue` — one certificate per endpoint.** A `Certificate` per release from
`--deploy-compute-tls-issuer-*`, which is what a private CA wants: issuance is
local, instant, unmetered, and each endpoint holds only its own key. The CA is
the `amp-ca` `ClusterIssuer` [`deploy/tls/trust-root.sh`](../../deploy/tls/trust-root.sh)
already bootstraps for the YB lane. Its cost is trust distribution — a client has
to be given `amp-ca-bundle`'s `ca.crt` as `sslrootcert`, which is fine when the
client is ours and not when it is somebody else's `psql`.

**`existing-secret` — one wildcard the fleet shares.** Every pod mounts the same
Secret, `*.<zone>` from Let's Encrypt via
[`deploy/tls/acme-issuer.sh`](../../deploy/tls/acme-issuer.sh). Clients need
nothing: the system trust store already has the root.

Per-endpoint ACME is not the default, because Let's Encrypt allows **50 new
certificates per registered domain per week** (renewals exempt, so it is a ceiling on
creates rather than on fleet size).

Whose ceiling it is depends on who owns the zone, and in BYOC the zone is the
**customer's** — one DNS zone per data plane. So the budget is theirs alone, not a
bucket every customer shares, and per-endpoint public certificates are viable for a
customer creating fewer than ~50 endpoints a week. Two caveats: it is the budget for
their *whole* registered domain, which their other infrastructure may already be
spending, and DNS-01 means giving cert-manager in their cluster credentials scoped to
that zone. The wildcard remains the option that needs neither.

Its cost is a shared private key: any endpoint pod that is compromised leaks the
key for every name in the zone. That is the trade the two modes are, and it is
why both exist rather than one winning.

If per-endpoint certificates *and* public trust are both wanted, the lever is the
account and not the topology — Let's Encrypt grants rate-limit increases to
providers issuing at this shape, and Google Trust Services' ACME meters per
account (EAB) rather than per registered domain. What does **not** work is asking a
public CA for a subordinate CA; see below.

DNS-01 is the only solver that can work. HTTP-01 and TLS-ALPN-01 need the ACME
server to reach the name on 80 or 443, and what answers at an endpoint's address
is the Postgres protocol on a shard port.

### Why there is no shared root, and no intermediate

The recurring proposal is a Yugabyte root with one intermediate per data plane, so a
customer trusts one anchor across their environments. It was designed out
(`ycp/PKI.md`, kept as a decision record) and is not being built. Three reasons, in
order of weight:

- **A public CA cannot sign a subordinate anyway.** Let's Encrypt's leaves carry
  `CA:FALSE`, and a publicly trusted sub-CA needs CA/Browser Forum audits that no CA
  sells. So an intermediate is only ever a *private* CA in a nicer shape — it never
  buys public trust, and the ACME limit it was meant to dodge is not in the picture
  once the root is ours.
- **It would put a CA key that chains to a fleet-wide root inside a cluster the
  customer controls**, with `nameConstraints` — beta in cert-manager 1.17 — as the
  only thing bounding it. A root per data plane bounds the same blast radius *by
  construction*: the key in that cluster signs for that cluster and is worthless
  anywhere else.
- **Root rotation stays local.** A per-cluster root rotates on one customer's
  schedule, and `amp-ca-roots` already appends rather than replaces so both roots are
  trusted during the overlap. A fleet root cannot: every client at every customer has
  to install the new one first, with no server-side signal that they have.

What is given up is the single anchor — a customer with N environments trusts N
roots. That is bounded work, because the DNS zone is per data plane too, so the DSN
is already per-environment and `sslrootcert` is one more field in config that is
already per-environment. The thing to watch is not root *count* but root *churn*: if
environments are created and destroyed often, trust distribution stops being one-time
setup. A customer who does want one anchor should bring their own — see below.

### Three ways to get a root, all built

`trust-root.sh` bootstraps a **self-signed root per cluster** by default, and takes
`TLS_ISSUER_NAME` / `TLS_ISSUER_KIND` / `TLS_CA_FILE` to use an issuer that already
exists instead. So a deployment picks:

| | Root | Clients trust | When |
|---|---|---|---|
| **Self-signed per data plane** | `amp-ca`, generated in the cluster | that environment's root | the default |
| **Customer's own CA** | theirs, named by `TLS_ISSUER_NAME` | what they already trust | they run a PKI — usually the best answer for an enterprise |
| **Public wildcard** | a public CA's | nothing — system trust store | they want no root distribution at all |

The first two are `computeTls.mode: issue`; the third is `existing-secret`. Nothing in
the chart, controller or supervisor names a root, so all three are configuration.

### What Supabase does, for calibration

The same question, answered by the closest comparable product: Supabase's Postgres
endpoints present a certificate from **"Supabase Root 2021 CA" — a self-signed
root**, valid to 2031, which the customer downloads per project from the dashboard
and passes as `sslrootcert`. A leaf per project for `db.<ref>.supabase.co`; the
session pooler chains to the same root. Their publicly-trusted certificates are on
the HTTPS/API side, not the Postgres wire.

So the private-CA path is the normal choice for this problem, and the
trust-distribution cost it carries is one they simply accept and document. Two
things to take from it:

- **One root, central issuance.** They can hold a single root because the signing
  key never leaves their control plane — the tenant never runs a CA. That option is
  open to us and is the alternative to the intermediate above; what is *not* open
  is a single root with `issue` as it stands, because that would put one fleet-wide
  key in every customer's cluster.
- **Their leaf leaned on the legacy `commonName`** rather than SANs
  ([supabase/postgres#73](https://github.com/supabase/postgres/issues/73)), which
  strict clients reject under RFC 2818. Ours sets no `commonName` at all and puts
  every name in `dnsNames`, which is why.

The dated root name is also the rotation answer: a new generation root rather than
an in-place swap, which is the shape `amp-ca-roots` already supports.

### What ACME needs that a CA does not

- **No `.svc.cluster.local` in the SANs.** No public CA can validate it, and one
  unvalidatable name fails the whole order — so
  `--deploy-compute-tls-include-service-dns=false`. An in-cluster client then
  cannot `verify-full` against the Service name, which is the same position it was
  in before this existed.
- **No requested `duration`.** Let's Encrypt fixes the lifetime at 90 days and
  cert-manager only warns that the request was ignored. `renewBefore` is
  cert-manager's own and is honoured.
- **No `commonName`.** It is capped at 64 characters, carries no weight against a
  client that matches SANs only, and an ACME issuer rejects one that is not among
  the names it validated. Neither mode sets it.

## In the pod

`ssl = on`, `ssl_cert_file` and `ssl_key_file` are rendered by the supervisor into
the compute spec, with the paths taken verbatim from the environment — they are
where the chart mounted the Secret, and anything derived would point the postmaster
at a file that is not there, which it refuses to start over.

They go in **last**, after the endpoint's own `pg_settings`, which is the one place
the operator's settings do not win. Precedence in this spec is positional
(`compute_ctl` writes the settings in order and Postgres keeps the last
occurrence), so position is the whole mechanism. It has to be, because the
alternative does not work: `pg_settings` is screened only for what Postgres will
*parse* (`guc::screen` probes the postmaster), and `ssl_cert_file = /tmp/nope`
parses fine and then fails startup. The daemon deny-list in `protected_keys.rs` is
no help either — it guards the `pageserver_settings` / `safekeeper_settings`
columns, not this one.

The mount is a **whole directory**, no `subPath`, for the same reason the YB
client mount is: the kubelet only propagates a renewed Secret into a directory
mount.

`defaultMode: 0440` with the pod's `fsGroup` is the one shape that works. Postgres
refuses a key file that is group- or world-readable *unless* it is root-owned and
at most `0640` (`be-secure-common.c`), and a Secret volume under an `fsGroup`
lands root-owned with that group — so a non-root server reads it through its group
and the permission check still passes.

The mode is therefore only half the shape, and the chart **refuses to render
without the other half**: `computeTls.enabled` with no `podSecurityContext.fsGroup`
fails at render, in both modes, because without it the files land root:root and the
compute crash-loops on a key it cannot read — with nothing in the manifest to point
at the cause. That guard belongs beside the other three (unknown mode, a shared
secret with no secret named, an empty SAN list) rather than being the one
misconfiguration discovered from a pod log.

### Renewal, without dropping a connection

Postgres reads both files at start and at each reload, and only for new backends,
so a renewal disturbs nothing in flight — and reaches nothing new either until
something asks for a reload. `compute_tls.rs` is that something: it polls the
files every 60s and, on a change, sends `compute_ctl` a `SIGHUP`, which reaches
Postgres as `pg_reload_conf()`. Open connections survive.

Polling rather than inotify, and contents rather than mtime, for the reasons the
YB lane's `yb_tls` has: the path is a symlink into a `..data` directory the
kubelet *replaces*, so a watch on the inode watches the wrong thing after the
first swap, and a Secret resync that rewrites identical bytes is not a renewal.
A file that cannot be read keeps its last contents — an absent mount is not a
change, and treating it as one would reload on a certificate the compute already
loaded.

One watcher per process, not per lease: the certificate belongs to the endpoint,
not to whichever write lease it is serving under.

**The signal is not the evidence.** `reload_compute()` returns once the `SIGHUP`
is delivered, and delivery proves nothing about application: `compute_ctl` returns
early from `reload_spec_from_disk` with only a log line when its status is neither
`Empty` nor `Running`; a signal landing before that handler is installed takes the
default action and kills it; and a Postgres reload whose new pair fails to load
keeps the **old** `SSL_context` (`be_tls_init` swaps it only on success) while
logging and returning normally. All three are indistinguishable from success to
the caller.

So the watcher **confirms on the wire**. After signalling it opens a TLS handshake
to the compute's own port and compares the leaf the server presents against the
leaf in the mount, polling for up to 10 seconds. Only a match calls `loaded()`.
This is the only place the answer exists: `SHOW ssl_cert_file` returns the
configured path, which never changes when the bytes behind it do, and
`pg_conf_load_time()` advances even for the reload that kept the old context.
Validation is off for that handshake deliberately — the question is *which*
certificate is being served, not whether it is trustworthy, and nothing is sent
over the connection.

`Watched::last` is the pair the compute is **confirmed to be serving**, and only
`loaded()` advances it. Every failure therefore collapses to the same retry:
undeliverable signal, dropped signal, rejected pair, or a compute still starting
all leave `last` where it was, so the next poll signals again. Committing on
observation — or on the signal — would drop the renewal permanently, and nothing
would say so: the expiry gauges read the *file*, so they would report the new
`notAfter` while the postmaster served the old certificate until `renewBefore` ran
out. `compute_cert_reload_pending` is exactly that gap, and it is now confirmed
rather than inferred.

Blocking for the confirmation costs nothing: the watcher sleeps 60s between rounds
anyway and nothing waits on it. The 10s deadline is not sized against how long a
reload takes — `pg_reload_conf()` is sub-second — but so that reaching it means
something.

`loaded()` also takes the observation the reload was made *for* rather than
re-reading, so a swap that lands during the reload stays a change for the next
poll instead of being adopted as already served.

### Watching it expire

The watcher publishes what it sees: `compute_cert_not_after_seconds`,
`compute_ca_not_after_seconds` and `compute_cert_reload_pending`, on the
supervisor's port
([`ac-metrics.md`](ac-metrics.md#certificate-expiry--from-the-pod-not-from-cert-manager)).
The first two are read from the files the postmaster is configured with rather
than from what cert-manager believes it issued, because those differ precisely
when a renewal has not reached the pod — and a lapsed certificate leaves the
server looking healthy while every `verify-full` client fails.

The third covers what reading the file cannot: a renewal that reached the volume
but not the postmaster. Both expiry gauges move on the *file*, so they report
health either way; `compute_cert_reload_pending` is `1` from the moment a change
is seen until a reload lands. Alert on it sustained, not on any `1` — the watcher
retries every poll, so a momentary `1` is the normal path.

### Saying which names it serves

At startup and after every renewal it loads, the watcher logs the certificate's
`dnsNames`. Under `mode: issue` that is a belt-and-braces line, because the chart
derived those names itself and refuses to render an empty list. Under
`mode: existingSecret` it is the only check there is: the chart mounts a Secret it
did not create, and nothing in the render path can compare a wildcard's names to
the address the controller published — the Secret's contents are not visible at
template time. A `*.<zone>` wildcard matches exactly **one** label, so a zone that
drifts from `--ingress-zone-*` leaves every endpoint offering TLS that no client
can verify, with every render, rollout and probe still green. The log line is what
makes that findable.

## Enabling it on an existing fleet

The certificate is part of the endpoint's rendered release, so existing endpoints
do not have one until they are re-rendered. That is the fleet roll that already
exists for the YB-lane transition: `plan_endpoint_rerenders`
([`ac-tasks.md`](ac-tasks.md)). Nothing is filtered, and a re-render of a
converged endpoint is a no-op Helm upgrade.

## Not built

- **No re-resolution when the load balancer was not ready in time.** The second
  pass runs at create and at each redeploy, not on a Service watch, so an
  endpoint whose LB outran `lbAddressPollTimeout` has neither the address nor
  the SAN until something redeploys it — #1380, which it inherits rather than
  extends. See above.
- **No per-endpoint switch, and no advertisement of one.** `Endpoint.address` is a
  `host:port`; a client learns whether it can `verify-full` from the deployment,
  not from the API. Adding a field means an OpenAPI change and the
  [`ycp` spec copy](../../../ycp/internal/ampclient/) alongside it, for something
  a fleet-wide flag already answers.
- **No client-certificate authentication.** `ssl_ca_file` is unset and pg_hba
  still uses a `host` rule with password auth, so this authenticates the
  *server* to the client and not the
  reverse. The YB lane does the reverse direction for storage traffic
  (`ac-endpoint-tls.md`); doing it for customer clients needs a per-endpoint
  client CA and an HBA the control plane writes, neither of which exists.
- **Nothing requires TLS.** There is no `hostssl`-only mode, so a client can still
  choose plaintext. Making TLS mandatory is an HBA change in `compute_ctl`, not a
  certificate one.
- **No forced reissue on upgrade** — deferred, to be built controller-driven.
  cert-manager reissues on three triggers only: a change to the Certificate
  *spec*, `renewBefore` being reached, or the `Issuing` condition set by hand
  (what `cmctl renew` does); annotations trigger nothing. So the chart-only route
  would mean carrying a changing value — the helm release revision — inside the
  spec, whose only harmless home is `subject.organizationalUnits`, and a helm
  revision number does not belong in an X.509 subject. The endpoint upgrade task
  setting the `Issuing` condition is the supported mechanism, and costs a
  `certificates/status` patch plus the RBAC for it.

  Worth being clear about what it buys, because it is not freshness —
  `renewBefore` already guarantees that. It **exercises the issuance path at a
  moment somebody is watching**, so a broken issuer surfaces during an upgrade
  rather than whenever a renewal happens to fire.
