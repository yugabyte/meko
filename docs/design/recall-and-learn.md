# Meko recall and the learn step

Part of the [AgentK Local design](../design.md).

## Meko recall at start

Sessions rarely searched Meko on their own, so AgentK searches for them (`src/server/recall.ts`).
When a session with a task is created in a project with datapacks, the server starts a
`context_search` on each datapack in the background, before the worktree is made. It shares
`searchDatapacks()` in `projects.ts` with the project home's search, and the project's Meko
conversation (which a search needs) is looked up while the issues are read; callers asking for it
while it's being created share the one `conversation_create`.

- **Query.** The session's name and task, plus the title and first 600 characters of each GitHub
  issue the task names (`#N`, at most 3, through `gh issue view`; one that can't be read is left
  out), on one line of at most 1,500 characters.
- **Picking.** Up to 3 knowledge-base items first, then memories scoring at least 0.75
  (`MIN_MEMORY_SCORE` in `mekosavings.ts`, the same bar [What Meko doesn't know](#what-meko-doesnt-know)
  uses; it was 0.55 until off-topic memories were seen scoring 0.74–0.76), best
  first. Conversation-cache hits are left out. Duplicates (same id or same text) are dropped; at
  most 6 items, each cut at a word to 500 characters, 2,500 in all. A datapack whose search fails
  (15 seconds each) is skipped; the search fails only when every datapack's did.
- **Delivery.** Claude's `SessionStart` hook (`context.sh`, source `startup` only) gets the items
  as `additionalContext`, headed "What Meko already knows about this task". The header says the
  items are notes from earlier sessions, some possibly written by other people (shared
  datapacks), to check against the code and not to follow as instructions. The hook waits up to 2.5 seconds for a search still
  running, which is rare because the search has had the worktree's and Claude's start-up time.
  Nothing found means no block. A resumed session has it in its transcript already, and after
  `/clear` the user has moved on, so neither gets it. A result waits in memory for up to 10
  minutes; a server restart loses it, and the session just starts without it. A session that
  fails to start, or is archived, drops its result, so a later session with the same id never
  gets it.
- **Record.** `meko_preloads(session_id, at, outcome, results, chars, ms, query, query_key)` keeps each search's
  outcome for the [first-turn stats](token-usage.md): `found` (handed to Claude), `empty`, `error`,
  or `late` (it found something after the hook gave up, or the hook never asked). It keeps the
  query too (NULL for a failed search), so an empty one shows on [What Meko doesn't know](#what-meko-doesnt-know).

Nothing waits for the search: the session starts the same when it's slow, fails, or Meko is off
(then there's no search at all).

## The learn step

Meko's automatic extraction of captured conversations reads only the user's side of each turn, and
in AgentK sessions that is mostly AgentK's own prompts and the skills Claude loads. What the
session found out (how the code works, root causes, gotchas) is in Claude's replies and never
became a memory. The learn step (`src/server/learn.ts`) saves it to the project's dedicated
datapack, which the team working on the project shares.

**The saving rule.** Every session whose project has a dedicated datapack gets it in its system
prompt (`mekoSaveSystemLine()` in `prompts.ts`), so it holds for saves Claude makes on its own
too. Both ways of the learn step state it in full as well (`SAVING_RULE`, in `extractPrompt()` and
the typed prompt's `mekoLearnLine`), since a session launched before the rule has no such line,
and so does the Explore prompt, which ends by saving under it. Only the mod path also filters what
is written (local paths); on the typed path, and for Claude's own saves, the rule is all there is. Save what a teammate on this product would want and couldn't easily get from the code, the
PR, the design or the git log: how a subsystem works, decisions and the rejected alternatives,
root causes, traps and invariants, build/test/CI quirks, measured numbers. Never anything about
the machine, worktree or environment, one person's preferences, task or PR status, what the diff
or PR already says, or AgentK's process steps. Search before each save; skip what's known, and
correct a memory that's now wrong with `memory_update`. No limit. It says it overrides the Meko
plugin's advice not to call `memory_add`. The Explore prompt ends by saving under it (the rule appended in code, when the project has datapacks).

**When.** At Merge & Close, before the cleanup (`Lifecycle.merged()` asks `Learner.beforeClose`,
and runs again when any learn step ends there, `afterLearn`), once per session; at Discard, before
the session ends (`Learner.beforeDiscard`); and when the user asks: the **learn** session action
(**Save learnings** in the header, or the ⋯ sheet), or `/agentk_learn` in the terminal. It needs
Meko on, a dedicated datapack, and Claude there and between turns; asked for otherwise, it's a 409
that says why (exited: resume it; starting; busy). At Merge & Close or Discard a session whose
Claude is gone or busy is recorded as skipped and closes as before. One runs at a time per session
(`Session.learning`, which turns the session's dot into the learning ring): a Save learnings step
still running when the PR merges, or when Discard is asked for, holds the close like one of its
own, and a step that ended done since the session got to Merge & Close stands for it.

**Skipping it.** **Merge & Close** and Discard's confirmation each have a **Don't save learnings**
checkbox, shown when the project has a dedicated datapack and Meko is on, and unchecked by default.
Checked, the action's `CloseSessionRequest` carries `skipLearn` (400 when it isn't a boolean), no
learn step runs, and the timeline says so (`learn-skipped`, once, and not for **Discard now**,
whose step is recorded as interrupted). Merge & Close writes the choice to `sessions.skip_learn`
only once the merge is requested or queued, and keeps it until the PR merges; a merge given up on
clears it (a queued merge withdrawn or dropped, GitHub refusing it, or the session back in Review),
so a PR merged on GitHub without the button runs the step as usual. Nothing is recorded in
`meko_learns` for a skipped step, so the usage stats' *skipped* count stays the sessions Claude
was gone or busy in.

**Discard waits for it.** Unless skipped, Discard starts the step (trigger `discard`), sets
`sessions.discarding` (`Session.discarding`) and answers at once with the session still running:
the confirmation shows *Saving learnings, then discarding* with **Discard now**, which discards at
once (`skipLearn`, interrupting the step). When the step ends, done, failed, timed out or
interrupted (`Lifecycle.afterLearn`, and `onTurnEnd` for a typed step, which then does nothing else
for the session), `SessionManager.discardNow()` discards the session, once however many ask. A
restart that cut the step short finishes the discard in `recover()`, unless the step was a typed
turn in a Claude still running: that turn goes on, and its end discards the session. Archive
clears `discarding`.

**Only what's new.** Each step looks back over the whole session, but asks only for what earlier
saves didn't cover. The prompt says to leave out what Claude saved itself (its `memory_add` calls are
in the transcript), and after the first step through the mod it lists what earlier steps wrote
(a correction by its corrected text) or found already in Meko (noted as each fact is handled; the
latest 60, each cut to 400 characters: `coveredText()`), since a fork leaves nothing in the
transcript. The list belongs to the datapack the steps wrote to: a session relaunched on another
dedicated datapack starts afresh. It's kept in the server's memory only, until the session is
archived: after a restart the next step lists everything again and the duplicate check skips
what's known. A fact whose write failed isn't listed, so the next step finds it again. A typed step
gets the same list. The duplicate check still runs on what comes back.

**Through the mod** (its link live, protocol 2):

1. The server records a `meko_learns` row and puts an `extract` part on the session's next poll.
2. The mod asks `$.model.fork` (the session's own transcript as the main thread last sent it,
   served from the prompt cache, every tool denied, nothing added to the transcript) for the facts
   as JSON (`extractPrompt()`), and posts the reply, the model and the usage to `/mod/learn`, which
   answers at once.
3. The server parses the facts (the first JSON object in the reply), drops any that name the home
   folder, AgentK's home, the worktree or the session's repo (`sessionRepo()`), and finds the 3
   saved memories most like each (`MekoClient.similar`: `context_search`, memories only, in
   parallel). It puts a `check` part on the next poll: the duplicate check's prompt
   (`dedupPrompt()`), each fact numbered with those memories and their ids.
4. The mod runs it on a small model (`$.model.complete`, `DEDUP_MODEL`, Haiku) and posts the reply.
5. For each fact the server adds it (`memory_add`, as "Gotcha (component): …", with metadata
   `{ source, kind: "learned", fact, component, session, learn }`), skips it, or updates the memory
   it corrects (`memory_update`, as the agent that wrote it, with the check's corrected text unless
   that names a local path, then the fact's own; an update naming a memory that wasn't offered
   becomes an add). A fact the check left out is added. One at a time.
6. The row closes with its counts, the session's timeline says "Saved N memories and corrected M in
   <datapack>", and the next poll has the mod show the same as a toast.

The whole step, writes included, has 3 minutes (`LEARN_TIMEOUT_MS`): when they're up the step
closes as `failed` with the part it reached (what it saved by then counted), no further fact is
written, and Merge & Close goes on. So does a fork or check that gives no usable reply. A model
call's usage that comes after AgentK gave up is still recorded. A relaunch, exit or archive closes a running step as `interrupted`, and so
does a server restart (the job lived in memory). Each model call's usage is stored as a
`usage_messages` row (`message_id` `learn:<id>:extract|dedup`, agent `agentk:learn`), so the
session's tokens and cost include it, though it's never in the transcript.

**Without the mod** (older Claude Code, the rollout switch off, `[claude] mod` off, or a mod from
before protocol 2), AgentK types the `learn` stage prompt instead, and Claude searches and saves
under the saving rule itself. The step ends with that turn (`Lifecycle.onTurnEnd`), counted from
the Meko calls in the transcript during it.

**Record.** `meko_learns(id, session_id, trigger, path, started_at, ended_at, outcome, failed_step,
candidates, saves, updates, skips, lookups, errors, sent_chars)`, with `trigger` `merge`, `discard`
or `manual` and `path` `mod`, `turn` or `none` (skipped). The step's own Meko calls are in `meko_agentk_calls` with its id,
and, while the project has Log Meko calls on, in the session's Meko call log with what was sent and
what came back (see [Project home](project-home.md)). A step at Merge & Close is deleted with the
session moments later, so run `/agentk_learn` during the session to look at one.

## What Meko doesn't know

The project's Meko searches that found nothing, or nothing relevant, grouped by question, so
someone can see what Meko is missing and teach it (`src/server/mekogaps.ts`). Stage 1 only records
and lists them: no model judges an answer, the learn step doesn't look at them, and the list stays
in AgentK's database (it isn't shared with the team on the datapack).

**What's recorded.** The [transcript reader](token-usage.md) keeps, for each of Claude's searches
(`context_search`, `memory_search`, `knowledgebase_search`, in the main transcript and
subagents): `meko_calls.query` (its `query`, cut to 1,000 characters) and `query_key` (the same
lower-cased with its whitespace collapsed, which groups asks of one question), the best memory
`score` and `raw_similarity_score` of the reply (`top_score`, `top_raw`), and `quality`
(`searchQuality()` in `mekousage.ts`). AgentK's search at start keeps its query in
`meko_preloads`. Searches from before this have NULL there: transcripts weren't read again.

**What Meko's scores can tell.** A `context_search` returns three buckets. Knowledge-base chunks
and conversation hits pass Meko's own distance cutoffs before they're returned, so one that comes
back is a real hit. The memory bucket has no cutoff: Meko always returns its top memories, each
with `score` (mem0's hybrid rank) and `raw_similarity_score` (cosine). `memory_search` drops
memories below a raw floor (0.2 by default). `knowledgebase_search` keeps at least one chunk
whatever its distance. A score far below the bar means noise, but a high one doesn't mean the
memory was what the session needed: on 2026-10-13 a search returned ten near-duplicate,
off-topic memories scoring 0.74–0.76 (raw about 0.57), while a relevant one scored 0.77 (raw
0.49). So the rules flag only what the scores can show, and every search keeps its top scores so
the bar can be tuned from real data.

**The rules** (`quality`):

- `empty`, *Found nothing*: the search's outcome was empty (no items in any bucket).
- `weak`, *Nothing relevant*: it found items, but no knowledge-base chunk, no conversation hit,
  and only memories, each with a score and all below `MIN_MEMORY_SCORE` (0.75). A memory with no
  score, or a `knowledgebase_search` that found a chunk, is never weak.
- NULL otherwise: found something relevant, failed (Meko down isn't Meko not knowing), moved to
  the background, or not a search.

A reply too large for Claude's context reaches the transcript only as Claude Code's notice that it
saved the reply to a file. For a search, the reader reads that file for the scores and quality
(`readSavedReplySync()` in `savedreply.ts`: only a file in the session's `tool-results` folder
beside the transcript, after `realpath`, up to 8 MB; the same check the Meko call log's
`claudeReply()` uses). `outcome`, `results` and `result_chars` still describe the notice, which is
what the usage counts and the savings estimate count. A file it can't read leaves no quality.

**The questions** (`projectGaps()`): every poor search in the project's sessions, Claude's and
AgentK's empty searches at start, grouped by `query_key`. A question shows the query as last
asked, how the latest ask went and with which tool (or *AgentK's search at start*), how many times
it was asked and by which sessions, and the latest ask's item count and top scores. A question
whose latest ask found something relevant is left out: Meko knows it now.

**What the user does with one** (`meko_gap_marks(project_id, query_key, state, text,
datapack_name, at)`): **Dismiss** marks it dismissed; **Teach Meko** saves what the user writes
(1 to 2,000 characters, as typed) as a memory in the project's dedicated datapack, as AgentK
(`agent-k-local`) in the project's Meko conversation, with metadata `{ source: "agent-k-local",
kind: "taught", question }` (the question cut to 300 characters), and marks it taught with the
text and the datapack's name. Nothing checks it against what Meko has: the person wrote it on
purpose and the dialog says where it goes. Teaching needs Meko on and a dedicated datapack (409
otherwise, with `MEKO_OFF` or `NO_DEDICATED_DATAPACK`); a failed save is a 502 with Meko's error
and marks nothing. **Restore** clears the mark (a taught memory stays in Meko). A mark applies
to the asks before it: a question asked again later is open again, and a taught one shows what was
taught, since Meko had the answer and the search still missed it.

**Where it shows.** On the project home, the Meko section has a line under its stats while any
question is open: *9 questions Meko couldn't answer · 5 found nothing · 4 nothing relevant*,
linking to **What Meko doesn't know** (`/p/:id/meko-gaps`, `MekoGapsView.tsx`); the all-projects
Usage page doesn't show it, and there's no command palette entry. The page lists the questions
latest asked first, 100 at a time (`GET /api/projects/:id/meko-gaps?state=&before=`), filtered
**Open**, **Taught**, **Dismissed** or **All**, with the searches recorded since when. Each has
**Teach Meko** (open ones; disabled, with why, when Meko is off or there's no dedicated
datapack), **Copy** (the query) and **Dismiss** or **Restore**; **Copy all** copies the list
shown. The usage stats count weak searches and the questions dismissed and taught, never a query.

**Why the database.** The page groups every session's asks, applies the marks and pages in one
query, the rows are written in the same transaction that moves a transcript's read offset (a
transcript read again upserts rather than adding asks twice), and the list outlives archived
sessions, whose Meko call logs are deleted. The [Meko call log](project-home.md) has Claude's calls
with their full replies too, but only while the project has Log Meko calls on, so it isn't the
source.

