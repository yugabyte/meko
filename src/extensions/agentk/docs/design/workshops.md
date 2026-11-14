# Workshops: the Kode tour

A workshop is a guided walk through Kode, bundled with it: its levels' stages, which Kode runs
itself as the project's **tour**, and the demo repo it's played in. The first is the Kode tour (`agentk-101`), whose Level 1 has Kode run
its code review on a teammate's change, where Meko's gotchas catch a trap the code alone
doesn't show, and whose Level 2 has Learn the project fill the demo's datapack, a fresh session
answer from it, and Kode's learn step teach Meko what that session found it didn't know. It needs
no network and never touches GitHub. Levels 3 and 4 (a feature with a design, sharing) come later;
each is new text in the bundle, plus a stage kind of Kode's own where one is new.

Until November 2026 a Claude session (the **host**, `purpose = 'guide'`) ran the tour from a script,
GUIDE.md, hearing what happened through hidden "Kode events" prompts. Kode runs it itself now, for
three reasons the host couldn't get past: a Kiro or OpenCode Stop carries no reply, so a level there
never ended; a plugin can't hide the prompts it submits itself, so the events showed; and edits to the
script never reached a host already running. The tour's words are written in the bundle, so every run
reads the same, and its numbers are the ones Kode measured.

## The bundle

`src/workshops/<id>/` (copied to `dist/workshops` by `npm run build:workshops`, read beside the
server as the session tools' skill is: `WORKSHOPS_SOURCE` in `src/server/workshops.ts`):

| File | What it holds |
|---|---|
| `workshop.json` | id, title, summary, the welcome card shown once before the tour begins (`welcome`: title and text), the tour's last card (`end`), the bar on top of the played game (`play`), the demo repo (`repo`: its files' folder, the folder name in the clone folder (`[repos] clone_into`), the made-up author and date of main's one commit and its message, and each branch's patch), the gotchas (`file`, the gotchas datapack's `datapack` name and what's in it), the code review's settings (`codeReview`) and the model its review sessions and its Learn the project session start on for each agent (`reviewModels`: Sonnet on Claude Code, for speed), and the levels (id, title, what each teaches (`teaches`, over its first stage's card), what each needs (`datapack`), `prompt`: one prompt file, or a list of `{ name, file, learn }`, each session the level begins by its name; Level 2's `learn` is Learn the project's brief; and `stages`, below). `readWorkshop` checks it: the brief as a learn session's (`checkLearn`), and each stage's kind, text and placeholders (`checkStages`). |
| `levels/*.md` | The prompt of each session a level's stages begin, under the session tools' `PROMPT_MAX` (8,000 characters). |
| `repo/` | The demo's files on main: a small Snake game with no build, plain scripts (no ES modules and no `fetch`, so `index.html` opens from disk, where browsers refuse both), rules in `src/game.js` (on a `Game` global, `module.exports` for `node --test`) apart from the drawing in `src/ui.js`, no dot-files (a `.gitignore` there would apply to Kode's own repo). |
| `branches/*.patch` | A teammate's change, as `git format-patch` wrote it, by a made-up author. |
| `gotchas.md` | The gotchas (`MG-<n>`) uploaded to the demo's gotchas datapack. Never copied into the repo: if the task or the review could read the trap from the worktree, Level 1 would prove nothing about Meko. |

Each stage in a level's `stages` is `{ id, kind, title, text, labels }`. `kind` is what Kode runs
(`TourStageKind`, below); `text` is the guide card's words for each phase, `ready`, `running`,
`done` (all three needed where the kind starts work; `repo` needs only `done`), and `failed`,
`blocked` and `missed` (Review with context done with no credited gotcha); `labels` name its buttons
(`run`, `next`, `finish`). The text may use only these
placeholders (`TOUR_VARS`), which the tour fills: `{{repo}}`, `{{datapack}}`, `{{gotchasDatapack}}`,
`{{session}}` (the stage's session), `{{failure}}`, `{{meko}}` (the session's Meko line,
`sessionMekoLine`), `{{cold.kept}}`, `{{cold.caught}}`, `{{cold.tokens}}`, `{{context.kept}}`,
`{{context.caught}}`, `{{context.gotchas}}`, `{{context.tokens}}`, `{{teach.gotchas}}` (the ids of
`gotchas.md`'s `## MG-n` headings, "MG-1, MG-2 and MG-3") and `{{gotchas.list}}` (a bullet per
gotcha, its id and title, a function's name as code: `gotchaList`). Kode needs Meko to run (Meko
serves it), so the card has no text for Meko being off.

