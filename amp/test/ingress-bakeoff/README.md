# Ingress bakeoff — Traefik vs Envoy Gateway

A spike harness for the shard tier in the per-compute Postgres ingress design: which
proxy fronts a shard's port range, and whether a shared-SNI front door can exist at
all without something in the path terminating TLS.

It stands up TLS-serving Postgres backends, installs a tier, and runs the
questions whose answers change what we build.

## The questions

| Test | What it settles |
|---|---|
| `sni` | Does the tier route by SNI while the **pod** terminates TLS? |
| `port` | Does port-per-compute routing work, and reach the right backend? |
| `churn` | Do sessions on a shard's other ports survive a route add? |
| `scale` | How long does programming one more route take once N exist? |
| `allowlist` | Can source-IP filtering stay **per endpoint** behind a shared listener? |
| `proxyproto` | Does that filter read the client a load balancer *declares*, not the TCP peer? |

`sni` is the one worth running first. Postgres does not open with TLS — the client
sends a plaintext `SSLRequest` and the server answers `S` before any ClientHello —
so a router has to speak that preamble before SNI is even visible to it. The engines
that handle it mostly handle it by terminating, which the design forbids.

### How `sni` distinguishes passthrough from termination

Two unrelated CAs. Backends are signed by the **backend CA**; a terminating proxy
could only ever present something signed by the **proxy CA**. The client trusts only
the backend CA and connects `sslmode=verify-full` through the shared port:

- **verify-full succeeds** → the chain reaches the backend CA → the pod terminated TLS.
- **verification fails** → something in the middle presented its own certificate.

A control connection then repeats the request trusting only the proxy CA and must
fail. Without that control, a proxy that ignored verification entirely would read as
a pass.

SNI reaches the proxy without DNS: libpq sends `host` as the TLS server name and
dials `hostaddr`, so the client presents `ep-0.bakeoff.test` while connecting to a
ClusterIP.

## Results — meko-dev-eks, 2026-09-13

Traefik **v3.7.13**, Envoy Gateway **1.9.1**, three backends on `postgres:18-alpine`,
k8s 1.34 / EKS us-east-2.

| Tier | sni | port | churn | allowlist | route 201 served | mem/replica @200 |
|---|---|---|---|---|---|---|
| traefik | **PASS** | PASS | PASS | **PASS** | 3s | ~70 MiB |
| envoy   | **N/A**  | PASS | PASS | not implemented | 3s | ~46 MiB |

**Per-endpoint CIDR allowlists survive a shared load balancer.** Traefik's
`MiddlewareTCP.ipAllowList` filters by source IP per *route*, so the allowlist stays per
endpoint even though `loadBalancerSourceRanges` (a per-Service field) cannot. Verified
both ways: a client outside the range is refused, and the same route widened to its /32
admits it. `MiddlewareTCP.inFlightConn` gives per-endpoint connection caps from the same
place.

**Traefik passes TLS through.** Its TCP router handles the Postgres preamble and still
leaves the session end-to-end, so a shared-SNI front door needs no proxy of our own. It
served both schemes at once — a port route per backend on its own entryPoint, and SNI
routes for all of them on `:5432`.

**Envoy cannot be asked the SNI question.** Its `tls_inspector` is a listener filter
that runs before the filter chain, so the `SSLRequest` preamble is never consumed and no
ClientHello is ever reached. Port routing is unaffected.

**Neither tier is slow at 200 routes.** Both served a newly added route 3s after apply,
so programming latency is not a discriminator at this size. Envoy is the leaner of the
two on memory; Traefik carries 200 entryPoints in static config against Envoy's five
Gateways.

### PROXY protocol — verified 2026-09-14

Behind an NLB with `ip` targets the proxy sees the load balancer, not the client, so a
per-database allowlist would be meaningless. PROXY protocol v2 fixes that, and the whole
chain composes:

A relay stands in for the load balancer, prepending a PROXY v2 header that claims
`192.0.2.77` — TEST-NET-1, so it can never collide with a real pod address. The Traefik
entryPoint sets `proxyProtocol.trustedIPs`.

| Allowlist | Result | What it proves |
|---|---|---|
| `192.0.2.77/32` — the declared client | connects | the filter reads the PROXY header |
| `10.0.0.0/8` — the relay's **real** pod IP | refused | it does **not** filter on the TCP peer |
| `198.51.100.0/24` — neither | refused | the filter is actually on |
| declared client, `verify-full` to the backend CA | connects | TLS still terminates at the **pod** |

The middle row is the decisive one: had Traefik filtered on the TCP peer, it would have
passed and the first row failed. Connecting at all also proves Traefik *consumes* the
header rather than forwarding it — Postgres does not speak PROXY protocol and would have
failed to parse the startup packet.

### Structural findings

- **`TCPRoute` is GA.** It is served as `v1` in the Gateway API bundled with Envoy
  Gateway 1.9.1, `v1alpha2` deprecated behind it — it is no longer experimental-channel.
- **A Gateway is capped at 64 listeners** (`maxItems: 64` on `spec.listeners`), so an
  Envoy Gateway shard can never exceed 64 ports however many the cloud would allow.
  Traefik has no equivalent cap; its entryPoints are static config.
- **Envoy Gateway puts its data plane in its own namespace**, not beside the Gateway.
- The Envoy Gateway helm chart installs the Gateway API CRDs itself, so `install-crds`
  is optional — but the install is cluster-scoped either way.
- **external-dns on meko-dev-eks is deployed but has never worked.** It holds no AWS
  credentials by any mechanism — no IRSA annotation, no EKS Pod Identity association, no
  Route53 policy on the node role — and IMDS refuses it. Every reconcile since
  2026-02-24 has failed (276,942 consecutive errors at one a minute) and no DNS record
  has ever been created. Its config is separately wrong for our use: sources `service`
  and `ingress` only, so a `DNSEndpoint` would be inert, and `policy=upsert-only`, so
  records are never deleted. `preflight` reports both halves — it tails the deployment's
  logs and warns when it cannot reach Route53.

## Running it

```bash
cd amp/test/ingress-bakeoff

./run.sh preflight              # context, tools, and what is already installed
./run.sh setup                  # namespace, certs, backends, client pod
./run.sh deploy traefik
./run.sh test traefik
./run.sh report

./run.sh install-crds           # cluster-scoped — asks for confirmation
./run.sh deploy envoy
./run.sh test envoy
./run.sh scale envoy 200        # programming latency with 200 routes present

./run.sh teardown --all
```

Everything lands in one namespace and every Service is ClusterIP, so a run
provisions **no cloud load balancer**. `install-crds` is the only cluster-scoped
step and is never implied.

Knobs: `BAKEOFF_CONTEXT` (default `meko-dev-eks`), `BAKEOFF_NS`, `BAKEOFF_BACKENDS`,
`BAKEOFF_ZONE`, `BAKEOFF_PORT_LO`, `BAKEOFF_CHURN_SESSIONS`.

## Limits worth knowing before citing a result

- `scale` shards its listeners across ceil(n/50) Gateways, since Gateway API caps
  one Gateway at 64 — so it does approximate the real shape. What it does not
  reach is the fleet size: 200 routes was measured, 1,000 across ~20 shards was
  not, and the controller-side cost is what grows there.
- `churn` adds a route and re-counts sessions; it does not exercise a proxy
  restart, a rollout, or a delete under load.
- Backends are stock Postgres, not amp endpoints — right for the routing question,
  silent on anything compute-specific.
- `report` only reads; it is `test` that appends a row per assertion, so repeated
  runs accumulate. `setup` truncates.
