# Agents: Claude Code, OpenCode and Kiro

Part of the [Kode design](../design.md).

A session runs on one coding agent, its **harness**, picked when it starts and kept for its life (a
conversation can't move between CLIs): Claude Code, [OpenCode](https://opencode.ai) or [Kiro
CLI](https://kiro.dev/cli/), listed in that order everywhere (`HARNESS_IDS`). The worktree, stages
and gates, the PR flow, Meko recall and learn, the browser terminal, recovery and Resume work the
same on each. Codex and Cursor CLI are left for later; the interface has room for them.

## Picking one

`[agents] default` (Settings → Harness, **Default agent**) is where new sessions and
[Ask Kode](palette.md#ask-kode) start. The New session dialog shows **Agent** under
**Advanced** once more than one harness is available, and a failed start's **Try again** keeps the
one it asked for (`CreateSessionRequest.harness`). A harness is *available* when it's turned on
(`[claude] enabled`, `[opencode] enabled`, `[kiro] enabled`: **Offer Claude Code / OpenCode / Kiro
for new sessions** in Settings), its `--version` answers with its minimum version or later
(`AppState.harnesses`; Claude Code: any version, OpenCode: 2.0.0, Kiro: 2.32.0), and Meko reports it
connected ([Connected to Meko](#connected-to-meko) below). An agent too old says
so in Settings with the command that updates it: OpenCode 1's `opencode upgrade` stays on 1.x, so
for OpenCode that's `opencode upgrade --major` (`HarnessCaps.upgrade`). Each one's command
is set only in `config.toml` (`[claude] command`, `[opencode] command`, `[kiro] command`); OpenCode's
default, `opencode`, is also found in `~/.opencode/bin` when it isn't on the PATH (`openCodeCommand`),
since OpenCode's installer puts it there and adds that only to a shell's rc file, which a service
doesn't read. A create that names one that isn't is a 400 that says what to do (for one only Meko keeps
off: "Kiro isn't connected to Meko: connect it in Settings › Harness, or run meko agent connect kiro");
the dialog always names
the agent it shows (none while no agent has answered yet, just after a start: the server then picks).
A default that isn't available never decides: a request that names no harness, and Ask Kode, start
on the first available one in `HARNESS_IDS` order (`Harnesses.defaultFor`). Settings' **Default
agent**, like New session's **Agent**, offers only available agents and shows only while two or more
are: with one, there's nothing to choose (`defaultAgentChoice()` in `util.ts`). A stored default that
goes unavailable some other way (not installed, too old, or turned off in config.toml) stays stored,
and decides again once it's available; turning **Offer …** off in Settings is the
exception, which moves the default to another available agent. Both choices are `AgentPicker` in
`common.tsx`, each agent's logo then its name. Before deciding, `pick` reads versions again (see
[Reading versions](#reading-versions) below). The session's harness is `sessions.harness` (`Session.harness`).

### Picking a model

New session's **Model**, under **Agent** in **Advanced** (shown with any agent, even one alone),
picks the model the session's agent runs on: **Default** (the agent's own: Claude Code's `model`
setting, OpenCode's config, Kiro's settings), one on the agent's list, or **Other…**, which takes
any name. Claude Code's list is fixed in `HARNESS_CAPS.models`: its aliases Fable, Opus, Sonnet
and Haiku, which follow its newest models. OpenCode's and Kiro's (`models: 'cli'`) depend on the
providers and plan the user has, so they're read from the CLI (`Harness.models`): `opencode models
--standalone` (with `[opencode] disable_tracking`'s variables, so Zen's free models it hides aren't
offered) and `kiro-cli chat --list-models -f json` (Kiro's `ModelListResult`, `parseKiroModels`),
asked only once `kiro-cli whoami` says someone is signed in, since signed out the list starts Kiro's
sign-in and opens a browser. `Harnesses.models` keeps each list 10 minutes (`MODELS_TTL_MS`, read
again for a new version or setting, and after **Check again**; the last list read stays for
`modelsKept` until the new one lands), and `GET /api/harnesses/:id/models`
serves it; a list that can't be read is empty, and the dialog then offers Other alone. Kode has
no default model of its own: Default is the agent's.

The pick is `CreateSessionRequest.model`, kept in `sessions.model` (`Session.model`; NULL for
Default) and passed on every launch of the session (`LaunchInput.model`), its resumes, recoveries
and wakes included: Claude Code resumed without `--model` runs on its default, not the model the
conversation used. A model changed with `/model` in the terminal is lost at the next relaunch. Each
adapter passes it in its CLI's terms: Claude Code's and Kiro's `--model`, and OpenCode's `model` in
the session's `OPENCODE_CONFIG_CONTENT`, since OpenCode 2's TUI has no `--model` (passing `-m`
prints its help and exits). Kode checks only the name's form (`checkModel`: letters, digits and
`._:/@[]-`, not starting with `-`, at most 200 characters; a 400 otherwise); a model the agent
doesn't know fails in its terminal. A task's code review doesn't take the task's model: its steps
run on `[code_review.models.<harness>]`. The [session tools](tools.md)' `start_sessions` takes
`agent` and `model` too.

**Settings › Harness** holds every agent setting. At its top are **Default agent** and the logo
setting (below); under them a bar lists every harness in `HARNESS_IDS` order, each with its logo
and name (`HarnessBar`, a tab list), off or not installed ones too, since that's where an agent is
turned on. It starts on the default agent, and the agent picked there shows its own settings under
it (`AgentSettings` in `SettingsDialog.tsx`): **Offer … for new sessions** with its version or why it
isn't offered, **Check again**, **Update Claude Code**, **Update OpenCode** or **Update Kiro** while it's available
(`POST /api/claude/update`, whose answer carries the outcome, and `POST /api/harnesses/<id>/update`,
which runs `opencode upgrade` or `kiro-cli update`; each agent's update runs on its own, so one
running doesn't hold up another's button), and for
Kiro **Let Kiro run any tool without asking** while Kiro is on, and for OpenCode **Turn off OpenCode's tracking**
while OpenCode is on ([Without tracking](#opencode)). Each of the tab's fields (`defaultHarness`,
`claudeEnabled`, `opencodeEnabled`, `opencodeDisableTracking`, `kiroEnabled`, `kiroTrustTools`) saves as it's
changed; there's no Save.
Under each agent that's on and new enough, a **Meko** row says whether it's connected to Meko (below)
and whether its sessions reach Kode's Meko, with **Connect to Meko** when it isn't connected, or
**Set up Claude Code with this Meko** or **Set up Kiro with this Meko** when what Meko's capture
needs is missing ([Datapacks](datapacks.md)); the Claude Code Meko banner's **Details** opens the tab
on Claude Code. The no-agent banner's **Settings** opens this tab.

### Connected to Meko

Meko owns the agents' connections: it lists the agents on this computer and whether each one's `meko`
MCP server reaches it (`GET /api/v1/agents`, `meko agent list`), and connects or disconnects them
(`meko agent connect <id>`, its web UI's Agents card). Kode's server reads that list
(`MekoAgents` in `src/server/mekoagents.ts`) from the local Meko, at the origin of its
MCP URL, with its client token (`localMeko()`), at startup, when Settings opens, on **Check again**,
after an agent's Meko setup, when the local Meko or its Meko Cloud link changes, before a page's
claim to connect Kiro or OpenCode by itself (below), and before a new
session (or Ask Kode) that the list kept would refuse an agent for its connection, so `meko agent
connect` in a terminal counts at once; nothing polls. Reads run one at a time, and one asked for
while another runs reads again after it, so the read after a Connect never answers with the list
from before it.
Each harness gets `HarnessStatus.meko`: `connected` (its `meko` server is at Meko's target, the
linked Meko Cloud or this Meko, with its key or token), `stale` (at one of Meko's own but not the
target), `elsewhere` (another server), `none`, or `missing` (Meko didn't find it installed). A harness
that's `none`, `stale` or `elsewhere` isn't available (`mekoGates`), so everything that picks one for a new session follows
(`Harnesses.pick` and `defaultFor`: the New session dialog, Ask Kode and work from the queue); a
running or resumed session keeps the agent it started on. NO_AGENT's 409, the no-agent banner and the
New session dialog name the fix for each agent only Meko keeps off (`notConnectedText`), the dialog
at its top when that's the default agent, else under **Agent** in **Advanced**. One that's `missing`
stays available: Meko looks for agents only on its own PATH, in a few folders and on the user's login
shell's PATH, never at Kode's `[<id>] command`, so an agent Kode runs (a command of its own) can
be missing from Meko's list with nothing the user could fix. Its Meko row and its Meko gaps say
"Meko didn't find <Label> on this computer.", with no Connect.

The last list read is kept in `state.db`'s kv (`meko-agents`) and used while Meko doesn't answer
(stopped, slow, or down), so a restart then still gates on it. With no list ever read, or a Meko
too old to list agents (404), nothing is gated (`meko` null, `MekoStatus.agentList` `unread` or
`old`): a Kode update must not block every session on an older or stopped Meko. Settings then
says "Update Meko (meko update) to manage agent connections" for the 404, and offers no Connect.

Kode's server never posts to Meko (its client token can't: connecting may write Meko Cloud's
account-wide key into an agent's config, so it's the owner's). **Connect to Meko** in Settings ›
Harness is the page's own call to Meko's web route, root-absolute on Meko's origin with its access,
as updates are (`connectAgent()` in `src/web/mekoagents.ts`: `POST /api/v1/agents/<id>/connect`, in
Meko's `{ data, error }` envelope), followed by **Check again**'s `POST /api/harnesses/refresh`,
which reads Meko's list again. Meko's notes (a server it replaced) show under the row.

**Connecting Kiro and OpenCode by itself.** The page also connects Kiro and OpenCode
(`HarnessCaps.autoConnect` in `src/shared/harnesses.ts`) without a click, once per agent, ever: the first time
it sees one turned on, new enough and with a `meko` server Meko reports `none` or `stale`
(`autoConnectDue`, which `HarnessStatus.autoConnect` gives the page). It asks the server first
(`POST /api/harnesses/:id/auto-connect`: Meko's list read again, then `go: true` for the first ask
only, recorded in state.db's kv, `meko-auto-connect`, by `MekoAutoConnect` in
`src/server/mekoautoconnect.ts`), then runs Connect to Meko's own steps (`autoConnectToMeko()` in
`src/web/mekoagents.ts`, from an effect in `App.tsx`) and says the result in a toast ("Connected
Kiro to Meko.", or Meko's error and "Connect it in Settings › Harness."). Why this way:
- The page, not the server: Meko's connect route is the owner's, and the server's client token is
  refused (`OWNER_ONLY`). The server running `meko agent connect` would sidestep that.
- Not Claude Code, whose Meko setup is the user's and changes only when they ask; Kiro's and
  OpenCode's `meko` server is one file Meko edits, with no sign-in.
- `none` and `stale` only: a stale server is Meko's own entry, while one `elsewhere` is the user's,
  which a connect would replace.
- Once ever: several open pages connect it once, a user who later runs `meko agent disconnect`
  stays disconnected, and a failed try isn't retried (Connect to Meko is the retry).
- Not in `meko kode install`: one way to do it, and the install opens the page.

Until a page has opened, Ask Kode and the session tools still refuse an unconnected one. Under
`npm run dev` (no `<base>`) the page doesn't try, since Meko's route isn't on its origin.

**No agent is required.** Kode installs and runs with any one of them, or more; nothing assumes
`claude` is there. At start the server writes Kode's own hook files (`writeHookFiles`: the
`--settings` file and the scripts Kiro's `kode` agent runs too), then prepares every harness in the
background (Kiro's writes its guard script, OpenCode's Kode's plugin), so no CLI holds up the start. Every script is written to
a temp file and renamed over the old one (`writeScript` in `scriptfile.ts`): sessions keep starting
the scripts through a restart, and Linux refuses to write a script in place while it's being exec'd
(`ETXTBSY`), which stopped the server's start, or Kiro's preparation. `HarnessStatus.checked` says an
agent's version has been read for the command it's run with since it was last turned on (turning it
off forgets the read), so the UI tells "not installed" from "not asked yet". Once every
agent that's turned on has answered and none is available, a create (and Ask Kode) is a 409 that
says to install Claude Code, OpenCode or Kiro CLI, a banner says the same with a **Settings** button, the New
session dialog shows it in place of starting, and the palette leaves Ask Kode out
(`AppState.askAgentk`). Claude Code's Meko check (`MekoClaudeWatch`) runs only while Claude Code is
on and new enough, connected to Meko or not, and Settings › Harness shows an agent's Meko row only then. Sessions already on an agent that's
gone keep their row: a relaunch exits at once and shows as exited, like a crash. Turning an agent off
only stops offering it for new sessions: Claude Code sessions still relaunch, resume and recover with
Kode's mod, whose version check (`supportsMod`) reads Claude Code's version even while it's off.

The install needs one agent: `scripts/deps.sh`, which `meko kode install` runs first, takes Claude Code, OpenCode (on the PATH or in
`~/.opencode/bin`) or `kiro-cli`, and with none asks which to install (Claude Code first, OpenCode
second, Kiro CLI third; `--agent claude|opencode|kiro` for unattended runs, and `--install-deps` takes
Claude Code without asking unless `--agent` says otherwise), with Anthropic's installer, OpenCode's
(`opencode.ai/v2/install`, OpenCode 2's, into `~/.opencode/bin`; `opencode.ai/install` installs only
OpenCode 1) or Kiro's (`cli.kiro.dev/install`, which needs `unzip` on Linux), and stops when that's
declined or fails. It checks each agent's sign-in (`claude auth status`, `opencode auth list
--standalone`: "No authenticated integrations", neither a stored credential nor a provider key in the
environment, `kiro-cli whoami`) and offers `claude auth login`, `opencode auth login --standalone` or
`kiro-cli login`, each reading the device `/dev/tty` stands for (the script's controlling terminal as
`ps -o tty=` names it, such as `/dev/ttys003`, where its questions are asked), not `/dev/tty` itself: on macOS Bun, which Claude Code and OpenCode run on, stops with "EINVAL: invalid
argument, kqueue" when its input is `/dev/tty`, which kqueue can't watch, and Meko's first install
hands `meko kode install` the terminal as `/dev/tty`. The checks themselves read no terminal
(`claude auth status </dev/null`): with a terminal for input, Claude Code's status check reopens it
to read keys, so with `/dev/tty` it stopped the same way on macOS, and a signed-in Claude Code was
asked to sign in on every first install. `meko kode install` then reads each
agent's version (`settleAgents`): with none it stops, saying what to install (and, for OpenCode,
`opencode upgrade --major` from 1.x); a default that isn't
installed becomes the first agent that is; and Meko's setup is offered for each agent found whose
capture isn't set up (Claude Code's plugin and its settings' URL and key, Kiro's agent loading its MCP
servers, `--client kiro`; OpenCode needs none, see [OpenCode](#opencode)), asking first, each a
warning when declined. The agents' `meko` MCP servers are Meko's (`meko agent connect`).

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
its dark tile), and `harness-kiro.svg` is Kiro's own `https://kiro.dev/icon.svg`. Each is its owner's mark, used to
show which agent a session runs on, on its own color so it reads in either theme.

## The adapter

Everything specific to one CLI sits behind one interface, `Harness` in `src/server/harness/types.ts`,
with an adapter per CLI (`claude.ts`, `opencode.ts`, `kiro.ts`) and the registry (`Harnesses` in `index.ts`). The
session manager, projects and Ask Kode call a session's adapter rather than ask which harness it
runs on. The adapter says how to prepare its files and settle what a launch needs first
(`prepare`, `beforeLaunch`: Claude Code's folder trust, Kiro's agent file), which folders to trust
and what its trust dialog looks like (`trustFolder`, `trustPrompt`, Claude Code only), its command line
(`launcher`, which also takes `LaunchInput.tools`: launch with [Kode's session tools](tools.md), and `LaunchInput.review`: a [code review](code-review.md)'s read-only subagents, one per model tier, as Claude Code's `--agents` and OpenCode's `agent` config; Kiro, whose `HARNESS_CAPS.reviewSteps` is `turns`, runs each step as a turn of its own, after `/clear` once the one before ends), its hooks (`normalize`, `deny`, `context`, `editedPaths`, and `mekoTool` for a CLI without the mod: which events open and close a Meko call for the dot's m), its conversation on disk
(`canResume`, and `findConversation` for a CLI that picks its own id), the files a session's
progress shows in (`activityFiles`, for the hookless fallback), whether a prompt sent between turns
started its turn when its hook never came (`transcriptMark`, `promptStarted`, Claude Code only: see
[Kode's mod](mod.md)), its usage (`readUsage`), the
transcript Kode sends to Meko (`captureTranscript`, Claude Code only), Kode's mod (`modDir`,
`offersMod`, Claude Code only), what it keeps for a session that can't run again (`forget`, called
when its project is deleted) and drops when Meko's URL or key changes (`mekoChanged`, called by
`Core.updateConfig`; both OpenCode only), Ask Kode (`ask`), and its version and updater. Reading a CLI's
version and comparing it is shared (`version.ts`), and so is the cached read of a CLI configured by
`[<id>] command, enabled` with its updater flag (`cliState`, OpenCode's and Kiro's). What each can do is data, `HARNESS_CAPS` in
`src/shared/harnesses.ts`, which the server and the UI both gate on, its minimum version included
(`minVersion`, which the installer checks too), and the Starting view's line (`loads`).

What still names an agent outside the adapters, on purpose: Kiro's plan reader (`Core.kiroPlan`,
read after Kiro's `prepare`, when a Kiro session is created, and — at most every 10 min — when a
Kiro session's turn ends; its plan is Kiro-only data in
`AppState.kiroPlan`), the usage report's per-day `kiro` counts (`callhome.ts`, a field of the
report's schema), each agent's Meko status (`Core.claudeMekoStatus`, `Core.kiroMeko`, `Core.openCodeMeko`:
`MekoStatus.claude`, `.kiro`, `.opencode`), the usage report's agent versions (`installFacts`), the work queue's Slack read (`slack.ts`, and
`claudeAvailable` in `Core`: it reaches the user's claude.ai Slack connector, which only Claude Code
has, so it runs Claude Code whatever the default agent), and the command a harness runs as
(`harnessCommand` in `version.ts`, for the installer's version reads: OpenCode's default is also
looked for in `~/.opencode/bin`). Adding a harness is an id in `HARNESS_IDS`, its caps, its `[<id>]` config
section and an adapter. The compiler names each `Record<HarnessId, …>` that needs an entry (its
logo, its Meko agent id prefix, the Settings dialog's switch), but not these, which also need one:
the Settings route's `<id>Enabled` field (`Settings` in `types.ts`, checked and applied in
`server.ts`), the installer's "no coding agent found" message
(`settleAgents` in `install.ts`), which names each agent and where to get it, and `deps.sh`'s
agent choice and sign-in check:

| | Claude Code | OpenCode | Kiro |
|---|---|---|---|
| Launch | `claude --settings … --session-id/--resume … [--model <model>] …` | `opencode --standalone [--session=<id>] [--prompt=<prompt>]` with `OPENCODE_CONFIG_CONTENT` (the session's `model` there: no `--model` in the TUI), and `OPENCODE_CLI_CONFIG_CONTENT` for session tabs off | `kiro-cli chat --agent agentk\|kode-trusted [--trust-tools=*] [--model <model>] [--resume-id <id>] [prompt]` |
| Hooks | `run/hooks.json` via `--settings` | Kode's plugin, `run/opencode/kode-plugin-<hash>/` | the `kode` agent's inline hooks |
| System prompt | `--append-system-prompt` | an `instructions` file written at each launch | printed by the context hook at every `agentSpawn` |
| Design guard | JSON deny | the plugin throws the reason | `kiro-guard.sh`: exit 2, the reason on stderr |
| Conversation id | chosen (`--session-id`) | OpenCode's, from the plugin's events | Kiro's, from its hooks' `session_id`, else its session store |
| Kode's mod, Artifact designs | yes | no | no |
| [Session tools](tools.md) and their skill | `--mcp-config run/tools/mcp.json`, the skill plugin `run/tools/claude` via `--plugin-dir`, `list` in `permissions.allow` | `mcp.kode_sessions`, `skills.paths` and an `ask` rule for each write tool in `OPENCODE_CONFIG_CONTENT` | the `kode` agent's `mcpServers.kode_sessions`, the skill as a `skill://` resource (Kiro asks before each tool, `list` too) |
| Plan limits in the rail | 5-hour and weekly, from the status line and the mod | none (they're the provider's) | credits this period, from `/usage` hourly and, while Kiro is in use, at most every 10 min on a turn's end |
| Needs-you and interrupt signals | `PermissionRequest`, `Notification`, the mod's `/mod/turn` | `permission.asked` (an interrupted turn ends with its `session.execution.interrupted`) | none |
| Usage | transcript tokens, priced | tokens from the plugin's log, priced | credits from Kiro's session file |
| Meko calls (counts) | from the transcript | the plugin's `meko` lines in its log | Kiro's session log (`<conversation>.jsonl`) |
| The dot's Meko m | the mod | the plugin's `execute.before`/`after` on Meko tools | `preToolUse` on `@<Meko server>`, `postToolUse` |
| The dot's work in the background (`Session.background`) | `Stop`'s `background_tasks` | the plugin's `shell.*` and subagent `session.execution.*` events, with `session.idle` and `kode.background` | none: plain idle (see [Kiro](#kiro)) |
| By-model usage rows | the model id | the agent first: **OpenCode · claude-sonnet-4-5** | the agent first: **Kiro · auto** (`labelModels`) |
| A public repo's PR head | `--setting-sources user` | `OPENCODE_DISABLE_PROJECT_CONFIG=1` | refused while the workspace has config |

The launcher (`launcher.ts`) is shared: the login-shell script, the session's variables after the
profile, and every harness's markers cleared (`SESSION_MARKERS`: Claude Code's, OpenCode's
`OPENCODE` and `OPENCODE_PID`, and `KIRO_SESSION_ID`, `KIRO_CLI_IS_CHILD_PROCESS`,
`KIRO_CLI_ACP_CLIENT_NAME`, `KIRO_HOST_PID`), so a Kode started inside one never leaks into a
session of another.

**Events.** Claude Code's hook shape is Kode's event shape. The hook server knows each request's
session from its token, so `onHook` normalizes the payload with the session's adapter
(`normalize()`) before the status machine sees it. Kiro's `agentSpawn`, `userPromptSubmit`,
`preToolUse`, `postToolUse` and `stop` become `SessionStart`, `UserPromptSubmit`, `PreToolUse`,
`PostToolUse` and `Stop` (Kiro 3.0 sends those Claude Code names itself, which are taken as they
are), and Kiro's `session_id` moves to `agent_session_id` (Claude's `session_id`
is Kode's key for the session's Meko conversation, `claude_session_id`, which stays as it was
for every harness). OpenCode's plugin turns OpenCode 2's hooks and events into OpenCode 1's event
names (`chat.message` for its `prompt` hook, `session.idle` for its `session.execution.*`, and so
on), which `normalizeOpenCode` maps the same way (see [OpenCode](#opencode)). The guard and context routes answer through the
adapter too (`deny()`, `context()`): JSON for Claude Code, plain text for OpenCode and Kiro
(`GuardDecision` and `HookContext` take either).

## OpenCode

OpenCode 2 (2.0.0 or later; checked live against 2.1.4, and against the 2.0.0 to 2.1.4 sources, MIT,
`anomalyco/opencode`). OpenCode 1 isn't supported: its plugins and CLI differ, and `--standalone`
(below) fails its strict flag parser. OpenCode's hooks are plugins: a folder whose `server` (or
`index`) module's default export is `{ id, setup }`, `setup(ctx)` registering hooks (`ctx.session.hook`,
`ctx.tool.hook`) and reading events (`ctx.event.subscribe()`); a throw in the tool `execute.before`
hook fails that call, the error shown to the model. OpenCode allows every tool by default, asking
only where the user's permission config says `ask`.

**A server of its own.** OpenCode 2 runs every TUI and `opencode run` against one shared background
server (`opencode serve --service`, started by the first CLI and kept running), whose environment is
that first CLI's. A session's `OPENCODE_CONFIG_CONTENT`, `OPENCODE_CONFIG` and `AK_*` variables would
never reach it, so the plugin wouldn't load and the session would run unsupervised; and if Kode
started the service, every later OpenCode run of the user's would get that session's variables. So
every OpenCode command Kode runs that needs a server passes `--standalone`, a private server with
the command's own environment: the session's TUI, Ask Kode's `run`, `session delete`, and the
installer's `auth list` and `auth login`. The two that need none don't: `opencode --version` (the
version reads) and `opencode upgrade` (Settings' Update), one-shot commands of the server's own that
neither start nor reach a server.

**Kode's plugin, only in Kode's sessions.** Kode writes its plugin to
`run/opencode/kode-plugin-<hash>/` (`server.js` and a `package.json`; `src/server/opencode.ts`),
named by a hash of both files and written only when that folder isn't there, never over it: OpenCode
watches a loaded plugin's files and runs its setup again when they change, even to the same text,
which would restart the plugin in every running session. A changed plugin gets a new folder; older
ones stay for the sessions still running on them (a few KB each), and the plugin file of Kode
before OpenCode 2 (`agentk-plugin.js`) is removed. Nothing goes in OpenCode's own config. A launch
passes `OPENCODE_CONFIG_CONTENT`, the inline config OpenCode merges last over the user's own,
concatenating its `plugin` and `instructions` lists with theirs (OpenCode 2 still reads these
OpenCode 1 keys, `permission`, `mcp` and `skills` too): Kode's plugin, the session's
`instructions` file (`run/opencode/sessions/<session>/system.md`, the system-prompt lines, written at
every launch), and `permission.external_directory` allowing the session's design folder (`<dir>/*`),
outside the worktree, so writing the design never asks. The plugin does nothing without
`KODE_HOOK_TOKEN`. It talks to the hook socket with Bun's `fetch(…, { unix })` (Node's `http` with
`socketPath` where Bun isn't there, as in the tests), sends its events one after another in
OpenCode's order, and gives up on each after 3 seconds. It sends them under the names
`normalizeOpenCode` maps, which are OpenCode 1's:

| OpenCode | Sent as | Kode's event (`normalizeOpenCode`) |
|---|---|---|
| the plugin's `setup` | `kode.start` (`source`: `startup`, or `resume` with `KODE_OPENCODE_SESSION`) | `SessionStart` |
| the session `prompt` hook | `chat.message` | `UserPromptSubmit` |
| the tool `execute.before` hook on `edit`, `write`, `patch` | the guard route, with the tool's input | the reason thrown (`editedPaths`: `path`, relative to the worktree or absolute, or the `*** Add/Update/Delete File:` and `*** Move to:` lines of `patchText`) |
| the tool `execute.before` hook on a Meko server's tool (`meko…_<tool>`) | `tool.execute.before` with `call_id` | `PreToolUse`: a Meko call opens for the dot's m (`mekoTool()`: `<call_id>:<tool>`) |
| the tool `execute.after` hook | `tool.execute.after` with `call_id` | `PostToolUse` (and a Meko call closes); for a Meko tool, also a `meko` line in `messages.jsonl` with the call and its reply, see [Token usage](token-usage.md#meko-calls) |
| event `permission.asked` | `permission.asked` with its `action` | `PermissionRequest`: **Needs you**, "Permission needed for shell" |
| event `permission.replied` | `permission.replied` | `PostToolUse` (working again) |
| event `session.execution.succeeded`, `.failed` or `.interrupted` (OpenCode 2 doesn't send `session.idle`) | `session.idle`, with `interrupted: true` for `.interrupted`, and `background_tasks`: the session's shells still running and its own subagents mid-turn (not ones a subagent started) | `Stop`; an interrupted one ends the turn as interrupted, as the mod's `/mod/turn` does for Claude Code; `background_tasks` replaces `Session.background` |
| events `shell.created`, `.exited`, `.deleted`; a subagent's `session.execution.started` and its end | `kode.background` with `background_tasks`, when one of those ends between turns | `BackgroundTasks` (Kode's own name): replaces `Session.background` |

A tool called from OpenCode's code mode (its `execute` tool) goes through `execute.before` under its
own name too, so the guard sees those edits. Events come as `{ id, type, data }`; one the stream
hands over again after it reconnects is handled once (by its `id`). OpenCode's session id moves to
`agent_session_id`. The session the plugin follows is the one the user last prompted. A subagent's
(the `subagent` tool's: a session with a `parentID`, from its `session.created` event or, for a
prompt that comes first, `ctx.session.get()`) sends its events without its id, so it never becomes
the session's conversation, and never its turn's end, so it never ends the turn; its tool calls still
mark the session working, its permission asks still show **Needs you** (one waiting on the user does
need them), and its edits still go to the guard. OpenCode fires nothing when it exits: reconcile
finds the dead pane, as for Kiro.

**Work in the background.** OpenCode 2's bash and subagent tools take `background: true`: the call
returns at once, the work runs on as one of OpenCode's jobs, and OpenCode wakes the session with
its result. Every bash command runs in a shell OpenCode announces with `shell.created` (its
`info.metadata.sessionID` the session that ran it, `info.command` the command) and ends with
`shell.exited` and `shell.deleted`, so a shell of the main session still running when its turn
ends is background work; a subagent's session (one whose parent, from `session.created`, is the
main session: not one a subagent started) is running from its `session.execution.started` to its
turn's end. OpenCode 2.1.4 has no event for a job itself. The `shell.created` and
`session.execution.started` events go through the plugin's dedupe like the others: one handed over
again after its work ended would bring that work back for good. The plugin sends the list with each
`session.idle`, and again as `kode.background` whenever one ends between turns, so the dot's ring
goes when the work does even before OpenCode's wake turn ends. A subagent shows as "subagent".

**No session tabs.** OpenCode 2's TUI can keep several of its sessions open as tabs (`tabs.mode` in
its CLI config, `cli.json`: `auto`, the default, is on outside the herdr terminal). In a Kode
pane that's a second conversation the plugin would follow once prompted, and since OpenCode keeps
the open tabs per folder, a reused worktree would bring back an earlier session's. So a launch also
passes `OPENCODE_CLI_CONFIG_CONTENT` (`OPENCODE_SESSION_CLI_CONFIG`), the inline CLI config OpenCode
merges key by key over the user's `cli.json`: `{"tabs":{"mode":"off","enabled":false}}`. `mode` is
the key from 2.0.20 on, `enabled` the one before it (2.0.20 and later read `enabled` only when `mode`
isn't set). OpenCode 2.0.0 to 2.0.5 don't read the variable, so their sessions keep their tabs. The
user's other CLI settings still apply, and their own `opencode` runs and `cli.json` are left as they
are. Turning tabs on from OpenCode's settings inside a session writes the user's `cli.json` but
doesn't win over the variable there.

**Starting and resuming.** `opencode --standalone --prompt=<first prompt>` in the worktree (the `=`
form, so a prompt starting with `-` stays the value); resume and recovery run `--session=<id>`, never
`--continue` (OpenCode's newest in the folder, which can be an earlier session's in a pooled
worktree). OpenCode starts a new, empty session under an id it no longer has, so a session resumes
only while OpenCode's store still has it (`canResume`, `openCodeStoredSession`: a row in the
`session_v2` table of `$XDG_DATA_HOME/opencode/opencode.db`, or a release channel's
`opencode-<channel>.db`, opened read only); one OpenCode no longer has (deleted with `opencode
session delete`, or its data cleared) starts again with its first prompt. When no store can be read
(none there, or a schema Kode doesn't know), the session's usage log decides: a response of that id
was logged. A session still on OpenCode 1 after Kode's update isn't checked for: its relaunch exits
on `--standalone`.

**Recall.** Kode's Meko recall for a new session's first turn comes from the plugin's one
`/context` call at its start (`KODE_OPENCODE_SOURCE=startup`; waiting up to 5 seconds), added to the
main session's system prompt through the session `context` hook, which runs before every model
request. If a later OpenCode drops that hook, sessions start without recall; the workflow lines still
come through `instructions`.

**Meko.** Meko's setup has no OpenCode client, so while Meko is on Kode gives OpenCode sessions its
own Meko as a remote MCP server named `meko` (Kode's URL, `Authorization: Bearer <key>`, `oauth:
false`), in an owner-only `run/opencode/meko.json` passed with `OPENCODE_CONFIG`: in a file, so the
key never goes in the session's environment, where every command OpenCode runs would see it. When
the user's own OpenCode config (`$XDG_CONFIG_HOME/opencode` or `~/.config/opencode`: `opencode.json`,
`opencode.jsonc`, `config.json`) already has a `meko` server, Kode leaves it alone and passes no
file. While Meko reports OpenCode (`HarnessStatus.meko`), any `meko` server it reports counts as OpenCode's
own (`Harnesses.openCodeOwnMeko`), so on a connected OpenCode Kode adds none; the file check is
for when nothing is gated or Meko didn't find it. OpenCode's Meko row in Settings › Harness says which (`MekoStatus.opencode`). The file is written at each
launch, and removed as soon as Meko's URL or key changes (`mekoChanged`), so
a key the user removed or rotated never stays on disk. An `OPENCODE_CONFIG` the user sets in their
profile is replaced by Kode's while Kode adds its Meko.

**A public repo's PR head.** Such a session runs with `OPENCODE_DISABLE_PROJECT_CONFIG=1`, which
leaves out the worktree's `opencode.json(c)` and `.opencode/` folders (plugins, which would run the
PR's code, agents, commands, MCP servers); Kode's own config still loads, from the environment.

**Without tracking.** With **Turn off OpenCode's tracking** (`[opencode] disable_tracking`, off by
default; Settings › Harness › OpenCode), every OpenCode Kode runs (sessions, from their next launch or resume,
and Ask Kode's runs and their session deletes) gets `OPENCODE_NO_TRACKING_ENV` (`opencode.ts`). Only Kode's
runs change: OpenCode's own config isn't touched. Each variable was read from OpenCode 2.1.4's code:

| What OpenCode would send | Switch | Set |
|---|---|---|
| OpenTelemetry logs, metrics and traces, to an `OTEL_EXPORTER_OTLP_*ENDPOINT` in the environment | `OTEL_SDK_DISABLED=true` | yes (the session's own commands inherit it, so a project's OpenTelemetry doesn't export from them either) |
| The update check, the version and channel to `opencode.ai/update/api` at each start (and a self-update with `autoupdate` on) | `OPENCODE_DISABLE_AUTOUPDATE=1` | yes (**Update OpenCode** still works) |
| Prompts to OpenCode Zen's free models, whose providers may train on them | `OPENCODE_DISABLE_FREE_MODELS=1` | yes; Zen's paid models stay |
| Web search queries, to Exa, Parallel, Tavily or Firecrawl | `permission.websearch` | no: OpenCode asks which provider first |
| The model catalog (`models.opencode.ai/api.json`, no user data) | `OPENCODE_DISABLE_MODELS_FETCH` | no: the model list stays current |
| Session sharing | `OPENCODE_DISABLE_SHARE`, `share` | no: OpenCode 2 removed sharing and reads neither |
| Error reports | none | already off in OpenCode 2 (`disableErrorReporting`) |

OpenCode 1's `experimental.openTelemetry` is dropped by OpenCode 2, which exports only through the `OTEL_*`
variables. Prompts and code still go to the model the user picks, so the setting's hint suggests a local model
(Ollama, LM Studio) for code that must not leave the computer. A newer OpenCode may rename these variables:
check them against its code when the minimum version moves.

**Prompts, designs, learn** take the paths Claude Code takes without the mod, as on Kiro: prompts
typed into the terminal as a bracketed paste, then Enter; designs as markdown with
`questions.json`; the learn step as a typed prompt.

**Usage.** On every finished step (`session.step.ended`, with the model from its
`session.step.started`; logged once per step), the main session's and its subagents', the plugin
appends a line to
`run/opencode/sessions/<session>/messages.jsonl` (model, input, output, reasoning, cache read and
write). `readUsage` reads it from the offset in `usage_files` into `usage_messages` (`opencode:<id>`,
a subagent's under its own session id as the agent; reasoning counted as output; no 1-hour cache
writes), priced with the API price table like Claude Code's: an unknown model counts tokens but no
cost. A model can be one the user named under a provider of their own (`HarnessCaps.userModels`), so
the usage report sends an OpenCode model only when the price table knows it, else `other`. The log
is read from that offset (`openSync`/`readSync`), not read again whole. Its `meko` lines, one per
call to a Meko server's tool with the reply, go into `meko_calls` the same way (see
[Token usage](token-usage.md#meko-calls)). That file is also the session's activity file for the hookless fallback. A session's folder is
removed with its project; an archived session keeps it, so it can still resume.

**Ask Kode** on OpenCode is `opencode run --standalone --format json --agent kode-ask`, the message on stdin,
in the ask's folder, with no tools, MCP servers or hooks of the user's: `XDG_CONFIG_HOME` points
OpenCode's global config at the ask's own (`run/ask/opencode-config/opencode/opencode.json`,
`openCodeAskEnv`), which defines the `kode-ask` agent with Kode's instructions and `permission:
"deny"` and keeps of the user's config only their model and providers (`model`, `small_model`,
`provider`, `enabled_providers`, `disabled_providers`), so their plugins, MCP servers and agents don't
load; `OPENCODE_DISABLE_PROJECT_CONFIG` and `OPENCODE_DISABLE_CLAUDE_CODE` leave out project config and
Claude Code's files. Its sign-ins and sessions are in OpenCode's data folder, which stays. The answer
is the run's `text` events (an `error` event's `error.message` is what's wrong); the run's session is
deleted afterwards (`opencode session delete --standalone`, with the same variables), whether or not the answer could be read, and stopping waits for that. Only a
plugin in `~/.opencode/` itself, which OpenCode always reads, would still load.

## Kiro

Checked against kiro-cli 2.32.0. Its default engine runs only the selected agent's inline hooks,
each given the event as JSON on stdin (`hook_event_name`, `cwd`, `session_id`, and `tool_name`,
`tool_input` for tools); a `preToolUse` hook that exits 2 denies the call, with every tool trusted
too; `agentSpawn`'s stdout goes into the model's context. Kiro CLI 3.0 (its `kas` 3.0.0) runs the
same hooks but names their events as Claude Code does (`SessionStart` for `agentSpawn`, and so on).

**The `kode` agent.** Kode writes `~/.kiro/agents/kode.json` (`KIRO_HOME` honoured) built
from the user's default agent (`chat.defaultAgent` in `~/.kiro/settings/cli.json`; Meko's
`meko-default` after its setup), else Kiro's built-in one, which Kode exports once with `kiro-cli
agent create --from kiro_default` into the run folder (`run/kiro-default.json`). That export copies
the servers in Kiro's MCP config (`settings/mcp.json`) into the agent's `mcpServers`, headers and
all, as they were then; the built-in agent has none of its own. A server in an agent's `mcpServers`
wins over the MCP config's of the same name, even with `includeMcpJson`, so the copy would keep a
Meko token Meko has since replaced, and every Kiro session would get 401 from Meko (#467; Kiro then
tries OAuth, and its log says Dynamic Client Registration was rejected). So Kode reads the export
without its `mcpServers` while it has `includeMcpJson: true` (`builtinKiroAgent`), and Kiro reads
the MCP config at each start. The base keeps its
tools, MCP servers, resources and hooks; Kode adds its own after them: `hook.sh` on every event it
follows and `context.sh` on `agentSpawn`, `kiro-guard.sh` on `preToolUse` for `fs_write` (Kiro's
`write` tool), `hook.sh` on `preToolUse` for each Meko server (`@<name>`, Kiro's matcher for an MCP
server's tools, `@meko/<tool>`; the servers named `meko` or starting with it in Kiro's MCP config and
the base agent, `kiroMekoServerNames()`, else `@meko`: checked on kiro-cli 2.34.0 not to fire for
other tools), and `postToolUse` on every tool. A Meko tool's `PreToolUse` and `PostToolUse` open and
close a call for the dot's m (`mekoTool()`: the tool's name, since Kiro gives a call no id). Those
exit at once without `KODE_HOOK_TOKEN`, and the agent
grants nothing its base doesn't, so outside Kode it behaves as the user's own agent. `[kiro]
trust_tools` is a flag on Kode's launch instead, `--trust-tools=*` (every tool trusted for that run),
so a chat someone starts as `kode` themselves still asks; not `--trust-all-tools`, which makes Kiro
ask on screen whether to go on, with **No, exit** first, and the next prompt Kode types would pick
it (skipped only by a Kiro-wide setting, `chat.disableTrustAllConfirmation`, which Kode doesn't
change). Kiro 3.0's engine ignores `--trust-tools` whatever its value, and has no flag that applies
a permission rule. So on Kiro 3.0 or later (by the version read before each launch,
`kiroTrustByAgent`) a launch with `trust_tools` runs as a second agent, `kode-trusted`: `kode`
with Kiro 3.0's rule that allows every tool, `{"capability": "all", "effect": "allow"}`, after the
base's own `permissions` rules. Kiro settles a call by a matching deny, then an ask, before an allow,
so the user's own deny and ask rules, and Kiro's own (`.kiroignore`), still hold. The flag stays on
that launch too, for a Kiro 3.0 that runs its older engine (`chat.agentEngine`). Kode writes
`kode-trusted.json` only while `trust_tools` is on with such a Kiro, and removes it (when it's still
Kode's) once that's no longer so: as soon as `trust_tools` is turned off in Settings (which runs a
harness's `prepare` whenever its settings change), and at the next launch or server start for a Kiro
read as older than 3.0. A session still running as it then falls back to Kiro's default agent and
asks again, without Kode's hooks until it's restarted. The version used is the last one any read
gave (`kiroTrustByAgent`), read for a launch even while Kiro is turned off for new sessions (its
sessions still resume); with none yet, or after reads that failed, the file is left as it is and a
launch runs as it while it's there, so a failed `kiro-cli --version` never knocks running sessions off
it. Putting the
rule in `kode.json` instead would let a chat started as `kode` outside Kode run everything,
and every toggle would rewrite the file running sessions use. Kiro 3.0.0 leaves a file that already
has `permissions` as it is ("out of sync" to `kiro-cli agent upgrade`), so `kode-trusted.json`
keeps Kode's format; should a later Kiro upgrade it and drop the rule, `writeKiroAgent` writes it
again (`runsAsWritten` also compares whether every tool is allowed). Meko's capture hooks (`kiro-handler.js`) are left out
while `[meko] capture_transcripts` is off. The file is written only when it changes. Kiro 3.0
upgrades it in place to its own format (`kiro-cli agent upgrade`: its hooks become a list of named
hooks with `fs_write` widened to `write`, and it adds `permissions` and `kiroUpgrade`), and writing
Kode's over that again would make every running Kiro session reload it and fall back to Kiro's
default agent, without Kode's hooks or session tools. So a file that differs is left alone while
Kode would still write what it last wrote (a copy kept in the run folder, `run/kiro-kode.json`)
and the file is still Kode's agent with the same hooks (trigger, matcher, command) and MCP
servers; another Kode home's agent (other script paths, such as `npm run dev`'s) or an older
release's (other hooks) is written over. It's written at server start (only where `kiro-cli --version` answers: Kode never makes a `~/.kiro` of
its own), before every launch, and after **Set up Kiro with this Meko**; `uninstall` removes it and `kode-trusted`. Its description says it's
Kode's, and `readKiroAgent` never takes it as a base. Kiro uses a workspace agent of the same
name (`<worktree>/.kiro/agents/*.json` named `kode`, or `kode-trusted` for a trusted launch) over this one, and the agent loads a
workspace's `.kiro/settings/mcp.json`, with no flag to skip either; so a session on a public repo's
PR head ([Code from public repos](security.md#code-from-public-repos)) refuses to start on Kiro while
its worktree has either (`kiroWorkspaceConfig()` in `headtrust.ts`, checked in the launcher).

**Starting and resuming.** The first prompt is a positional argument, never after `--`: with
`--agent`, kiro-cli 2.32 drops a prompt given after `--`; one starting with `-` gets a leading space
instead. The session's system-prompt lines come back from `context.sh` at every spawn (with Kode's
Meko recall on a new session's first), since Kiro has no flag for them. Kiro picks the conversation
id; Kode saves it (`agent_session_id`) from the first hook that carries it. With no hook (Kiro died
before `agentSpawn`, say), it takes the newest `~/.kiro/sessions/cli/*.json` in the worktree (either
spelling) that Kiro says started (`created_at`) after the session was created, only while the session
runs and owns the worktree, and keeps it: worktrees are pooled, so Kiro's store holds earlier
sessions' conversations under the same folder. Resume and recovery run `--resume-id <id>`; a session
with no conversation of its own starts again with its first prompt, never `--resume` (Kiro's newest
in the folder, which can be an earlier session's).

**Colors.** Kiro 3.0 picks its theme once, at its start: the one picked with its `/theme`, else
`KIRO_TERMINAL_THEME` (`dark`, `light`, `safe`), else a guess from `COLORFGBG`, then an OSC 11 query
to the terminal (0.2 s for the answer), then the terminal's variables (`ITERM_PROFILE` and others),
then the system (macOS in light mode reads as light). With nothing answering the query, a Mac in light
mode gave Kode's dark terminal Kiro's light theme: its prompt bars on `#EEEEEE` with the
terminal's own near-white text. So Kode starts Kiro with `KIRO_TERMINAL_THEME` set to the terminal
colors sessions start with (`LaunchInput.termTheme`, `Core.termTheme`: the ones a browser last
reported, dark before any did; [The app](app.md)), and the session host answers the query with them
too. A theme picked with `/theme` in Kiro still wins, and a running session keeps its colors until
it starts again.

**Status.** Kiro fires no hook when it waits for a permission answer, when you interrupt a turn,
or when it exits. A session waiting on a permission prompt shows as working until you answer in the
terminal; an interrupted turn ends with the next turn's `stop`; an exit is found by reconcile's dead
pane, as Claude's crash is. With no hooks at all, the hookless fallback follows the session file.

**No background dot.** Kiro CLI 2.34 has background shells only behind a feature flag that's off
for nearly everyone (`background_execution`, dark-shipped), and its `stop` hook carries no list of
what's running, so a Kiro session between turns always shows the plain idle dot. Kode doesn't
guess from the processes still running under the idle agent: Kiro always runs its MCP servers and
can run language servers there, so nothing tells a background command from those.

**Prompts, designs, learn.** The paths Claude Code takes without the mod: prompts are typed into
the terminal (a bracketed paste, Enter 300 ms later, which Kiro's TUI takes multi-line), designs are
markdown in the session's design folder with `questions.json` (the Design prompt only offers
Artifacts "if you have the Artifact tool", and names the `design_questions` tool only to an agent
with the mod, so Kiro is told to write the file; its key design decisions and Meko items go there the same
way), and the learn step is a typed `learn` prompt.

**Usage** is in [Token usage](token-usage.md#kiros-credits); Meko is in
[Datapacks](datapacks.md) (Meko's own setup for Kiro, `--client kiro`); Ask Kode on Kiro is in
[Ask Kode](palette.md#ask-kode).
