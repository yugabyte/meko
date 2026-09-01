# AMP Voyager job image — assess scripts

These scripts replace `yb-voyager assess-migration` for a PostgreSQL → AMP
migration. They are copied into the job image at `/opt/amp/voyager/` by
`deploy/voyager/Dockerfile`. The assess job entrypoint is
`/opt/amp/voyager/assess.sh`. The migrate job entrypoint is
`/opt/amp/voyager/migrate.sh`.

AMP compute is PostgreSQL, so voyager's YugabyteDB compatibility analysis
(unsupported datatypes, PL/pgSQL, query constructs, colocation, sharding)
does not apply. Assessment reports two things: complexity `LOW`, and a vCPU
recommendation matched to the source database's own capacity. The payload is
shaped like voyager's `AssessMigrationPayloadYugabyteD`, and the same
`ybvoyager_visualizer` rows are written, so the AMP controller read path does
not change.

Migrate still runs stock `yb-voyager` export/import; that sequence lives in
`migrate.sh` rather than in the controller.

## What each file does

| File | Runs against | Role |
| --- | --- | --- |
| `assess.sh` | — | Image entrypoint. Translates AMP job env (`SOURCE_HOST`, `EXPORT_DIR`, …) into driver arguments. This is what the assess Job/container executes. |
| `migrate.sh` | source + target | Image entrypoint. Runs stock `yb-voyager` export schema → create target DB → import schema → export data → import data → finalize. |
| `amp-assess-migration.sh` | source + control plane | Driver. Sequences facts → start row → vCPU ladder → payload → completed row. |
| `amp-source-facts.psql` | source | One cheap round trip: privileges, GUCs, identity, database size. |
| `amp-source-os-capacity.psql` | source | The `exact` vCPU tier: real CPU limits via `pg_read_file` on `/proc` and cgroup. |
| `amp-controlplane-bootstrap.psql` | control plane | Creates `ybvoyager_visualizer.*` with the same DDL voyager uses. |
| `amp-create-target-db.psql` | target | Idempotent `CREATE DATABASE` before `import schema`. |

`assess.sh` and `migrate.sh` are the only files the job runner needs to know
about. The rest are implementation details of those entrypoints.

### `assess.sh`

Thin adapter. The controller already injects:

- `SOURCE_HOST` / `SOURCE_PORT` / `SOURCE_USER` / `SOURCE_DB` / `SOURCE_SCHEMAS`
- `SOURCE_DB_PASSWORD` → exported as `PGPASSWORD` for `psql`
- `EXPORT_DIR` (`/voyager/export` on the mounted volume)
- `YB_VOYAGER_MIGRATION_UUID` (AMP `migration.id`)
- `CONTROL_PLANE_TYPE=yugabyted` and `YUGABYTED_DB_CONN_STRING`

