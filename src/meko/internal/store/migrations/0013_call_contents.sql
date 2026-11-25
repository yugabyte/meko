-- What each call sent and got back, kept beside its call log row only while Settings' "Keep call
-- contents" is on, for a few days (memory.CallContentsRetention), so a user can see what an agent
-- asked Meko and what it was told. call_log itself still holds no arguments and no text. These rows
-- are never exported or sent in usage stats.
-- client: the X-Meko-Client header the call came with ('agentk' for AgentK's own calls), a name only.
-- Migrations are append-only: never edit one that has shipped.
ALTER TABLE call_log ADD COLUMN client TEXT NOT NULL DEFAULT '';

CREATE TABLE call_contents (
  call_id INTEGER PRIMARY KEY REFERENCES call_log(id) ON DELETE CASCADE,
  args    TEXT NOT NULL,                 -- the arguments as sent, JSON (cut at memory.CallContentsMax)
  reply   TEXT NOT NULL,                 -- the reply's text, an error's too
  cut     INTEGER NOT NULL DEFAULT 0     -- 1 when either was cut to fit
) STRICT;
