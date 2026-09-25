#!/usr/bin/env bash
# Run upstream documentdb's functional-test gate against an amp documentdb
# endpoint's gateway — the `/ci amp_deploy_gke_documentdb` suite.
#
# Upstream gates its own PRs on an allowlist of the functional tests
# (github.com/documentdb/functional-tests) that pass reliably on
# documentdb-local. This runs that same gate, with upstream's own tooling, at
# the release amp ships (the documentdb tarball's documentdb-local/
# functional-tests/), against the gateway a customer would reach:
#
#   1. create an engine:"documentdb" cluster and an endpoint through the amp API,
#      and wait for its gateway: compute_ctl creates the extension in a
#      `documentdb` database, and the gateway binds only once it exists;
#   2. build a ConfigMap from the release's allowlist, image pin and gate tool,
#      plus amp's exclusions.yml and the shard.sh / amp_gate.py beside this file;
#   3. run SHARDS pods of upstream's functional-tests image in the namespace.
#      Each runs one round-robin slice through shard.sh against the endpoint's
#      documentdb_address — the in-cluster Service, so no port-forward carries
#      the load — logged in as the default user over SCRAM-SHA-256;
#   4. merge the shard reports and enforce the (amp-filtered) allowlist over the
#      whole set with upstream's summarize-gate, exactly as its combine job does.
#
# Exit status is the gate's: 0 only if every gated test passed. Results
# (merged report, gate-summary.md/json, each shard's logs) land in RESULTS_DIR.
#
# Prereqs: kubectl pointed at the cluster, curl, jq, python3 (PyYAML is
# installed if missing); a controller deployed in $NS with the documentdb
# extension and gateway images pushed (amp_deploy_gke_extensions does both).
#
# Usage: NS=<namespace> deploy/documentdb-functional/run.sh
#   SHARDS (4), WORKERS per shard (2), TIMEOUT_MIN (100), KEEP=1 to leave it all up.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NS="${NS:-amp-test}"
PF_PORT="${PF_PORT:-18081}"
SHARDS="${SHARDS:-4}"
WORKERS="${WORKERS:-2}"
TIMEOUT_MIN="${TIMEOUT_MIN:-100}"
KEEP="${KEEP:-0}"
EP_PASSWORD="${EP_PASSWORD:-functional-pw-1}"
RESULTS_DIR="${RESULTS_DIR:-$PWD/documentdb-functional-results}"
PSQL_BIN="${PSQL_BIN:-/usr/local/v18/bin/psql}"
# The same pool the smoke reuses (smoke-test.sh explains why it is one fixed row).
POOL_NAME="pool-smoke"
CLUSTER_NAME="docfunc-$(date +%s)"

# The documentdb release amp builds (deploy/extensions/extensions.yaml) carries
# the gate it was released against. Pinned like the extension source.
DOCUMENTDB_VERSION="0.116-0"
DOCUMENTDB_URL="https://github.com/documentdb/documentdb/archive/refs/tags/v${DOCUMENTDB_VERSION}.tar.gz"
DOCUMENTDB_SHA256="09bff8c573669f4281c5ad9352946a03e91f5c697cb9d537d9e76330d63f1a4d"

BASE="http://127.0.0.1:${PF_PORT}/api/v1"
work=$(mktemp -d)
PF_PID=""
PROJ=""

log() { printf '>> %s\n' "$*"; }
die() { printf '::error::%s\n' "$*" >&2; exit 1; }

cleanup() {
  if [ "$KEEP" != 1 ]; then
    kubectl -n "$NS" delete pod -l app=documentdb-functional --wait=false >/dev/null 2>&1 || true
    kubectl -n "$NS" delete configmap documentdb-functional-gate >/dev/null 2>&1 || true
    if [ -n "$PROJ" ] && [ -n "$PF_PID" ]; then
      curl -s -o /dev/null -X DELETE "$BASE/storage-pools/$POOL/clusters/$PROJ" || true
    fi
  fi
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
  rm -rf "$work"
}
trap cleanup EXIT

# --- 0. the release's gate, verified ---
log "fetching documentdb v$DOCUMENTDB_VERSION (functional-test gate)"
curl -fsSL --retry 3 -o "$work/src.tgz" "$DOCUMENTDB_URL" || die "download failed: $DOCUMENTDB_URL"
echo "$DOCUMENTDB_SHA256  $work/src.tgz" | sha256sum -c - >/dev/null || die "checksum mismatch for $DOCUMENTDB_URL"
mkdir -p "$work/src"
tar -xzf "$work/src.tgz" -C "$work/src" --strip-components=1 \
  --wildcards '*/documentdb-local/functional-tests/config/*' '*/documentdb-local/functional-tests/tools/*' \
  || die "the v$DOCUMENTDB_VERSION tarball has no documentdb-local/functional-tests"
