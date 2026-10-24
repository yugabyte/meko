# Agents: Claude Code, OpenCode and Kiro

Part of the [AgentK design](../design.md).

A session runs on one coding agent, its **harness**, picked when it starts and kept for its life (a
conversation can't move between CLIs): Claude Code, [OpenCode](https://opencode.ai) or [Kiro
CLI](https://kiro.dev/cli/), listed in that order everywhere (`HARNESS_IDS`). The worktree, stages
and gates, the PR flow, Meko recall and learn, the browser terminal, recovery and Resume work the
same on each. Codex and Cursor CLI are left for later; the interface has room for them.

## Picking one

`[agents] default` (Settings → Harness, **Default agent**) is where new sessions and
[Ask AgentK](palette.md#ask-agentk) start. The New session dialog shows **Agent** under
**Advanced** once more than one harness is available, and a failed start's **Try again** keeps the
one it asked for (`CreateSessionRequest.harness`). A harness is *available* when it's turned on
(`[claude] enabled`, `[opencode] enabled`, `[kiro] enabled`: **Offer Claude Code / OpenCode / Kiro
for new sessions** in Settings) and its `--version` answers with its minimum version or later
(`AppState.harnesses`; Claude Code: any version, OpenCode: 1.19.4, Kiro: 2.32.0). Each one's command
is set only in `config.toml` (`[claude] command`, `[opencode] command`, `[kiro] command`); OpenCode's
default, `opencode`, is also found in `~/.opencode/bin` when it isn't on the PATH (`openCodeCommand`),
since OpenCode's installer puts it there and adds that only to a shell's rc file, which a service
doesn't read. A create that names one that isn't is a 400 that says what to do; the dialog always names
the agent it shows (none while no agent has answered yet, just after a start: the server then picks).
A default that isn't available never decides: a request that names no harness, and Ask AgentK, start
on the first available one in `HARNESS_IDS` order (`Harnesses.defaultFor`). Settings' **Default
agent**, like New session's **Agent**, offers only available agents and shows only while two or more
are: with one, there's nothing to choose (`defaultAgentChoice()` in `util.ts`). A stored default that
goes unavailable some other way (not installed, too old, or turned off in config.toml) stays stored,
and decides again once it's available; turning **Offer …** off in Settings is the
exception, which moves the default to another available agent. Both choices are `AgentPicker` in
`common.tsx`, each agent's logo then its name. Before deciding, `pick` reads versions again (see
[Reading versions](#reading-versions) below). The session's harness is `sessions.harness` (`Session.harness`).

**Settings › Harness** holds every agent setting. At its top are **Default agent** and the logo
setting (below); under them a bar lists every harness in `HARNESS_IDS` order, each with its logo
and name (`HarnessBar`, a tab list), off or not installed ones too, since that's where an agent is
turned on. It starts on the default agent, and the agent picked there shows its own settings under
it (`AgentSettings` in `SettingsDialog.tsx`): **Offer … for new sessions** with its version or why it
isn't offered, **Check again**, **Update Claude Code**, **Update OpenCode** or **Update Kiro** while it's available
(`POST /api/claude/update`, whose answer carries the outcome, and `POST /api/harnesses/<id>/update`,
which runs `opencode upgrade` or `kiro-cli update`; each agent's update runs on its own, so one
running doesn't hold up another's button), and for
Kiro **Let Kiro run any tool without asking** while Kiro is on. Each of the tab's fields (`defaultHarness`,
`claudeEnabled`, `opencodeEnabled`, `kiroEnabled`, `kiroTrustTools`) saves as it's changed; there's no Save.
The no-agent banner's **Settings** opens this tab. The Meko tab keeps each agent's Meko row.

**No agent is required.** AgentK installs and runs with any one of them, or more; nothing assumes
`claude` is there. At start the server writes AgentK's own hook files (`writeHookFiles`: the
`--settings` file and the scripts Kiro's `agentk` agent runs too), then prepares every harness in the
background (Kiro's writes its guard script, OpenCode's AgentK's plugin), so no CLI holds up the start. Every script is written to
a temp file and renamed over the old one (`writeScript` in `scriptfile.ts`): sessions keep starting
the scripts through a restart, and Linux refuses to write a script in place while it's being exec'd
(`ETXTBSY`), which stopped the server's start, or Kiro's preparation. `HarnessStatus.checked` says an
agent's version has been read for the command it's run with since it was last turned on (turning it
off forgets the read), so the UI tells "not installed" from "not asked yet". Once every
agent that's turned on has answered and none is available, a create (and Ask AgentK) is a 409 that
says to install Claude Code, OpenCode or Kiro CLI, a banner says the same with a **Settings** button, the New
session dialog shows it in place of starting, and the palette leaves Ask AgentK out
(`AppState.askAgentk`). Claude Code's Meko check (`MekoClaudeWatch`) runs only while Claude Code is
available, and the Meko tab shows Claude Code's row only then. Sessions already on an agent that's
gone keep their row: a relaunch exits at once and shows as exited, like a crash. Turning an agent off
only stops offering it for new sessions: Claude Code sessions still relaunch, resume and recover with
AgentK's mod, whose version check (`supportsMod`) reads Claude Code's version even while it's off.

The installer needs one agent: `install.sh` takes Claude Code, OpenCode (on the PATH or in
`~/.opencode/bin`) or `kiro-cli`, and with none asks which to install (Claude Code first, OpenCode
second, Kiro CLI third; `--agent claude|opencode|kiro` for unattended runs, and `--install-deps` takes
Claude Code without asking unless `--agent` says otherwise), with Anthropic's installer, OpenCode's
(`opencode.ai/install`, into `~/.opencode/bin`) or Kiro's (`cli.kiro.dev/install`, which needs
`unzip` on Linux), and stops when that's declined or fails. It checks each agent's sign-in (`claude
auth status`, `opencode auth list`: no credentials and no provider key in the environment, `kiro-cli
whoami`) and offers `claude auth login`, `opencode auth login` or `kiro-cli login`. `agentk install` then reads each
agent's version (`settleAgents`): with none it stops, saying what to install; a default that isn't
installed becomes the first agent that is; and Meko's setup is offered for each agent found (Claude
Code's plugin and MCP server, Kiro's `--client kiro`; OpenCode needs none, see
[OpenCode](#opencode)), asking first, each a warning when declined.

### Reading versions

The CLIs update themselves (OpenCode at its start), but nothing polls them. An agent's `--version` is read at
startup, then again once the last read is over 10 minutes old when a page opens (`/ws/events`) or a
session is created (`Harnesses.refresh`, `pick`). It's read right away when a create needs an agent
that's turned on but not available (not read yet, its read failed, or too old: it may have been
installed or updated since), when an agent is turned on or updated, and on **Check again** under an
agent's **Offer … for new sessions** in Settings → Harness (shown while that agent is on; `POST
/api/harnesses/refresh`). While an agent is off its version isn't read (apart from the mod's
`supportsMod` check above); turning it on reads it. A version that changed is pushed to every page.

**Telling them apart.** Each session's tooltip names its agent. While the open sessions across all
projects run on more than one harness (`harnessLogos()` in `src/web/util.ts`; archived sessions
don't count), every session tab and every session row in the rail shows its agent's logo between
its dot and its name (`HarnessLogo` in `common.tsx`, all in one 18 × 14 px box so names line up);
while they all run on one, nothing is added. Unticking **Show each session's agent logo beside its
name** in Settings › Harness (offered while a second agent is turned on or open sessions still run
on two; on by default, kept in this browser, `ak-harness-logos`) turns them off there; the agent
pickers keep theirs. The logos are files in `src/web/public/`, never
loaded from elsewhere: `harness-claude.svg` is Clawd, Claude Code's mascot, redrawn cell for cell
from `png/clawd.png` in [dashboard-icons](https://github.com/homarr-labs/dashboard-icons)
(Apache-2.0), `harness-opencode.svg` is OpenCode's own favicon (`favicon-v3.svg` in its MIT repo, on
its dark tile), and `harness-kiro.svg` is Kiro's own `https://kiro.dev/icon.svg`. All are marks on
their own color that read in either theme.

## The adapter

Everything specific to one CLI sits behind one interface, `Harness` in `src/server/harness/types.ts`,
with an adapter per CLI (`claude.ts`, `opencode.ts`, `kiro.ts`) and the registry (`Harnesses` in `index.ts`). The
session manager, projects and Ask AgentK call a session's adapter rather than ask which harness it
runs on. The adapter says how to prepare its files and settle what a launch needs first
(`prepare`, `beforeLaunch`: Claude Code's folder trust, Kiro's agent file), which folders to trust
and what its trust dialog looks like (`trustFolder`, `trustPrompt`, Claude Code only), its command line
(`launcher`, which also takes `LaunchInput.tools`: launch with [AgentK's session tools](tools.md)), its hooks (`normalize`, `deny`, `context`, `editedPaths`), its conversation on disk
(`canResume`, and `findConversation` for a CLI that picks its own id), the files a session's
progress shows in (`activityFiles`, for the hookless fallback), its usage (`readUsage`), the
transcript AgentK sends to Meko (`captureTranscript`, Claude Code only), AgentK's mod (`modDir`,
`offersMod`, Claude Code only), what it keeps for a session that can't run again (`forget`, called
when its project is deleted) and drops when Meko's URL or key changes (`mekoChanged`, called by
`Core.updateConfig`; both OpenCode only), Ask AgentK (`ask`), and its version and updater. Reading a CLI's
version and comparing it is shared (`version.ts`), and so is the cached read of a CLI configured by
`[<id>] command, enabled` with its updater flag (`cliState`, OpenCode's and Kiro's). What each can do is data, `HARNESS_CAPS` in
`src/shared/harnesses.ts`, which the server and the UI both gate on, its minimum version included
(`minVersion`, which the installer checks too), and the Starting view's line (`loads`).

What still names an agent outside the adapters, on purpose: Kiro's plan reader (`Core.kiroPlan`,
read after Kiro's `prepare` and when a Kiro session is created; its plan is Kiro-only data in
`AppState.kiroPlan`), the usage report's per-day `kiro` counts (`callhome.ts`, a field of the
report's schema), each agent's Meko status (`Core.kiroMeko`, `Core.openCodeMeko`: `MekoStatus.kiro`,
`.opencode`), the usage report's agent versions (`installFacts`), and the command a harness runs as
(`harnessCommand` in `version.ts`, for the installer's version reads: OpenCode's default is also
looked for in `~/.opencode/bin`). Adding a harness is an id in `HARNESS_IDS`, its caps, its `[<id>]` config
section and an adapter. The compiler names each `Record<HarnessId, …>` that needs an entry (its
logo, its Meko agent id prefix, the Settings dialog's switch), but not these, which also need one:
the Settings route's `<id>Enabled` field (`Settings` in `types.ts`, checked and applied in
`server.ts`), the installer's "no coding agent found" message
(`settleAgents` in `install.ts`), which names each agent and where to get it, and `install.sh`'s
agent choice and sign-in check:

