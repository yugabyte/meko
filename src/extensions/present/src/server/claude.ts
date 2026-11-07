// Runs Claude Code headless for a deck: one persisted session per deck, which every job continues
// (`--session-id` the first time, `--resume` after, `--fork-session` for jobs that run side by side),
// with all of Claude's tools and the user's own MCP servers, plus Meko under Presenter's own name.
// The answer must match a JSON schema; what Claude does on the way streams in as activity.

import { spawn } from 'node:child_process';

/** The name Meko gets in the MCP config Presenter passes, beside the user's own servers. */
export const MEKO_SERVER = 'present_meko';
/** The environment variable the MCP config reads Meko's key from, so it's never on the command line. */
export const MEKO_KEY_ENV = 'PRESENT_MEKO_KEY';

export interface SessionArgs {
  /** The deck's session id. */
  id: string;
  /** Whether it has run before: then it's resumed, else started with this id. */
  started: boolean;
  /** Start from the session as it is, as a new session that's dropped after (parallel scene builds). */
  fork?: boolean;
}

export interface ClaudeArgsInput {
  session: SessionArgs | null;
  schema: object;
  systemPrompt: string;
  mekoUrl: string | null;
  model?: string;
}

/** The arguments for one job, as an array (never a shell). */
export function claudeArgs(i: ClaudeArgsInput): string[] {
  const args = ['-p', '--output-format', 'stream-json', '--verbose', '--json-schema', JSON.stringify(i.schema)];
  if (i.model) args.push('--model', i.model);
  // A headless run can't stop to ask, and Claude may use any tool: it works in the deck's own folder
  // and is told to change nothing outside it.
  args.push('--permission-mode', 'bypassPermissions');
  // The user's own settings keep their sign-in, provider and MCP servers; hooks stay off.
  args.push('--settings', JSON.stringify({ disableAllHooks: true }));
  if (i.mekoUrl) {
    args.push('--mcp-config', JSON.stringify({
      mcpServers: { [MEKO_SERVER]: { type: 'http', url: i.mekoUrl, headers: { Authorization: `Bearer \${${MEKO_KEY_ENV}}` } } },
    }));
  }
  args.push('--append-system-prompt', i.systemPrompt);
  if (!i.session) args.push('--no-session-persistence');
  else if (!i.session.started) args.push('--session-id', i.session.id);
  else args.push('--resume', i.session.id, ...(i.session.fork ? ['--fork-session'] : []));
  return args;
}

export interface RunResult {
  /** The schema's answer, or null when there's none. */
  answer: unknown;
  /** Claude's own words when it failed. */
  error: string | null;
  sessionId: string | null;
  code: number | null;
  spawnError: string | null;
  cancelled: boolean;
  timedOut: boolean;
}

/** Runs Claude: the arguments as an array, the message on stdin, each stream-json event to `onEvent`. */
export type ClaudeRunner = (cmd: string[], args: string[], o: {
  cwd: string; input: string; env: NodeJS.ProcessEnv; timeoutMs: number; signal: AbortSignal; onEvent: (e: Record<string, unknown>) => void;
}) => Promise<RunResult>;

export const runClaude: ClaudeRunner = (cmd, args, o) =>
  new Promise((resolve) => {
    const r: RunResult = { answer: null, error: null, sessionId: null, code: null, spawnError: null, cancelled: false, timedOut: false };
    // Cancelled before it started (while the job waited on Meko): an abort listener added now never fires.
    if (o.signal.aborted) {
      r.cancelled = true;
      return resolve(r);
    }
    let child;
    try {
      child = spawn(cmd[0]!, [...cmd.slice(1), ...args], { cwd: o.cwd, env: o.env, stdio: ['pipe', 'pipe', 'pipe'] });
    } catch (e) {
      r.spawnError = (e as Error).message;
      return resolve(r);
    }
    let buf = '';
    let stderr = '';
    const timer = setTimeout(() => {
      r.timedOut = true;
      child.kill('SIGTERM');
    }, o.timeoutMs);
    const onAbort = () => {
      r.cancelled = true;
      child.kill('SIGTERM');
    };
    o.signal.addEventListener('abort', onAbort, { once: true });
    const line = (l: string) => {
      if (!l.trim()) return;
      let e: Record<string, unknown>;
      try {
        e = JSON.parse(l) as Record<string, unknown>;
      } catch {
        return;
      }
      if (typeof e.session_id === 'string') r.sessionId = e.session_id;
      if (e.type === 'result') {
        r.answer = e.structured_output ?? null;
        if (r.answer === null && typeof e.result === 'string') {
          try {
            r.answer = JSON.parse(e.result);
          } catch {
            /* no answer */
          }
        }
        if (e.is_error === true) r.error = typeof e.result === 'string' ? e.result : 'Claude Code reported an error.';
      }
      o.onEvent(e);
    };
    child.stdout.setEncoding('utf8');
    child.stdout.on('data', (d: string) => {
      buf += d;
      let nl: number;
      while ((nl = buf.indexOf('\n')) >= 0) {
        line(buf.slice(0, nl));
        buf = buf.slice(nl + 1);
      }
    });
    child.stderr.setEncoding('utf8');
    child.stderr.on('data', (d: string) => {
      if (stderr.length < 64 * 1024) stderr += d;
    });
    child.on('error', (e) => {
      r.spawnError = e.message;
    });
    child.on('close', (code) => {
      line(buf);
      clearTimeout(timer);
      o.signal.removeEventListener('abort', onAbort);
      r.code = code;
      if (!r.error && code !== 0 && !r.cancelled && !r.timedOut) r.error = stderr.trim().split('\n').slice(-3).join(' ') || `Claude Code exited with ${code}.`;
      resolve(r);
    });
    child.stdin.on('error', () => undefined);
    child.stdin.end(o.input);
  });

/** One line of what Claude is doing, from a stream-json event, or null for events that say nothing. */
export function activityOf(e: Record<string, unknown>): string[] {
  if (e.type !== 'assistant') return [];
  const content = (e.message as { content?: unknown[] } | undefined)?.content ?? [];
  const out: string[] = [];
  for (const c of content as Record<string, unknown>[]) {
    if (c.type !== 'tool_use') continue;
    const name = String(c.name ?? '');
    const input = (c.input ?? {}) as Record<string, unknown>;
    const s = (v: unknown) => String(v ?? '').slice(0, 120);
    if (name.startsWith(`mcp__${MEKO_SERVER}__`) || name.startsWith('mcp__meko__')) {
      const tool = name.split('__').pop()!;
      out.push(`Meko: ${tool.replace(/_/g, ' ')}${input.query ? ` "${s(input.query)}"` : ''}`);
    } else if (name === 'WebSearch') out.push(`searching the web: ${s(input.query)}`);
    else if (name === 'WebFetch') out.push(`reading ${s(input.url)}`);
    else if (name === 'Read') out.push(`reading ${s(input.file_path)}`);
    else if (name === 'Write' || name === 'Edit') out.push(`writing ${s(input.file_path)}`);
    else if (name === 'Bash') out.push(`running ${s(input.command)}`);
    else out.push(`using ${name}`);
  }
  return out;
}

/** Whether Claude's error means the session's transcript isn't on this computer. */
export function sessionMissing(error: string | null): boolean {
  return !!error && /no conversation found|session.*not found|could not find session/i.test(error);
}

/** Whether Claude's error means it isn't signed in. */
export function notSignedIn(error: string | null): boolean {
  return !!error && /not logged in|\/login|invalid api key|authenticat/i.test(error);
}
