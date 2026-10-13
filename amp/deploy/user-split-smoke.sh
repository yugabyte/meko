#!/usr/bin/env bash
#
# Prove the endpoint user split holds on live endpoints (AMP-89).
#
# The split runs the pageserver, safekeeper and LFC state server as the storage
# user and compute-ctl/Postgres as the compute user, so that a compromised
# Postgres has no YugabyteDB credential. Unit tests cover what the supervisor
# builds, not what the kernel enforces once it runs as root with SETUID, so this
# checks the running pod:
#   1. each process runs with the uid, gid and supplementary groups it should,
#      read from /proc, and every expected process is there;
#   2. the compute's identity cannot read the YB client key or the daemons'
#      configs, and Postgres's environment carries no YB_* or password variable;
#   3. from that identity, the daemons refuse a state-changing request (403)
#      while still answering reads, and the LFC state server refuses a write for
#      another endpoint;
#   4. that identity reaches the LFC state server for its own endpoint.
#
# The reads and requests run under `setpriv` from a `kubectl exec` (root, with
# SETUID/SETGID, under no_new_privs) with the compute's uid and gid and no
# supplementary groups. Check 1 proves the real compute processes have exactly
# those, so the result carries over to them. The image has no curl, so HTTP goes
# over bash's /dev/tcp.
#
# Run it while endpoints are up: ycp-reconcile-smoke.sh calls it through
# BEFORE_WITHDRAW. Every live endpoint pod in NS is checked, and a check that
# cannot run is a failure, not a skip.
#
# Prereqs: kubectl pointed at the cluster, endpoints deployed with
# controller.endpoint.userSplit=true. ALLOW_NO_YB_KEY=1 accepts a backing without
# a YB client key. Exits non-zero on any failure.
set -euo pipefail

NS=${NS:?NS is required}

fail=0
ok() { printf '  ok   %s\n' "$*"; }
bad() {
  printf '  FAIL %s\n' "$*"
  fail=1
}

# Running and not being deleted: a Terminating pod still reports Running.
pods=$(kubectl -n "$NS" get pods -l app.kubernetes.io/name=yb-amp-endpoint \
  --field-selector=status.phase=Running \
  -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.deletionTimestamp}{"\n"}{end}' |
  awk 'NF == 1 {print $1}')
[ -n "$pods" ] || {
  echo "::error::no live endpoint pods in $NS"
  exit 1
}

# Runs in the endpoint container as root and prints one "ok|FAIL <check>" line
# per check, then END. The uids and gids are the chart's, from the container env.
read -r -d '' CHECKS <<'EOS' || true
set -u
allow_no_key=$1
say() { echo "$1 $2"; }
su=${STORAGE_UID:?} sg=${STORAGE_GID:?} cu=${COMPUTE_UID:?} cg=${COMPUTE_GID:?}
field() { awk -v k="$2:" '$1 == k {$1 = ""; sub(/^ /, ""); print}' "/proc/$1/status" 2>/dev/null; }

# 1. Identity of every process, from /proc. comm is cut at 15 characters.
seen=" "
for p in $(ps -eo pid=); do
  comm=$(cat "/proc/$p/comm" 2>/dev/null) || continue
  args=$(tr '\0' ' ' <"/proc/$p/cmdline" 2>/dev/null)
  case "$comm" in
    safekeeper | pageserver) name=$comm uid=$su gid=$sg ;;
    postgres)
      # The pageserver's WAL-redo children are postgres too, and run as storage.
      case "$args" in
        *--wal-redo*) name=walredo uid=$su gid=$sg ;;
        *) name=postgres uid=$cu gid=$cg ;;
      esac
      ;;
    compute-ctl) name=$comm uid=$cu gid=$cg ;;
    endpoint-superv)
      case "$args" in
        *serve-lfc-state*) name=lfc_state uid=$su gid=$sg ;;
        *) name=supervisor uid=0 gid= ;;
      esac
      ;;
    *) continue ;;
  esac
  seen="$seen$name "
  ids=$(field "$p" Uid)
  [ "$(echo $ids)" = "$uid $uid $uid $uid" ] || { say FAIL "$name (pid $p) uids '$ids', want $uid"; continue; }
  [ -z "$gid" ] && { say ok "$name (pid $p) runs as $uid"; continue; }
  gids=$(field "$p" Gid) groups=$(field "$p" Groups)
  if [ "$(echo $gids)" != "$gid $gid $gid $gid" ]; then
    say FAIL "$name (pid $p) gids '$gids', want $gid"
  elif [ "$uid" = "$cu" ] && [ -n "$(echo $groups)" ]; then
    say FAIL "$name (pid $p) keeps supplementary groups '$groups'"
  else
    say ok "$name (pid $p) runs as $uid:$gid, groups '$(echo $groups)'"
  fi
done
for want in supervisor safekeeper pageserver lfc_state compute-ctl postgres; do
  case "$seen" in *" $want "*) ;; *) say FAIL "no $want process" ;; esac
done

as_compute() { setpriv --reuid "$cu" --regid "$cg" --clear-groups -- bash -c "$1"; }

# 2. Postgres's environment. Read as the compute user: this root has no
# CAP_SYS_PTRACE, which reading another uid's environ needs.
pg=$(pgrep -o -x -u "$cu" postgres || true)
if [ -z "$pg" ]; then
  say FAIL "no postgres owned by $cu, so its environment cannot be checked"
