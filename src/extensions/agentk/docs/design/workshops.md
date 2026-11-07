# Workshops: the Kode tour

A workshop is a guided walk through Kode, bundled with it: a script for a **host** session and the
demo repo it's played in. The first is the Kode tour (`agentk-101`), whose Level 1 has Kode run
its code review on a teammate's change, where Meko's gotchas catch a trap the code alone
doesn't show, and whose Level 2 has Learn the project fill the demo's datapack, a fresh session
answer from it, and Kode's learn step teach Meko what that session found it didn't know. It needs
no network and never touches GitHub. Levels 3 and 4 (a feature with a design, sharing) come later;
each is mostly new text in the bundle.

## The bundle

`src/workshops/<id>/` (copied to `dist/workshops` by `npm run build:workshops`, read beside the
server as the session tools' skill is: `WORKSHOPS_SOURCE` in `src/server/workshops.ts`):

| File | What it holds |
|---|---|
| `workshop.json` | id, title, summary, the host's script (`guide`), the demo repo (`repo`: its files' folder, the folder name in the repos folder, the made-up author and date of main's one commit and its message, and each branch's patch), the gotchas (`file`, the gotchas datapack's `datapack` name and what's in it), the code review's settings (`codeReview`) and the model its review sessions start on for each agent (`reviewModels`), and the levels (id, title, what each needs, and `prompt`: one prompt file, or a list of `{ name, file, learn }` for a level with more than one session; Level 2's `learn` is Learn the project's brief). `readWorkshop` checks it, the brief as a learn session's (`checkLearn`). |
| `GUIDE.md` | The host's first prompt. Placeholders: `{{projectId}}`, `{{repo}}` (the demo repo's path), `{{meko}}` (`on` or `off`), `{{harness}}`, `{{gotchas}}` (the gotchas datapack's name, from the demo's MEKO.md, or `none`), `{{datapack}}` (the project's dedicated datapack's name, or `none`) and `{{levels}}` (each level's prompts, fenced, a named one under `#### Session "<name>"`, for the host to pass to `start_sessions` word for word, with that name). |
| `levels/*.md` | The prompt of each session the host starts, under the session tools' `PROMPT_MAX` (8,000 characters). |
| `repo/` | The demo's files on main: a small Snake game with no build, plain scripts (no ES modules and no `fetch`, so `index.html` opens from disk, where browsers refuse both), rules in `src/game.js` (on a `Game` global, `module.exports` for `node --test`) apart from the drawing in `src/ui.js`, no dot-files (a `.gitignore` there would apply to Kode's own repo). |
| `branches/*.patch` | A teammate's change, as `git format-patch` wrote it, by a made-up author. |
| `gotchas.md` | The gotchas (`MG-<n>`) uploaded to the demo's gotchas datapack. Never copied into the repo: if the task or the review could read the trap from the worktree, Level 1 would prove nothing about Meko. |

`test/unit/workshops.test.ts` builds the demo in a temp folder and checks that the patch applies, the
bug is on main and the teammate's fix on its branch, MEKO.md is on both, and the gotchas and dot-files
aren't; that no game file is a module or fetches; that GUIDE.md renders with every placeholder
filled; and that the demo's tests still leave Level 2's answer untested (below).

### Level 1's trap

On main, New game keeps the last game's score (`Game.newGame()` sets up the board and leaves
`state.score`). The teammate's fix resets the score in `newGame()`, which in this repo only the
button calls, so it reads as right. MG-2 says the arcade cabinet's daily challenge, built from
another repo, calls `newGame()` between its rounds and adds the rounds' scores up: the reset belongs
where a run starts, never in `newGame()`. MG-1 and MG-3 are true but have nothing to do with the
fix, so a credit shows the search was specific.

