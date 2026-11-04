# Reviewing a PR

Part of the [AgentK design](../design.md).

The other side of sharing a project: someone the author shared it with (see
[Sharing a project](sharing.md)) opens its datapack as a project, from the invite's
banner, and reviews the PR with their own AgentK. The New session dialog opens by itself for a
project just opened, and starts on **Review a PR** when an open PR names the project's datapack.

**Picking the PR.** Picking **Review a PR** swaps the first prompt for a **Pull request** picker
and **Notes for AgentK** (optional: what to focus on). The picker lists, newest first and at most
20, the open PRs in any repo you can read whose description names one of the project's datapacks
(`gh search prs "<datapack id>" --match body`, once per datapack: the PRs its sessions open end
with that id, followed by `;` and the datapack's name, see the PR stage in [Task lifecycle](lifecycle.md)). A project's sessions can work in several repos, so the PRs
can be in several repos too, and each row names its repo. **Another PR** takes any other one: a
number (looked up in the repo picked under Advanced), `owner/name#number`, or a PR link. GitHub's
search lags new PRs by a minute or so, so a PR missing from the list can be typed, and
**Refresh** searches again. `GET /api/projects/:id/review-prs` serves both (`src/server/prreview.ts`).
Each PR's repo is matched to a local clone among the scanned repos (and the project's and its
sessions' repos) with the rules the Open project dialog uses for a datapack's remote
(`matchRepo()`): a fork whose `upstream` is the PR's repo first, then a clone whose `origin` is,
never a name alone. A PR whose repo isn't cloned here shows **Clone** (`POST /api/repos/clone`,
which clones into the repos folder as Open project does). A typed PR whose description names a
datapack the project doesn't have offers **Attach it**, as a shared datapack, so its design can be
searched. The name fills in as `Review #21 <title>` until you edit it, and **Advanced** shows the
repo (the PR's clone, changeable to another clone of it); the base and branch come from the PR.

**A public repo's PR.** Anyone can open a PR on a public repo, so a review of one runs without
the repo's own config: Claude Code loads only your user settings (none of the PR's `.claude/`
settings, hooks, status line or `.mcp.json` servers), and the PR's `.agent-k/stages/` prompts aren't
used. On Kiro, a PR that brings its own `agentk` agent (or `agentk-trusted`, with `[kiro] trust_tools` on Kiro 3.0) or MCP servers can't be reviewed. A private
or internal repo's PR runs as any session does. See [Code from public repos](security.md#code-from-public-repos).

