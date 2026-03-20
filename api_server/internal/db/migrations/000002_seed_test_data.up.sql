INSERT INTO meko_system.account_registry (id, account_first_name, account_last_name, account_email, account_type)
VALUES (
    'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11',
    'Meko',
    'Admin',
    'admin@meko.dev',
    'enterprise'
) ON CONFLICT (id) DO NOTHING;

INSERT INTO meko_system.yba_universe_registry (id, yb_universe_uuid, yb_universe_name, ysql_hostname, ysql_port, ysql_admin_connection_string, yba_management_url)
VALUES (
    'b1eebc99-9c0b-4ef8-bb6d-6bb9bd380a22',
    'c2eebc99-9c0b-4ef8-bb6d-6bb9bd380a33',
    'meko-dev-universe',
    'a2150d65ed96440809a5530c9fa0ed4a-389989580.us-east-2.elb.amazonaws.com',
    5433,
    'postgresql://yugabyte:Meko%40123@a2150d65ed96440809a5530c9fa0ed4a-389989580.us-east-2.elb.amazonaws.com:5433/yugabyte?sslmode=disable',
    'http://a0ea6ee2d58194693870037cb841bdbe-1826583179.us-east-2.elb.amazonaws.com'
) ON CONFLICT (id) DO NOTHING;
