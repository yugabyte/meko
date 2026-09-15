#!/usr/bin/env bash
#
# Ingress bakeoff: Traefik vs Envoy Gateway as the shard tier for per-compute
# Postgres ingress. Answers the questions that decide the design:
#
#   sni      Does the tier route by SNI while the POD terminates TLS? Only
#            Traefik plausibly can (it speaks the Postgres SSLRequest preamble),
#            and the answer decides whether a bespoke splicer is on the roadmap.
#   port     Does port-per-compute routing work, and reach the right backend?
#   churn    Does adding or removing a route disturb sessions already open on
#            the shard's other ports?
#   scale    How long does programming one more route take once N exist?
#   allowlist Can source-IP filtering stay per endpoint behind a shared listener?
#   proxyproto Does that filter read the client a load balancer declares, rather
#            than the TCP peer — which is what makes the allowlist true in
#            production, where the peer is always the load balancer.
#
# Safety: everything lands in one namespace and every Service is ClusterIP, so a
# run provisions no cloud load balancer. Two steps ARE cluster-scoped: the
# explicit `install-crds`, and `deploy envoy` — whose chart installs the Gateway
# API CRDs and Envoy Gateway's own ClusterRole whether or not `install-crds` ran
# first. Both say so before acting.
set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

usage() {
  cat <<EOF
usage: $(basename "$0") <command> [tier]

  preflight              check context, tools and what is already installed
  install-crds           install Gateway API EXPERIMENTAL CRDs (cluster-scoped!)
  setup                  namespace, certs, $BACKENDS TLS-enabled Postgres backends, client pod
  deploy   traefik|envoy install the tier and its routes
  test     traefik|envoy run sni, port, churn and allowlist against an installed tier
  proxyproto traefik    assert the allowlist reads the client a load balancer declares
  scale    traefik|envoy [routes] time route programming at N routes (default 200)
  report                 print the comparison table
  teardown [--all]       remove the namespace; --all also removes the tiers

env: BAKEOFF_CONTEXT=$CTX  BAKEOFF_NS=$NS  BAKEOFF_BACKENDS=$BACKENDS  BAKEOFF_ZONE=$ZONE
EOF
}

# --------------------------------------------------------------------- preflight
cmd_preflight() {
  say "preflight"
  for t in kubectl helm openssl; do
    command -v "$t" >/dev/null || die "$t not on PATH"
  done
  k version -o json >/dev/null 2>&1 || die "cannot reach context $CTX"
  info "context   $CTX (nodes at $(k get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}'))"
  info "nodes     $(k get nodes --no-headers | wc -l | tr -d ' ')"

  if k get crd tcproutes.gateway.networking.k8s.io >/dev/null 2>&1; then
    info "gateway   TCPRoute CRD present"
  else
    warn "gateway   TCPRoute CRD absent — 'install-crds' needed for the envoy tier"
  fi
  k get crd ingressroutetcps.traefik.io >/dev/null 2>&1 \
    && info "traefik   CRDs present" || info "traefik   CRDs absent (helm install adds them)"

  # Findings that feed straight back into the plan.
  if k -n external-dns get deploy external-dns >/dev/null 2>&1; then
    local args; args="$(k -n external-dns get deploy external-dns \
      -o jsonpath='{.spec.template.spec.containers[0].args}')"
    grep -q 'source=crd' <<<"$args" \
      && info "extdns    crd source ENABLED" \
      || warn "extdns    crd source NOT enabled — DNSEndpoint objects are inert until it is"
    grep -q 'policy=sync' <<<"$args" \
      || warn "extdns    policy is not 'sync' — records are never deleted, so retire leaks them"
    # Config is the easy half. On meko-dev-eks external-dns also has no AWS
    # credentials at all and has failed every reconcile since it was installed,
    # which no amount of flag-reading reveals — only its log does.
    if k -n external-dns logs deploy/external-dns --tail=5 2>/dev/null \
         | grep -qi 'get credentials\|no EC2 IMDS role\|AccessDenied'; then
      warn "extdns    CANNOT REACH ROUTE53 — no usable AWS credentials; it has never published a record"
    fi
  fi
}

