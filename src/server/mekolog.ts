import { randomBytes } from 'node:crypto';
import { appendFileSync, closeSync, existsSync, mkdirSync, openSync, readSync, rmSync, statSync } from 'node:fs';
import { dirname, join } from 'node:path';
import type { MekoCallDetail, MekoCallPage, MekoCallSummary } from '../shared/types.js';
import { errMsg, type Logger } from './log.js';
import type { MekoTrace } from './meko.js';

/** The session's Meko call log, in its folder under ~/.agent-k-local/designs. */
export const MEKO_LOG_FILE = 'meko-calls.jsonl';

/** How much of the file is read at a time, from the end. */
const CHUNK = 64 * 1024;

export const DEFAULT_PAGE = 50;
export const MAX_PAGE = 200;

/** One line of the file. */
interface Line {
  id: string;
  at: string;
  tool: string;
  learn: string | null;
  ms: number;
  ok: boolean;
  attempts: number;
  args: unknown;
  result: unknown;
  error?: string | null;
}

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
  /** Whether the session's calls are logged: it's running and its project has Log Meko calls on. */
  enabled: (sessionId: string) => boolean;
  log: Logger;
}

/**
 * Each session's Meko calls, with what AgentK sent and what came back, one JSON object per line
 * of `meko-calls.jsonl` in the session's folder, for debugging. Nothing is cut and the file has no
 * cap: it goes when the session is archived. It's read backwards from the end, a page at a time,
 * and one call by the offset its line starts at, so a big file stays cheap to look at.
 */
export class MekoCallLog {
  constructor(private o: MekoCallLogOptions) {}

  /** Appends a call made for a session whose project logs them. Never throws. */
  record(t: MekoTrace): void {
    if (!t.sessionId) return;
    try {
      if (!this.o.enabled(t.sessionId)) return;
      const line: Line = {
        id: randomBytes(4).toString('hex'),
        at: new Date(t.at).toISOString(),
        tool: t.tool,
        learn: t.tag,
        ms: t.ms,
        ok: t.ok,
        attempts: t.attempts,
        args: t.args,
        result: t.text === null ? null : parseReply(t.text),
        ...(t.error !== null ? { error: t.error } : {}),
      };
      const file = mekoLogPath(this.o.designs, t.sessionId);
      mkdirSync(dirname(file), { recursive: true });
      // One write per line, so a reader never sees half of one (bar a crash mid-write, which it skips).
      appendFileSync(file, JSON.stringify(line) + '\n', { mode: 0o600 });
    } catch (e) {
      this.o.log.warn(`writing the Meko call log of ${t.sessionId} failed`, errMsg(e));
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
  page(sessionId: string, q: { before?: number; limit?: number; tool?: string; failed?: boolean } = {}): MekoCallPage {
    const file = mekoLogPath(this.o.designs, sessionId);
    if (!existsSync(file)) return { calls: [], more: false };
    const limit = Math.min(MAX_PAGE, Math.max(1, q.limit ?? DEFAULT_PAGE));
    const calls: MekoCallSummary[] = [];
    let more = false;
    const fd = openSync(file, 'r');
    try {
      const size = statSync(file).size;
      let pos = Math.min(size, Math.max(0, q.before ?? size));
      // Bytes from `pos` up to the start of the last line looked at: the end of a line split by a chunk.
      let carry = Buffer.alloc(0);
      const take = (bytes: Buffer, offset: number): boolean => {
        const l = parseLine(bytes);
        if (!l || (q.tool && l.tool !== q.tool) || (q.failed && l.ok)) return false;
        if (calls.length === limit) {
          more = true;
          return true;
        }
        calls.push(summary(l, offset));
        return false;
      };
      outer: while (pos > 0) {
        const start = Math.max(0, pos - CHUNK);
        const buf = Buffer.alloc(pos - start);
        readSync(fd, buf, 0, buf.length, start);
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
      closeSync(fd);
    }
    return { calls, more };
  }

  /** The call whose line starts at `offset`, if its id is `id`. */
  detail(sessionId: string, offset: number, id: string): MekoCallDetail | null {
    const file = mekoLogPath(this.o.designs, sessionId);
    if (!existsSync(file) || !Number.isSafeInteger(offset) || offset < 0) return null;
    const fd = openSync(file, 'r');
    try {
      const size = statSync(file).size;
      const parts: Buffer[] = [];
      for (let pos = offset; pos < size;) {
        const buf = Buffer.alloc(Math.min(CHUNK, size - pos));
        readSync(fd, buf, 0, buf.length, pos);
        const nl = buf.indexOf(0x0a);
        parts.push(nl >= 0 ? buf.subarray(0, nl) : buf);
        if (nl >= 0) break;
        pos += buf.length;
      }
      const l = parseLine(Buffer.concat(parts));
      if (!l || l.id !== id) return null;
      return { ...summary(l, offset), args: l.args ?? null, result: l.result ?? null };
    } finally {
      closeSync(fd);
    }
  }

  clear(sessionId: string): void {
    removeMekoLog(this.o.designs, sessionId);
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

function summary(l: Line, offset: number): MekoCallSummary {
  return {
    id: l.id,
    offset,
    at: Date.parse(l.at) || 0,
    tool: l.tool,
    learn: l.learn ?? null,
    ms: l.ms,
    ok: l.ok,
    attempts: l.attempts,
    error: l.error ?? null,
  };
}
