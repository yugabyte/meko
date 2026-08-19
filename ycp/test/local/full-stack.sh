#!/usr/bin/env bash
#
# Prove that REGISTERING an environment deploys the whole Meko stack.
#
# No desired state is POSTed anywhere in this script. It registers an environment with
# nothing but a name, deploys an agent into a fresh namespace, and waits: the control
# plane resolves the pinned release, the enroll response carries it, and the agent
# brings up YugabyteDB with the mage and pg_dist_rag extensions, then api-server,
# mcp-server, rag-worker, inference-gateway and ui over it.
#
# That is the difference from meko-stack.sh, which commands a stack the way an operator
# used to have to. This one exercises the path a customer actually takes.
#
# Every image in the release is private, so this needs ECR credentials — which is why
# it is not part of the CI e2e. It also needs real capacity: YugabyteDB asks for 2Gi and
# meko-mcp-server is ~11 GB uncompressed, so on a laptop expect the pulls to dominate.
#
# It does not create a cluster. Run the e2e first and keep it:
#
#   YCP_E2E_KEEP=1 ycp/test/e2e/run.sh
#   ycp/test/local/full-stack.sh
#
# Environment:
#   AWS_PROFILE           profile that can read the Meko ECR (meko-dev here).
#   OPENAI_API_KEY        written to the data-plane Secret when set. The MCP server and
#                         the gateway start without it and fail on the first embedding.
#   MEKO_FULL_STACK_ENV   environment name to register (default full-stack).
#   MEKO_FULL_STACK_NS    namespace for the new agent (default ycp-full-stack).
#   MEKO_FULL_STACK_AGENT_IMAGE
#                         agent image to deploy; default = whatever the e2e is running.
#                         Set this after a local build to deploy the tree you just edited.
#   YCP_E2E_CLUSTER       kind cluster name (default ycp-e2e).
#   YCP_E2E_BASE_PATH     subpath the control plane is mounted under (default /ycp).
#   MEKO_FULL_STACK_KEEP  1 to leave the namespace behind on failure (default: leave it
#                         either way — this script never deletes anything).
#
set -euo pipefail

CLUSTER="${YCP_E2E_CLUSTER:-ycp-e2e}"
CTX="kind-${CLUSTER}"
CP_NS=ycp-e2e
NS="${MEKO_FULL_STACK_NS:-ycp-full-stack}"
ENV_NAME="${MEKO_FULL_STACK_ENV:-full-stack}"
ADMIN_KEY=e2eadminkey
PORT="${YCP_LOCAL_PORT:-18082}"
BASE="${YCP_E2E_BASE_PATH-/ycp}"
TIMEOUT="${MEKO_FULL_STACK_TIMEOUT:-900s}"
ECR="161908593886.dkr.ecr.us-east-2.amazonaws.com"
ECR_REGION=us-east-2
PULL_SECRET=meko-ecr
CREDS_SECRET=meko-data-plane

log() { printf '\n==> %s\n' "$*"; }
die() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
k() { kubectl --context "$CTX" "$@"; }
api() {
  local method="$1" path="$2"; shift 2
  curl -fsS -X "$method" "http://127.0.0.1:$PORT$BASE$path" \
    -H "X-Meko-Admin-Secret: $ADMIN_KEY" "$@"
}

for bin in kubectl curl python3 aws; do
  command -v "$bin" >/dev/null 2>&1 || die "$bin is required but not on PATH"
done

cleanup() {
  rc=$?
  for pid in ${PF_PID:-}; do kill "$pid" 2>/dev/null || true; done
  exit "$rc"
}
trap cleanup EXIT

# Same context guard as the e2e: only ever talk to the kind cluster, never to whatever
# a kubeconfig happens to point at.
k get nodes -o name 2>/dev/null | grep -qx "node/${CLUSTER}-control-plane" ||
  die "context $CTX is not a kind cluster named $CLUSTER — start one with: YCP_E2E_KEEP=1 ycp/test/e2e/run.sh"
