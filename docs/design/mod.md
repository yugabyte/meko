# AgentK's mod in Claude

Part of the [AgentK Local design](../design.md).

Claude Code runs *mods*: plugins of TypeScript function hooks that run inside the Claude process
(early access). AgentK Local ships one, `agentk` (`src/mod/agentk`, a couple of hundred lines), that
gives the server a way into each session besides the terminal. The server copies it to
`run/mod/agentk` at startup, writing only files that changed, each through a temp file renamed into
place; when a source file is missing or a write fails, it removes the run copy, so sessions start
without the mod rather than with an old or half-updated one. Claude reloads a `--plugin-dir` mod whenever its files change, so an update reaches running
sessions without a relaunch, and a plain restart reloads nothing. Every launch adds
`--plugin-dir run/mod/agentk` and `CLAUDE_CODE_ENABLE_FUNCTION_HOOKS=1`, since hooks modules of
installed plugins are otherwise off unless Claude Code's own rollout turns them on. That variable
only helps while the rollout hasn't decided: when its switch serves off, Claude loads no plugin's
hooks module whatever the variable says, and AgentK types prompts as before. It does this when:

- `[claude] mod` is on (the default);
- the mod's files are there;
- `claude --version` (read at startup, then at most every 10 minutes) is 2.1.287 or later. Older
  versions frame a plugin's prompt as the plugin's message, not the person's (before 2.1.286),
  or may lack what the learn step uses (`$.model.fork`, `$.model.complete`, `$.command.register`
  and `command.run`, all in 2.1.287's declarations; 2.1.286's couldn't be checked).

**Claude Code's version.** AgentK installs Claude Code with Anthropic's installer (`install.sh`) and
never updates it; Claude Code updates itself, unless that's turned off or it hasn't run for a while.
`ClaudeCode` (`claudecode.ts`) keeps the version read above and puts it in `AppState.claudeCode`,
with whether it runs the mod. A running session keeps the version it started with until Claude
restarts in it, so what matters is the version the next launch gets. While `[claude] mod` is on and
that version is older than 2.1.287, a banner says so, and that AgentK types its prompts instead:

- **Update Claude Code** runs `claude update`, Claude Code's own updater (`POST /api/claude/update`,
  one at a time, stopped after 5 minutes), then reads the version again;
- the outcome shows as its own banner: updated (running sessions keep their version until Claude
  restarts in them), nothing newer found (its release channel or the organization's settings may
  hold it back), or failed, with the last lines it printed and what to run on the host instead;
- each can be dismissed (the too-old banner once per version, in the browser's storage).

When Claude Code's rollout switch keeps mods off despite a new enough version, nothing shows: an
update can't fix it, and the switch flips back on by itself.

The mod reaches the server on the hook socket, with the session's `AK_HOOK_SOCK` and
`AK_HOOK_TOKEN`, like the hook scripts. Its routes answer with `Connection: close`, so a mod that
keeps polling on one connection can't hold the socket open. Before the socket closes (a stop, or a
rebind after its file went missing) the server answers every held poll and holds none until it's
bound again, so no hook is kept out for the 20 seconds a held poll would take:

