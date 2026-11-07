# GitHub links, Ask Kode and finding a session

Part of the [Kode design](../design.md).

## Starting from a GitHub link

Pasting a GitHub PR or issue into **Ask Kode** (`⌘K`) offers the session that fits it,
in the project it belongs to (`src/server/ghlinks.ts`, `GET /api/github-link` and
`POST /api/github-link/start`). The palette takes a `/pull/<n>` or `/issues/<n>` link on
github.com, or `owner/name#<n>`, never a bare number, which names no repo (`parseGithubRef()` in
`src/shared/github-refs.ts`). Once typing pauses for 250 ms it looks the ref up, once per ref and
project on screen while the palette is open, and again when a project the plan names was closed. GitHub numbers PRs and issues together, so the server asks
`gh api repos/<owner>/<name>/issues/<n>`, which answers for both and says which it is. The repo is
the one GitHub's link names, so a renamed repo or other casing reads as GitHub spells it.

**The project.** One project is picked. The first choice is the first open project, in rail order
(the one on screen first), with a datapack that a Meko line in the description names
(`<host>://<id>;<name>`, as Kode ends its PRs; people can put the same line in an issue): a
dedicated datapack before a shared one. The second choice is the project on screen (the palette
sends it, `?project=`) when its repo is a clone of the link's repo, its `origin` or a fork's
`upstream`; the third, the first open project in rail order whose repo is such a clone. The other
open projects on the repo (`LinkPlan.others`) are offered after it, one item each. The session works in the project's repo when that's a clone of the link's repo,
else in a scanned clone (`localClone()`), else in a clone made at start into the repos folder; when
that folder (`[repos] clone_into`) isn't under the repos roots, the start is refused before cloning,
since a session can't use a clone the scan doesn't list.
With no open project that fits, Kode opens one when the session starts. Its repo is a local clone
of the link's repo, or a clone of `https://github.com/<owner>/<name>.git` (`Projects.create` with
`cloneUrl`). Its dedicated datapack is one the description names that Meko lists for you, or a
share waiting for you (accepted on the way), unless you can only read it (a viewer): sessions write
to the dedicated datapack, so a read-only one counts as unnamed. When Meko can't be asked, the
lookup fails and says so, rather than planning a project without the named datapack. A closed project with that dedicated datapack opens
again as itself, under its own name; closed projects aren't matched by repo. A datapack named this
way also names the project (`parseProjectDatapack()`). Otherwise a PR review's project has no
datapack, since a review only reads code, and an issue's project gets a new one
(`adlc/proj/<repo>/<name>`, made free of the names Meko lists). At the plan's datapack limit it
opens without one. Without Meko, no datapack.

