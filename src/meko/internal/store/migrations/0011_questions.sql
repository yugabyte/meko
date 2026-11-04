-- Context Gaps: the searches Meko answered poorly, grouped by question, and what was done about
-- each (docs/api.md, "Context Gaps"). Unlike the call log, these hold the query's text: they're the
-- datapack's own content, deleted with it (DeleteDatapack), and asks older than 90 days are pruned
-- with the call log. No foreign key to datapacks, so a Meko Cloud datapack's questions can be kept
-- too once its searches pass through this Meko.
-- Migrations are append-only: never edit one that has shipped.

-- One row per question (a datapack's query, lower-cased with its whitespace collapsed), with the
-- latest time of each thing done about it. The latest of dismissed, taught and opened at or after
-- its last poor ask decides its state; a later search that found something relevant
-- (answered_at) leaves it off the list.
CREATE TABLE questions (
  datapack_id         TEXT NOT NULL,
  query_key           TEXT NOT NULL,
  answered_at         TEXT,                      -- RFC 3339, UTC, as every time here
  dismissed_at        TEXT,
  dismissed_by        TEXT,                      -- the conversation of the agent that dismissed it; NULL: a person
  dismissed_memory    TEXT,                      -- the memory that already said the answer
  taught_at           TEXT,
  taught_text         TEXT,
  taught_memory       TEXT,
  taught_by           TEXT,                      -- as dismissed_by
  opened_at           TEXT,                      -- a restore
  person_dismissed_at TEXT,                      -- a person's own latest, for the usage stats
  person_taught_at    TEXT,
  PRIMARY KEY (datapack_id, query_key)
) STRICT;

-- One row per poor search: what was asked, how it went and who asked.
CREATE TABLE question_asks (
  id              INTEGER PRIMARY KEY,
  datapack_id     TEXT NOT NULL,
  query_key       TEXT NOT NULL,
  at              TEXT NOT NULL,
  query           TEXT NOT NULL,                 -- as asked, cut to 1,000 characters
  quality         TEXT NOT NULL CHECK (quality IN ('empty', 'weak')),
  tool            TEXT NOT NULL,
  agent_id        TEXT NOT NULL DEFAULT '',
  conversation_id TEXT NOT NULL DEFAULT '',
  results         INTEGER NOT NULL DEFAULT 0,
  top_score       REAL,                          -- the best memory score and cosine, when any
  top_raw         REAL,
  FOREIGN KEY (datapack_id, query_key) REFERENCES questions(datapack_id, query_key) ON DELETE CASCADE
) STRICT;
CREATE INDEX question_asks_question ON question_asks(datapack_id, query_key, at);
CREATE INDEX question_asks_at ON question_asks(at);
CREATE INDEX question_asks_conversation ON question_asks(conversation_id, at);
