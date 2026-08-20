# pg_cron amp expected variants

Blessed alternative expected-output for pg_cron's upstream pg_regress suite
(`sql/pg_cron-test.sql` vs `expected/pg_cron-test.out`), overlaid onto the
fetched source's `expected/` dir by
[`../../extensions.sh --regress`](../../extensions.sh).

Name files `<test>_<N>.out` so pg_regress accepts either the upstream golden or
the amp variant. Empty (README only) means no amp-specific diff is blessed.

See [`../README.md`](../README.md) for the blessing workflow.
