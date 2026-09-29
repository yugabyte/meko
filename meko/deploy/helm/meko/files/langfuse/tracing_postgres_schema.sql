-- Tracing Postgres schema (tables + partitioning + optional corresponding indexes)
-- Scope: traces, observations, scores

BEGIN;

-- Non-destructive schema init: keep existing data/tables.
-- Enum types used by the tracing tables.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'observation_level') THEN
    CREATE TYPE observation_level AS ENUM ('ERROR', 'WARNING', 'DEFAULT', 'DEBUG');
  END IF;
END $$;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'observation_type') THEN
    CREATE TYPE observation_type AS ENUM (
      'SPAN',
      'GENERATION',
      'EVENT',
      'AGENT',
      'TOOL',
      'CHAIN',
      'RETRIEVER',
      'EVALUATOR',
      'EMBEDDING',
      'GUARDRAIL'
    );
  END IF;
END $$;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'score_data_type') THEN
    CREATE TYPE score_data_type AS ENUM ('NUMERIC', 'BOOLEAN', 'CATEGORICAL');
  END IF;
END $$;

ALTER TYPE observation_type ADD VALUE IF NOT EXISTS 'SPAN';
ALTER TYPE observation_type ADD VALUE IF NOT EXISTS 'GENERATION';
ALTER TYPE observation_type ADD VALUE IF NOT EXISTS 'EVENT';
ALTER TYPE observation_type ADD VALUE IF NOT EXISTS 'AGENT';
ALTER TYPE observation_type ADD VALUE IF NOT EXISTS 'TOOL';
ALTER TYPE observation_type ADD VALUE IF NOT EXISTS 'CHAIN';
ALTER TYPE observation_type ADD VALUE IF NOT EXISTS 'RETRIEVER';
ALTER TYPE observation_type ADD VALUE IF NOT EXISTS 'EVALUATOR';
ALTER TYPE observation_type ADD VALUE IF NOT EXISTS 'EMBEDDING';
ALTER TYPE observation_type ADD VALUE IF NOT EXISTS 'GUARDRAIL';

ALTER TYPE score_data_type ADD VALUE IF NOT EXISTS 'NUMERIC';
ALTER TYPE score_data_type ADD VALUE IF NOT EXISTS 'BOOLEAN';
ALTER TYPE score_data_type ADD VALUE IF NOT EXISTS 'CATEGORICAL';

-- IMPORTANT:
-- For partitioned tables, primary/unique constraints must include the partition key.

-- 1) traces
-- ClickHouse parity: PARTITION BY toYYYYMM(timestamp), ORDER BY (project_id, toDate(timestamp), id)
CREATE TABLE IF NOT EXISTS traces (
  id           TEXT        NOT NULL,
  project_id   TEXT        NOT NULL,
  "timestamp"  TIMESTAMPTZ NOT NULL,
  name         TEXT,
  user_id      TEXT,
  session_id   TEXT,
  environment  TEXT,
  public       BOOLEAN     NOT NULL DEFAULT FALSE,
  bookmarked   BOOLEAN     NOT NULL DEFAULT FALSE,
  tags         TEXT[]      NOT NULL DEFAULT '{}',
  input        TEXT,
  output       TEXT,
  metadata     JSONB       NOT NULL DEFAULT '{}'::jsonb,
  release      TEXT,
  version      TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  event_ts     TIMESTAMPTZ NOT NULL DEFAULT now(),
  is_deleted   BOOLEAN     NOT NULL DEFAULT FALSE,
  PRIMARY KEY (project_id, id, "timestamp")
) PARTITION BY RANGE ("timestamp");

