# Using Kode with Meko

A guide for teams starting with Kode: what it does with projects, sessions, stages, code reviews
and agents, how it learns with Meko, then how to set it up, lay out projects and datapacks, share
them with your team, and the habits that make the shared context worth having. The
[README](../README.md) is the short version, [features](features.md) lists everything Kode does,
and [install](install.md) has the install details.

## What Kode does

- **Projects.** A project is an effort on a service, feature area or migration, worked on in one
  git repo. Each has a Meko **dedicated datapack** that holds what its sessions learn, and can
  read any number of **shared datapacks** (team conventions, the repo's gotchas). Sharing a
  project with your team is sharing its dedicated datapack.
- **Sessions.** Each task is a session with its own git worktree, branch and coding agent, so
  many tasks run at once without stepping on each other. You see the agent's real terminal in
  your browser or on your phone, and sessions keep running when you close the page or Kode
  updates.
- **Stages.** Every task runs on **Explore → Design → Code → Pull Request → Merge & Close**.
  Explore is for investigating and experiments, Design ends with a design you approve, Code ends
  with a PR you approve opening, and Merge & Close merges it and cleans up. Start a task at
  whichever stage fits and move it on or back between turns, or check **Run to merge** and Kode
  passes the gates by itself, stopping only when it needs you.
- **Code reviews.** Before a PR opens, Kode reviews the change on your computer (Sagittarius 3.0:
  five specialist passes and a scorer for each finding; or the quicker Sirius 1.0). You check the
  findings, keep or dismiss each with a note, and Kode fixes what you keep. The review also
  searches the repo's gotchas in Meko, and marks what that caught **Caught with Meko**. Run it any
  time from a session, or on any checkout with `meko kode review`. For someone else's PR, a
  **Review a PR** session sums it up with the author's design beside it.
- **Claude Code, OpenCode or Kiro CLI.** Each session runs on the agent you pick, with the same
  stages, gates, design review and PR flow; pick a model per session too. On Claude Code,
  designs are Artifacts and you can answer its questions in Kode; on Kiro, Kode shows the
  credits it used.
- **Learning with Meko.** When a session starts, Kode searches the project's datapacks for its
  task and gives the agent what the team already knows, and each stage's prompt asks it to search
  Meko before reading code. Designs go to Meko as they're approved, and the PR names them. When a
  task merges or is discarded, Kode's **learn step** asks the session what it learned (how things
  work, decisions and why, root causes, gotchas, measured numbers) and saves what the datapack
  doesn't have yet, correcting what it has wrong. **Learn the project** seeds a new datapack from
  the repo's files and its merged PRs.
- **Self-learning.** Meko keeps the searches it couldn't answer as **Context Gaps**. When a later
  session finds the answer, Kode's learn step (on Claude Code, through Kode's mod) saves it and
  marks the gap taught, so the next session that asks finds it in Meko. Over time the datapack
  fills in where agents actually got stuck.

## Meko in one minute

- **Meko** is the context engine. Its unit is the **datapack**: memories (short facts), a
  knowledge base (documents), conversations and artifacts (files such as designs), searched
  together with `context_search`.
- **Meko Cloud** holds datapacks your team can share. Kode always talks to the Meko on your own
  computer; once that Meko is linked to Meko Cloud, it passes each call to the side that holds
  the datapack, and new datapacks are made in Meko Cloud.

## 1. Set up

Each person on the team does this once, on the machine where their agents run (a devserver, a Mac
or a Mac mini). You need Linux (x64 or arm64) or macOS 14+ on Apple Silicon, and Claude Code,
OpenCode or Kiro CLI signed in.

1. **Install Meko, then Kode.**

   ```sh
   curl -fsSL https://mekodata.ai/install.sh | sh
   meko extension install kode
   ```

   Setup offers to install what's missing and prints the address to open (`meko kode ui` opens it
   later).