The trap rests on a fact the repo doesn't hold, so it should need Meko. `npm run check:demo-review`
(`scripts/check-demo-review.mjs`) measures that, by hand against a running Kode with a real agent
and Meko (it isn't in CI: every run is a real review): it reviews `demo/score-fix` 3 times with the
demo's MEKO.md and 3 times with MEKO.md taken out of the worktree, and passes only when every review
with the datapack keeps a finding that credits MG-2 and none without keeps one about the trap. First
it waits for the level's own review, on the tour's open level session or else a new one: after a
first look it gives the go (`POST /api/workshops/:id/go`, a 409 when the user already gave it) and
waits for the review proper, so the gotchas are indexed. Its own reviews use the project's Sirius 1.0 and stop
where the findings wait on the user. Run it whenever the patch or `gotchas.md` changes, and put its
result in the PR. The tour itself shows the same contrast once: the first look, then the review.

### Level 2: learning the project

The project's dedicated datapack starts empty (the gotchas are in their own datapack). The host
sends the user to the project home's **Learn the project**: in a workshop's project the dialog opens
with the level's `learn` brief (`Workshop.learn` from `GET /api/workshops`, as the dialog's
`initial.learn`), so the user only presses Start, and the home offers it while the datapack is
empty, as everywhere, and also while no learn session has run on it (`ProjectHome.learnOffered`),
since a Level 1 session may have saved a memory there. It reads "Meko hasn't learned this project
yet." then, in place of "This datapack is empty.". The brief points Learn at `src/` and the docs, with no PRs.

The host then starts "Ask about the game" (`levels/2-ask.md`), which asks how the game is run and
tested (Learn covers it) and which rules have no test (it doesn't: the demo's tests leave running
into itself and a full board untested, and the unit test keeps it so). The prompt tells Claude not
to save to Meko itself: a fact Claude saves with `memory_add` is one the learn step leaves out, and
only facts the learn step writes mark a Context Gaps question taught. Its prompt in `workshop.json`
has `learn: true`, so when that session's first turn ends, not interrupted, Kode runs the learn
step on it by itself (`learnLevel`: the session found by its name, once per session), as Save
learnings does. The step reads the open questions on Context Gaps once, as it starts: Meko isn't
polled for them, so a question Meko lists only later stays open. "Ask again" (`levels/2-again.md`) then
asks the second question alone, and answers it from Meko. The host starts it only on the user's go,
once they've read the first answer, and Kode archives "Ask about the game" as "Ask again" is
created (`closeEarlier`: a level's prompts are in order, and a later one's session replaces the
earlier ones still open, unless one is saving what it learned or Kode is starting that step). The host's script has it wait
for the user after each result, and ask whether they've read "Ask again" before it ends the level. A host whose learn step can't start is
told why, and that Save learnings in the session's menu does the same.

There's no measuring script: if Learn already saved the untested rules, the host says Meko knew,
and skips the rest.

## Starting one

`WorkshopService.start(id)` (`POST /api/workshops/:id/start`; one start per workshop at a time):

1. With its project open (`projects.workshop`), it goes to that project's host, starting a new one
   when none is open.
2. With Meko on, the gotchas datapack first: a new one for this tour, never one reused by name,
   since a level's first look must find it empty. It's named after the demo's folder
   (`adlc/repo/kode-demo/gotchas`, `gotchasName`), with a suffix on its repo part when the name
   is taken (`adlc/repo/kode-demo-2/gotchas`, or a random one when Meko refuses it), keeping the
   `/gotchas` ending the code review looks for.
   Then the project's dedicated datapack, named as Open project names one. At the plan's datapack
   limit the gotchas one is made and the project goes without its own; any refusal is logged and
   the tour goes on without it.
3. The demo repo in the repos folder (`[repos] clone_into`) as `kode-demo` (`-2`, `-3` when taken):
   the files, a MEKO.md listing those datapacks (none without Meko), one commit on main by the
   bundle's made-up author at its fixed date, then each branch from main with `git am` of its
   patch, which keeps the teammate's name, message and date. Git runs without the user's config
   (`GIT_CONFIG_GLOBAL=/dev/null`, no hooks, no signing), so every install gets the same history
   apart from MEKO.md. No remote: nothing is fetched and nothing can be pushed.
4. The project (`projects.create`, with the dedicated datapack as an existing one), marked
   `projects.workshop` (migration 50). The gotchas datapack isn't attached: only the code review
   reads it, through MEKO.md, as on a real repo, and the dedicated datapack starts empty for a
   later level's Learn the project.
5. The host: `sessions.begin(…, { purpose: 'guide' })`, a task at Explore on the default harness.
   A host whose worktree can't be made isn't kept as a failed start, whose Try again would start a
   plain task: its notification says to start the tour again, which begins a new host.
