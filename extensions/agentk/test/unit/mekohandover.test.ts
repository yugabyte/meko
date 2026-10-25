import { chmodSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { handOverMekoKey } from '../../src/server/mekohandover.js';
import { tempDir } from '../helpers.js';

describe('handOverMekoKey', () => {
  let home: string;
  beforeEach(() => {
    home = tempDir('akl-handover-');
    mkdirSync(join(home, '.local', 'bin'), { recursive: true });
  });
  afterEach(() => rmSync(home, { recursive: true, force: true }));

  /** A stand-in `meko` in ~/.local/bin that records its arguments, MEKO_HOME and stdin, then runs `tail`. */
  const meko = (tail: string) => {
    const f = join(home, '.local', 'bin', 'meko');
    writeFileSync(f, `#!/bin/sh\necho "$* home=$MEKO_HOME" > "${home}/args"\ncat > "${home}/stdin"\n${tail}\n`);
    chmodSync(f, 0o755);
  };
  const env = () => ({ HOME: home, PATH: '/usr/bin:/bin' });
  const link = { url: 'https://mcp.mekodata.ai/mcp', apiKey: 'mko_tkn_secret', webUrl: 'https://cloud.mekodata.ai', apiUrl: '' };

  it('runs meko cloud_link with the URLs as arguments and the key on stdin only', async () => {
    meko('exit 0');
    expect(await handOverMekoKey('/data/meko', link, env())).toEqual({ ok: true });
    expect(readFileSync(join(home, 'args'), 'utf8').trim()).toBe(
      'cloud_link --url https://mcp.mekodata.ai/mcp --web-url https://cloud.mekodata.ai home=/data/meko');
    expect(readFileSync(join(home, 'stdin'), 'utf8')).toBe('mko_tkn_secret\n');
  });

  it("passes on Meko's own reason when it refuses", async () => {
    meko(`echo "meko: Meko at https://mcp.mekodata.ai/mcp didn't accept that key: 401 Unauthorized. Nothing was saved" >&2; exit 1`);
    expect(await handOverMekoKey(null, link, env())).toEqual({
      ok: false, error: "Meko at https://mcp.mekodata.ai/mcp didn't accept that key: 401 Unauthorized. Nothing was saved",
    });
  });
});
