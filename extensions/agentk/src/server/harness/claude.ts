import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { HARNESS_CAPS } from '../../shared/harnesses.js';
import type { HarnessStatus } from '../../shared/types.js';
import { claudeAsker } from '../askagentk.js';
import { EDIT_TOOLS, claudeLauncher, modDirPath, statusLineScriptPath, transcriptPathFor, userStatusLine, writeModFiles } from '../claude.js';
import type { ClaudeCode } from '../claudecode.js';
import type { Config } from '../config.js';
import type { HookPayload } from '../hooks.js';
import { errMsg, type Logger } from '../log.js';
import { claudePath, type Paths } from '../paths.js';
import type { SessionRow } from '../store.js';
import { trustFolder } from '../trust.js';
import { ingestSessionUsage, sessionTranscript } from '../usage.js';
import type { Harness } from './types.js';

/**
 * Claude Code: AgentK's first harness, and the only one with AgentK's mod and Artifact designs. Like
 * any other, it's available while it's turned on and its `--version` answers (any version runs
 * sessions; the mod needs MIN_MOD_CLAUDE).
 */
export function claudeHarness(deps: { claudeCode: ClaudeCode; log: Logger }): Harness {
  const status = (cfg: Config): HarnessStatus => {
    const s = deps.claudeCode.status();
    return {
      id: 'claude', enabled: cfg.claude.enabled, version: s.version, minVersion: HARNESS_CAPS.claude.minVersion,
      available: cfg.claude.enabled && s.version !== null, checked: deps.claudeCode.checked(), isDefault: cfg.agents.default === 'claude',
      updating: s.updating,
    };
  };
  // The mod's files are copied at start (prepare); a half-updated install may lack them.
  const modReady = (paths: Paths, cfg: Config) => cfg.claude.mod && existsSync(join(modDirPath(paths), 'hooks', 'register.ts'));
  const transcript = (row: SessionRow) => sessionTranscript(row, transcriptPathFor);
  return {
    id: 'claude',
    caps: HARNESS_CAPS.claude,
    systemPromptByContext: false,
    // The hook files (run/hooks.json and the scripts Kiro's agent runs too) are AgentK's own: Core
    // writes them at start whether or not Claude Code is there (writeHookFiles in Core.start).
    async prepare(paths) {
      try {
        if (!writeModFiles(paths)) deps.log.warn("AgentK's mod for Claude Code is missing from this install; sessions start without it");
      } catch (e) {
        deps.log.warn("couldn't write AgentK's mod for Claude Code; sessions start without it", errMsg(e));
      }
      // Reads the Claude Code version now (none while it's turned off), so the first launch doesn't
      // wait for it, and New session and the banners know whether it's there.
      await deps.claudeCode.version();
    },
    async beforeLaunch({ row, repo, log }) {
      // Claude Code keys folder trust by the git root, which for a worktree is the main repo.
      if (repo) await trustFolder(repo, log);
      // Started by its local spelling (claudePath), a worktree isn't tied back to its repo (its .git
      // file names the stored spelling), so Claude asks about the worktree itself: trust exactly it.
      const cwd = claudePath(row.worktree_path);
      if (cwd !== row.worktree_path) await trustFolder(cwd, log, true);
    },
    async modDir(paths, cfg) {
      return modReady(paths, cfg) && (await deps.claudeCode.supportsMod()) ? modDirPath(paths) : null;
    },
    // A version not read yet counts as new enough: the prompt lines say "if you have it".
    offersMod: (paths, cfg) => modReady(paths, cfg) && deps.claudeCode.status().supportsMod !== false,
    launcher: (paths, cfg, l) => claudeLauncher(paths, {
      sessionId: l.row.id, worktree: l.row.worktree_path, claudeCommand: cfg.claude.command, claudeSessionId: l.row.claude_session_id,
      resume: l.resume, name: l.row.name, planMode: l.planMode, addDirs: l.addDirs, pluginDirs: l.modDir ? [l.modDir] : [],
      appendSystemPrompt: l.systemPrompt, prompt: l.prompt, unsetEnv: l.unsetEnv, userSettingsOnly: l.untrusted,
      env: {
        ...l.env,
        // Our status line replaces the user's; it runs theirs (resolved for this worktree) after reporting limits.
        // Set even when empty: empty means the user has none, unset means a session from before this.
        // Not a PR's own status line when its code isn't trusted: the relay runs it with sh.
        AK_USER_STATUSLINE: userStatusLine(l.untrusted ? null : l.row.worktree_path, statusLineScriptPath(paths))?.command ?? '',
        // Plugins' hooks modules are early access in Claude Code, off unless this says otherwise.
        ...(l.modDir ? { CLAUDE_CODE_ENABLE_FUNCTION_HOOKS: '1' } : {}),
      },
    }),
    // Claude Code's hook shape is AgentK's event shape.
    normalize: (raw) => raw,
    deny: (reason) => ({ hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: reason } }),
    context: (text) => ({ hookSpecificOutput: { hookEventName: 'SessionStart', additionalContext: text } }),
    editedPaths(p: HookPayload) {
      if (!EDIT_TOOLS.includes(p.tool_name ?? '')) return null;
      const raw = p.tool_input?.file_path ?? p.tool_input?.notebook_path;
      return typeof raw === 'string' && raw ? [raw] : [];
    },
    canResume: (row) =>
      (!!row.transcript_path && existsSync(row.transcript_path)) || existsSync(transcriptPathFor(row.worktree_path, row.claude_session_id)),
    activityFiles: (row) => [row.transcript_path, transcriptPathFor(row.worktree_path, row.claude_session_id)].filter((f): f is string => !!f),
    readUsage: (db, row) => ({ changed: ingestSessionUsage(db, row.id, transcript(row), row.base_branch) }),
    captureTranscript: transcript,
    ask: claudeAsker,
    status,
    version: (_cfg, force) => deps.claudeCode.version(force),
    async update(cfg) {
      await deps.claudeCode.update();
      return status(cfg);
    },
  };
}
