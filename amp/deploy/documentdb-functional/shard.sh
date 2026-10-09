#!/bin/sh
# One shard of the documentdb functional-test gate, run INSIDE upstream's
# functional-tests image (entrypoint overridden) by run.sh. It is upstream's
# own per-shard PR-gate step (.github/workflows/functional_tests.yml in the
# documentdb tree), with amp's exclusions applied to the allowlist first:
#
#   1. this shard's round-robin slice of the (amp-filtered) allowlist;
#   2. those tests under pytest-xdist against the gateway;
#   3. the non-passes re-run sequentially, when there are at most 200 of them,
#      and merged over the parallel report. Upstream does this because a
#      parallel pass can cascade a transient failure onto passing tests;
#   4. the result in /results/report_shard.json, then /results/done.
#
# Enforcing the allowlist is run.sh's job, over all shards merged, as upstream's
# combine job does. This only has to produce an honest report.
#
# Env: URI (gateway connection string), SHARD, NUM_SHARDS, WORKERS.
# /gate holds the ConfigMap run.sh built; /results is this pod's scratch.
set -u
G=/gate
R=/results
mkdir -p "$R"
# The container outlives the run so run.sh can copy /results out: kubectl exec
# cannot reach a completed pod. run.sh deletes the pod once it has them.
finish() {
  echo "$1" >"$R/shard.rc"
  touch "$R/done"
  exec sleep 3600
}

python3 -c 'import gzip, shutil; shutil.copyfileobj(gzip.open("/gate/allowlist.yml.gz"), open("/tmp/allowlist.yml", "wb"))' || finish 2
python3 "$G/amp_gate.py" /tmp/allowlist.yml "$G/exclusions.yml" documentdb "$R/allowlist.amp.yml" || finish 2
GATE="python3 $G/functional_gate.py --image $G/image.yml --allowlist $R/allowlist.amp.yml --engine-name documentdb"

$GATE shard-allowlist --num-shards "$NUM_SHARDS" --shard-id "$SHARD" \
  --prefix documentdb_tests/ --output "$R/ids.txt" || finish 2
# An empty slice would leave pytest with no positional arguments, and it would
# then collect the whole upstream suite, engine-crashing files included.
[ -s "$R/ids.txt" ] || {
  echo "shard $SHARD: empty allowlist slice" >&2
  finish 2
}
echo "shard $SHARD/$NUM_SHARDS: $(grep -c . "$R/ids.txt") allowlisted test(s)"

cd /app || finish 2
# @file: pytest reads the node IDs from the file, one per line. Thousands of
# them would not fit on a command line.
pytest --rootdir documentdb_tests "@$R/ids.txt" \
  --engine-name documentdb --connection-string "$URI" \
  -n "$WORKERS" -q -p no:cacheprovider \
  --json-report --json-report-file="$R/report.json" \
  --junitxml="$R/results.xml" >"$R/pytest.log" 2>&1
tail -5 "$R/pytest.log"
[ -f "$R/report.json" ] || {
  echo "shard $SHARD: no report.json" >&2
  finish 2
}

$GATE gate-failures --report "$R/report.json" --output "$R/rerun_ids.txt"
n=$(grep -c . "$R/rerun_ids.txt" 2>/dev/null)
n=${n:-0}
echo "shard $SHARD: $n non-pass after the parallel run"
if [ "$n" -gt 0 ] && [ "$n" -le 200 ]; then
  sed 's#^#documentdb_tests/#' "$R/rerun_ids.txt" >"$R/rerun_args.txt"
  sleep 20
  pytest --rootdir documentdb_tests "@$R/rerun_args.txt" \
    --engine-name documentdb --connection-string "$URI" \
    -p no:xdist --timeout=600 -q -p no:cacheprovider \
    --json-report --json-report-file="$R/report_rerun.json" >"$R/pytest_rerun.log" 2>&1
  tail -3 "$R/pytest_rerun.log"
  if [ -f "$R/report_rerun.json" ]; then
    $GATE merge-reports --base "$R/report.json" --overlay "$R/report_rerun.json" \
      --out "$R/report_shard.json" || finish 2
  fi
fi
[ -f "$R/report_shard.json" ] || cp "$R/report.json" "$R/report_shard.json"
finish 0