FT="$work/src/documentdb-local/functional-tests"
python3 -c 'import yaml' 2>/dev/null || python3 -m pip install --quiet --user pyyaml || die "PyYAML unavailable"
IMAGE=$(python3 -c 'import sys, yaml; print(yaml.safe_load(open(sys.argv[1]))["image"])' "$FT/config/image.yml")
[[ "$IMAGE" == *@sha256:* ]] || die "functional-tests image is not pinned by digest: $IMAGE"
log "functional-tests image: $IMAGE"

# The same filter the shards apply, so the gate below judges what they ran.
python3 "$here/amp_gate.py" "$FT/config/allowlist.yml" "$here/exclusions.yml" documentdb "$work/allowlist.amp.yml" \
  || die "exclusions.yml does not apply to this release's allowlist"

# --- 1. a documentdb endpoint ---
kubectl -n "$NS" port-forward svc/amp-controller "${PF_PORT}:8080" >"$work/pf.log" 2>&1 &
PF_PID=$!
for ((i=0; i<30; i++)); do curl -sf "http://127.0.0.1:${PF_PORT}/healthz" >/dev/null 2>&1 && break; sleep 1; done

# task <METHOD> <path> [body] -> waits for the task; echoes its final status.
task() {
  local tid st
  tid=$(curl -s -X "$1" "$BASE$2" -H 'Content-Type: application/json' ${3:+-d "$3"} | jq -r '.id // empty')
  [ -n "$tid" ] || { echo "no-task"; return 1; }
  for ((i=0; i<300; i++)); do
    st=$(curl -sf "$BASE/tasks/$tid" | jq -r '.status' 2>/dev/null)
    case "$st" in succeeded|failed) echo "$st"; [ "$st" = succeeded ]; return; esac
    sleep 2
  done
  echo timeout; return 1
}

USER_ID=$(curl -s -X POST "$BASE/users" -H 'Content-Type: application/json' -d '{}' | jq -r '.id // empty')
POOL=$(curl -s -X POST "$BASE/storage-pools" -H 'Content-Type: application/json' \
         -d "{\"name\":\"$POOL_NAME\",\"owner_user_id\":\"$USER_ID\"}" | jq -r '.id // empty')
[ -n "$POOL" ] || POOL=$(curl -sf "$BASE/storage-pools" | jq -r --arg n "$POOL_NAME" '.storage_pools[]|select(.name==$n)|.id' | head -1)
[ -n "$POOL" ] || die "no storage pool $POOL_NAME"

st=$(task POST "/storage-pools/$POOL/clusters" "{\"name\":\"$CLUSTER_NAME\",\"engine\":\"documentdb\"}") || die "createCluster(documentdb) task=$st"
PROJ=$(curl -sf "$BASE/storage-pools/$POOL/clusters" | jq -r --arg n "$CLUSTER_NAME" '.clusters[]|select(.name==$n)|.id')
DEF=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$PROJ" | jq -r '.default_branch.branch_id')
st=$(task POST "/storage-pools/$POOL/clusters/$PROJ/branches/$DEF/endpoints" \
       "{\"role\":\"read_write\",\"password\":\"$EP_PASSWORD\"}") || die "createEndpoint task=$st"
EP=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$PROJ/branches/$DEF/endpoints" | jq -r '.endpoints[0].id')
ADDR=$(curl -sf "$BASE/storage-pools/$POOL/clusters/$PROJ/branches/$DEF/endpoints/$EP" | jq -r '.documentdb_address // ""')
[ -n "$ADDR" ] || die "endpoint $EP reports no documentdb_address (needs a ClusterIP Service)"
log "endpoint $EP, gateway at $ADDR"

POD=""
for ((i=0; i<60; i++)); do
  POD=$(kubectl -n "$NS" get pods -o name 2>/dev/null | grep "yb-ep-$EP" | head -1)
  [ -n "$POD" ] && [ "$(kubectl -n "$NS" get "$POD" -o jsonpath='{.status.containerStatuses[?(@.name=="documentdb-gateway")].ready}' 2>/dev/null)" = true ] \
    && [ "$(kubectl -n "$NS" get "$POD" -o jsonpath='{.status.containerStatuses[?(@.name=="endpoint")].ready}' 2>/dev/null)" = true ] && break
  sleep 3
done
[ -n "$POD" ] || die "endpoint pod for $EP not found"
ver=$(kubectl -n "$NS" exec -i "$POD" -c endpoint -- "$PSQL_BIN" -h 127.0.0.1 -p 55433 -U cloud_admin -d documentdb -tA \
  -c "SELECT extversion FROM pg_extension WHERE extname = 'documentdb'" 2>&1)
[ -n "$ver" ] || die "documentdb is not created in database documentdb on $POD ($ver)"
log "documentdb $ver in database documentdb"