cmd_install_crds() {
  local ver="${GATEWAY_API_VERSION:-v1.2.1}"
  warn "installing CLUSTER-SCOPED Gateway API $ver (experimental channel) on $CTX"
  read -r -p "   type the cluster name to confirm: " ans
  [[ "$ans" == "$CTX" ]] || die "not confirmed"
  k apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/$ver/experimental-install.yaml"
}

# ------------------------------------------------------------------------ setup
cmd_setup() {
  say "setup"
  mkdir -p "$WORK"; : >"$RESULTS"
  gen_certs; info "certs in $CERTS (backend CA + proxy CA, deliberately unrelated)"

  k create namespace "$NS" --dry-run=client -o yaml | k apply -f - >/dev/null

  for i in $(seq 0 $((BACKENDS - 1))); do
    kn create secret generic "bk$i-tls" \
      --from-file=server.crt="$CERTS/bk$i.crt" \
      --from-file=server.key="$CERTS/bk$i.key" \
      --dry-run=client -o yaml | kn apply -f - >/dev/null
    BK_ID="$i" BK_NAME="bk$i" envsubst <"$HERE/templates/backend.yaml" | kn apply -f - >/dev/null
    info "backend bk$i  cluster_name=bk$i  cert CN=$(sni_name "$i")"
  done

  kn create secret generic bakeoff-ca \
    --from-file=backend-ca.crt="$CERTS/backend-ca.crt" \
    --from-file=proxy-ca.crt="$CERTS/proxy-ca.crt" \
    --dry-run=client -o yaml | kn apply -f - >/dev/null
  kn apply -f "$HERE/templates/client.yaml" >/dev/null

  for i in $(seq 0 $((BACKENDS - 1))); do wait_rollout "deploy/bk$i"; done
  kn wait --for=condition=Ready pod/bakeoff-client --timeout=120s >/dev/null
  ok "backends and client ready"
}

# ----------------------------------------------------------------------- deploy
deploy_traefik() {
  say "deploy traefik"
  local ports=() vals="$WORK/traefik-values.yaml"
  {
    echo "deployment: { replicas: 2 }"
    echo "service: { type: ClusterIP }"
    echo "ingressRoute: { dashboard: { enabled: false } }"
    echo "providers: { kubernetesCRD: { enabled: true, allowCrossNamespace: false } }"
    echo "ports:"
    echo "  web:        { expose: { default: false } }"
    echo "  websecure:  { expose: { default: false } }"
    echo "  pgsni:      { port: $SNI_PORT, exposedPort: $SNI_PORT, protocol: TCP, expose: { default: true } }"
    for i in $(seq 0 $((PORT_SLOTS - 1))); do
      local p; p="$(port_for "$i")"
      # Only PP_SLOT trusts PROXY protocol. Trust is not per-connection: an
      # entryPoint that accepts a header from a peer REQUIRES one from that peer,
      # so enabling it everywhere would make every ordinary client connection
      # malformed and hang the other tests.
      if [[ "$i" == "$PP_SLOT" ]]; then
        echo "  p$p:        { port: $p, exposedPort: $p, protocol: TCP, expose: { default: true }, proxyProtocol: { trustedIPs: [\"10.0.0.0/8\"] } }"
      else
        echo "  p$p:        { port: $p, exposedPort: $p, protocol: TCP, expose: { default: true } }"
      fi
    done
  } >"$vals"

  helm --kube-context "$CTX" repo add traefik https://traefik.github.io/charts >/dev/null 2>&1 || true
  helm --kube-context "$CTX" repo update traefik >/dev/null
  helm --kube-context "$CTX" upgrade --install traefik traefik/traefik \
    -n "$NS" -f "$vals" --wait --timeout 5m >/dev/null
  wait_rollout deploy/traefik

  for i in $(seq 0 $((BACKENDS - 1))); do
    BK_NAME="bk$i" PORT="$(port_for "$i")" SNI="$(sni_name "$i")" SNI_PORT="$SNI_PORT" \
      envsubst <"$HERE/templates/traefik-routes.yaml" | kn apply -f - >/dev/null
  done
  ok "traefik up with $BACKENDS port routes and $BACKENDS SNI routes"
}

