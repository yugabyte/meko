# pgvector amp expected variants

Blessed alternative expected-output for pgvector's upstream pg_regress suite
(`test/sql/*.sql` vs `test/expected/*.out`), overlaid onto the fetched source's
`test/expected/` dir by [`../../extensions.sh --regress`](../../extensions.sh).

Name files `<test>_<N>.out` (an alternative index not already used upstream), so
pg_regress accepts either the upstream golden or the amp variant. Empty (README
only) means no amp-specific diff is blessed — a clean run is green as-is.

See [`../README.md`](../README.md) for the blessing workflow.
