#!/usr/bin/env bash
#
# Print the name of a tserver pod in UNIVERSE_NS, for the scripts that run
# ysqlsh by `kubectl exec`.
#
# The authoritative answer to "which pods are tservers" is the SELECTOR on the
# Service the universe publishes, not a label guessed here. Which label key the
# chart writes depends on YBA's oldNamingStyle, which is YBA's choice and not
# ours, and a namespace holding two universes has two sets of tserver pods that
# a hand-written selector cannot tell apart. The foundation deployer read the
# selector for exactly this reason until the database step left it.
#
# The guess survives as a FALLBACK, for the single-node `yugabyted` lane: it
# publishes no such Service, and there is only ever one universe beside it.
#
# Env:
#   UNIVERSE_NS         namespace holding the DB pods (required)
#   TSERVER_SVC_LABELS  labels selecting the published Service
set -euo pipefail

UNIVERSE_NS="${UNIVERSE_NS:?UNIVERSE_NS is required}"
TSERVER_SVC_LABELS="${TSERVER_SVC_LABELS:-serviceName=yb-tserver-conn,scope=Namespaced}"

selector=""
svc="$(kubectl -n "${UNIVERSE_NS}" get service -l "${TSERVER_SVC_LABELS}" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
if [ -n "${svc}" ]; then
  # shellcheck disable=SC2016  # go-template syntax, not shell
  selector="$(kubectl -n "${UNIVERSE_NS}" get service "${svc}" \
    -o go-template='{{range $k, $v := .spec.selector}}{{$k}}={{$v}},{{end}}' 2>/dev/null \
    | sed 's/,$//')"
fi

for sel in "${selector}" "app.kubernetes.io/name=yb-tserver" "app=yb-tserver"; do
  [ -n "${sel}" ] || continue
  pod="$(kubectl -n "${UNIVERSE_NS}" get pods -l "${sel}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [ -n "${pod}" ]; then
    echo "${pod}"
    exit 0
  fi
done

exit 1
