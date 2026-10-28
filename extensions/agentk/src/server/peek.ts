// A quick look at where a Claude Code session is: its last reply, and the tool it's running since, from its transcript's tail.
import { closeSync, fstatSync, openSync, readSync } from 'node:fs';
import type { PeekReply, PeekTool } from '../shared/types.js';

/** How much of the transcript's end is read: enough for the last reply, however many tool calls follow it. */
const TAIL_BYTES = 512 * 1024;
const REPLY_MAX = 4000;
const SUMMARY_MAX = 160;

/** The fields that say what a tool call does, in the order they're looked for. */
const SUMMARY_FIELDS = ['description', 'command', 'file_path', 'path', 'pattern', 'query', 'url', 'prompt'];

function cut(s: string, max: number): string {
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
}

function toolSummary(input: unknown): string {
  if (!input || typeof input !== 'object') return '';
  const r = input as Record<string, unknown>;
  const v = SUMMARY_FIELDS.map((k) => r[k]).find((x): x is string => typeof x === 'string' && !!x.trim());
  return v ? cut(v.replace(/\s+/g, ' ').trim(), SUMMARY_MAX) : '';
}

/**
 * From a transcript's lines, oldest first: the last thing Claude said (its last text, cut to
 * REPLY_MAX) and the tool it called after that, if any (what it's doing now, or did last). Subagents'
 * and meta lines are left out.
 */
export function lastActivity(lines: string[]): { reply: PeekReply | null; tool: PeekTool | null } {
  let tool: PeekTool | null = null;
  for (let i = lines.length - 1; i >= 0; i--) {
    let e: { type?: string; isSidechain?: boolean; isMeta?: boolean; timestamp?: string; message?: { content?: unknown } };
    try {
      e = JSON.parse(lines[i]) as typeof e;
    } catch {
      continue;
    }
    if (e.type !== 'assistant' || e.isSidechain || e.isMeta || !Array.isArray(e.message?.content)) continue;
    const at = e.timestamp ? Date.parse(e.timestamp) || null : null;
    const blocks = e.message.content as { type?: string; text?: string; name?: string; input?: unknown }[];
    for (let j = blocks.length - 1; j >= 0; j--) {
      const b = blocks[j];
      if (b.type === 'text' && typeof b.text === 'string' && b.text.trim()) return { reply: { text: cut(b.text.trim(), REPLY_MAX), at }, tool };
      if (b.type === 'tool_use' && !tool && typeof b.name === 'string') tool = { name: b.name, summary: toolSummary(b.input), at };
    }
  }
  return { reply: null, tool };
}

/** lastActivity of the file's last TAIL_BYTES (its first, partial line dropped when it's cut). */
export function readLastActivity(path: string): { reply: PeekReply | null; tool: PeekTool | null } {
  let fd: number;
  try {
    fd = openSync(path, 'r');
  } catch {
    return { reply: null, tool: null };
  }
  try {
    const size = fstatSync(fd).size;
    const start = Math.max(0, size - TAIL_BYTES);
    const buf = Buffer.alloc(size - start);
    readSync(fd, buf, 0, buf.length, start);
    const lines = buf.toString('utf8').split('\n').filter(Boolean);
    return lastActivity(start > 0 ? lines.slice(1) : lines);
  } finally {
    closeSync(fd);
  }
}
