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

  it('reads the URLs and token of a running Meko, and whether it has the Calls page, asking the Meko whose folder it is given', async () => {
    // Only the Meko in /data/meko names its web listener and has the Calls page: the other is too old for either.
    meko(`w=''; [ -n "$MEKO_HOME" ] && w=',"web_url":"https://box:7777","features":["calls"]'
echo "{\\"url\\":\\"http://127.0.0.1:7778/mcp\\",\\"token\\":\\"tok-$MEKO_HOME\\",\\"running\\":true$w}"`);
    expect(await probe('/data/meko')).toEqual({
      state: 'running', meko: { url: 'http://127.0.0.1:7778/mcp', token: 'tok-/data/meko', webUrl: 'https://box:7777', calls: true },
    });
    expect(await probe()).toEqual({ state: 'running', meko: { url: 'http://127.0.0.1:7778/mcp', token: 'tok-', webUrl: null } });
  });

  it('says when the web UI is off (meko ui stop), with no URL to open', async () => {
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":true,"web_url":"","web_off":true}'`);
    expect(await probe()).toEqual({ state: 'running', meko: { url: 'http://127.0.0.1:7778/mcp', token: 'abc', webUrl: null, webOff: true } });
  });

  it("reads Meko's Meko Cloud link, running or not, and none from a Meko too old to keep one", async () => {
    const link = '"cloud":{"set":true,"url":"https://mcp.mekodata.ai/mcp","api_key":"mko_tkn_x","web_url":"https://cloud.mekodata.ai","api_url":"https://cloud.mekodata.ai/api/v1"}';
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":false,${link}}'`);
    expect(await probe()).toEqual({
      state: 'stopped', meko: null,
      cloud: { set: true, url: 'https://mcp.mekodata.ai/mcp', apiKey: 'mko_tkn_x', webUrl: 'https://cloud.mekodata.ai', apiUrl: 'https://cloud.mekodata.ai/api/v1' },
    });
    // Unlinked: no key, so no URLs either.
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":true,"cloud":{"set":true,"url":"https://x/mcp","api_key":""}}'`);
    expect((await probe()).cloud).toEqual({ set: true, url: '', apiKey: '', webUrl: '', apiUrl: '' });
    // A Meko that keeps the key says only whether it's linked, and its web app while it is.
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":true,"cloud":{"set":true,"linked":true,"web_url":"https://cloud.mekodata.ai","account":"0123456789abcdef"}}'`);
    expect((await probe()).cloud).toEqual({ set: true, url: '', apiKey: '', webUrl: 'https://cloud.mekodata.ai', apiUrl: '', linked: true, account: '0123456789abcdef' });
    // One whose /api/v1 routes datapacks by id says so.
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":true,"cloud":{"set":true,"linked":true,"web_url":"https://cloud.mekodata.ai","account":"0123456789abcdef","datapack_routing":true}}'`);
    expect((await probe()).cloud).toMatchObject({ linked: true, routed: true });
    meko(`echo '{"url":"http://127.0.0.1:7778/mcp","token":"abc","running":true,"cloud":{"set":true,"linked":false,"web_url":"https://cloud.mekodata.ai"}}'`);
    expect((await probe()).cloud).toEqual({ set: true, url: '', apiKey: '', webUrl: '', apiUrl: '', linked: false, account: '' });
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
    ['/home/u/.meko/extensions/agentk', '/home/u/.meko'],
    ['/home/u/.agent-k-local', null],
    ['/data/extensions/agentk-dev', null],
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
