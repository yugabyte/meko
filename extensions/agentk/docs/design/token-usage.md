# Token usage

Part of the [AgentK design](../design.md).

Claude Code writes every model response to the session transcript with its token counts. The
server reads new lines from each session's transcript, and from any subagent transcripts under
`<session>/subagents/`, after every turn (on the `Stop` hook, or an interrupted turn's
`/mod/turn`), when a session ends or is archived,
and once a minute. It remembers the byte offset it reached, so each line is read once.

One response spans several transcript lines with the same message id, so usage is stored once per
message: `usage_messages(session_id, message_id, agent, model, at, input, output, cache_read,
cache_write, cache_write_1h)`. Totals per session, project, model and day are sums over that table, and archived
sessions keep counting toward their project. `cache_write_1h` is the 1-hour share of the cache
writes (the transcript's `cache_creation.ephemeral_1h_input_tokens`; without that split, every write
counts as 1-hour, which is what Claude Code uses).

**Cost.** Tokens are priced at Anthropic's API list prices (`src/shared/pricing.ts`: input, output
and cache read per model, with cache writes at 1.25× input for the 5-minute cache and 2× for the
1-hour one), from per-model sums. Sums can't tell a request's prompt size, so Claude Haiku 5.5 is
priced at its rates for prompts of up to 100K tokens, never its 5× rate above that. It's shown as
**Est. cost**, what the tokens would cost on the API: on a Pro or Max plan nobody pays per token, but raw token totals overstate cheap cache reads.
A model with no known price adds tokens but no cost; the cost then reads `≥ $…` (**Est. cost,
partial**), and the tooltip says how many responses were left out. Model ids match with a date
(`-20251001`), `[1m]` or `@…` suffix, and in Bedrock's form (`us.anthropic.claude-…-v1:0`).

**OpenCode's tokens.** An [OpenCode session](harnesses.md#opencode) has no Claude transcript
either: AgentK's plugin logs each finished response (the session's and its subagents') to
`run/opencode/sessions/<session>/messages.jsonl`, read incrementally into `usage_messages` as
`opencode:<message id>` (a subagent's with its session id as the agent), reasoning tokens counted as
output and cache writes as 5-minute ones. They're priced like Claude Code's, from the model's API list
price; a model the price table doesn't know (another provider's) adds tokens but no cost. By-model
rows name the agent first: **OpenCode · claude-sonnet-4-5**.