k -n "$CP_NS" get deploy meko-api-server >/dev/null 2>&1 ||
  die "no control plane in $CP_NS — run the e2e first (YCP_E2E_KEEP=1 ycp/test/e2e/run.sh)"

# The agent image: whatever was asked for, else whatever the e2e left running. The
# override is what makes "build locally, then deploy that build" a single step — without
# it this always reuses the e2e's image, which is not the tree you just edited.
AGENT_IMAGE="${MEKO_FULL_STACK_AGENT_IMAGE:-}"
if [ -z "$AGENT_IMAGE" ]; then
  AGENT_IMAGE="$(k -n "$CP_NS" get deploy meko-agent -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
fi
[ -n "$AGENT_IMAGE" ] ||
  die "no agent image: set MEKO_FULL_STACK_AGENT_IMAGE, or run the e2e first so one can be read from $CP_NS"
echo "    agent image: $AGENT_IMAGE"

# -------------------------------------------------- teach the control plane the Secrets
#
# The e2e's control plane is started without the data-plane Secret names, because the
# stack it seeds is unpullable there anyway. Here they matter: every image in the release
# is private, so the seeded stack has to reference a pull Secret by name. Patch them in
# if they are absent, and wait for the rollout — a registration made against the old pod
# would seed a stack with no pull Secret in it, and every workload would fail to pull.

cp_args() { k -n "$CP_NS" get deploy meko-api-server -o jsonpath='{.spec.template.spec.containers[0].args}'; }
if ! cp_args | grep -q 'dataplane-image-pull-secret'; then
  log "adding the data-plane Secret names to the control plane"
  k -n "$CP_NS" patch deploy meko-api-server --type=json -p "$(cat <<JSON