**Starting.** `CreateSessionRequest.pr` names the PR (`{ repo: 'owner/name', number }`), with
`repoPath` its clone. The server checks that the clone's `upstream` or `origin` is the PR's repo
and that the PR is open (400 otherwise, with nothing started). Then, after the dialog has closed
(a fetch that fails is a failed start: see [New session](open-project-and-new-session.md#new-session); the PR isn't polled for new
commits until the session is created), it fetches `refs/pull/<n>/head` from
that remote into `refs/agentk/pr/<n>` (a fork's PR too) along with the PR's base, and puts the
branch `<prefix>review-pr-<n>` (numbered like any session's, so PR #7 of two repos never collide)
at that commit, in a new or idle worktree, with no upstream and a `pushRemote` naming a remote
that doesn't exist, so a bare `git push` fails whatever the push config (`push.default=current`
and `push.autoSetupRemote` included); an explicit `git push <remote>` still works when the user
asks for one. The base is the PR's base, so the footer diff and the Changes panel show the PR's
diff, and the prompts compare with the remote's branch the review fetched (`upstream/main` in a
fork), never the local one, which may follow the fork, lag behind or not exist. The
session keeps the PR's repo, title, author, the commit checked out (`review_sha`) and the PR's
head and state at the last look (`Session.reviewingPr`). The search at start
([Meko recall](recall-and-learn.md#meko-recall-at-start)) uses the PR's title and description, without its Meko
lines, and the notes.

**The PR's design.** When the PR's description has a `meko_design:<name>;<hash>` line (written by
the author's AgentK, see [The design after approval](lifecycle.md#the-design-after-approval)), AgentK fetches
that version from Meko while the PR and the worktree are made ready (`src/server/prdesign.ts`;
the startup step reads *Fetching the PR and its design, and preparing the worktree*). Meko's
`artifact_get` takes only an artifact's content hash, which is why the line carries it. It asks
the datapacks the PR's Meko lines name that the project has (all of the project's when the PR
names none), and keeps the file only when its bytes hash to the line's hash. The first prompt waits
for it up to 15 seconds; one that comes later still shows. The copy becomes the session's design:
a `designs` row (`source` says which line it came from, and it's never sent to Meko), and
`design.md` or `design.html` in the session's designs folder, which a line added in code to the
`review-pr` prompt tells Claude to read, its *Key Design Decisions* section included (or *Questions
and decisions*, its name in designs written before), as
content written by the PR's author (the path is its `claudePath()`, like every path in a prompt).
The Design panel opens by itself once per session (kept in the browser's localStorage) and says
*from the PR*; it has no Questions tab, since the questions and decisions are in the doc. Each poll
reads the line again (after its other work, and keeping what it fetched only while the review is
still open): a new hash (the author changed the design, often after pushing the commits) replaces
the copy and logs it. A version Claude hasn't read (`ReviewingPr.designUnread`: one that came after
the first prompt, or a newer one) makes **Update to latest** show even with no new commits. With
new commits its prompt gets the design line too; without, the `review-pr-design` prompt has Claude
read the design alone. Either says it changed only when Claude read an earlier version. A version
Meko says it doesn't have (an expired artifact) or whose bytes don't match isn't asked for again
until the line changes; one Meko couldn't be asked for (off or failing) is tried again after 10
minutes. A review whose start fails removes any copy it fetched.

**AgentK assists; the user reviews.** The `review-pr` prompt tells Claude to get ready without
reviewing: read the PR description, the reviews and threads and CI's state, look at the shape of
the diff, then show a short summary (what the PR does and why, the main changes and where to start
reading, the review so far, what the author asked reviewers to look at) and wait. It lists no
problems and gives no verdict on its own; it answers what the user asks (explain code, trace a
path, check a suspicion, run a test, draft a comment in their words). Every `gh` command names the
PR's repo (`--repo owner/name`), since the clone may be a fork. With datapacks, the prompt starts
with a Meko search for the decisions and gotchas behind the PR, and a second line (`mekoQuestionLine()`, added in
code like the other Meko lines, and to a repo's own `.agent-k/stages/review-pr.md` too) asks for
a Meko search before every answer about why the code is the way it is, saying which parts came
from Meko and which from the code. The system prompt's workflow line says the same, so it holds
after a resume or `/clear`. Nothing goes to GitHub, and nothing is committed or pushed, until the
user asks; then Claude posts one review with their comments inline
(`gh api repos/<owner>/<name>/pulls/<n>/reviews`), with the verdict they give. Comments left in
the Changes panel go out through the `review-pr-comments` template: they're the user's review
comments, to answer or draft as review comments, never changes to make or comments of Claude's own.

**Keeping up with the PR.** The session is polled like a Review & PR one (every 45 seconds and at
each turn end, `prWatch`), and nothing is sent to Claude by itself, so waiting costs no tokens.
New commits on the PR show in the footer and the stage bar (*3 new commits*, or *PR force-pushed*
when the commit reviewed is no longer in it), with one notification per new head. **Update to
latest** fetches the head, moves the review branch there (`reset --keep`) and sends the
`review-pr-update` prompt: a short summary of what changed since the commit reviewed so far and of
the threads that got replies, again without reviewing. The button reads *Updating* with moving
dots, and takes no second press, from the click until the state shows the update (its answer
comes before the state, which is pushed within 100 ms), since the fetch can take a while. It's refused while AgentK is mid-turn or
waiting on you, while the worktree has uncommitted files, or while the branch has commits of your
own on top of the commit reviewed (move them to another branch first); queued in
`pending_prompts` if Claude is busy. A review
you post is logged ("You requested changes on PR #21"). When the PR is merged or closed, a
notification says so, and **Close review** becomes the main button. It archives the session,
deletes the review branch (kept when it has commits of yours on top of the commit reviewed, or
when git can't tell) and `refs/agentk/pr/<n>`, and is refused while the worktree has uncommitted
files. Discard works as for any session; its warning counts only your own commits, not the
author's, which are on GitHub. A closed review ends in Done with someone else's PR, so the usage
counts never take it for one of your merged PRs. A Review a PR
session can't be moved to another stage, and no session can be moved into one.
