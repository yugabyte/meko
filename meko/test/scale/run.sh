#!/usr/bin/env bash
# Drive the scale test from a laptop: code ships as a ConfigMap, runs as Jobs on the
# tainted meko-onemil-app nodes next to the database.
#
#   ./run.sh seed <start> <end> [workers]   seed datapacks [start, end)
#   ./run.sh probe <seeded> <label> [probe.py args...]
#   ./run.sh variants <seeded> <label> [variants.py args...]
#   ./run.sh backfill [backfill_memories.py args...]
#   ./run.sh mcp surface|load <label> [mcp_suite.py args...]
#   ./run.sh compact [table]                 compact all 128 partitions of a table
#   ./run.sh wait <job> [timeout]
#   ./run.sh logs <job>
#   ./run.sh sql '<statement>'
set -euo pipefail

CTX=${CTX:-amiram.mizne@amp-bench.us-east-2.eksctl.io}
# Accounts the datapacks are spread over (workload.py); 1 = everything in one account.
export SCALE_ACCOUNTS=${SCALE_ACCOUNTS:-10000}
NS=${NS:-meko-onemil}
HERE=$(cd "$(dirname "$0")" && pwd)
K=(kubectl --context "$CTX" -n "$NS")

ship() {
  "${K[@]}" create configmap scale-code --from-file="$HERE/workload.py" \
    --from-file="$HERE/seed.py" --from-file="$HERE/probe.py" --from-file="$HERE/variants.py" --from-file="$HERE/backfill_memories.py" --from-file="$HERE/mcp_suite.py" \
    --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
}

# job <name> <completions> <cpu> <memory> <command>
job() {
  local name=$1 n=$2 cpu=$3 mem=$4 cmd=$5
  "${K[@]}" delete job "$name" --ignore-not-found --wait=true >/dev/null
  "${K[@]}" apply -f - <<EOF >/dev/null
apiVersion: batch/v1
kind: Job
metadata: { name: $name, labels: { app: scale-test } }
spec:
  completionMode: Indexed
  completions: $n
  parallelism: $n
  backoffLimit: $((n * 3))
  template:
    metadata: { labels: { app: scale-test, job: $name } }
    spec:
      restartPolicy: OnFailure
      nodeSelector: { workload: meko-onemil-app }
      tolerations: [{ key: dedicated, operator: Equal, value: meko-onemil, effect: NoSchedule }]
      containers:
        - name: run
          image: python:3.12-slim
          workingDir: /code
          env: [{ name: DSN, value: "postgresql://yugabyte@yb-tservers:5433/yugabyte" },
                { name: PIP_DISABLE_PIP_VERSION_CHECK, value: "1" },
                { name: SCALE_ACCOUNTS, value: "${SCALE_ACCOUNTS:-10000}" },
                { name: NEW_TABLE_ONLY, value: "${NEW_TABLE_ONLY:-0}" },
                { name: MCP_PATS_FILE, value: /pats/pats }]
          command: [bash, -c]
          args:
            - |
              pip install -q --root-user-action=ignore numpy 'psycopg[binary]' pgvector httpx && $cmd
          resources: { requests: { cpu: "$cpu", memory: $mem }, limits: { memory: $mem } }
          # A file, not an env var: 10k PATs outgrow Linux's 128 KiB limit on one env string.
          volumeMounts: [{ name: code, mountPath: /code }, { name: pats, mountPath: /pats }]
      volumes: [{ name: code, configMap: { name: scale-code } },
                { name: pats, secret: { secretName: mcp-load-pats, optional: true } }]
EOF
  echo "job/$name started ($n pods)"
}

case ${1:-} in
seed)
  start=$2 end=$3 workers=${4:-16}
  ship
  job "seed-$start-$end" "$workers" 1 2Gi \
    "python seed.py --start $start --end $end --workers $workers"
  ;;
probe)
  seeded=$2 label=$3
  shift 3
  ship
  job "probe-$label" 1 2 4Gi \
    "python probe.py --seeded $seeded --label $label --out /dev/stdout $* | grep -v '\"test\"'"
  ;;
variants)
  seeded=$2 label=$3
  shift 3
  ship
  job "variants-$label" 1 2 4Gi "python variants.py --seeded $seeded $*"
  ;;
mcp)
  # ./run.sh mcp surface|load <label> [mcp_suite.py args...]; results stay in the pod log
  mode=$2 label=$3
  shift 3
  ship
  job "mcp-$label" 1 1 2Gi "python mcp_suite.py $mode --label $label --out /dev/stdout $*"
  ;;
backfill)
  shift
  ship
  job backfill-memories 1 4 4Gi "python backfill_memories.py $*"
  ;;
compact)
  # A bulk-backfilled vector index answers ~15x slower until compacted
  # (270 ms vs 17 ms p50 at 100k datapacks), so compact before switching reads.
  table=${2:-meko_mem0_memories}
  "${K[@]}" exec yb-master-0 -c yb-master -- bash -c "
      M=yb-master-0.yb-masters:7100,yb-master-1.yb-masters:7100,yb-master-2.yb-masters:7100
      for i in \$(seq -f '%03g' 0 127); do
        /home/yugabyte/bin/yb-admin --master_addresses \$M compact_table ysql.yugabyte ${table}_\$i 600 >/dev/null || echo \"failed: ${table}_\$i\"
      done"
  ;;
wait)
  "${K[@]}" wait --for=condition=complete "job/$2" --timeout="${3:-3600s}" >/dev/null
  ;;
logs)
  "${K[@]}" logs -l job="$2" --tail=-1 --prefix --max-log-requests=64
  ;;
sql)
  "${K[@]}" exec yb-tserver-0 -c yb-tserver -- ysqlsh -h yb-tserver-0.yb-tservers -c "$2"
  ;;
*)
  sed -n '2,14p' "$0"
  exit 1
  ;;
esac
