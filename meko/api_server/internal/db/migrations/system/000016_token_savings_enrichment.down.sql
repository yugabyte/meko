ALTER TABLE meko_system.token_savings_summary
  DROP COLUMN IF EXISTS tokens_if_raw_total,
  DROP COLUMN IF EXISTS score_high_total,
  DROP COLUMN IF EXISTS score_mid_total,
  DROP COLUMN IF EXISTS score_low_total;
