-- Reverse 0009. Losing fleet_version is safe: a controller that has the table
-- again reseeds it at startup from the endpoints — their common version, or
-- "mixed" when they differ.

DROP TABLE IF EXISTS amp_registry.controller_settings;