`test/unit/workshops.test.ts` builds the demo in a temp folder and checks that the patch applies, the
bug is on main and the teammate's fix on its branch, MEKO.md is on both, and the gotchas and dot-files
aren't; that no game file is a module or fetches; that every stage's text uses only the tour's
placeholders and the stage checks refuse what they should; and that the demo's tests still leave
Level 2's answer untested (below).

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
first look it calls `POST /api/workshops/:id/go` (the teach stage and Review with context in one; a 409
when the tour is past them) and waits for the review proper, so the gotchas are indexed. Its own reviews use the project's Sirius 1.0 and stop
where the findings wait on the user. Run it whenever the patch or `gotchas.md` changes, and put its
result in the PR. The tour itself shows the same contrast once: the first look, then the review.

### Level 2: learning the project

The project's dedicated datapack starts empty (the gotchas are in their own datapack), and Learn the
project makes it empty again each time (below). Its first stage, Learn the project, starts a learn session with the level's `learn` brief, which points Learn at
`src/` and the docs, with no PRs. The project home offers the same: in a workshop's project its
dialog opens with that brief (`Workshop.learn` from `GET /api/workshops`, as the dialog's
`initial.learn`), and the home offers it while the datapack is empty, as everywhere, and also while
no learn session has run on it (`ProjectHome.learnOffered`), since a Level 1 session may have saved
a memory there. It reads "Meko hasn't learned this project yet." then, in place of "This datapack
is empty.".

