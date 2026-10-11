# Move memories to `meko_mem0_memories`

Moves a deployment's mem0 memories from `meko_mem0_collection` to
`meko_mem0_memories`, the table data migration 000006 creates. The new table is
partitioned by datapack alone, so `memory_search` reads one partition instead
of 128. Meko stays up throughout.

Only api-server and meko-mcp-server write these tables, and the chart value
`memory.collectionName` picks the table for both. During the switch, some pods
write each table. So before the switch, `sync.py` installs triggers on both
tables: each insert, edit or delete on one is applied to the other in the same
transaction. The two tables stay identical, and switching or rolling back is a
config change.

## Cost

While the triggers are on, each memory write also writes the other table. On
the 3-node scale-test universe, that added about 2-3ms to each single-row write,
through either table. Reads and searches are unaffected. Keep the triggers on
only as long as you want to be able to roll back, then remove them (step 5).

During the switch, many writes to the same memory through both tables at once
can still fail: 42 of 4,366 writes (about 1%) failed with `could not serialize`
when 16 writers hammered one row through both tables. That needs one memory
written many times a second from old and new pods during the rollout.

## Before you start

- Migration 000006 is applied (any api-server release that ships it).
- meko-mcp-server runs meko-mem0 2.0.14.5 or later.
- Python 3 with psycopg 3, and a DSN for the data database:

  ```bash
  pip install "psycopg[binary]"
  export DSN='postgresql://<user>:<password>@<host>:5433/<database>'
  ```

## 1. Start mirroring

```bash
python sync.py --mirror-on
```

From here on, every write to either table lands in both.

## 2. Copy what was there before

```bash
python sync.py --copy --parallel 8
python sync.py --sync --parallel 8
python sync.py --diff --parallel 8
```

`--copy` copies the old rows the new table lacks. `--sync` fixes the few rows a
write changed while `--copy` was reading them. `--diff` counts rows that differ
and exits 1 if any do. If it does, repeat `--sync` and `--diff` until `--diff`
reports none.

Then compact both new tables: a freshly copied vector index answered about 15x
slower (270 ms against 17 ms) until it was compacted.

```bash
M=<master addresses, host:7100,...>
DB=<the database in DSN>
for t in meko_mem0_memories meko_mem0_memories_entities; do
  for p in $(psql "$DSN" -Atc "SELECT inhrelid::regclass FROM pg_inherits WHERE inhparent = 'datapacks.$t'::regclass"); do
    yb-admin --master_addresses "$M" compact_table "ysql.$DB" "${p#datapacks.}" 600
  done
done
```

## 3. Switch

```bash
helm upgrade <release> <chart> --reuse-values \
  --set memory.collectionName=meko_mem0_memories
```

Add the search options (`memory.exactSearchMaxRows`, `memory.hnswEfSearch`) in
the same upgrade if you use them. Then check that `memory_search` returns
memories for a known datapack.

## 4. Roll back, if needed

While the triggers are on, the old table is current, so rolling back is the
same upgrade with the old name:

```bash
helm upgrade <release> <chart> --reuse-values --set memory.collectionName=""
```

After step 5 the old table no longer gets writes. To roll back then, bring it
up to date first, then switch:

```bash
python sync.py --sync --reverse --parallel 8
python sync.py --diff --reverse --parallel 8
```

Writes made between the sync and the switch would land only in the new table,
so either turn mirroring back on first (`--mirror-on`, then the sync) or roll
back with writes stopped.

## 5. Stop mirroring

Once you're past the point of rolling back:

```bash
python sync.py --mirror-off
```

The old table then stops getting writes. Drop it in a later release.

## How it was tested

On the scale-test universe (YugabyteDB 2026.1, 3 nodes, RF3). The scratch
tables had the production layout and keys, with 4 hash partitions instead of
128, and were seeded from real partitions. `--mirror-on` created the triggers
on the partitioned parents, the database copied them to every partition, and
the writers wrote through the parents. 8 writers inserted, edited, promoted and
deleted about 100 times a second:

- `--copy`, `--sync` and `--diff` ran under that load; `--diff` found 0
  differences.
- Half the writers then wrote each table for 2 minutes, as during a rolling
  switch, then all wrote the new table. `--diff` still found 0, the counts
  matched, and no write failed. That held in two runs: 40,000 and 20,000
  starting memories.
- Same-row contention: 16 writers on one or two rows through both tables. With
  both triggers running after the write, 2-3% of writes failed with `deadlock
  detected`. With the new table's trigger running first, none deadlocked; on
  one row, about 1% failed with `could not serialize`.
- `--sync` removed exactly the 300 rows planted only in the new table while
  13,170 writes hit both tables, and `--diff` then found 0.
- Entity-table writes on either side reached the other. Writes kept working
  after either table was dropped with the triggers in place, and
  `--mirror-off` left no trigger or function behind.

## How the comparison works

`--sync` and `--diff` look up every source row in the destination by primary
key, one partition at a time, and every destination row in the source. They
never use a plain join: on the scale test the planner hashed the other table
whole, spilled past `temp_file_limit` and failed. The script's own writes set
`meko.mem0_mirror = off`, so the triggers don't copy them back.