| | Claude Code | OpenCode | Kiro |
|---|---|---|---|
| Launch | `claude --settings … --session-id/--resume …` | `opencode [--session=<id>] [--prompt=<prompt>]` with `OPENCODE_CONFIG_CONTENT` | `kiro-cli chat --agent agentk [--trust-tools=*] [--resume-id <id>] [prompt]` |
| Hooks | `run/hooks.json` via `--settings` | AgentK's plugin, `run/opencode/agentk-plugin.js` | the `agentk` agent's inline hooks |
| System prompt | `--append-system-prompt` | an `instructions` file written at each launch | printed by the context hook at every `agentSpawn` |
| Design guard | JSON deny | the plugin throws the reason | `kiro-guard.sh`: exit 2, the reason on stderr |
| Conversation id | chosen (`--session-id`) | OpenCode's, from the plugin's events | Kiro's, from its hooks' `session_id`, else its session store |
| AgentK's mod, Artifact designs | yes | no | no |
| [Session tools](tools.md) and their skill | `--mcp-config run/tools/mcp.json`, the skill plugin `run/tools/claude` via `--plugin-dir`, `list` in `permissions.allow` | `mcp.agentk_sessions`, `skills.paths` and an `ask` rule for each write tool in `OPENCODE_CONFIG_CONTENT` | the `agentk` agent's `mcpServers.agentk_sessions`, the skill as a `skill://` resource (Kiro asks before each tool, `list` too) |
| Plan limits in the rail | 5-hour and weekly, from the status line and the mod | none (they're the provider's) | credits this period, from `/usage` hourly |
| Needs-you and interrupt signals | `PermissionRequest`, `Notification`, the mod's `/mod/turn` | `permission.asked` (an interrupted turn ends with its `session.idle`) | none |
| Usage | transcript tokens, priced | tokens from the plugin's log, priced | credits from Kiro's session file |
| By-model usage rows | the model id | the agent first: **OpenCode · claude-sonnet-4-5** | the agent first: **Kiro · auto** (`labelModels`) |
| A public repo's PR head | `--setting-sources user` | `OPENCODE_DISABLE_PROJECT_CONFIG=1` | refused while the workspace has config |

