-- 0009: controller-wide state the controller writes itself, one row per key.
--
-- The first key is `fleet_version`: the version the endpoint fleet was last
-- confirmed at in full (ac-endpoint-upgrade.md §6.1). It is written only by the
-- controller — seeded at startup and advanced by a successful roll — never by a
-- caller, so there is no API that sets a row here.
--
-- A row per key rather than one JSON document so two keys never share a
-- read-modify-write.

CREATE TABLE IF NOT EXISTS amp_registry.controller_settings (
    key        text        PRIMARY KEY,
    value      text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
