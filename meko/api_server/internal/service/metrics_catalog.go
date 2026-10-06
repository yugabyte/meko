package service

// Catalog of the PromQL behind each metric the environment metrics API serves.
//
// The names and the expressions are the ones the YCP console's environment
// metrics tab already declares (yugabyte-cloud
// ui/src/ycp/environments/metrics/YcpMetricsChartConfig.ts), which in turn
// transcribes the Grafana amp dashboards — Controller, Pod resources, Postgres,
// Storage layer. Keeping the two in step is what lets a chart be added on either
// side without re-deriving the query.
//
// Not served here: the two "Endpoint logs" charts. They are LogsQL against
// VictoriaLogs, not PromQL against a Prometheus API, so they need a different
// read path entirely; their names are absent and therefore rejected as unknown.
//
// Placeholders, substituted by buildQuery:
//
//	$FLEET       the tenancy matchers — environment_id, account_id, source
//	$ENDPOINT    endpoint-name regex, ".*" when the caller filters on nothing
//	$POD         pod-name regex, ".*" when the caller filters on nothing
//
// Every expression must scope every one of its selectors with $FLEET. A selector
// that forgets it reads the whole fleet's series, which is a cross-account leak,
// not a cosmetic bug — TestCatalog_ScopesEverySelector guards it.
var metricsCatalog = map[string]string{
	// ── Controller: the Go amp controller's own instrumentation ────────────────
	"AMP_CONTROLLER_TASKS_INFLIGHT": `amp_controller_tasks_inflight{$FLEET}`,
	"AMP_CONTROLLER_HTTP_REQUESTS_PER_SEC": `sum(rate(` +
		`amp_controller_http_requests_total{$FLEET}[5m]))`,
	"AMP_CONTROLLER_TASKS_SUCCEEDED_PER_SEC": `sum(rate(` +
		`amp_controller_tasks_total{$FLEET,status="succeeded"}[5m]))`,
	"AMP_CONTROLLER_HTTP_REQUESTS_BY_ROUTE": `sum by (route) (rate(` +
		`amp_controller_http_requests_total{$FLEET}[5m]))`,
	"AMP_CONTROLLER_HTTP_REQUESTS_BY_STATUS": `sum by (status) (rate(` +
		`amp_controller_http_requests_total{$FLEET}[5m]))`,
	"AMP_CONTROLLER_HTTP_LATENCY_P50_SEC": `histogram_quantile(0.5, sum by (le) (rate(` +
		`amp_controller_http_request_duration_seconds_bucket{$FLEET}[5m])))`,
	"AMP_CONTROLLER_HTTP_LATENCY_P95_SEC": `histogram_quantile(0.95, sum by (le) (rate(` +
		`amp_controller_http_request_duration_seconds_bucket{$FLEET}[5m])))`,
	"AMP_CONTROLLER_HTTP_LATENCY_P99_SEC": `histogram_quantile(0.99, sum by (le) (rate(` +
		`amp_controller_http_request_duration_seconds_bucket{$FLEET}[5m])))`,
	"AMP_CONTROLLER_TASKS_BY_STATUS": `sum by (status) (rate(` +
		`amp_controller_tasks_total{$FLEET}[5m]))`,
	"AMP_CONTROLLER_TASK_DURATION_P95_SEC": `histogram_quantile(0.95, sum by (le,kind) (rate(` +
		`amp_controller_tasks_duration_seconds_bucket{$FLEET}[5m])))`,

	// ── Meko api-server: its own instrumentation (internal/metrics) ───────────
	"MEKO_API_HTTP_REQUESTS_PER_SEC": `sum(rate(` +
		`meko_api_http_requests_total{$FLEET}[5m]))`,
	"MEKO_API_HTTP_REQUESTS_BY_ROUTE": `sum by (route) (rate(` +
		`meko_api_http_requests_total{$FLEET}[5m]))`,
	"MEKO_API_HTTP_REQUESTS_BY_STATUS": `sum by (status) (rate(` +
		`meko_api_http_requests_total{$FLEET}[5m]))`,
	"MEKO_API_HTTP_LATENCY_P50_SEC": `histogram_quantile(0.5, sum by (le) (rate(` +
		`meko_api_http_request_duration_seconds_bucket{$FLEET}[5m])))`,
	"MEKO_API_HTTP_LATENCY_P95_SEC": `histogram_quantile(0.95, sum by (le) (rate(` +
		`meko_api_http_request_duration_seconds_bucket{$FLEET}[5m])))`,
	"MEKO_API_HTTP_LATENCY_P99_SEC": `histogram_quantile(0.99, sum by (le) (rate(` +
		`meko_api_http_request_duration_seconds_bucket{$FLEET}[5m])))`,
	"MEKO_API_DB_OPEN_CONNECTIONS": `sum by (db_name) (` +
		`go_sql_open_connections{$FLEET})`,

	// ── Meko MCP server: tool calls (meko-mcp-server src/metrics.py) ──────────
	"MEKO_MCP_TOOL_CALLS_BY_TOOL": `sum by (tool) (rate(` +
		`meko_mcp_tool_calls_total{$FLEET}[5m]))`,
	"MEKO_MCP_TOOL_ERRORS_BY_TOOL": `sum by (tool) (rate(` +
		`meko_mcp_tool_calls_total{$FLEET,outcome="error"}[5m]))`,
	"MEKO_MCP_TOOL_LATENCY_P95_SEC": `histogram_quantile(0.95, sum by (le,tool) (rate(` +
		`meko_mcp_tool_call_duration_seconds_bucket{$FLEET}[5m])))`,

	// ── Meko inference gateway: chat turns and LLM calls ──────────────────────
	"MEKO_GATEWAY_HTTP_REQUESTS_BY_ROUTE": `sum by (route) (rate(` +
		`meko_gateway_http_requests_total{$FLEET}[5m]))`,
	"MEKO_GATEWAY_CHAT_LATENCY_P95_SEC": `histogram_quantile(0.95, sum by (le) (rate(` +
		`meko_gateway_http_request_duration_seconds_bucket{$FLEET,route="/chat"}[5m])))`,
	"MEKO_GATEWAY_LLM_LATENCY_P95_SEC": `histogram_quantile(0.95, sum by (le,model) (rate(` +
		`meko_gateway_llm_request_duration_seconds_bucket{$FLEET}[5m])))`,
	"MEKO_GATEWAY_LLM_ERRORS_PER_SEC": `sum by (model) (rate(` +
		`meko_gateway_llm_requests_total{$FLEET,outcome="error"}[5m]))`,
	"MEKO_GATEWAY_LLM_TOKENS_PER_SEC": `sum by (model,direction) (rate(` +
		`meko_gateway_llm_tokens_total{$FLEET}[5m]))`,

	// ── Reconcile agent (ycp/cmd/reconcile-agent/metrics.go) ─────────────────
	"YCP_AGENT_SECONDS_SINCE_LAST_TICK": `time() - max(` +
		`ycp_agent_last_successful_tick_timestamp_seconds{$FLEET})`,
	"YCP_AGENT_RESOURCES_BY_CONDITION": `sum by (condition) (` +
		`ycp_agent_resources{$FLEET})`,
	"YCP_AGENT_TICK_ERRORS_PER_SEC": `sum(rate(` +
		`ycp_agent_ticks_total{$FLEET,outcome="error"}[5m]))`,
	"YCP_AGENT_STATUS_REPORT_ERRORS_PER_SEC": `sum(rate(` +
		`ycp_agent_status_reports_total{$FLEET,outcome="error"}[5m]))`,

	// ── Knowledge-base ingestion, as the api-server reads it from dist_rag ────
	"MEKO_RAG_DOCUMENTS_BY_STATUS": `max by (status) (` +
		`meko_api_rag_documents{$FLEET})`,

	// ── Admin business metrics the api-server caches from the admin repo ───────
	// Current totals, not rates: window by date in PromQL. Every replica exports
	// the same DB-wide total, so collapse replicas with max (never sum, which
	// would multiply by the replica count) — as MEKO_RAG_DOCUMENTS_BY_STATUS does.
	"MEKO_ADMIN_USERS_BY_TIER": `max by (tier) (` +
		`meko_api_admin_users{$FLEET})`,
	"MEKO_ADMIN_DATAPACKS": `max(` +
		`meko_api_admin_datapacks{$FLEET})`,
	"MEKO_ADMIN_MEMORIES_BY_KIND": `max by (kind) (` +
		`meko_api_admin_memories{$FLEET})`,
	"MEKO_ADMIN_TOKEN_SAVINGS_REQUESTS": `max(` +
		`meko_api_admin_token_savings_requests{$FLEET})`,
	"MEKO_ADMIN_TOKEN_SAVINGS_TOKENS": `max(` +
		`meko_api_admin_token_savings_tokens{$FLEET})`,
	"MEKO_ADMIN_KB_DOCUMENTS": `max(` +
		`meko_api_admin_kb_documents{$FLEET})`,
	"MEKO_ADMIN_KB_CHUNKS": `max(` +
		`meko_api_admin_kb_chunks{$FLEET})`,
	"MEKO_ADMIN_KB_DOCUMENTS_BY_TYPE": `max by (type) (` +
		`meko_api_admin_kb_documents_by_type{$FLEET})`,

	// ── Pod resources: cAdvisor, via the compute collector's kubelet job ───────
	// Kind-agnostic: a meko environment's collector runs the same job, and $FLEET's
	// source matcher keeps the two apart.
	// container!="" drops the per-pod cgroup rollup, which would double-count.
	"AMP_PODS_TRACKED": `count(count by (pod) (` +
		`container_memory_working_set_bytes{$FLEET,container!=""}))`,
	"AMP_PODS_TOTAL_MEMORY_BYTES": `sum(` +
		`container_memory_working_set_bytes{$FLEET,container!=""})`,
	"AMP_PODS_TOTAL_CPU_CORES": `sum(rate(` +
		`container_cpu_usage_seconds_total{$FLEET,container!=""}[5m]))`,
	"AMP_PODS_MEMORY_BY_POD_BYTES": `sum by (pod) (` +
		`container_memory_working_set_bytes{$FLEET,container!="",pod=~"$POD"})`,
	"AMP_PODS_CPU_BY_POD_CORES": `sum by (pod) (rate(` +
		`container_cpu_usage_seconds_total{$FLEET,container!="",pod=~"$POD"}[5m]))`,
	"AMP_PODS_NETWORK_RECEIVE_BYTES_PER_SEC": `sum by (pod) (rate(` +
		`container_network_receive_bytes_total{$FLEET,pod=~"$POD"}[5m]))`,
	"AMP_PODS_NETWORK_TRANSMIT_BYTES_PER_SEC": `sum by (pod) (rate(` +
		`container_network_transmit_bytes_total{$FLEET,pod=~"$POD"}[5m]))`,

	// ── Postgres: the postgres_exporter sidecar in each endpoint pod ───────────
	// The exporter's counters carry the _total suffix (ac-metrics.md step 3).
	"AMP_POSTGRES_UP": `pg_up{$FLEET,endpoint=~"$ENDPOINT"}`,
	"AMP_POSTGRES_CONNECTIONS": `sum(` +
		`pg_stat_database_numbackends{$FLEET,endpoint=~"$ENDPOINT"})`,
	"AMP_POSTGRES_CACHE_HIT_RATIO_PERCENT": `100 * sum(rate(` +
		`pg_stat_database_blks_hit_total{$FLEET,endpoint=~"$ENDPOINT"}[5m])) / clamp_min(sum(rate(` +
		`pg_stat_database_blks_hit_total{$FLEET,endpoint=~"$ENDPOINT"}[5m])) + sum(rate(` +
		`pg_stat_database_blks_read_total{$FLEET,endpoint=~"$ENDPOINT"}[5m])), 1)`,
	"AMP_POSTGRES_XACT_COMMIT_PER_SEC": `sum by (endpoint) (rate(` +
		`pg_stat_database_xact_commit_total{$FLEET,endpoint=~"$ENDPOINT"}[5m]))`,
	"AMP_POSTGRES_XACT_ROLLBACK_PER_SEC": `sum by (endpoint) (rate(` +
		`pg_stat_database_xact_rollback_total{$FLEET,endpoint=~"$ENDPOINT"}[5m]))`,
	"AMP_POSTGRES_TUP_FETCHED_PER_SEC": `sum(rate(` +
		`pg_stat_database_tup_fetched_total{$FLEET,endpoint=~"$ENDPOINT"}[5m]))`,
	"AMP_POSTGRES_TUP_RETURNED_PER_SEC": `sum(rate(` +
		`pg_stat_database_tup_returned_total{$FLEET,endpoint=~"$ENDPOINT"}[5m]))`,
	"AMP_POSTGRES_TUP_INSERTED_PER_SEC": `sum(rate(` +
		`pg_stat_database_tup_inserted_total{$FLEET,endpoint=~"$ENDPOINT"}[5m]))`,
	"AMP_POSTGRES_TUP_UPDATED_PER_SEC": `sum(rate(` +
		`pg_stat_database_tup_updated_total{$FLEET,endpoint=~"$ENDPOINT"}[5m]))`,
	"AMP_POSTGRES_TUP_DELETED_PER_SEC": `sum(rate(` +
		`pg_stat_database_tup_deleted_total{$FLEET,endpoint=~"$ENDPOINT"}[5m]))`,
	"AMP_POSTGRES_BLK_READ_TIME_MS_PER_SEC": `sum(rate(` +
		`pg_stat_database_blk_read_time_total{$FLEET,endpoint=~"$ENDPOINT"}[5m]))`,
	"AMP_POSTGRES_BLK_WRITE_TIME_MS_PER_SEC": `sum(rate(` +
		`pg_stat_database_blk_write_time_total{$FLEET,endpoint=~"$ENDPOINT"}[5m]))`,
	"AMP_POSTGRES_DEADLOCKS_PER_SEC": `sum(rate(` +
		`pg_stat_database_deadlocks_total{$FLEET,endpoint=~"$ENDPOINT"}[5m]))`,
	"AMP_POSTGRES_TEMP_BYTES_PER_SEC": `sum(rate(` +
		`pg_stat_database_temp_bytes_total{$FLEET,endpoint=~"$ENDPOINT"}[5m]))`,

	// ── Storage layer: the Rust storage controller, safekeeper, pageserver ─────
	"AMP_STORAGE_CONTROLLER_BRANCHES": `max(storage_controller_branches{$FLEET})`,
	"AMP_STORAGE_PAGESERVER_NODES":    `max(storage_controller_pageserver_nodes{$FLEET})`,
	"AMP_STORAGE_SAFEKEEPER_ACTIVE_TIMELINES": `sum(` +
		`safekeeper_active_timelines{$FLEET,endpoint=~"$ENDPOINT"})`,
	"AMP_STORAGE_SAFEKEEPER_CONNECTED_COMPUTES": `sum(` +
		`safekeeper_connected_computes{$FLEET,endpoint=~"$ENDPOINT"})`,
	"AMP_STORAGE_CONTROLLER_HTTP_BY_STATUS": `sum by (status) (rate(` +
		`storage_controller_http_request_status_total{$FLEET}[5m]))`,
	"AMP_STORAGE_CONTROLLER_DB_LATENCY_P95_SEC": `histogram_quantile(0.95, sum by (le) (rate(` +
		`storage_controller_database_query_latency_bucket{$FLEET}[5m])))`,
	"AMP_STORAGE_SAFEKEEPER_WAL_FLUSH_BYTES_PER_SEC": `sum by (endpoint) (rate(` +
		`safekeeper_flush_lsn{$FLEET,endpoint=~"$ENDPOINT"}[5m]))`,
	"AMP_STORAGE_SAFEKEEPER_DISK_USAGE_BYTES": `sum by (endpoint) (` +
		`safekeeper_disk_usage_bytes{$FLEET,endpoint=~"$ENDPOINT"})`,
	"AMP_STORAGE_PAGESERVER_ACTIVE_OPS": `sum by (endpoint) (` +
		`pageserver_active_storage_operations_count{$FLEET,endpoint=~"$ENDPOINT"})`,
	"AMP_STORAGE_COMPUTE_PG_DOWNTIME_MS": `sum by (endpoint) (` +
		`compute_pg_downtime_ms_total{$FLEET,endpoint=~"$ENDPOINT"})`,
}