Its next stage begins "Ask about the game" (`levels/2-ask.md`), which asks how the game is run and
tested (Learn covers it) and which rules have no test (it doesn't: the demo's tests leave running
into itself and a full board untested, and the unit test keeps it so). The prompt tells Claude not
to save to Meko itself: a fact Claude saves with `memory_add` is one the learn step leaves out, and
only facts the learn step writes mark a Context Gaps question taught. Its prompt in `workshop.json`
has `learn: true`, so when that session's first turn ends, not interrupted, Kode runs the learn
step on it by itself (`learnLevel`: the session found by its name, once per session), as Save
learnings does. The step reads the open questions on Context Gaps once, as it starts: Meko isn't
polled for them, so a question Meko lists only later stays open. A step that can't start is kept
(kv `workshop-learn-failed:<session>`) and shown with Retry. The last stage begins "Ask again"
(`levels/2-again.md`), which asks the second question alone and answers it from Meko; Kode archives
"Ask about the game" as it's created (`closeEarlier`: a level's prompts are in order, and a later
one's session replaces the earlier ones still open, unless one is saving what it learned or Kode is
starting that step). Both stages link to the datapack's Context Gaps in Meko's web UI on its Taught
filter (`localMeko().meko.webUrl`, `mekoGapsPath` and `/taught`, which Meko's page opens on), where
the question the learn step answered shows. While Learn the project runs, the card links the
datapack's Memories in Meko's web UI, to watch them arrive. Learn the project starts on the tour's
model (`reviewModels` for the agent it starts on), and the questions' sessions on the user's own.

There's no measuring script: if Learn already saved the untested rules, the answer says so.

## Starting one

`WorkshopService.start(id)` (`POST /api/workshops/:id/start`; one start per workshop at a time):

1. With its project open (`projects.workshop`), it goes to that project.
2. With Meko on, the gotchas datapack first: a new one for this tour, never one reused by name,
   since a level's first look must find it empty. It's named after the demo's folder
   (`adlc/repo/kode-demo/gotchas`, `gotchasName`), with a suffix on its repo part when the name
   is taken (`adlc/repo/kode-demo-2/gotchas`, or a random one when Meko refuses it), keeping the
   `/gotchas` ending the code review looks for.
   Then the project's dedicated datapack, named as Open project names one. At the plan's datapack
   limit the gotchas one is made and the project goes without its own; any refusal is logged and
   the tour goes on without it.
3. The demo repo in the clone folder (`[repos] clone_into`, Settings › Clone into) as `kode-demo`
   (`-2`, `-3` when taken, by anything, a link to nowhere too: `cpSync` onto one aborts Node on
   macOS). That folder is checked before step 2 (`cloneFolder`: a full path, made
   if missing, and writable); one that can't be used is a 400 naming Settings › Clone into, and no
   datapack is made. The demo repo is the files, a MEKO.md listing those datapacks (none without Meko), one commit on main by the
   bundle's made-up author at its fixed date, then each branch from main with `git am` of its
   patch, which keeps the teammate's name, message and date. Git runs without the user's config
   (`GIT_CONFIG_GLOBAL=/dev/null`, no hooks, no signing), so every install gets the same history
   apart from MEKO.md. No remote: nothing is fetched and nothing can be pushed.
4. The project (`projects.create`, with the dedicated datapack as an existing one), marked
   `projects.workshop` (migration 50). The gotchas datapack isn't attached: only the code review
   reads it, through MEKO.md, as on a real repo, and the dedicated datapack starts empty for a
   later level's Learn the project.
5. No upload yet when a level looks first without the gotchas (the tour's Level 1): they go in at
   its teach stage. Otherwise the gotchas' upload starts now (`addGotchas`).

No session is begun: the page goes to the project, where the tour shows its welcome card. A build
that fails leaves no project and answers 500 with the reason.

**Fresh datapacks.** Each Run the review (Level 1's review without context) drops the gotchas datapack
and makes it again, empty, under the same name (`freshGotchas`), and each Learn the project does the
same to the project's own datapack (`freshProjectDatapack`, which keeps it the project's dedicated
one under the new id and makes a new Meko conversation for it), so a level taken again starts as
the first time did: dropping a datapack is faster than deleting what's in it. Meko gives the new
one a new id, which the demo's MEKO.md must name, since the review reads MEKO.md from the task's
worktree: Kode writes it on main, commits it, and makes each branch again from main with its patch
(`recommitDemo` in `git.ts`), so the branch's own diff stays the teammate's fix. That needs the
branch out of every worktree: a level's task archived clean parks its worktree, which frees it.
A tour started while Meko was down has no MEKO.md: Run the review says to start the tour again.

**A fresh install.** `meko kode install` writes `[welcome] tour = "agentk-101"` into config.toml only
when it made config.toml (`welcomeTour`), and an interactive install whose server answers opens
Kode in the browser (`opensBrowser`: not with `--non-interactive`, which Meko's Extensions page
uses, and not over SSH or without a display). The server's start (`startOnFreshInstall`, after
recovery, not awaited) runs the tour while there are no projects and `tour-started` isn't in kv,
then writes `tour-started` and removes the key; with projects already there it only removes the
key; a failure keeps it for the next start. The App's rule of opening the first project lands on
the tour.

**By hand.** The empty state has **Take the Kode tour** as its main button, before Open project, and the command
palette **Start the Kode tour**. While a tour is open, the palette offers **Go to the Kode tour** and
**Start the Kode tour over** in its place (`tourEntries()` in `src/web/palette.ts`, whose keywords
take "again", "restart", "over" and the like, since the palette wants every word typed).

**Starting over.** **Start the Kode tour over** asks first (`RestartTourDialog`), then
`WorkshopService.startOver` (`POST /api/workshops/:id/start-over`, under the same one-at-a-time
lock as a start: a second press gets the first one's tour) first checks Clone into (`cloneFolder`),
so a folder it can't use leaves the old tour as it was. It reads the datapacks the tour made
(`tourDatapacks`: the gotchas one MEKO.md names and the project's own, each only under the name the
tour gives it, so a datapack of the user's the project was switched to is never deleted, while the
tour's own, kept as shared after that switch, still is), then closes the project (`Projects.delete`,
which archives its sessions and returns the worktrees they kept for uncommitted changes) and stops
any session that began as it closed (a level's code review: `startReviewSession` also refuses a
closed task or project), since it would hold the demo's branch in the new tour. It removes the kept
worktrees in the demo through git (`git worktree remove --force`, `dropKeptWorktree`, and from the
pool; one a session kept in a repo of the user's stays, as Close tour leaves it), deletes the
datapacks, removes the demo folder unless a live project or one of its sessions uses that repo
(`WorktreePool.repoInUse`, the count `releaseRepo` keeps idle worktrees by), and starts a new tour
as above, which takes the freed folder. A delete that fails is logged and doesn't stop the new
tour. Close tour, by contrast, keeps the demo folder and its datapacks.

## The tour

`TourService` (`src/server/tour.ts`, `Core.tours`). `workshop_tours` (migration 52) keeps the level and
stage on screen, when the level (re)started (`level_at`: its sessions are the ones made since), the
stages passed over and the levels finished. Everything else about a stage is worked out each time
from what its work leaves, so a restart loses nothing and nothing has to be kept in step: the
level's task (the newest task on its review branch) and its code reviews (`hand_off` 0, the first
look; 1, the review with the gotchas), the gotchas' upload (kv `workshop-gotchas:<project>` once
indexed, `workshop-gotchas-result:<project>` with how it ended), the learn session, the question
sessions by name and their learn steps (`meko_learns`), and each session's last turn end in the
project (kv `workshop-turn:<session>`, from `WorkshopService.turnEnded`). A tour with no row (just
started, or one a host ran) starts where its records say: Review with context after a review with
the gotchas, the teach stage after a first look, else its first stage. One with no row and no
session in its project shows the welcome card (`TourState.welcome`), whose Start the tour
(`begin`) writes the row.

`Core.state()` adds it to each workshop project (`ProjectWithSessions.tour`, `TourState`): the
levels and their stages for the rail (done, current, todo, skipped), the stage's phase (`ready`,
`running`, `done`, `failed`, `blocked`), its card's text with the numbers filled in, what the level teaches on its first stage (`lead`),
its buttons and links, its session, and Level 1's scoreboard. A level whose `needs` aren't met
(`datapack`: the project's own) shows why, with Skip this level.

| Kind | Its button | Kode does | Done when |
|---|---|---|---|
| `repo` | Play the game (first, and the blue button until it's clicked) · Move on to step 2 | Nothing. | At once |
| `review-first-look` | Run the review | Makes the gotchas datapack again (above), then begins the level's task (its first prompt, at Code on the level's branch, `existingBranch`), which only waits; its first look starts as it's created (`reviewLevel`). A review that can't start is kept (kv `workshop-review-failed:<task>`) for Retry. | That review's findings are in |
| `teach` | Add the gotchas to Meko | Uploads `gotchas.md` (`addGotchas`, Meko's presigned flow, followed until indexed; an upload Kode stops following ends as `unknown`). | Meko is done with it; a failure shows its reason and Retry |
| `review-context` | Run the review again | The review proper (`runLevelReview(task, false)`, hand-off on); its findings stay with the review. Blocked without the task. A review that credits no gotcha (`missed` text) offers Run the review again beside Move on. | Its findings wait on the user (`triage`) or it's done |
| `learn` | Learn the project | Makes the project's datapack again (above), then a learn session with the level's brief, on the tour's model. | It closed itself |
| `ask` | Ask about the game | Begins the level's `learn` prompt's session; Kode's learn step follows its first turn. | The step ran |
| `ask-again` | Ask again | Begins the level's other named session. | Its first turn ended |

After a stage's work is done, the tour waits for the user's Next: nothing moves on by itself.
Finish (a level's last stage: Move on to Level 2, Finish level 2) archives every open session in the
project but code reviews' own (Archive, not Discard: a worktree with uncommitted changes is kept, and
the branch stays; one still saving what it learned stays open, since Archive would cut the save
short), marks the level finished and goes to the next one not finished, or to the tour's end (`end`,
with Use Kode on your own repo and Close tour). The rail's level names switch levels (`goto`), after the page asks: it archives the
open level's sessions the same way and starts that level at its first stage.

**The buttons.** `POST /api/workshops/:id/act` `{ stage, action, level? }` (`TourActRequest`). The
stage must be the one on screen, and the action one its card offers (`begin`, `next`, `run`, `retry`,
`finish`; `goto` anywhere), else a 409, so a double click or a stale page does nothing; one action
runs per project at a time. The answer names the stage's session, which the page then shows. The
page runs `own-repo` (Open project; the tour stays where it is) and `close` itself: Close tour, red,
at the panel's top right on every card and beside Use Kode on your own repo at the end, asks first,
then closes the tour's project (`DELETE /api/projects/:id`, which archives its sessions); Start the
Kode tour makes a new one.

**The scoreboard.** Each side is one review's first round: its findings kept (ones the user
dismissed count, since the review found them), those that credit a gotcha and which, and the tokens
its session took in (`usage_messages`' input, cache reads and cache writes, subagents included; Kiro's
credits when it billed those). Review without context is the first look; Review with context is
the review with the gotchas. Each row says its issues; what Meko caught is the
Difference row's (`scoreDifference` in `src/shared/tour.ts`). The tokens are measured but not shown:
the rows give issues only, and the stage texts don't name them.

