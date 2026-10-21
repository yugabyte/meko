# pg_hint_plan: blessed amp diffs

All four come from contrib modules amp does not ship (`CONTRIB_MODULES` in
[`postgres.mk`](../../../../../postgres.mk)), so they are captured runs of the
suite rather than hand edits. When amp ships a module, the files that depend on
it stop matching and are deleted, not regenerated.

## `ut-init_1.out`, `ut-S_1.out`: no `btree_gist`

`ut-init` runs `CREATE EXTENSION btree_gist`, then builds `s1.ti1_gist`, a GiST
index on an integer column. Without the extension both fail:

```
ERROR:  extension "btree_gist" is not available
ERROR:  data type integer has no default operator class for access method "gist"
```

`ut-S` then hints that index by name (`IndexScan(ti1 ti1_gist)`,
`BitmapScan(ti1 ti1_gist)`, `IndexOnlyScan(ti1 ti1_gist)`). With no such index
each hint lands under "not used hint" and the planner picks its own plan.

## `ut-fdw_1.out`, `ut-W_1.out`: no `file_fdw`

`ut-fdw` builds foreign table `ft1` on `file_fdw`, and every statement after
`CREATE EXTENSION file_fdw` fails on the missing wrapper, server or table. `ut-W`
queries `ft1` once, in a parallel-append plan, and that query fails with
`relation "ft1" does not exist`. `ut-W_1.out` also reports
`max_worker_processes(64)`, the ceiling endpoint_supervisor sets on every
compute, where upstream's file has 8.

See [`../README.md`](../README.md) for the blessing workflow.
