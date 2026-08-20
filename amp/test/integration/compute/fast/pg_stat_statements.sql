-- pg_stat_statements is preloaded on every compute (yb_amp_local writes it into
-- postgresql.conf, the endpoint chart into SHARED_PRELOAD_LIBRARIES) -- the half
-- that cannot be added later without restarting the postmaster. Both assertions
-- read only postmaster state, so they say nothing about whether the supervisor's
-- CREATE EXTENSION has landed yet: touching the extension here would race
-- compute_ctl's background task and make this golden flaky.
SELECT setting LIKE '%pg_stat_statements%' AS preloaded
FROM pg_settings WHERE name = 'shared_preload_libraries';

-- The library registers its GUCs from _PG_init, so their presence proves the .so
-- was loaded and not merely named.
SELECT count(*) > 0 AS guc_registered
FROM pg_settings WHERE name LIKE 'pg_stat_statements.%';