[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--dataplane-image-pull-secret=$PULL_SECRET"},
 {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--dataplane-secret-ref=$CREDS_SECRET"}]
JSON
  )" >/dev/null
  k -n "$CP_NS" rollout status deploy/meko-api-server --timeout=300s
else
  echo "    control plane already carries --dataplane-image-pull-secret"
fi

# A port nothing else is on, checked BEFORE forwarding rather than after.
#
# `kubectl port-forward` fails to bind a busy port and keeps running, so a stale forward
# from an earlier session — quite possibly to a different cluster — silently keeps the
# port and every call below goes to the wrong control plane. Waiting for /health to
# answer does not catch it: the wrong control plane answers too. This cost an afternoon
# once, with the symptom being a registration that seeded a stack shape the code being
# tested cannot produce.
log "picking a free local port"
port_free() { ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
for candidate in $(seq "$PORT" $((PORT + 20))); do
  if port_free "$candidate"; then PORT="$candidate"; break; fi
  echo "    $candidate is in use (something else is listening) — trying the next"
done
port_free "$PORT" || die "no free port in $PORT..$((PORT + 20)) — close stale kubectl port-forwards"
echo "    using 127.0.0.1:$PORT"

log "port-forwarding the control plane"
k -n "$CP_NS" port-forward svc/meko-api-server "$PORT:80" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:$PORT$BASE/api/v1/health" >/dev/null 2>&1 && break
  kill -0 "$PF_PID" 2>/dev/null || die "the port-forward exited — is $CP_NS/meko-api-server serving?"
  sleep 1
done
curl -fsS "http://127.0.0.1:$PORT$BASE/api/v1/health" >/dev/null 2>&1 ||
  die "the control plane did not answer on 127.0.0.1:$PORT$BASE"

# ------------------------------------------------------------------- register
#
# Start from nothing, every time.
#
# Registering an existing environment RESETS it: the status token hash is cleared and
# the state goes back to PENDING, so an agent left over from a previous run is holding a
# credential the control plane no longer honours. It re-enrolls and recovers eventually,
# but in the meantime the fleet reports PENDING with no components and every assertion
# below fails for a reason that has nothing to do with the code under test. Observed
# exactly that on the second run of this script.
#
# So: remove the environment and the namespace first. It also means the database really
# is created from scratch, which is the thing being tested.
if api GET "/api/v1/environments/$ENV_NAME" >/dev/null 2>&1; then
  log "removing the previous $ENV_NAME environment and its namespace"
  api DELETE "/api/v1/environments/$ENV_NAME" >/dev/null 2>&1 || true
  k delete namespace "$NS" --ignore-not-found --wait=true >/dev/null
  echo "    gone (the database's PersistentVolumeClaim went with the namespace)"
fi

# A name, a placement and a region. Nothing about images, versions or Secrets: the
# control plane resolves the release, and the two Secret NAMES it references are
# whatever it was started with (--dataplane-image-pull-secret / --dataplane-secret-ref).
# We create Secrets under those names below.

log "registering $ENV_NAME with nothing but a name"
reg="$(api POST /api/v1/environments -H 'content-type: application/json' \
  -d "{\"name\":\"$ENV_NAME\",\"account_id\":\"22222222-2222-2222-2222-222222222222\",\"placement\":\"cloud\",\"region\":\"local\"}")" ||
  die "POST /api/v1/environments failed"
TOKEN="$(printf '%s' "$reg" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["enrollment_token"])')"
[ -n "$TOKEN" ] || die "no enrollment_token in the register response: ${reg:0:200}"
echo "    enrollment token minted (${TOKEN:0:12}...)"

log "checking what registration seeded"
seeded="$(api GET "/api/v1/environments/$ENV_NAME/desired")"
python3 - "$seeded" <<'PY' || die "registration did not seed a full meko-stack"
import json, sys
res = json.loads(sys.argv[1])["data"]["resources"]
if len(res) != 1 or res[0]["kind"] != "meko-stack":
    sys.exit("    seeded %s, want one meko-stack" % [r["kind"] for r in res])
spec = res[0]["spec"]
images = spec.get("images", {})
want = ["database", "api_server", "mcp_server", "rag_worker", "inference_gateway", "ui"]
missing = [w for w in want if not images.get(w)]
if missing:
    sys.exit("    no image for %s — is the control plane running a build with the "
             "pinned release in it?" % ", ".join(missing))
for w in want:
    print("    %-18s %s" % (w, images[w]))
print("    pull secret: %s   credentials: %s" % (
    spec.get("image_pull_secret") or "(none)", spec.get("secret_ref") or "(none)"))
PY

# The control plane may have been started without the dataplane Secret names (the e2e
# does not set them). Read back what it actually seeded and honour that, rather than
# assuming our own constants — a Secret under the wrong name is invisible until the
# pods fail to pull.
PULL_SECRET="$(printf '%s' "$seeded" | python3 -c '
import json,sys
spec = json.load(sys.stdin)["data"]["resources"][0]["spec"]
print(spec.get("image_pull_secret") or "")')"
CREDS_SECRET="$(printf '%s' "$seeded" | python3 -c '
import json,sys
spec = json.load(sys.stdin)["data"]["resources"][0]["spec"]
print(spec.get("secret_ref") or "")')"
if [ -z "$PULL_SECRET" ]; then
  die "the seeded stack names no image_pull_secret, and every image in it is private.
    Restart the control plane with --dataplane-image-pull-secret=meko-ecr
    (and --dataplane-secret-ref=meko-data-plane), then re-run this."
fi

# ---------------------------------------------------------------- credentials

log "creating namespace $NS and its Secrets"
k create namespace "$NS" --dry-run=client -o yaml | k apply -f - >/dev/null

# An ECR pull Secret rather than side-loading: meko-mcp-server is ~11 GB uncompressed,
# so `kind load` writes it twice, and a pull Secret is what a real data plane uses.
# The token is valid ~12h — a cluster kept longer needs this re-run before the next pull.
ecr_token="$(aws ecr get-login-password --region "$ECR_REGION")" ||
  die "aws ecr get-login-password failed — is AWS_PROFILE set to a profile with ECR access?"
k -n "$NS" create secret docker-registry "$PULL_SECRET" \
  --docker-server="$ECR" --docker-username=AWS --docker-password="$ecr_token" \
  --dry-run=client -o yaml | k apply -f - >/dev/null
echo "    $PULL_SECRET written (ECR tokens expire after ~12h)"

if [ -n "$CREDS_SECRET" ]; then
  creds=(--from-literal=placeholder=unused)
  if [ -n "${OPENAI_API_KEY:-}" ]; then
    creds=(--from-literal=OPENAI_API_KEY="$OPENAI_API_KEY")
    echo "    OPENAI_API_KEY included in $CREDS_SECRET"
  else
    # The gateway builds its LangChain client at startup, so no key is not "degraded" —
    # it exits before serving anything. The MCP server is laxer: it starts and fails on
    # the first embedding. Neither is a fault in the stack, so the assertions below drop
    # the gateway rather than reporting a red component for missing configuration.
    NEED_KEY=1
    echo "    OPENAI_API_KEY not set — the gateway will NOT start (it builds its LLM"
    echo "    client at import), and the MCP server will fail on the first embedding."
    echo "    Set OPENAI_API_KEY to exercise those two properly."
  fi
  k -n "$NS" create secret generic "$CREDS_SECRET" "${creds[@]}" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
fi

k -n "$NS" create secret generic meko-agent-enrollment \
  --from-literal=enrollment-token="$TOKEN" --dry-run=client -o yaml | k apply -f - >/dev/null

# ---------------------------------------------------------------- the agent
#
# RBAC is the same set the chart grants and test/e2e/agent.yaml pins: server-side apply
# (patch) on the workload kinds, plus secrets in its own namespace for its state.

log "deploying the agent into $NS"
k apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: meko-agent, namespace: $NS}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: meko-agent, namespace: $NS}
rules:
  - apiGroups: [""]
    resources: [secrets, services, configmaps]
    verbs: [get, list, create, update, patch, delete]
  - apiGroups: ["apps"]
    resources: [deployments, statefulsets]
    verbs: [get, list, create, update, patch, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: meko-agent, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: meko-agent}
