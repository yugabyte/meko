#!/usr/bin/env bash
#
# Prove YugabyteDB metrics export is actually on, from inside the cluster and
# with no metrics backend involved.
#
# The whole feature is declarative — a TelemetryProvider and a telemetry block on
# the YBUniverse (meko#1405) — and every link in the chain after that is
# somebody else's: the YB operator turns the CRs into YBA config, YBA renders a
# collector config, the DB chart puts it in an OpenTelemetryCollector, and the
# OTel Operator injects that as a sidecar. Each link fails
# quietly. A provider whose Secret is missing parks in Error with the reason only
# in YBA's log; a universe whose telemetry never reconciled looks completely
# healthy; and a collection level whose parameters are dropped collects the wrong
# thing while reporting nothing wrong at all.
#
# So this checks the links rather than the intent:
#   1. the TelemetryProvider reached Ready and YBA gave it a UUID;
#   2. every master and tserver pod has a sidecar, waiting out the rolling
#      upgrade that adds them;
#   3. the config YBA generated says what the spec asked for — the two export
#      types this pod serves, the collection level's parameters, and the
#      identity tags;
#   4. the collector has accepted datapoints AND successfully sent them on,
#      read over port-forward, because the collector image has no shell.
#
# Step 3 is the one that earns its keep. collectionLevel is the field that
# silently under- or over-collects, and reading the generated config is the only
# place the choice becomes visible before the storage bill does.
#
# Step 4 reads the EXPORTER counters, not just the receiver's. A collector can
# scrape perfectly and have every send refused — the BYOC door returns 415 to an
# uncompressed payload — and ingestion alone would read green through that. The
# same convention as ycp/cmd/reconcile-agent/preflight.go, which polls the send
# counters for this door for exactly that reason.
#
# A universe with no telemetry declared is not a failure — that is how a
# foundation comes up, before the install hint's step turns it on — so this says
# so and exits 0.
#
# Prereqs: kubectl pointed at the cluster, and curl. No backend, no credentials.
#
# Usage:
#   NS=amp amp/deploy/yb-metrics-smoke.sh
#   NS=amp DB_NS=yb-universe amp/deploy/yb-metrics-smoke.sh
set -uo pipefail

NS="${NS:?NS (the foundation namespace, where the CRs live) is required}"
# Where the DB pods are. The universe lands in the provider zones' kubeNamespace,
# which defaults to the foundation's namespace.
DB_NS="${DB_NS:-$NS}"
# The sidecar container name is the OpenTelemetry Operator's, not ours.
SIDECAR="otc-container"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}
ok() { echo "ok: $*"; }

# 1. The destination. Until this is Ready with a resourceUUID the operator does
#    not reconcile the universe's telemetry section at all, so a failure here
#    explains every later one.
LIST_ERR="$(mktemp)"
PROVIDERS="$(kubectl -n "$NS" get telemetryproviders.operator.yugabyte.io \
  -o jsonpath='{.items[*].metadata.name}' 2>"$LIST_ERR")"
LIST_RC=$?
# An empty list and a failed list are different answers. Without this, a missing
# CRD, a denied read or the wrong namespace all read as "nothing declared" and
# the whole smoke no-ops green.
if [ "$LIST_RC" -ne 0 ]; then
  fail "cannot list TelemetryProviders in $NS: $(tr -d '\n' <"$LIST_ERR"). That is not the same as none being declared — the CRD may be absent, or this context may not be allowed to read them."
fi
rm -f "$LIST_ERR"
if [ -z "$PROVIDERS" ]; then
  echo "note: no TelemetryProvider in $NS — this universe exports nothing yet."
  echo "      That is how a foundation comes up; the environment's install hint"
  echo "      prints the blocks that turn metrics on (meko#1405). Nothing to check."
  exit 0
fi
for p in $PROVIDERS; do
  state="$(kubectl -n "$NS" get telemetryprovider "$p" -o jsonpath='{.status.state}' 2>/dev/null)"
  uuid="$(kubectl -n "$NS" get telemetryprovider "$p" -o jsonpath='{.status.resourceUUID}' 2>/dev/null)"
  msg="$(kubectl -n "$NS" get telemetryprovider "$p" -o jsonpath='{.status.message}' 2>/dev/null)"
  [ "$state" = "Ready" ] || fail "TelemetryProvider $p is '${state:-<no state>}', not Ready${msg:+ — $msg}. A provider whose Secret is missing or whose endpoint is rejected parks here, and the universe's telemetry is never reconciled."
  [ -n "$uuid" ] || fail "TelemetryProvider $p is Ready but published no resourceUUID; YBA has not created it"
  ok "TelemetryProvider $p is Ready (YBA uuid ${uuid})"
