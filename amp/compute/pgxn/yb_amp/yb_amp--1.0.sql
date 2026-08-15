\echo Use "CREATE EXTENSION yb_amp" to load this file. \quit

CREATE FUNCTION pg_cluster_size()
RETURNS bigint
AS 'MODULE_PATHNAME', 'pg_cluster_size'
LANGUAGE C STRICT
PARALLEL UNSAFE;

CREATE FUNCTION backpressure_lsns(
    OUT received_lsn pg_lsn,
    OUT consistent_lsn pg_lsn
)
RETURNS record
AS 'MODULE_PATHNAME', 'backpressure_lsns'
LANGUAGE C STRICT
PARALLEL UNSAFE;

CREATE FUNCTION backpressure_throttling_time()
RETURNS bigint
AS 'MODULE_PATHNAME', 'backpressure_throttling_time'
LANGUAGE C STRICT
PARALLEL UNSAFE;

CREATE FUNCTION local_cache_pages()
RETURNS SETOF RECORD
AS 'MODULE_PATHNAME', 'local_cache_pages'
LANGUAGE C PARALLEL SAFE;

-- Create a view for convenient access.
CREATE VIEW local_cache AS
	SELECT P.* FROM local_cache_pages() AS P
	(pageoffs int8, relfilenode oid, reltablespace oid, reldatabase oid,
	 relforknumber int2, relblocknumber int8, accesscount int4);

CREATE FUNCTION yb_amp_get_lfc_stats()
RETURNS SETOF RECORD
AS 'MODULE_PATHNAME', 'yb_amp_get_lfc_stats'
LANGUAGE C PARALLEL SAFE;

-- Create a view for convenient access.
CREATE VIEW yb_amp_lfc_stats AS
	SELECT P.* FROM yb_amp_get_lfc_stats() AS P (lfc_key text, lfc_value bigint);

-- Create a convenient view similar to pg_stat_database
-- that exposes all lfc stat values in one row.
CREATE OR REPLACE VIEW YBAMP_STAT_FILE_CACHE AS
   WITH lfc_stats AS (
   SELECT
     stat_name,
     count
   FROM yb_amp_get_lfc_stats() AS t(stat_name text, count bigint)
   ),
   lfc_values AS (
   SELECT
     MAX(CASE WHEN stat_name = 'file_cache_misses' THEN count ELSE NULL END) AS file_cache_misses,
     MAX(CASE WHEN stat_name = 'file_cache_hits'   THEN count ELSE NULL END) AS file_cache_hits,
     MAX(CASE WHEN stat_name = 'file_cache_used'   THEN count ELSE NULL END) AS file_cache_used,
     MAX(CASE WHEN stat_name = 'file_cache_writes' THEN count ELSE NULL END) AS file_cache_writes,
     -- Calculate the file_cache_hit_ratio within the same CTE for simplicity
     CASE
        WHEN MAX(CASE WHEN stat_name = 'file_cache_misses' THEN count ELSE 0 END) + MAX(CASE WHEN stat_name = 'file_cache_hits' THEN count ELSE 0 END) = 0 THEN NULL
        ELSE ROUND((MAX(CASE WHEN stat_name = 'file_cache_hits' THEN count ELSE 0 END)::DECIMAL /
        (MAX(CASE WHEN stat_name = 'file_cache_hits' THEN count ELSE 0 END) + MAX(CASE WHEN stat_name = 'file_cache_misses' THEN count ELSE 0 END))) * 100, 2)
     END AS file_cache_hit_ratio
   FROM lfc_stats
   )
SELECT file_cache_misses, file_cache_hits, file_cache_used, file_cache_writes, file_cache_hit_ratio from lfc_values;

-- externalize the view to all users in role pg_monitor
GRANT SELECT ON YBAMP_STAT_FILE_CACHE TO PG_MONITOR;

CREATE FUNCTION approximate_working_set_size(reset bool)
RETURNS integer
AS 'MODULE_PATHNAME', 'approximate_working_set_size'
LANGUAGE C PARALLEL SAFE;

GRANT EXECUTE ON FUNCTION approximate_working_set_size(bool) TO pg_monitor;

CREATE FUNCTION approximate_working_set_size_seconds(duration integer default null)
RETURNS integer
AS 'MODULE_PATHNAME', 'approximate_working_set_size_seconds'
LANGUAGE C PARALLEL SAFE;

GRANT EXECUTE ON FUNCTION approximate_working_set_size_seconds(integer) TO pg_monitor;

CREATE FUNCTION get_backend_perf_counters()
RETURNS SETOF RECORD
AS 'MODULE_PATHNAME', 'yb_amp_get_backend_perf_counters'
LANGUAGE C PARALLEL SAFE;

CREATE FUNCTION get_perf_counters()
RETURNS SETOF RECORD
AS 'MODULE_PATHNAME', 'yb_amp_get_perf_counters'
LANGUAGE C PARALLEL SAFE;

-- Show various metrics, for each backend. Note that the values are not reset
-- when a backend exits. When a new backend starts with the backend ID, it will
-- continue accumulating the values from where the old backend left. If you are
-- only interested in the changes from your own session, store the values at the
-- beginning of the session somewhere, and subtract them on subsequent calls.
--
-- For histograms, 'bucket_le' is the upper bound of the histogram bucket.
CREATE VIEW yb_amp_backend_perf_counters AS
  SELECT P.procno, P.pid, P.metric, P.bucket_le, P.value
  FROM get_backend_perf_counters() AS P (
    procno integer,
    pid integer,
    metric text,
    bucket_le float8,
    value float8
  );

-- Summary across all backends. (This could also be implemented with
-- an aggregate query over yb_amp_backend_perf_counters view.)
CREATE VIEW yb_amp_perf_counters AS
  SELECT P.metric, P.bucket_le, P.value
  FROM get_perf_counters() AS P (
    metric text,
    bucket_le float8,
    value float8
  );

CREATE FUNCTION get_prewarm_info(out total_pages integer, out prewarmed_pages integer, out skipped_pages integer, out active_workers integer)
RETURNS record
AS 'MODULE_PATHNAME', 'get_prewarm_info'
LANGUAGE C STRICT
PARALLEL SAFE;

CREATE FUNCTION get_local_cache_state(max_chunks integer default null)
RETURNS bytea
AS 'MODULE_PATHNAME', 'get_local_cache_state'
LANGUAGE C
PARALLEL UNSAFE;

CREATE FUNCTION prewarm_local_cache(state bytea, n_workers integer default 1)
RETURNS void
AS 'MODULE_PATHNAME', 'prewarm_local_cache'
LANGUAGE C STRICT
PARALLEL UNSAFE;