deploy_envoy() {
  say "deploy envoy gateway"
  # The gateway-helm chart carries the Gateway API CRDs in a subchart, so this
  # install is cluster-scoped whether or not `install-crds` ran first.
  k get crd tcproutes.gateway.networking.k8s.io >/dev/null 2>&1 \
    || warn "installing CLUSTER-SCOPED Gateway API CRDs as part of the chart"

  helm --kube-context "$CTX" upgrade --install eg \
    oci://docker.io/envoyproxy/gateway-helm --version "${EG_VERSION:-1.9.1}" \
    -n "$EG_NS" --create-namespace --wait --timeout 5m >/dev/null
  keg rollout status deploy/envoy-gateway --timeout=180s >/dev/null

  # ClusterIP, so a bakeoff never provisions an NLB on a shared cluster.
  kn apply -f "$HERE/templates/envoy-proxy-config.yaml" >/dev/null

  {
    PORTS=""
    for i in $(seq 0 $((PORT_SLOTS - 1))); do
      PORTS+="    - name: port-$(port_for "$i")"$'\n'
      PORTS+="      protocol: TCP"$'\n'
      PORTS+="      port: $(port_for "$i")"$'\n'
      PORTS+="      allowedRoutes: { namespaces: { from: Same } }"$'\n'
    done
    export PORTS
    envsubst <"$HERE/templates/envoy-gateway.yaml"
  } | kn apply -f - >/dev/null

  for i in $(seq 0 $((BACKENDS - 1))); do
    BK_NAME="bk$i" PORT="$(port_for "$i")" envsubst <"$HERE/templates/envoy-route.yaml" | kn apply -f - >/dev/null
  done

  # The data-plane Deployment is created by the controller, so wait on it by label.
  local tries=0
  until keg get deploy -l gateway.envoyproxy.io/owning-gateway-name=bakeoff \
        -o name 2>/dev/null | grep -q deployment; do
    ((tries++ < 60)) || die "envoy data plane never appeared in $EG_NS"
    sleep 2
  done
  keg rollout status "$(keg get deploy -l gateway.envoyproxy.io/owning-gateway-name=bakeoff -o name | head -1)" --timeout=180s >/dev/null
  ok "envoy gateway up with $BACKENDS TCP listeners"
}

tier_ip() {
  case "$1" in
    traefik) svc_ip traefik ;;
    envoy)   keg get svc -l gateway.envoyproxy.io/owning-gateway-name=bakeoff \
               -o jsonpath='{.items[0].spec.clusterIP}' ;;
  esac
}

# ------------------------------------------------------------------------ tests
#
# T1 is the decisive one. Trusting ONLY the backend CA, a successful verify-full
# through the shared SNI port means the pod terminated TLS: a proxy that
# terminated could not have produced a chain to that CA.
test_sni() {
  local tier="$1" ip; ip="$(tier_ip "$tier")"
  say "[$tier] sni — pod-terminated TLS through a shared port"

  if [[ "$tier" == "envoy" ]]; then
    record "$tier" sni "N/A" "no Postgres preamble handler; cannot reach a ClientHello"
    info "skipped: envoy's tls_inspector runs before the filter chain, so the"
    info "SSLRequest preamble is never consumed and SNI is never seen."
    return 0
  fi

  local out; out="$(pg "$(sni_name 0)" "$ip" "$SNI_PORT" \
    "sslmode=verify-full sslrootcert=/ca/backend-ca.crt" || true)"
  if [[ "$out" == "bk0" ]]; then
    ok "verify-full against the BACKEND ca succeeded → TLS terminates at the pod"
    record "$tier" sni PASS "passthrough confirmed; splicer not needed"
  elif grep -qiE "certificate verify failed|self.signed|unknown ca" <<<"$out"; then
    bad "chain did not reach the backend CA → the proxy terminated TLS"
    record "$tier" sni FAIL "terminates; a bespoke splicer stays on the roadmap"
  else
    bad "inconclusive: $out"
    record "$tier" sni ERROR "$(head -c 120 <<<"$out")"
  fi

  # Control: the same connection must fail against an unrelated CA. Without this
  # a proxy that ignores verification entirely would read as a pass above.
  local ctl; ctl="$(pg "$(sni_name 0)" "$ip" "$SNI_PORT" \
    "sslmode=verify-full sslrootcert=/ca/proxy-ca.crt" || true)"
  # Guarded as if/fi, not `[[ ]] && warn`: the latter returns non-zero when the
  # control passes, which under `set -e` silently ends the run before test_port.
  if [[ "$ctl" == "bk0" ]]; then
    # Overwrite the verdict rather than only warning: `report` reads the file
    # verbatim, so a PASS row left standing beside a stdout warning is what
    # someone reads back later.
    warn "control FAILED: verification is not being enforced — voiding the result above"
    record "$tier" sni VOID "control connection was admitted; the PASS above proves nothing"
  else
    info "control ok: the same connection is rejected against an unrelated CA"
  fi
}

