#!/usr/bin/env bash
#
# Create a database on an operator-managed universe.
#
# The foundation deployer used to do this, as the last step of `foundation up`.
# It was the only step that was not about standing up a universe, and the only
# one that needed pods/exec — which a foundation running as a pod would then
# have to be granted permanently. Creating the database belongs to whoever is
# going to use it, and YBA is expected to grow the capability later.
#
# Runs ysqlsh inside a tserver pod, the same shape as tls/yb-storage-role.sh,
# which creates the role this database is granted to. The two are separate
# because they have different owners: the database is the consumer's, the role
# is amp's.
#
# Idempotent: Postgres has no CREATE DATABASE IF NOT EXISTS, so it probes
# pg_database first and tolerates a racing create.
#
# Env:
#   NS             namespace holding the amp stack (required)
#   UNIVERSE_NS    namespace holding the DB pods (default: NS). They differ when
#                  a provider zone names its own kubeNamespace.
#   YB_HOST_PORTS  host:port the pod should dial, as `foundation connstr` reports
#   YB_DATABASE    database to create (default yb_storage)
#   YB_EXEC_POD    tserver pod to exec into; found from the published Service when unset
set -euo pipefail

NS="${NS:?NS is required}"
UNIVERSE_NS="${UNIVERSE_NS:-${NS}}"
YB_HOST_PORTS="${YB_HOST_PORTS:?YB_HOST_PORTS is required}"
YB_DATABASE="${YB_DATABASE:-yb_storage}"
YB_EXEC_POD="${YB_EXEC_POD:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { printf '\nERROR: %s\n' "$*" >&2; exit 1; }
say() { printf '\n==> %s\n' "$*"; }

# host only: the pod dials 5433 itself, and YB_HOST_PORTS carries the port.
YB_HOST="${YB_HOST_PORTS%%:*}"

if [ -z "${YB_EXEC_POD}" ]; then
  YB_EXEC_POD="$(UNIVERSE_NS="${UNIVERSE_NS}" "${SCRIPT_DIR}/tserver-pod.sh" || true)"
fi
[ -n "${YB_EXEC_POD}" ] || die "no tserver pod in namespace ${UNIVERSE_NS}; set YB_EXEC_POD"

say "database ${YB_DATABASE} (via ${YB_EXEC_POD} in ${UNIVERSE_NS})"
kubectl -n "${UNIVERSE_NS}" exec -i "${YB_EXEC_POD}" -c yb-tserver -- \
  bash -s -- "${YB_HOST}" "${YB_DATABASE}" <<'REMOTE'
set -euo pipefail
H="$1"; DB="$2"
# The binaries sit under the tserver's own Postgres install on a YBA-managed
# universe and on PATH in the chart image; take whichever is there.
YSQLSH="$(ls /home/yugabyte/tserver/postgres/bin/ysqlsh 2>/dev/null \
  || command -v ysqlsh \
  || echo /home/yugabyte/tserver/bin/ysqlsh)"
# The node's own client certificate when the pod has one, degrading to the CA
# alone and then to plain `require` — the same ladder yb-storage-role.sh climbs,
# because a YBA-managed pod does not expose /root/.yugabytedb to this exec. A
# plaintext universe refuses TLS outright, so ask for it only if the material is
# there to offer.
CONN="host=$H port=5433 user=yugabyte dbname=yugabyte"
if [ -r /root/.yugabytedb/root.crt ]; then
  CONN="$CONN sslmode=verify-full sslrootcert=/root/.yugabytedb/root.crt"
  if [ -r /root/.yugabytedb/yugabytedb.key ]; then
    CONN="$CONN sslcert=/root/.yugabytedb/yugabytedb.crt sslkey=/root/.yugabytedb/yugabytedb.key"
  fi
fi
# Exactly "1" when the database exists, empty when absent.
exists="$("$YSQLSH" "$CONN" -tAc "SELECT 1 FROM pg_database WHERE datname='$DB'")"
if [ "$exists" = "1" ]; then
  echo "   $DB already exists"
elif out="$("$YSQLSH" "$CONN" -c "CREATE DATABASE \"$DB\"" 2>&1)"; then
  echo "   created $DB"
elif printf '%s' "$out" | grep -q "already exists"; then
  echo "   $DB already exists"
else
  printf '   CREATE DATABASE %s failed: %s\n' "$DB" "$out" >&2
  exit 1
fi
REMOTE
