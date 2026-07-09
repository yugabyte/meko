-- 0004 down: drop the endpoint tailnet address column.

ALTER TABLE endpoint DROP COLUMN tailnet_address;