# --- 2. the gate, as a ConfigMap ---
gzip -c "$FT/config/allowlist.yml" > "$work/allowlist.yml.gz"
kubectl -n "$NS" create configmap documentdb-functional-gate \
  --from-file=allowlist.yml.gz="$work/allowlist.yml.gz" \
  --from-file=image.yml="$FT/config/image.yml" \
  --from-file=functional_gate.py="$FT/tools/functional_gate.py" \
  --from-file=exclusions.yml="$here/exclusions.yml" \
  --from-file=amp_gate.py="$here/amp_gate.py" \
  --from-file=shard.sh="$here/shard.sh" \
  --dry-run=client -o yaml | kubectl -n "$NS" apply -f - >/dev/null || die "gate ConfigMap"

# --- 3. the shards ---
# The gateway self-signs until https://github.com/yugabyte/meko/issues/1509.
URI="mongodb://cloud_admin:${EP_PASSWORD}@${ADDR}/?tls=true&tlsAllowInvalidCertificates=true&authMechanism=SCRAM-SHA-256"
for ((s=0; s<SHARDS; s++)); do
  kubectl -n "$NS" apply -f - >/dev/null <<YAML || die "shard pod $s"
apiVersion: v1
kind: Pod
metadata:
  name: documentdb-functional-$s
  labels: {app: documentdb-functional}
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    # The image names its user (testrunner) rather than a uid, and the kubelet
    # cannot check a name against runAsNonRoot: without a numeric uid every
    # container fails CreateContainerConfigError and the pod sits Pending.
    runAsUser: 1000
    runAsGroup: 1000
    seccompProfile: {type: RuntimeDefault}
  containers:
    - name: tests
      image: $IMAGE
      command: ["sh", "/gate/shard.sh"]
      env:
        - {name: URI, value: "$URI"}
        - {name: SHARD, value: "$s"}
        - {name: NUM_SHARDS, value: "$SHARDS"}
        - {name: WORKERS, value: "$WORKERS"}
      resources:
        requests: {cpu: "1", memory: 1Gi}
        limits: {memory: 2Gi}
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: {drop: [ALL]}
      volumeMounts:
        - {name: gate, mountPath: /gate, readOnly: true}
        - {name: results, mountPath: /results}
  volumes:
    - name: gate
      configMap: {name: documentdb-functional-gate}
    - name: results
      emptyDir: {}
YAML
done
log "$SHARDS shard(s) x $WORKERS worker(s) started; waiting up to $TIMEOUT_MIN min"

deadline=$(( $(date +%s) + TIMEOUT_MIN * 60 ))
mkdir -p "$RESULTS_DIR"
pending=$SHARDS
declare -A fetched=()

