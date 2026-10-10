import { describe, expect, it } from 'vitest';
import { claudePath } from '../../src/server/paths.js';

/** A stat over a made-up file system: each path's ids, and every other path missing. */
function fakeStat(ids: Record<string, [number, number]>) {
  const calls: string[] = [];
  const stat = (p: string) => {
    calls.push(p);
    const id = ids[p];
    if (!id) throw Object.assign(new Error(`ENOENT: ${p}`), { code: 'ENOENT' });
    return { dev: id[0], ino: id[1] };
  };
  return { stat, calls };
}

describe('claudePath', () => {
  it('drops the /net/<host> prefix when the folder is this machine’s own, keeping the rest even before it exists', () => {
    const { stat } = fakeStat({ '/net/box/data': [7, 100], '/data': [7, 100] });
    expect(claudePath('/net/box/data/me/.agent-k-local/worktrees/repo.1a2b3c4d/wt-1', stat, new Map(), 'box'))
      .toBe('/data/me/.agent-k-local/worktrees/repo.1a2b3c4d/wt-1');
    expect(claudePath('/net/box/data', stat, new Map(), 'box')).toBe('/data');
  });

  it('finds an export root deeper than one folder', () => {
    const { stat } = fakeStat({ '/net/box/home': [1, 2], '/home': [7, 2], '/net/box/home/me': [7, 55], '/home/me': [7, 55] });
    expect(claudePath('/net/box/home/me/code/x', stat, new Map(), 'box')).toBe('/home/me/code/x');
  });

  it('keeps the path when the local folder is another one, or is missing', () => {
    const { stat } = fakeStat({ '/net/box/data': [40, 100], '/data': [7, 100], '/net/box/only': [7, 3] });
    expect(claudePath('/net/box/data/x', stat, new Map(), 'box')).toBe('/net/box/data/x');
    expect(claudePath('/net/box/only/x', stat, new Map(), 'box')).toBe('/net/box/only/x');
  });

  it("never looks into another host's mount (a stat there can hang while its server is down)", () => {
    const { stat, calls } = fakeStat({ '/net/other/data': [7, 100], '/data': [7, 100] });
    expect(claudePath('/net/other/data/x', stat, new Map(), 'box')).toBe('/net/other/data/x');
    expect(calls).toEqual([]);
    // The host name may be fully qualified on either side.
    const { stat: stat2 } = fakeStat({ '/net/box.example.com/data': [7, 100], '/data': [7, 100] });
    expect(claudePath('/net/box.example.com/data/x', stat2, new Map(), 'Box')).toBe('/data/x');
    expect(claudePath('/net/box/data/x', fakeStat({ '/net/box/data': [7, 1], '/data': [7, 1] }).stat, new Map(), 'box.example.com')).toBe('/data/x');
  });

  it('leaves paths outside /net, and the bare map and host folders, alone without a stat', () => {
    const { stat, calls } = fakeStat({});
    for (const p of ['/home/me/x', '/network/box/x', 'net/box/x', '/net', '/net/box', '/net/box/']) {
      expect(claudePath(p, stat, new Map(), 'box')).toBe(p);
    }
    expect(calls).toEqual([]);
  });

  it('checks each prefix once', () => {
    const { stat, calls } = fakeStat({ '/net/box/data': [7, 100], '/data': [7, 100] });
    const cache = new Map<string, string | null>();
    claudePath('/net/box/data/a', stat, cache, 'box');
    claudePath('/net/box/data/b/c', stat, cache, 'box');
    expect(calls).toEqual(['/net/box/data', '/data']);
  });
});