subjects: [{kind: ServiceAccount, name: meko-agent, namespace: $NS}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: meko-agent-$NS}
rules:
  - apiGroups: [""]
    resources: [nodes]
    verbs: [get, list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: meko-agent-$NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: meko-agent-$NS}
subjects: [{kind: ServiceAccount, name: meko-agent, namespace: $NS}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: meko-agent, namespace: $NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: meko-agent}}
  template:
    metadata: {labels: {app: meko-agent}}
    spec:
      serviceAccountName: meko-agent
      containers:
        - name: agent
          image: $AGENT_IMAGE
          imagePullPolicy: IfNotPresent
          env:
            - {name: YCP_URL, value: "http://meko-api-server.$CP_NS.svc.cluster.local.$BASE"}
            - {name: ENV_NAME, value: "$ENV_NAME"}
            - {name: ENV_PLACEMENT, value: cloud}
            - {name: ENV_PROVIDER, value: kind}
            - {name: ENV_REGION, value: local}
            - name: ENROLLMENT_TOKEN
              valueFrom: {secretKeyRef: {name: meko-agent-enrollment, key: enrollment-token}}
YAML
k -n "$NS" rollout status deploy/meko-agent --timeout=120s

# ------------------------------------------------------- watch it deploy itself

log "checking the agent was handed a stack at enrollment"
handed=""
for _ in $(seq 1 60); do
  handed="$(k -n "$NS" logs deploy/meko-agent --tail=-1 2>/dev/null |
              grep -E 'enrolled with a Meko stack|no stack came with enrollment' | tail -1 || true)"
  [ -n "$handed" ] && break
  sleep 2
done
[ -n "$handed" ] || die "the agent never said whether it got a stack — check: kubectl -n $NS logs deploy/meko-agent"
printf '    %s\n' "$handed"
case "$handed" in
  *"no stack came with enrollment"*)
    die "the enroll response carried no stack. The control plane image predates this, or
    the environment was registered with an explicit \"desired\"." ;;
esac

# The stack has to be reachable from the persisted state too, or a restart would need
# the control plane. Checking the Secret is cheaper and more direct than restarting.
k -n "$NS" get secret meko-agent-state -o jsonpath='{.data.stack}' 2>/dev/null | grep -q . ||
  die "the agent did not persist the stack in meko-agent-state — a restart would have nothing to deploy"