**Kiro's credits.** A [Kiro session](harnesses.md) has no Claude transcript: Kiro keeps each session in
`~/.kiro/sessions/cli/<id>.json`, with every finished turn's token counts (0 under its `auto` model),
the credits it billed (`metering_usage`) and how full the context was. After each turn (its `stop`
hook, and again 3 seconds later, since Kiro may write the turn just after) and when the session
exits, AgentK stores each new turn as a `usage_messages` row keyed `kiro:<Kiro's id>:<when it ended>`
with `credits` set; Claude Code's rows leave it NULL. A `usage_files` row for the session file marks
the session as read, so the backfill at start leaves it alone. Only the session's own conversation
is read ([Agents](harnesses.md#kiro)), never another session's in the same pooled worktree. Cost is priced only from rows without
credits, so credits are never mixed into it: `TokenUsage.credits` sums them per session, project,
model and day, the session footer shows **Kiro · <n> credits**, and the usage views show **Kiro
credits** beside **Est. cost**, with a **Credits** column on the by-project, by-session, by-model
and daily tables once any of their rows has some. A Kiro row with credits and no tokens (its `auto`
model counts none) shows a dash for its tokens, not 0. The by-model tables group by agent and model
(`ModelUsage.harness`, from the session's `harness`), and name a Kiro row with the agent first,
**Kiro · auto** (`modelLabel`, from `HarnessCaps.labelModels`); an agent this version doesn't know
counts as Claude Code's, in the same row. Kiro's default model is just `auto`, and Kiro never records which
model it picked, so the row can say whose it is but not which model ran. The 14-day chart gives each day a Kiro
credits bar beside its tokens bar, on a second axis on the right (tokens and credits don't share
a scale), with a legend naming each bar's axis, once any of the 14 days has credits; without
any it's unchanged. The rail's tooltip on a project adds its credits. The last turn's context fill is the session's context ring, as the mod's measurement is for
Claude.

**Meko calls.** The same transcript pass finds Claude's calls to the Meko MCP server
(`mcp__meko__<tool>`, or a plugin's `mcp__plugin_…_meko__<tool>`) and their results, in the main
transcript and subagents: `meko_calls(session_id, tool_use_id, agent, tool, kind, at, outcome,
results, input_chars, result_chars, model, context_tokens, query, query_key, top_score, top_raw, quality)`
(the last five for [What Meko doesn't know](recall-and-learn.md#what-meko-doesnt-know)). A **lookup** searches or fetches
(`memory_search`, `knowledgebase_search`, `context_search`, `memory_get_all`, `memory_get_by_id`,
`conversation_get`, `artifact_get`); a **save** writes (`memory_add`, `memory_update`,
`memory_promote`, `artifact_put`, `conversation_add_message`); anything else (datapacks,
conversations) is counted as a call only. A result can come in a later read than its call, so a call
waits with no outcome until its `tool_result` is read. Meko wraps its JSON in `{"result": "…"}`; a
lookup **found something** when its items (`results`, `memories`, context_search's buckets, or a
single record; a plain-text reply unless it says nothing was found) aren't empty, and it failed on a
tool error or a reply with a non-empty `error`. A result too large to show, which Claude Code saved to
a file, counts as found at the size of its notice, since only the notice entered the context (reading
the file is Claude's own tool call); a call moved to the background found nothing. `model` and `context_tokens` (input, cache read and write)
come from the response that made the call. Sizes are characters ÷ 4. Apart from AgentK's own
search at start (`meko_preloads`, see [Meko recall at start](recall-and-learn.md#meko-recall-at-start)), the Meko
plugin's hooks inject no recalled memory today, so the transcript is the rest of the record of what
Meko gave a session. The migration that added the table cleared the read offsets again, so older
sessions got their calls and 1-hour writes.

**Meko in the first turn.** The same pass keeps when each session's first turn ended:
`sessions.first_turn_end_at`, set once from the main transcript (subagents don't count). It's the
first of: a response with `stop_reason: "end_turn"`, an API error Claude Code wrote in the model's
place (model `<synthetic>`), or, once there has been a response, anything the user sent (a prompt,
a command, "[Request interrupted by user]"; meta lines, compaction summaries and tool results
don't count). So an interrupted or failed first turn ends there, not at a later turn's end. After
a `/clear`, a transcript read again from the start is only the current conversation's. Among
sessions whose first turn has ended, in projects with datapacks (the others had no Meko to
search), AgentK adds up about how many tokens Meko brought into that turn (characters ÷ 4, like
the tokens found): from lookups by Claude or a subagent that found something at or before that
time (`searchedTokens`), and from the search at start when it was handed over (the seed, `seedTokens`).
It also counts the sessions the search at start ran in at all (`startSearches`, whatever it
found), since older sessions predate it and a session started with no task skips it. They're `mekoFirstTurn` on the project and overview
usage, beside `meko`. The migration that added the column cleared the read offsets once more, so
sessions from before AgentK searched at start give the numbers to compare with.

**What Meko saved (an estimate).** Nobody sees the run where Meko wasn't there, so savings come
from a fixed formula (`src/server/mekosavings.ts`), not a comparison. Only searches that found
something count (`memory_search`, `knowledgebase_search`, `context_search`), plus each **seed**:
what AgentK's search at session start passed to Claude (`meko_preloads` with outcome `found`). A
seed is credited like a search made by the session's first main-agent response: *C* is that
response's context less the seed (a search's context doesn't hold its result yet), *k* counts the
responses after it, and its price is that response's model. A seed waits for that first response,
and it counts only in the savings, not in the lookups or tokens found. Without Meko, Claude
would have found the same thing by exploring: several tool calls, each re-reading the whole context
and adding file contents. The constants come from this host's transcripts (2026-09-29):

| Constant | Value | From |
|---|---:|---|
| `EXPLORE_CALLS` | 3 | Calls one search stands in for (a search of the code and two reads); sessions make a median of 14 exploring calls before their first edit |
| `EXPLORE_RESULT_TOKENS` | 2,000 | Tokens each adds: median Read ≈ 5,800, median Bash ≈ 150, so one read and two shell calls ≈ 6,000 |
| `CALL_OUTPUT_TOKENS` | 240 | Median output of a response that makes a tool call |
| `CARRY_TURNS` | 10 | Later responses counted as re-reading the extra content (the median is 211, but compaction cuts the context) |

For a search with a result of *R* tokens at a context of *C* tokens, followed by *k* =
min(10, later responses in the same conversation or subagent), with *X* = 3 × 2,000:

```
re-reads  (3 − 1) × C       cache-read price
output    (3 − 1) × 240     output price
content   X − R             1-hour cache-write price (2× input)
carry     (X − R) × k       cache-read price
```

The saving is the sum, in tokens and in USD at the calling model's price. A search (or seed) on a
model with no known price counts in the tokens but not the dollars, and the tile says how many
searches and seeds were left out.
An index on `usage_messages(session_id, agent, at)` keeps counting *k* cheap. A result over 6,000
tokens makes the content and carry terms negative, and they're kept. Session, project and overall
figures are plain sums, so the pages agree. With a context of 216k, a 1,420-token result and 10
later responses, one search saves about 483k tokens, almost all of them cache reads, so the dollar
figure is much smaller than the token count. The estimate can't show that Meko didn't help, which is
why it's labelled **Est.** A comparison of sessions with and without Meko finds was tried and
dropped: too few sessions to compare, no chance deciding which sessions used Meko, and nothing to
compare with once every session does.

**What shipped.** A merged PR is the session's `merged` event, or a Done session with a PR (Merge &
Close only ends in Done after a merge), counted once per session. A push
straight to the base branch is found in the same transcript pass: a Bash tool result that is a
push (git push's `To <remote>` line, or Claude Code's `gitOperation.push` marker, which also covers
output cut short with `| tail`) with a fast-forward `old..new  src -> <base>` ref update is stored
in `pushes(session_id, range, ref, at, commits, file_types)`, and `git rev-list --count old..new` in the
project's repo fills in `commits` (`-1` when git can't tell; the push still counts), and
`git diff --numstat -z old new` fills in `file_types`, what it changed per file extension, for the
[usage stats](usage-stats.md) (NULL when git can't tell, and for pushes found before it was added). Fetch and pull
output (`main -> origin/main`, under `From`), forced updates and new refs don't match. The
migration that added the table cleared the read offsets, so every transcript was read once more
(usage rows are upserts) and older pushes were found; transcripts of archived sessions are read at
startup, one session per tick.

**Plan limits.** On Pro and Max plans, Claude Code passes the account's 5-hour and weekly usage
(`rate_limits.five_hour` / `seven_day`: percent used and reset time) to its status line command.
Sessions are launched with AgentK's own status line (`run/statusline.sh`, set in the
`--settings` file). It sends inputs that carry limits to the server over the hook socket, in the
background, and then runs the user's own status line command with the same input, so the terminal
looks unchanged. That command is found at launch (local project settings, then project, then user
settings) and passed as `AK_USER_STATUSLINE`; sessions launched before this existed use the
user-level command saved in `run/user-statusline`; its `padding` and `refreshInterval` are copied.
If that command is Meko's `meko-statusline.sh`, only the command it wraps (`--inner-cmd`, or
`MEKO_INNER_STATUSLINE`) runs: Meko's "Meko: <datapack>" suffix would repeat what the session
screen already shows, since sessions always use the project's datapack.
AgentK's mod reports the same windows after each turn (`session.measure`), whatever the status line does.
All sessions share one account, so reports are merged per window: a window that has reset is
ignored, a later window replaces an earlier one, and a report missing a window keeps the last one.
Within a window, what counts is whether the reading is new: the server keeps each session's last
reading of each window (a report without a window keeps the last one of it). A reading that rose
since that session's last one, or fell by more than 10 points, is new data and replaces the window
even when lower, since an early reset by Anthropic keeps the reset time and only lowers the use. A
smaller fall is taken for two status line runs that reached the server out of order (each is sent
in the background), so it changes nothing; a smaller reset shows with the session's next reading.
A repeat changes nothing, so an idle session re-running its status line with an old reading can't
pull the number back. A session's first reading only raises the number, since it may be old.
The reading and each session's last one are kept in the `kv` table (`plan_limits`, `plan_limits_seen`; a session's is dropped once its windows reset), so they hold across a restart. The rail shows the plan limits as a row per agent, led by its
logo: Claude's row has a ring for each window side by side (the percent in the middle, blue,
amber at 80% and red at 95%, its name marked ▲ when amber or red), with its name and reset time
beside it. A window disappears once its reset time passes. The numbers only move when a session
gets a reply, so each ring's tooltip says when they were last reported; the age has no line of its
own, to keep the block small. Each meter names its agent: **Claude 5-hour**, **Claude weekly**.

**Kiro's plan.** Kiro has no status line, but `kiro-cli chat --no-interactive --output-format
stream-json /usage` prints the account's credits this period (about 4 s; it bills nothing):
`Estimated Usage | resets on 2026-11-01 | KIRO FREE` and `Credits (36.02 of 50 covered in plan),
72.0%` on kiro-cli 2.32.0. `KiroPlanStore` (`src/server/kiroplan.ts`) runs it at start (once Kiro's
version is read), hourly, and when the first Kiro session is created, so that session's plan shows
without waiting for the hour; never after turns. It runs only while Kiro is available and some
session (archived ones too) has run on it, so an install that never uses Kiro in AgentK never
runs it for this. One read at a time, in `run/kiro-usage`, 20 s at most, as `agentk-usage`, an
inert agent written in that folder (no tools, MCP servers or hooks, like Ask AgentK's), so the
user's default agent isn't loaded for it. Kiro keeps each run as an empty session: after every
read AgentK deletes (`chat --delete-session`) the one the run named and any other Kiro session
whose folder is `run/kiro-usage`, since a run cut off at its timeout never names its own. A server
that is stopping waits up to 5 seconds for a read and its deletes still running. `parseKiroPlan` (`src/server/kiro.ts`)
reads the plan name, the reset day and the credits line, and returns nothing without the credits
line. Paid plans' bonus and overage lines haven't been seen, so they're matched loosely (a line
with `bonus` and two numbers, `remaining` or `left` meaning what's left; a line with `overage`, its
first amount that isn't money or a percentage, and `enabled`/`disabled`), and each is kept with the
line as Kiro printed it. The reading is kept in `kv` (`kiro_plan`) and sent as `AppState.kiroPlan`;
a read that fails (signed out, say) keeps the last one and logs once. The rail shows **Kiro
credits** on Kiro's row: a ring of used over covered in Kiro's violet (amber at 80%, red at 95%;
past 100%, **Over plan**, red, or amber while overage is on, since Kiro then bills the rest and
goes on), `36 of 50 · Free · resets Nov 1` beside it, then a line for bonus and overage when Kiro
printed them and Kiro's own lines quoted under it (since how they're read is a guess). Its tooltip
says it's Kiro's estimate of all your Kiro use, with its age. The reading is dropped once its reset day has come (UTC midnight), and while
Kiro isn't available.

- **Where it shows.** The session footer and tab tooltip show the session's total and output
  tokens. The rail tooltip shows the project total. The project home has a **Token usage**
  section: headline numbers with **Est. cost**, a 14-day chart with a table view, the Meko section,
  and breakdowns by session (with a **Meko** column, found / lookups, when any session called Meko)
  and by model. **Usage** in the rail (and the command palette) opens `/usage`: totals with Est.
  cost, merged PRs and commits pushed straight to a base branch, a 14-day chart, the Meko section
  for every project, and breakdowns by project (current ones first, then deleted ones, which keep
  their sessions and usage; each row shows its repo folder and when it was last active, and has
  **Cost**, **Meko** and **Est. saved** columns, which a phone leaves out) and by model. Every
  column header sorts its table; tapping it again reverses the order.
- **The Meko section** is the same on both pages (`MekoUsageSection`): the Meko logo (black in the
  light theme, white in the dark) and tiles for lookups, lookups that found something, saves,
  **1st-turn tokens** (the tokens Meko brought into first turns, `≈ 15.3k`, with the split on one
  more line, `Seeded ≈ 3.3k · Searched ≈ 12k`, or `Seeded none yet` before AgentK's search at
  start has run in any of them; a dash before any first turn has ended; the
  tooltip says how many sessions the search at start ran in; `firstTurnText` in
  `src/web/util.ts`), tokens found, and **Est. saved** in USD and tokens (searches and seeds), green for a saving
  (a negative estimate is in the normal text color, and a dash means no search has found
  anything and no session was seeded), then two tiles for the [learn step](recall-and-learn.md#the-learn-step)
  (`MekoLearning`, from `meko_learns` and the steps' own `usage_messages` rows; `learningText` in
  `src/web/util.ts`): **Learned**, the memories it saved, with `2 corrected · 3 steps` on the line
  under it and a tooltip that adds the facts found already known, the steps that didn't finish and
  the Merge & Close sessions it was skipped in; and **Learning cost**, the steps' model calls in USD
  with their tokens under it (tokens alone when no model had a price). Both are a dash before any
  step ran. It shows once there's any Meko call, seed or learn step, or before that while Meko is on
  (and, on the project home, the project has a datapack). On the project home only, a line under
  the tiles says how many questions Meko couldn't answer, with the split (`gapsLine` in
  `src/web/util.ts`), and links to [What Meko doesn't know](recall-and-learn.md#what-meko-doesnt-know);
  it's left out while none are open.
- **What's counted.** Four numbers, kept separate: uncached input, cache writes, cache reads and
  output. Cache reads dominate long sessions, so the headline total is shown next to output. The
  cost is an estimate at API list prices, not a bill.
