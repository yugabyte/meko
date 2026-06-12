---
status: done
severity: medium
area: yb-pageserver
opened: 2026-06-11
---

# `yb-dump-pages` sorts by rendered LSN text, not the bigint column

## Symptom

`tools/yb-dump-pages` displays rows in non-numeric LSN order. Example
from the task-28 forensics (rel 16391 blk 254): `29/f6e9d58`
(= `29/0F6E9D58`) sorts *above* `29/7b6a28d0`, so the "most recent
record first" promise in the tool's header comment is broken whenever
the low word's hex rendering has fewer digits. Cost real time during a
highest-severity corruption hunt ([see 28](28_walredo-corruption-catch-me-if-you-can.md),
"Minor tooling notes").

## Root cause

The SELECT list renders the LSN as hex text and aliases it `AS lsn`:

```sql
to_hex((lsn >> 32)::int8) || '/' || to_hex((lsn & x'ffffffff'::bigint)::int8) AS lsn,
...
ORDER BY blknum ASC, lsn DESC, is_image ASC
```

Per SQL resolution rules, `ORDER BY lsn` binds to the *output column
alias* — the text — not `pageserver.pages.lsn` (bigint). Unpadded hex
text compares lexicographically: `'f6e9d58' > '7b6a28d0'`.

`tools/yb-dump-wals` was checked and is not affected (orders by integer
`segno`/`chunk_offset`, no alias collision).

## Fix sketch

- Order on the underlying column, e.g. `ORDER BY blknum ASC,
  pages.lsn DESC, is_image ASC` (or sort in a subquery before
  rendering).
- While here: zero-pad the low word in the rendering
  (`lpad(to_hex(...), 8, '0')`) so displayed LSNs are visually
  comparable and match the `H/XXXXXXXX` form used in PS logs.

## Acceptance

Dump a block whose history spans a low-word digit-count boundary (e.g.
rel 16391 blk 254 on the W10 dataset) and verify rows are in strictly
descending numeric LSN order with same-LSN image rows after delta rows.

## Resolution

Fixed in `tools/yb-dump-pages`:

- `ORDER BY` now references the underlying bigint column
  (`ORDER BY blknum ASC, pageserver.pages.lsn DESC, is_image ASC`).
  A qualified column reference can't bind to the output alias, so the
  sort is now numeric.
- The rendered low word is zero-padded
  (`lpad(to_hex(...), 8, '0')`), so displayed LSNs are in `H/XXXXXXXX`
  form matching PS logs and are visually comparable.

The local dataset on hand had no multi-version block crossing a
low-word digit boundary, so the boundary case was verified directly
against the live DB with the task-28 values: under the old
text-alias sort `29/f6e9d58` sorted above `29/7b6a28d0`, while the new
column sort correctly orders `29/7b6a28d0` (178164213968) before
`29/0f6e9d58` (176352566616). The tool was also run end-to-end to
confirm the qualified `ORDER BY` parses and runs in YSQL.
