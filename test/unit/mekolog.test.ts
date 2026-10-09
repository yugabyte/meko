import { appendFileSync, existsSync, readFileSync, rmSync, statSync } from 'node:fs';
import { dirname } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { silentLogger } from '../../src/server/log.js';
import type { MekoTrace } from '../../src/server/meko.js';
import { MekoCallLog, mekoLogPath, removeMekoLog } from '../../src/server/mekolog.js';
import { tempDir } from '../helpers.js';

function trace(over: Partial<MekoTrace> = {}): MekoTrace {
  return {
    tool: 'context_search', at: Date.parse('2026-10-09T18:20:01.123Z'), ms: 412, ok: true, attempts: 1,
    args: { query: 'q', datapack_id: 'd1' }, text: '{"results":{"memory":[]}}', error: null, tag: null, sessionId: 's1', ...over,
  };
}

describe('Meko call log', () => {
  let designs: string;
  let on: Set<string>;
  let log: MekoCallLog;
  beforeEach(() => {
    designs = tempDir('ak-mekolog-');
    on = new Set(['s1']);
    log = new MekoCallLog({ designs, enabled: (id) => on.has(id), log: silentLogger });
  });
  afterEach(() => rmSync(designs, { recursive: true, force: true }));

  it("writes a session's calls whole, one JSON line each, readable only by the owner", () => {
    const big = 'x'.repeat(100_000);
    log.record(trace({ args: { text: big, content_base64: 'aGVsbG8=' }, text: JSON.stringify({ memory: big }), tag: 'L1' }));
    log.record(trace({ tool: 'memory_add', ok: false, attempts: 2, text: 'not json', error: 'boom' }));
    log.record(trace({ tool: 'memory_update', ok: false, text: null, error: 'timed out' }));
    const file = mekoLogPath(designs, 's1');
    expect(statSync(file).mode & 0o777).toBe(0o600);
    const lines = readFileSync(file, 'utf8').trimEnd().split('\n').map((l) => JSON.parse(l));
    expect(lines).toEqual([
      {
        id: expect.stringMatching(/^[0-9a-f]{8}$/), at: '2026-10-09T18:20:01.123Z', tool: 'context_search', learn: 'L1', ms: 412, ok: true,
        attempts: 1, args: { text: big, content_base64: 'aGVsbG8=' }, result: { memory: big },
      },
      expect.objectContaining({ tool: 'memory_add', ok: false, attempts: 2, result: 'not json', error: 'boom' }),
      expect.objectContaining({ tool: 'memory_update', result: null, error: 'timed out' }),
    ]);
  });

  it('keeps nothing for a call with no session, or one whose project has the log off', () => {
    log.record(trace({ sessionId: null }));
    log.record(trace({ sessionId: 's2' }));
    expect(existsSync(mekoLogPath(designs, 's2'))).toBe(false);
    expect(log.page('s2')).toEqual({ calls: [], more: false });
    // A check that throws (the database closed) is logged, never thrown.
    const broken = new MekoCallLog({ designs, enabled: () => { throw new Error('closed'); }, log: silentLogger });
    expect(() => broken.record(trace())).not.toThrow();
  });

  it('pages back from the end, across chunk edges and lines longer than a chunk, with the same calls as a full read', () => {
    for (let i = 0; i < 230; i++) {
      // Every 7th call is long enough to span a 64 KB chunk; the sizes vary so lines split anywhere.
      const pad = i % 7 === 0 ? 'y'.repeat(70_000 + i) : 'z'.repeat(i * 37);
      log.record(trace({ tool: i % 3 ? 'context_search' : 'memory_add', ok: i % 5 !== 0, args: { i, pad } }));
    }
    const all = readFileSync(mekoLogPath(designs, 's1'), 'utf8').trimEnd().split('\n').map((l) => JSON.parse(l) as { id: string; tool: string; ok: boolean });
    const read = (q: { tool?: string; failed?: boolean; limit?: number }) => {
      const ids: string[] = [];
      let before: number | undefined;
      for (;;) {
        const p = log.page('s1', { ...q, before });
        ids.push(...p.calls.map((c) => c.id));
        if (!p.more) return ids;
        before = p.calls.at(-1)!.offset;
      }
    };
    expect(read({})).toEqual(all.map((l) => l.id).reverse());
    expect(read({ limit: 7 })).toEqual(all.map((l) => l.id).reverse());
    expect(read({ tool: 'memory_add', limit: 10 })).toEqual(all.filter((l) => l.tool === 'memory_add').map((l) => l.id).reverse());
    expect(read({ failed: true })).toEqual(all.filter((l) => !l.ok).map((l) => l.id).reverse());
    const first = log.page('s1', { limit: 3 });
    expect(first.more).toBe(true);
    expect(first.calls).toHaveLength(3);
    expect(first.calls[0]).toEqual({
      id: all.at(-1)!.id, offset: expect.any(Number), at: Date.parse('2026-10-09T18:20:01.123Z'), tool: all.at(-1)!.tool, learn: null,
      ms: 412, ok: all.at(-1)!.ok, attempts: 1, error: null,
    });
  });

  it('opens one call by its offset and id, and skips a line a crash cut short', () => {
    log.record(trace({ args: { n: 1 } }));
    log.record(trace({ tool: 'memory_add', args: { n: 2, long: 'w'.repeat(200_000) }, text: '{"ok":true}' }));
    appendFileSync(mekoLogPath(designs, 's1'), '{"id":"cut","tool":"memo');
    const { calls } = log.page('s1');
    expect(calls.map((c) => c.tool)).toEqual(['memory_add', 'context_search']);
    const d = log.detail('s1', calls[0].offset, calls[0].id);
    expect(d).toEqual({ ...calls[0], args: { n: 2, long: 'w'.repeat(200_000) }, result: { ok: true } });
    expect(log.detail('s1', calls[1].offset, calls[1].id)?.args).toEqual({ n: 1 });
    expect(log.detail('s1', calls[0].offset, 'other')).toBeNull();
    expect(log.detail('s1', calls[0].offset + 1, calls[0].id)).toBeNull();
    expect(log.detail('s1', -1, calls[0].id)).toBeNull();
    expect(log.detail('s2', 0, calls[0].id)).toBeNull();
  });

  it('says how big a log is, and clears or removes it', () => {
    expect(log.stat('s1')).toBeNull();
    log.record(trace());
    expect(log.stat('s1')).toEqual({ bytes: statSync(mekoLogPath(designs, 's1')).size, updatedAt: expect.any(Number) });
    log.clear('s1');
    expect(log.stat('s1')).toBeNull();
    log.record(trace());
    removeMekoLog(designs, 's1');
    expect(existsSync(mekoLogPath(designs, 's1'))).toBe(false);
    // The session's folder (its design) stays.
    expect(existsSync(dirname(mekoLogPath(designs, 's1')))).toBe(true);
  });
});
