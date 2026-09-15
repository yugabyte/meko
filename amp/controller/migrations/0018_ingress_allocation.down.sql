-- Reverse 0018.
--
-- A real drop, like `0012`'s: this migration is the definition of both tables
-- and all three types — no earlier migration creates them — so dropping them
-- cannot destroy another migration's work, and the indexes go with the tables.
--
-- Every ingress allocation goes with them, which is what a downgrade to a
-- controller that cannot serve the ingress modes means: those endpoints keep
-- running and keep answering on their shard, but the control plane no longer
-- knows which port belongs to whom. Re-applying 0018 starts from an empty
-- allocation table, so a downgrade past this point is a one-way door for
-- addresses already issued — reissue them rather than assuming they survived.

DROP TABLE IF EXISTS amp_registry.ingress_allocations;
DROP TABLE IF EXISTS amp_registry.ingress_shards;

DROP TYPE IF EXISTS amp_registry.dns_record_type;
DROP TYPE IF EXISTS amp_registry.ingress_shard_state;
DROP TYPE IF EXISTS amp_registry.ingress_family;
