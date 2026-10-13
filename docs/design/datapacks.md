# Datapacks

Part of the [AgentK Local design](../design.md).

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
> The host is the Meko web app's (`cloud.mekodata.ai` for Meko Cloud), or the MCP server's when a
> self-hosted Meko's web app isn't set (`mekoHost()` in `config.ts`). The conversation id comes from `conversation_create` keyed by the session's
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

> **Meko is optional.** Datapacks are how a project keeps track of its work, and sharing a project
> goes through its datapack, but AgentK works without them. Meko is *on* when there's a key and
> `[meko] enabled` isn't false (`MekoStatus.configured`). Off, AgentK doesn't talk to Meko: projects
> get no datapack, and sessions get no datapack, pin or Meko lines in
> their prompt (even a project that has a datapack). Claude Code's own Meko setup, in its settings,
> is the user's and is changed only when they ask (see *Claude Code on AgentK's Meko* below).
>
> - **At install:** the installer reuses `MEKO_API_KEY` from `~/.claude/settings.json` if one is
>   there, and otherwise asks for one, with **Enter to skip** (pointing to the Meko web app to
>   create an account or key). With no key, or `--no-meko` (which also turns Meko off), it goes on
>   with a warning, non-interactively too. A key it's given must work: without anyone to ask it
>   stops, and interactively it asks whether to go on without Meko (and turns it off, so a bad key
>   from the environment isn't used). With a key, it checks that Meko is set up for Claude Code:
>   the `meko-agent-skills` plugin installed and enabled, a `MEKO_API_KEY` in Claude Code's
>   settings (sessions don't inherit the installer's environment), and the `meko` MCP server
>   registered, both on this Meko server, with tokens it accepts (they may differ from AgentK's
>   key; the installer deliberately stops at accepted, and the server checks datapacks once it
>   runs). A key it took from its own environment (`MEKO_API_KEY`, not typed) isn't stored in
>   `config.toml`, so a different key in Claude Code's settings counts too: the service, with no
>   `MEKO_*` environment, would switch to it. If anything is missing or wrong it
>   offers, after showing what will change, to run Meko's own setup (`@yugabytedb/meko-mcp
>   --client claude-code`), fetched from npm, checked against the registry's integrity hash and run
>   with the bundled Node, with the key passed in the environment. Declining it (or, without
>   `--install-deps`, not being asked) is a warning with the command to run yourself; the Meko tab
>   in Settings can run it later. Once Claude Code's settings hold the key, `config.toml` keeps
>   none, so an older key there can't shadow it.
> - **At project creation:** the dedicated datapack is an existing one, a new one, or **No
>   datapack** (`DatapackChoice` `{ mode: 'none' }`), the default while Meko is off.
>   When Meko refuses a new datapack because the plan's datapack limit is reached (HTTP 402,
>   `FREE_TIER_DATAPACK_LIMIT` / `STANDARD_TIER_DATAPACK_LIMIT`), the server answers 402 with code
>   `DATAPACK_LIMIT` and the dialog offers **Open without a datapack**. One can be added later
>   from the project's home.
> - **At session start:** every session gets `MEKO_AGENT_ID`, and `MEKO_DATAPACK_ID` when the
>   project has a dedicated datapack. A project without one starts sessions like any other; design
>   saves, the learn step and search skip Meko for it, and while Meko is off, for every project.
> - **If Meko goes down:** AgentK doesn't poll Meko or show a banner for it; being down is rare. An
>   action that needs Meko (creating a datapack, sharing, answering an invite, adding a key) shows
>   Meko's error, the project home shows no datapack counts, and background syncs log a warning and
>   try again later. Running sessions keep going; the Meko hooks capture from the transcript on
>   disk, so they catch up once it's back (when transcript capture is on). A project can still be
>   created with **No datapack** and get one later.
> - **In Settings:** the **Meko** tab turns Meko off (`[meko] enabled = false`; the key is kept, so
>   a key from the environment or Claude Code's settings is ignored too) and back on (checked with
>   Meko first; a new key turns it on as well). It sets the MCP server URL (empty means Meko Cloud, unless the
>   environment or Claude Code's settings name one), the API key, and the web app and API URLs
>   (worked out for Meko Cloud; for any other Meko, datapack links and sharing stay off until the
>   web app URL is set, and the tab and the server log say so). The
>   key never goes to the browser: the tab shows its last four characters and where it came from.
>   With no key from anywhere, the tab offers **Sign up ↗** and **Log in ↗** in the Meko web app
>   (`/signup` and `/signin` with `return_to=/settings`, so they land on Meko's Settings, where
>   keys are made under Security), with the steps and the Settings URL written out (with Copy) for
>   a browser on another device. The links follow the MCP and web app URLs as typed, before
>   they're saved, worked out the way the server will (`mekoWebUrl()` in `src/shared/mekourls.ts`,
>   used by both): an empty field falls back to the environment or Claude Code's settings (the
>   settings carry `fallbackUrl` and `envWebUrl` for that), and Meko Cloud's MCP URL gives its web
>   app. For any other Meko they wait for the web app URL. **Add a Meko API key** on a project's
>   home shows the same links.
>   A new URL or key is checked with a Meko ping before anything is saved; then the MCP connection
>   is dropped (one still being opened with the old ones is dropped once it's up) so the next call
>   reconnects, and the invites waiting in Meko are synced again. An API URL that comes from a typed
>   API or web app URL is checked too, without the key (it must answer with Meko's JSON envelope,
>   `{ data }` or `{ error: { code, message } }`),
>   so a mistyped host never gets the key; a changed API URL also syncs invites again. A save
>   writes only the fields it changed, so a change that landed while its checks ran (Claude Code's
>   setup clearing the key) isn't undone.
>   Switching to another Meko host asks you to confirm, because existing projects' datapacks stay
>   on the old one. The tab also says whether Claude Code's sessions reach this Meko (the same
>   list as the banner, below), and when they don't, **Set up Claude Code with this Meko** runs Meko's
>   setup package as the installer does (without blocking the server; its output goes to the log).
>   New sessions pick that up; running ones keep what they started with.
> - **Claude Code on AgentK's Meko:** Claude Code keeps its own copy of the Meko server and token
>   in two places: its settings' `env` (`MEKO_MCP_URL`, unset meaning Meko Cloud, and
>   `MEKO_API_KEY`), which the Meko hooks read, and the user-scope `meko` MCP server in
>   `~/.claude.json` (its URL and `Authorization: Bearer` header, `${VAR}` expanded with the
>   settings' `env` over AgentK's environment, as a session sees it), which the tools call. A
>   variable set in neither (a session may get it from its login shell) leaves that token
>   unchecked, not missing. Local- and project-scope registrations aren't checked. `MekoStatus.claude`
>   (`mekoclaude.ts`) lists what keeps sessions off AgentK's Meko: the plugin, a missing key or
>   token, either place on another server (named by host only), and each token that isn't AgentK's
>   own failing on AgentK's server: turned down (HTTP 401 or 403), or unable to see a project
>   datapack that AgentK's own token sees (another account's token). Tokens are only tried on
>   AgentK's server, and only where Claude Code is set up for it. A token that couldn't be checked
>   (Meko slow or down) is listed apart and never raises the banner. The file checks are cheap;
>   token results (and AgentK's own datapack list, and `<[claude] command> mcp get meko` when
>   `~/.claude.json` has no user-scope server: exit 1 means none, and a command that couldn't be
>   asked is unchecked) are reused for 10 minutes. It's refreshed at startup, on a config
>   change, when Settings opens, at each session launch, and after Claude Code's setup (which
>   starts over). While the list isn't empty, a banner says what's wrong, with **Set up Claude Code
>   with this Meko** and **Details** (the Meko tab). AgentK never fixes it on its own: not at
>   startup, and not after a Meko tab change, which only checks again.
