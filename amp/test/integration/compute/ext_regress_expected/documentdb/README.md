# documentdb — blessed amp diffs

## `documentdb_test_helpers_1.out`

The suite opens by asserting the cluster's collation setup:

```sql
SELECT MIN(datcollate), MIN(datctype), MIN(pg_encoding_to_char(encoding)), MIN(datlocprovider) FROM pg_database;
```

Upstream's golden expects `en_US.UTF-8 | en_US.UTF-8 | UTF8 | c` — the libc
provider (`datlocprovider = 'c'`) with a full locale, which is what their own
`--temp-instance` initdb produces. An amp compute is initdb'd with the **builtin**
provider and `C.UTF-8`, so it reports `C.UTF-8 | C.UTF-8 | UTF8 | b`.

That is a property of how amp initializes a compute, not a documentdb
regression, and nothing in the rest of the file depends on it — the diff is
these two result blocks and nothing else. Blessed as a `_N.out` **variant**, so
pg_regress accepts either and upstream's golden stays untouched: if amp ever
moves to the libc provider, this file simply stops being the one that matches
and can be deleted.

Regenerate it by replacing those two result blocks in upstream's
`pg_documentdb/src/test/regress/expected/documentdb_test_helpers.out` with the
values from a failing run's `regression.diffs`.

## `public_api_schema_1.out`

The suite lists every function in the public API, and two `bson_dollar_redact`
overloads come back in the other order:

```
upstream:  documentdb_core.bson, documentdb_core.bson, text, documentdb_core.bson
           document documentdb_core.bson, redactspec documentdb_core.bson, ...
amp:       (those two swapped)
```

Same rows, byte for byte — only their position moves. The query's ordering does
not fully disambiguate two overloads of one name, so what breaks the tie is the
collation, and amp's builtin `C.UTF-8` orders the argument lists differently from
upstream's libc `en_US.UTF-8`. Same root cause as
`documentdb_test_helpers_1.out`.

## `dynamic_cursor_selection_tests_pg17_1.out`, `bson_aggregation_pipeline_tests_geonear_1.out`, `bson_geospatial_index_pushdown_tests_1.out`

The 2d/2dsphere GIST operator classes are gated out of the install SQL because
amp does not ship postgis, so creating a geospatial index fails:

```
ERROR:  operator class "documentdb_api_catalog.bson_gist_geometry_ops_2d" does not exist
ERROR:  The $geoNear operator needs either a 2d or 2dsphere index, but no such index exists
```

The pushdown test also calls postgis's own `st_geomfromewkb`. These capture that
as the expected behaviour. If amp ever ships postgis and the gate is removed,
they stop matching and should be deleted rather than regenerated. On PG17 and
later, 1.0's schedule runs `dynamic_cursor_selection_tests` as `_pg17`.

## Not blessed: amp's own log lines

`DEBUG:  [YBAMP_SMGR] ...` and `LOG:  ConsoleURL not set, skipping forwarding`
appear in tests that lower `client_min_messages` far enough to see the
extension's own messages, and no upstream expected file contains them. They are
**not** blessed: `extensions.sh` appends two delete rules to a
`normalize_modified.sed` (the hook `bin/diff` provides for running against an
existing cluster), so they are filtered from both sides. Blessing them would
freeze one run's LSN into an expected file and fail on the next.

## Not blessed: internal connection strings

documentdb logs the conninfo of its internal connections at DEBUG
(`executing "..." via connection to "host=localhost port=58050 ... dbname=regression ..."`),
which names upstream's temp instance in the expected files and the live compute's
port and `contrib_regression` here. The port changes per run, so `extensions.sh`
normalizes port and dbname on both sides instead of blessing it.

## Not blessed: bson output format

Everything printing as `BSONHEX<hex>` instead of extended JSON is **not** an amp
divergence — it means `documentdb_core.bsonUseEJson` is off. Upstream sets it in
`src/test/regress/regression_opts.conf` via `--temp-config`, which the pgxs
driver strips; the manifest's `regress.pg_settings` carries it instead. Never bless
a hex-rendered expected file; fix the setting.

The suite runs upstream's full `basic_schedule` (104 tests), so further blessings
belong here — most likely around the geospatial operators, whose GIST index types
amp cannot serve because postgis is not shipped. Bless only what is a genuine amp
divergence: a test failing because a feature is absent by design is a candidate, a
test failing because it found a bug is not.
