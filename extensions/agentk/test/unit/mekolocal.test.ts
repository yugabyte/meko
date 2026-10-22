import { chmodSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { findLocalMeko } from '../../src/server/mekolocal.js';
import { tempDir } from '../helpers.js';

describe('findLocalMeko', () => {
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
  const find = () => findLocalMeko({ HOME: home, PATH: '/usr/bin:/bin' });

  it('reads the URL and token of a running Meko', async () => {
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":true}'`);
    expect(await find()).toEqual({ url: 'http://127.0.0.1:7778/mcp', token: 'abc' });
  });

  it.each([
    ['no meko at all', null],
    ['no server running', `echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":false}'`],
    ['a failure', 'exit 3'],
    ['an answer that is not JSON', 'echo nope'],
    ['no token', `echo '{"url":"http://127.0.0.1:7778/mcp","token":"","running":true}'`],
  ])('finds none with %s', async (_name, script) => {
    if (script) meko(script);
    expect(await find()).toBeNull();
  });
});