The launcher (`launcher.ts`) is shared: the login-shell script, the session's variables after the
profile, and every harness's markers cleared (`SESSION_MARKERS`: Claude Code's, OpenCode's
`OPENCODE` and `OPENCODE_PID`, and `KIRO_SESSION_ID`, `KIRO_CLI_IS_CHILD_PROCESS`,
`KIRO_CLI_ACP_CLIENT_NAME`, `KIRO_HOST_PID`), so an AgentK started inside one never leaks into a
session of another.

**Events.** Claude Code's hook shape is AgentK's event shape. The hook server knows each request's
session from its token, so `onHook` normalizes the payload with the session's adapter
(`normalize()`) before the status machine sees it. Kiro's `agentSpawn`, `userPromptSubmit`,
`preToolUse`, `postToolUse` and `stop` become `SessionStart`, `UserPromptSubmit`, `PreToolUse`,
`PostToolUse` and `Stop`, and Kiro's `session_id` moves to `agent_session_id` (Claude's `session_id`
is AgentK's key for the session's Meko conversation, `claude_session_id`, which stays as it was
for every harness). OpenCode's plugin sends OpenCode's own event names, which `normalizeOpenCode`
maps the same way (see [OpenCode](#opencode)). The guard and context routes answer through the
adapter too (`deny()`, `context()`): JSON for Claude Code, plain text for OpenCode and Kiro
(`GuardDecision` and `HookContext` take either).

## OpenCode

Checked against OpenCode 1.19.4's source (MIT, `anomalyco/opencode`). OpenCode's hooks are plugins:
JavaScript modules that run inside it, each export a function returning its hooks (`event`,
`chat.message`, `tool.execute.before`/`.after`, `experimental.chat.system.transform`, …); a throw in
`tool.execute.before` denies the call, the error shown to the model. OpenCode allows every tool by
default, asking only where the user's `permission` config says `ask`.

**AgentK's plugin, only in AgentK's sessions.** AgentK writes `run/opencode/agentk-plugin.js`
(`src/server/opencode.ts`, renamed into place at start and before every launch, so a running
session keeps the copy it loaded) and nothing in OpenCode's own config. A launch passes
`OPENCODE_CONFIG_CONTENT`, the inline config OpenCode merges last over the user's own, concatenating
its `plugin` and `instructions` lists with theirs: AgentK's plugin, the session's `instructions` file
(`run/opencode/sessions/<session>/system.md`, the system-prompt lines, written at every launch), and
`permission.external_directory` allowing the session's design folder (`<dir>/*`), outside the
worktree, so writing the design never asks. The plugin does nothing without `AK_HOOK_TOKEN`. It
talks to the hook socket with Bun's `fetch(…, { unix })` (Node's `http` with `socketPath` where Bun
isn't there, as in the tests), sends its events one after another in OpenCode's order, and gives up
on each after 3 seconds:

| OpenCode | Sent as | AgentK's event (`normalizeOpenCode`) |
|---|---|---|
| the plugin loading | `agentk.start` (`source`: `startup`, or `resume` with `AK_OPENCODE_SESSION`) | `SessionStart` |
| `chat.message` | `chat.message` | `UserPromptSubmit` |
| `tool.execute.before` on `edit`, `write`, `multiedit`, `apply_patch`, `patch` | the guard route, with the tool's args | the reason thrown (`editedPaths`: `filePath`, or the `*** Add/Update/Delete File:` and `*** Move to:` lines of `patchText`) |
| `tool.execute.after` | `tool.execute.after` | `PostToolUse` |
| `permission.asked` (`permission.updated` before) | `permission.asked` with the permission's name | `PermissionRequest`: **Needs you**, "Permission needed for bash" |
| `permission.replied` | `permission.replied` | `PostToolUse` (working again) |
| `session.idle` | `session.idle` | `Stop` |

OpenCode's session id moves to `agent_session_id`. The session the plugin follows is the one the
user last prompted (`chat.message`). A subagent's (the `task` tool's, a `session.created` with a
`parentID`) sends its events without its id, so it never becomes the session's conversation, and
never its `session.idle`, so it never ends the turn; its tool calls still mark the session working,
its permission asks still show **Needs you** (one waiting on the user does need them), and its edits
still go to the guard. OpenCode fires nothing when
it exits: reconcile finds the dead pane, as for Kiro. An interrupted turn ends with the `session.idle`
that follows it.

**Starting and resuming.** `opencode --prompt=<first prompt>` in the worktree (the `=` form, so a
prompt starting with `-` stays the value); resume and recovery run `--session=<id>`, never
`--continue` (OpenCode's newest in the folder, which can be an earlier session's in a pooled
worktree). OpenCode exits 1 on a session id it no longer has, so a session resumes only while
OpenCode's store still has it (`canResume`, `openCodeStoredSession`: a row in the `session` table of
`$XDG_DATA_HOME/opencode/opencode.db`, or a release channel's `opencode-<channel>.db`, opened read
only); one OpenCode no longer has (deleted with `opencode session delete`, or its data cleared)
starts again with its first prompt. When no store can be read (none there, or a schema AgentK
doesn't know), the session's usage log decides: a response of that id was logged.

**Recall.** AgentK's Meko recall for a new session's first turn comes from the plugin's one
`/context` call at its start (`AK_OPENCODE_SOURCE=startup`; waiting up to 5 seconds), added to the
main session's system prompt through `experimental.chat.system.transform`. If a later OpenCode drops
that hook, sessions start without recall; the workflow lines still come through `instructions`.

**Meko.** Meko's setup has no OpenCode client, so while Meko is on AgentK gives OpenCode sessions its
own Meko as a remote MCP server named `meko` (AgentK's URL, `Authorization: Bearer <key>`, `oauth:
false`), in an owner-only `run/opencode/meko.json` passed with `OPENCODE_CONFIG`: in a file, so the
key never goes in the session's environment, where every command OpenCode runs would see it. When
the user's own OpenCode config (`$XDG_CONFIG_HOME/opencode` or `~/.config/opencode`: `opencode.json`,
`opencode.jsonc`, `config.json`) already has a `meko` server, AgentK leaves it alone and passes no
file. The Meko tab's OpenCode row says which (`MekoStatus.opencode`). The file is written at each
launch, and removed as soon as Meko's URL or key changes or Meko is turned off (`mekoChanged`), so
a key the user removed or rotated never stays on disk. An `OPENCODE_CONFIG` the user sets in their
profile is replaced by AgentK's while AgentK adds its Meko.

**A public repo's PR head.** Such a session runs with `OPENCODE_DISABLE_PROJECT_CONFIG=1`, which
leaves out the worktree's `opencode.json(c)` and `.opencode/` folders (plugins, which would run the
PR's code, agents, commands, MCP servers); AgentK's own config still loads, from the environment.

**Prompts, designs, learn** take the paths Claude Code takes without the mod, as on Kiro: prompts
typed into the terminal as a bracketed paste, then Enter; designs as markdown with
`questions.json`; the learn step as a typed prompt.

**Usage.** On every finished response (`message.updated` with `time.completed`, logged once per id),
the main session's and its subagents', the plugin appends a line to
`run/opencode/sessions/<session>/messages.jsonl` (model, input, output, reasoning, cache read and
write). `readUsage` reads it from the offset in `usage_files` into `usage_messages` (`opencode:<id>`,
a subagent's under its own session id as the agent; reasoning counted as output; no 1-hour cache
writes), priced with the API price table like Claude Code's: an unknown model counts tokens but no
cost. A model can be one the user named under a provider of their own (`HarnessCaps.userModels`), so
the usage report sends an OpenCode model only when the price table knows it, else `other`. The log
is read from that offset (`openSync`/`readSync`), not read again whole. That file is also the session's activity file for the hookless fallback. A session's folder is
removed with its project; an archived session keeps it, so it can still resume.

**Ask AgentK** on OpenCode is `opencode run --format json --agent agentk-ask`, the message on stdin,
in the ask's folder, with no tools, MCP servers or hooks of the user's: `XDG_CONFIG_HOME` points
OpenCode's global config at the ask's own (`run/ask/opencode-config/opencode/opencode.json`,
`openCodeAskEnv`), which defines the `agentk-ask` agent with AgentK's instructions and `permission:
"deny"` and keeps of the user's config only their model and providers (`model`, `small_model`,
`provider`, `enabled_providers`, `disabled_providers`), so their plugins, MCP servers and agents don't
load; `OPENCODE_DISABLE_PROJECT_CONFIG` and `OPENCODE_DISABLE_CLAUDE_CODE` leave out project config and
Claude Code's files. Its sign-ins and sessions are in OpenCode's data folder, which stays. The answer
is the run's `text` events; the run's session is deleted afterwards (`opencode session delete`, with
the same variables), whether or not the answer could be read, and stopping waits for that. Only a
plugin in `~/.opencode/` itself, which OpenCode always reads, would still load.

## Kiro

Checked against kiro-cli 2.32.0. Its default engine runs only the selected agent's inline hooks,
each given the event as JSON on stdin (`hook_event_name`, `cwd`, `session_id`, and `tool_name`,
`tool_input` for tools); a `preToolUse` hook that exits 2 denies the call, with every tool trusted
too; `agentSpawn`'s stdout goes into the model's context.

**The `agentk` agent.** AgentK writes `~/.kiro/agents/agentk.json` (`KIRO_HOME` honoured) built
from the user's default agent (`chat.defaultAgent` in `~/.kiro/settings/cli.json`; Meko's
`meko-default` after its setup), else Kiro's built-in one, which AgentK exports once with `kiro-cli
agent create --from kiro_default` into the run folder (`run/kiro-default.json`). The base keeps its
tools, MCP servers, resources and hooks; AgentK adds its own after them: `hook.sh` on every event it
follows and `context.sh` on `agentSpawn`, `kiro-guard.sh` on `preToolUse` for `fs_write` (Kiro's
`write` tool), `postToolUse` on every tool. Those exit at once without `AK_HOOK_TOKEN`, and the agent
grants nothing its base doesn't, so outside AgentK it behaves as the user's own agent. `[kiro]
trust_tools` is a flag on AgentK's launch instead, `--trust-tools=*` (every tool trusted for that run),
so a chat someone starts as `agentk` themselves still asks; not `--trust-all-tools`, which makes Kiro
ask on screen whether to go on, with **No, exit** first, and the next prompt AgentK types would pick
it. Meko's capture hooks (`kiro-handler.js`) are left out
while `[meko] capture_transcripts` is off. The file is written only when it changes, at server start
(only where `kiro-cli --version` answers: AgentK never makes a `~/.kiro` of its own), before every
launch, and after **Set up Kiro with this Meko**; `uninstall` removes it. Its description says it's
AgentK's, and `readKiroAgent` never takes it as a base. Kiro uses a workspace agent of the same
name (`<worktree>/.kiro/agents/*.json` named `agentk`) over this one, and the agent loads a
workspace's `.kiro/settings/mcp.json`, with no flag to skip either; so a session on a public repo's
PR head ([Code from public repos](security.md#code-from-public-repos)) refuses to start on Kiro while
its worktree has either (`kiroWorkspaceConfig()` in `headtrust.ts`, checked in the launcher).

**Starting and resuming.** The first prompt is a positional argument, never after `--`: with
`--agent`, kiro-cli 2.32 drops a prompt given after `--`; one starting with `-` gets a leading space
instead. The session's system-prompt lines come back from `context.sh` at every spawn (with AgentK's
Meko recall on a new session's first), since Kiro has no flag for them. Kiro picks the conversation
id; AgentK saves it (`agent_session_id`) from the first hook that carries it. With no hook (Kiro died
before `agentSpawn`, say), it takes the newest `~/.kiro/sessions/cli/*.json` in the worktree (either
spelling) that Kiro says started (`created_at`) after the session was created, only while the session
runs and owns the worktree, and keeps it: worktrees are pooled, so Kiro's store holds earlier
sessions' conversations under the same folder. Resume and recovery run `--resume-id <id>`; a session
with no conversation of its own starts again with its first prompt, never `--resume` (Kiro's newest
in the folder, which can be an earlier session's).

**Status.** Kiro fires no hook when it waits for a permission answer, when you interrupt a turn,
or when it exits. A session waiting on a permission prompt shows as working until you answer in the
terminal; an interrupted turn ends with the next turn's `stop`; an exit is found by reconcile's dead
pane, as Claude's crash is. With no hooks at all, the hookless fallback follows the session file.

**Prompts, designs, learn.** The paths Claude Code takes without the mod: prompts are typed into
the terminal (a bracketed paste, Enter 300 ms later, which Kiro's TUI takes multi-line), designs are
markdown in the session's design folder with `questions.json` (the Design prompt only offers
Artifacts "if you have the Artifact tool", and names the `design_questions` tool only to an agent
with the mod, so Kiro is told to write the file; its key design decisions and Meko items go there the same
way), and the learn step is a typed `learn` prompt.

**Usage** is in [Token usage](token-usage.md#kiros-credits); Meko is in
[Datapacks](datapacks.md) (Meko's own setup for Kiro, `--client kiro`); Ask AgentK on Kiro is in
[Ask AgentK](palette.md#ask-agentk).