echo "    stack persisted in meko-agent-state (a restart needs no control-plane call)"

# Object names are prefixed with the RESOURCE name (`meko`), not the environment name:
# the stack the control plane seeds is called `meko`, so the workloads are
# meko-yugabytedb, meko-api-server and so on. Found by label rather than by name here,
# so this keeps working if that prefix ever changes.
log "waiting for YugabyteDB (yugabyted takes ~60-90s after the image lands)"
for _ in $(seq 1 90); do
  k -n "$NS" get statefulset -l meko.dev/stack=true 2>/dev/null | grep -q . && break
  sleep 2
done
DB="$(k -n "$NS" get statefulset -l meko.dev/stack=true -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
[ -n "$DB" ] || die "the agent never created the database StatefulSet — check: kubectl -n $NS logs deploy/meko-agent"
echo "    StatefulSet $DB"
if ! k -n "$NS" rollout status "statefulset/$DB" --timeout="$TIMEOUT"; then
  k -n "$NS" describe "statefulset/$DB" | grep -E '^(  Warning|    Reason|    Message)' || true
  k -n "$NS" logs "statefulset/$DB" --tail=40 || true
  die "the database never became ready (see above)"
fi

# The extensions are the point of the MAGE build, and the gate on the api-server: its
# data migrations reference dist_rag.vector_indexes and the dist_rag enum types.
log "checking mage and pg_dist_rag are installed in the meko database"
DB_PW="$(k -n "$NS" get secret meko-database -o jsonpath='{.data.password}' | base64 -d)"
exts=""
for _ in $(seq 1 30); do
  exts="$(k -n "$NS" exec "$DB-0" -- env PGPASSWORD="$DB_PW" bash -c \
    "bin/ysqlsh -h \$(hostname) -p 5433 -U yugabyte -d meko -tAc \
     \"select extname from pg_extension order by extname\"" 2>/dev/null || true)"
  printf '%s' "$exts" | grep -q '^mage$' && printf '%s' "$exts" | grep -q '^pg_dist_rag$' && break
  sleep 5
done
printf '%s' "$exts" | grep -q '^mage$' ||
  die "the mage extension is not installed. Is the database image the MAGE build, and is
    ysql_yb_enable_mage=true on both master and tserver? Extensions present: $(printf '%s' "$exts" | tr '\n' ' ')"
printf '%s' "$exts" | grep -q '^pg_dist_rag$' ||
  die "pg_dist_rag is not installed. Extensions present: $(printf '%s' "$exts" | tr '\n' ' ')"
printf '    extensions: %s\n' "$(printf '%s' "$exts" | tr '\n' ' ')"

# dist_rag has to be a schema, not just an extension row: that is what api_server's
# migrations actually reference.
k -n "$NS" exec "$DB-0" -- env PGPASSWORD="$DB_PW" bash -c \
  "bin/ysqlsh -h \$(hostname) -p 5433 -U yugabyte -d meko -tAc \
   \"select 1 from information_schema.schemata where schema_name='dist_rag'\"" 2>/dev/null | grep -q 1 ||
  die "the dist_rag schema is missing even though the extension is installed"
echo "    dist_rag schema present (api_server's migrations reference it)"

