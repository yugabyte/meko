-- Where a call ran: '' for this Meko, 'cloud' for one /mcp sent on to the linked Meko Cloud. This
-- Meko's metrics, usage and usage stats read only its own calls.
-- Migrations are append-only: never edit one that has shipped.
ALTER TABLE call_log ADD COLUMN location TEXT NOT NULL DEFAULT '';
