import { existsSync } from 'node:fs';
import { HARNESS_CAPS } from '../../shared/harnesses.js';
import type { HarnessStatus } from '../../shared/types.js';
import { EDIT_TOOLS, claudeLauncher, transcriptPathFor, writeModFiles } from '../claude.js';
import type { ClaudeCode } from '../claudecode.js';
import type { Config } from '../config.js';
import type { HookPayload } from '../hooks.js';
import { errMsg, type Logger } from '../log.js';
import type { SessionRow } from '../store.js';
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
      id: 'claude', enabled: cfg.claude.enabled, version: s.version, minVersion: null, available: cfg.claude.enabled && s.version !== null,
      checked: deps.claudeCode.checked(), isDefault: cfg.agents.default === 'claude', updating: s.updating,
    };
  };
  return {
    id: 'claude',
    caps: HARNESS_CAPS.claude,
    command: (cfg) => cfg.claude.command,
    enabled: (cfg) => cfg.claude.enabled,
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
    launcher: (paths, cfg, l) => claudeLauncher(paths, {
      sessionId: l.row.id, worktree: l.row.worktree_path, claudeCommand: cfg.claude.command, claudeSessionId: l.row.claude_session_id,
      resume: l.resume, name: l.row.name, planMode: l.planMode, addDirs: l.addDirs, pluginDirs: l.pluginDirs,
      appendSystemPrompt: l.systemPrompt, prompt: l.prompt, env: l.env, unsetEnv: l.unsetEnv,
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
    canResume: (row: SessionRow) =>
      (!!row.transcript_path && existsSync(row.transcript_path)) || existsSync(transcriptPathFor(row.worktree_path, row.claude_session_id)),
    status,
    version: (_cfg, force) => deps.claudeCode.version(force),
    async update(cfg) {
      await deps.claudeCode.update();
      return status(cfg);
    },
  };
}