test_port() {
  local tier="$1" ip fails=0; ip="$(tier_ip "$tier")"
  say "[$tier] port — one port per backend, and it reaches the right one"
  for i in $(seq 0 $((BACKENDS - 1))); do
    local got; got="$(pg "$(sni_name "$i")" "$ip" "$(port_for "$i")" "sslmode=require" || true)"
    if [[ "$got" == "bk$i" ]]; then ok "port $(port_for "$i") → bk$i"
    # NOT ((fails++)): with fails=0 the post-increment evaluates to 0, the
    # arithmetic command exits 1, and set -e kills the run on the FIRST misroute
    # — before the FAIL row is recorded and before the later tests run at all.
    else bad "port $(port_for "$i") → '$got' (wanted bk$i)"; fails=$((fails + 1)); fi
  done
  ((fails == 0)) && record "$tier" port PASS "$BACKENDS/$BACKENDS routed correctly" \
                || record "$tier" port FAIL "$fails/$BACKENDS misrouted"
}

# T3: hold sessions open on every port, mutate a route, and see who survived.
# A tier that reprograms by restarting its listeners fails here, and that failure
# is invisible in any test that only opens short-lived connections.
test_churn() {
  local tier="$1" ip; ip="$(tier_ip "$tier")"
  local hold="${BAKEOFF_CHURN_SECONDS:-60}"
  say "[$tier] churn — do sessions on sibling ports survive a route change?"

  local per="${BAKEOFF_CHURN_SESSIONS:-8}" i j
  for i in $(seq 0 $((BACKENDS - 1))); do
    for j in $(seq 1 "$per"); do
      kn exec bakeoff-client -- sh -c \
        "PGPASSWORD=$PGPASS nohup psql 'host=$(sni_name "$i") hostaddr=$ip port=$(port_for "$i") user=postgres dbname=postgres sslmode=require' \
         -qtAX -c 'SELECT pg_sleep($hold)' >/tmp/hold.$i.$j 2>&1 &" || true
    done
  done
  sleep 5
  local before; before="$(kn exec "deploy/bk0" -- psql -U postgres -qtAX \
    -c "SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'SELECT pg_sleep%'" 2>/dev/null | tr -d ' ')"
  info "sessions held on bk0: ${before:-0}"

  local extra=$((BACKENDS + 10))
  case "$tier" in
    traefik) BK_NAME=bk0 PORT="$(port_for "$extra")" SNI="$(sni_name "$extra")" SNI_PORT="$SNI_PORT" \
               envsubst <"$HERE/templates/traefik-routes.yaml" | kn apply -f - >/dev/null ;;
    envoy)   BK_NAME=bk0 PORT="$(port_for "$extra")" envsubst <"$HERE/templates/envoy-route.yaml" \
               | kn apply -f - >/dev/null ;;
  esac
  info "route added; waiting 15s for the tier to reprogram"
  sleep 15

  local after; after="$(kn exec "deploy/bk0" -- psql -U postgres -qtAX \
    -c "SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'SELECT pg_sleep%'" 2>/dev/null | tr -d ' ')"
  if [[ "${after:-0}" == "${before:-1}" && "${before:-0}" != "0" ]]; then
    ok "all ${before} sessions survived the route change"
    record "$tier" churn PASS "${after}/${before} sessions survived"
  else
    bad "sessions before=${before:-0} after=${after:-0}"
    record "$tier" churn FAIL "${after:-0}/${before:-0} survived"
  fi
}