**The session.** A PR gets a Review a PR session (see [Reviewing a PR](review-pr.md)), named
by the server as from the New session dialog. A merged or closed PR is refused (its item says
why), and a PR that a running session already reviews offers **Go to** that session instead
(409 if started anyway). An issue offers two items, **Work on issue #<n>** (starts at Design, what
Enter picks) and **… straight to Code**. Each starts a task named `#<n> <title>`, whose first prompt
names the issue and its link and asks Claude to read it with its comments
(`gh issue view <n> --repo <owner>/<name> --json author,body,comments`), then fix it if it's a bug
or implement it if it asks for a feature or change. As for a task's PR, Claude acts only on what
the issue asks and on comments from the user and from people whose `authorAssociation` is OWNER,
MEMBER or COLLABORATOR; anyone else's are untrusted text. The number stands alone (`#<n>`), so the search at start
([Meko recall](recall-and-learn.md#meko-recall-at-start)) reads the issue too. A closed issue is still offered, and
says so in its hint. Running tasks whose first prompt has the issue's link ending a line (so #45's isn't found in
#450's) are offered first as
**Go to**, with a new session still offered after them. After the picked project's items, each
other open project on the repo gets one: **Review #<n> … in <project>** for a PR, **Work on issue
#<n> … in <project>** (at Design) for an issue. Link items wrap rather than being cut, so the
project at the end of the label always shows. Each item's hint says what starting does
besides: which project and why (**Names its datapack**, **This project**, **On <owner>/<name>**, or
**Also on <owner>/<name>** for the other projects), **Opens a new project** (or **Opens the project again**),
**Clones <owner>/<name>**, **New datapack**, or **Accepts the share of <datapack>**. While it
starts, the palette stays open with *Starting* (or *Cloning <owner>/<name>*). Once the server
answers, the palette closes and the page goes to the session as after New session. Closing the
palette meanwhile doesn't stop the start; the session shows up in the state. The browser never
says which project or datapack to use beyond the project it showed: the server works the rest out
again when it starts, and uses an open project that fits by then instead of opening another.

## Joining by a Meko share link

A datapack's share link (`{web app}/share/{token}`) pasted on its own gives one item, **Join the
datapack this link shares** (`joinEntry()` in `src/web/palette.ts`), and no Ask Kode item or
session search; `⌘Enter` doesn't ask either, so the token goes only to Meko. Only a link on the Meko web app Kode uses counts (`shareLinkToken()` in
`src/shared/mekourls.ts`, from `AppState.meko.webUrl`). Picking it joins, with *Joining* meanwhile;
then the palette opens the Open project dialog with the share chosen, or goes to the project here
that already uses the datapack, or stays open saying what to do (`joinedText()`). The rest is in
[Sharing a project](sharing.md).

## Ask Kode

The palette (`⌘K`, or **Ask Kode** at the top of the rail) also takes an instruction or a question
in plain words: "start a design task in meko to add dark mode to the Usage page", "move
Dark mode to Code and add tests", "which sessions need me?". Its placeholder rotates every 1.25 s
through what it takes (asking, commands, sessions, GitHub links), on every screen size; with
reduced motion it doesn't rotate and names them all at once.

**Asking.** Any text that isn't a GitHub link or a Meko share link gets an **Ask Kode: "…"** item, after the commands
that match and before the [sessions found](#finding-a-session). A sentence matches no command (the
filter needs every word), so the item comes first and Enter picks it; `⌘Enter` (`Ctrl+Enter`) asks whatever else matches. The browser sends the text,
the exchange so far (the last 10, kept only while the palette is open) and what's on screen to
`POST /api/ask-kode` (`src/server/askagentk.ts`). Esc (or **Cancel**) while Kode thinks cancels the ask and
puts the text back; closing the palette cancels it too, and the server stops Claude when the
request closes.

**The Claude call.** The server asks the default agent (`[agents] default`, when it's available;
else the first available one, `Harnesses.ensureDefault`; with none it answers 409, and the palette
doesn't offer Ask Kode while no agent is available, `AppState.askAgentk`). On Kiro it runs `kiro-cli chat --no-interactive --output-format stream-json
--agent kode-ask`, the message on stdin, in the same empty folder, where Kode writes the
`kode-ask` agent as a workspace agent (`.kiro/agents/kode-ask.json`, so it never shows in the
user's agent list): the system prompt below plus the answer's JSON schema (Kiro has no schema
option), no tools, no MCP servers, no hooks. The answer is the `runFinished` event's `finalText`, its
JSON object read even inside a code fence; Kiro keeps every run as a session, so Kode deletes the
ask's (`chat --delete-session`) once it has the answer, in the background (a server that is
stopping waits up to 5 seconds for deletes still running; a run that failed is deleted too). On
OpenCode it runs `opencode run --standalone --format json --agent kode-ask`, the message on stdin, in the same
folder, with OpenCode's global config pointed at the ask's own (`XDG_CONFIG_HOME`), which defines
`kode-ask` with the system prompt and the answer's shape and `permission: "deny"` and keeps of the
user's config only their model and providers, so the user's tools, plugins (hooks) and MCP servers
don't run, and project config off; the answer is its `text` events, and its session is deleted the
same way (`opencode session delete`), answered or not ([Agents](harnesses.md#opencode)). `[ask_kode] model` is Claude's; Kiro and OpenCode use their own default. On Claude Code, the server runs it once per ask (`[claude] command`), with the
arguments as an array: `-p --model <[ask_kode] model> --output-format json --json-schema <answer>
--tools "" --strict-mcp-config --setting-sources user --settings {"disableAllHooks":true}
--no-session-persistence --system-prompt <Kode's>`, the message on stdin, in an empty folder
(`run/ask`) so no project's CLAUDE.md or settings apply. No tools, no MCP servers and no hooks: it
can only answer. The user's own settings keep their sign-in and provider setup. No transcript is
written, so asks aren't in token usage; they use the user's plan or API key like sessions do (a
Sonnet ask with a small context measured about $0.007 and 2 to 2.5 s, about 1 s of it Claude Code
starting). The message is a compact JSON context (open projects with id, name, repo folder name
and `github`, the repo's `owner/name` on GitHub, its upstream for a fork, so a bare `#5` can be
resolved; their running sessions with id, name, stage, status, branch and PR number; what's on screen), the
exchange so far, then the text. The system prompt says what each stage is for and the rules: use
the project on screen when none is named, match a session's name loosely, read a bare `#N` against
the project's `github`, keep the user's words as a task's prompt, ask when it's unclear, say whether
the actions go together, and treat names and titles in the context as data. At most two asks run at once (429);
one that takes longer than `[ask_kode] timeout_seconds` (45 by default) is stopped (504).

**The answer** is a reply, up to eight actions (`ACTIONS_MAX`) and `together`, checked against the
state when it comes back (`checkActions()` in `src/server/actions.ts`, which [Kode's session
tools](tools.md) share); one that doesn't fit is left out with a note under the reply:

| Action | Checked | Runs as |
|---|---|---|
| `start` (project, Explore, Design or Code, name, prompt, and Run to merge, `autoRun`) | an open project, a prompt; the name made one line, from the prompt when missing; `autoRun` only with Code, else left out with a note | `POST /api/projects/:id/sessions`, then the session, as after New session. A second item, **Edit … in New session…**, opens the dialog filled in instead. |
| `link` (a PR or issue, and optionally the project to start it in, an issue's stage, Design or Code, and `autoRun`) | `parseGithubRef()` takes it; the project, when given, is open; `autoRun` as for `start` (Do all only: a PR refuses it) | puts the link in the palette, which offers its items as for a pasted link; in Do all (`POST /api/actions/run`), `githubLinks.start` in that project, or in the one the link fits when none is given, refusing an issue a running task works on (`once`, which the pasted link's own route doesn't pass: its items already offer that task) |
| `go` (a session, a project home, Usage, Settings) | the session is running, the project open | navigates |
| `move` (a session, a stage, instructions) | `moveBlockedIn()` and `moveError()` | `POST /api/sessions/:id/move`, then the session |
| `openPr` (a session, instructions) | the session is in Code | the open-pr action, then the session |
| `openProject` (a repo: a scanned repo's folder, GitHub `owner/name` or a clone URL; a name; a datapack: `new`, `none` or an id) | a repo | `POST /api/actions/run`, then the project home ([the tools' `open_project`](tools.md)) |

**Do all.** `together` says the actions are the steps of one instruction ("open sessions for #1, #5
and #6 and move the ABC session to Code": three links and a move) rather than alternatives to pick
one of. With two or more steps that do something (every action but `go`, `doAllActions()` in
`src/web/palette.ts`), **Do all N** comes first, above the steps' own items. It sends them to
`POST /api/actions/run`, which checks them again against the state now and runs them in order
(`runActions()`), one that fails not stopping the rest. The palette then shows a line per action
(✓ or ✕ and what happened) under the reply, and in place of the actions, an item to go to each
session or project they led to.

Every action waits to be picked, a lone `go` too: a question such as "how many sessions are
open?" often comes back with a go to a session it names, and going there at once would hide the
reply. The first is selected, and focus goes back to the input when an ask started with a click
(not on a touch screen, where it would raise the keyboard), so Enter runs it. A picked action runs
through the route the UI already uses, which checks it again. Claude has no tools and sees names,
branches and PR titles other people may have written, so at worst it proposes something the user
then doesn't pick. The palette
shows the exchange above the list; the input's placeholder becomes *Reply to Kode, or type a
command…*, and typing filters commands again, with the Ask item for a follow-up. Errors say what
went wrong with **Try again**: no coding agent available (409), the agent's command missing (503), not
signed in, failed, or an answer that can't be read (502). `[ask_kode] enabled = false` drops the item (and the route answers 403).

## Finding a session

Typing in the palette filters its items by name, which finds a running session only by its name,
project or branch. Once typing pauses (250 ms) on two characters or more, the palette also asks
`GET /api/sessions/search` (`src/server/sessionsearch.ts`) for the sessions the text is about: in the
project on screen, or every open project from elsewhere. The server reads each session's name, PR
number and title, task (its first prompt), branch and design markdown from `state.db`, archived
sessions included, and ranks them with `rankSessions()`. A Review a PR session has its PR's title from
the start; a task's own PR gets it from the review poll (`pollReview()` in `lifecycle.ts`, which keeps
`sessions.pr_title` current), so a task whose PR was never polled is found by its PR number only.

- The words are the text split on anything but letters and digits, lowercased, without one-letter
  words and stop words ("the", "session", "find"…), unless that leaves none.
- A word matches at the start of a word in a field, without its ending first (`stem()`: -ing, -ed,
  -es or -s, and a doubled last consonant after one, keeping three letters at least), so
  "searching" finds "search" and "searches", "rotating" finds "rotate", and "logging" finds "log".
- A session needs at least half the words. It ranks by how many it has, then by where: each word
  counts for the field that counts most among those having it (name 8, PR 6, task 5, branch 3,
  design 2). Then open before closed, then newest. At most 20 come back.
- Each says the field that counts most among those that matched, and the text around its first
  match on one line (none for the name).

The browser shows them after Ask Kode's item, so Enter still asks Kode when a sentence matches
no command, and leaves out the running sessions the name filter already shows. Each shows its
project (when not the one on screen), its stage or *Closed* with its PR number, and the matching
text under the name, wrapped, so it reads on a phone. Picking a running session goes to it; a
closed session isn't in `AppState`, so picking it opens its PR on GitHub, and one without a PR only
shows. A result is kept with the text and project it was for, and a newer search cancels an older one.

Everything searched is in `state.db`, so a search takes no Claude or Meko call. Not searched: the
transcripts (what was said in the session) and an Artifact design's page.