# pytest logs one line per test in the image's -vv format,
# "[gw1] [ 42%] PASSED <node id>", so the log alone says how far a shard has
# got and how it is going, without waiting for its report.
shard_progress() {
  kubectl -n "$NS" exec "documentdb-functional-$1" -- sh -c '
    l=/results/pytest.log; [ -f "$l" ] || { echo "not started"; exit 0; }
    sed "s/\x1b\[[0-9;]*m//g" "$l" | awk "
      /\\[ *[0-9]+%\\] (PASSED|FAILED|ERROR|SKIPPED|XFAIL|XPASS) / {
        n++; if (match(\$0, /\\[ *[0-9]+%\\]/)) pct = substr(\$0, RSTART + 1, RLENGTH - 2)
        for (k = 1; k <= NF; k++) if (\$k ~ /^(PASSED|FAILED|ERROR|SKIPPED|XFAIL|XPASS)\$/) { c[\$k]++; break }
      }
      END { gsub(/ /, \"\", pct); printf \"%s done: %d ran, %d passed, %d failed, %d errors, %d skipped\", (pct == \"\" ? \"0%\" : pct), n, c[\"PASSED\"], c[\"FAILED\"], c[\"ERROR\"], c[\"SKIPPED\"] }"
  ' 2>/dev/null
}
# Under the parallel pass every shard can lose its connection at once, and the
# sequential re-run exists to absorb that. Either the gateway sidecar restarted
# (seen on amp: restartCount 1, the compute untouched), or a Postgres backend
# crashed and the postmaster restarted them all (upstream documents a RUM
# dynamic-cursor race). Record which, and why, since the pod is gone afterwards.
endpoint_health() {
  log "endpoint restarts: $(kubectl -n "$NS" get "$POD" -o jsonpath='{range .status.containerStatuses[*]}{.name}={.restartCount} {end}' 2>/dev/null)"
  last=$(kubectl -n "$NS" get "$POD" -o jsonpath='{range .status.containerStatuses[?(@.restartCount>0)]}{.name}: {.lastState.terminated.reason} (exit {.lastState.terminated.exitCode}) {end}' 2>/dev/null)
  [ -n "$last" ] && log "last termination: $last"
  crashes=$(kubectl -n "$NS" logs "$POD" -c endpoint 2>/dev/null \
    | grep -E 'was terminated by signal|terminating any other active server processes|all server processes terminated' | head -5)
  if [ -n "$crashes" ]; then log "compute backend crash(es) during the run:"; printf '%s\n' "$crashes"; else log "no compute backend crash in the endpoint log"; fi
}
# On a timeout, keep what each unfinished shard has so far: its pytest log says
# which tests failed, and a partial result is what the next run is sized from.
collect_partial() {
  for ((s=0; s<SHARDS; s++)); do
    [ -n "${fetched[$s]:-}" ] && continue
    mkdir -p "$RESULTS_DIR/shard-$s-partial"
    kubectl -n "$NS" exec "documentdb-functional-$s" -- tar cf - -C /results . 2>/dev/null \
      | tar xf - -C "$RESULTS_DIR/shard-$s-partial" 2>/dev/null
    log "shard $s at timeout: $(shard_progress "$s")"
  done
}
last_progress=$(date +%s)
while (( pending > 0 )); do
  if (( $(date +%s) >= deadline )); then
    endpoint_health
    collect_partial
    die "timed out with $pending shard(s) still running"
  fi
  if (( $(date +%s) - last_progress >= 300 )); then
    for ((s=0; s<SHARDS; s++)); do
      [ -n "${fetched[$s]:-}" ] || log "shard $s: $(shard_progress "$s" || echo "no progress yet (pod not running)")"
    done
    last_progress=$(date +%s)
  fi
  for ((s=0; s<SHARDS; s++)); do
    [ -n "${fetched[$s]:-}" ] && continue
    phase=$(kubectl -n "$NS" get pod "documentdb-functional-$s" -o jsonpath='{.status.phase}' 2>/dev/null)
    case "$phase" in Failed|Succeeded) kubectl -n "$NS" logs "documentdb-functional-$s" --tail=50 >&2; die "shard $s exited ($phase) before finishing";; esac
    # A container that cannot start leaves the pod Pending forever, which would
    # otherwise look exactly like a slow shard until the timeout.
    waiting=$(kubectl -n "$NS" get pod "documentdb-functional-$s" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null)
    case "$waiting" in
      CreateContainerConfigError|CreateContainerError|ErrImagePull|ImagePullBackOff|InvalidImageName|CrashLoopBackOff)
        kubectl -n "$NS" describe pod "documentdb-functional-$s" | tail -15 >&2
        die "shard $s cannot start: $waiting";;
    esac
    kubectl -n "$NS" exec "documentdb-functional-$s" -- test -f /results/done 2>/dev/null || continue
    mkdir -p "$RESULTS_DIR/shard-$s"
    kubectl -n "$NS" exec "documentdb-functional-$s" -- tar cf - -C /results . | tar xf - -C "$RESULTS_DIR/shard-$s"
    kubectl -n "$NS" logs "documentdb-functional-$s" > "$RESULTS_DIR/shard-$s/pod.log" 2>&1
    rc=$(cat "$RESULTS_DIR/shard-$s/shard.rc" 2>/dev/null)
    log "shard $s done (rc=${rc:-?}): $(grep -E 'passed|failed' "$RESULTS_DIR/shard-$s/pytest.log" 2>/dev/null | tail -1)"
    [ "$rc" = 0 ] || die "shard $s could not produce a report (rc=${rc:-?}); see $RESULTS_DIR/shard-$s"
    fetched[$s]=1; pending=$((pending - 1))
  done
  sleep 20
done

endpoint_health

# --- 4. the gate, over every shard ---
GATE=(python3 "$work/src/documentdb-local/functional-tests/tools/functional_gate.py"
      --image "$FT/config/image.yml" --allowlist "$work/allowlist.amp.yml" --engine-name documentdb)
reports=()
for ((s=0; s<SHARDS; s++)); do reports+=("$RESULTS_DIR/shard-$s/report_shard.json"); done
if (( SHARDS > 1 )); then
  "${GATE[@]}" merge-reports --base "${reports[0]}" --overlay "${reports[@]:1}" \
    --out "$RESULTS_DIR/report.json" --sum-collected || die "merging shard reports"
else
  cp "${reports[0]}" "$RESULTS_DIR/report.json"
fi
"${GATE[@]}" summarize-gate --report "$RESULTS_DIR/report.json" --output-dir "$RESULTS_DIR"
gate_rc=$?
# summarize-gate prints the summary itself; the job page gets a copy.
if [ -f "$RESULTS_DIR/gate-summary.md" ] && [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  cat "$RESULTS_DIR/gate-summary.md" >> "$GITHUB_STEP_SUMMARY"
fi
exit "$gate_rc"
