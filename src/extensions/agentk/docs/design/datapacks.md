# Datapacks

Part of the [AgentK design](../design.md).

- **Dedicated datapack (one per project).** It's created with the project, named
  `adlc/proj/<repo>/<project_name>` by default, or you can pick an existing one. It is the only
  place session transcripts and extracted memories are written. It can be switched to another
  datapack from the project's home; the old one then stays attached as shared, or is detached, and
  what was written to it stays there.
- **Shared datapacks (zero or more).** These are attached from your `datapack_list`: a team
  knowledge base, org conventions, another project's memory. They are read-only from this
  project's point of view, and one datapack can be attached to many projects.

Each session is launched with this environment:

```sh
MEKO_DATAPACK_ID=dp_7f3…                 # dedicated: capture + memory writes
MEKO_AGENT_ID=claude_code:fix-retry-backoff-3f9c1a2e  # the session: its slug and a hash of its id
MEKO_DATAPACK_IDS=dp_7f3…,dp_a01…,dp_c9e… # everything this session may read
AK_SESSION=billing-api--fix-retry-backoff
```

A short `--append-system-prompt` also lists the attached datapacks by name and id, so Claude
knows to pass the right `datapack_id` when it searches them, and says when to search: before
reading code for a new task, a review or a CI fix. Claude Code hides MCP tools until ToolSearch
loads them, so it names the query that does (`+meko context_search`, which matches the Meko server
under its own name or a plugin's). The first prompt of each task (`explore`, `design`,
`design-plan`, `code`, `review-pr`) and the review, conflict and CI prompts end with one line asking for that search;
the move prompts (`move-design`, `move-code`) ask for one only if the work has moved on to something not searched yet.
A Review a PR session's prompts (`review-pr`, `review-pr-comments`) and system prompt also ask for a
search before every answer about why the code is the way it is (`mekoQuestionLine()`,
`mekoQuestionSystemLine()`). These lines are added in code (`mekoRecallLine()`, `mekoRecallSystemLine()` in `prompts.ts`), so
a repo's own stage prompts get them too, and only when the project has datapacks and Meko is on.

