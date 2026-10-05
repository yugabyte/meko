# Voyager end-to-end migration runbook

This runbook performs a complete local migration through the AMP controller API:

```text
sample PostgreSQL source
        │
        ▼
AMP migration API → assess → provision AMP target → migrate
        │                                  │
        ▼                                  ▼
Voyager Docker job                 AMP endpoint Docker container
```

It uses Docker for every runtime component so the controller, source PostgreSQL,
Voyager jobs, and provisioned target endpoint can resolve each other by Docker
service/container name.

> This is a local development runbook. It uses insecure, development-only
> YugabyteDB credentials and mounts the Docker socket into the controller. Do
> not use this setup unchanged in production.

## 1. Prerequisites

You need:

- Docker Desktop running. Confirm that both commands succeed:

  ```bash
  docker info
  docker ps
  ```

- `curl`, `jq`, and `psql` available locally.
- A controller image containing the current Voyager implementation:
  `yb-amp/controller:dev`.
- An AMP endpoint image:
  `yb-amp/neon-endpoint:dev`.
- A Voyager image compatible with this integration. Set it below as
  `VOYAGER_IMAGE`.

Build the controller image when it is not already available:

```bash
cd /path/to/meko/amp
make controller-bundle BUILD_TYPE=release
docker build -t yb-amp/controller:dev -f deploy/controller/Dockerfile .
```

Build the Voyager job image (embeds `/opt/amp/voyager/assess.sh`,
`/opt/amp/voyager/migrate.sh`, and `/opt/amp/voyager/end.sh`):

```bash
docker build -t yb-amp/voyager:dev -f deploy/voyager/Dockerfile .
```

Build the endpoint image from the AMP repository root. The Quay base
`quay.io/yugabyte/amp-build-tools:v2` is private — `docker login quay.io`

```bash
docker build -t yb-amp/neon-endpoint:dev -f deploy/endpoint/Dockerfile .
```

A warm BuildKit cargo cache can skip regenerating
`communicator/communicator_bindings.h`. The cargo target dir is a
BuildKit cache mount, so `--no-cache` only drops layer cache and the
stale fingerprints remain. If `yb-amp-pg-ext` fails on that header,
clear the cache mount and rebuild:

```bash
docker builder prune --filter type=exec.cachemount
docker build -t yb-amp/neon-endpoint:dev -f deploy/endpoint/Dockerfile .
```

## 2. Define local configuration

Run the following from the AMP repository root:

```bash
cd /path/to/meko/amp

export NETWORK=voyager-e2e
export API=http://127.0.0.1:8080/api/v1
export YB_CONTAINER=voyager-yb
export SOURCE_CONTAINER=voyager-source-pg
export CONTROLLER_CONTAINER=voyager-controller

# Must be the AMP Voyager image (deploy/voyager/Dockerfile), which embeds
# /opt/amp/voyager/assess.sh, /opt/amp/voyager/migrate.sh, and /opt/amp/voyager/end.sh.
# Stock yugabytedb/yb-voyager cannot run these AMP entrypoints.
export VOYAGER_IMAGE='yb-amp/voyager:dev'

# This is intentionally a test-only credential.
export SOURCE_PASSWORD='change-this-local-test-password'
```

Create an isolated network and record its gateway. Voyager jobs run on
this network, so the target address the controller stores must be
reachable from a container — not `127.0.0.1` (that is the Voyager job
itself). `AMP_DEPLOY_EXTERNAL_HOST` defaults to `127.0.0.1` and is what
the Docker deployer writes into `target.host`.

```bash
docker network create "$NETWORK" 2>/dev/null || true
export DEPLOY_EXTERNAL_HOST=$(
  docker network inspect -f '{{(index .IPAM.Config 0).Gateway}}' "$NETWORK"
)
echo "DEPLOY_EXTERNAL_HOST=$DEPLOY_EXTERNAL_HOST"
```

## 3. Start controller-state YugabyteDB

