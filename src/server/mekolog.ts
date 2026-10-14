import { randomBytes } from 'node:crypto';
import { appendFileSync, closeSync, existsSync, fstatSync, mkdirSync, openSync, readSync, rmSync, statSync } from 'node:fs';
import { open, readFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import type { MekoCallBy, MekoCallDetail, MekoCallPage, MekoCallSummary } from '../shared/types.js';
import { errMsg, type Logger } from './log.js';
import type { MekoScope, MekoTrace } from './meko.js';
import { savedReplyFile } from './savedreply.js';

/** The session's Meko call log, in its folder under ~/.agent-k-local/designs. */
export const MEKO_LOG_FILE = 'meko-calls.jsonl';

/** How much of the file is read at a time, from the end. */
const CHUNK = 64 * 1024;

export const DEFAULT_PAGE = 50;
export const MAX_PAGE = 200;

/**
 * One line of the file. The keys before `args` are written first, so a page reads a call's
 * summary from the start of its line without parsing what was sent and what came back. Lines
 * written before Claude's calls were logged have no `by` or `subagent` (AgentK's calls).
 */
interface Line {
  id: string;
  at: string;
  tool: string;
  by?: MekoCallBy;
  subagent?: boolean;
  learn: string | null;
  ms: number;
  ok: boolean;
  attempts: number;
  error: string | null;
  args: unknown;
  /** Claude's call whose reply was too large for its context: the notice it read instead. */
  notice?: string | null;
  result: unknown;
}

/** One of Claude's Meko calls, as AgentK's mod reports it (`/mod/meko`), its reply already read. */
export interface ClaudeMekoCall {
  /** The Meko tool, without its MCP server's prefix (`context_search`). */
  tool: string;
  at: number;
  ms: number;
  ok: boolean;
  error: string | null;
  args: unknown;
  /** The reply's text (or the saved reply, for one too large for Claude), or null when none came. */
  text: string | null;
  notice: string | null;
  subagent: boolean;
}

/** Where a line's summary ends. Inside a JSON string a quote is escaped, so this only matches the key. */
const ARGS_KEY = Buffer.from(',"args":');

export function mekoLogPath(designs: string, sessionId: string): string {
  return join(designs, sessionId, MEKO_LOG_FILE);
}

/** Deletes a session's Meko call log (with the session, or when the user clears it). */
export function removeMekoLog(designs: string, sessionId: string): void {
  rmSync(mekoLogPath(designs, sessionId), { force: true });
}

export interface MekoCallLogOptions {
  /** ~/.agent-k-local/designs: each session's folder is there. */
  designs: string;
  /**
   * Whether the session's calls are logged: it's this start of it (not a failed start whose id
   * Try again reused), it's running, and its project has Log Meko calls on.
   */
  enabled: (session: MekoScope) => boolean;
  log: Logger;
}

/**
 * Each session's Meko calls, AgentK's (`record`) and Claude's (`recordClaude`, as AgentK's mod
 * reports them), with what was sent and what came back, one JSON object per line of
 * `meko-calls.jsonl` in the session's folder, for debugging. Lines are in the order the server
 * heard of the calls ending: one of Claude's whose reply is still being read from the file Claude
 * Code saved it to holds back the session's later lines (`queue`). Nothing is cut and the file has no
 * cap: it goes when the session is archived. A page is read backwards from the end, a chunk at a
 * time without blocking the server, parsing only each line's summary: an unfiltered page reads
 * only as far back as its own calls, a filtered one as far as it takes to find them (the whole
 * file at worst). One call is read by the offset its line starts at.
 */
export class MekoCallLog {
  /** Files known to end with a newline: this process wrote their last line. */
  private whole = new Set<string>();
  /** Per session, the last line still waiting to be written: later lines are written after it. */
  private queue = new Map<string, Promise<void>>();

  constructor(private o: MekoCallLogOptions) {}

  /** Whether the session's calls are kept now. Never throws. */
  wants(session: MekoScope): boolean {
    try {
      return this.o.enabled(session);
    } catch {
      return false;
    }
  }

  /** Appends a call AgentK made for a session whose project logs them. Never throws. */
  record(t: MekoTrace): void {
    if (!t.session) return;
    this.write(t.session, {
      id: randomBytes(4).toString('hex'),
      at: new Date(t.at).toISOString(),
      tool: t.tool,
      by: 'agentk',
      learn: t.tag,
      ms: t.ms,
      ok: t.ok,
      attempts: t.attempts,
      error: t.error,
      args: t.args,
      result: t.text === null ? null : parseReply(t.text),
    });
  }

  /**
   * Appends one of Claude's calls in a session whose project logs them, in its place now while its
   * reply is still being read (`call`, null when there's nothing to log). Never rejects.
   */
  recordClaude(session: MekoScope, call: Promise<ClaudeMekoCall | null>): Promise<void> {
    const id = randomBytes(4).toString('hex');
    return this.write(session, call.then((c) => c && claudeLine(id, c), () => null));
  }

  /** Writes a line now, unless an earlier one of the session's is still waiting: then after it. */
  private write(session: MekoScope, line: Line | Promise<Line | null>): Promise<void> {
    const before = this.queue.get(session.id);
    if (!before && !(line instanceof Promise)) {
      this.append(session, line);
      return Promise.resolve();
    }
    const done = (async () => {
      await before;
      const l = await line;
      if (l) this.append(session, l);
    })();
    this.queue.set(session.id, done);
    void done.finally(() => {
      if (this.queue.get(session.id) === done) this.queue.delete(session.id);
    });
    return done;
  }

  private append(session: MekoScope, line: Line): void {
    const file = mekoLogPath(this.o.designs, session.id);
    try {
      if (!this.o.enabled(session)) return;
      mkdirSync(dirname(file), { recursive: true });
      // A line a crash cut short gets its newline first, so this one isn't joined onto it.
      const lead = this.whole.has(file) || !endsMidLine(file) ? '' : '\n';
      // One write per line, so a reader never sees half of one (bar a crash mid-write, which it skips).
      appendFileSync(file, lead + JSON.stringify(line) + '\n', { mode: 0o600 });
      this.whole.add(file);
    } catch (e) {
      this.whole.delete(file);
      this.o.log.warn(`writing the Meko call log of ${session.id} failed`, errMsg(e));
    }
  }

  /** The file's size and when it last changed, or null when the session has no log. */
  stat(sessionId: string): { bytes: number; updatedAt: number } | null {
    try {
      const s = statSync(mekoLogPath(this.o.designs, sessionId));
      return { bytes: s.size, updatedAt: s.mtimeMs };
    } catch {
      return null;
    }
  }

  /** Calls older than the byte offset `before` (newest first) that match the filters. */
  async page(sessionId: string, q: { before?: number; limit?: number; tool?: string; by?: MekoCallBy; failed?: boolean } = {}): Promise<MekoCallPage> {
    const limit = Math.min(MAX_PAGE, Math.max(1, q.limit ?? DEFAULT_PAGE));
    const calls: MekoCallSummary[] = [];
    let more = false;
    const fh = await openOrNull(mekoLogPath(this.o.designs, sessionId));
    if (!fh) return { calls, more };
    try {
      const size = (await fh.stat()).size;
      let pos = Math.min(size, Math.max(0, q.before ?? size));
      // Bytes from `pos` up to the start of the last line looked at: the end of a line split by a chunk.
      let carry = Buffer.alloc(0);
      const take = (bytes: Buffer, offset: number): boolean => {
        const s = parseSummary(bytes, offset);
        if (!s || (q.tool && s.tool !== q.tool) || (q.by && s.by !== q.by) || (q.failed && s.ok)) return false;
        if (calls.length === limit) {
          more = true;
          return true;
        }
        calls.push(s);
        return false;
      };
      outer: while (pos > 0) {
        const start = Math.max(0, pos - CHUNK);
        const buf = Buffer.alloc(pos - start);
        await fh.read(buf, 0, buf.length, start);
        const data = carry.length ? Buffer.concat([buf, carry]) : buf;
        let end = data.length;
        // (Not lastIndexOf(…, -1): a negative offset counts from the end.)
        while (end > 0) {
          const nl = data.lastIndexOf(0x0a, end - 1);
          if (nl < 0) break;
          if (take(data.subarray(nl + 1, end), start + nl + 1)) break outer;
          end = nl;
        }
        carry = data.subarray(0, end);
        pos = start;
      }
      // The file's first line, which has no newline before it.
      if (!more && pos === 0 && carry.length) take(carry, 0);
    } finally {
      await fh.close();
    }
    return { calls, more };
  }

  /** The call whose line starts at `offset`, if its id is `id`. */
  async detail(sessionId: string, offset: number, id: string): Promise<MekoCallDetail | null> {
    if (!Number.isSafeInteger(offset) || offset < 0) return null;
    const fh = await openOrNull(mekoLogPath(this.o.designs, sessionId));
    if (!fh) return null;
    try {
      const size = (await fh.stat()).size;
      const parts: Buffer[] = [];
      for (let pos = offset; pos < size;) {
        const buf = Buffer.alloc(Math.min(CHUNK, size - pos));
        await fh.read(buf, 0, buf.length, pos);
        const nl = buf.indexOf(0x0a);
        parts.push(nl >= 0 ? buf.subarray(0, nl) : buf);
        if (nl >= 0) break;
        pos += buf.length;
      }
      const l = parseLine(Buffer.concat(parts));
      if (!l || l.id !== id) return null;
      return { ...summary(l, offset), args: l.args ?? null, result: l.result ?? null, notice: l.notice ?? null };
    } finally {
      await fh.close();
    }
  }

  clear(sessionId: string): void {
    removeMekoLog(this.o.designs, sessionId);
  }
}

async function openOrNull(file: string) {
  try {
    return await open(file, 'r');
  } catch {
    return null;
  }
}

/** The file has bytes and its last one isn't a newline: a crash cut its last line short. */
function endsMidLine(file: string): boolean {
  if (!existsSync(file)) return false;
  const fd = openSync(file, 'r');
  try {
    const size = fstatSync(fd).size;
    if (!size) return false;
    const b = Buffer.alloc(1);
    readSync(fd, b, 0, 1, size - 1);
    return b[0] !== 0x0a;
  } finally {
    closeSync(fd);
  }
}

/** The reply as JSON when it is, else its text. */
function parseReply(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

/** A line of the file, or null for a blank one or one that isn't a call (a write cut short). */
function parseLine(bytes: Buffer): Line | null {
  if (!bytes.length) return null;
  try {
    const l = JSON.parse(bytes.toString('utf8')) as Line;
    return l && typeof l === 'object' && typeof l.id === 'string' && typeof l.tool === 'string' ? l : null;
  } catch {
    return null;
  }
}

/** A line's summary, parsed from the part before `args` only. */
function parseSummary(bytes: Buffer, offset: number): MekoCallSummary | null {
  const head = bytes.indexOf(ARGS_KEY);
  const l = parseLine(head >= 0 ? Buffer.concat([bytes.subarray(0, head), Buffer.from('}')]) : bytes);
  return l && summary(l, offset);
}

function summary(l: Line, offset: number): MekoCallSummary {
  return {
    id: l.id,
    offset,
    at: Date.parse(l.at) || 0,
    tool: l.tool,
    by: l.by === 'claude' ? 'claude' : 'agentk',
    subagent: l.subagent === true,
    learn: l.learn ?? null,
    ms: l.ms,
    ok: l.ok,
    attempts: l.attempts,
    error: l.error ?? null,
  };
}

/**
 * The reply of one of Claude's calls: when Claude read only Claude Code's notice that it was too
 * large, the reply Claude Code saved (`notice` the notice), else the text itself. The file is read
 * only from a session's `tool-results` folder beside the transcript (`savedReplyFile`), and a file
 * that can't be read leaves the notice as the reply.
 */
export async function claudeReply(text: string | null, transcriptPath: string | null): Promise<{ text: string | null; notice: string | null }> {
  const file = savedReplyFile(text, transcriptPath);
  if (!file) return { text, notice: null };
  try {
    return { text: await readFile(file, 'utf8'), notice: text };
  } catch {
    return { text, notice: null };
  }
}

/** One of Claude's calls as a line of the file. */
function claudeLine(id: string, c: ClaudeMekoCall): Line {
  return {
    id,
    at: new Date(c.at).toISOString(),
    tool: c.tool,
    by: 'claude',
    subagent: c.subagent,
    learn: null,
    ms: c.ms,
    ok: c.ok,
    attempts: 1,
    error: c.error,
    args: c.args,
    notice: c.notice,
    result: c.text === null ? null : parseReply(c.text),
  };
}