elif ! pgenv=$(as_compute "tr '\\0' '\\n' </proc/$pg/environ" 2>/dev/null) || [ -z "$pgenv" ]; then
  say FAIL "cannot read postgres (pid $pg) environment"
else
  leaked=$(echo "$pgenv" | cut -d= -f1 | grep -E "^(YB_|COMPUTE_PASSWORD$|${YB_PASSWORD_ENV:-YB_PASSWORD}$)" || true)
  [ -z "$leaked" ] && say ok "postgres environment has no YB_* or password variable" || say FAIL "postgres environment carries: $(echo $leaked)"
  if [ -n "${YB_PASSWORD:-}" ] && echo "$pgenv" | grep -qF -- "$YB_PASSWORD"; then
    say FAIL "postgres environment contains the YB password value"
  fi
fi
http() { # method port path [body] -> status code
  local body=${4:-}
  as_compute "exec 3<>/dev/tcp/127.0.0.1/$2 || exit 1
    printf '%s %s HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' '$1' '$3' ${#body} '$body' >&3
    head -1 <&3 | cut -d' ' -f2" 2>/dev/null
}

# 2. Files the compute must not read. Each has to exist (root checks), so a
# refusal means permissions, and a missing file is a failure rather than a pass.
files="/data/pageserver/pageserver.toml /data/safekeeper/safekeeper.toml"
if [ -n "${YB_SSLKEY:-}" ]; then
  files="${YB_SSLKEY} $files"
elif [ "$allow_no_key" = 1 ]; then
  say ok "no YB_SSLKEY on this backing (ALLOW_NO_YB_KEY=1); key check skipped"
else
  say FAIL "YB_SSLKEY is unset, so the client-key check cannot run"
fi
for f in $files; do
  if [ ! -e "$f" ]; then
    say FAIL "$f does not exist"
  elif as_compute "cat '$f' >/dev/null 2>&1"; then
    say FAIL "compute identity can read $f"
  else
    say ok "compute identity cannot read $f"
  fi
done

# 3. The daemons refuse writes from the compute and still answer reads.
for d in "pageserver ${PAGESERVER_HTTP_PORT:-9898}" "safekeeper ${SAFEKEEPER_HTTP_PORT:-7676}"; do
  set -- $d
  s=$(http GET "$2" /v1/status); [ "$s" = 200 ] && say ok "$1 GET /v1/status -> 200" || say FAIL "$1 GET /v1/status -> ${s:-no answer}"
  s=$(http PUT "$2" /v1/failpoints '[]'); [ "$s" = 403 ] && say ok "$1 PUT /v1/failpoints -> 403" || say FAIL "$1 PUT /v1/failpoints -> ${s:-no answer}, want 403"
  s=$(http POST "$2" /v1/write_token '{}'); [ "$s" = 403 ] && say ok "$1 POST /v1/write_token -> 403" || say FAIL "$1 POST /v1/write_token -> ${s:-no answer}, want 403"
done

# 3-4. LFC state: own endpoint reachable (200, or 404 before the first offload),
# another endpoint's not writable.
lfc=${LFC_STATE_PORT:-9896}
if [ -n "${ENDPOINT_ID:-}" ]; then
  s=$(http GET "$lfc" "/v1/lfc_state/$ENDPOINT_ID")
  case "$s" in 200 | 404) say ok "LFC GET own state -> $s" ;; *) say FAIL "LFC GET own state -> ${s:-no answer}" ;; esac
else
  say FAIL "ENDPOINT_ID is unset, so the compute's LFC state cannot be checked"
fi
s=$(http PUT "$lfc" /v1/lfc_state/00000000-0000-4000-8000-000000000000 x)
[ "$s" = 403 ] && say ok "LFC PUT another endpoint -> 403" || say FAIL "LFC PUT another endpoint -> ${s:-no answer}, want 403"
echo END
EOS

for pod in $pods; do
  echo "== $pod"
  run_as=$(kubectl -n "$NS" get "pod/$pod" -o jsonpath='{.spec.containers[?(@.name=="endpoint")].securityContext.runAsUser}{.spec.securityContext.runAsUser}')
  case "$run_as" in
  0*) ;;
  *)
    bad "$pod is not split (runAsUser '$run_as'); was controller.endpoint.userSplit set?"
    continue
    ;;
  esac
  rc=0
  out=$(kubectl -n "$NS" exec "$pod" -c endpoint -- bash -c "$CHECKS" _ "${ALLOW_NO_YB_KEY:-0}" 2>&1) || rc=$?
  ended=0
  while IFS= read -r line; do
    case "$line" in
    "ok "*) ok "${line#ok }" ;;
    "FAIL "*) bad "${line#FAIL }" ;;
    END) ended=1 ;;
    "") ;;
    *) bad "unexpected output: $line" ;;
    esac
  done <<<"$out"
  # A check that never printed is not a pass.
  [ "$rc" = 0 ] && [ "$ended" = 1 ] || bad "$pod: the checks did not finish (exec exit $rc)"
done

[ "$fail" = 0 ] && echo "user split holds on every endpoint" || echo "::error::user split check failed"
exit "$fail"
