-- 0008: retire the controller's outbound YCP loop. The controller no longer
-- enrolls with or reports to the control plane — the standalone reconcile-agent
-- is the sole YCP client for the environment now, driving the controller over its
-- REST API and pinning itself via the 0007 binding (see ac-ycp.md). The durable
-- enrollment identity from 0004 (env_name -> status_token) therefore has no writer
-- or reader left; drop it.
DROP TABLE IF EXISTS ycp_identity;
