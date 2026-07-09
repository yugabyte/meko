#!/usr/bin/env bash
# Per-target connection params. Source this from runner scripts:
#   source ~/benchmark/scripts/_env.sh ybLight    # or aurora

set_target() {
  case "$1" in
    ybLight)
      export TARGET=ybLight
      export PGHOST=172.151.29.184
      export PGPORT=55432
      export PGUSER=cloud_admin
      export PGPASSWORD='Yugabyte!1'
      export PGSSLMODE=disable
      export PGDATABASE=postgres
      ;;
    ybLight4)
      # 4-vCPU box (172.151.23.96); compute + control plane here, pageserver off-box.
      export TARGET=ybLight4
      export PGHOST=172.151.23.96
      export PGPORT=55432
      export PGUSER=cloud_admin
      export PGPASSWORD='Yugabyte!1'
      export PGSSLMODE=disable
      export PGDATABASE=postgres
      ;;
    aurora)
      export TARGET=aurora
      export PGHOST=yb-light-2core2.cluster-cbtcvpszcgdq.us-west-2.rds.amazonaws.com
      export PGPORT=5432
      export PGUSER=postgres
      export PGPASSWORD='Yugabyte!1'
      export PGSSLMODE=require
      export PGDATABASE=postgres
      ;;
    aurora3)
      # yb-light-2core3 instance (172.151.45.251) — Aurora target for the 750 run.
      export TARGET=aurora3
      export PGHOST=yb-light-2core3-instance-1.cbtcvpszcgdq.us-west-2.rds.amazonaws.com
      export PGPORT=5432
      export PGUSER=postgres
      export PGPASSWORD='Yugabyte!1'
      export PGSSLMODE=require
      export PGDATABASE=postgres
      ;;
    *)
      echo "Usage: set_target ybLight|ybLight4|aurora|aurora3" >&2
      return 1
      ;;
  esac
}

# Bundled libpq + psql + pgbench (needed for Neon extensions; libpq RUNPATH workaround).
export PSQL=~/yb-neon-bundle/pg_install/v18/bin/psql
export PGBENCH=~/yb-neon-bundle/pg_install/v18/bin/pgbench
export LD_LIBRARY_PATH=~/yb-neon-bundle/pg_install/v18/lib:${LD_LIBRARY_PATH:-}

# Results dir for this run, set by callers (default: timestamped).
results_dir() {
  local target="$1" testid="$2"
  local ts; ts=$(date -u +%Y%m%d-%H%M)
  echo ~/benchmark/results/${ts}-${target}-${testid}
}

if [[ -n "${1:-}" ]]; then
  set_target "$1"
fi
