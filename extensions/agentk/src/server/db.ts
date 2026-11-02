import { DatabaseSync, type StatementSync } from 'node:sqlite';
import { chmodSync, closeSync, mkdirSync, openSync, readdirSync, rmSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';

export type Db = DatabaseSync;
type Param = null | number | bigint | string | Uint8Array;
export type Row = Record<string, Param>;

/**
 * Schema migrations, applied in order. PRAGMA user_version records how many
 * have run. Never edit a shipped migration; append a new one.
 */
const MIGRATIONS: string[] = [
  `
  CREATE TABLE projects (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    repo_path TEXT NOT NULL,
    default_branch TEXT NOT NULL,
    position INTEGER NOT NULL DEFAULT 0,
    settings_json TEXT NOT NULL DEFAULT '{}',
    meko_conversation_id TEXT,
    created_at INTEGER NOT NULL,
    deleted_at INTEGER
  );
  CREATE UNIQUE INDEX projects_repo ON projects(repo_path) WHERE deleted_at IS NULL;

  CREATE TABLE project_datapacks (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    datapack_id TEXT NOT NULL,
    name TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('dedicated', 'shared')),
    PRIMARY KEY (project_id, datapack_id)
  );
  CREATE UNIQUE INDEX one_dedicated ON project_datapacks(project_id) WHERE role = 'dedicated';

  CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    slug TEXT NOT NULL,
    name TEXT NOT NULL,
    claude_session_id TEXT NOT NULL,
    transcript_path TEXT,
    branch TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    worktree_path TEXT NOT NULL,
    tmux_name TEXT NOT NULL,
    hook_token TEXT NOT NULL,
    desired TEXT NOT NULL CHECK (desired IN ('running', 'archived')),
    status TEXT NOT NULL,
    status_detail TEXT,
    stage TEXT NOT NULL,
    pr_number INTEGER,
    pr_url TEXT,
    review_state TEXT NOT NULL DEFAULT 'none',
    ci_state TEXT NOT NULL DEFAULT 'none',
    merge_state TEXT,
    seen_review_ids TEXT NOT NULL DEFAULT '[]',
    initial_prompt TEXT,
    auto_continue_at TEXT NOT NULL DEFAULT '[]',
    last_event_at INTEGER,
    created_at INTEGER NOT NULL,
    archived_at INTEGER,
    recovered_at INTEGER
  );
  CREATE UNIQUE INDEX sessions_slug ON sessions(project_id, slug);

  CREATE TABLE session_stages (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    stage TEXT NOT NULL,
    entered_at INTEGER NOT NULL,
    left_at INTEGER,
    moved_by TEXT NOT NULL
  );

  CREATE TABLE session_events (
    id INTEGER PRIMARY KEY,
    session_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    text TEXT,
    payload_json TEXT,
    at INTEGER NOT NULL
  );
  CREATE INDEX session_events_project ON session_events(project_id, at);

  CREATE TABLE designs (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    markdown TEXT NOT NULL,
    updated_at INTEGER NOT NULL
  );

  CREATE TABLE owner (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    username TEXT NOT NULL,
    pw_scrypt TEXT NOT NULL,
    totp_secret TEXT,
    totp_pending TEXT
  );

  CREATE TABLE auth_sessions (
    token_sha256 TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    user_agent TEXT,
    ip TEXT
  );

  CREATE TABLE audit_log (
    id INTEGER PRIMARY KEY,
    at INTEGER NOT NULL,
    action TEXT NOT NULL,
    detail_json TEXT
  );

  CREATE TABLE kv (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
  );

  CREATE TABLE stage_prompts (
    stage TEXT PRIMARY KEY,
    template TEXT NOT NULL
  );
  `,
  // 2: remember that Claude died mid-turn (its SessionEnd hook can fire as tmux dies).
  `ALTER TABLE sessions ADD COLUMN interrupted INTEGER NOT NULL DEFAULT 0;`,
  // 3: token usage, one row per model response, read incrementally from transcripts.
  `
  CREATE TABLE usage_messages (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    message_id TEXT NOT NULL,
    agent TEXT,
    model TEXT NOT NULL,
    at INTEGER NOT NULL,
    input INTEGER NOT NULL,
    output INTEGER NOT NULL,
    cache_read INTEGER NOT NULL,
    cache_write INTEGER NOT NULL,
    PRIMARY KEY (session_id, message_id)
  );
  CREATE INDEX usage_messages_at ON usage_messages(at);
  CREATE TABLE usage_files (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    offset INTEGER NOT NULL,
    PRIMARY KEY (session_id, path)
  );
  `,
  // 4: designs without plan mode. Sessions created before this keep plan mode for Design;
  // a design can be a published Artifact (its HTML and claude.ai link) instead of markdown.
  `
  ALTER TABLE sessions ADD COLUMN plan_mode INTEGER NOT NULL DEFAULT 1;
  ALTER TABLE designs ADD COLUMN html TEXT;
  ALTER TABLE designs ADD COLUMN artifact_url TEXT;
  `,
  // 5: review comments on a session's diff, sent to Claude in batches.
  `
  CREATE TABLE review_comments (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    side TEXT NOT NULL CHECK (side IN ('old', 'new')),
    line INTEGER NOT NULL,
    end_line INTEGER NOT NULL,
    excerpt TEXT NOT NULL,
    body TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'draft' CHECK (state IN ('draft', 'queued', 'sent')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    sent_at INTEGER
  );
  CREATE INDEX review_comments_session ON review_comments(session_id, state);
  `,
  // 6: several projects may share a repo.
  `DROP INDEX projects_repo;`,
  // 7: Merge and Cleanup became one stage, Merge & Close.
  `UPDATE sessions SET stage = 'merge' WHERE stage = 'cleanup';`,
  // 8: pushes straight to a session's base branch, found in its transcripts. Clearing the read
  // offsets makes every transcript be read once more (usage rows are upserts), to find old pushes.
  `
  CREATE TABLE pushes (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    range TEXT NOT NULL,
    ref TEXT NOT NULL,
    at INTEGER NOT NULL,
    commits INTEGER,
    PRIMARY KEY (session_id, range)
  );
  DELETE FROM usage_files;
  `,
  // 9: a review prompt waiting for Claude ('review' or 'review-people'), or 'sent' until the
  // turn that addresses it ends; kept across restarts.
  `ALTER TABLE sessions ADD COLUMN pending_review TEXT;`,
  // 10: prompts waiting for Claude, as a JSON map of prompt name to { state: 'queued' | 'sent',
  // vars } (replaces pending_review, which stays unused); the CI-fix watcher (the head that last
  // got a CI prompt, and how many heads in a row did); whether AgentK is active on the PR; and
  // every project on the AgentK + People reviewer mode.
  `
  ALTER TABLE sessions ADD COLUMN pending_prompts TEXT NOT NULL DEFAULT '{}';
  ALTER TABLE sessions ADD COLUMN ci_fix_head TEXT;
  ALTER TABLE sessions ADD COLUMN ci_fixes INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE sessions ADD COLUMN agentk_seen INTEGER NOT NULL DEFAULT 0;
  UPDATE sessions SET pending_prompts = json_object(pending_review, json_object('state', 'queued'))
    WHERE pending_review IN ('review', 'review-people');
  UPDATE sessions SET pending_prompts = json_object('review', json_object('state', 'sent'))
    WHERE pending_review = 'sent';
  UPDATE projects SET settings_json = json_set(CASE WHEN json_valid(settings_json) THEN settings_json ELSE '{}' END,
    '$.reviewer', 'agentk-people');
  `,
  // 11: session kinds (explore, plan, quick). A Plan & Implement session always entered Design
  // first, so one that never did was started with "Skip design", which is now Quick Code.
  `
  ALTER TABLE sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'plan';
  UPDATE sessions SET kind = 'quick' WHERE id NOT IN (SELECT session_id FROM session_stages WHERE stage = 'design');
  `,
  // 12: project datapacks shared with us (invites waiting in Meko, and shares not yet a project).
  // state: invited | shared | later | used | baseline (already shared when this was first synced).
  `
  CREATE TABLE datapack_invites (
    datapack_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    invite_id TEXT,
    grant_role TEXT,
    owner_name TEXT,
    owner_email TEXT,
    repo_remote TEXT,
    state TEXT NOT NULL,
    first_seen_at INTEGER NOT NULL,
    resolved_at INTEGER
  );
  `,
  // 13: session worktrees, kept idle between sessions so build outputs carry over. repo is the
  // repo's real path (the server re-keys rows at startup); session_id NULL means idle; stale 1 means
  // idle but not reusable (files git doesn't ignore, so it won't remove it either). A session's
  // worktree_released is 1 once it let go of its worktree (idle, another session's, or removed).
  `
  CREATE TABLE worktrees (
    path TEXT PRIMARY KEY,
    repo TEXT NOT NULL,
    session_id TEXT,
    released_at INTEGER,
    stale INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
  );
  CREATE INDEX worktrees_idle ON worktrees(repo, released_at) WHERE session_id IS NULL;
  ALTER TABLE sessions ADD COLUMN worktree_released INTEGER NOT NULL DEFAULT 0;
  INSERT OR IGNORE INTO worktrees(path, repo, session_id, created_at)
    SELECT s.worktree_path, p.repo_path, s.id, s.created_at
    FROM sessions s JOIN projects p ON p.id = s.project_id
    WHERE s.desired = 'running';
  `,
  `
  ALTER TABLE sessions ADD COLUMN repo_path TEXT;
  ALTER TABLE sessions ADD COLUMN branch_reused INTEGER NOT NULL DEFAULT 0;
  UPDATE sessions SET repo_path = (SELECT repo_path FROM projects WHERE projects.id = sessions.project_id);
  `,
  // 15: designs in Meko, where a file can't be replaced: how many versions of the markdown and
  // HTML were sent (naming the next one, design-<slug>-v2.md, …), and whether the current one was.
  // A design saved before this in a project with a dedicated datapack counts as sent once.
  `
  ALTER TABLE designs ADD COLUMN md_version INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE designs ADD COLUMN md_sent INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE designs ADD COLUMN html_version INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE designs ADD COLUMN html_sent INTEGER NOT NULL DEFAULT 0;
  UPDATE designs SET md_version = 1, md_sent = 1 WHERE markdown != '' AND EXISTS (
    SELECT 1 FROM sessions s JOIN project_datapacks d ON d.project_id = s.project_id AND d.role = 'dedicated'
    WHERE s.id = designs.session_id);
  UPDATE designs SET html_version = 1, html_sent = 1 WHERE html IS NOT NULL AND EXISTS (
    SELECT 1 FROM sessions s JOIN project_datapacks d ON d.project_id = s.project_id AND d.role = 'dedicated'
    WHERE s.id = designs.session_id);
  `,
  // 16: what a direct push changed, per file extension (a JSON list of FileTypeCount), filled in
  // with its commit count. Pushes found before this stay NULL.
  `ALTER TABLE pushes ADD COLUMN file_types TEXT;`,
  // 17: Meko calls found in transcripts (outcome NULL until the result is read), and the 1-hour
  // share of cache writes, which costs more. The index serves the savings estimate, which counts
  // the responses after each search in the same conversation or subagent. Clearing the read
  // offsets makes every transcript be read once more (usage rows are upserts), so old sessions
  // get both.
  `
  CREATE TABLE meko_calls (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    tool_use_id TEXT NOT NULL,
    agent TEXT,
    tool TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('lookup', 'save', 'other')),
    at INTEGER NOT NULL,
    outcome TEXT CHECK (outcome IN ('found', 'empty', 'error', 'ok')),
    results INTEGER,
    input_chars INTEGER NOT NULL,
    result_chars INTEGER,
    model TEXT,
    context_tokens INTEGER,
    PRIMARY KEY (session_id, tool_use_id)
  );
  ALTER TABLE usage_messages ADD COLUMN cache_write_1h INTEGER NOT NULL DEFAULT 0;
  CREATE INDEX usage_messages_agent_at ON usage_messages(session_id, agent, at);
  DELETE FROM usage_files;
  `,
  // 18: the dedicated datapack a session was launched with, so AgentK's writes for it (designs,
  // the PR's Meko lines, what shipped) keep going there after the project switches datapacks,
  // as its own Meko calls and capture do. NULL for sessions launched before this, or with no
  // datapack: those use the project's dedicated datapack.
  `
  ALTER TABLE sessions ADD COLUMN meko_datapack_id TEXT;
  ALTER TABLE sessions ADD COLUMN meko_datapack_name TEXT;
  `,
  // 19: when each session's first turn ended (TurnMark in usage.ts), and
  // AgentK's Meko search at session start (recall.ts), for the first-turn Meko stats. Clearing the
  // read offsets reads every transcript again, so older sessions get first_turn_end_at too.
  `
  ALTER TABLE sessions ADD COLUMN first_turn_end_at INTEGER;
  CREATE TABLE meko_preloads (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    at INTEGER NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('found', 'empty', 'error', 'late')),
    results INTEGER NOT NULL,
    chars INTEGER NOT NULL,
    ms INTEGER NOT NULL
  );
  DELETE FROM usage_files;
  `,
  // 20: the Meko agent id a session was launched with, set when it's created and at each launch.
  // NULL for sessions from before sessions had their own: they run with claude_code:<project id>
  // until they're launched again. And projects closing and opening again, for the usage stats
  // (a reopen clears deleted_at); closes so far are carried over.
  `
  ALTER TABLE sessions ADD COLUMN meko_agent_id TEXT;
  CREATE TABLE project_events (
    project_id TEXT NOT NULL REFERENCES projects(id),
    kind TEXT NOT NULL CHECK (kind IN ('closed', 'reopened')),
    at INTEGER NOT NULL
  );
  INSERT INTO project_events(project_id, kind, at) SELECT id, 'closed', deleted_at FROM projects WHERE deleted_at IS NOT NULL;
  `,
  // 21: Review a PR sessions. The PR's repo on GitHub (owner/name), title and author; the commit
  // checked out for the review (review_sha) and the PR's head at the last look (pr_head_sha), with
  // the commits since review_sha (NULL when the history was rewritten); the PR's state; the
  // remote of the clone the PR is fetched from (origin, or upstream for a fork); and the user's
  // notes for AgentK. All NULL for other kinds.
  `
  ALTER TABLE sessions ADD COLUMN pr_repo TEXT;
  ALTER TABLE sessions ADD COLUMN pr_title TEXT;
  ALTER TABLE sessions ADD COLUMN pr_author TEXT;
  ALTER TABLE sessions ADD COLUMN review_sha TEXT;
  ALTER TABLE sessions ADD COLUMN pr_head_sha TEXT;
  ALTER TABLE sessions ADD COLUMN pr_new_commits INTEGER;
  ALTER TABLE sessions ADD COLUMN pr_state TEXT;
  ALTER TABLE sessions ADD COLUMN pr_remote TEXT;
  ALTER TABLE sessions ADD COLUMN review_notes TEXT;
  `,
  // 22: a design's questions for the user (normalized JSON, where they came from, and how many are
  // open), and the user's answers: drafts until sent, at most one draft per question. question_id
  // is '_feedback' for other feedback on the design; question and choice_label are what the user saw.
  `
  ALTER TABLE designs ADD COLUMN questions TEXT NOT NULL DEFAULT '[]';
  ALTER TABLE designs ADD COLUMN questions_source TEXT;
  ALTER TABLE designs ADD COLUMN open_questions INTEGER NOT NULL DEFAULT 0;
  CREATE TABLE design_answers (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    question_id TEXT NOT NULL,
    question TEXT NOT NULL,
    choice INTEGER,
    choice_label TEXT,
    your_call INTEGER NOT NULL DEFAULT 0,
    text TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK (state IN ('draft', 'queued', 'sent')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    sent_at INTEGER
  );
  CREATE INDEX design_answers_session ON design_answers(session_id, state);
  CREATE UNIQUE INDEX design_answers_unsent ON design_answers(session_id, question_id) WHERE state != 'sent';
  `,
  // 23: the merge-conflict watcher, like the CI-fix one: the PR head that last got a prompt to
  // resolve conflicts with the base, and how many heads in a row did.
  `
  ALTER TABLE sessions ADD COLUMN conflict_head TEXT;
  ALTER TABLE sessions ADD COLUMN conflict_fixes INTEGER NOT NULL DEFAULT 0;
  `,
  // 24: a task started on a PR whose head isn't origin's branch of the same name (a fork's, say):
  // the remote and the branch there its pushes go to. NULL: origin, under the session's branch.
  `
  ALTER TABLE sessions ADD COLUMN push_remote TEXT;
  ALTER TABLE sessions ADD COLUMN push_branch TEXT;
  `,
  // 25: the images added in the New session dialog, as a JSON list of their paths in the session's
  // pastes folder. Kept apart from the task, so a session started at Explore with only images
  // still has no task.
  `
  ALTER TABLE sessions ADD COLUMN start_images TEXT;
  `,
  // 26: a gate pressed before it could go (QueuedGate: 'open-pr' or 'merge'), kept until it goes or
  // is dropped, so a merge waiting on CI isn't lost to a restart. NULL: none.
  `
  ALTER TABLE sessions ADD COLUMN queued_gate TEXT;
  `,
  // 27: the learn step (learn.ts): each run with what it found and wrote, the Meko calls AgentK
  // itself makes (tagged with the learn step that made them), and the Meko plugin's transcript
  // captures, for the usage report. A learn step's own model calls are usage_messages rows with
  // agent 'agentk:learn'.
  `
  CREATE TABLE meko_learns (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    trigger TEXT NOT NULL CHECK (trigger IN ('merge', 'manual')),
    path TEXT NOT NULL CHECK (path IN ('mod', 'turn', 'none')),
    started_at INTEGER NOT NULL,
    ended_at INTEGER,
    outcome TEXT CHECK (outcome IN ('done', 'failed', 'interrupted', 'skipped')),
    failed_step TEXT,
    candidates INTEGER NOT NULL DEFAULT 0,
    saves INTEGER NOT NULL DEFAULT 0,
    updates INTEGER NOT NULL DEFAULT 0,
    skips INTEGER NOT NULL DEFAULT 0,
    lookups INTEGER NOT NULL DEFAULT 0,
    errors INTEGER NOT NULL DEFAULT 0,
    sent_chars INTEGER NOT NULL DEFAULT 0
  );
  CREATE INDEX meko_learns_session ON meko_learns(session_id, started_at);
  CREATE TABLE meko_agentk_calls (
    at INTEGER NOT NULL,
    tool TEXT NOT NULL,
    input_chars INTEGER NOT NULL,
    result_chars INTEGER NOT NULL,
    ok INTEGER NOT NULL,
    ms INTEGER NOT NULL,
    learn_id TEXT
  );
  CREATE INDEX meko_agentk_calls_at ON meko_agentk_calls(at);
  CREATE TABLE meko_capture_runs (
    at INTEGER NOT NULL,
    ok INTEGER NOT NULL,
    ms INTEGER NOT NULL
  );
  `,
  // 28: Discard runs the learn step too (trigger 'discard', so meko_learns is made again for its
  // CHECK), and waits for it (sessions.discarding, so a restart finishes the discard); Merge &
  // Close and Discard can skip it (sessions.skip_learn, the merge's choice kept until it lands).
  `
  ALTER TABLE sessions ADD COLUMN discarding INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE sessions ADD COLUMN skip_learn INTEGER NOT NULL DEFAULT 0;
  CREATE TABLE meko_learns_v28 (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    trigger TEXT NOT NULL CHECK (trigger IN ('merge', 'manual', 'discard')),
    path TEXT NOT NULL CHECK (path IN ('mod', 'turn', 'none')),
    started_at INTEGER NOT NULL,
    ended_at INTEGER,
    outcome TEXT CHECK (outcome IN ('done', 'failed', 'interrupted', 'skipped')),
    failed_step TEXT,
    candidates INTEGER NOT NULL DEFAULT 0,
    saves INTEGER NOT NULL DEFAULT 0,
    updates INTEGER NOT NULL DEFAULT 0,
    skips INTEGER NOT NULL DEFAULT 0,
    lookups INTEGER NOT NULL DEFAULT 0,
    errors INTEGER NOT NULL DEFAULT 0,
    sent_chars INTEGER NOT NULL DEFAULT 0
  );
  INSERT INTO meko_learns_v28(id, session_id, trigger, path, started_at, ended_at, outcome, failed_step, candidates, saves, updates, skips, lookups, errors, sent_chars)
    SELECT id, session_id, trigger, path, started_at, ended_at, outcome, failed_step, candidates, saves, updates, skips, lookups, errors, sent_chars FROM meko_learns;
  DROP TABLE meko_learns;
  ALTER TABLE meko_learns_v28 RENAME TO meko_learns;
  CREATE INDEX meko_learns_session ON meko_learns(session_id, started_at);
  `,
  // 29: the default branch prefix is ak_<your GitHub login>/ (DEFAULT_BRANCH_PREFIX). Projects
  // on ak/ move to it, and any other prefix stays. A project stores its whole settings when it's
  // made, so an ak/ typed in by hand can't be told from the old default and moves too.
  `
  UPDATE projects SET settings_json = json_set(settings_json, '$.branchPrefix', 'ak_{login}/')
    WHERE json_valid(settings_json) AND json_extract(settings_json, '$.branchPrefix') = 'ak/';
  `,
  // 30: the user's instructions given with an Open PR queued for the turn's end (OpenPrRequest),
  // sent with it and cleared with queued_gate. NULL: none.
  `
  ALTER TABLE sessions ADD COLUMN queued_instructions TEXT;
  `,
  // 31: designs in PRs. md_hash/html_hash: the content hash of the latest version sent to Meko,
  // which the PR's `meko_design:` line names (NULL: none sent since this). source: for a Review a
  // PR session's copy of its PR's design, the PR line it came from as JSON {name, sha256}, and
  // source_told the sha256 Claude was last pointed at. Both NULL for a task's own design.
  `
  ALTER TABLE designs ADD COLUMN md_hash TEXT;
  ALTER TABLE designs ADD COLUMN html_hash TEXT;
  ALTER TABLE designs ADD COLUMN source TEXT;
  ALTER TABLE designs ADD COLUMN source_told TEXT;
  `,
  // 32: Ask AgentK's asks, for the usage report (counts only, never the text): when, whether
  // Claude answered, how many actions the answer proposed, and how long it took.
  `
  CREATE TABLE agentk_asks (
    at INTEGER NOT NULL,
    ok INTEGER NOT NULL,
    actions INTEGER NOT NULL,
    ms INTEGER NOT NULL
  );
  CREATE INDEX agentk_asks_at ON agentk_asks(at);
  `,
  // 33: the people the Share dialog has invited (contacts.ts), suggested in it next time: the
  // address as last typed, keyed in lower case, the name from Meko's member lists, how many invites
  // went through and when the latest did. Starts with the emails remembered for PR reviewers. The
  // key is SQLite's lower() (ASCII letters only): emailKey() in contacts.ts builds it the same way.
  `
  CREATE TABLE share_contacts (
    email_key TEXT PRIMARY KEY,
    email TEXT NOT NULL,
    name TEXT,
    shares INTEGER NOT NULL,
    last_at INTEGER NOT NULL
  );
  INSERT INTO share_contacts(email_key, email, shares, last_at)
    SELECT lower(value), value, 1, CAST(strftime('%s', 'now') AS INTEGER) * 1000
    FROM kv WHERE key LIKE 'reviewer-email:%'
    ON CONFLICT(email_key) DO NOTHING;
  `,
  // 34: What Meko doesn't know (mekousage.ts, mekogaps.ts). For Claude's searches: what was asked
  // (query, cut to 1,000 characters, and query_key, the same lower-cased with its whitespace
  // collapsed, which groups asks), the best memory scores of the reply (score,
  // raw_similarity_score), and quality: 'empty' (found nothing) or 'weak' (nothing relevant), NULL
  // otherwise. For AgentK's search at start: its query, and the quality and best memory score of
  // what Meko returned before picking. Calls and searches from before this keep NULL: transcripts
  // aren't read again. meko_gap_marks: what the user did with a question (by query_key), each
  // action's latest time: dismissed_at (cleared by a restore), taught_at with what was saved and
  // where, and opened_at (a restore); the latest one at or after the question's last ask decides
  // its state.
  `
  ALTER TABLE meko_calls ADD COLUMN query TEXT;
  ALTER TABLE meko_calls ADD COLUMN query_key TEXT;
  ALTER TABLE meko_calls ADD COLUMN top_score REAL;
  ALTER TABLE meko_calls ADD COLUMN top_raw REAL;
  ALTER TABLE meko_calls ADD COLUMN quality TEXT CHECK (quality IN ('empty', 'weak'));
  CREATE INDEX meko_calls_quality ON meko_calls(quality) WHERE quality IS NOT NULL;
  ALTER TABLE meko_preloads ADD COLUMN query TEXT;
  ALTER TABLE meko_preloads ADD COLUMN query_key TEXT;
  ALTER TABLE meko_preloads ADD COLUMN quality TEXT CHECK (quality IN ('empty', 'weak'));
  ALTER TABLE meko_preloads ADD COLUMN top_score REAL;
  CREATE TABLE meko_gap_marks (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    query_key TEXT NOT NULL,
    dismissed_at INTEGER,
    taught_at INTEGER,
    taught_text TEXT,
    taught_datapack TEXT,
    opened_at INTEGER,
    PRIMARY KEY (project_id, query_key)
  );
  `,
  // 35: the learn step addresses What Meko doesn't know (learn.ts). meko_gap_marks: the session
  // whose learn step taught or dismissed a question (NULL: a person did), and for a dismissal by
  // one, the saved memory that already said the answer; and a person's own latest dismissal and
  // teach (person_dismissed_at, person_taught_at), which a learn step's marks don't overwrite, for
  // the usage report (every mark so far was a person's). meko_learns: the questions a step offered
  // the fork, and how many it marked taught or dismissed.
  `
  ALTER TABLE meko_gap_marks ADD COLUMN taught_session TEXT;
  ALTER TABLE meko_gap_marks ADD COLUMN dismissed_session TEXT;
  ALTER TABLE meko_gap_marks ADD COLUMN dismissed_memory TEXT;
  ALTER TABLE meko_gap_marks ADD COLUMN person_dismissed_at INTEGER;
  ALTER TABLE meko_gap_marks ADD COLUMN person_taught_at INTEGER;
  UPDATE meko_gap_marks SET person_dismissed_at = dismissed_at, person_taught_at = taught_at;
  ALTER TABLE meko_learns ADD COLUMN questions INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE meko_learns ADD COLUMN taught INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE meko_learns ADD COLUMN dismissed INTEGER NOT NULL DEFAULT 0;
  `,
  // 36: reviews held for the user to send to AgentK or dismiss (heldreviews.ts), one row per
  // review on the session's PR: what the sheet shows (body, inline comments cut to 50) and the
  // REST id the prompt names. Projects without a reviewer mode keep AgentK + People, the default
  // they had: new projects default to AgentK only. `held-reviews-since`: when holding began, since
  // People only never marked AgentK's reviews seen, so its earlier rounds aren't held all at once.
  `
  CREATE TABLE held_reviews (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    review_id TEXT NOT NULL,
    pr_number INTEGER NOT NULL,
    db_id INTEGER,
    author TEXT NOT NULL,
    kind TEXT NOT NULL,
    state TEXT NOT NULL,
    body TEXT NOT NULL,
    url TEXT NOT NULL,
    comments_json TEXT NOT NULL DEFAULT '[]',
    more_comments INTEGER NOT NULL DEFAULT 0,
    submitted_at TEXT NOT NULL,
    held_at INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'held',
    decided_at INTEGER,
    PRIMARY KEY (session_id, review_id)
  );
  UPDATE projects SET settings_json = json_set(CASE WHEN json_valid(settings_json) THEN settings_json ELSE '{}' END, '$.reviewer', 'agentk-people')
    WHERE NOT json_valid(settings_json) OR json_type(settings_json, '$.reviewer') IS NULL;
  INSERT OR REPLACE INTO kv(key, value) VALUES ('held-reviews-since', CAST(CAST(strftime('%s', 'now') AS INTEGER) * 1000 AS TEXT));
  `,
  // 37: sessions on other coding agents (src/shared/harnesses.ts). harness: the one a session runs
  // on, Claude Code for every session before this. agent_session_id: the conversation id a harness
  // picks itself (Kiro's), learned after it starts, for Resume. usage_messages.credits: what Kiro
  // billed for a turn (NULL for Claude Code's responses, which are priced from their tokens).
  `
  ALTER TABLE sessions ADD COLUMN harness TEXT NOT NULL DEFAULT 'claude';
  ALTER TABLE sessions ADD COLUMN agent_session_id TEXT;
  ALTER TABLE usage_messages ADD COLUMN credits REAL;
  `,
  // 38: untrusted: 1 when the session works on a PR's head on a public repo (or one whose visibility
  // gh couldn't read), so it runs without the repo's own config (headtrust.ts); 0 when that head is
  // trusted; NULL when it has no PR head, or until it's checked (at its next launch).
  `
  ALTER TABLE sessions ADD COLUMN untrusted INTEGER;
  `,
  // 39: position: a session's place in its project's list, lowest first. New sessions and Move to top
  // take one below the project's lowest, so nothing is renumbered. Existing ones keep their oldest-first order.
  `
  ALTER TABLE sessions ADD COLUMN position INTEGER NOT NULL DEFAULT 0;
  UPDATE sessions SET position = (SELECT COUNT(*) FROM sessions o WHERE o.project_id = sessions.project_id
    AND (o.created_at < sessions.created_at OR (o.created_at = sessions.created_at AND o.id < sessions.id)));
  `,
  // 40: purpose: what a session is for beyond its kind ('learn': a learn session, SessionPurpose); NULL for the rest.
  `
  ALTER TABLE sessions ADD COLUMN purpose TEXT;
  `,
  // 41: auto_run: 1 while Run to merge is on, so AgentK passes Open PR and Merge & Close by itself (lifecycle.ts);
  // queued_auto: 1 when Run to merge queued the gate in queued_gate (not the user's press), so stopping it drops only its own.
  `
  ALTER TABLE sessions ADD COLUMN auto_run INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE sessions ADD COLUMN queued_auto INTEGER NOT NULL DEFAULT 0;
  `,
  // 42: the work queue (workqueue.ts): items in the user's order (position, lowest first), each with its
  // links (matched by their parseWorkLink key) and at most one session (session_id, unique: one item per
  // session). source_key: a synced item's link key, so a sync never adds it again, even once closed.
  `
  CREATE TABLE work_items (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    notes TEXT NOT NULL DEFAULT '',
    position INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'open',
    source TEXT NOT NULL DEFAULT 'manual',
    source_key TEXT UNIQUE,
    session_id TEXT UNIQUE REFERENCES sessions(id) ON DELETE SET NULL,
    unplaced INTEGER NOT NULL DEFAULT 0,
    github_done_at INTEGER,
    created_at INTEGER NOT NULL,
    closed_at INTEGER
  );
  CREATE TABLE work_item_links (
    item_id TEXT NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    ord INTEGER NOT NULL,
    key TEXT NOT NULL,
    kind TEXT NOT NULL,
    url TEXT,
    label TEXT NOT NULL,
    PRIMARY KEY (item_id, key)
  );
  CREATE INDEX work_item_links_key ON work_item_links(key);
  `,
  // 43: the code review (review/runner.ts, docs/design/code-review.md). parent_id: a code-review
  // session's task; review_json: a session's own review settings (ReviewOverrides), NULL for none.
  // code_reviews: one row per review a task's Open PR started (its rounds, sweeps and state; the
  // current round's candidates, every round's findings for the panel, the counts for the PR line, the
  // Open PR instructions it holds); code_review_steps: each step's report as AgentK's tools took it,
  // so a review resumes after a restart and a Retry reuses what was reported.
  `
  ALTER TABLE sessions ADD COLUMN parent_id TEXT;
  ALTER TABLE sessions ADD COLUMN review_json TEXT;
  CREATE TABLE code_reviews (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    session_id TEXT,
    strategy TEXT NOT NULL,
    rounds INTEGER NOT NULL,
    depth INTEGER NOT NULL,
    state TEXT NOT NULL,
    round INTEGER NOT NULL DEFAULT 1,
    sweep INTEGER NOT NULL DEFAULT 1,
    asked INTEGER NOT NULL DEFAULT 0,
    candidates_json TEXT NOT NULL DEFAULT '[]',
    findings_json TEXT NOT NULL DEFAULT '[]',
    notes_json TEXT NOT NULL DEFAULT '[]',
    datapacks_json TEXT NOT NULL DEFAULT '[]',
    counts_json TEXT NOT NULL DEFAULT '{"blocker":0,"major":0,"minor":0}',
    last_kept INTEGER NOT NULL DEFAULT 0,
    instructions TEXT,
    by TEXT NOT NULL DEFAULT 'you',
    detail TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    finished_at INTEGER
  );
  CREATE INDEX code_reviews_task ON code_reviews(task_id, created_at);
  CREATE TABLE code_review_steps (
    review_id TEXT NOT NULL REFERENCES code_reviews(id) ON DELETE CASCADE,
    step TEXT NOT NULL,
    report_json TEXT NOT NULL,
    at INTEGER NOT NULL,
    PRIMARY KEY (review_id, step)
  );
  `,
  // 44: code_reviews.lead: the `moved` line for a review a move to Review & PR started from Explore or
  // Design (Lifecycle.reviewBeforePr), sent before the review's first prompt to the task (the ask for
  // the PR description, since that was added; its fixes or the PR prompt before) and cleared once that
  // has gone; NULL for none.
  `ALTER TABLE code_reviews ADD COLUMN lead TEXT;`,
  // 45: What Meko doesn't know moved into Meko, which judges every search itself and keeps the
  // questions per datapack (its Context Gaps; mekoquestions.ts). What 34 and 35 added for AgentK's own
  // list goes, with what it held: the questions' marks, and each search's query and how poor its
  // answer was. meko_learns keeps its counts of the questions a learn step marked.
  `
  DROP TABLE meko_gap_marks;
  DROP INDEX meko_calls_quality;
  ALTER TABLE meko_calls DROP COLUMN query;
  ALTER TABLE meko_calls DROP COLUMN query_key;
  ALTER TABLE meko_calls DROP COLUMN top_score;
  ALTER TABLE meko_calls DROP COLUMN top_raw;
  ALTER TABLE meko_calls DROP COLUMN quality;
  ALTER TABLE meko_preloads DROP COLUMN query;
  ALTER TABLE meko_preloads DROP COLUMN query_key;
  ALTER TABLE meko_preloads DROP COLUMN quality;
  ALTER TABLE meko_preloads DROP COLUMN top_score;
  `,
  // 46: code reviews run on demand (the review-run action; review/runner.ts). purpose: 'pr' for one
  // an Open PR started, 'demand' for one the user ran (it sends no PR prompt, and a stopped one holds
  // nothing). stage: where it runs, 'code' or 'review' (Review & PR); leaving that stage cancels it.
  // stale: 1 once its task left Code after it finished with its PR prompt unsent, so Open PR doesn't
  // reuse it.
  `
  ALTER TABLE code_reviews ADD COLUMN purpose TEXT NOT NULL DEFAULT 'pr';
  ALTER TABLE code_reviews ADD COLUMN stage TEXT NOT NULL DEFAULT 'code';
  ALTER TABLE code_reviews ADD COLUMN stale INTEGER NOT NULL DEFAULT 0;
  `,
];

export function openDb(path: string): Db {
  if (path !== ':memory:') {
    mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
    // Owner-only whatever its folder is: it holds the login's password hash and TOTP secret.
    // SQLite gives the -wal and -shm files the database's mode; ones made before this, and the
    // snapshots taken before updates (snapshotDb), are fixed too.
    closeSync(openSync(path, 'a', 0o600));
    const snapshots = readdirSync(dirname(path)).filter((f) => f.startsWith(`${basename(path)}.bak-`)).map((f) => join(dirname(path), f));
    for (const f of [path, `${path}-wal`, `${path}-shm`, ...snapshots]) {
      try {
        chmodSync(f, 0o600);
      } catch {
        /* not there yet */
      }
    }
  }
  const db = new DatabaseSync(path);
  // NORMAL, not FULL: in WAL mode FULL fsyncs on every commit, on the event loop (node:sqlite is
  // synchronous), and on a busy disk one commit can hold every request for hundreds of ms.
  db.exec(`
    PRAGMA journal_mode = WAL;
    PRAGMA synchronous = NORMAL;
    PRAGMA foreign_keys = ON;
    PRAGMA busy_timeout = 5000;
  `);
  migrate(db);
  return db;
}

export function schemaVersion(db: Db): number {
  const r = db.prepare('PRAGMA user_version').get() as { user_version: number };
  return r.user_version;
}

function migrate(db: Db): void {
  const current = schemaVersion(db);
  for (let i = current; i < MIGRATIONS.length; i++) {
    tx(db, () => {
      db.exec(MIGRATIONS[i]);
      db.exec(`PRAGMA user_version = ${i + 1}`);
    });
  }
}

export function tx<T>(db: Db, fn: () => T): T {
  db.exec('BEGIN IMMEDIATE');
  try {
    const out = fn();
    db.exec('COMMIT');
    return out;
  } catch (e) {
    db.exec('ROLLBACK');
    throw e;
  }
}

const stmtCache = new WeakMap<Db, Map<string, StatementSync>>();

function stmt(db: Db, sql: string): StatementSync {
  let m = stmtCache.get(db);
  if (!m) stmtCache.set(db, (m = new Map()));
  let s = m.get(sql);
  if (!s) m.set(sql, (s = db.prepare(sql)));
  return s;
}

export function all<T = Row>(db: Db, sql: string, ...params: Param[]): T[] {
  return stmt(db, sql).all(...params) as T[];
}

export function get<T = Row>(db: Db, sql: string, ...params: Param[]): T | undefined {
  return stmt(db, sql).get(...params) as T | undefined;
}

export function run(db: Db, sql: string, ...params: Param[]): { changes: number } {
  const r = stmt(db, sql).run(...params);
  return { changes: Number(r.changes) };
}

export function kvGet(db: Db, key: string): string | null {
  return get<{ value: string }>(db, 'SELECT value FROM kv WHERE key = ?', key)?.value ?? null;
}

export function kvSet(db: Db, key: string, value: string): void {
  run(db, 'INSERT INTO kv(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value', key, value);
}

export function kvDelete(db: Db, key: string): void {
  run(db, 'DELETE FROM kv WHERE key = ?', key);
}

export function audit(db: Db, action: string, detail?: unknown): void {
  run(db, 'INSERT INTO audit_log(at, action, detail_json) VALUES (?, ?, ?)', Date.now(), action,
    detail === undefined ? null : JSON.stringify(detail));
}

/** Consistent copy of the live database (used before updates). */
export function snapshotDb(db: Db, dest: string): void {
  rmSync(dest, { force: true }); // VACUUM INTO refuses to overwrite
  // Owner-only from the start, as the database is: VACUUM INTO takes an empty file, and keeps its mode.
  closeSync(openSync(dest, 'wx', 0o600));
  db.exec(`VACUUM INTO '${dest.replace(/'/g, "''")}'`);
  chmodSync(dest, 0o600);
}

/** snapshotDb for a database file nothing has open, without migrating it (updates.ts, before a new version opens it). */
export function snapshotFile(path: string, dest: string): void {
  const db = new DatabaseSync(path);
  try {
    db.exec('PRAGMA busy_timeout = 5000');
    snapshotDb(db, dest);
  } finally {
    db.close();
  }
}
