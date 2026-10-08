# Runbook: TLS and certificate authentication to YugabyteDB

Operating the certificate trust between the amp stack and its YugabyteDB. The
design is [`../controller/architecture/ac-endpoint-tls.md`](../controller/architecture/ac-endpoint-tls.md);
this is what an operator runs and checks.

The certificate a compute presents to its own **clients** is a separate switch
with its own section: [§Server TLS on the compute's listener](#server-tls-on-the-computes-listener).

## What is deployed

| Piece | Where | Made by |
|---|---|---|
| cert-manager, trust-manager | namespace `cert-manager` | [`deploy/tls/trust-root.sh`](../deploy/tls/trust-root.sh), or pre-existing (bring-your-own) |
| `ClusterIssuer amp-ca` over the root CA Secret `amp-root-ca` | cluster / `cert-manager` | `trust-root.sh` (skipped when `TLS_ISSUER_NAME` names an existing issuer) |
| ConfigMap `amp-ca-roots` (every trusted root, PEM) | `cert-manager` | `trust-root.sh`; the file to edit for a CA rotation |
| Bundle `amp-ca-bundle` → ConfigMap `amp-ca-bundle` key `ca.crt` | every namespace labelled `amp.yugabyte.com/trust=true` | trust-manager |
| YugabyteDB with per-pod certificates, TLS on 5433 and 9100, client certificates required on RPC | the amp namespace | [`deploy/tls/yugabyte-tls.sh`](../deploy/tls/yugabyte-tls.sh) via the `yugabyte` chart |
| Role `amp_storage` (member of `yugabyte`, `CONNECT` on `yb_storage`) | YugabyteDB | [`deploy/tls/yb-storage-role.sh`](../deploy/tls/yb-storage-role.sh), called by both DB lanes |
| HBA `hostssl yb_storage amp_storage all cert` | YugabyteDB | `yugabyte-tls.sh` — **yugabyted lane only**. The operator lane sets no hba rules: YBA health-checks postgres as `yugabyte` with no certificate, so a cert rule for it stops the universe ever finishing creation, and login policy is not the foundation's to set. There, clients are encrypted and verify the server but are not certificate-authenticated. |
| `Certificate <controller>-db` (CN `yugabyte`) → Secret `<controller>-db-tls`, mounted at `/etc/amp/tls/client`; `amp-ca-bundle` at `/etc/amp/tls/ca` | the controller pod | controller chart, `tls.enabled=true` |
| `Certificate yb-ep-<id>-yb-client` (CN `amp_storage`) → Secret `yb-ep-<id>-yb-client-tls`, same mounts | every endpoint pod | the controller, from `controller.endpoint.ybTlsMode` |

Enable it on a fresh bring-up with `TLS=1 deploy/up-gke.sh` (or `up.sh`). Knobs:
`TLS_PERMISSIVE` (default `1`: plaintext still accepted, see below),
`TLS_ISSUER_KIND` / `TLS_ISSUER_NAME` / `TLS_ISSUER_GROUP` (bring-your-own issuer),
`YB_CHART_VERSION` (default from `amp/versions.txt`; the chart's appVersion is the
image it runs, so it must be one that carries `ThinClientService` — 2026.1.2 or
later), `YB_STORAGE_CLASS`, `YB_STORAGE_ROLE`. The controller's DSN
then carries `sslmode=verify-full` and the three file paths, and endpoints get the
same four values through their chart (`yugabyte.sslmode` … `yugabyte.sslkey`).

**On an operator-managed universe (`FOUNDATION=operator`) TLS is not a knob.** Both
encryption flags are create-time only in the CRD, so the foundation spec decides and
`up-gke.sh` reads the shape back from `foundation validate --json`; `TLS=0` against an
encrypted spec is refused. The rows above still hold, with three differences: the
universe's certificates are issued by YBA under its cert-manager type rather than by
`yugabyte-tls.sh` (the CA reaches it as a `YBCertificate` naming Secret
`amp-ca-bundle-tls`, which `up-gke.sh` copies from the trust bundle), `TLS_ISSUER_KIND`
must be `ClusterIssuer` because YBA deploys into its own namespace, and the node
certificates carry the endpoint Service's name through
`kubernetesOverrides.tls.extraSANs` — without it the Service is in no SAN and every
`verify-full` client fails the hostname check. See
[`foundation/examples/foundation-cli.yaml`](../../foundation/examples/foundation-cli.yaml).

## Permissive versus strict

`TLS_PERMISSIVE=1` keeps `allow_insecure_connections=true` on the tservers and a
trailing `host all all all trust` HBA rule, so clients that have not moved to TLS
still connect. It is the transition shape while an existing deployment is being
moved. `TLS_PERMISSIVE=0` drops both; switch to it once the `pg_stat_ssl` check
below shows every amp connection on a certificate.

## Checks

Trust root:

```bash
kubectl get clusterissuer amp-ca                       # READY True
kubectl -n cert-manager get certificate amp-root-ca    # READY True
kubectl -n cert-manager get configmap amp-ca-roots -o jsonpath='{.data.ca\.crt}' | openssl crl2pkcs7 -nocrl -certfile /dev/stdin | openssl pkcs7 -print_certs -noout
kubectl -n "$NS" get configmap amp-ca-bundle           # synced by trust-manager
kubectl get bundle amp-ca-bundle -o jsonpath='{.status.conditions}'
```

Database certificates (chart universe):

```bash
kubectl -n "$NS" get certificate                       # yb-master-yugabyte-tls-cert, yb-tserver-yugabyte-tls-cert, yugabyte-tls-client-cert
kubectl -n "$NS" exec yb-tserver-0 -- bash -c 'openssl x509 -in /home/yugabyte/cert-manager/tls.crt -noout -subject -ext subjectAltName -enddate'
```

Who is connected how (from inside the tserver pod, which carries a CN=yugabyte
client certificate at `/root/.yugabytedb/`):

```bash
kubectl -n "$NS" exec yb-tserver-0 -- bash -c 'bin/ysqlsh "host=yb-tserver-0.yb-tservers.'"$NS"'.svc.cluster.local sslmode=verify-full" \
  -c "select a.application_name, a.usename, s.ssl, s.client_dn from pg_stat_ssl s join pg_stat_activity a using (pid) where a.backend_type = '"'"'client backend'"'"' order by 1"'
```

`ssl = t` with a `client_dn` is a certificate login; `ssl = f` rows are what the
permissive plaintext rule is still admitting. When that column is empty for
every amp process, the deployment is ready for strict mode.

## Leaf certificate renewal

cert-manager renews every certificate at `renewBefore` and rewrites its Secret,
and the kubelet refreshes the mounted files within about a minute. The controller
reads the files on every new connection. Endpoint daemons poll their files every
60s and reconnect both lanes on the next use. To confirm one actually happened,
read `yb_tls.reloads` on the safekeeper's and pageserver's `/v1/status`: it
counts the times that process saw a watched file change and asked its pools to
reconnect, and their logs carry a matching `TLS reload` line. The serial beside
it, `yb_tls.client_cert`, is re-read from disk per request — it shows the new
certificate the moment cert-manager writes it, whether or not anything picked
it up, so it is not on its own evidence of a reload. A renewed tserver
certificate is copied into place only at pod start (the chart's
`certmanager-init` container). The CronJob `yb-cert-rollout` that
`yugabyte-tls.sh` installs beside the chart (`YB_CERT_ROLLOUT_SCHEDULE`, hourly
by default; `YB_CERT_ROLLOUT=0` to skip it) compares each Certificate's
`notBefore` with its pods' start time and restarts the StatefulSet whose
certificate is newer than its pods, masters before tservers. To do it by hand,
or to force a renewal:

```bash
cmctl renew -n "$NS" yb-tserver-yugabyte-tls-cert yb-master-yugabyte-tls-cert   # or wait for renewBefore
kubectl -n "$NS" create job --from=cronjob/yb-cert-rollout yb-cert-rollout-now  # or: rollout restart statefulset yb-master yb-tserver
```

Check the serial the server presents against the Secret afterwards:

```bash
openssl s_client -connect yb-tserver-0.yb-tservers."$NS".svc.cluster.local:5433 -starttls postgres </dev/null 2>/dev/null | openssl x509 -noout -serial
```

## CA rotation

Two roots are trusted at once while the leaves move; nothing is removed until
the last leaf has moved.

1. **Add the new root to the trust.** With a bootstrapped CA: create the new root
   (`kubectl -n cert-manager apply` a second `Certificate`/`Secret`, or let
   `amp-root-ca` renew), then re-run `deploy/tls/trust-root.sh`: a root not yet in
   `amp-ca-roots` is appended, never replaced. With a BYO issuer, re-run it with
   `TLS_CA_FILE` set to a PEM holding both roots. trust-manager rewrites
   `amp-ca-bundle` everywhere within seconds. The **endpoints** read it live.
   The **controller does not**: `installClientCertReload` replaces only
   `GetClientCertificate`, while its trust store is the `RootCAs` pgx built
   once from `sslrootcert` at `ParseConfig`, and nothing re-reads it. The
   database does not either: a chart universe trusts exactly the CA
   in its leaf Secret's `ca.crt`, which cert-manager sets to the certificate
   that signed the leaf (never a bundle, even when the issuer Secret carries
   one). It cannot trust both roots at once, so step 3 is a cut-over rather
   than a drain.
2. **Repoint the issuer.** `kubectl edit clusterissuer amp-ca` → `spec.ca.secretName`
   to the new root's Secret (or point the BYO issuer at its new CA).
3. **Renew every leaf, then roll the database.** `cmctl renew -n "$NS" --all`
   in the amp and database namespaces, then
   `kubectl -n "$NS" create job --from=cronjob/yb-cert-rollout yb-cert-rollout-now`
   rather than waiting for the hourly run. From the moment a tserver restarts
   on the new root it refuses new handshakes from a client still presenting an
   old-root leaf; established connections are unaffected, and each endpoint
   presents its renewed leaf within the 60s poll plus one reconnect. Expect a
   minute or two of connection retries in the daemon logs, not an outage for
   the endpoints: a compute keeps its pooled connections throughout.

   **Restart the controller here.** Its leaf reloads but its trust store does
   not (step 1), so once the tservers serve new-root leaves every fresh
   controller connection fails verification — and because the pool sets
   `MaxConnLifetime`, the pooled ones recycle into that failure rather than
   riding it out:

   ```bash
   kubectl -n "$NS" rollout restart deploy/yb-amp-controller
   ```
4. **Remove the old root.** Edit `amp-ca-roots` down to the new root and re-apply
   the same Bundle (trust-manager syncs). Connections
   established under the old chain stay up until they recycle, then are refused.
5. **Re-run `deploy/tls/yugabyte-tls.sh`.** The chart holds the bundle in its own
   `tls.rootCA.cert` value, which none of the steps above touch, so until it is
   re-rendered a later `helm upgrade` of the database would reinstate the
   pre-rotation roots.

`features/amp_tls_ca_rotation` in the QA framework rehearses steps 1 to 3 against
a trust root the run owns (`functional.tls_rotation.enabled=true`, plus an
`env.ampstack.tls.issuer_name` other than the shared `amp-ca`); see
[`../docs/qa-framework/architecture.md`](../docs/qa-framework/architecture.md).

## Bring your own cert-manager or issuer

cert-manager is cluster-scoped and often already present. `trust-root.sh` installs
it only when its CRDs are absent (`INSTALL_CERT_MANAGER=auto`), and never edits
an existing install. Two things to know about an install it did not make:

- Without `--enable-certificate-owner-ref`, a deleted `Certificate` leaves its
  Secret behind. The script says so. Deleting an endpoint removes its
  `Certificate` with the release; the `yb-ep-<id>-yb-client-tls` Secret then has
  to be deleted by hand on such an install.
- The bootstrapped CA Secret lives in cert-manager's cluster-resource namespace,
  which defaults to the namespace cert-manager runs in. If yours runs elsewhere,
  set `CERT_MANAGER_NS` to it.

To use an existing issuer instead of `amp-ca`, pass `TLS_ISSUER_KIND`,
`TLS_ISSUER_NAME` (and `TLS_ISSUER_GROUP` for an external issuer such as AWS
Private CA), plus `TLS_CA_FILE` when the CA chain cannot be read from the
issuer's own Secret. The `amp-ca-bundle` and everything downstream are the same.

## Enabling TLS on an existing deployment

Moves a running deployment from plaintext to certificates without recreating the
database or the endpoint records. Each endpoint pod is recreated once; nothing is
refused until the last step. The reasoning behind each step is in
[`ac-endpoint-tls.md`](../controller/architecture/ac-endpoint-tls.md#enabling-tls-on-an-existing-deployment).
Controller and endpoint images must carry the TLS fields (an older endpoint image
refuses the `sslcert` key at boot, which is the check).

1. **Trust root.** `deploy/tls/trust-root.sh` with `TLS_TRUST_NAMESPACES` naming
   the controller, database and endpoint namespaces; or, for an existing CA, a
   TLS Secret made from its certificate and key behind a `ca` ClusterIssuer, and
   `TLS_ISSUER_NAME` pointing at it. Nothing running is affected.
2. **Database on TLS, permissively.** Chart universe: `TLS_PERMISSIVE=1
   deploy/tls/yugabyte-tls.sh` against the existing release (it is a `helm
   upgrade`; the StatefulSets roll). YBA universe: the K8S cert-manager
   certificate, the issuer on the provider region, then Encryption in-Transit
   with a rolling restart and the gflags in the design doc. Existing endpoints
   stay up on plaintext; `pg_stat_ssl` shows `ssl = f` for them.
3. **Storage role.** `yugabyte-tls.sh` creates `amp_storage`; on YBA, `CREATE ROLE
   amp_storage LOGIN; GRANT yugabyte TO amp_storage`.
4. **Controller.** `TLS=1 DB_URL='postgres://yugabyte@<tserver>:5433/yb_storage?sslmode=verify-full&sslrootcert=/etc/amp/tls/ca/ca.crt&sslcert=/etc/amp/tls/client/tls.crt&sslkey=/etc/amp/tls/client/tls.key'
   ENDPOINT_YB_USER=amp_storage deploy/controller/redeploy.sh`. The pod restarts
   once and its backend shows `ssl = t`. New endpoints get certificates from here.
5. **Existing endpoints.** `curl -X POST <controller>/api/v1/endpoint-rerenders`
   rolls the fleet: one parent task owning a child per endpoint, four at a
   time by default (`max_parallel`), each pod recreated once. Poll the task at
   the `Location` header, and `GET /tasks/<id>/subtasks` for each endpoint's
   own outcome. Then the `pg_stat_ssl` check above shows
   `client_dn = /CN=amp_storage` for every `safekeeper:*` and `pageserver:*`.
6. **Strict.** With no `ssl = f` rows left, re-run step 2 with
   `TLS_PERMISSIVE=0` (YBA: drop the trailing `host all all all trust` rule and
   set `allow_insecure_connections=false`, rolling restart).
7. **Clean up.** Remove any endpoint password Secret, and record the TLS values
   in the deployment's checked-in values so a redeploy keeps them.

**Rollback.** Before step 6 each step reverses on its own: `TLS=0
deploy/controller/redeploy.sh` and a re-render put endpoints back on plaintext,
which the permissive database still accepts. After step 6, restore the permissive
flags with a rolling restart first.

## Server TLS on the compute's listener

The other direction, and a separate switch: the certificate a compute presents to
its **own clients**, so `psql "... sslmode=verify-full"` against the endpoint's
published name succeeds. Nothing above is a prerequisite — an endpoint can do this
on a plaintext YB lane and vice versa. Design:
[`ac-compute-tls.md`](../controller/architecture/ac-compute-tls.md).

Pick by who the client is.

**Private CA — clients are ours.** One certificate per endpoint from `amp-ca`,
which `trust-root.sh` already created:

```bash
helm upgrade --install ... \
  --set controller.endpoint.computeTlsMode=issue \
  --set controller.endpoint.computeTlsIssuerRef.name=amp-ca
```

Clients need the CA:

```bash
kubectl -n "${NS}" get configmap amp-ca-bundle -o jsonpath='{.data.ca\.crt}' > amp-ca.crt
psql "host=ep-<id>.<zone> port=<port> user=cloud_admin dbname=postgres \
      sslmode=verify-full sslrootcert=amp-ca.crt"
```

**Let's Encrypt — clients are somebody else's.** One wildcard for the zone, which
every endpoint mounts; clients need nothing but their system trust store. Staging
first — prod's limit is 50 certificates per registered domain per week and a
failed order counts:

```bash
ZONE=db.example.com ACME_EMAIL=you@example.com NS=amp \
  ACME_ENV=staging AWS_HOSTED_ZONE_ID=Z… deploy/tls/acme-issuer.sh
# then re-run with ACME_ENV=prod
helm upgrade --install ... \
  --set controller.endpoint.computeTlsMode=existing-secret \
  --set controller.endpoint.computeTlsSecret=amp-endpoints-wildcard-tls \
  --set controller.endpoint.computeTlsIncludeServiceDns=false
```

`computeTlsIncludeServiceDns` has **no effect in this mode** — the chart renders
no `Certificate`, so the SAN list it would build is never used; the names come
from whatever `acme-issuer.sh` ordered (`*.<zone>` and `<zone>`). The flag matters
only for `mode: issue` pointed at an ACME issuer, where it must be `false`: no
public CA can validate a `.svc.cluster.local` name, and one unvalidatable name
fails the whole order.

`ZONE` must be the zone the controller's `--ingress-zone-*` publishes into. A
`*.<zone>` wildcard matches exactly one label, and nothing at render time can
compare it to the name the controller allocated — so if the two drift, every
endpoint comes up offering TLS no client can verify, with every render and probe
still green. Confirm from the pod after the first rollout, not from the values:

```bash
kubectl -n "${NS}" logs deploy/yb-ep-<id> | grep 'server certificate covers'
```

**Checking an external endpoint.** An `nlb_*` endpoint, and an `ingress_*` one in
a deployment with no DNS zone, are reached by an address rather than a name, so
their certificates carry an **IP SAN**. `verify-full` against the address then
works with no `/etc/hosts` games:

```bash
psql "host=<address> port=<port> user=cloud_admin dbname=postgres \
      sslmode=verify-full sslrootcert=amp-ca.crt"

# what the certificate actually covers
kubectl -n "${NS}" get secret <release>-compute-tls -o jsonpath='{.data.tls\.crt}' \
  | base64 -d | openssl x509 -noout -ext subjectAltName
```

An `IP Address:` entry is the load balancer; `DNS:` entries are the in-cluster
Service name and any published name. If the address is missing, the second pass
did not run — look for `endpoint certificate does not cover its client address`
in the controller log, and re-render the endpoint to retry it.

**The documentdb gateway** on a documentdb endpoint serves the same certificate
on port 10260. It is published only on a `cluster_ip` endpoint's Service, so a
MongoDB client dials the in-cluster name, which the certificate names under
`mode: issue` with the Service DNS included (the default):

```bash
mongosh "mongodb://cloud_admin@yb-ep-<id>.<ns>.svc.cluster.local:10260/?tls=true&tlsCAFile=amp-ca.crt"
```

A Let's Encrypt wildcard cannot name that host, so under `existing-secret` a
verifying client fails on the hostname, and the UI still hands out the verifying
string so that it does. With compute TLS off the gateway self-signs, and the
only string that connects is `tls=true&tlsAllowInvalidCertificates=true`:
encrypted, with the server unverified. `GET …/endpoints/<id>/ca` reports which
(`documentdb_verifiable`).

**Existing endpoints** are re-rendered by the same fleet roll step 5 above uses —
`curl -X POST <controller>/api/v1/endpoint-rerenders`. Until then they keep
serving plaintext-only, and their gateways keep self-signing, which is what they
did before.

**Upgrading a controller that already has compute TLS on** to one whose chart has
the gateway serve the certificate needs the same roll. Nothing re-renders on a
controller upgrade, so each documentdb endpoint's gateway keeps self-signing
until its endpoint is re-rendered; `/ca` reports `documentdb_verifiable: false` meanwhile,
and the UI keeps offering the unverified string with its warning. The re-render
recreates the pod (the gateway's env and mounts change), so roll it when a
restart per documentdb endpoint is acceptable.

### Rehearsing it without a cluster

`yb_amp_local` reads the same two variables the pod's ConfigMap sets, so the whole
path — spec render, `ssl = on`, handshake, hot reload — can be exercised against a
throwaway CA:

```bash
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -keyout ca.key -out ca.crt -subj "/CN=amp-local-ca"
openssl req -newkey rsa:2048 -nodes -keyout tls.key -out tls.csr \
  -subj "/CN=unused" -addext "subjectAltName=DNS:localhost"
openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -out tls.crt \
  -days 1 -copy_extensions copyall
chmod 0600 tls.key   # Postgres refuses a key it owns that anyone else can read

export COMPUTE_SSL_CERT_FILE=$PWD/tls.crt COMPUTE_SSL_KEY_FILE=$PWD/tls.key
# ... start yb_amp_local ...
psql "host=localhost port=<pgport> user=cloud_admin dbname=postgres \
      sslmode=verify-full sslrootcert=$PWD/ca.crt" \
  -c "SELECT ssl, version FROM pg_stat_ssl WHERE pid = pg_backend_pid()"
```

Overwriting the pair with a freshly-signed one while a session is open exercises
the reload: within 60s the log says the certificate changed, the open session
survives, and the next connection gets the new serial.

### Checks

```bash
# The endpoint has a certificate, and it covers the name clients are given.
kubectl -n "${NS}" get certificate yb-ep-<id>-compute       # issue mode
kubectl -n "${NS}" get certificate amp-endpoints-wildcard   # wildcard mode
kubectl -n "${NS}" get secret <secret> -o jsonpath='{.data.tls\.crt}' \
  | base64 -d | openssl x509 -noout -text | grep -A1 'Subject Alternative Name'

# The compute is offering it.
kubectl -n "${NS}" exec deploy/yb-ep-<id> -- \
  psql -h 127.0.0.1 -p 55433 -U cloud_admin -d postgres \
  -c 'show ssl' -c 'select * from pg_stat_ssl where pid = pg_backend_pid()'

# A renewal was picked up without a restart: the log line, and a RESTARTS count
# that has not moved.
kubectl -n "${NS}" logs deploy/yb-ep-<id> | grep 'server certificate changed'

# The names the compute is actually presenting, logged at startup and after each
# renewal it loads. In existingSecret mode this is the ONLY check that the
# wildcard covers the address clients were handed — nothing at render time can
# see inside a Secret the chart did not create.
kubectl -n "${NS}" logs deploy/yb-ep-<id> | grep 'server certificate covers'

# A renewal that reached the volume but not the postmaster. The two expiry gauges
# read the file and report health either way; this one is confirmed by a handshake
# against the compute, so 0 means the server really is presenting the mounted
# certificate. Momentary 1 is the normal path (confirmation runs for up to 10s
# after each signal, and the poll retries every 60s); sustained 1 means the
# compute is not taking it.
# (port-forward, not `kubectl exec ... curl`: the endpoint image ships libcurl
# but no curl binary, so exec'ing curl is executable-not-found.)
kubectl -n "${NS}" port-forward deploy/yb-ep-<id> 9899:9899 &
curl -s localhost:9899/metrics | grep compute_cert_reload_pending

# The documentdb gateway serves the same leaf, with its chain. It re-reads the
# pair within 60s of a renewal and logs it; RESTARTS on the sidecar should not move.
kubectl -n "${NS}" port-forward svc/yb-ep-<id> 10260:10260 &
openssl s_client -connect 127.0.0.1:10260 -CAfile amp-ca.crt -showcerts </dev/null \
  | grep -E '^ *[0-9] s:|Verify return code'
kubectl -n "${NS}" logs deploy/yb-ep-<id> -c documentdb-gateway | grep 'TLS certificates reloaded'
```

A gateway in `CrashLoopBackOff` right after enabling compute TLS, with `Failed to
create TLS provider` in its log, could not read the mount; `No such file` is a
Secret with no `tls.crt` or `tls.key` key. It never falls back to self-signing.

A certificate stuck `Ready=False` in ACME mode is almost always the DNS-01 solver:
`kubectl describe certificaterequest` then `kubectl -n cert-manager logs deploy/cert-manager`.
A cert-manager without Route53 credentials fails every order silently from the
outside, the same way external-dns does (`ac-ingress.md`).

**Rollback.** Change the DSNs **first**, then re-render. `computeTlsMode=disable`
(or unset) brings the compute back with `ssl` off, and only `sslmode=prefer`
survives that: `require`, `verify-ca` and `verify-full` all fail closed once the
server stops offering TLS. Those are exactly the modes this feature exists to
make possible, so assume clients are pinned. A documentdb client with `tlsCAFile`
or a bare `tls=true` fails the same way once the gateway self-signs again. Drop
the pin everywhere it is set, confirm, and only then `computeTlsMode=disable` and
a re-render.
