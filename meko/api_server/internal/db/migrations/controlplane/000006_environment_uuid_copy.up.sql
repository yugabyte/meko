-- Step 2 of 3: copy rows into the tables 000005 created. Rows only -- no schema
-- changes here, which is the whole point of the split (see 000005's header).
INSERT INTO meko_system.environments_new (
    id, account_id, placement, region, state, k8s_version, node_count, provider,
    datapacks, max_datapacks, agent_version, last_heartbeat_at, mcp_base_url,
    cert_fingerprint, components, kind, resources, live,
    enroll_token_hash, enroll_token_expires_at, status_token_hash, created_at)
SELECT
    id, COALESCE(account_id, ''), placement, region, state, k8s_version, node_count, provider,
    datapacks, max_datapacks, agent_version, last_heartbeat_at, mcp_base_url,
    cert_fingerprint, components, COALESCE(kind, 'meko'), resources, live,
    enroll_token_hash, enroll_token_expires_at, status_token_hash, created_at
FROM meko_system.environments;

-- Resolved through the new table, which carries both uuid and id, so the join is
-- unambiguous while the old ids are still globally unique. Orphans (an env_id naming
-- no environment) are dropped rather than carried: nothing could read them, since
-- every lookup starts from an environment.
INSERT INTO meko_system.environment_desired_resources_new (env_uuid, kind, name, spec, generation)
SELECT e.uuid, d.kind, d.name, d.spec, d.generation
FROM meko_system.environment_desired_resources d
JOIN meko_system.environments_new e ON e.id = d.env_id;