The AMP controller cannot use stock PostgreSQL for its own state store:
its storage-plane migrations use YugabyteDB-specific DDL. YugabyteDB stores:

- normal AMP controller state;
- `amp_voyager_control.migration`;
- `amp_voyager_control.migration_run`;
- Voyager visualizer data written by Voyager.

Start a single-node, development-only YugabyteDB. This image treats
`--listen` as a deprecated alias of `--advertise_address` and exits if
both are set to different values (`0.0.0.0` vs the container name). Pass
only `--advertise_address`. YSQL still listens on the container address
that Docker publishes as `-p 5433:5433`, and the controller/Voyager
containers reach it as `voyager-yb:5433` on `$NETWORK`.

```bash
docker run -d --name "$YB_CONTAINER" --network "$NETWORK" \
  -p 5433:5433 \
  yugabytedb/yugabyte:2025.1.1.1-b1 \
  bin/yugabyted start \
    --daemon=false \
    --advertise_address="$YB_CONTAINER"
```

Wait for its YSQL endpoint:

```bash
until docker exec "$YB_CONTAINER" \
  bin/ysqlsh -h "$YB_CONTAINER" -U yugabyte -d yugabyte -c 'SELECT 1' >/dev/null 2>&1
do
  sleep 2
done
```

The controller and Voyager containers use this internal Docker-network URL:

```bash
export CONTROL_DB='postgres://yugabyte:yugabyte@voyager-yb:5433/yugabyte?sslmode=disable'
```

## 4. Start and populate the source PostgreSQL database

Start a PostgreSQL 17 source on the same network:

```bash
docker run -d --name "$SOURCE_CONTAINER" --network "$NETWORK" \
  -e POSTGRES_USER=source_user \
  -e POSTGRES_PASSWORD="$SOURCE_PASSWORD" \
  -e POSTGRES_DB=shop \
  postgres:17

until docker exec "$SOURCE_CONTAINER" pg_isready -U source_user -d shop >/dev/null
do
  sleep 1
done
```

Create a representative schema and data:

```bash
docker exec -i "$SOURCE_CONTAINER" psql -U source_user -d shop <<'SQL'
CREATE TABLE customers (
  id         bigserial PRIMARY KEY,
  email      text UNIQUE NOT NULL,
  name       text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE orders (
  id          bigserial PRIMARY KEY,
  customer_id bigint NOT NULL REFERENCES customers(id),
  total_cents bigint NOT NULL,
  status      text NOT NULL DEFAULT 'pending',
  placed_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX orders_by_customer ON orders(customer_id);

INSERT INTO customers (email, name)
SELECT 'user' || i || '@example.com', 'User ' || i
FROM generate_series(1, 500) AS i;

INSERT INTO orders (customer_id, total_cents, status)
SELECT
  (random() * 499)::int + 1,
  (random() * 100000)::int,
  (ARRAY['pending', 'paid', 'shipped'])[(random() * 2)::int + 1]
FROM generate_series(1, 2000);
SQL
```

Record the expected source counts:

```bash
docker exec "$SOURCE_CONTAINER" psql -U source_user -d shop -c \
  "SELECT 'customers' AS table_name, count(*) FROM customers
   UNION ALL
   SELECT 'orders', count(*) FROM orders;"
```

Expected result:

```text
 customers | 500
 orders    | 2000
```

## 5. Start the AMP controller with Voyager enabled

The Docker deployer requires access to the host Docker daemon, because the
controller creates both AMP endpoint and Voyager containers. `--user 0` is
needed only in this local setup to access the mounted Docker socket.