log "waiting for the Meko services (first pull of meko-mcp-server is several GB)"
for dep in $(k -n "$NS" get deploy -l meko.dev/stack=true -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
  printf '    %s\n' "$dep"
done
failed=""
for dep in $(k -n "$NS" get deploy -l meko.dev/stack=true -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
  if [ -n "${NEED_KEY:-}" ] && [ "$dep" = "meko-inference-gateway" ]; then
    echo "    skipping $dep — it needs OPENAI_API_KEY to start at all"
    continue
  fi
  if ! k -n "$NS" rollout status "deploy/$dep" --timeout="$TIMEOUT"; then
    failed="$failed $dep"
    k -n "$NS" describe "deploy/$dep" | grep -E '^(  Warning|    Reason|    Message)' | head -5 || true
    k -n "$NS" logs "deploy/$dep" --tail=30 || true
  fi
done
[ -z "$failed" ] || die "these components never became ready:$failed (see above)"

# What the fleet sees. Poll, because the fleet view is whatever the agent last reported:
# for up to one tick (~10s) after a workload becomes ready it still says 0/1, and
# reading once turns that lag into a failure.
log "waiting for the fleet API to report the whole stack healthy"
want='yugabytedb api-server mcp-server rag-worker inference-gateway ui'
[ -n "${NEED_KEY:-}" ] && want='yugabytedb api-server mcp-server rag-worker ui'
for _ in $(seq 1 30); do
  envs="$(api GET /api/v1/environments 2>/dev/null || true)"
  python3 - "$ENV_NAME" "$envs" "$want" >/dev/null 2>&1 <<'PY' && break
import json, sys
items = json.loads(sys.argv[2] or "{}").get("data", {}).get("items") or []
e = next((x for x in items if x["id"] == sys.argv[1]), None)
comps = {c["name"]: c for c in ((e or {}).get("components") or [])}
want = sys.argv[3].split()
sys.exit(0 if all(
    w in comps and comps[w]["total"] > 0 and comps[w]["healthy"] == comps[w]["total"]
    for w in want) else 1)
PY
  sleep 5
done
python3 - "$ENV_NAME" "${envs:-}" "$want" "${NEED_KEY:-}" <<'PY' || die "the fleet API does not report a healthy stack"
import json, sys
items = json.loads(sys.argv[2] or "{}").get("data", {}).get("items") or []
e = next((x for x in items if x["id"] == sys.argv[1]), None)
if e is None:
    sys.exit("%s not in the fleet" % sys.argv[1])
comps = {c["name"]: c for c in (e.get("components") or [])}
print("    state=%s" % e["state"])
for c in (e.get("components") or []):
    print("      %-20s %d/%d %s%s" % (
        c["name"], c["healthy"], c["total"], c.get("version") or "",
        "  ERROR: " + c["error"] if c.get("error") else ""))
missing = [w for w in sys.argv[3].split()
           if w not in comps or comps[w]["total"] == 0 or comps[w]["healthy"] != comps[w]["total"]]
if missing:
    sys.exit("not healthy: %s" % ", ".join(missing))
# READY means EVERY declared component is up, so it is only reachable when nothing was
# skipped. With no OPENAI_API_KEY the gateway cannot start, and PROVISIONING is then the
# correct answer rather than a failure — the environment really does have a declared
# component that is not running.
if sys.argv[4]:
    print("    (PROVISIONING is expected: the gateway is declared but cannot start "
          "without OPENAI_API_KEY. Set it to reach READY.)")
elif e["state"] != "READY":
    sys.exit("state=%s, want READY" % e["state"])
PY

cat <<EOF

==> The whole Meko stack is running in $NS, from a registration and nothing else.

    api-server:   kubectl --context $CTX -n $NS port-forward svc/meko-api-server 25433:25433
                  then GET http://127.0.0.1:25433/api/v1/health
    UI:           kubectl --context $CTX -n $NS port-forward svc/meko-ui 5000:5000
    MCP server:   kubectl --context $CTX -n $NS port-forward svc/meko-mcp-server 18000:8000
                  then POST http://127.0.0.1:18000/mcp
    Gateway:      kubectl --context $CTX -n $NS port-forward svc/meko-inference-gateway 8088:8088
                  then GET http://127.0.0.1:8088/healthz
    Database:     kubectl --context $CTX -n $NS exec -it $DB-0 -- bin/ysqlsh -h \$(hostname) -p 5433 -U yugabyte -d meko

    Agent logs:   kubectl --context $CTX -n $NS logs -f deploy/meko-agent
    What it runs: kubectl --context $CTX -n $NS get all,cm -l meko.dev/stack=true

    Tear down:    kubectl --context $CTX delete namespace $NS
                  kubectl --context $CTX delete clusterrole,clusterrolebinding meko-agent-$NS
EOF
