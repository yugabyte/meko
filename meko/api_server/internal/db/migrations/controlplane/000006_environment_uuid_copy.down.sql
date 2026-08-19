-- Undoes 000005: copy the rows back into the pre-uuid tables 000007's down created.
-- Rows only, no schema changes (see 000005's header).
--
-- Fails here on a duplicate id if two accounts hold the same environment name, which
-- PRIMARY KEY (id) cannot represent. That is deliberate: the alternative is silently
-- dropping one account's environment.
INSERT INTO meko_system.environments_old (
    id, account_id, placement, region, state, k8s_version, node_count, provider,
    datapacks, max_datapacks, agent_version, last_heartbeat_at, mcp_base_url,
    cert_fingerprint, components, kind, resources, live,
    enroll_token_hash, enroll_token_expires_at, status_token_hash, created_at)
SELECT
    id, NULLIF(account_id, ''), placement, region, state, k8s_version, node_count, provider,
    datapacks, max_datapacks, agent_version, last_heartbeat_at, mcp_base_url,
    cert_fingerprint, components, kind, resources, live,
    enroll_token_hash, enroll_token_expires_at, status_token_hash, created_at
FROM meko_system.environments_new;

INSERT INTO meko_system.environment_desired_resources_old (env_id, kind, name, spec, generation)
SELECT e.id, d.kind, d.name, d.spec, d.generation
FROM meko_system.environment_desired_resources_new d
JOIN meko_system.environments_new e ON e.uuid = d.env_uuid;
