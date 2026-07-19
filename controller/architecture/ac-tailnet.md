# ac — Tailnet exposure & endpoint tailnet address

**Status:** Draft
**Last updated:** 2026-07-09

How the amp control plane and its compute endpoints are made reachable over a
[Tailscale](https://tailscale.com) tailnet — so any authorized tailnet device
can hit them by name with no `kubectl port-forward` and no public exposure — and
how the controller records each endpoint's **tailnet address** alongside its
Kubernetes LoadBalancer address (`ac-apis.md`, `ac-k8s-deploy.md`).

There are two independent layers: a cluster-level **Tailscale operator** that
does the actual exposure, and a controller feature that merely **reports** the
resulting address to API/UI clients.

## Layer 1 — Tailscale Kubernetes operator (cluster infra)

The [Tailscale k8s operator](https://tailscale.com/kb/1236/kubernetes-operator)
runs in the `tailscale` namespace and joins the tailnet using an OAuth client
(secret `operator-oauth`) tagged **`tag:k8s-meko`** (`--set
operatorConfig.defaultTags[0]=tag:k8s-meko`; the tag must exist in the tailnet
ACL `tagOwners`).

Any `Service` annotated with the following gets its own tailnet **proxy device**
(a StatefulSet in `tailscale`) and a MagicDNS name:

```
tailscale.com/expose = "true"
tailscale.com/tags   = "tag:k8s-meko"
tailscale.com/hostname = <optional explicit device name>
```

- **Device name** = the `tailscale.com/hostname` annotation if set, else
  `"<namespace>-<serviceName>"` (the operator default).
- **MagicDNS name** = `<device>.<tailnet-suffix>` (e.g. `<device>.tail5955.ts.net`).
- Traffic to the device's tailnet IP:port is proxied to the Service's ports.

The control-plane Services are exposed with explicit hostnames
(`amp-ui`, `amp-api`, `amp-mcp`). Compute endpoints are exposed automatically:
the controller stamps the `expose`/`tags` annotations onto every endpoint
`Service` it creates via `AMP_DEPLOY_SERVICE_ANNOTATIONS`, so each
`yb-ep-<uuid>` registers as `amp-dev-yb-ep-<uuid>.<suffix>` with no per-endpoint
step. Exposure coexists with the endpoint's internal NLB — the operator does not
own the Service's `status`.

## Layer 2 — endpoint tailnet address (controller)

The controller records a **second, optional** connection address per endpoint so
API/UI clients can show the tailnet path next to the k8s LB one. It is derived,
not queried — the operator writes the tailnet name back nowhere the controller
can read, but the name is deterministic.

- **Derivation** (`internal/deploy/kubernetes.go`, `tailnetAddress`): when
  `AMP_DEPLOY_TAILNET_SUFFIX` is set **and** the endpoint Service carries
  `tailscale.com/expose="true"`, the deployer returns
  `"<device>.<suffix>:<computePort>"`, where `<device>` mirrors the operator's
  naming rule above (`tailscale.com/hostname`, else `"<namespace>-<release>"`).
  Otherwise it returns `""`.
- **Persistence**: stored on `endpoint.tailnet_address` (part of the `endpoint`
  table in `0001_init.up.sql`), set on a successful deploy by the widened
  `SetEndpointAddress` (`internal/task/step_create_endpoint.go`). NULL/`""` until
  set, coalesced to `""` on read.
- **API**: served as the `tailnet_address` field on the `Endpoint` schema
  (`openapi_spec.yaml`; empty when not tailnet-exposed).
- **UI** (`amp_ui`, `MultitenancyPage`): when present, the endpoint renders
  `primary: <address>` and `tailnet: <tailnet_address>` stacked; when empty, just
  the bare address (no labels) — so non-tailnet deployments are unchanged.

## Config

| Knob | Where | Effect |
|---|---|---|
| `AMP_DEPLOY_TAILNET_SUFFIX` | `controller.endpoint.tailnetSuffix` (Helm) | tailnet MagicDNS suffix, e.g. `tail5955.ts.net`. Empty ⇒ feature off. |
| `tailscale.com/expose`, `tailscale.com/tags` | `controller.endpoint.service.annotations` | make each endpoint Service a tailnet device; also the gate for address reporting. |
| operator `tag:k8s-meko` | operator Helm values + tailnet ACL | identity the proxies register under; ACL must grant caller devices access. |

## Notes & non-goals

- The `tailnet_address` is **derived**, not confirmed — it does not verify the
  proxy device has registered. The name is stable and correct in practice.
- Only endpoints created **after** the feature ships record an address; existing
  rows stay `""` until recreated.
- Exposure is **HTTP** on the Service port (no MagicDNS TLS today); reachability
  is gated entirely by the tailnet ACL for `tag:k8s-meko`.
- One proxy device per exposed Service; endpoints follow the one-endpoint-per-
  branch rule (`NODE_ID` is derived from the branch id).
