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
- **Picking.** Up to 3 knowledge-base items first, then memories scoring at least 0.55, best
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
- **Record.** `meko_preloads(session_id, at, outcome, results, chars, ms)` keeps each search's
  outcome for the [first-turn stats](token-usage.md): `found` (handed to Claude), `empty`, `error`,
  or `late` (it found something after the hook gave up, or the hook never asked).

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