done

# 2. The sidecar, which the DB chart injects because YBA set otelCollector on it.
#
# Polled, and across EVERY pod of both roles. Turning telemetry on is a rolling
# helm upgrade, and a StatefulSet rolls highest-ordinal first — so for several
# minutes some pods have the sidecar and some do not, and sampling one is a coin
# flip on which. Nothing upstream waits for this either: the universe's status
# has no observedGeneration, so right after the apply it still reads Ready from
# the previous generation and `foundation apply` returns before the roll starts.
yb_pods() {
  local sels sel out
  case "$1" in
  master) sels="app.kubernetes.io/name=yb-master app=yb-master" ;;
  tserver) sels="app.kubernetes.io/name=yb-tserver app=yb-tserver" ;;
  *) return 1 ;;
  esac
  for sel in $sels; do
    out="$(kubectl -n "$DB_NS" get pods -l "$sel" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)"
    if [ -n "$out" ]; then
      echo "$out"
      return 0
    fi
  done
  return 1
}

has_sidecar() {
  kubectl -n "$DB_NS" get pod "$1" \
    -o jsonpath='{.spec.containers[*].name}{" "}{.spec.initContainers[*].name}' 2>/dev/null |
    tr ' ' '\n' | grep -qx "$SIDECAR"
}

MASTER_PODS="$(yb_pods master)" || fail "no yb-master pods in namespace $DB_NS"
TSERVER_PODS="$(yb_pods tserver)" || fail "no yb-tserver pods in namespace $DB_NS"
[ -n "$MASTER_PODS" ] || fail "no yb-master pods in namespace $DB_NS"
[ -n "$TSERVER_PODS" ] || fail "no yb-tserver pods in namespace $DB_NS"

# Wait for the pods, and use YBA's task only to explain a timeout.
#
# Turning telemetry on is a rolling helm upgrade of six DB pods. Waiting for the
# universe to SETTLE is the obvious gate and is too strict: YBA's
# KubernetesConfigureExportTelemetryConfig takes ~24 minutes, well past the
# point every pod has its collector. What this asserts is the collectors, so
# that is what it waits for. The universe's in-flight action is read only when
# the wait runs out, because it separates "still rolling, give it longer" from
# "YBA is not doing this at all".
# 35 minutes. The roll took 14.7 and 17.9 minutes on two runs; the ceiling is
# generous because it is only ever paid when the roll will not finish at all.
ROLL_TRIES="${ROLL_TRIES:-140}"
UNIVERSE="$(kubectl -n "$NS" get ybuniverse -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"

in_flight() {
  [ -n "$UNIVERSE" ] || return 0
  kubectl -n "$NS" get ybuniverse "$UNIVERSE" -o jsonpath='{range .status.actions[?(@.status=="running")]}{.action_type}/running {end}{range .status.actions[?(@.status=="queued")]}{.action_type}/queued {end}' 2>/dev/null
}

missing=""
for _ in $(seq 1 "$ROLL_TRIES"); do
  missing=""
  for pod in $MASTER_PODS $TSERVER_PODS; do
    has_sidecar "$pod" || missing="$missing $pod"
  done
  [ -z "$missing" ] && break
  sleep 15
done

if [ -n "$missing" ]; then
  acts="$(in_flight)"
  if [ -n "$acts" ]; then
    fail "these pods still have no $SIDECAR:$missing — and YBA is still working ($acts), so the roll simply outlasted the wait. Raise ROLL_TRIES rather than treating this as a failure of the export."
  fi
  fail "these pods have no $SIDECAR:$missing, and YBA has no action in flight — so nothing is going to add one. Metrics export is not active: check yb.universe.otel_collector_enabled, which defaults off and is read at yugaware startup."
fi
ok "sidecar present in every pod: $(echo "$MASTER_PODS" "$TSERVER_PODS" | tr " " ",")"

# Anything below reads one tserver; the roll above proved the rest.
TSERVER_POD="$(echo "$TSERVER_PODS" | awk '{print $1}')"

