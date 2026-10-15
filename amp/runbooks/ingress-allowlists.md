# Runbook: per-endpoint allowlists on the ingress_* modes

How to turn on `connectivity.allowed_cidrs` for `ingress_internal` / `ingress_external`
endpoints, prove it filters on the real client address, and turn it off or roll back. Why it
needs a different setup per cloud: [`ac-ingress.md`, "Per-endpoint allowlists"](../controller/architecture/ac-ingress.md#per-endpoint-allowlists).

Until this is done the API refuses `allowed_cidrs` on `ingress_*` with `400`, and YCP must keep
`ybcloud.conf.ycp.amp_ingress_allowed_cidrs_enabled` off for the environment.

## Conventions

```bash
export NS=amp-test
kubectl -n "$NS" port-forward svc/amp-controller 8080:8080 &
export BASE=http://127.0.0.1:8080/api/v1
```

`redeploy.sh` reads its whole configuration from env on every run. Run it with the same variables
the environment is normally deployed with, plus the ones below; leaving one out changes it.

## The flow differs by cloud

| Cloud | Where the client address is lost | What you set |
|---|---|---|
| AWS (EKS) | At the NLB: it forwards to Traefik pods by IP and does not pass the client address on | PROXY protocol, trusting only the NLB's subnets |
| GCP (GKE), Azure (AKS) | At kube-proxy: the load balancer delivers the client address, then `externalTrafficPolicy: Cluster` rewrites it to a node's | `externalTrafficPolicy: Local` on the shard Service |

`redeploy.sh` picks the flow from `INGRESS_CLOUD`. In both, it changes the shards first and turns
the controller flag on only after every shard has rolled out.

## AWS

1. Find the CIDRs of the subnets each shard's NLB sits in. These are the only peers allowed to
   assert a client address, so take them exactly, never the VPC or cluster CIDR:

   ```bash
   for shard in pgin-0 pgin-0-ext; do
     host=$(kubectl -n "$NS" get svc "$shard" -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null) || continue
     subnets=$(aws elbv2 describe-load-balancers \
       --query "LoadBalancers[?DNSName=='$host'].AvailabilityZones[].SubnetId" --output text)
     aws ec2 describe-subnets --subnet-ids $subnets --query 'Subnets[].CidrBlock' --output text
   done
   ```

2. Redeploy with filtering on:

   ```bash
   INGRESS_CLOUD=aws INGRESS_PROXY_PROTOCOL=true \
   INGRESS_PROXY_TRUSTED_IPS=10.0.32.0/20,10.0.48.0/20 \
   INGRESS_SOURCE_FILTERING=true \
   deploy/controller/redeploy.sh
   ```

   The script exits before changing anything if either PROXY variable is missing.

## GCP and Azure

1. Check where the shard's Traefik replicas run. Under `Local` the load balancer sends traffic only
   to nodes with a replica, so they should sit on different nodes in different zones:

   ```bash
   kubectl -n "$NS" get pods -l app.kubernetes.io/name=yb-amp-ingress -o wide
   ```

   Two replicas on one node means that node carries the whole shard; reschedule one first.

2. Redeploy with filtering on:

   ```bash
   INGRESS_CLOUD=gcp INGRESS_SOURCE_FILTERING=true deploy/controller/redeploy.sh   # or azure
   ```

3. Confirm the shard Services changed:

   ```bash
   kubectl -n "$NS" get svc -l app.kubernetes.io/name=yb-amp-ingress \
     -o custom-columns=NAME:.metadata.name,POLICY:.spec.externalTrafficPolicy
   ```

## Without redeploy.sh

`redeploy.sh` is the only script that installs shards and orders this change. An environment
installed some other way (the YCP install hint's `helm upgrade --install yb-amp-controller`, or
`helm --set` by hand) needs the same values applied in the same order. Nothing checks them for you.

The shard releases are `pgin-0` (internal) and `pgin-0-ext` (external), from the
`yb-amp-ingress` chart; the controller release is `yb-amp-controller`.

**Turning it on.** Shards first, then the controller, never the reverse:

1. Upgrade every shard release, keeping its other values:

   ```bash
   # GCP / Azure
   helm upgrade pgin-0 <yb-amp-ingress chart> -n "$NS" --reuse-values \
     --set externalTrafficPolicy=Local

   # AWS: trustedIPs is the NLB's subnets only (see "AWS" above)
   helm upgrade pgin-0 <yb-amp-ingress chart> -n "$NS" --reuse-values \
     --set proxyProtocol.enabled=true \
     --set 'proxyProtocol.trustedIPs={10.0.32.0/20,10.0.48.0/20}'
   ```

   Repeat for `pgin-0-ext` if it exists.

2. Wait for each shard to roll out: `kubectl -n "$NS" rollout status deploy/pgin-0`.
3. Only then turn the controller on:

   ```bash
   helm upgrade yb-amp-controller <yb-amp-controller chart> -n "$NS" --reuse-values \
     --set controller.endpoint.ingress.sourceFiltering=true
   ```

4. Verify as below.

**Turning it off.** The reverse order, after the lists are gone:

1. List the endpoints that still hold an allowlist. Every line printed must be cleared
   (`"allowed_cidrs": []`) first:

   ```bash
   kubectl -n "$NS" get middlewaretcps.traefik.io \
     -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.ipAllowList.sourceRange}{"\n"}{end}' |
     grep -- '-allow ' | grep -v ' \["0.0.0.0/0","::/0"\]$'
   ```

2. Turn the controller off (`--set controller.endpoint.ingress.sourceFiltering=false`), then revert
   the shards if you want to (`externalTrafficPolicy=Cluster`, or `proxyProtocol.enabled=false`).

A later `helm upgrade` that does not pass `--reuse-values` or the same `--set` flags resets these
values to the chart defaults: filtering off on the controller, `Cluster` and no PROXY protocol
on the shards. Shards reset that way while the controller stays on is the failure this runbook
exists to prevent, so re-check the shard Services after any upgrade.

## Verify, on every cloud

Use a test `ingress_*` endpoint and a client whose public or VPC address you know (`$MY_IP`).
`$EP` is the endpoint's path under `$BASE`:
`storage-pools/<pool>/clusters/<cluster>/branches/<branch>/endpoints/<endpoint>`.

1. The controller declares it:

   ```bash
   kubectl -n "$NS" get deploy amp-controller \
     -o jsonpath='{.spec.template.spec.containers[?(@.name=="controller")].env[?(@.name=="AMP_DEPLOY_INGRESS_SOURCE_FILTERING")].value}'
   ```

   `true`, or the API still refuses lists.

2. Allow only the client, and connect. It must succeed:

   ```bash
   curl -s -X POST "$BASE/$EP/connectivity" -H 'Content-Type: application/json' \
     -d "{\"allowed_cidrs\":[\"$MY_IP/32\"]}"
   # wait for the task, then: psql "<the endpoint's address>" -c 'select 1'
   ```

3. Allow only a range the client is not in, and connect. It must be refused:

   ```bash
   curl -s -X POST "$BASE/$EP/connectivity" -H 'Content-Type: application/json' \
     -d '{"allowed_cidrs":["192.0.2.0/24"]}'
   ```

Step 2 failing while step 3 is refused means Traefik still sees a node or NLB address: re-check the
shard settings above. Step 3 succeeding means the middleware is not applied. Check that
`<release>-allow` exists and lists the range:
`kubectl -n "$NS" get middlewaretcps.traefik.io <release>-allow -o yaml`.

Then put the endpoint back to `[]`, and turn on the YCP flag for the environment.

## Turning it off, or rolling back below this release

Every stored list would be re-rendered behind shards that no longer keep the client address, so
the lists go first:

1. Clear every ingress endpoint's list (`"allowed_cidrs": []`) and wait for the tasks to finish.
2. Redeploy with `INGRESS_SOURCE_FILTERING=false`. The script refuses, naming the endpoints, while
   any `<release>-allow` middleware still holds a list.
3. Turn off the YCP flag, then roll back if that was the goal.

## Errors you may see

| Error | Meaning |
|---|---|
| `400 … --deploy-ingress-source-filtering is off` | The controller flag is off; finish the flow above |
| `endpoint … has no allowlist middleware` | The endpoint's release predates the middleware. Re-render it with `POST /endpoint-rerenders` naming it (the pod restarts), then retry. Clearing its list works without this |
| `INGRESS_SOURCE_FILTERING=true on aws needs …` | Set `INGRESS_PROXY_PROTOCOL=true` and `INGRESS_PROXY_TRUSTED_IPS` |
| `INGRESS_SOURCE_FILTERING is on … these endpoints hold an allowlist` | Clear the listed endpoints before turning it off |