2. **Link Meko to Meko Cloud.** Sign up at [cloud.mekodata.ai](https://cloud.mekodata.ai/), then
   either Settings › Meko Cloud in Meko's web UI (`meko ui`), or on a machine without a browser:

   ```sh
   meko cloud-link
   ```

   Without the link, datapacks stay on your computer and can't be shared: Kode's setup ends with
   `! Meko Cloud  not linked` to remind you.

3. **Connect your agent to Meko**, so sessions can search and save:

   ```sh
   meko agent list               # which agents reach Meko
   meko agent connect claude     # or kiro, opencode
   ```

   Kode also offers this in a banner when an agent isn't connected, or is connected to another Meko.

4. **Take the tour.** A new install opens on it (or **Start the Kode tour** in `⌘K`). It
   shows a code review with and without a team's gotchas in Meko, and a session answering from a datapack that Kode seeded. It runs on a local demo repo with no remote.

Kode has no login: with remote access (the default over SSH) anyone who can reach port 7777 can
open it and its terminals. See [Who can open it](install.md#who-can-open-it).

## 2. Lay out projects and datapacks

### One project per effort, one dedicated datapack per project

**Open project** picks a repo from your repos folder (`~/code` by default), clones one from a URL,
or starts a new one. Its dedicated datapack is named `adlc/proj/<repo>/<project>` unless you pick
an existing one. A repo can back several projects (say `payments/retries` and `payments/ledger`),
each with its own datapack, sessions and settings.

How to split: a project is an outcome a group of people works on together for weeks or months: a
service, a feature area, a migration. Split when two groups shouldn't read each other's working
notes, or when the subjects are so different that one's facts would be noise in the other's
searches. Don't split per task: tasks are sessions.

### Shared datapacks for what many projects need

Attach shared datapacks to a project from its home page. Sessions read them but never write to
them. Good candidates:

| Datapack | What goes in it |
|---|---|
| `adlc/repo/<repo>/gotchas` | The traps reviewers in this repo keep catching. Meko Cloud's gotchas miner fills it from PR review threads; Kode's code review searches it and marks findings **Caught with Meko**. |
| Team or org conventions | Coding standards, release process, on-call runbooks, architecture decision records: as documents in the knowledge base. |
| A neighbouring project's datapack | When this project calls that one's service or library. |

### Seed a new datapack: Learn the project

An empty datapack can't help anyone. When you open a project on a new datapack, New session starts
on **Learn the project**: say what the project is about, which folders matter and how many months
of merged PRs to read (24 by default). The session reads them and saves what a new teammate would
want to know: how the parts fit, how to build and test, the traps, and the decisions and why. It
changes no files and closes by itself.

It can take hours and millions of tokens on a big repo: the dialog warns past 200 PRs or 5,000
files. Name the folders you care about, or read fewer months.

### Add documents

Design docs, specs, runbooks and API references belong in the knowledge base. Drop or paste a PDF,
Markdown, text, JSON, CSV, XML or HTML file on a session's terminal and choose **Upload to Meko**,
or upload from Meko's web UI, or run `meko-cli upload <file>...`.

### Put a MEKO.md in the repo

A `MEKO.md` lists the datapacks that hold context for the folder it's in, as a table:

```md
| Datapack | ID | What's in it |
|---|---|---|
| `adlc/repo/demo-repo/gotchas` | `<datapack id>` | Traps reviewers here have caught before. |
| `adlc/proj/demo-repo/payments` | `<datapack id>` | Payments: design decisions, how it's built and tested. |
```

Kode's code review reads every `MEKO.md` from the repo root down to the changed files and searches
the datapacks they list. Agents outside Kode (and people) find the context the same way: add a
line to your `AGENTS.md` or `CLAUDE.md` saying to search the datapacks in `MEKO.md` before reading
code.

## 3. Share with your team

**Share** on the project's home shares its dedicated datapack. Invite by email, or turn on a share
link and post it where your team is. Roles:

| Role | Can |
|---|---|
| Viewer | Read the datapack. Can't open it as a Kode project. |
| Contributor (default) | Open it as a project, run sessions and save their own memories. |
| Maintainer | Also promote memories and invite others. |

An invite shows in the teammate's Kode as a banner: **Open as project** opens it as a project on
their machine, with the repo and name filled in. A share link pasted into `⌘K` joins it.

**What teammates see.** In Meko, only the agents of the person who wrote a memory can search it.
Owners and maintainers see everyone's unpromoted memories in the datapack's **Learnings** in Meko's
web UI, but their agents can't find them either. A teammate's agents find the datapack's **shared
knowledge**: its knowledge base and the memories a maintainer has **promoted**. So what your
sessions learn reaches the team's agents once it's promoted (see
[Best practices](#5-best-practices)). Designs are stored as artifacts and named in each PR, so a
reviewer's Kode can fetch the author's design.

## 4. What Kode does with Meko during a task

You don't call Meko by hand during a task: Kode does it at each stage, and asks the agent to.

| When | Reads | Writes |
|---|---|---|
| A session starts | Kode searches every datapack of the project for the task (and the GitHub issues it names) and gives the agent what it found, headed "What Meko already knows about this task". | A Meko conversation for the session in the dedicated datapack. |
| Explore, Design, Code | Each stage's prompt asks the agent to search Meko before reading code; the system prompt names the datapacks and the conversation. | Whatever the agent saves when you ask it to. |
| Design | Decisions that rest on something from Meko cite it (the design panel marks them). | Each version of the design, as an artifact. |
| Code review | The datapacks in the repo's `MEKO.md` files, the gotchas one tagged. | — |
| Pull Request | — | The PR description ends with the datapack, the design and the conversation (`meko_design:…`, `meko_conversation_id:…`). |
| Review a PR | The author's design and the decisions behind it, from the PR's Meko lines. | — |
| Merge & Close or Discard | — | **The learn step**: Kode asks the session what it learned (how things work, decisions and why, root causes, gotchas, measured numbers), checks each fact against the datapack, skips what's known, corrects what's wrong, and saves the rest. |

### Conversations

Every session has its own Meko conversation in the dedicated datapack, named after the session.
Kode's writes for it (designs, learned facts) go under that conversation, and the PR names it, so
anyone can trace a change back to the session that made it. Full session transcripts are not sent
by default; `[meko] capture_transcripts = true` in Kode's `config.toml` sends them (restart Kode
after changing it).

### Searching yourself

- **The project home** has a search box over all the project's datapacks.
- **In a session**, ask the agent: "search Meko for how retries work in the billing client".
- **From a terminal**: `meko-cli search "<question>"`.
- **From any MCP agent**: `context_search` with the `datapack_id`, which queries memories, the
  knowledge base and conversations at once.

### Context Gaps

Meko keeps the searches it couldn't answer on each datapack's **Context Gaps** tab in its web UI;
the project home says how many are open. When a later session finds the answer, Kode's learn step
marks the gap taught, so the next session finds it in Meko.

## 5. Best practices

**Before the first task**

- Link to Meko Cloud before you open projects, so their datapacks are made there and can be shared.
  A datapack made while unlinked stays on your computer.
- Seed every new datapack with **Learn the project**, scoped to the folders the project is about.
- Upload the documents people would otherwise paste into prompts: specs, ADRs, runbooks.
- Commit a `MEKO.md` and point your `AGENTS.md` or `CLAUDE.md` at it, so agents outside Kode use
  the same context.
- Attach the repo's gotchas datapack and your conventions datapack as shared datapacks.

**On every task**

- Write the task the way you'd brief a teammate, and name the GitHub issue (`#123`): the start-up
  search is built from the session's name, task and issues, so vague tasks find little.
- Start at **Design** for anything you'd want reviewed; read the "What Meko already knows" block
  and the decisions marked with Meko's mark before approving.
- Let tasks end with **Merge & Close** or **Discard**, not by closing the browser: that's when the
  learn step saves what the session learned. **Save learnings to Meko** in the session's ⋯ menu
  (or `/kode_learn`) saves mid-task.
- When the agent works something out the hard way, tell it to save it: "save to Meko: the
  integration tests need the emulator on port 8085".

**Keeping the context good**

- A maintainer reviews the datapack's **Learnings** in Meko's web UI regularly (weekly works) and
  promotes the good ones, so teammates' agents find them.
- Look at **Context Gaps** after a few weeks: repeated gaps are documents to upload or facts to
  save.
- Correct wrong memories rather than adding a second one; the learn step does this for facts it
  can check; otherwise ask an agent to correct it (`memory_update`).
- Keep secrets out: no passwords, tokens or keys in prompts you ask an agent to save, or in
  uploaded files.

## 6. When something's off

| Symptom | Check |
|---|---|
| Share says Meko isn't linked | `meko cloud-link`, then share again. A datapack made before the link has to move to Meko Cloud first. |
| A banner says the agent isn't connected to Meko | **Connect to Meko** in the banner or Settings › Harness, or `meko agent connect <agent>`. |
| Sessions start with nothing from Meko | The datapack may be empty (Learn the project), or the task too vague to match. The project home's search shows what Meko has. |
| A teammate's agents don't find your team's learnings | They're private until promoted: a maintainer promotes them in Meko's Learnings tab. |
| Is Kode running? | `meko kode status`; `meko kode logs -f` follows the log. |