**Playing the game.** `GET /api/workshops/:id/play/:where/*` serves the demo's files (`main`: the
demo repo; a session's id: its worktree, in the workshop's project only), read-only: by real path
inside that folder, no dot-file or dot-folder, known types only, 5 MB at most (`insideRoot`). It
answers under `Content-Security-Policy: sandbox allow-scripts` (an opaque origin), so a script an
agent wrote there can't call Kode's API or drive its page. An opaque origin's own requests reach
Meko as another site's (`Sec-Fetch-Site: cross-site`), which Meko refuses (`SameSite` in
`src/meko/internal/server/local.go`), so an HTML page comes back with the scripts and stylesheets it
names in the demo put inline (`inlineAssets`): only its own top-level load goes through Meko. It
also gets the bundle's `play` line in a bar across its top (`withBar`): the game opens in a tab of
its own, and the line says to go back to Kode's.

**The page.** `TourPanel` (`src/web/components/TourPanel.tsx`) sits above the session area on every
tab of a workshop's project: the welcome card alone at first, then the levels, the stages (a row that
scrolls sideways on a phone, keeping the current one in view), the guide card (what the level
teaches on its first stage, its text as Markdown in a larger size, its buttons and links) and the
scoreboard beside it, stacked on a phone. A link marked `first` (Play the game) comes before the
buttons and is the blue one until it's clicked (kept in this browser), then Move on is. A bold
**m** in the text, which names Meko's mark on a tab, shows in the accent blue. With no session
open, the panel takes the whole area: there's no terminal yet. The tour's project never asks to be
closed when its last session closes (`App.tsx`): its levels end by archiving their sessions.

