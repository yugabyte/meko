import { mkdirSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { getPaths, homeIn, mekoDirOf, mekoHome, refuseOtherMeko } from '../../src/server/paths.js';
import { tempDir } from '../helpers.js';

/** Where Meko's data folder is: the cases Meko's own paths.Resolve and AgentK are tested against too. */
const CASES = (JSON.parse(readFileSync(join(__dirname, '../../../../backend/internal/paths/testdata/resolve-cases.json'), 'utf8')) as {
  cases: { name: string; env: Record<string, string>; networkDirs: string[]; want?: string; error?: boolean }[];
}).cases;

let root: string;
beforeEach(() => {
  root = tempDir('pres-home-');
});
afterEach(() => rmSync(root, { recursive: true, force: true }));

describe("Meko's data folder", () => {
  it.each(CASES)('$name', (c) => {
    const home = join(root, 'home');
    const sharedTmp = join(root, 'var-tmp');
    const fill = (p: string) => {
      const s = p.replace('$HOME', home).replace('$SHARED_TMP', sharedTmp).replace('$USER', 'me');
      return s.startsWith(root) || !s.startsWith('/') ? s : join(root, s);
    };
    const network = c.networkDirs.map(fill);
    for (const d of [home, sharedTmp, ...network]) mkdirSync(d, { recursive: true });
    const type = (dir: string) => (network.some((n) => dir === n || dir.startsWith(n + '/')) ? 'nfs' : 'ext2/ext3');
    const env = Object.fromEntries(Object.entries(c.env).map(([k, v]) => [k, fill(v)]));
    const run = () => mekoHome({ env, home, user: 'me', sharedTmp, type });
    if (c.error) expect(run).toThrow(/network filesystem/);
    else expect(run()).toBe(fill(c.want!));
  });
});

describe("Presenter's home", () => {
  it('is extensions/presenter in Meko\'s data folder', () => {
    expect(homeIn('/data/meko')).toBe('/data/meko/extensions/presenter');
    expect(mekoDirOf('/data/meko/extensions/presenter')).toBe('/data/meko');
    expect(mekoDirOf('/tmp/dev-home')).toBeNull();
  });

  it('keeps its socket short enough for a unix socket', () => {
    expect(getPaths(join(root, 'h')).webSock).toBe(join(root, 'h', 'run', 'web.sock'));
    expect(Buffer.byteLength(getPaths(join(root, 'x'.repeat(120))).webSock)).toBeLessThanOrEqual(100);
  });

  it('refuses a command Meko runs for another data folder', () => {
    const paths = getPaths(join(root, 'meko', 'extensions', 'presenter'));
    expect(() => refuseOtherMeko(paths, { MEKO_HOME: join(root, 'other') })).toThrow(/belongs to the Meko in/);
    expect(() => refuseOtherMeko(paths, { MEKO_HOME: join(root, 'meko') })).not.toThrow();
    expect(() => refuseOtherMeko(paths, {})).not.toThrow();
  });
});
