import { appendFileSync, mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { openDb, run } from '../../src/server/db.js';
import { ingestSessionUsage, parseUsageLine, projectUsage, totalTokens, usageByProject, usageBySession } from '../../src/server/usage.js';

function line(id: string, usage: Record<string, number>, opts: { model?: string; at?: string; type?: string } = {}): string {
  return JSON.stringify({
    type: opts.type ?? 'assistant',
    timestamp: opts.at ?? new Date().toISOString(),
    message: { id, model: opts.model ?? 'claude-opus-5-5', usage },
  }) + '\n';
}

function setup() {
  const dir = mkdtempSync(join(tmpdir(), 'akl-usage-'));
  const db = openDb(':memory:');
  const now = Date.now();
  run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('p', 'P', '/r', 'main', ?)`, now);
  for (const [id, desired] of [['s1', 'running'], ['s2', 'archived']]) {
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name,
             hook_token, desired, status, stage, created_at) VALUES (?, 'p', ?, ?, 'c', 'b', 'main', '/w', 't', ?, ?, 'idle', 'code', ?)`,
      id, id, id.toUpperCase(), `tok-${id}`, desired, now);
  }
  return { dir, db };
}

describe('token usage from transcripts', () => {
  it('parses model responses and ignores everything else', () => {
    expect(parseUsageLine(line('m1', { input_tokens: 2, output_tokens: 5, cache_read_input_tokens: 7, cache_creation_input_tokens: 11 }))).toMatchObject({
      messageId: 'm1', model: 'claude-opus-5-5', input: 2, output: 5, cacheRead: 7, cacheWrite: 11,
    });
    expect(parseUsageLine(line('m2', { input_tokens: 0 }, { model: '<synthetic>' }))).toBeNull();
    expect(parseUsageLine(line('m3', { input_tokens: 1 }, { type: 'user' }))).toBeNull();
    expect(parseUsageLine('{"type":"assistant","usage":')).toBeNull();
  });

  it('dedupes by message id, includes subagents, and reads incrementally', () => {
    const { dir, db } = setup();
    const main = join(dir, 'c.jsonl');
    const u = { input_tokens: 3, output_tokens: 50, cache_read_input_tokens: 1000, cache_creation_input_tokens: 200 };
    // A response spans several lines with the same id and usage.
    writeFileSync(main, line('a', u) + line('a', u) + '{"type":"user"}\n' + line('b', u));
    mkdirSync(join(dir, 'c', 'subagents'), { recursive: true });
    writeFileSync(join(dir, 'c', 'subagents', 'agent-x1.jsonl'), line('sub1', { input_tokens: 1, output_tokens: 9, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 }, { model: 'claude-haiku-4-5' }));

    expect(ingestSessionUsage(db, 's1', main)).toBe(true);
    let s = usageBySession(db).get('s1')!;
    expect(s).toEqual({ input: 7, output: 109, cacheRead: 2000, cacheWrite: 400, messages: 3 });
    expect(ingestSessionUsage(db, 's1', main)).toBe(false); // nothing new

    // A half-written line isn't counted until it is complete.
    const next = line('c', u);
    appendFileSync(main, next.slice(0, 20));
    ingestSessionUsage(db, 's1', main);
    expect(usageBySession(db).get('s1')!.messages).toBe(3);
    appendFileSync(main, next.slice(20));
    ingestSessionUsage(db, 's1', main);
    s = usageBySession(db).get('s1')!;
    expect(s.messages).toBe(4);
    expect(totalTokens(s)).toBe(3 * 1253 + 10); // a, b, c in the main transcript + one subagent response

    // A rewritten (shorter) transcript is re-read from the start without double counting.
    writeFileSync(main, line('a', u));
    ingestSessionUsage(db, 's1', main);
    expect(usageBySession(db).get('s1')!.messages).toBe(4);
  });

  it('aggregates per project with models, sessions (archived included) and a zero-filled daily series', () => {
    const { dir, db } = setup();
    const now = new Date(2026, 8, 24, 15, 0, 0).getTime();
    const yesterday = new Date(2026, 8, 23, 10, 0, 0).toISOString();
    const u = { input_tokens: 1, output_tokens: 10, cache_read_input_tokens: 100, cache_creation_input_tokens: 0 };
    writeFileSync(join(dir, 's1.jsonl'), line('a', u, { at: new Date(now).toISOString() }) + line('b', u, { at: yesterday, model: 'claude-sonnet-5' }));
    writeFileSync(join(dir, 's2.jsonl'), line('c', u, { at: yesterday }));
    ingestSessionUsage(db, 's1', join(dir, 's1.jsonl'));
    ingestSessionUsage(db, 's2', join(dir, 's2.jsonl'));

    expect(usageByProject(db).get('p')!.messages).toBe(3);
    const pu = projectUsage(db, 'p', 14, now);
    expect(pu.total.output).toBe(30);
    expect(pu.byModel.map((m) => m.model)).toEqual(['claude-opus-5-5', 'claude-sonnet-5']);
    expect(pu.bySession.map((s) => [s.sessionId, s.archived])).toEqual([['s1', false], ['s2', true]]);
    expect(pu.daily).toHaveLength(14);
    expect(pu.daily.at(-1)).toEqual({ day: '2026-09-24', usage: { input: 1, output: 10, cacheRead: 100, cacheWrite: 0, messages: 1 } });
    expect(pu.daily.at(-2)!.usage.messages).toBe(2);
    expect(pu.daily[0].usage.messages).toBe(0);
  });
});

describe('token formatting (web)', () => {
  it('abbreviates large counts', async () => {
    const { formatTokens } = await import('../../src/web/util.js');
    expect(formatTokens(950)).toBe('950');
    expect(formatTokens(1000)).toBe('1k');
    expect(formatTokens(12_300)).toBe('12.3k');
    expect(formatTokens(4_560_000)).toBe('4.56M');
    expect(formatTokens(250_000_000)).toBe('250M');
    expect(formatTokens(1_200_000_000)).toBe('1.2B');
  });
});