> **No Meko plugin change needed.** The Meko hooks already take `MEKO_AGENT_ID` over the folder
> name, and they read the datapack pin from `~/.claude/meko-capture/pin-<agent id>.json` (with `:`
> as `_`, e.g. `pin-claude_code_fix-retry-backoff-3f9c1a2e.json`). AgentK
> Local gives every session its own `MEKO_AGENT_ID` (`sessionAgentId()` in `meko.ts`: the slug,
> plus 8 hex digits of a hash of the session id, since a slug is only unique within its project)
> and writes that pin each time the session starts, so capture goes into the dedicated datapack.
> The agent id doesn't name the project: Meko reads memories across agents within a datapack, so
> it only says which session wrote a memory, and every session of a project, and the project
> opened again after a close, sees them all. AgentK's own writes for a session (designs, the
> learn step's new facts) go under the session's Meko conversation with the agent id the session
> runs with (`launchedAgentId()`, recorded in `sessions.meko_agent_id`); a learn step's correction
> keeps the agent id of the memory it corrects (its original writer); the project's conversation
> and its searches (the project home's, and the search at session start) use `agent-k-local`. A session launched before sessions had their own agent id keeps
> `claude_code:<project id>` for all of that until it's launched again. A pin stays for a day after
> its session is archived (the plugin's SessionEnd hook may still read it), then it's removed at
> startup or on the 6-hourly sweep; a restore pins it again. The old project-wide pins
> (`pin-claude_code_<project id>.json`) are never removed: the Meko plugin gives plain Claude Code
> in a repo the agent id `claude_code:<repo folder>`, so for a project named after its repo that
> file may be the user's own pin (one AgentK used to overwrite). The hooks read the pin when a session starts and keep it in the
> session's watermark, so after a switch a running session's capture stays in the old datapack. `MEKO_DATAPACK_ID` is set too, for when the hooks learn to read it.

> **Transcript capture is off by default.** Meko's capture script extracts a skill Claude loads
> as a user requirement and cuts the turn that loaded it short (MEKO-710, fixed in
> yugabyte/meko-mcp-server#360, not yet released), which fills datapacks with skill text. So
> `[meko] capture_transcripts` in `config.toml` defaults to `false`; set it to `true` to send
> transcripts again (a restart applies a change). While it's off, the server doesn't run the
> plugin's `capture.js checkpoint` after each turn, at startup or when a session stops, and the
> `--settings` file sessions launch with turns the Meko plugin off (`enabledPlugins` set to
> `false` for each `meko-agent-skills@…` key in Claude Code's plugin registry, and for
> `meko-agent-skills@meko-agent-skills`), so its SessionStart, PreCompact and SessionEnd hooks and
> its checkpoint timer don't run either. A session launched before the change keeps the plugin
> until it's launched again. Sessions also get `MEKO_CHECKPOINT_INTERVAL=2147483`, the longest
> interval Node's `setInterval` honours (a larger one would fire every millisecond), for a plugin
> that still runs; with capture on, the launcher clears any inherited `MEKO_CHECKPOINT_INTERVAL`.
> The plugin's MCP server is configured apart from it (`~/.claude.json`), so Meko's tools stay.
> (With capture on, a server that is stopping waits up to 5 seconds for captures still running.)
> The learn step's facts are memories, not transcripts, and are still sent.

> **The system prompt gives the conversation_id.** Meko's tools require a `conversation_id`, which
> only the plugin's SessionStart context gave before. Every launch now creates (or finds) the
> session's conversation, keyed by its Claude session id like the plugin's, in the dedicated
> datapack, or the project's conversation (`agent-k-local-project-<id>`, owned by `agent-k-local`)
> when the project only has shared datapacks (`launchConversation()` in `sessions.ts`, waiting up
> to 5 seconds), and the system prompt names it (`mekoConversationLine()` in `prompts.ts`). When
> Meko doesn't answer in time, the line gives the `conversation_create` arguments that make that
> same conversation instead (for the project's conversation, with no datapack, which Meko takes
> only when the account has a default one, as for AgentK's own create). Resumed and recovered
> launches in the next minute then don't wait at all, so a restart that recovers many sessions one
> by one isn't held up 5 seconds each. A new session still waits: its conversation has to be
> created, and titled, before the Meko plugin's SessionStart hook creates it under a generic title.

> **PRs point back at Meko.** When the project has a dedicated datapack and Meko is on, Open PR
> appends to the PR prompt (after any repo or owner override) an instruction to end the PR
> description with these lines: `<Meko host>://<datapack id>;<datapack name>`, the design's
> `meko_design:<name>;<hash>` when the task has a design in Meko (see
> [The design after approval](lifecycle.md#the-design-after-approval)), and `meko_conversation_id:<id>`. The name is for people reading the PR; AgentK reads only the id
> (`datapackIdsIn()`, which also takes the line without a name, as PRs opened before it was added end).
> The host is the web app's that Meko names (`cloud.mekodata.ai` while it's linked to Meko Cloud),
> else the Meko on this computer's (`mekoHost()` in `config.ts`). The conversation id comes from `conversation_create` keyed by the session's
> Claude session id, the same key the plugin's hooks use, so it names the conversation that holds
> the session (created and titled after the session if it doesn't exist yet, even with capture
> off). If Meko fails or doesn't answer within 10 seconds (connecting included), the PR goes out
> with just the datapack line. Open PR checks again that Claude is idle after that wait, before it
> types the prompt.

> **Folder trust.** Claude Code asks "Do you trust this folder?" the first time it runs somewhere
> new, and fires no hooks until that's answered. For a worktree, Claude keys that decision by the
> main repository, so the server marks the project's repo as trusted in `~/.claude.json` when the
> project is created, and checks again before each launch (other Claude processes can overwrite a
> fresh entry when they start, so it re-checks after a moment). If the prompt still appears (for a
> new, resumed or recovered session), the session shows *needs you* with an explanation.

> **Meko: the Meko on this computer.** Datapacks are how a project keeps track of its work, and
> sharing a project goes through its datapack. AgentK uses the Meko running on this computer, and
> nothing else; it can't be turned off. It doesn't know or ask whether that Meko is linked to Meko
> Cloud: Meko owns the link (the Meko Cloud, or other Meko server, its extensions use, and its API
> key, in `cloud.toml` and Meko's secret store; Meko's `docs/cloud.md`), set in Meko's web UI
> (**Settings › Meko Cloud**, `/settings/meko-cloud`) or with `meko cloud_link`. AgentK calls that
> Meko's `/mcp` with its URL and client token, and its REST API at `/api/v1` on the same origin
> (`mekoApiBase` in `mekolocal.ts`, `resolveMekoApiUrl`), for sharing, uploads, Context Gaps and the
> agents' list. That Meko serves a datapack it has and, while it's linked, passes any other id on to
> Meko Cloud. It refuses what it can't do with an error that says what to do, and AgentK shows it:
> sharing while unlinked is a 409 `CLOUD_NOT_LINKED` ("link it in Settings › Meko Cloud"), and
> sharing a datapack still on that Meko is a 409 `DATAPACK_LOCAL`. So Share is offered whenever
> there's a Meko. An upload link may be plain http only on that Meko's own origin (one of its own
> datapacks); any other must be https (`MekoApi.postPresigned`). Datapack names link to that Meko's
> own page for them (`/datapacks/<id>` on the origin that serves AgentK: `mekoDatapackUrl()` in
> `src/web/util.ts`), and the Context Gaps line to `/datapacks/<id>/gaps`; Meko's web UI sends a
> Meko Cloud datapack on to Meko Cloud's web app, and its gaps to its own `/gaps/<id>` page. Share
> links are on the web app Meko names (`cloud.web_url`, Meko Cloud's while it's linked), else Meko
> Cloud's. Meko is *set up* while that Meko runs (`MekoStatus.configured`, with `url` its MCP URL).
> With none, AgentK still works without datapacks: it doesn't talk to Meko, projects get no
> datapack, and sessions get no datapack, pin or Meko lines in their prompt (even a project that
> has a datapack).
>
> AgentK reads it with `meko token --json` and `MEKO_HOME` set to the Meko data folder that holds
> AgentK's home (`<meko>/extensions/agentk`, `mekoDirOf`), else the one `[meko] home` names (a home
> the move hasn't reached yet), else `meko`'s own default (a dev home set with `AK_HOME`)
> (`probeLocalMeko` in `mekolocal.ts`): the Meko's MCP URL and client token, and of its link
> (`cloud`, `MekoCloudLink`) only the account behind it (`account`, '' while unlinked) and the web
> app it names for share links. It asks rather than reading Meko's files: on macOS the secrets are
> in the Keychain, and whether Meko runs is a lock only `meko` checks. The answer is kept in memory
> (`LocalMekoReader`) and never written to `config.toml`, so a new port, token or link is picked up
> without a reinstall. A read that can't ask `meko` (it failed or took too long), or whose link
> Meko couldn't read (`cloud.error`: a bad `cloud.toml`, a locked Keychain), keeps the link last
> read. It's read at server start (before sessions are recovered), when Settings opens (which waits
> up to 1.5 s for it), when Meko says something changed (`POST /_meko/changed` `{what: 'cloud'}` on
> AgentK's socket, which Meko sends after every change to the link and at each start of its
> server), once more when connecting fails (at most every 30 s), and every minute while there's no
> Meko or no `meko` on this computer (Meko normally starts AgentK, so it's up first). A read that
> changes what AgentK shows (whether there's a Meko, its URL, the web app) shows it at once. One
> that changes the Meko it connects to (it started, stopped, has a new port or token, or another
> account behind it) reconnects (`Core.refreshMeko`); the first read at start connects nothing yet.
> On the same account it keeps the invites and shares AgentK knows; on another (Meko linked,
> unlinked, or linked to another account) it forgets them (`Core.mekoChanged`). A spell with no Meko
> at all (the Meko stopped) isn't a change of account: AgentK compares with the last account it used
> (`mekoAccountOf`). Claude Code's own Meko setup, in its settings, is the user's and is changed only
> when they ask (see *Claude Code on AgentK's Meko* below).
>
> - **Keys of AgentK's own, from before:** AgentK no longer reads a Meko key of its own:
>   `[meko] url`, `api_key`, `web_url` and `api_url` in `config.toml` are ignored, though a save keeps
>   them in the file (`oldMekoKeys`: a rollback to a release that read them restores only `state.db`,
>   and one may be the only copy of a key never linked in Meko), and so are `MEKO_API_KEY`, `MEKO_MCP_URL`, `MEKO_WEB_URL`, `MEKO_API_URL` and Claude Code's
>   settings' `env`. It hands nothing to Meko: a key that was never linked is linked in Meko's web UI
>   or with `meko cloud_link`.
> - **At install:** the installer asks for no key, and uses the Meko on this computer, storing
>   nothing of it. `--meko-url` and `--meko-key` are still taken, and unused: `meko extension install
>   agentk` passes them with Meko's own URL and token, and refuses an AgentK whose files don't
>   mention `--meko-url` (`tooOld` in Meko's `internal/cli/install.go`). With no Meko running, it goes
>   on with a warning and the server uses that Meko once it runs. `--no-meko` doesn't turn Meko off
>   (it warns so): it skips Meko's setup for the agents. With a Meko, it checks that Meko is set up
>   for Claude Code: the `meko-agent-skills` plugin installed and enabled, a `MEKO_API_KEY` in Claude
>   Code's settings (sessions don't inherit the installer's environment), and the `meko` MCP server
>   registered, both on this Meko, with tokens it accepts (they may differ from AgentK's; the
>   installer deliberately stops at accepted, and the server checks datapacks once it runs). If
>   anything is missing or wrong it offers, after showing what will change, to run Meko's own setup
>   (`@yugabytedb/meko-mcp --client claude-code`), fetched from npm, checked against the registry's
>   integrity hash and run with the bundled Node, with the token passed in the environment. Declining
>   it (or, without `--install-deps`, not being asked) is a warning with the command to run yourself;
>   Settings › Harness › Claude Code can run it later. That command reads the token in the user's
>   shell, `--api-key "$(meko token)"` (with `MEKO_HOME=<folder>` when AgentK knows Meko's data
>   folder), since nothing else gives the package a key there (`manualMekoSetup`).
> - **At project creation:** the dedicated datapack is an existing one, a new one, or **No
>   datapack** (`DatapackChoice` `{ mode: 'none' }`), the only one while there's no Meko. The
>   Open project dialog has no datapack box: the project name finds it (`adlc/proj/<repo>/<project>`,
>   that datapack if it exists and no project here uses it, else a new one), Project name's dropdown
>   offers your projects with their datapacks, **Search all datapacks** finds any other, and **No
>   datapack** is a checkbox
>   ([Open project](open-project-and-new-session.md#open-project)).
>   When Meko refuses a new datapack because the plan's datapack limit is reached (HTTP 402,
>   `FREE_TIER_DATAPACK_LIMIT` / `STANDARD_TIER_DATAPACK_LIMIT`), the server answers 402 with code
>   `DATAPACK_LIMIT` and the dialog offers **Open without a datapack**, or picking an existing one.
>   One can be added later from the project's home.
> - **At session start:** every session gets `MEKO_AGENT_ID`, and `MEKO_DATAPACK_ID` when the
>   project has a dedicated datapack. A project without one starts sessions like any other; design
>   saves, the learn step and search skip Meko for it, and while there's no Meko, for every project.
> - **If Meko isn't installed:** while there's no `meko` on this computer (`MekoStatus.local` is
>   `none`), a banner that can't be dismissed says so and gives the command that installs it (the
>   latest release's installer, `MEKO_INSTALL_COMMAND` in `src/shared/mekourls.ts`), with **Copy**.
>   AgentK looks for `meko` every minute meanwhile, and the banner goes once it's found, running or
>   not. Once a Meko runs, AgentK stops looking, so `meko` removed later brings the banner back only
>   at the next read (a restart, Settings opening, or Meko's notice of a change).
> - **If Meko goes down:** AgentK doesn't poll Meko or show a banner for it; being down is rare.
>   The one exception is looking for the Meko on this computer every minute while there's no Meko
>   at all or no `meko` (`meko token --json`, a local command, never a Meko call). An
>   action that needs Meko (creating a datapack, sharing, answering an invite) shows
>   Meko's error, the project home shows no datapack counts, and background syncs log a warning and
>   try again later. Running sessions keep going; the Meko hooks capture from the transcript on
>   disk, so they catch up once it's back (when transcript capture is on). A project can still be
>   created with **No datapack** and get one later.
> - **In Settings:** the Meko Cloud link is set on Meko's own page, not AgentK's. AgentK's Settings
>   › General has a **Meko** section that says AgentK uses the Meko on this computer, with
>   **Meko Cloud ↗** (`/settings/meko-cloud`) as a plain link on Meko's origin (`MEKO_CLOUD_PATH`),
>   and **Account ↗** (`/account`, `MEKO_ACCOUNT_PATH`) only when Meko's status names a username,
>   which only a Meko that still has a login does (its password, two-step sign-in and Sign out). **Set up Meko** on a project's home (while there's no Meko) says what's wrong
>   with the Meko on this computer, and opens Meko. Meko's page checks a new server or key before saving it (a MCP
>   handshake with the key, and a typed web app or API URL must answer like Meko's API), sends a
>   saved key only where it already goes or to Meko Cloud (otherwise it must be typed again), asks
>   before switching hosts (existing projects' datapacks stay on the old one), and offers **Unlink**,
>   which sends the extensions back to the local Meko. Each agent's Meko row is in Settings ›
>   Harness, under that agent (`AgentMeko` in `SettingsDialog.tsx`), shown while the agent is on and
>   new enough. It says whether Meko has the agent connected, from Meko's list of agents
>   (`HarnessStatus.meko`: connected, needs reconnecting, connected elsewhere with its URL, not
>   connected, or not found; "Update Meko (meko update) to manage agent connections" from a Meko too
>   old to list them), which new sessions are gated on ([Agents](harnesses.md#connected-to-meko)),
>   with **Connect to Meko** when Meko can connect it. Meko owns the agents' `meko` MCP servers;
>   AgentK checks only what Meko's capture needs. Claude Code's row lists that (the same list as the
>   banner, below, less its MCP server), and while anything's missing **Set up Claude Code with this
>   Meko** has Meko connect it first (the page's own call to Meko's route), then runs Meko's setup
>   package as the installer does (`POST /api/settings/meko/claude-setup`, without blocking the
>   server; its output goes to the log), only while the capture part is still missing. New sessions
>   pick that up; running ones keep what they started with. Kiro's row says whether Kiro's sessions
>   reach this Meko (`MekoStatus.kiro`: its MCP server from Meko's list, then, from `mekokiro.ts`,
>   whether the agent Kiro sessions are built from loads Kiro's MCP config (`includeMcpJson`);
>   capture hooks aren't checked), and **Set up Kiro with this Meko** connects it, then runs the
>   same package with `--client kiro` while that agent doesn't load it. That writes Meko's MCP server,
>   skill, steering and capture hooks for Kiro, and makes its `meko-default` agent Kiro's default,
>   which AgentK's `agentk` agent is then built from, so Kiro sessions get Meko's tools and capture
>   with the session's `MEKO_AGENT_ID` (`kiro:<slug>-<hash>`) and `MEKO_DATAPACK_ID`, as Claude's
>   plugin does; its capture hooks are left out of `agentk` while `capture_transcripts` is off, as
>   Claude's sessions run without the plugin then. AgentK writes no Meko config for Kiro itself.
>   OpenCode's row says which Meko its sessions use (`MekoStatus.opencode`): Meko's setup has no
>   OpenCode client, so AgentK adds this Meko to each OpenCode session's config as a remote `meko`
>   MCP server, from an owner-only file (`run/opencode/meko.json`, `OPENCODE_CONFIG`; the key never
>   goes in the session's environment), unless OpenCode has a `meko` server of its own (any Meko
>   reports for it, else one in its own config), which it then leaves alone ([Agents](harnesses.md#opencode)). The file is removed as soon as this
>   Meko's URL or key changes, and written again at the next launch. OpenCode sessions get
>   `MEKO_AGENT_ID` `opencode:<slug>-<hash>`.
> - **Claude Code on AgentK's Meko:** Claude Code keeps its own copy of the Meko server and token
>   in two places: its user-scope `meko` MCP server, which the tools call and which Meko reports and
>   connects (`HarnessStatus.meko`), and its settings' `env` (`MEKO_MCP_URL`, unset meaning Meko
>   Cloud, and `MEKO_API_KEY`), which the Meko hooks read. `MekoStatus.claude` lists what keeps
>   sessions off AgentK's Meko: first its MCP server as Meko reports it (`mekoMcpGaps` in
>   `mekoagents.ts`: none, needs reconnecting, on another server named by host, or Meko didn't find
>   Claude Code; nothing while nothing is gated), then the capture part (`capture`, from
>   `mekoclaude.ts`): the plugin, a missing key, the settings on another server, and their token,
>   when it isn't AgentK's own, failing on AgentK's server: turned down (HTTP 401 or 403), or unable
>   to see a project datapack that AgentK's own token sees (another account's token). The token is
>   only tried on AgentK's server, and only when the settings name it. A token that couldn't be
>   checked (Meko slow or down) is listed apart and never raises the banner. The file checks are
>   cheap; token results (and AgentK's own datapack list) are reused for 10 minutes. It's refreshed
>   at startup, on a config change, when Settings opens, at each session launch, and after Claude
>   Code's setup (which starts over). While the list isn't empty, a banner says what's wrong, with
>   **Set up Claude Code with this Meko** (Meko connects it, then the setup package runs for what's
>   left) and **Details** (Settings › Harness › Claude Code). AgentK never fixes it on its own: not
>   at startup, and not after a change to Meko's link, which only checks again.

## Adding files to the knowledge base

Files dropped on or pasted into a session's terminal wait in the strip under it, held for Claude
(see the paste and drop part of [The app](app.md)). While at least one of them is a PDF, TXT,
Markdown, JSON, CSV, XML or HTML file, the strip offers **Upload to Meko**, which adds those to the
knowledge base of the project's dedicated datapack (`MekoUpload.tsx`, `uploadToMeko()` in
`terminals.ts`). It isn't offered with Meko off or a project with no dedicated datapack. The types
are `KB_UPLOAD_EXTS`, a copy of `uploadMimeByExt` in Meko's API server, matched by extension in any
case (`isKnowledgeFile()` in `src/shared/knowledge.ts`).

- **Ask first.** **Upload to Meko** asks in the strip before anything leaves: which files go, to
  which datapack, that everyone with access to it can find what's in them, and which held files it
  skips (`mekoAsk()` in `src/web/knowledge.ts`). Nothing is sent before **Upload**.
- **Out of the strip.** Each file Meko takes leaves the strip, so it doesn't also go to Claude; one
  that fails stays held, and a notice sums up what was added, what Meko already had and why any
  failed. While they're sent they're left out of an Enter's paths. The state (`meko`: asking, and
  the ids being sent) lives on the session's terminal, which outlasts its pane, so switching
  sessions mid-upload and back shows it where it is, and a file can't be sent twice.
- **Through the server.** The browser can't call Meko: the key never reaches it. **Upload** posts
  each file in turn to `POST /api/sessions/:id/knowledge?name=<file name>`, the raw bytes as
  `application/octet-stream`. That route's parser exists only in its own Fastify scope, as the image
  routes' does, so no other route takes a non-JSON body, and no HTML form can send that type.
  `KnowledgeService.upload()` (`src/server/knowledge.ts`) checks the session (409 archived), Meko
  (409 off), the project's dedicated datapack (409 none: the one the UI names, so a running session
  after a switch still uploads where the question said), the name (its base name only, with a known
  extension, else 400), and the body (400 empty, 413 over 50 MB, a standard plan's cap in Meko). It
  doesn't look at the bytes: Meko checks them against the extension. Upload to Meko sends what the
  strip holds, and the strip holds files up to 20 MB (the paste limit), so from the UI a file goes
  to Meko at 20 MB at most.
- **Meko's presigned upload.** Meko has no single-call upload (it removed
  `/datapacks/:id/knowledge-bases/upload` in October 2026); files go in through its presigned
  flow, in two steps. `MekoApi.requestUpload()` posts `{filename, size, content_hash, client}` (the
  bytes' SHA-256, `client` `agent-k-local`) to `/datapacks/:id/knowledge-bases/upload-url` with the
  same key as MCP. Meko answers with an S3 POST signed for exactly those bytes (`url`, `fields`,
  valid 5 minutes) and the document's `key`, or `already_uploaded` with the key and no URL when the
  datapack has the same bytes ("already there", nothing more sent). `MekoApi.postPresigned()` then
  posts the signed fields and the file, last, to that URL **without Meko's key**, only over https
  (or http to Meko's own origin, for one of its own datapacks), and waits up to
  2 minutes. Meko records every URL it issues and indexes the file once it lands, so there's no
  call back (Meko's `upload-complete` takes only a signed-in browser). Meko lets only the
  datapack's owner and maintainers upload. Refusals come back in words that say what to do
  (`uploadError()`): a 403 asks the owner for maintainer access; a 401 (or a token without write
  scope) says to run `meko restart`; a 404 without Meko's envelope means a Meko without the route (add
  the file in Meko's web app, or have that Meko updated); a 404 in it, a datapack Meko doesn't have
  (check the project's datapack); Meko's own message for 400 (a type it doesn't take, or a file
  over the plan's per-file cap) and for 402 (no room left on the plan), as a 413; a 503
  `S3_DISABLED`, a Meko whose file storage its admin has to set up; the storage's refusal
  (`STORAGE_<S3 code>`), a 403 there being the link expiring (try again), anything else its
  message plus try again or use Meko's web app; anything else, try again or use Meko's web app. Only 400,
  403, 413 and 415 keep their status; the rest are 502, so the browser never gets a 401, which
  would sign it out. The upload's URL and fields are never logged: they're a credential for the
  file until they expire.
- **Indexing.** For a `processing` upload the server follows
  `/datapacks/:id/knowledge-bases/document-status?document_key=` (its `state`, or from an older
  Meko its `document_status`) every
  5 seconds for a minute, then every 30, and gives up after 15 minutes. Waiting only covers what
  can pass: a 404 in Meko's envelope while it registers the document, Meko unreachable, a 429 or
  a 5xx (logged once per upload). A refused key, a Meko without the route or no key ends it at
  once. At `completed`, at `failed`, `abandoned` (the link expired before the bytes arrived) or
  `oversized`, the give-up or such an error it sends a `knowledge` event on
  `/ws/events`, and a page showing that session's terminal says so in a notice ("notes.md is in
  the knowledge base"). With Meko turned off it makes no more calls, and a change of Meko's URL or
  key (`mekoChanged()`) drops the list, saying nothing, since the documents may not be on the new
  Meko; an upload still on its way then isn't followed either (a generation counter). The list is in memory only: a restart drops it too, and the files are in Meko all the same.