**Old hosts.** At its start, before recovery, the server archives any open `purpose = 'guide'`
session (`archiveOldHosts`), and its tour goes on from its records. `SessionPurpose` keeps `guide`
only so stored rows read; a host relaunched meanwhile starts on the Explore prompt.

## No PR in the demo

A workshop's project (`Project.workshop`) opens no PR: its repo has no remote, and Open PR has no
remote check, so its push and `gh pr create` would fail and leave the session in Pull Request with
Merge unavailable. The server refuses Open PR, Run to merge (on a session and at a session's start)
and a move to Pull Request there with a 409 (`checkOpensPr` in `lifecycle.ts`; the session tools'
`open_pr` and `move_session` go through the same calls). In the UI, the Code gate shows "No PR in
the demo" and **Run code review** (the `review-run` action, while `canRunReview` offers it;
`demoReviewGate` in `src/web/util.ts`) where Open PR and Run to merge would be, except while the
tour runs there (`tourRuns`): for the whole tour its guide card's button is the next step (the
review's own on its review steps, none on the others), and the only filled blue one on screen (the
rail marks the current step without filling it, and the install-as-an-app banner waits while the
tour's project is on screen), so the gate leaves Run code review to the ⋯ menu and the palette; while a review's
findings wait on the user, or it waits in its terminal, the gate shows that as a review's gate does
(**Findings wait on you**, **Send to Kode**). The session's ⋯ and the palette leave out Open PR
with instructions. Every review there skips the PR description (`runNow` turns `describe` off for a
workshop's project), so Run code review sends the task nothing first. Tasks there end at Code.
