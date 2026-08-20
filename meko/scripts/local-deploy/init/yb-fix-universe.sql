-- Repoint the seed universe row that api_server's migration 000001
-- inserts. The default points at an AWS ELB that isn't reachable from
-- a local stack; this rewrite makes datapack provisioning target the
-- host's yugabyted instance.
--
-- Idempotent: safe to run after every start.sh.

UPDATE meko_system.yb_universes
SET
  ysql_hostname = 'host.docker.internal',
  ysql_port = 5433,
  ysql_admin_connection_string = 'host=host.docker.internal port=5433 user=yugabyte password='''' dbname=yugabyte sslmode=disable',
  yb_management_url = 'http://host.docker.internal:15433/',
  yb_universe_name = 'meko-local-universe',
  updated_at = NOW()
WHERE universe_id = 'b1eebc99-9c0b-4ef8-bb6d-6bb9bd380a22';
