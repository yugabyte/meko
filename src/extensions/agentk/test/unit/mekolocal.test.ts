import { chmodSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { lookForLocalMeko, probeLocalMeko } from '../../src/server/mekolocal.js';
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

  it('reads the URLs and token of a running Meko, asking the Meko whose folder it is given', async () => {
    // Only the Meko in /data/meko names its web listener: the other is too old to serve Kode.
    meko(`w=''; [ -n "$MEKO_HOME" ] && w=',"web_url":"https://box:7777"'
echo "{\\"url\\":\\"http://127.0.0.1:7778/mcp\\",\\"token\\":\\"tok-$MEKO_HOME\\",\\"running\\":true$w}"`);
    expect(await probe('/data/meko')).toEqual({
      state: 'running', meko: { url: 'http://127.0.0.1:7778/mcp', token: 'tok-/data/meko', webUrl: 'https://box:7777' },
    });
    expect(await probe()).toEqual({ state: 'running', meko: { url: 'http://127.0.0.1:7778/mcp', token: 'tok-', webUrl: null } });
  });

  it('says when the web UI is off (meko ui stop), with no URL to open', async () => {
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":true,"web_url":"","web_off":true}'`);
    expect(await probe()).toEqual({ state: 'running', meko: { url: 'http://127.0.0.1:7778/mcp', token: 'abc', webUrl: null, webOff: true } });
  });

  it("reads the account and web app of Meko's Meko Cloud link, running or not, and none from a Meko too old to keep one", async () => {
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":false,"cloud":{"set":true,"linked":true,"web_url":"https://cloud.mekodata.ai","account":"0123456789abcdef"}}'`);
    expect(await probe()).toEqual({ state: 'stopped', meko: null, cloud: { account: '0123456789abcdef', webUrl: 'https://cloud.mekodata.ai' } });
    // Unlinked: no account and no web app, whatever else it says.
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":true,"cloud":{"set":true,"linked":false,"web_url":"https://cloud.mekodata.ai","account":"x"}}'`);
    expect((await probe()).cloud).toEqual({ account: '', webUrl: '' });
    // An older Meko that handed out the link's key, which Kode no longer uses: no account of its own.
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":true,"cloud":{"set":true,"url":"https://mcp.mekodata.ai/mcp","api_key":"mko_tkn_x","web_url":"https://cloud.mekodata.ai","api_url":"https://cloud.mekodata.ai/api/v1"}}'`);
    expect((await probe()).cloud).toEqual({ account: '', webUrl: '' });
    // A web app that isn't http(s) is none.
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":true,"cloud":{"set":true,"linked":true,"web_url":"file:///x","account":"a"}}'`);
    expect((await probe()).cloud).toEqual({ account: 'a', webUrl: '' });
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":true,"cloud":"what"}'`);
    expect(await probe()).not.toHaveProperty('cloud');
    // One Meko couldn't read says so, for the reader to keep the link it had.
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":true,"cloud":{"set":false,"url":"","api_key":"","web_url":"","api_url":"","error":"cloud.toml: bad"}}'`);
    expect(await probe()).toEqual({ state: 'running', meko: { url: 'http://127.0.0.1:7778/mcp', token: 'abc', webUrl: null }, cloudUnreadable: true });
  });

  it.each([
    ['no meko at all', null, 'none'],
    ['no server running', `echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":false}'`, 'stopped'],
    ['a failure (meko there, but refusing)', 'exit 3', 'failed'],
    ['an answer that is not JSON', 'echo nope', 'failed'],
    ['no token', `echo '{"url":"http://127.0.0.1:7778/mcp","token":"","running":true}'`, 'failed'],
  ])('finds no running Meko with %s', async (_name, script, state) => {
    if (script) meko(script);
    expect(await probe()).toEqual({ state, meko: null });
  });
});

describe('mekoDirOf', () => {
  it.each([
    ['/home/u/.meko/extensions/kode', '/home/u/.meko'],
    ['/home/u/.agent-k-local', null],
    ['/data/extensions/kode-dev', null],
  ])('%s → %s', (akHome, want) => {
    expect(mekoDirOf(akHome)).toBe(want);
  });
});

describe('lookForLocalMeko', () => {
  it('looks every minute while there is no Meko, and with a key only until `meko` is found', () => {
    expect(['running', 'stopped', 'none', 'failed'].map((s) => lookForLocalMeko(false, s as 'none'))).toEqual([true, true, true, true]);
    expect(lookForLocalMeko(true, 'none')).toBe(true);
    expect(['running', 'stopped', 'failed'].map((s) => lookForLocalMeko(true, s as 'none'))).toEqual([false, false, false]);
  });
});
