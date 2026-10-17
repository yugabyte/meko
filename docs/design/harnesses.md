# Agents: Claude Code and Kiro

Part of the [AgentK Local design](../design.md).

A session runs on one coding agent, its **harness**, picked when it starts and kept for its life (a
conversation can't move between CLIs): Claude Code, or [Kiro CLI](https://kiro.dev/cli/). The
worktree, stages and gates, the PR flow, Meko recall and learn, the browser terminal, recovery and
Resume work the same on both. Codex and Cursor CLI are left for later; the interface has room for them.

## Picking one

`[agents] default` (Settings → General, **Default agent**) is where new sessions and
[Ask AgentK](palette.md#ask-agentk) start. The New session dialog shows **Agent** under
**Advanced** once more than one harness is available, and a failed start's **Try again** keeps the
one it asked for (`CreateSessionRequest.harness`). A harness is *available* when it's turned on and
its `--version` answers with its minimum version or later (`AppState.harnesses`; Kiro: `[kiro]
enabled`, 2.32.0). A create that names one that isn't is a 400 that says what to do; the dialog
always names the agent it shows. A default that isn't available any more never decides: a request
that names no harness, and Ask AgentK, start on Claude Code (`Harnesses.defaultFor`). Settings'
**Default agent**, like New session's **Agent**, offers only available agents and shows only while
two or more are: with one, there's nothing to choose (`defaultAgentChoice()` in `util.ts`). A
stored default that goes unavailable (Kiro missing, too old, or `[kiro] enabled` turned off in
config.toml) stays stored, and decides again once it's available; turning **Offer Kiro** off in
Settings is the exception, which also sets the default to Claude Code. Both choices are
`AgentPicker` in `common.tsx`, each agent's logo then its name. The session's harness is
`sessions.harness` (`Session.harness`).

**Telling them apart.** Each session's tooltip names its agent. While the open sessions across all
projects run on more than one harness (`harnessLogos()` in `src/web/util.ts`; archived sessions
don't count), every session tab and every session row in the rail shows its agent's logo between
its dot and its name (`HarnessLogo` in `common.tsx`, all in one 18 × 14 px box so names line up);
while they all run on one, nothing is added. The logos are files in `src/web/public/`, never
loaded from elsewhere: `harness-claude.svg` is Clawd, Claude Code's mascot, redrawn cell for cell
from `png/clawd.png` in [dashboard-icons](https://github.com/homarr-labs/dashboard-icons)
(Apache-2.0), and `harness-kiro.svg` is Kiro's own `https://kiro.dev/icon.svg`. Both are colored
marks that read in either theme.

## The adapter

Everything Claude-specific sits behind one interface, `Harness` in `src/server/harness/`, with an
adapter per CLI (`claude.ts`, `kiro.ts`) and the registry (`Harnesses` in `index.ts`). What each can
do is data, `HARNESS_CAPS` in `src/shared/harnesses.ts`, which the server and the UI both gate on:

| | Claude Code | Kiro |
|---|---|---|
| Launch | `claude --settings … --session-id/--resume …` | `kiro-cli chat --agent agentk [--trust-tools=*] [--resume-id <id>] [prompt]` |
| Hooks | `run/hooks.json` via `--settings` | the `agentk` agent's inline hooks |
| System prompt | `--append-system-prompt` | printed by the context hook at every `agentSpawn` |
| Design guard | JSON deny | `kiro-guard.sh`: exit 2, the reason on stderr |
| Conversation id | chosen (`--session-id`) | Kiro's, from its hooks' `session_id`, else its session store |
| AgentK's mod, Artifact designs | yes | no |
| Plan limits in the rail | 5-hour and weekly, from the status line and the mod | credits this period, from `/usage` hourly |
| Needs-you and interrupt signals | `PermissionRequest`, `Notification`, the mod's `/mod/turn` | none |
| Usage | transcript tokens, priced | credits from Kiro's session file |

The launcher (`launcher.ts`) is shared: the login-shell script, the session's variables after the
profile, and every harness's markers cleared (`SESSION_MARKERS`: Claude Code's and
`KIRO_SESSION_ID`, `KIRO_CLI_IS_CHILD_PROCESS`, `KIRO_CLI_ACP_CLIENT_NAME`, `KIRO_HOST_PID`), so an
AgentK started inside either never leaks into a session of the other.

**Events.** Claude Code's hook shape is AgentK's event shape. The hook server knows each request's
session from its token, so `onHook` normalizes the payload with the session's adapter
(`normalize()`) before the status machine sees it. Kiro's `agentSpawn`, `userPromptSubmit`,
`preToolUse`, `postToolUse` and `stop` become `SessionStart`, `UserPromptSubmit`, `PreToolUse`,
`PostToolUse` and `Stop`, and Kiro's `session_id` moves to `agent_session_id` (Claude's `session_id`
is AgentK's key for the session's Meko conversation, `claude_session_id`, which stays as it was
for every harness). The guard and context routes answer through the adapter too (`deny()`,
`context()`): JSON for Claude Code, plain text for Kiro (`GuardDecision` and `HookContext` take
either).

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
Artifacts "if you have the Artifact tool"), and the learn step is a typed `learn` prompt.

**Usage** is in [Token usage](token-usage.md#kiros-credits); Meko is in
[Datapacks](datapacks.md) (Meko's own setup for Kiro, `--client kiro`); Ask AgentK on Kiro is in
[Ask AgentK](palette.md#ask-agentk).