-- 2) observations
-- ClickHouse parity: PARTITION BY toYYYYMM(start_time), ORDER BY (project_id, toDate(start_time), id)
CREATE TABLE IF NOT EXISTS observations (
  id                      TEXT              NOT NULL,
  project_id              TEXT              NOT NULL,
  trace_id                TEXT              NOT NULL,
  parent_observation_id   TEXT,
  environment             TEXT,
  type                    observation_type  NOT NULL,
  name                    TEXT              NOT NULL,
  start_time              TIMESTAMPTZ       NOT NULL,
  end_time                TIMESTAMPTZ,
  level                   observation_level NOT NULL DEFAULT 'DEFAULT',
  status_message          TEXT,
  version                 TEXT,
  input                   TEXT,
  output                  TEXT,
  metadata                JSONB             NOT NULL DEFAULT '{}'::jsonb,
  provided_model_name     TEXT,
  internal_model_id       TEXT,
  model_parameters        JSONB,
  provided_usage_details  JSONB             NOT NULL DEFAULT '{}'::jsonb,
  usage_details           JSONB             NOT NULL DEFAULT '{}'::jsonb,
  provided_cost_details   JSONB             NOT NULL DEFAULT '{}'::jsonb,
  cost_details            JSONB             NOT NULL DEFAULT '{}'::jsonb,
  total_cost              NUMERIC(18,12),
  usage_pricing_tier_id   TEXT,
  usage_pricing_tier_name TEXT,
  completion_start_time   TIMESTAMPTZ,
  prompt_id               TEXT,
  prompt_name             TEXT,
  prompt_version          INTEGER,
  tool_definitions        JSONB             NOT NULL DEFAULT '{}'::jsonb,
  tool_calls              JSONB             NOT NULL DEFAULT '[]'::jsonb,
  tool_call_names         TEXT[]            NOT NULL DEFAULT '{}',
  created_at              TIMESTAMPTZ       NOT NULL DEFAULT now(),
  updated_at              TIMESTAMPTZ       NOT NULL DEFAULT now(),
  event_ts                TIMESTAMPTZ       NOT NULL DEFAULT now(),
  is_deleted              BOOLEAN           NOT NULL DEFAULT FALSE,
  PRIMARY KEY (project_id, id, start_time)
) PARTITION BY RANGE (start_time);

-- 3) scores
-- ClickHouse parity: PARTITION BY toYYYYMM(timestamp), ORDER BY (project_id, toDate(timestamp), id)
CREATE TABLE IF NOT EXISTS scores (
  id                  TEXT            NOT NULL,
  project_id          TEXT            NOT NULL,
  "timestamp"         TIMESTAMPTZ     NOT NULL,
  trace_id            TEXT            NOT NULL,
  observation_id      TEXT,
  session_id          TEXT,
  dataset_run_id      TEXT,
  environment         TEXT,
  name                TEXT            NOT NULL,
  value               DOUBLE PRECISION,
  string_value        TEXT,
  long_string_value   TEXT,
  data_type           score_data_type NOT NULL,
  source              TEXT,
  comment             TEXT,
  author_user_id      TEXT,
  config_id           TEXT,
  queue_id            TEXT,
  execution_trace_id  TEXT,
  metadata            JSONB           NOT NULL DEFAULT '{}'::jsonb,
  created_at          TIMESTAMPTZ     NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ     NOT NULL DEFAULT now(),
  event_ts            TIMESTAMPTZ     NOT NULL DEFAULT now(),
  is_deleted          BOOLEAN         NOT NULL DEFAULT FALSE,
  PRIMARY KEY (project_id, id, "timestamp")
) PARTITION BY RANGE ("timestamp");

-- Example monthly partitions (create per month as needed)
CREATE TABLE IF NOT EXISTS traces_2026_03
  PARTITION OF traces
  FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');

CREATE TABLE IF NOT EXISTS observations_2026_03
  PARTITION OF observations
  FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');

CREATE TABLE IF NOT EXISTS scores_2026_03
  PARTITION OF scores
  FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');

CREATE TABLE IF NOT EXISTS traces_default
  PARTITION OF traces DEFAULT;

CREATE TABLE IF NOT EXISTS observations_default
  PARTITION OF observations DEFAULT;

CREATE TABLE IF NOT EXISTS scores_default
  PARTITION OF scores DEFAULT;

COMMIT;

-- Optional: corresponding indexes inspired by ClickHouse bloom indexes.
-- Keep commented unless you explicitly want them.
-- CREATE INDEX IF NOT EXISTS idx_traces_id ON traces (id);
-- CREATE INDEX IF NOT EXISTS idx_traces_metadata_gin ON traces USING GIN (metadata);
-- CREATE INDEX IF NOT EXISTS idx_traces_session_id ON traces (project_id, session_id);
-- CREATE INDEX IF NOT EXISTS idx_traces_user_id ON traces (project_id, user_id);
-- CREATE INDEX IF NOT EXISTS idx_observations_id ON observations (id);
-- CREATE INDEX IF NOT EXISTS idx_observations_trace_id ON observations (project_id, trace_id);
-- CREATE INDEX IF NOT EXISTS idx_observations_metadata_gin ON observations USING GIN (metadata);
-- CREATE INDEX IF NOT EXISTS idx_scores_id ON scores (id);
-- CREATE INDEX IF NOT EXISTS idx_scores_project_trace_observation ON scores (project_id, trace_id, observation_id);
-- CREATE INDEX IF NOT EXISTS idx_scores_project_session ON scores (project_id, session_id);
-- CREATE INDEX IF NOT EXISTS idx_scores_project_dataset_run ON scores (project_id, dataset_run_id);