```bash
docker run -d --name "$CONTROLLER_CONTAINER" \
  --network "$NETWORK" \
  --user 0 \
  -p 8080:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e AMP_DATABASE_URL="$CONTROL_DB" \
  -e AMP_VOYAGER_DATABASE_URL="$CONTROL_DB" \
  -e AMP_VOYAGER_IMAGE="$VOYAGER_IMAGE" \
  -e AMP_DEPLOY=true \
  -e AMP_DEPLOY_DRIVER=docker \
  -e AMP_DEPLOY_NETWORK="$NETWORK" \
  -e AMP_DEPLOY_ENDPOINT_IMAGE='yb-amp/neon-endpoint:dev' \
  -e AMP_DEPLOY_YB_HOST_PORTS='voyager-yb:5433' \
  -e AMP_DEPLOY_YB_USER='yugabyte' \
  -e AMP_DEPLOY_YB_DBNAME='yugabyte' \
  -e AMP_DEPLOY_EXTERNAL_HOST="$DEPLOY_EXTERNAL_HOST" \
  yb-amp/controller:dev
```

Wait for startup. The HTTP server starts only after control-plane
migrations apply (tens of seconds). `curl` may see `Connection reset by
peer` until then — keep polling:

```bash
until curl -fsS "$API/version" | jq
do
  sleep 2
done

docker logs "$CONTROLLER_CONTAINER" 2>&1 | grep -i voyager
```

The logs should include both:

```text
Voyager migrations applied
Voyager migration job runner enabled
```

## 6. Verify source connectivity through the API

This check is performed by the controller itself. It proves the hostname works
from the controller container, and the same hostname will work from a Voyager
job because both are on `$NETWORK`.

```bash
curl -sS "$API/migrations/connectivity-checks" \
  -H 'Content-Type: application/json' \
  -d @- <<JSON | jq
{
  "source": {
    "db_type": "postgresql",
    "host": "voyager-source-pg",
    "port": 5432,
    "username": "source_user",
    "password": "$SOURCE_PASSWORD",
    "database": "shop",
    "schemas": ["public"]
  }
}
JSON
```

Expected: `reachable: true`.

## 7. Create the migration

Build the body with `jq` rather than a heredoc inside `$(...)`. Pasting
`curl ... -d @- <<JSON | jq` into an interactive shell often fails to
parse the terminator, leaves `MIGRATION_ID` empty, and the follow-up GET
returns `not_found`.

```bash
REQ=$(jq -n --arg pw "$SOURCE_PASSWORD" '{
  name: "shop-postgres-to-amp",
  source: {
    db_type: "postgresql",
    host: "voyager-source-pg",
    port: 5432,
    username: "source_user",
    password: $pw,
    database: "shop",
    schemas: ["public"]
  }
}')

MIGRATION_ID=$(
  echo "$REQ" |
    curl -sS "$API/migrations" \
      -H 'Content-Type: application/json' \
      -d @- |
    jq -r '.id'
)

echo "Migration: $MIGRATION_ID"
curl -sS "$API/migrations/$MIGRATION_ID" | jq
```

The response intentionally omits the source password. The controller persists
it only in `amp_voyager_control.migration.source_config` for Voyager jobs.

## 8. Run and retrieve the assessment

Start an asynchronous assessment:

```bash
ASSESS_TASK_ID=$(
  curl -sS -X POST "$API/migrations/$MIGRATION_ID/assess" |
  jq -r '.id'
)

echo "Assessment launcher task: $ASSESS_TASK_ID"
```

The task only covers recording and launching the job. Wait until it completes:

```bash
while :; do
  curl -sS "$API/tasks/$ASSESS_TASK_ID" | jq '{id, status, error}'
  STATUS=$(curl -sS "$API/tasks/$ASSESS_TASK_ID" | jq -r '.status')
  [ "$STATUS" = "succeeded" ] && break
  [ "$STATUS" = "failed" ] && exit 1
  sleep 3
done
```

Then inspect migration state and the parsed assessment:

```bash
curl -sS "$API/migrations/$MIGRATION_ID" | jq
curl -sS "$API/migrations/$MIGRATION_ID/assessment" | jq
```

If assessment fails, locate the job and read its logs:

```bash
docker ps -a --filter 'name=voyager-assess-'
docker logs "$(docker ps -aq --filter 'name=voyager-assess-' | head -1)"
```

