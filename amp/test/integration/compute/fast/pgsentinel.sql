-- Reads only install and postmaster state, so it never races compute_ctl's
-- background CREATE EXTENSION.

-- The control file is the half a cached-but-stale pg_install would be missing.
SELECT count(*) = 1 AS available
FROM pg_available_extensions WHERE name = 'pgsentinel';

-- A name in shared_preload_libraries whose library is missing stops the
-- postmaster, so a running one that lists it has loaded it.
SELECT 'pgsentinel' = ANY(regexp_split_to_array(
    current_setting('shared_preload_libraries'), '\s*,\s*')) AS preloaded;
