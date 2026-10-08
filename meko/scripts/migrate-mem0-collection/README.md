# Move memories to `meko_mem0_memories`

Moves a deployment's mem0 memories from `meko_mem0_collection` to the
datapack-partitioned `meko_mem0_memories` that data migration 000006 creates.
`memory_search` then reads one partition instead of 128.

Only api-server and meko-mcp-server write these tables, and both pick the table
from one chart value, `memory.collectionName`. They must never write different
tables at the same time: a delete, edit or promotion would land in only one.
So the cutover stops both, syncs, and starts both on the new table.

The pause lasts about as long as stopping and starting the two Deployments plus
the sync. On the 3-node scale-test universe the sync compared about 1,000 rows
a second per stream. At 8 streams that's about 2 minutes for 214k memories and
279k entity rows, the production size in October 2026.

## Before you start

- Migration 000006 is applied (any api-server release that ships it).
- meko-mcp-server runs meko-mem0 2.0.14.5 or later.
- Python 3 with psycopg 3, and a DSN for the data database that can reach it
  from where you run the script:

  ```bash
  pip install "psycopg[binary]"
  export DSN='postgresql://<user>:<password>@<host>:5433/yugabyte'
  ```

## 1. Copy while Meko is up

```bash
python sync.py --copy --parallel 8
```

Then compact both new tables: a freshly copied vector index answered about 15x
slower (270 ms against 17 ms) until it was compacted.

```bash
M=<master addresses, host:7100,...>
DB=<the data database, the one in DSN>
for t in meko_mem0_memories meko_mem0_memories_entities; do
  for i in $(seq -f '%03g' 0 127); do
    yb-admin --master_addresses "$M" compact_table "ysql.$DB" "${t}_$i" 600
  done
done
```

Rows written after the copy are picked up by the sync in step 2.

## 2. Cut over

1. Stop both writers. Meko is down from here until step 3 finishes.

   ```bash
   kubectl -n <api-server ns> scale deploy/meko-api-server --replicas=0
   kubectl -n <mcp ns> scale deploy/meko-mcp-server --replicas=0
   kubectl -n <api-server ns> wait --for=delete pod -l app=meko-api-server --timeout=5m
   kubectl -n <mcp ns> wait --for=delete pod -l app=meko-mcp-server --timeout=5m
   ```

   The two can share a namespace; the chart lets each component set its own.

2. Make the new tables match the old ones, then check the counts:

   ```bash
   python sync.py --sync --parallel 8
   python sync.py --verify
   ```

   `--verify` exits non-zero on a count mismatch. Don't continue past one.

3. Point both services at the new table. `helm upgrade` also restores their
   replica counts from the chart values:

   ```bash
   helm upgrade <release> <chart> --reuse-values \
     --set memory.collectionName=meko_mem0_memories
   ```

   Add the search options (`memory.exactSearchMaxRows`, `memory.hnswEfSearch`)
   in the same upgrade if you use them.

4. Check that `memory_search` returns memories for a known datapack.

Leave `meko_mem0_collection` in place until you're past the point of rolling
back. Dropping it is a later release.

## Roll back

The same pause, in the other direction: stop both Deployments, then

```bash
python sync.py --sync --reverse --parallel 8
python sync.py --verify
helm upgrade <release> <chart> --reuse-values --set memory.collectionName=""
```

`--reverse` copies every change made on the new table back to the old one, so
nothing written after the cutover is lost.

## How the sync works

For each source partition, the sync looks up every row in the destination by
primary key, and inserts it or overwrites it if it's missing or differs in any
column. Then, for each destination partition, it deletes rows the source no
longer has. A rerun with nothing changed writes nothing. It never uses a plain
join: on the scale test the planner hashed the other table whole, spilled past
`temp_file_limit` and failed.
