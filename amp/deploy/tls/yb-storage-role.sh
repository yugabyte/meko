#!/usr/bin/env bash
#
# The YugabyteDB role amp's endpoints log in as, by certificate CN.
#
# Its own script because two different databases need it and neither can be the
# other's home: the yugabyted TLS lane creates the database and the role
# together (yugabyte-tls.sh), while on an operator-managed universe the database
# comes from yb-database.sh (or, once a build carrying it is pinned, from YBA's
# own yb.universe.create_yb_storage_db) and still has no role — the role is
# amp's, not the universe's, so the foundation deployer has no business creating
# either.
#
# Runs ysqlsh inside a tserver pod, logging in with the node's own client
# certificate: the hba rules both lanes install make `yugabyte` a certificate
# login, so there is no password to pass and nothing to leak into argv.
#
# Idempotent.
#
# Env:
#   NS               namespace holding the amp stack (required)
#   UNIVERSE_NS      namespace holding the DB pods (default: NS). They differ
#                    when a provider zone names its own kubeNamespace.
#   YB_HOST_PORTS    host:port the pod should dial, as connstr reports it
#   YB_STORAGE_ROLE  role to create (default amp_storage)
#   YB_DATABASE      database to grant CONNECT on; must already exist (yb_storage)
#   YB_EXEC_POD      tserver pod to exec into; found from the published Service when unset
set -euo pipefail

NS="${NS:?NS is required}"
UNIVERSE_NS="${UNIVERSE_NS:-${NS}}"
YB_HOST_PORTS="${YB_HOST_PORTS:?YB_HOST_PORTS is required}"
YB_STORAGE_ROLE="${YB_STORAGE_ROLE:-amp_storage}"
YB_DATABASE="${YB_DATABASE:-yb_storage}"
YB_EXEC_POD="${YB_EXEC_POD:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { printf '\nERROR: %s\n' "$*" >&2; exit 1; }
say() { printf '\n==> %s\n' "$*"; }

# host only: the pod dials 5433 itself, and YB_HOST_PORTS carries the port.
YB_HOST="${YB_HOST_PORTS%%:*}"

if [ -z "${YB_EXEC_POD}" ]; then
  YB_EXEC_POD="$(UNIVERSE_NS="${UNIVERSE_NS}" "${SCRIPT_DIR}/../tserver-pod.sh" || true)"
fi
[ -n "${YB_EXEC_POD}" ] || die "no tserver pod in namespace ${UNIVERSE_NS}; set YB_EXEC_POD"

say "role ${YB_STORAGE_ROLE} on ${YB_DATABASE} (via ${YB_EXEC_POD} in ${UNIVERSE_NS})"
kubectl -n "${UNIVERSE_NS}" exec -i "${YB_EXEC_POD}" -c yb-tserver -- \
  bash -s -- "${YB_HOST}" "${YB_STORAGE_ROLE}" "${YB_DATABASE}" <<'REMOTE'
set -euo pipefail
H="$1"; ROLE="$2"; DB="$3"
# The binaries sit under the tserver's own Postgres install on a YBA-managed
# universe and on PATH in the chart image; take whichever is there.
YSQLSH="$(ls /home/yugabyte/tserver/postgres/bin/ysqlsh 2>/dev/null \
  || command -v ysqlsh \
  || echo /home/yugabyte/tserver/bin/ysqlsh)"
# The node's own client certificate when the pod has one. The chart lane puts it
# at /root/.yugabytedb; a YBA-managed pod does not expose it to this exec, so
# that path degrades to the CA alone and then to plain `require`.
#
# Safe because neither lane makes `yugabyte` a certificate login any more: the
# foundation spec sets no hba rules at all, and yugabyte-tls.sh's own rules put
# `hostssl all yugabyte all cert` first only on the lane whose pods DO have the
# certificate. A deployment that adds such a rule here gets a refusal at connect
# rather than a silent fallthrough, which is the right failure.
CONN="host=$H port=5433 user=yugabyte dbname=yugabyte"
if [ -r /root/.yugabytedb/root.crt ]; then
  CONN="$CONN sslmode=verify-full sslrootcert=/root/.yugabytedb/root.crt"
  if [ -r /root/.yugabytedb/yugabytedb.key ]; then
    CONN="$CONN sslcert=/root/.yugabytedb/yugabytedb.crt sslkey=/root/.yugabytedb/yugabytedb.key"
  fi
else
  CONN="$CONN sslmode=require"
fi
has_role="$("$YSQLSH" "$CONN" -tAc "SELECT 1 FROM pg_roles WHERE rolname='$ROLE'")"
if [ "$has_role" = "1" ]; then
  echo "   role $ROLE already exists"
else
  "$YSQLSH" "$CONN" -c "CREATE ROLE \"$ROLE\" LOGIN" >/dev/null
  # Member of yugabyte until the controller provisions explicit grants at
  # startup.
  "$YSQLSH" "$CONN" -c "GRANT yugabyte TO \"$ROLE\"" >/dev/null
  echo "   created role $ROLE (member of yugabyte)"
fi
"$YSQLSH" "$CONN" -c "GRANT CONNECT ON DATABASE \"$DB\" TO \"$ROLE\"" >/dev/null
echo "   granted CONNECT on $DB to $ROLE"
REMOTE
