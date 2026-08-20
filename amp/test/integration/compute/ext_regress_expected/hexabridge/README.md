# hexabridge amp expected variants

Blessed output for hexabridge's own regression runner (`scripts/regression.sh`,
which diffs each `test/pg/<schema>/<routine>.sql` first against the Oracle ground
truth `test/ora/**/*.out`, then against a saved `test/pg/**/*.out` as a known PG
divergence). Files here are overlaid onto the fetched source's `test/pg/` dir by
[`../../extensions.sh --regress`](../../extensions.sh).

To bless a routine whose amp output legitimately differs, drop its output at the
matching path, e.g. `hexabridge/dbms_output/put_line.out`.

Blessed:

- `hexabridge/to_char.out` — the `L` (currency) format element renders the
  `lc_monetary` symbol, which is empty under the `LC_ALL=C` we run the suite with,
  so `$ 1,234.56` becomes `  1,234.56`. Only that one line differs from upstream.
- `hexabridge/uid.out` — `uid()` returns the current role's OID. Upstream hardcodes
  `10`, the OID of the bootstrap superuser `postgres` its CI runs as. amp's bootstrap
  superuser is `cloud_admin` (OID 10); the `postgres` role `extensions.sh` adds for
  the run gets the next free OID instead, so `<T01>`/`<T02>` print that OID rather
  than `10`. It is deterministic given the pinned hexabridge commit and PG major, so
  re-bless this line if either changes. The `MATCH`/`STABLE`/`POSITIVE` checks are
  computed against the live catalog and are unaffected.

See [`../README.md`](../README.md) for the blessing workflow.