It exports `PGHOST` / `PGPORT` / `PGUSER` / `PGDATABASE` (so usernames like
Azure's `user@server` are not stuffed into a URI), converts comma-separated
schemas to pipe-separated (voyager's `schema_name` convention), and `exec`s
the driver with `--psql-env`. It never calls `yb-voyager`.

A re-assessment reuses the migration's shared export volume, so the driver
clears the prior `assessment/` output before regenerating — the start-clean
analogue of what `migrate.sh` does with `--start-clean`. Without it a re-run
would layer onto stale files (and a stock `yb-voyager assess-migration` would
refuse a dirty export dir). The cleanup is scoped to `assessment/`, so a
re-assessment never disturbs a migrate's exported schema/data on the same
volume.

### `migrate.sh`

Thin entrypoint for the migrate job. The controller already injects the
source, target, `EXPORT_DIR`, and `PARALLEL_JOBS` env. The script validates
those, creates the target database if it is missing
(`amp-create-target-db.psql`), and runs the stock voyager sequence. Target
identity is passed as `psql -h/-p/-U/-d` and voyager `--target-db-*` flags,
not a URI.

### `amp-assess-migration.sh`

The driver. Standalone-testable with three arguments
(`connection_string`, `schema_list`, `output_dir`). Flow:

```
validate args + env
   │
   ├─ amp-source-facts.psql ──────────────► 10 fields, one psql round trip
   │
   ├─ [if CONTROL_PLANE_TYPE=yugabyted]
   │     amp-controlplane-bootstrap.psql
   │     SEQ = MAX(invocation_sequence) + 1
   │     INSERT seq=SEQ, status='IN PROGRESS', payload=''
   │
   ├─ vCPU ladder ────────────────────────► VCPUS, CONFIDENCE, DETECTION_METHOD
   │
   ├─ build payload, write local report
   │
   └─ [if control plane enabled]
         INSERT seq=SEQ+1, status='COMPLETED', payload=<json>
```

The `IN PROGRESS` row is written *before* detection, mirroring voyager. If
detection dies, status shows a started-never-finished assessment.

vCPU ladder:

| Confidence | Rule |
| --- | --- |
| `exact` | `min(Cpus_allowed_list, cgroup cpu.max quota)`, else `/proc/cpuinfo` |
| `high` | `max_worker_processes > 8 → ÷2`, else `max_parallel_workers > 8 → ×2` |
| `unknown` | `-1` — the caller decides |

The `> 8` gate is the trick: PostgreSQL ships both GUCs at exactly 8, so a
larger value means the platform sized them from the machine
(`GREATEST(vCPU*2, 8)` / `GREATEST(vCPU/2, 8)`). Small managed instances
often fall through to `-1`. A hand-tuned GUC is indistinguishable from a
platform-computed one.

Payload version is `1.8-amp.1`. `MigrationComplexity` is `LOW`.
`MemoryPerInstance` is fixed at 16. How the vCPU number was arrived at
(detection method and confidence) is recorded in `SizingNotes`/`Notes`.

Because PG → AMP is PostgreSQL → PostgreSQL, the payload carries only the
fields the AMP controller reads. Everything else is omitted:
no schema/conversion analysis (`SchemaSummary`, `AssessmentIssues`,
`ConversionIssues`), no colocation/sharding (`ColocatedTables`,
`ShardedTables`, `TargetRecommendations`), no multi-node cluster sizing or
YB import-time modelling (`OptimalConnections*`, `EstimatedTimeInMinForImport*`),
no `TargetDBVersion` / `TableIndexStats`, and no `SourceEnvironment` (the parser
never read it). The remaining shape still unmarshals into voyager's
`AssessMigrationPayloadYugabyteD`, so the controller read path is unchanged —
absent fields simply parse as their zero value.

The kept payload is:

```json
{
  "PayloadVersion": "1.8-amp.1",
  "VoyagerVersion": "amp-assess-1.0",
  "MigrationComplexity": "LOW",
  "MigrationComplexityExplanation": "...",
  "SourceSizeDetails": { "TotalDBSize": 8689331 },
  "Sizing": {
    "SizingRecommendation": { "NumNodes": 1, "VCPUsPerInstance": 14, "MemoryPerInstance": 16 },
    "FailureReasoning": ""
  },
  "GeneralNotes": ["..."],
  "SizingNotes": ["Recommended AMP compute matched to source capacity: 14 vCPU (confidence: exact, via Cpus_allowed_list)."],
  "Notes": ["Recommended AMP compute matched to source capacity: 14 vCPU (confidence: exact, via Cpus_allowed_list)."]
}
```

### `amp-source-facts.psql`

Always runs against the source. Emits one pipe-separated row; field order is
load-bearing.

| # | Field | Used for |
| --- | --- | --- |
| 1–3 | superuser / `pg_read_server_files` / `EXECUTE` on `pg_read_file` | Whether the `exact` tier is attempted |
| 4–5 | `max_worker_processes`, `max_parallel_workers` | The `high` tier |
| 6–10 | database name, version, address, port, `pg_database_size` | Control-plane columns and `TotalDBSize` |

`pg_settings` lookups are `coalesce(..., '0')` so a missing GUC does not
shift later fields. Reading `/proc` needs **both** the role and the function
privilege; either one alone still fails, which is why fields 2 and 3 are
separate.

### `amp-source-os-capacity.psql`

Runs against the source only when the capability probe passed. Three fields:
cgroup `cpu.max`, `Cpus_allowed_list`, `/proc/cpuinfo` processor count.

It aims at a number that is true *for the database*, not the machine:

- `/proc` is host-scoped inside a container, so `cpuinfo` is a last resort.
- `cpu.max` lives on the process cgroup, not always `/sys/fs/cgroup/cpu.max`.
- A cpuset restriction leaves `cpu.max` as `max`; only `Cpus_allowed_list`
  shows it. Both are read and the smaller wins.

`pg_read_file(..., missing_ok)` turns an absent path into NULL, so the file
is harmless on macOS / non-Linux. cgroup v1 is not handled.

### `amp-controlplane-bootstrap.psql`

Pure DDL against the control-plane database. Copied from voyager's
`setupDatabase()`: original 11-column `CREATE TABLE`, then the 7 `ALTER`s
that added `host_ip` / `port` / `db_version` / `voyager_info` and widened
three `VARCHAR`s. Matching that sequence means a DB bootstrapped here and
one bootstrapped by voyager end up identical in either order.

## Image path

From the AMP repository root (`meko/amp`):

```bash
docker build -t yb-amp/voyager:dev -f deploy/voyager/Dockerfile .
```

Scripts land at `/opt/amp/voyager/`. The assess job command is
`/opt/amp/voyager/assess.sh`; migrate is `/opt/amp/voyager/migrate.sh`.

## Local run (no image)

Requires `psql` on `PATH`.

```sh
PGPASSWORD=secret \
YB_VOYAGER_MIGRATION_UUID=90b8b0b8-1111-2222-3333-444455556666 \
CONTROL_PLANE_TYPE=yugabyted \
YUGABYTED_DB_CONN_STRING='postgresql://yugabyte:yugabyte@127.0.0.1:5433/voyager' \
./deploy/voyager/scripts/amp-assess-migration.sh 'postgresql://user@dbhost:5432/prod' 'public' /tmp/amp-assessment
```

Or, with the same env the job runner sets:

```sh
SOURCE_HOST=dbhost SOURCE_PORT=5432 SOURCE_USER=user SOURCE_DB=prod \
SOURCE_SCHEMAS=public SOURCE_DB_PASSWORD=secret \
EXPORT_DIR=/tmp/amp-assessment \
YB_VOYAGER_MIGRATION_UUID=90b8b0b8-1111-2222-3333-444455556666 \
CONTROL_PLANE_TYPE=yugabyted \
YUGABYTED_DB_CONN_STRING='postgresql://yugabyte:yugabyte@127.0.0.1:5433/voyager' \
./deploy/voyager/scripts/assess.sh
```

Migrate, with the same env the job runner sets:

```sh
SOURCE_HOST=dbhost SOURCE_PORT=5432 SOURCE_USER=user SOURCE_DB=prod \
SOURCE_SCHEMAS=public SOURCE_DB_PASSWORD=secret \
TARGET_HOST=amp-ep TARGET_PORT=55433 TARGET_USER=cloud_admin TARGET_DB=prod \
TARGET_DB_PASSWORD=secret \
EXPORT_DIR=/tmp/amp-migrate PARALLEL_JOBS=4 \
YB_VOYAGER_MIGRATION_UUID=90b8b0b8-1111-2222-3333-444455556666 \
./deploy/voyager/scripts/migrate.sh
```
