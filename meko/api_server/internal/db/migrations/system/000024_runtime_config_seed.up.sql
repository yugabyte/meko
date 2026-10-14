-- Separate migration so YSQL can retry this insert. YSQL cannot retry a
-- statement inside a multi-statement migration (yugabyte-db#21833).
INSERT INTO meko_system.runtime_config (config_key, config_value)
VALUES ('memory_search_min_score', '0.2')
ON CONFLICT (config_key) DO NOTHING;