6. No upload yet when a level looks first without the gotchas (the tour's Level 1): they go in after
   its first look (below). Otherwise the gotchas' upload starts now (`KnowledgeService.uploadFile`,
   Meko's presigned flow, followed until indexed). The host hears of a failed upload only: a
   success changes nothing it says. An upload Kode stops following (Meko changed or went off)
   ends as `unknown`, so the level goes on.

A build that fails leaves no project and answers 500 with the reason.

**A fresh install.** `meko kode install` writes `[welcome] tour = "agentk-101"` into config.toml only
when it made config.toml (`welcomeTour`), and an interactive install whose server answers opens
Kode in the browser (`opensBrowser`: not with `--non-interactive`, which Meko's Extensions page
uses, and not over SSH or without a display). The server's start (`startOnFreshInstall`, after
recovery, not awaited) runs the tour while there are no projects and `tour-started` isn't in kv,
then writes `tour-started` and removes the key; with projects already there it only removes the
key; a failure keeps it for the next start. The App's rule of opening the first project's last
session lands on the host.

**By hand.** The empty state has **Take the Kode tour** as its main button, before Open project, and the command
palette **Start the Kode tour**.

## The host

A task at Explore with `sessions.purpose = 'guide'`, following Learn the project's shape: no new
SessionKind, and no stage rules of its own.

- Its first prompt is `guide`: the bundle's `GUIDE.md` rendered by `WorkshopService.guidePrompt`,
  never a repo's `.kode/stages` or the owner's override. The host's first reply opens with the
  tour's ASCII banner (Kode in block letters over "Tutorial") and asks one question, which level to start (only Level 1
  for now). It takes the user's browser to be on the computer Kode runs on, so they can open the
  game's `index.html`.
- Its system prompt adds `guideSystemLine`: sessions it starts are the user's; start them with the
  session tools and never pass their gates; prompts that start "Kode events:" come from Kode.
- It drives the tour with the existing session tools: `start_sessions` (which takes an existing
  `branch` for this) and `list`, and gives the user no homework: Kode does each level's work
  while the user watches.
- **A level's code review runs by itself: a first look, then the review proper.** A level with
  `review` in `workshop.json` (Level 1: `{ "branch": "demo/score-fix", "gotchasAfterFirst": true }`)
  has Kode review the level's session in the workshop's project (a task at Code on that branch,
  while no other open session there has a review; others, `check:demo-review`'s, are reviewed by hand) as soon as its
  worktree is made (its `created` event, `WorkshopService.noted`; a turn end with no review yet
  tries again). With `gotchasAfterFirst` and a gotchas datapack, that first review is a **first
  look**: `CodeReviews.runNow(task, 'auto', { describe: false, handOff: false })`, Meko on but the
  datapack still empty, so it misses the trap; its findings stay with it (`code_reviews.hand_off =
  0`, `CodeReviewSummary.firstLook`, "Kode ran a first look", "First look done"), never sent to
  the task. At its end (`firstLookDone`) the level pauses: the host hears that Kode waits for the
  user's go and asks the user to type **go**. A host prompt whose first word is "go" (its
  `UserPromptSubmit`, `WorkshopService.hostPrompt`), or `POST /api/workshops/:id/go`, goes on while
  a first look is done with no review proper after it: `gotchas.md` is uploaded and, once Meko is
  done with it (indexed or not, or a failed upload, which the host hears of), Kode runs the review
  proper (`handOff` on, "Kode ran the code review"), which catches MG-2. A go with nothing
  waiting does nothing (the route answers 409); the pause is read from `code_reviews`, so it
  survives a restart. The word is matched in code rather than left to a session tool, so every
  other session's tool list stays as it is. Both run with no
  PR description first, since the demo opens no PR, so neither waits for the task's turn. Without
  Meko there's no gotchas datapack and one review. The workshop's `codeReview` (the tour's:
  `{ "strategy": "sirius-1.0" }`, one quick pass) becomes the project's code review settings, and
  its review sessions start on the workshop's `reviewModels` for their agent (the tour's: Sonnet on
  Claude Code, `WorkshopService.reviewModel`, passed to `startReviewSession`) rather than the
  agent's default; the pass and the scorers keep their tiers' models (`[code_review.models]`). The
  review proper's findings wait for the user to check them and send them to Kode, as every
  review's do by default; the round's event counts the ones caught with Meko, so the host can point
  to them. The level's prompt runs no tests. A refusal is logged and told to the host. **Run code
  review** in the Code gate runs it again.