# T4: the cost that scales with 1,000 computes is route programming, not
# throughput. The listener range is static in our design — a shard provisions it
# once at build — so this provisions listeners first, then measures only route
# churn against them, which is what production actually does.
#
# Gateway API caps a Gateway at 64 listeners, so Envoy needs one Gateway per
# ~50 ports. Traefik has no equivalent cap; its entryPoints are static config.
EG_MAX_LISTENERS=50

provision_ports() {
  local tier="$1" n="$2"
  case "$tier" in
    traefik)
      local vals="$WORK/traefik-scale-values.yaml" i p
      {
        echo "deployment: { replicas: 2 }"
        echo "service: { type: ClusterIP }"
        echo "ingressRoute: { dashboard: { enabled: false } }"
        echo "ports:"
        echo "  web:       { expose: { default: false } }"
        echo "  websecure: { expose: { default: false } }"
        echo "  pgsni:     { port: $SNI_PORT, exposedPort: $SNI_PORT, protocol: TCP, expose: { default: true } }"
        for i in $(seq 0 "$n"); do
          p="$(port_for "$i")"
          echo "  p$p: { port: $p, exposedPort: $p, protocol: TCP, expose: { default: true } }"
        done
      } >"$vals"
      info "helm upgrade: $((n + 1)) entryPoints on one traefik"
      helm --kube-context "$CTX" upgrade --install traefik traefik/traefik \
        -n "$NS" -f "$vals" --wait --timeout 10m >/dev/null
      ;;
    envoy)
      local gw=0 i=0 lo
      while ((i <= n)); do
        local ports=""
        for ((lo = 0; lo < EG_MAX_LISTENERS && i <= n; lo++, i++)); do
          ports+="    - name: port-$(port_for "$i")"$'\n'
          ports+="      protocol: TCP"$'\n'
          ports+="      port: $(port_for "$i")"$'\n'
          ports+="      allowedRoutes: { namespaces: { from: Same } }"$'\n'
        done
        PORTS="$ports" GW="bakeoff-$gw" envsubst <"$HERE/templates/envoy-gateway-n.yaml" | kn apply -f - >/dev/null
        # NOT ((gw++)): post-increment returns the OLD value, so the first pass
        # exits 1 and `set -e` ends the run with one gateway created.
        gw=$((gw + 1))
      done
      info "$gw gateways x <=$EG_MAX_LISTENERS listeners (Gateway API caps one Gateway at 64)"
      sleep 20
      ;;
  esac
}

route_doc() {
  local tier="$1" idx="$2" gw
  case "$tier" in
    traefik) BK_NAME=bk0 PORT="$(port_for "$idx")" SNI="$(sni_name "$idx")" SNI_PORT="$SNI_PORT" \
               envsubst <"$HERE/templates/traefik-routes.yaml" ;;
    envoy)   gw="bakeoff-$((idx / EG_MAX_LISTENERS))"
             BK_NAME=bk0 PORT="$(port_for "$idx")" GW="$gw" \
               envsubst <"$HERE/templates/envoy-route-n.yaml" ;;
  esac
  echo "---"
}

cmd_scale() {
  local tier="$1" n="${2:-200}" ip
  say "[$tier] scale — programming latency with $n routes present"

  provision_ports "$tier" $((n + 2))
  # Each Envoy Gateway gets its own Service, so the probe's address depends on
  # which gateway its port landed on; Traefik serves the whole range from one.
  case "$tier" in
    traefik) ip="$(svc_ip traefik)" ;;
    envoy)   ip="$(keg get svc -l "gateway.envoyproxy.io/owning-gateway-name=bakeoff-$(( ${2:-200} / EG_MAX_LISTENERS ))" \
                    -o jsonpath='{.items[0].spec.clusterIP}')" ;;
  esac

  local i
  for i in $(seq 0 $((n - 1))); do route_doc "$tier" "$i"; done | kn apply -f - >/dev/null
  info "$n routes applied; settling 30s"
  sleep 30
  info "routes live: $(kn get ingressroutetcp,tcproute --no-headers 2>/dev/null | wc -l | tr -d ' ')"

  local probe="$n" start elapsed
  start="$(date +%s)"
  route_doc "$tier" "$probe" | kn apply -f - >/dev/null
  # Serving, not merely Accepted: a programmed listener is all a client cares about.
  until [[ "$(pg "$(sni_name "$probe")" "$ip" "$(port_for "$probe")" "sslmode=require" || true)" == "bk0" ]]; do
    elapsed=$(( $(date +%s) - start ))
    ((elapsed < 240)) || { bad "route $probe never served (>240s)"; record "$tier" "scale@$n" FAIL ">240s"; return 0; }
    sleep 2
  done
  elapsed=$(( $(date +%s) - start ))
  ok "route $((probe + 1)) served ${elapsed}s after apply"
  record "$tier" "scale@$n" PASS "${elapsed}s to serve route $((probe + 1))"

  kn top pods -l app.kubernetes.io/name=traefik 2>/dev/null | tail -n +2 | while read -r p c m _; do
    info "traefik $p cpu=$c mem=$m"; done
  keg top pods -l gateway.envoyproxy.io/owning-gateway-name=bakeoff-0 2>/dev/null | tail -n +2 | while read -r p c m _; do
    info "envoy $p cpu=$c mem=$m"; done
}

