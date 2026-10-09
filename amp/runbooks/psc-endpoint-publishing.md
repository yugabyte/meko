# Runbook: publish an amp endpoint over Private Service Connect

**Status:** Current — customer-owned procedure
**Applies to:** GKE / GCP. AWS and Azure sketched at the end, not verified.

How a consumer in a **different VPC or project** reaches an amp endpoint's Postgres port
without VPC peering, a VPN, or a public address.

amp does **not** create any of this. The endpoint's internal load balancer is amp's; the
service attachment in front of it is the customer's, created and destroyed by them. That
split is deliberate — nothing in the control plane holds credentials in the customer's
cloud account, so there is no path by which amp could provision it.

The reference implementation for the producer side is YBM's terraform module,
`common/src/main/resources/terraform/gcp/private-service-connect/` in `yugabyte-cloud`.
It targets a YBM-managed cluster's LB and YBM's own state bucket, so it cannot be used
as-is here, but the resource shape and its caveats are the same. This runbook is the
`gcloud` equivalent against an amp endpoint.

## Before you start

| | |
|---|---|
| The endpoint exists with `connectivity.mode = nlb_internal` | `cluster_ip` has no load balancer to attach to, and `nlb_external` is already public |
| A **free /29 or larger CIDR** in the cluster's VPC, per endpoint you publish | A NAT subnet **cannot be shared between service attachments** — see [Costs](#costs-read-before-promising-this-at-scale) |
| The consumer's **project ID(s)** | They go on the attachment's accept-list |
| `gcloud` authenticated against the **producer** project (the cluster's), and the consumer project | Two different identities in most customer setups |

## 1. Let the PSC NAT range through the endpoint's allowlist

Adding the NAT range to `allowed_cidrs` is all this takes, at create or afterwards. On an
existing endpoint, replace the whole list — it is not merged — and poll the task:

```
POST /api/v1/storage-pools/{poolId}/clusters/{clusterId}/branches/{branchId}/endpoints/{endpointId}/connectivity
{ "allowed_cidrs": ["10.72.4.0/29", "<everything already allowed>"] }
```

That patches `loadBalancerSourceRanges` on the existing `<release>-lb` Service and nothing
else, so the pod, the load balancer, the forwarding rule and any attachment built on it
stay. The connectivity **mode** has
no such path: changing it means recreating the endpoint.

PSC traffic arrives at the load balancer sourced from the **NAT subnet**, not from the
consumer's real address. `allowed_cidrs` is rendered into `loadBalancerSourceRanges`, so
if the NAT range is not in it every PSC connection is dropped at the firewall — `psql`
hangs and times out with nothing in any log.

Pick the NAT CIDR first, then create the endpoint with it allowed:

```jsonc
"connectivity": {
  "mode": "nlb_internal",
  "allowed_cidrs": ["10.72.4.0/29"]   // the PSC NAT subnet, plus anything else you allow
}
```

An endpoint with an empty `allowed_cidrs` is unrestricted and needs nothing here, which
is a legitimate choice when the surrounding VPC is already the trust boundary — it is the
`nlb_internal` default. Only the endpoint's own Postgres port is published either way.

To reach an endpoint *without* any of this — for a smoke test, or to verify the database
before publishing it — `kubectl -n <ns> port-forward svc/<endpoint> 55433:55433` needs no
load balancer, no allowlist and no attachment. That is an operator path, not a path for
the consumer's application traffic, which is what the rest of this runbook is for.

## 2. Find the forwarding rule behind the endpoint

GKE's name for the forwarding rule is not derivable from the endpoint's name, so resolve
it through the load balancer address rather than guessing:

```bash
NS=amp
EP=<endpoint release name>        # kubectl -n $NS get svc | grep yb-ep-
PROJECT=<producer project>
REGION=<cluster region>

LB_IP=$(kubectl -n $NS get svc "$EP" -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
FR=$(gcloud compute forwarding-rules list --project "$PROJECT" \
       --filter="IPAddress=$LB_IP AND region:$REGION" --format='value(name)')
echo "$LB_IP -> $FR"
```

Empty `FR` means the Service has no cloud LB yet — check the endpoint reached Ready and
that its mode really is `nlb_internal`.

## 3. Create the NAT subnet

Dedicated to this attachment, in the cluster's VPC and region:

```bash
NETWORK=$(gcloud compute forwarding-rules describe "$FR" --project "$PROJECT" \
            --region "$REGION" --format='value(network)')

gcloud compute networks subnets create "psc-nat-$EP" \
  --project "$PROJECT" --region "$REGION" --network "$NETWORK" \
  --range 10.72.4.0/29 --purpose PRIVATE_SERVICE_CONNECT
```

`--purpose PRIVATE_SERVICE_CONNECT` is required; a normal subnet is rejected by the
attachment.

## 4. Create the service attachment

```bash
gcloud compute service-attachments create "psc-$EP" \
  --project "$PROJECT" --region "$REGION" \
  --producer-forwarding-rule "$FR" \
  --nat-subnets "psc-nat-$EP" \
  --connection-preference ACCEPT_MANUAL \
  --consumer-accept-list "<consumer-project-id>=10"

gcloud compute service-attachments describe "psc-$EP" \
  --project "$PROJECT" --region "$REGION" --format='value(selfLink)'
```

`ACCEPT_MANUAL` plus an explicit accept-list is the posture to use: `ACCEPT_AUTOMATIC`
admits any consumer that can guess the attachment URI. The `=10` is that consumer's
connection limit.

Hand the consumer the `selfLink` — that is the service attachment URI.

## 5. Consumer side

In an allow-listed project, **same region** as the producer:

```bash
gcloud compute forwarding-rules create "psc-$EP" \
  --project=<consumer-project> --region="$REGION" \
  --network=<consumer-vpc> \
  --target-service-attachment="<service attachment URI>" \
  --load-balancing-scheme=""

CONSUMER_IP=$(gcloud compute forwarding-rules describe "psc-$EP" \
  --project=<consumer-project> --region="$REGION" --format='value(IPAddress)')

psql "host=$CONSUMER_IP port=55433 user=<user> dbname=<db> sslmode=require"
```

**The consumer connects to an IP, not a name**, so certificate hostname verification
cannot pass: `sslmode=require` works, `verify-full` does not. If the customer needs
`verify-full`, they map a private DNS name to `$CONSUMER_IP` in their own zone and use
that as `host`.

`amp`'s API keeps reporting the endpoint's **internal LB address** — it has no idea the
attachment exists, and the consumer IP is allocated in an account amp cannot see. Whoever
publishes the endpoint has to record the consumer IP themselves.

## Lifecycle — the two orderings that matter

**Destroy the attachment before deleting the endpoint.** The attachment targets the
forwarding rule; deleting the endpoint deletes its Service, its load balancer and that
rule underneath the attachment, which then cannot be cleaned up normally. Order:
consumer forwarding rule → service attachment → NAT subnet → the amp endpoint.

**The forwarding rule survives an upgrade, not a recreate.** The Service belongs to the
endpoint's own Helm release, so a resize or a redeploy keeps the same load balancer and
the attachment keeps working. Deleting and recreating the endpoint, or changing its
`connectivity.mode`, replaces the Service — new forwarding rule, and the attachment must
be recreated against it.

## Costs — read before promising this at scale

- **One NAT subnet per published endpoint.** A NAT subnet cannot be used by more than one
  service attachment, so publishing N endpoints consumes N subnets out of the VPC's
  address space. At a /29 each that is small, but it is N more objects to create, track
  and destroy in the right order.
- **One internal load balancer per published endpoint** already, from `nlb_internal`.
- **Every step is manual and customer-side.** Nothing here is reconciled: an endpoint
  recreated by amp leaves a dangling attachment, and nothing detects it.

If a customer wants many privately-published databases, this procedure is the wrong
shape and the answer is a shared front end fronting many endpoints behind one attachment
— a design decision, not a runbook.

## AWS and Azure

Not covered here, and not verified. The equivalent shape is an **AWS VPC endpoint
service** in front of the NLB the endpoint's Service created (consumers create interface
endpoints; the accept-list is by AWS principal), and an **Azure Private Link service** in
front of the internal Standard LB. The `allowed_cidrs` step in §1 has the same trap on
both: the endpoint's allowlist must admit the source range the private-link path presents.
