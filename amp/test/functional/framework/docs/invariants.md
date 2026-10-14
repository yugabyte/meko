# Invariant-to-test map

One row per guarantee the product makes, and the tests that guard it. Use it two
ways: to find what already covers a behaviour before writing a new test, and to
see where coverage is thin (an empty fault-type or red-proof cell is the
roadmap).

Test names are as `ampqa collect` prints them, prefixed by suite.

Fault types are the `framework/chaos` registry actions (`pod_delete`,
`pod_freeze`, `process_kill`, `process_restart`, `process_stall`,
`node_restart`, `network_partition`, `network_slowdown`, `packet_loss`,
`node_stop`) plus the faults an environment injects outside that registry: disk
full, a namespace resource quota, an unschedulable request, and backing topology
(restart, gflag change, leader stepdown, tablet split, master leader change,
data move, backup/restore, upgrade). `none` means the row runs under no fault.
`node_restart` and `node_stop` appear in no cell below, because nothing
injects them today.

Red-proof run id is the run in which the assertion was shown to fail once. Empty
means nobody has broken this guarantee on purpose yet.

| Invariant | Tests that guard it | Fault types | Time scale | Red-proof run |
|---|---|---|---|---|
| An acknowledged commit is durable | features/pg_transaction, features/pg_restart_compute, features/pg_ddl_survives_restart, features/pg_read_cold_cache, features/amp_compute_restart, features/amp_safekeeper_restart (a cancelled or timed-out commit keeps waiting), features/workloadgen_verify; stress/commit_durability, stress/wal_tail_nemesis, stress/storage_proc_nemesis, stress/bulk_load_kill, stress/amp_nemesis, stress/backing_nemesis, stress/load_sustain, stress/combined_load | none, pod_delete, process_kill, process_restart | seconds to hours | |
| Transactions stay consistent under faults: an external checker (elle append, bank, long-fork, single-key-acid, counter, set) finds no anomaly in the history | stress/jepsen | pod_delete, process_stall, pod_freeze, process_kill, process_restart, network_partition, network_slowdown, packet_loss | hours | |
| Single writer per branch: no second writer, and a fenced writer cannot acknowledge | features/amp_single_writer, features/amp_branch_write_token; stress/endpoint_backing_partition, stress/fence_stall, stress/fence_stall_supervisor | none, network_partition, pod_freeze, process_stall | seconds to minutes | 63dca5d8-d49c-475b-806e-9b2c47e86923 |
| Tenants and branches are isolated: nobody reads another's rows | features/pg_parallel_isolation_alpha, features/pg_parallel_isolation_beta, features/pg_multi_db_workload, features/amp_branch_isolation; stress/multitenant_load; perf/multitenant | none | minutes | |
| A fork at an LSN equals the parent's state at that LSN, and never a torn set | features/amp_fork_at_lsn, features/amp_fork_under_write, features/amp_fork_after_endpoint_create, features/amp_branch_of_branch, features/amp_branch_delete, features/amp_product_health; stress/branch_churn; perf/branch_depth (every read at every depth of a 20-deep chain checks its value) | none | seconds to minutes | |
| GC never removes a page a live branch can read | features/amp_fork_below_gc_cutoff, features/amp_gc_reclaim, features/amp_gc_deep_trim | none | minutes | |
| The delta chain stays bounded by `image_chain_limit`, so a read stays bounded | features/pg_delta_chain (reconstruction only, not the bound), perf/chain_depth_read | none | minutes | 5a527679-d504-417f-80f2-92df0ad07569 |
| Pages served are internally consistent after a fault | the `physical_consistency` final step, in 14 of the 21 stress tests (absent from the two failed-create reclaim tests, stress/backing_unreachable_api, stress/combined_load, stress/load_sustain and the two fence tests) | pod_delete, process_kill, network_partition, disk full, backing topology | minutes to hours | a82101a8-bbad-4fbb-889e-1871ce400d30 |
| A failed or raced create leaves no orphan resource | features/amp_failed_create_reclaim, features/amp_cluster_failed_create_reclaim, features/amp_endpoint_drop_race, features/amp_endpoint_resize_delete_race, features/amp_pool_lifecycle, the seven features/amp_task_redpath_* tests; stress/branch_churn, stress/control_plane_nemesis | none, pod_delete, network_partition, resource quota, unschedulable request | minutes, over an hour for the two reclaim tests (120m and 150m timeouts) | 2760713d |
| Settings reach the compute and survive its restart | features/amp_endpoint_settings, features/amp_endpoint_settings_create, features/amp_settings_no_writer, features/pg_wal_defaults, features/backing_config_contract | none, pod_delete | minutes | |
| An upgrade preserves data and the endpoints keep serving | features/amp_upgrade, features/amp_upgrade_chain, features/amp_endpoint_upgrade_roll, features/amp_endpoint_upgrade_roll_failfast, features/amp_mixed_versions, features/amp_endpoint_rollback; stress/backing_maintenance | none, backing topology | 20m to 150m, and 4h for backing_maintenance | |
| Writes acknowledged before and during an upgrade are all served after it | features/amp_upgrade, features/amp_endpoint_upgrade_roll, features/amp_mixed_versions | none | 40m to 80m | |
| The current release reads what an older one wrote: heap, TOAST, GIN and GiST indexes, forks, delta chains, and GC of what it dropped | features/amp_format_compat | none | up to 75m | |
| A compute under load gets backpressure, not a crash | features/pg_backpressure; perf/scale_connections (tagged `scale`, on demand only) | none | minutes to hours | |
| A task that cannot succeed still reaches a terminal status, naming its real cause | stress/control_plane_nemesis; features/amp_task_redpath_backing_down_mid_create, features/amp_task_redpath_quota, features/amp_task_redpath_unschedulable, features/amp_task_redpath_delete_during_create, features/amp_task_redpath_duplicate_names, features/amp_task_redpath_bad_setting_restart, features/amp_task_redpath_api_backing_unreachable | pod_delete, network_partition, resource quota, unschedulable request | minutes | 2760713d |
| A client sees only retryable errors (57/08) during a control-plane or backing operation, never a 500 and never a hang | stress/backing_unreachable_api, stress/backing_node_partition, stress/endpoint_backing_partition, stress/bulk_load_kill, stress/backing_disk_full, stress/backing_maintenance, stress/backing_backup_restore; features/amp_endpoint_resize_in_place | pod_delete, network_partition, disk full, backing topology | minutes to hours | 9f861466-a631-4968-a0df-c9f812d80412 |
| Storage outlives compute: deleting, pausing or replacing a compute loses nothing | features/amp_endpoint_lifecycle, features/amp_endpoint_pause_resume (skipped outright, meko#518: the routes do not exist), features/amp_compute_restart, features/pg_restart_compute, features/pg_ddl_survives_restart | none, pod_delete | minutes | |
| Postgres semantics hold through amp storage | features/pg_roundtrip, features/pg_mutations, features/pg_index, features/pg_ddl, features/pg_column_ops, features/pg_copy, features/pg_copy_formats, features/pg_savepoints, features/pg_sequences, features/pg_prepared_statements, features/pg_partitioned_tables, features/pg_toast_large_values, features/pg_concurrent_writers, features/pg_gin_search, features/pg_transaction, features/pg_read_cold_cache, features/pg_delta_chain, features/pg_index_build_eviction, features/workloadgen_verify | none | seconds to minutes | |
| A query returns the same rows however it is planned or partitioned (SQLancer's TLP, NoREC, PQS, CERT, DQP and related oracles) | stress/sqlancer | none | hours | |
| A stored page payload round trips byte for byte through the compression path | features/pg_payload_compression_roundtrip, features/pg_payload_compression_envelope, features/pg_payload_compression_saves_space, features/pg_payload_compression_survives_restart | none, pod_delete | minutes | |
| A resource reservation is exactly what was asked for, or the request is refused | features/amp_endpoint_resize, features/amp_endpoint_resize_in_place, features/amp_endpoint_resize_memory_down, features/amp_endpoint_resize_infeasible, features/amp_endpoint_memory_floor, features/amp_endpoint_memory_units | none | minutes | |
| Concurrent operations on one object serialise: one wins, none is lost | features/amp_endpoint_resize_lock, features/amp_endpoint_resize_delete_race, features/amp_endpoint_drop_race, features/amp_pool_lifecycle, features/amp_api_validation, features/amp_extension_pgvector, features/amp_endpoint_upgrade_roll; stress/branch_churn | none | minutes | |
| The REST contract holds: validation, pagination, task ordering | features/amp_api_validation, features/amp_api_pagination, features/amp_task_feed, features/amp_mcp_tools | none | minutes | |
| An endpoint is exposed exactly as requested, never wider | features/amp_endpoint_connectivity | none | minutes | |
| An endpoint's health is its pod's, read live, and never ready while the pod is not | features/amp_endpoint_health, features/amp_documentdb_gateway | none, pod_delete, scale to zero, image pull failure | minutes | |
| Tags and names reach every object of an endpoint's release and survive a redeploy, and only the cluster's tags reach another endpoint | features/amp_resource_tags | none | minutes | |
| The documentdb gateway serves the branch's documents at documentdb_address, verifiable when /ca says so | features/amp_documentdb_gateway | none, pod_delete | minutes | |
| Session history (pgsentinel) never returns a row mixed from two samples | features/pg_active_session_history | none | minutes | |
| An endpoint answers only to the password its writer was created with, and none is stored in the clear | features/amp_endpoint_client_access | none, pod_delete | minutes | |
| A fork inherits its root's password and cannot be taken below where it was set | features/amp_endpoint_client_access | none | minutes | |
| An endpoint's recorded address survives a reschedule and a redeploy | features/amp_endpoint_client_access | pod_delete | minutes | |
| With the endpoint user split, Postgres holds no YugabyteDB credential and cannot change the pageserver's or safekeeper's state, and LFC prewarm still works | features/amp_endpoint_user_split | none, pod_delete | minutes | |
| A cancelled task stops at a step boundary, ends failed as cancelled, and blocks nothing after it | features/amp_task_cancel; stress/branch_churn | none | minutes | |
| Every extension the release commits to is present and works | features/pg_extensions_matrix, features/amp_cluster_engine, features/amp_extension_age, features/amp_extension_hexabridge, features/amp_extension_hexabridge_mssql, features/amp_extension_pg_cron, features/amp_extension_pgvector, features/amp_extension_registry (an operator-registered image) | none, pod_delete | minutes | |
| The shipped deployment is what the release claims: release build, declared RBAC, working metrics | features/amp_release_build, features/amp_byoc_scope, features/amp_otel_pipeline, features/backing_config_contract | none | minutes | |
| A migration lands every source row and never leaks the source password | features/amp_voyager_migration | none | minutes | |
| Sustained real load does not degrade past the gate | features/amp_product_health; stress/combined_load, stress/load_sustain; perf/tpcc, perf/pgbench, perf/sysbench, perf/microbench, perf/lifecycle, perf/multitenant, perf/ops_latency, perf/idle_footprint, perf/branch_depth | none, pod_delete, process_kill, process_restart | minutes to hours | |

## Thin cells worth filling

- No red proof exists for any row except the five recorded above. Every other
  assertion in the table is unproven in the sense the plan means: nobody has
  broken the guarantee on purpose and watched the test fail.
- Single writer per branch is guarded under partition and stall, not under
  pod_delete of the fencing authority.
- GC and delta-chain rows run under no fault at all. A GC forced while a
  pageserver is restarting is uncovered.
- Fork-at-LSN is uncovered under any fault.
- Durability is guarded under pod and process faults only. No test in that row
  fills a disk or moves the backing's topology under a write it later verifies.
- Pausing is in the storage-outlives-compute row on a test that skips outright,
  so that half of the row is unguarded until the pause/resume routes land.
- The amp-side upgrade tests have no fault injection: a controller killed
  mid-roll is uncovered. stress/backing_maintenance moves the backing's
  leadership and splits its tablets under load, but its software_upgrade
  operation is off by default.

## Regression tests that fit no invariant row

Each of these is placed in the table above, but in a row that states a contract
rather than a product invariant. They are listed here for review, not for
deletion.

- `features/amp_release_build`, `features/amp_otel_pipeline`: build and
  deployment hygiene. Nothing about data.
- `features/amp_mcp_tools`, `features/amp_api_pagination`: interface shape.
  They guard the API's ergonomics, not a guarantee about state.
- `features/amp_byoc_scope`: an RBAC assertion about the install, not about
  amp's own behaviour.
- `features/pg_parallel_isolation_beta`: identical to
  `features/pg_parallel_isolation_alpha`. The pair exists to exercise the
  framework's parallel scheduling, so one of the two proves nothing new about
  the product.
- `features/amp_extension_age`: skips itself while the controller catalog
  predates the age entry. A test that can pass by skipping needs a check that
  the skip is still legitimate.
- `features/amp_extension_hexabridge_mssql`, and the `sqlserver` case of
  `features/amp_cluster_engine`: the same, for the hexabridge_mssql entry and
  the `sqlserver` engine.