# T5: per-endpoint source-IP filtering on a SHARED load balancer. This is the
# capability `loadBalancerSourceRanges` cannot provide once one LB fronts many
# endpoints, because that field is per Service and a shard has one.
test_allowlist() {
  local tier="$1" ip idx=5 client; ip="$(tier_ip "$tier")"
  say "[$tier] allowlist — per-endpoint CIDR filtering behind a shared listener"

  if [[ "$tier" != "traefik" ]]; then
    record "$tier" allowlist "N/A" "no per-route TCP source-IP filter in this tier"
    info "skipped: only implemented for traefik's MiddlewareTCP here"
    return 0
  fi

  client="$(kn get pod bakeoff-client -o jsonpath='{.status.podIP}')"
  # Defensive: PP_SLOT and this idx sit above the backend range, so nothing
  # should be bound here — but a route left by an earlier run would mask the
  # guarded one and the test would silently assert nothing.
  kn delete ingressroutetcp "bk0-port-$(port_for $idx)" "bk0-sni-$(port_for $idx)" --ignore-not-found >/dev/null 2>&1 || true

  local out
  BK_NAME=bk0 PORT="$(port_for $idx)" CIDRS='"10.255.255.0/32"' \
    envsubst <"$HERE/templates/traefik-allowlist.yaml" | kn apply -f - >/dev/null
  sleep 10
  out="$(pg "$(sni_name $idx)" "$ip" "$(port_for $idx)" "sslmode=require connect_timeout=8" || true)"
  if [[ "$out" == "bk0" ]]; then
    bad "a client outside the allowlist still connected"
    record "$tier" allowlist FAIL "excluded client was admitted"
    return 0
  fi
  ok "excluded client refused"

  BK_NAME=bk0 PORT="$(port_for $idx)" CIDRS="\"$client/32\"" \
    envsubst <"$HERE/templates/traefik-allowlist.yaml" | kn apply -f - >/dev/null
  sleep 10
  out="$(pg "$(sni_name $idx)" "$ip" "$(port_for $idx)" "sslmode=require connect_timeout=8" || true)"
  if [[ "$out" == "bk0" ]]; then
    ok "same route, widened to the client's /32, admits it"
    record "$tier" allowlist PASS "per-route CIDR filter enforced both ways"
  else
    bad "widened allowlist still refused: $out"
    record "$tier" allowlist FAIL "widened allowlist did not admit"
  fi
}