| Route | What it does |
|---|---|
| `/mod/hello` | Sent once per load, from `session.start`, with the mod's protocol: 2 runs learn steps, so a session still running a mod from before them gets the typed learn prompt instead. Every poll names it too, since a server restarted since the hello hears it only there. |
| `/mod/poll` | A loop the mod keeps going with `$.clock.after` (a reload drops the old module's timers, so only the new loop polls). The poll carries the ids of the prompts the mod submitted that Claude still holds and its protocol (`{ held: [...], protocol }`). The server holds each poll up to 20 seconds and answers the prompts it has for the session (`{ prompts: [{ id, text }] }`, with `learn: { id, kind, prompt, model? }` when a part of a learn step is waiting and `toast` when the mod should show a line), or 204. A newer poll answers the one before it with nothing. Unknown tokens get 403; a closing socket, or `[claude] mod` turned off, 503. The mod waits 3 seconds after any failure. |
| `/mod/ack` | `accepted` for each prompt as it arrives. The server answers 204 when it still wants the prompt, and only then does the mod hand it to `$.prompt.submit({ text, asUser: true })`, so the model reads it as the person's own words; 410 (it gave up on the prompt) or no answer, and the mod drops it. `dropped` with a reason, and whether it filled the prompt box, when another plugin's `prompt.submit` hook refused it. |
| `/mod/measure` | From `session.measure` after each turn: the context window's fill, and the plan limits. |
| `/mod/held` | `{ held: [...] }`, as on a poll, as soon as it changes: a held prompt's `$.prompt.submit` resolved (its turn started) or another plugin refused it. |
| `/mod/ask` | From a `tool.call` hook on Claude's `AskUserQuestion`: `{ id, questions }` (the call's `tool_use_id` and its questions). The server holds it up to 20 seconds and answers the user's answers by question index (`{ answers: { 0: "Blue" } }`), or 204, and the mod asks again; 410 when it dropped the ask, 400 (`{ error }`) for questions it can't show. Either way the mod then leaves the question to the terminal's dialog. |
| `/mod/ask/done` | `{ id }`: the call has its answer, from AgentK Local or the terminal, or ended. |
| `/mod/questions` | From the `design_questions` tool the mod gives Claude: the design's questions, checked and saved (`{ text }` for Claude), or what's wrong (400, `{ error }`). |
| `/mod/learn` | A [learn step](recall-and-learn.md#the-learn-step)'s results, `{ id, step, model, text \| reason, usage }`: what the fork of the session found (`step: "facts"`, for a `learn` part of kind `extract`), then the duplicate check's reply (`step: "decisions"`, kind `check`). Answered at once, 204, so no request is held while AgentK works with Meko: what comes next arrives on a poll. 410 for a step AgentK gave up on; its usage is still recorded. |
| `/mod/learn/start` | From `/agentk_learn`, a slash command the mod registers: starts a learn step, answered `{ text }` (the command's output: started, or why not). |
| `/mod/meko` | From a `tool.call` hook on every tool that goes straight on unless the tool is Meko's (`/^mcp__(?:[\w-]+_)?meko__/`, a plugin's Meko too, checked in the hook: the oldest Claude Code the mod runs in may not take a RegExp matcher): `{ id, state: "start" }` before the call runs, awaited so it always comes before its end, then `{ id, state: "end" }` from a `finally`, so a failed or interrupted call ends too (the hook's `.catch` replays what `next(e)` settled to, so a failed call never runs twice). `id` is the call's `tool_use_id`; answered 204, and an `end` that comes after a reset is dropped without bringing the link back. See *Meko in the dot* under [The app](app.md). |

**Prompts.** `submit()` (`sessions.ts`) sends a prompt through the mod while its link is live:

- live means the mod is polling, or polled in the last 10 seconds (`modlink.ts`), and `[claude] mod`
  is on. `config.toml` is read when the server starts, so turning it off takes effect at the next
  server restart; from then on prompts are typed, for running sessions too, though those keep the
  mod (and `CLAUDE_CODE_ENABLE_FUNCTION_HOOKS`) until they're relaunched;
- the send resolves when the server answers the `accepted` ack with 204;
- the engine holds the prompt until Claude is idle, so it never lands in a permission dialog, a
  menu or a running turn, and a long one arrives whole rather than as `[Pasted text]`;
- the transcript row is a plain user message (it adds an `origin` naming the plugin), and the
  `UserPromptSubmit` and `Stop` hooks fire as for a typed prompt, so status and usage work as before.

Without a live link the prompt is typed into the terminal as before (the session host's bracketed
paste, then Enter). A prompt is never sent both ways:

- once handed to the mod, a send that isn't acked within 5 seconds fails, as a failed paste does,
  and the caller keeps the prompt queued; a late ack gets 410, so the mod never runs it;
- a relaunch, `SessionEnd`, archive, project delete or stop resets the link and fails what was on
  its way, and those prompts' acks get 410 too;
- the one gap left is on the other side: if the 204 answering an ack never reaches the mod, the
  prompt counts as sent but doesn't run. That costs a prompt, never a duplicate;
- a `dropped` ack comes after the send resolved, so the caller already counts the prompt as sent.
  The session goes to **Needs you**, naming the prompt (its first line) and the plugin's reason.
  When Claude's prompt box was empty, the mod put the prompt's text there, and the detail says to
  press Enter in the terminal to send it; otherwise it says to ask AgentK for it in the terminal.

The links are in memory only. After a server restart the mod's next poll brings its link back
within seconds, and the prompts AgentK queues are in the database anyway. Whether a session's link
is live is in `Session.modLive`: the server pushes a change when the mod is first heard from, on a
reset, and when a sweep every 2 seconds finds a link gone stale. The UI offers what needs the mod
(below) only while it's live, and the server checks again on every request.

**Claude's questions.** The mod's `tool.call` hook on `AskUserQuestion` lets Claude's own dialog
open in the terminal as always (`next(e)`), and sends the questions to the server too
(`/mod/ask`, asked again after each hold). Whichever is answered first wins: the terminal's answer
is returned as it is; AgentK Local's comes back as the tool's result,
`{ questions, answers: { "<question>": "<answer>" } }`, which closes the terminal's dialog (checked
on Claude Code 2.1.286, and on 2.1.287 (the oldest that loads the mod now) and 2.1.291; so is the
`design_questions` tool below). The hook's `.catch` falls back to `next(e)`, and any failure
leaves the dialog to answer alone. On the server (`modlink.ts`) an ask lives only while Claude's
call waits: in `Session.asking` (`AskRequest`), with no copy in the database. It's dropped when the
mod says it has its answer (`/mod/ask/done`), when the mod stops asking for 10 seconds (the call
was interrupted, or Claude died), at the turn's end (`Stop`), and on a reset; and since the mod
sends the questions with every ask, a server restart brings the ask back with the next one. Only
choice questions (1 to 4, each with options) are offered: a question of another kind (Claude
Code's `text` and `number` questions) is left to the terminal. Text is capped for the browser (the
question 1,000 characters, a label 200, a description 1,000, a preview 8,000); labels are kept
whole, since the answer names them.

The session goes to **Needs you** with *AgentK asks: <first question>*, and you're notified once;
the classic `PermissionRequest` hook for `AskUserQuestion`, which Claude raises as well, uses the
same words (without the mod too). The questions show:

- in the design's **Questions** view, under **AgentK is asking**, at the top (its tab counts them);
- in any stage, in a bar above the terminal, *AgentK asks: …* with **Answer**, and on a phone also
  as the ⋯ sheet's first row (with a dot on ⋯). **Answer** opens the questions in a dialog that
  fills the window (a full-height sheet from the bottom on a phone): the questions scroll and
  **Answer** stays at the bottom. Nothing opens by itself: you may be answering in the terminal.

Each question is a card: its header, the options (radio rows, or checkboxes for a multi-select
question, each with its description and its preview as preformatted text with a **Wrap lines**
toggle for lines too long to fit: off at first on a desktop so tables and code stay aligned, on at
first on a phone; in the dialog a preview grows to 60% of the window's height before it scrolls,
40% on a phone), and **Other** with a
text box, which is the dialog's "Other". `POST /api/sessions/:id/ask/:askId` checks the answers
against the questions (labels must be the question's, one unless it's multi-select, the text
within 10,000 characters) and hands them to the mod: the labels comma-joined, then the text, as the
dialog gives them. A 409 means AgentK has moved on: the terminal answered first, or the turn ended.

**Answers and approvals mid-turn.** Mid-turn means a turn is running (`Session.midTurn`): from its
`UserPromptSubmit` to its `Stop` (or Claude's end), while AgentK works or Claude waits on a
permission prompt or a question during it. A Needs you with no turn behind it (a parked recovery,
a prompt another plugin refused) isn't, and nor is a design waiting for approval; after a server
restart only a session showing as working counts. While the mod is live, the design's **Send for
revision** works mid-turn: the answers go through the mod at once and Claude holds them until the
turn ends, so the server keeps nothing. A prompt sent mid-turn leaves the session's status alone (a
permission prompt still shows as Needs you). Which prompts Claude still holds comes from the mod
(`held` on each poll, and `/mod/held`), so one another plugin refused, or one still held after a
server restart, counts right; a mod too old to report it (until it reloads) has one taken off at
each `UserPromptSubmit`. They're in `Session.promptsHeld`. While Claude holds any, the *Design
ready* notification at a turn end is skipped, since AgentK revises the design again right away, and
an approval is refused, mid-turn or not: the held answers run first, and the approval would move
the session to Code while AgentK revises the design.

**Approve design** works mid-turn too, but waits on the server instead: a turn in Design may still
change the design (often it's the revision you asked for), and moving to Code at once would lift
the Design edit guard while that turn runs. The approval, with your answers, is kept in memory
(`Session.approvalQueued`; the gate and the questions' footer say so, with **Cancel approval**). At
the turn's end it's sent, as an approval between turns is, if the turn left the design and its
questions alone; if it changed them it's dropped, and you're notified to review the design and
approve again (the answers are still in the browser). It's dropped, with a line in the activity
log, when Claude exits (with its `SessionEnd`, or found dead by `reconcile()`) or is relaunched, or
the session is archived; a server restart forgets it. While it waits, the answers it took can't be
edited, and answers can't be sent; the browser marks its drafts (`approved` in
`ak-design-answers:<session>`) and forgets them once the session has left Design, or makes them
drafts again when the approval was dropped. Without the mod, approving and sending answers are
refused mid-turn, as before.

**The design's questions tool.** The mod registers a `design_questions` tool at `session.start`
(`mcp__agentk__design_questions`; Claude Code may defer it, so the prompt says to load it with
ToolSearch). Its input is the questions file's list. The server checks it strictly
(`checkToolQuestions()` in `design.ts`: each question an object with an id and a question, ids not
repeated, options with labels, a recommendation that points at an option, a status of open or
resolved, at most 30, and no more than the file can hold) and answers what's wrong, so Claude fixes
it in the same turn. Good questions are saved to `questions.json` at once (through a temp file
renamed into place, so a restart and the next turn end read the same ones) and shown straight away:
when they differ from the ones kept, the design's time moves on and the change counts as a change to
the design (the same list again changes nothing). The tool is refused
outside Design. Without it (no mod, or an older session) Claude writes the file as before.

**Context fill.** The session footer (and the ⋯ sheet on a phone) shows `context` and a ring for
the latest measurement. The ring fills counter-clockwise from 12 o'clock (a C at 50%, a closed O
at 100%), so its shape gives the level without colour; its arc is blue, amber from 75% and red
from 95%. The exact percent is in its tooltip and its meter value, and shown with the ring: with a mouse it pops up over the ring as soon as the mouse is on it (the tooltip takes a second), floating so the footer doesn't shift; on a touch screen, which can't hover, it is written after the ring, in the footer on a tablet and in the ⋯ sheet on a phone. It is cleared when Claude is relaunched or
`/clear`ed, until the next turn measures it again. The plan limits in the measurement are merged
like the status line's (see [Plan limits](token-usage.md)).

Without the mod everything works as before; the footer just shows no context fill. The mod
swallows every failure, and a hook that throws is skipped by Claude Code. `claude plugin validate`
and `claude plugin test src/mod/agentk` check it against the engine (`test/unit/mod.test.ts` runs
both where Claude Code is installed). Its source compiles against Claude Code's own declarations,
not the repo's tsconfig.
