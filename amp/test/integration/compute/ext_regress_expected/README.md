# amp expected-output variants for extension regression suites

These are alternative expected-output files for the **externalized-extension**
regression suites run by [`../extensions.sh --regress`](../extensions.sh) (issue
[#608](https://github.com/yugabyte/meko/issues/608)), capturing the places where
an extension's suite legitimately diverges on an amp compute by design.

## Layout

One subdirectory per extension, named by its `extensions.yaml` `name`:

```
ext_regress_expected/
  pgvector/     overlaid onto the fetched source's  test/expected/
  pg_cron/      overlaid onto the fetched source's  expected/
  hexabridge/   overlaid onto the fetched source's  test/pg/
```

At run time `extensions.sh --regress` re-fetches the extension source (the
`FROM scratch` ImageVolume ships only the built tree, not the tests), then copies
every file under `ext_regress_expected/<name>/` — **except this README** — into
the driver's expected/golden directory before running the suite:

- **pgxs driver** (pgvector, pg_cron): copied into the pg_regress `expected/`
  dir. pg_regress passes a test if the results match `expected/<test>.out` or any
  `expected/<test>_N.out` variant, so a blessed file is a known-good alternative
  and the upstream golden is left untouched.
- **script driver** (hexabridge): copied into `test/pg/`, which the extension's
  own `scripts/regression.sh` already treats as the "known PG divergence from the
  Oracle ground truth" output. A blessed `test/pg/<schema>/<routine>.out` makes
  that routine pass as a known divergence.

An empty subdirectory (README only) means **no amp-specific diff is blessed** —
a clean tree must be a genuine green.

## Blessing a diff

Only after confirming the diff is a **by-design** amp divergence (a platform
limitation like `CREATE TABLESPACE`, not a real regression):

1. Run the suite and let it fail:
   `test/integration/compute/extensions.sh --ext <name> --regress --keep`
2. Inspect the diff (pg_regress writes `regression.diffs`; the hexabridge runner
   prints the diff inline). Confirm it is expected.
3. Copy the produced output into `ext_regress_expected/<name>/`, preserving the
   relative path the overlay target expects (e.g. `pgvector/<test>_1.out`, or
   `hexabridge/<schema>/<routine>.out`).
4. Re-run: the tree must now be green.