# T6: the allowlist behind a cloud load balancer. Without PROXY protocol the
# proxy sees the load balancer as the client and every per-endpoint allowlist
# matches the wrong address — so this asserts the filter reads the DECLARED
# client, not the TCP peer, which is the only thing that makes T5 true in
# production.
#
# A relay stands in for the load balancer: it prepends a PROXY v2 header claiming
# an address it does not hold, so an allowlist naming the relay's real address
# must NOT admit it. That asymmetry is the whole test.
test_proxyproto() {
  local tier="$1" idx="$PP_SLOT" claim="192.0.2.77" relay_ip
  say "[$tier] proxyproto — does the allowlist see the real client behind a load balancer?"
  if [[ "$tier" != "traefik" ]]; then
    record "$tier" proxyproto "N/A" "only implemented for traefik's entryPoint proxyProtocol"
    info "skipped: not implemented for this tier"
    return 0
  fi

  # 192.0.2.0/24 is TEST-NET-1, so the claimed address can never collide with a
  # real pod and a pass cannot be an accident of cluster addressing.
  CLAIM_IP="$claim" UPSTREAM_HOST="$(svc_ip traefik)" UPSTREAM_PORT="$(port_for $idx)" \
    envsubst <"$HERE/templates/proxyproto-relay.yaml" | kn apply -f - >/dev/null
  wait_rollout deploy/pp-relay
  relay_ip="$(svc_ip pp-relay)"

  local out
  probe_via_relay() {
    kn exec bakeoff-client -- env PGPASSWORD="$PGPASS" \
      psql "host=$(sni_name $idx) hostaddr=$relay_ip port=5432 user=postgres dbname=postgres sslmode=require connect_timeout=8" \
      -qtAX -c "SELECT current_setting('cluster_name')" 2>&1 | tail -1
  }
  apply_guarded() {
    BK_NAME=bk0 PORT="$(port_for $idx)" CIDRS="$1" \
      envsubst <"$HERE/templates/traefik-allowlist.yaml" | kn apply -f - >/dev/null
    sleep 10
  }
  kn delete ingressroutetcp "bk0-port-$(port_for $idx)" "bk0-sni-$(port_for $idx)" --ignore-not-found >/dev/null 2>&1 || true

  apply_guarded "\"$claim/32\""
  out="$(probe_via_relay || true)"
  if [[ "$out" != "bk0" ]]; then
    bad "allowlisting the DECLARED client did not admit it: $out"
    record "$tier" proxyproto FAIL "declared client refused; the PROXY header is not being read"
    return 0
  fi
  ok "allowlisting the declared client ($claim) admits it"

  # The discriminator: the relay's OWN address must not work, or the filter is
  # reading the TCP peer and the header is decorative.
  apply_guarded "\"10.0.0.0/8\""
  out="$(probe_via_relay || true)"
  if [[ "$out" == "bk0" ]]; then
    bad "allowlisting the relay's real address admitted it — the filter reads the TCP peer, not the PROXY header"
    record "$tier" proxyproto FAIL "filters on the TCP peer; per-endpoint allowlists would match the load balancer"
  else
    ok "allowlisting the relay's real address does NOT admit it"
    record "$tier" proxyproto PASS "filters on the PROXY-declared client, not the TCP peer"
  fi
}

cmd_test() { local t="$1"; test_sni "$t"; test_port "$t"; test_churn "$t"; test_allowlist "$t"; }

cmd_report() {
  say "results"
  [[ -s "$RESULTS" ]] || die "no results yet — run 'test' first"
  printf '   %-9s %-12s %-7s %s\n' TIER TEST VERDICT DETAIL
  printf '   %-9s %-12s %-7s %s\n' --------- ------------ ------- ------------------------------
  while IFS='|' read -r tier test verdict detail; do
    printf '   %-9s %-12s %-7s %s\n' "$tier" "$test" "$verdict" "$detail"
  done <"$RESULTS"
  echo
  info "raw: $RESULTS"
}

cmd_teardown() {
  say "teardown"
  kn delete all --all >/dev/null 2>&1 || true
  k delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
  if [[ "${1:-}" == "--all" ]]; then
    helm --kube-context "$CTX" uninstall eg -n "$EG_NS" >/dev/null 2>&1 || true
    warn "Gateway API CRDs left installed — they are cluster-scoped; remove by hand if unwanted"
  fi
  ok "namespace $NS removed"
}

case "${1:-}" in
  preflight)    cmd_preflight ;;
  install-crds) cmd_install_crds ;;
  setup)        cmd_setup ;;
  deploy)       case "${2:?tier}" in traefik) deploy_traefik ;; envoy) deploy_envoy ;; *) die "unknown tier" ;; esac ;;
  test)         cmd_test "${2:?tier}" ;;
  proxyproto)   test_proxyproto "${2:?tier}" ;;
  scale)        cmd_scale "${2:?tier}" "${3:-200}" ;;
  report)       cmd_report ;;
  teardown)     cmd_teardown "${2:-}" ;;
  *)            usage; exit 1 ;;
esac