# The wait above proved every pod's SPEC carries a collector, which is not the
# same as one running. A StatefulSet rolls highest ordinal first, so tserver-0 —
# the pod everything below reads — is the last recreated and the one most likely
# to still be starting. Without this, step 4 execs into a pod that has no
# container yet, finds :8888 unreadable and downgrades itself to a warning:
# the export leg would go unchecked exactly when it was newest.
kubectl -n "$DB_NS" wait --for=condition=Ready "pod/$TSERVER_POD" --timeout=5m >/dev/null 2>&1 ||
  fail "$TSERVER_POD has a $SIDECAR in its spec but never became Ready; the collector cannot be read. $(kubectl -n "$DB_NS" get pod "$TSERVER_POD" -o jsonpath='{.status.phase}: {range .status.containerStatuses[*]}{.name}={.ready} {end}' 2>/dev/null)"

# 3. What YBA actually generated.
#
# The DB chart injects nothing itself: it creates an OpenTelemetryCollector CR
# with mode: sidecar carrying YBA's rendered config verbatim, and the OTel
# Operator turns that into the container. How the config reaches the container
# varies by operator release — older ones pass OTEL_CONFIG, newer ones mount it
# — so try the pod first, where it is the strongest evidence, then the CR. The
# CR is one hop earlier than the running collector, which is acceptable only
# because step 4 proves that collector really scrapes and ships.
CONFIG="$(kubectl -n "$DB_NS" get pod "$TSERVER_POD" \
  -o jsonpath="{.spec.containers[?(@.name==\"$SIDECAR\")].env[?(@.name==\"OTEL_CONFIG\")].value}" 2>/dev/null)"
CONFIG_SRC="the $SIDECAR's OTEL_CONFIG in $TSERVER_POD"
if [ -z "$CONFIG" ]; then
  # Several collectors exist — the tserver's and, because metrics export turns it
  # on, the master's. Pick by content rather than by name: the name is derived
  # from a helm fullname this script has no way to predict.
  for c in $(kubectl -n "$DB_NS" get opentelemetrycollectors.opentelemetry.io \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    body="$(kubectl -n "$DB_NS" get opentelemetrycollector "$c" -o jsonpath='{.spec.config}' 2>/dev/null)"
    case "$body" in
    *tserver_export*)
      CONFIG="$body"
      CONFIG_SRC="OpenTelemetryCollector/$c"
      break
      ;;
    esac
  done
fi
[ -n "$CONFIG" ] || fail "cannot read the collector config for $TSERVER_POD: the $SIDECAR carries no OTEL_CONFIG and no OpenTelemetryCollector in $DB_NS has a tserver config. Without it there is no way to tell what the collector was told to collect."

want_in_config() {
  printf '%s\n' "$CONFIG" | grep -q -- "$1" ||
    fail "the collector config ($CONFIG_SRC) does not mention '$1' ($2)"
}
want_in_config "prometheus" "no prometheus receiver: nothing is being scraped"
# Two, not three: this config came from the tserver pod, and master runs its own
# sidecar with its own.
want_in_config "tserver_export" "the tserver target is missing"
want_in_config "ysql_export" "the YSQL target is missing"
# The parameters the collection level turns into. MINIMAL sends both a metrics
# allowlist and a priority_regex; a level whose parameters are dropped (TABLE_OFF
# is one) reaches the database as no filter at all and collects everything.
want_in_config "__param_priority_regex" "no priority_regex parameter: the collection level was dropped, so this is collecting every per-table series"
want_in_config "__param_metrics" "no metrics parameter: the server-level allowlist was dropped"
# CQL is deliberately not exported.
if printf '%s\n' "$CONFIG" | grep -q -- "cql_export"; then
  fail "the generated config scrapes YCQL, which amp does not use — check scrapeConfigTargets"
fi
ok "collector config ($CONFIG_SRC) scrapes tserver + YSQL with the collection level applied"

printf '%s\n' "$CONFIG" | grep -q -- "environment_id" ||
  fail "the generated config carries no environment_id tag; the series would reach the store unattributable"
ok "generated config stamps the environment identity"

# 4. Producing AND shipping. Both counters, because they fail differently: a
#    receiver at zero means nothing is being scraped, while an exporter at zero
#    with a non-zero receiver means everything is being dropped at the door.
#
# Read over port-forward, which depends on nothing inside any container: the
# collector image is distroless and the database containers have no HTTP client,
# so no exec can reach this endpoint.
#
# The PORT is YBA's choice, not a constant. On Kubernetes it binds internal
# telemetry to communicationPorts.otelCollectorMetricsPort, default 8889
# (OtelCollectorConfigGenerator.K8S_OTEL_METRICS_PORT_DEFAULT); 8888 is only the
# upstream collector default and is refused here. A universe can override the
# port, so take it from the config just read — YBA emits it as a
# declarative-config reader on 0.0.0.0 — and fall back to both known defaults.
telemetry_ports() {
  printf '%s\n' "$CONFIG" | tr ',{}' '\n' | awk '
    /host:[[:space:]]*"?0\.0\.0\.0"?/ { seen = 1; next }
    seen && /port:/ { gsub(/[^0-9]/, "", $0); if (length($0)) { print; exit } }'
  echo 8889
  echo 8888
}

PORTS=""
for p in $(telemetry_ports); do
  case " $PORTS " in *" $p "*) continue ;; esac
  PORTS="${PORTS:+$PORTS }$p"
done

PF_LOG="$(mktemp)"
PF_PID=""
close_tunnel() {
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
  PF_PID=""
}
cleanup() {
  close_tunnel
  rm -f "$PF_LOG"
}
trap cleanup EXIT

open_tunnel() {
  : >"$PF_LOG"
  kubectl -n "$DB_NS" port-forward "pod/$TSERVER_POD" ":$1" >"$PF_LOG" 2>&1 &
  PF_PID=$!
  PF_PORT=""
  for _ in $(seq 1 20); do
    PF_PORT="$(sed -nE 's#^Forwarding from 127\.0\.0\.1:([0-9]+).*#\1#p' "$PF_LOG" | head -1)"
    [ -n "$PF_PORT" ] && return 0
    kill -0 "$PF_PID" 2>/dev/null || return 1
    sleep 1
  done
  return 1
}

# A sum of no lines and a sum of zero are the same number, so emptiness is
# carried separately: awk prints nothing at all when it saw no metrics page.
sum_metric() {
  awk -v pat="$2" '$0 ~ pat {s += $NF; seen = 1} END {if (seen) printf "%d", s}' <<<"$1"
}

# Polled, because the collector may have only just come up and the jobs are on a
# 30s clock. The port hunt is inside the poll: a collector that is still
# starting refuses every candidate, which must not be read as the wrong port.
METRICS=""
TELEMETRY_PORT=""
for _ in $(seq 1 12); do
  for p in $PORTS; do
    if open_tunnel "$p"; then
      METRICS="$(curl -sf --max-time 10 "http://127.0.0.1:$PF_PORT/metrics" 2>/dev/null)"
      if [ -n "$METRICS" ]; then
        TELEMETRY_PORT="$p"
        break
      fi
    fi
    close_tunnel
  done
  if [ -n "$METRICS" ]; then
    [ -n "$(sum_metric "$METRICS" "^otelcol_exporter_sent_metric_points")" ] && break
  fi
  close_tunnel
  sleep 10
done

if [ -z "$METRICS" ]; then
  # A failure, not a warning: shipping is the point of meko#1405 and this is the
  # only step that proves it, so a run that cannot read the counters has
  # verified nothing that matters.
  kubectl -n "$DB_NS" logs "$TSERVER_POD" -c "$SIDECAR" --tail=15 >&2 2>/dev/null || true
  fail "the collector in $TSERVER_POD serves nothing on any of [$PORTS] over port-forward, so whether anything is being shipped is unknown. The last attempt said: $(tr '\n' ' ' <"$PF_LOG"). A refused connection on every candidate means the internal telemetry reader is somewhere else — read service.telemetry.metrics.readers in the config above."
else
  accepted="$(sum_metric "$METRICS" "^otelcol_receiver_accepted_metric_points")"
  sent="$(sum_metric "$METRICS" "^otelcol_exporter_sent_metric_points")"
  failed="$(sum_metric "$METRICS" "^otelcol_exporter_send_failed_metric_points")"
  if [ -z "$accepted" ] || [ "$accepted" -eq 0 ] 2>/dev/null; then
    fail "the collector has accepted 0 datapoints: it is running but scraping nothing"
  fi
  if [ -z "$sent" ] || [ "$sent" -eq 0 ] 2>/dev/null; then
    fail "the collector accepted $accepted datapoints and successfully sent none${failed:+ ($failed sends failed)}. Scraping works and the export leg does not — the door refuses an uncompressed payload with 415, so check otlp.compression on the TelemetryProvider."
  fi
  ok "collector accepted $accepted datapoints and sent $sent${failed:+ ($failed failed)} (:$TELEMETRY_PORT)"
fi

echo "PASS: YugabyteDB metrics export is configured and running"
