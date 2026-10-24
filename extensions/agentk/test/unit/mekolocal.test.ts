import { chmodSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { probeLocalMeko } from '../../src/server/mekolocal.js';
import { mekoDirOf } from '../../src/server/movehome.js';
import { tempDir } from '../helpers.js';

describe('probeLocalMeko', () => {
  let home: string;
  beforeEach(() => {
    home = tempDir('akl-mekolocal-');
    mkdirSync(join(home, '.local', 'bin'), { recursive: true });
  });
  afterEach(() => rmSync(home, { recursive: true, force: true }));

  /** A stand-in `meko` in ~/.local/bin (off PATH, where Meko's installer links it). */
  const meko = (script: string) => {
    const f = join(home, '.local', 'bin', 'meko');
    writeFileSync(f, `#!/bin/sh\n[ "$1 $2" = "token --json" ] || exit 9\n${script}\n`);
    chmodSync(f, 0o755);
  };
  const probe = (mekoHome: string | null = null) => probeLocalMeko(mekoHome, { HOME: home, PATH: '/usr/bin:/bin' });

  it('reads the URL and token of a running Meko, asking the Meko whose folder it is given', async () => {
    meko(`echo "{\\"url\\":\\"http://127.0.0.1:7778/mcp\\",\\"token\\":\\"tok-$MEKO_HOME\\",\\"running\\":true}"`);
    expect(await probe('/data/meko')).toEqual({ state: 'running', meko: { url: 'http://127.0.0.1:7778/mcp', token: 'tok-/data/meko' } });
    expect(await probe()).toEqual({ state: 'running', meko: { url: 'http://127.0.0.1:7778/mcp', token: 'tok-' } });
  });

  it.each([
    ['no meko at all', null, 'none'],
    ['no server running', `echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":false}'`, 'stopped'],
    ['a failure', 'exit 3', 'none'],
    ['an answer that is not JSON', 'echo nope', 'none'],
    ['no token', `echo '{"url":"http://127.0.0.1:7778/mcp","token":"","running":true}'`, 'none'],
  ])('finds none with %s', async (_name, script, state) => {
    if (script) meko(script);
    expect(await probe()).toEqual({ state, meko: null });
  });
});

describe('mekoDirOf', () => {
  it.each([
    ['/home/u/.meko/extensions/agentk', '/home/u/.meko'],
    ['/home/u/.agent-k-local', null],
    ['/data/extensions/agentk-dev', null],
  ])('%s → %s', (akHome, want) => {
    expect(mekoDirOf(akHome)).toBe(want);
  });
});
