#!/usr/bin/env bash
# Shared helpers for the ingress bakeoff. Sourced by run.sh; not executable on its own.

NS="${BAKEOFF_NS:-ingress-bakeoff}"
CTX="${BAKEOFF_CONTEXT:-meko-dev-eks}"
ZONE="${BAKEOFF_ZONE:-bakeoff.test}"
BACKENDS="${BAKEOFF_BACKENDS:-3}"
PORT_LO="${BAKEOFF_PORT_LO:-20000}"
# Listener slots each tier provisions, deliberately MORE than there are backends.
# Tests that add a route mid-run (churn, allowlist) need an entryPoint that
# already exists: a route naming an undefined one is accepted by Traefik and then
# never programmed, so the mutation never reaches the data plane and the test
# measures nothing while reporting PASS.
PORT_SLOTS="${BAKEOFF_PORT_SLOTS:-$((BACKENDS + 20))}"
# The one slot whose entryPoint trusts PROXY protocol, for the proxyproto test.
# Exactly one, because trust is not per-connection: an entryPoint that accepts
# PROXY headers from a peer REQUIRES one from that peer, so enabling it
# everywhere makes every ordinary client connection malformed.
PP_SLOT="${BAKEOFF_PP_SLOT:-7}"
SNI_PORT="${BAKEOFF_SNI_PORT:-5432}"
PGPASS="${BAKEOFF_PGPASSWORD:-bakeoff}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="${BAKEOFF_WORKDIR:-${TMPDIR:-/tmp}/ingress-bakeoff}"
CERTS="$WORK/certs"

# Envoy Gateway puts the data plane it creates in its OWN namespace, not
# alongside the Gateway — so data-plane lookups need this, not `kn`.
EG_NS="${BAKEOFF_EG_NS:-envoy-gateway-system}"

k()   { kubectl --context "$CTX" "$@"; }
keg() { kubectl --context "$CTX" -n "$EG_NS" "$@"; }
kn() { kubectl --context "$CTX" -n "$NS" "$@"; }

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
info() { printf '   %s\n' "$*"; }
ok()   { printf '   \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { printf '   \033[31mFAIL\033[0m %s\n' "$*"; }
warn() { printf '   \033[33m!\033[0m %s\n' "$*"; }
die()  { printf '\n\033[31mabort:\033[0m %s\n' "$*" >&2; exit 1; }

# Where results accumulate, one "tier|test|verdict|detail" line each.
RESULTS="$WORK/results.psv"
record() { printf '%s|%s|%s|%s\n' "$1" "$2" "$3" "$4" >>"$RESULTS"; }

sni_name() { printf 'ep-%s.%s' "$1" "$ZONE"; }
port_for() { printf '%s' "$((PORT_LO + $1))"; }

# ---------------------------------------------------------------- certificates
#
# Two independent CAs, and that separation is the whole point of the SNI test.
# Backends are signed by the BACKEND ca; a terminating proxy can only ever
# present something signed by the PROXY ca. A client that trusts only the
# backend CA and still completes verify-full therefore proves the pod terminated
# TLS itself — which is the fact the bakeoff exists to establish.
gen_certs() {
  # Cache keyed on the ZONE as well as existence: the certs carry it in their
  # SANs, and $CERTS outlives a run (nothing removes /tmp), so reusing a cache
  # minted for another zone yields certs that silently fail verification.
  mkdir -p "$CERTS"
  if [[ -f "$CERTS/backend-ca.crt" && "$(cat "$CERTS/.zone" 2>/dev/null)" == "$ZONE" ]]; then
    return 0
  fi
  rm -f "$CERTS"/*.crt "$CERTS"/*.key "$CERTS"/*.csr "$CERTS"/*.srl 2>/dev/null || true
  printf '%s' "$ZONE" >"$CERTS/.zone"

  openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
    -keyout "$CERTS/backend-ca.key" -out "$CERTS/backend-ca.crt" \
    -subj "/CN=bakeoff-backend-ca" 2>/dev/null
  # The proxy CA exists only as the negative control: a client trusting it and
  # nothing else must be refused, which is what rules out a tier that ignores
  # verification entirely. No leaf is minted under it — nothing here terminates
  # TLS, so there would be nothing to present it.
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
    -keyout "$CERTS/proxy-ca.key" -out "$CERTS/proxy-ca.crt" \
    -subj "/CN=bakeoff-proxy-ca" 2>/dev/null

  for i in $(seq 0 $((BACKENDS - 1))); do
    local cn; cn="$(sni_name "$i")"
    openssl req -newkey rsa:2048 -nodes \
      -keyout "$CERTS/bk$i.key" -out "$CERTS/bk$i.csr" -subj "/CN=$cn" 2>/dev/null
    openssl x509 -req -in "$CERTS/bk$i.csr" -days 30 \
      -CA "$CERTS/backend-ca.crt" -CAkey "$CERTS/backend-ca.key" -CAcreateserial \
      -extfile <(printf 'subjectAltName=DNS:%s\n' "$cn") \
      -out "$CERTS/bk$i.crt" 2>/dev/null
  done

}

# ------------------------------------------------------------------- addressing
#
# libpq sends `host` as the TLS SNI and dials `hostaddr`, so the two together
# reach a proxy by IP while presenting the name a route matches on. No DNS, no
# /etc/hosts, and the SNI under test is exactly the one a real client would send.
svc_ip() { kn get svc "$1" -o jsonpath='{.spec.clusterIP}'; }

# psql from inside the cluster, against $1=host-header $2=ip $3=port, rest args.
pg() {
  local host="$1" addr="$2" port="$3"; shift 3
  kn exec bakeoff-client -- env PGPASSWORD="$PGPASS" \
    psql "host=$host hostaddr=$addr port=$port user=postgres dbname=postgres $*" \
    -qtAX -c "SELECT current_setting('cluster_name')" 2>&1
}

wait_rollout() { kn rollout status "$1" --timeout="${2:-180s}" >/dev/null; }
