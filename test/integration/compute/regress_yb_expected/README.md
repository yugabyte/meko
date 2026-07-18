# yb-amp expected-output variants for pg_regress

These are alternative expected-output files for the upstream PostgreSQL
regression suite run by [`../pg_regress.sh`](../pg_regress.sh) (see issue
#119), capturing the handful of places where yb-amp legitimately diverges
from stock PostgreSQL by design.

## Why they exist

The suite produces exactly one class of diff against yb-amp: **`CREATE
TABLESPACE` is unsupported** (rejected in `yb_amp_ddl_handler.c`). That diff
surfaces in four tests — `test_setup` (which creates `regress_tblspace` up
front), `tablespace`, `stats`, and `database` — the latter three as direct
cascades. It is a known platform limitation, not a bug.

Rather than edit the upstream goldens (vendored, would churn on every
submodule bump and lie about what upstream expects), we use pg_regress's
own **alternative expected-file** mechanism: for each test it diffs against
`expected/<test>.out`, then tries `expected/<test>_0.out` … `_9.out` and
passes if *any* matches (`pg_regress.c:results_differ`). `pg_regress.sh`
overlays these files into the vendored `expected/` dir at run time and
removes them on exit, so the submodule stays pristine.

File naming: `<test>_<N>.out`, where `N` is an index not already used by an
upstream variant (`tablespace` ships `tablespace_1.out`, so the yb-amp
one is `tablespace_2.out`; the others have no upstream variant, so `_1`).

## Regenerating

After an *intended* change to one of these tests' output (e.g. an upstream
submodule bump, or the tablespace error text changes), regenerate the
affected file from a run's results:

```
test/integration/compute/pg_regress.sh            # run; note the ">>> results under: <OUTDIR>"
cp <OUTDIR>/results/<test>.out test/integration/compute/regress_yb_expected/<test>_<N>.out
```

Before blessing, confirm the only diff vs the upstream golden is the
tablespace limitation (and its deterministic cascades) — not a real
regression:

```
diff compute/vendor/postgres-v18/src/test/regress/expected/<test>.out <OUTDIR>/results/<test>.out
```
