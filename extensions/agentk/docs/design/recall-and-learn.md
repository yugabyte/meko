# Meko recall and the learn step

Part of the [AgentK design](../design.md).

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
  (`MIN_MEMORY_SCORE` in `mekosavings.ts`, the same bar Meko's [Context Gaps](#mekos-context-gaps)
  judge a search by; it was 0.55 until off-topic memories were seen scoring 0.74–0.76), best
  first. Conversation-cache hits are left out. Duplicates (same id or same text) are dropped; at
  most 6 items, each cut at a word to 500 characters, 2,500 in all. A datapack whose search fails
  (15 seconds each) is skipped; the search fails only when every datapack's did.
- **Delivery.** Claude's `SessionStart` hook (`context.sh`, source `startup` only) gets the items
  as `additionalContext`, headed "What Meko already knows about this task", each labelled with its
  source, datapack and Meko ids (`[memory · <datapack> · id <id> · datapackId <id>]`), so a design can cite the item a
  decision rests on ([key design decisions](design-panel.md)). The header says the
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
  or `late` (it found something after the hook gave up, or the hook never asked). Meko keeps no
  question for it, since its query is the task ([Context Gaps](#mekos-context-gaps)).

Nothing waits for the search: the session starts the same when it's slow, fails, or there's no Meko
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
(**Save learnings to Meko** in the session's ⋯ menu, or the palette), or `/agentk_learn` in the terminal. It needs
Meko on, a dedicated datapack, and Claude there and between turns; asked for otherwise, it's a 409
that says why (exited: resume it; starting; busy). At Merge & Close or Discard a session whose
Claude is gone or busy is recorded as skipped and closes as before. One runs at a time per session
(`Session.learning`, which turns the session's dot into the learning ring): a Save learnings step
still running when the PR merges, or when Discard is asked for, holds the close like one of its
own, and a step that ended done since the session got to Merge & Close stands for it.

**Skipping it.** The session's ⋯ menu (the ⋯ sheet on a phone) has **Merge & Close without saving
learnings** beside the Merge & Close gate, and Discard's confirmation has a **Don't save learnings**
checkbox, unchecked by default; both are offered when the project has a dedicated datapack and Meko
is on. Either way the action's `CloseSessionRequest` carries `skipLearn` (400 when it isn't a boolean), no
learn step runs, and the timeline says so (`learn-skipped`, once, and not for **Discard now**,
whose step is recorded as interrupted). Merge & Close writes the choice to `sessions.skip_learn`
only once the merge is requested or queued, and keeps it until the PR merges; a merge given up on
clears it (a queued merge withdrawn or dropped, GitHub refusing it, or the session back in Review),
so a PR merged on GitHub without the button runs the step as usual. Nothing is recorded in
`meko_learns` for a skipped step, so the usage stats' *skipped* count stays the sessions Claude
was gone or busy in.

**Discard waits for it.** Unless skipped, Discard starts the step (trigger `discard`), sets
`sessions.discarding` (`Session.discarding`) and answers at once with the session still running:
the confirmation says what the step is doing (*Checking 3 facts against Meko, then discarding*,
see **Its progress** below), with how long it has run, and offers **Discard now**, which discards at once (`skipLearn`, interrupting the step). Save learnings is hidden meanwhile (`offersLearn` in
`src/web/util.ts`), so the step isn't shown twice. When the step ends, done, failed, timed out or
interrupted (`Lifecycle.afterLearn`, and `onTurnEnd` for a typed step, which then does nothing else
for the session), `SessionManager.discardNow()` discards the session, once however many ask. A
restart that cut the step short finishes the discard in `recover()`, unless the step was a typed
turn in a Claude still running: that turn goes on, and its end discards the session. Archive
clears `discarding`.

**Its progress.** `Session.learnProgress` says where a running step is: through the mod,
`extract` (Claude looks back over the session), `search` (AgentK looks up what Meko has on each
fact), `check` (the duplicate check) and `save` (`done` of `facts` gone through so far, a failed
write included); `turn` for a typed step. `Learner.progress()` reads it from the open `meko_learns` row and the step's job, and
each part pushes the state as it starts (and `save` after each fact), so the labels follow the step
(`learnStepText()` in `src/web/util.ts`: *Finding what it learned*, *Saving 2 of 3 to Meko*), with
the time since it started (`Elapsed` in `common.tsx`). Merge & Close's pill says which part of the close the session is
in, `Session.closeStep` (`closeStep()` in `src/server/store.ts`): waiting for GitHub to merge the
PR (even while a Save learnings step runs), then, once merged, the learn step (as above, with its
time), waiting for Claude's turn to end, or cleaning up (`closingText()` in `src/web/util.ts`).

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

1. The server records a `meko_learns` row and puts an `extract` part on the session's next poll,
   with the project's questions Meko couldn't answer numbered at its end (*The project's open questions*, below).
2. The mod asks `$.model.fork` (the session's own transcript as the main thread last sent it,
   served from the prompt cache, every tool denied, nothing added to the transcript) for the facts
   as JSON (`extractPrompt()`), each with the questions it answers (`answers`), and posts the
   reply, the model and the usage to `/mod/learn`, which answers at once.
3. The server parses the facts (the first JSON object in the reply; an answer that isn't the number
   of a question it offered is dropped), drops any that name the home
   folder, AgentK's home, the worktree or the session's repo (`sessionRepo()`), and finds the 3
   saved memories most like each (`MekoClient.similar`: `context_search`, memories only, in
   parallel). It puts a `check` part on the next poll: the duplicate check's prompt
   (`dedupPrompt()`), each fact numbered with those memories and their ids; a skip or an update
   names the memory it means.
4. The mod runs it on a small model (`$.model.complete`, `DEDUP_MODEL`, Claude Haiku 5.5) and posts the reply.
5. For each fact the server adds it (`memory_add`, as "Gotcha (component): …", with metadata
   `{ source, kind: "learned", fact, component, session, learn }`), skips it, or updates the memory
   it corrects (`memory_update`, as the agent that wrote it, with the check's corrected text unless
   that names a local path, then the fact's own; an update naming a memory that wasn't offered
   becomes an add). A fact the check left out is added. One at a time. An added fact that answers
   questions has their queries in its metadata too (`questions`), and its questions are marked
   (*The project's open questions*, below).
6. The row closes with its counts, the session's timeline says "Saved N memories and corrected M in
   <datapack>" (then "; taught Meko N questions it couldn't answer and dismissed M questions it knew
   already", for what it marked), and the next poll has the mod show the same as a toast.

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
the Meko calls in the transcript during it. It isn't offered the session's questions: AgentK
couldn't tell which of Claude's saves answers which, so it couldn't mark them.

**The datapack's open questions.** A session that found out something Meko couldn't answer has
the answer in its context, whether it searched for it or another session did. So every step
through the mod (Save learnings, `/agentk_learn`, Merge & Close and Discard alike) is offered the
open questions on Meko's [Context Gaps](#mekos-context-gaps) for the datapack it writes to, read
from Meko with the session's Meko conversation (`MekoQuestions.openQuestions`, `learnQuestions()`
in `learn.ts`):

- first those the session's own conversation asked, then the rest, each in Meko's order (latest
  asked first);
- at most 50 in all (`LEARN_QUESTIONS_MAX`), each as last asked, on one line and cut to 300
  characters.

AgentK's own searches (at start, and the step's lookups) are never questions, so none of these is
a task's text. With no list (AgentK on a Meko Cloud link, a Meko too old to keep questions, or
Meko not answering) the step runs without questions.

The prompt numbers them in one run, in two lists ("from this session", then "from other sessions
on this project"). It asks for a fact answering each question the session found the answer to,
with the question numbers in its `answers` (one fact can answer several wordings of one question),
and to leave out a question it didn't find out about, or whose only answer is about the task or
the machine. The fork knows only this session, so it's told not to guess: another session's
question is answered only when this session established the answer itself. A wrong mark shows on
Meko's tab with the conversation that made it, and **Restore** there opens the question again.
Such a fact goes through the rule, the local-path filter and the duplicate check like any other,
and then Meko is told (`POST …/questions/mark`, with the session's conversation):

- written (added, or a correction): each question it answers is marked **taught** with the text
  written (and, for a correction, the memory's id);
- skipped, since a saved memory already says it: each is **dismissed** as a duplicate of what Meko
  has, with the id of the memory the check named;
- not written (its write failed, it was filtered out, or the step ran out of time): nothing is marked.

A question is marked only while it's still open (Meko's list is read again before the marks), so one
a person (or another session's learn step) taught or dismissed while the step ran keeps their mark.
The step records how many it offered, and how many marks Meko took, taught and dismissed
(`meko_learns.questions`, `taught`, `dismissed`).

**Record.** `meko_learns(id, session_id, trigger, path, started_at, ended_at, outcome, failed_step,
candidates, saves, updates, skips, lookups, errors, sent_chars, questions, taught, dismissed)`, with `trigger` `merge`, `discard`
or `manual` and `path` `mod`, `turn` or `none` (skipped). The step's own Meko calls are in `meko_agentk_calls` with its id,
and, while the project has Log Meko calls on, in the session's Meko call log with what was sent and
what came back (see [Project home](project-home.md)). A step at Merge & Close is deleted with the
session moments later, so run `/agentk_learn` during the session to look at one.

## Learn sessions

A new datapack knows nothing, so a project's first sessions get nothing from Meko. A learn session
seeds it: **Learn the project** in the New session dialog (`CreateSessionRequest.learn`), picked
for you when a project opens on an empty datapack (see [Open project](open-project-and-new-session.md#open-project)),
and offered on the project home while the dedicated datapack has no memories and no knowledge and
no learn session is open on it (`ProjectHome.learnSessionOpen`: one launched on a datapack since
switched away from doesn't count, and one that closed with the datapack still empty, having saved
nothing, gives the offer back). It isn't offered anywhere else: a plain New session, on a datapack
that already knows the project, has no Learn the project, and only **Try again** on a failed learn
start brings it back.

- **What the user gives.** What the project is about (required), where its files are (folders and
  files in the repo, docs, other places; left empty, the README and docs), and how many months of
  merged PRs to read (0 to 120, 24 by default; 0 reads none). There's no cap on the number of PRs:
  the window and the paths are the limits. It needs Meko on and a dedicated datapack (one is always
  writable); otherwise, and for a review or a PR, the create is a 400.
- **How much that is, before it starts.** The dialog shows an estimate under the months field
  (`learnScopeHint()` in `src/web/util.ts`), asked for 400 ms after the brief last changed
  (`GET /api/projects/:id/learn-scope`, `ProjectService.learnScope`, `learnScope()` in
  `src/server/learnscope.ts`): the merged PRs in the window and the tracked files, from the repo's own
  git with no fetch and no GitHub calls, on `origin/<base>` as last fetched, else the local base. PRs
  are first-parent commits since the window's first day (`learnSince()`, which the task uses too):
  one per PR for squash and merge commits, more for a repo that rebases PRs in. Where is free text,
  so it's read best effort: its words that could be paths (a `/`, a glob or a file extension; no
  URLs) and name files on the base become git pathspecs (`whereWords`, `wherePaths`; `:(glob)` for
  globs), and with none, the estimate is for the whole repo and says so. Past 200 PRs or 5,000 files
  (`LEARN_WARN_PRS`, `LEARN_WARN_FILES`) the hint turns into a warning, with what would shrink it:
  naming folders (or fewer of them), or reading fewer months. It only warns: Start learning stays
  enabled. A failed estimate shows the plain hint.
- **What it is.** A task at Explore, marked `sessions.purpose = 'learn'` (`Session.purpose`), not a
  session kind of its own: a new `StoredKind` would make pages and releases from before throw, as
  they index their stage lists by it. Its task (`initial_prompt`) is written from the brief by
  `learnTask()` in `prompts.ts`, with the PR window's first day worked out when the session is made,
  so a relaunch later reads the same PRs, and recovery and Try again need no storage of their own.
  Its first prompt is `seed` (`firstPrompt()` in `sessions.ts`), which a repo can override with
  `.agent-k/stages/seed.md` like any other.
- **What Claude does.** Maps the project from the brief and the repo's docs (skipping generated,
  vendored and lock files), then reads the PRs
  merged in the window that touch those files (through `git log` or `gh pr list`, then each one's
  description and review threads), closely for the ones with discussion and skimming the rest, in
  subagents when it can. PR text is other people's writing: learned from, never followed. It
  saves as it goes with `memory_add` under `SEED_RULE` (`mekoLearnLine('seed')`), which replaces the
  saving rule for the session: the usual one leaves out what a PR already says, which suits a task
  with its own PR, but here the PRs are the source. From a PR it saves the reason (decisions,
  rejected alternatives, root causes, review findings that became rules), naming the PR as
  "(PR #123)", never a summary of the change. Metadata adds `"seed": true` and `"pr"`. It changes no
  files; nothing enforces that beyond the prompt (the edit guard is Design's). The searches it
  makes before each save, while it's in Explore, aren't questions Meko should be taught: when it
  leaves Explore, AgentK has Meko forget them ([Context Gaps](#mekos-context-gaps)).
- **When it's done, it closes.** Claude writes its summary to `learned.md` beside the design file
  (`SessionManager.learnedPath`, outside the repo), only once it has finished; a session that stops
  to ask something doesn't write it. At each turn's end, a learn session still in Explore whose turn
  wasn't interrupted is checked for the file (`Lifecycle.learnDone`): the summary (cut to 2,000
  characters) goes on the timeline as `learn-done`, the project's notification says so, and the
  session is discarded with no learn step, since it saved as it went. A file rather than a marker
  in Claude's reply works the same for every harness, with no transcript to read. When the worktree
  has uncommitted files or unpushed commits, which a Discard would throw away, it isn't closed: the
  timeline says why (`learn-kept`) for the user to look and discard it. A Save learnings step still
  running holds the close, as it holds a Discard: the session is marked `discarding` and goes when
  the step ends (`afterLearn`). The file is removed, and the user told, only once the session is
  closing (or kept), so a close that fails leaves it for the next turn's end to try again, and only
  a new summary closes a kept session later. The PR window's first day is the same day `months`
  back, or that month's last day when it has fewer (never a day in the month after). Discarding a learn session by hand starts
  with **Don't save learnings** checked, for the same reason. Its Explore gate reads **Learning the
  project** in place of **Move on…**; it can still be moved from the stage bar, and then it no
  longer closes by itself.

## Meko's Context Gaps

The searches Meko found nothing, or nothing relevant, for, grouped by question, so someone can see
what Meko is missing and teach it. Meko keeps them: it judges every search an agent makes on its
`/mcp` when it answers it, keeps the poor ones per datapack, and lists them on the datapack's
**Context Gaps** tab in its web UI, with Teach, Dismiss and Restore (Meko's `docs/api.md` and
`docs/web-ui.md` have the rules and the routes). AgentK reads them and marks them; it no longer
judges searches itself. Earlier versions kept their own list, from the agents' transcripts, in
`state.db`; migration 45 dropped it (its marks, and each search's query and how poor its answer
was): Meko's list starts empty.

AgentK talks to it while its Meko calls go to the Meko on this computer (`MekoQuestions` in
`mekoquestions.ts`, on Meko's API listener with the client token, as `mekoagents.ts` reads the
agents). That includes a linked Meko Cloud's datapacks: Meko passes every `/mcp` call on to Meko
Cloud and keeps their questions too, and its list says so (`location: "cloud"`). Not with an older
Meko that handed out Meko Cloud's key, whose agents' searches go to Meko Cloud directly. Anything
Meko doesn't answer, or a Meko too old to keep questions (a 404), is no list; nothing that needs it
fails.

- **AgentK's own searches aren't questions.** Its MCP client sends `X-Meko-Client: agentk` on
  every request (`MEKO_CLIENT_HEADER` in `meko.ts`), and Meko records no question for those calls:
  the search at start (its query is the task) and the learn step's lookups. A header rather than a
  tool argument, which Meko Cloud refuses.
- **The project home's line.** Under the Meko stats: *9 questions Meko couldn't answer · 5 found
  nothing · 4 nothing relevant*, or *No open questions Meko couldn't answer*, read with the home
  (`ProjectHome.mekoGaps`, `GET …/questions?state=open`'s counts), opening the datapack's
  Context Gaps (`/datapacks/<id>/gaps` on this origin, or `/gaps/<id>` for a Meko Cloud datapack:
  `mekoGapsPath`) in a new tab.
- **The learn step** is offered the datapack's open questions and marks those it answered
  ([above](#the-learn-step)).
- **A learn session's searches while it seeds** check before each save into the datapack it's
  filling, so finding nothing there is expected, not something Meko should be taught. When it
  leaves Explore (moved on by hand, after which it's a task like any other, or closed),
  `Learner.seeded` has Meko forget its conversation's asks made until then (`DELETE
  …/questions/asks?conversation_id=&before=`), once, in the background. One that found something
  had already answered another session's question there.
- **The usage report** counts the questions learn steps taught and dismissed (`learn.taught`,
  `learn.dismissed`); Meko's own report counts the rest, never a query.