- A level ends when the host ends a turn with "That's the end of Level N" as its reply's last line
  (its script's wrap-up says it last): the host's `Stop` carries the reply (`last_assistant_message`,
  Claude Code), and `WorkshopService.hostSaid` archives that level's sessions (every open session
  in the project but the host and code reviews, since one level runs at a time), so the user has
  nothing to archive. One still saving what it learned stays open: Archive would cut the save short. Archive, not Discard: the host's words
  never throw work away, so a worktree with the level's uncommitted fixes is kept, as Archive keeps
  one, and the branch stays. A host on a harness whose Stop has no reply leaves them for the user.
- A replay of a level (a new session on its branch once the last one closed) gets its review by
  itself too: the guard counts only open sessions' reviews. Once the gotchas are indexed
  (`uploadGotchas` notes it in kv, `workshop-gotchas:<project>`) there's no empty datapack to look
  with, so a replay gets the review proper alone, with no first look or pause.
- The stage bar shows **Guiding you** in place of Explore's Move on…, and Merge & Close skips the
  learn step by default for it.

### What the host hears

`GuideEvents` in `workshops.ts`. `SessionManager.event()`, where every activity event is recorded,
hands each one to `noted()`; for a session in a workshop's project that isn't the host or a code
review's own session, the kinds `created`, `stage`, `review`, `design`, `archived`, `discarded`,
`exited`, `learn-done` (a learn session's summary) and `learned` (a learn step's result) are queued
for the project's open host, as "<session>: <text>". Each such session's turn end is queued too
(`turnEnded`, after the diff's refresh), saying when the turn changed the diff, and, for a session
that used Meko, its Meko line (`sessionMekoLine` in `mekousage.ts`: what Kode passed it at its
start, its lookups and what they found, and the estimated tokens saved, by the Usage page's formula):
the host quotes the saving, which the UI shows only per project. With a Context Gaps list for the
datapack (Meko on this computer), the line ends with how many of the session's own questions Meko
couldn't answer and the full address of the datapack's Context Gaps page in Meko's web UI
(`localMeko().meko.webUrl` and `mekoGapsPath`, whose tab sends a linked Meko Cloud's datapack on
to its own page), and a `learned` event that taught a question adds the same address, where the
question is now under Taught (the page opens on Open, and its filter isn't in the URL).
The workshop's own notices (the pause for the user's go, a failed gotchas upload) go with `tell()`.
The prompt is only the events' lines; GUIDE.md and the host's system line say what to do with them.
The user doesn't see it: Kode's view mod (`src/mod/agentk-view`) draws a prompt from Kode's mod
whose first line is the host's `KODE_HIDDEN_PROMPT` (`Kode events:`) as nothing in the terminal,
with Claude Code's `ui.render` hook on `UserMessage` rows; the full transcript (ctrl+o) still
shows it ([Kode's mod](mod.md#how-kodes-prompts-look)). A reply of just "OK" in that prompt's turn, which
GUIDE.md asks for when nothing needs saying, is hidden by Kode's mod (`AssistantMessage` rows,
for the prompts it submits with `hidden`: `SessionManager.submit`'s `hidden`, on the mod's poll as
`hidden: true`). Typed into the terminal (no mod) the events show, and so does such an "OK" after
Claude restarts (the mod's list is in memory). Sessions in other projects send
nothing.

The queue goes to the host as one `guide-event` prompt ("Kode events: …"): a few seconds after the
first event while the host is between turns (`GUIDE_SETTLE_MS`), else at its own turn's end. It's
kept in memory, at most 30 lines; after a restart the host reads where things stand with `list`.

## No PR in the demo

A workshop's project (`Project.workshop`) opens no PR: its repo has no remote, and Open PR has no
remote check, so its push and `gh pr create` would fail and leave the session in Pull Request with
Merge unavailable. The server refuses Open PR, Run to merge (on a session and at a session's start)
and a move to Pull Request there with a 409 (`checkOpensPr` in `lifecycle.ts`; the session tools'
`open_pr` and `move_session` go through the same calls). In the UI, the Code gate shows "No PR in
the demo" and **Run code review** (the `review-run` action, while `canRunReview` offers it;
`demoReviewGate` in `src/web/util.ts`) where Open PR and Run to merge would be; while a review's
findings wait on the user, or it waits in its terminal, the gate shows that as a review's gate does
(**Findings wait on you**, **Send to Kode**). The session's ⋯ and the palette leave out Open PR
with instructions. Every review there skips the PR description (`runNow` turns `describe` off for a
workshop's project), so Run code review sends the task nothing first. Tasks there end at Code.