## 9. Create the AMP user and storage pool

Target provisioning creates a project, default branch, and endpoint in an
existing storage pool.

```bash
USER_ID=$(curl -sS -X POST "$API/users" | jq -r '.id')

POOL_ID=$(
  curl -sS "$API/storage-pools" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"voyager-e2e-pool\",\"owner_user_id\":\"$USER_ID\"}" |
  jq -r '.id'
)

echo "User: $USER_ID"
echo "Pool: $POOL_ID"
```

## 10. Provision and attach the migration target

The route is `POST /migrations/{id}/provision-target`. `POST .../target`
returns `404`. `vcpu` is normally chosen from the assessment. This local
run uses two vCPUs. Use a unique `project_name` if you retry after a
failed provision — a half-created cluster makes the next attempt fail
with `ensure cluster+default branch: conflict`.

```bash
TARGET_REQ=$(jq -n --arg pool "$POOL_ID" --arg proj "shop-migrated-$(date +%s)" '{
  pool_id: $pool,
  vcpu: 2,
  project_name: $proj
}')

TARGET_TASK_ID=$(
  echo "$TARGET_REQ" |
    curl -sS -X POST "$API/migrations/$MIGRATION_ID/provision-target" \
      -H 'Content-Type: application/json' \
      -d @- |
    jq -r '.id'
)

echo "Target-provisioning task: $TARGET_TASK_ID"
```

Poll until the target task reaches a terminal state:

```bash
while :; do
  curl -sS "$API/tasks/$TARGET_TASK_ID" | jq '{id, status, error}'
  STATUS=$(curl -sS "$API/tasks/$TARGET_TASK_ID" | jq -r '.status')
  [ "$STATUS" = "succeeded" ] && break
  [ "$STATUS" = "failed" ] && exit 1
  sleep 5
done

curl -sS "$API/migrations/$MIGRATION_ID" | jq '.target'
```

The returned target contains the host, port, username, and database name
used by Voyager. Host should be `$DEPLOY_EXTERNAL_HOST` (the Docker
network gateway), not `127.0.0.1`. The response intentionally contains
no target password. The Docker-deployed endpoint uses `cloud_admin` /
`cloud_admin`.

## 11. Start the actual migration

```bash
MIGRATE_TASK_ID=$(
  curl -sS -X POST "$API/migrations/$MIGRATION_ID/migrate" \
    -H 'Content-Type: application/json' \
    -d '{"parallel_jobs":4}' |
  jq -r '.id'
)

echo "Migration launcher task: $MIGRATE_TASK_ID"
```

Wait for launch completion:

```bash
while :; do
  curl -sS "$API/tasks/$MIGRATE_TASK_ID" | jq '{id, status, error}'
  STATUS=$(curl -sS "$API/tasks/$MIGRATE_TASK_ID" | jq -r '.status')
  [ "$STATUS" = "succeeded" ] && break
  [ "$STATUS" = "failed" ] && exit 1
  sleep 3
done
```

Poll migration state—not just the launcher task—until Voyager finishes:

```bash
while :; do
  RESPONSE=$(curl -sS "$API/migrations/$MIGRATION_ID")
  echo "$RESPONSE" | jq '{
    state,
    detail,
    runs,
    voyager: {
      phases: (.voyager.phases // []),
      data_progress: (.voyager.data_progress // null)
    }
  }'

  STATE=$(echo "$RESPONSE" | jq -r '.state')
  [ "$STATE" = "completed" ] && break
  [ "$STATE" = "failed" ] && exit 1
  sleep 5
done
```

Inspect Voyager logs when a run fails:

```bash
docker ps -a --filter 'name=voyager-migrate-'
docker logs "$(docker ps -aq --filter 'name=voyager-migrate-' | head -1)"
```

## 12. Validate migrated data

Fetch the target connection information:

