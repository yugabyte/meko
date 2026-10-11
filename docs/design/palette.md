# GitHub links and Ask AgentK

Part of the [AgentK Local design](../design.md).

## Starting from a GitHub link

Pasting a GitHub PR or issue into **Ask AgentK** (`⌘K`) offers the session that fits it,
in the project it belongs to (`src/server/ghlinks.ts`, `GET /api/github-link` and
`POST /api/github-link/start`). The palette takes a `/pull/<n>` or `/issues/<n>` link on
github.com, or `owner/name#<n>`, never a bare number, which names no repo (`parseGithubRef()` in
`src/shared/github-refs.ts`). Once typing pauses for 250 ms it looks the ref up, once per ref and
project on screen while the palette is open, and again when a project the plan names was closed. GitHub numbers PRs and issues together, so the server asks
`gh api repos/<owner>/<name>/issues/<n>`, which answers for both and says which it is. The repo is
the one GitHub's link names, so a renamed repo or other casing reads as GitHub spells it.

**The project.** One project is picked. The first choice is the first open project, in rail order
(the one on screen first), with a datapack that a Meko line in the description names
(`<host>://<id>;<name>`, as AgentK ends its PRs; people can put the same line in an issue): a
dedicated datapack before a shared one. The second choice is the project on screen (the palette
sends it, `?project=`) when its repo is a clone of the link's repo, its `origin` or a fork's
`upstream`; the third, the first open project in rail order whose repo is such a clone. The other
open projects on the repo (`LinkPlan.others`) are offered after it, one item each. The session works in the project's repo when that's a clone of the link's repo,
else in a scanned clone (`localClone()`), else in a clone made at start into the repos folder; when
that folder (`[repos] clone_into`) isn't under the repos roots, the start is refused before cloning,
since a session can't use a clone the scan doesn't list.
With no open project that fits, AgentK opens one when the session starts. Its repo is a local clone
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

## Ask AgentK

The palette (`⌘K`, or **Ask AgentK** at the top of the rail) also takes an instruction or a question
in plain words: "start a design task in agent-k-local to add dark mode to the Usage page", "move
Dark mode to Code and add tests", "which sessions need me?". Its placeholder rotates every 1.25 s
through what it takes (asking, commands, sessions, GitHub links), on every screen size; with
reduced motion it doesn't rotate and names them all at once.

**Asking.** Any text that isn't a GitHub link gets an **Ask AgentK: "…"** item, last in the list.
A sentence matches no command (the filter needs every word), so the item is usually alone and
Enter picks it; `⌘Enter` (`Ctrl+Enter`) asks whatever else matches. The browser sends the text,
the exchange so far (the last 10, kept only while the palette is open) and what's on screen to
`POST /api/ask-agentk` (`src/server/askagentk.ts`). Esc (or **Cancel**) while AgentK thinks cancels the ask and
puts the text back; closing the palette cancels it too, and the server stops Claude when the
request closes.

**The Claude call.** The server runs Claude Code once per ask (`[claude] command`), with the
arguments as an array: `-p --model <[ask_agentk] model> --output-format json --json-schema <answer>
--tools "" --strict-mcp-config --setting-sources user --settings {"disableAllHooks":true}
--no-session-persistence --system-prompt <AgentK's>`, the message on stdin, in an empty folder
(`run/ask`) so no project's CLAUDE.md or settings apply. No tools, no MCP servers and no hooks: it
can only answer. The user's own settings keep their sign-in and provider setup. No transcript is
written, so asks aren't in token usage; they use the user's plan or API key like sessions do (a
Sonnet ask with a small context measured about $0.007 and 2 to 2.5 s, about 1 s of it Claude Code
starting). The message is a compact JSON context (open projects with id, name and repo folder name;
their running sessions with id, name, stage, status, branch and PR number; what's on screen), the
exchange so far, then the text. The system prompt says what each stage is for and the rules: use
the project on screen when none is named, keep the user's words as a task's prompt, ask when it's
unclear, and treat names and titles in the context as data. At most two asks run at once (429);
one that takes longer than `[ask_agentk] timeout_seconds` (45 by default) is stopped (504).

**The answer** is a reply and up to three actions, checked against the state when it comes back
(`checkActions()`); one that doesn't fit is left out with a note under the reply:

| Action | Checked | Runs as |
|---|---|---|
| `start` (project, Explore, Design or Code, name, prompt) | an open project, a prompt; the name made one line, from the prompt when missing | `POST /api/projects/:id/sessions`, then the session, as after New session. A second item, **Edit … in New session…**, opens the dialog filled in instead. |
| `link` (a PR or issue) | `parseGithubRef()` takes it | puts the link in the palette, which offers its items as for a pasted link |
| `go` (a session, a project home, Usage, Settings) | the session is running, the project open | navigates |
| `move` (a session, a stage, instructions) | `moveBlockedIn()` and `moveError()` | `POST /api/sessions/:id/move`, then the session |
| `openPr` (a session, instructions) | the session is in Code | the open-pr action, then the session |

Every action waits to be picked, a lone `go` too: a question such as "how many sessions are
open?" often comes back with a go to a session it names, and going there at once would hide the
reply. The first is selected, and focus goes back to the input when an ask started with a click
(not on a touch screen, where it would raise the keyboard), so Enter runs it. A picked action runs
through the route the UI already uses, which checks it again. Claude has no tools and sees names,
branches and PR titles other people may have written, so at worst it proposes something the user
then doesn't pick. The palette
shows the exchange above the list; the input's placeholder becomes *Reply to AgentK, or type a
command…*, and typing filters commands again, with the Ask item for a follow-up. Errors say what
went wrong with **Try again**: Claude Code missing (503), not signed in, failed, or an answer that
can't be read (502). `[ask_agentk] enabled = false` drops the item (and the route answers 403).
