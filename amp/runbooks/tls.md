# Runbook: TLS and certificate authentication to YugabyteDB

Operating the certificate trust between the amp stack and its YugabyteDB. The
design is [`../controller/architecture/ac-endpoint-tls.md`](../controller/architecture/ac-endpoint-tls.md);
this is what an operator runs and checks.

## What is deployed

| Piece | Where | Made by |
|---|---|---|
| cert-manager, trust-manager | namespace `cert-manager` | [`deploy/tls/trust-root.sh`](../deploy/tls/trust-root.sh), or pre-existing (bring-your-own) |
| `ClusterIssuer amp-ca` over the root CA Secret `amp-root-ca` | cluster / `cert-manager` | `trust-root.sh` (skipped when `TLS_ISSUER_NAME` names an existing issuer) |
| ConfigMap `amp-ca-roots` (every trusted root, PEM) | `cert-manager` | `trust-root.sh`; the file to edit for a CA rotation |
| Bundle `amp-ca-bundle` → ConfigMap `amp-ca-bundle` key `ca.crt` | every namespace labelled `amp.yugabyte.com/trust=true` | trust-manager |
| YugabyteDB with per-pod certificates, TLS on 5433 and 9100, client certificates required on RPC | the amp namespace | [`deploy/tls/yugabyte-tls.sh`](../deploy/tls/yugabyte-tls.sh) via the `yugabyte` chart |
| Role `amp_storage` (member of `yugabyte`), HBA `hostssl yb_storage amp_storage all cert` | YugabyteDB | `yugabyte-tls.sh` |
| `Certificate <controller>-db` (CN `yugabyte`) → Secret `<controller>-db-tls`, mounted at `/etc/amp/tls/client`; `amp-ca-bundle` at `/etc/amp/tls/ca` | the controller pod | controller chart, `tls.enabled=true` |
| `Certificate yb-ep-<id>-yb-client` (CN `amp_storage`) → Secret `yb-ep-<id>-yb-client-tls`, same mounts | every endpoint pod | the controller, from `controller.endpoint.ybTlsMode` |

Enable it on a fresh bring-up with `TLS=1 deploy/up-gke.sh` (or `up.sh`). Knobs:
`TLS_PERMISSIVE` (default `1`: plaintext still accepted, see below),
`TLS_ISSUER_KIND` / `TLS_ISSUER_NAME` / `TLS_ISSUER_GROUP` (bring-your-own issuer),
`YB_CHART_VERSION`, `YB_STORAGE_CLASS`, `YB_STORAGE_ROLE`. The controller's DSN
then carries `sslmode=verify-full` and the three file paths, and endpoints get the
same four values through their chart (`yugabyte.sslmode` … `yugabyte.sslkey`).

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
it up, so it is not on its own evidence of a reload. A renewed tserver certificate is copied into place
only at pod start (the chart's `certmanager-init` container), so after a renewal:

```bash
cmctl renew -n "$NS" yb-tserver-yugabyte-tls-cert yb-master-yugabyte-tls-cert   # or wait for renewBefore
kubectl -n "$NS" rollout restart statefulset yb-tserver yb-master
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
   `amp-ca-bundle` everywhere within seconds.
2. **Repoint the issuer.** `kubectl edit clusterissuer amp-ca` → `spec.ca.secretName`
   to the new root's Secret (or point the BYO issuer at its new CA).
3. **Renew every leaf.** `cmctl renew -n "$NS" --all`, then the rolling restart
   above for the database. Endpoints and the controller pick the new leaves up
   on their own.
4. **Remove the old root.** Edit `amp-ca-roots` down to the new root and re-apply
   the same Bundle (trust-manager syncs). Connections established under the old
   chain stay up until they recycle, then are refused.

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