```bash
TARGET=$(curl -sS "$API/migrations/$MIGRATION_ID" | jq '.target')
echo "$TARGET" | jq

TARGET_HOST=$(echo "$TARGET" | jq -r '.host')
TARGET_PORT=$(echo "$TARGET" | jq -r '.port')
TARGET_USER=$(echo "$TARGET" | jq -r '.username')
TARGET_DB=$(echo "$TARGET" | jq -r '.database')
```

Run validation from a temporary Postgres client container on the same network:

```bash
docker run --rm --network "$NETWORK" \
  -e PGPASSWORD=cloud_admin \
  postgres:17 \
  psql \
    -h "$TARGET_HOST" \
    -p "$TARGET_PORT" \
    -U "$TARGET_USER" \
    -d "$TARGET_DB" \
    -c "SELECT 'customers' AS table_name, count(*) FROM customers
        UNION ALL
        SELECT 'orders', count(*) FROM orders;"
```

For this sample, expected counts are:

```text
 customers | 500
 orders    | 2000
```

Also compare schema objects:

```bash
docker run --rm --network "$NETWORK" \
  -e PGPASSWORD=cloud_admin \
  postgres:17 \
  psql -h "$TARGET_HOST" -p "$TARGET_PORT" -U "$TARGET_USER" -d "$TARGET_DB" \
  -c '\dt public.*'
```

## 13. End the migration

> **Irreversible.** This scrubs Voyager metadata from source and target
> databases and cleans up the export directory. The migration cannot be resumed
> after this.

```bash
END_RESP=$(curl -sS -X POST "$API/migrations/$MIGRATION_ID/end")
echo "$END_RESP" | jq
```

Poll until state reaches `ended`:

```bash
while true; do
  STATE=$(curl -sS "$API/migrations/$MIGRATION_ID" | jq -r '.state')
  echo "state=$STATE"
  if [ "$STATE" = "ended" ] || [ "$STATE" = "failed" ]; then break; fi
  sleep 5
done
```

## 14. Troubleshooting

### `Cannot connect to the Docker daemon`

Docker Desktop is not ready. Do not proceed until both work:

```bash
docker info
docker ps
```

### Migration APIs return `501`

The controller did not receive `AMP_VOYAGER_DATABASE_URL`, or failed before it
could wire the Voyager store/runner:

```bash
docker logs "$CONTROLLER_CONTAINER"
```

The controller startup logs must include:

```text
Voyager migrations applied
Voyager migration job runner enabled
```

### Connectivity check is unreachable

Confirm the source container is on the same network as the controller:

```bash
docker network inspect "$NETWORK" | jq '.[0].Containers'
docker exec "$CONTROLLER_CONTAINER" getent hosts voyager-source-pg
```

### Assessment/migration task failed

The API task error identifies launch errors. Then inspect the matching Voyager
container:

```bash
docker ps -a --filter 'name=voyager-'
docker logs <voyager-container-id>
```

### YugabyteDB container exits immediately

```text
ERROR: --listen and --advertise_address flags are same. --listen is depricated.
```

This image rejects `--listen` when it differs from `--advertise_address`.
Drop `--listen` and keep only `--advertise_address="$YB_CONTAINER"`.

### `target_not_attached` on `/migrate`

The target-provisioning task has not completed successfully. Re-check:

```bash
curl -sS "$API/tasks/$TARGET_TASK_ID" | jq
curl -sS "$API/migrations/$MIGRATION_ID" | jq '.target'
```

## 15. Cleanup

This deletes all local test state:

```bash
docker rm -f \
  "$CONTROLLER_CONTAINER" \
  "$SOURCE_CONTAINER" \
  "$YB_CONTAINER" 2>/dev/null || true

for pat in voyager-assess- voyager-migrate- voyager-end- yb-ep-; do
  for id in $(docker ps -aq --filter "name=$pat"); do
    docker rm -f "$id"
  done
done
for vol in $(docker volume ls -q --filter 'name=voyager-exp-'); do
  docker volume rm "$vol"
done
docker network rm "$NETWORK" 2>/dev/null || true
```
