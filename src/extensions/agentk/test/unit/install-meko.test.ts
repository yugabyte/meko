import { chmodSync, mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it, vi } from 'vitest';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';
import { agentVersions, installMeko, settleAgents, type InstallMekoDeps } from '../../src/server/install.js';
import type { MekoCloudLink } from '../../src/server/mekolocal.js';
import { MekoSetupError } from '../../src/server/mekosetup.js';
import { tempDir } from '../helpers.js';

const LOCAL = { url: 'http://127.0.0.1:7778/mcp', token: 'local-token' };
const UNLINKED: MekoCloudLink = { account: '', webUrl: '' };
const LINKED: MekoCloudLink = { account: '3f1c09a4be7d5e21', webUrl: 'https://cloud.mekodata.ai' };

/**
 * The installer's Meko step with stand-ins, recording what it did. `local`: a Meko runs on this
 * computer; `cloud`: the link `meko token --json` reports.
 */
function deps(o: { setupError?: string; agents?: ('claude' | 'kiro')[]; local?: boolean; cloud?: MekoCloudLink } = {}) {
  const agents = o.agents ?? ['claude'];
  const said: string[] = [];
  const calls: string[] = [];
  const d: InstallMekoDeps = {
    ensureInClaude: !agents.includes('claude') ? undefined : async (url, key) => {
      calls.push(`claude ${url} ${key}`);
      if (o.setupError) throw new MekoSetupError(o.setupError);
    },
    ensureInKiro: !agents.includes('kiro') ? undefined : async (url, key) => {
      calls.push(`kiro ${url} ${key}`);
      if (o.setupError) throw new MekoSetupError(o.setupError);
    },
    probe: async () => ({ state: o.local ? 'running' : 'none', meko: o.local ? LOCAL : null, ...(o.cloud ? { cloud: o.cloud } : {}) }),
    ui: {
      ok: (l, t = '') => {
        said.push(`ok ${l} ${t}`);
        return true;
      },
      warn: (l, t = '') => {
        said.push(`warn ${l} ${t}`);
        return true;
      },
      section: () => true,
    },
  };
  return { d, said, calls };
}

const cfg = (): Config => structuredClone(DEFAULT_CONFIG);

describe('installer: the Meko on this computer', () => {
  it('asks for no key, and goes on without Meko when none runs on this computer', async () => {
    const { d, said, calls } = deps();
    expect(await installMeko({ noMeko: false }, d)).toBe(false);
    expect(calls).toEqual([]);
    expect(said).toEqual(['warn Meko not running on this computer: Kode uses it once it runs (meko start).']);
  });

  it.each([
    ['unlinked', UNLINKED],
    ['linked to Meko Cloud', LINKED],
  ])('uses the Meko on this computer, %s, the same way, and says nothing of Meko Cloud', async (_, cloud) => {
    const { d, said, calls } = deps({ local: true, cloud });
    expect(await installMeko({ noMeko: false }, d)).toBe(true);
    expect(calls).toEqual([`claude ${LOCAL.url} local-token`]);
    expect(said[0]).toBe('ok Meko on this computer (127.0.0.1:7778)');
    expect(said.join('\n')).not.toMatch(/Meko Cloud|linked/);
  });

  it("doesn't set Meko up for the agents with --no-meko, and says Meko can't be turned off", async () => {
    const { d, said, calls } = deps({ local: true, cloud: UNLINKED });
    expect(await installMeko({ noMeko: true }, d)).toBe(true);
    expect(calls).toEqual([]);
    expect(said[0]).toMatch(/^warn --no-meko Meko can't be turned off any more/);
  });

  it.each([
    ['only Claude Code', ['claude' as const], 'ok Meko in Claude'],
    ['only Kiro', ['kiro' as const], 'ok Meko in Kiro'],
  ])('sets Meko up in the agents that are installed: %s', async (_, agents, line) => {
    const { d, said } = deps({ agents, local: true, cloud: LINKED });
    expect(await installMeko({ noMeko: false }, d)).toBe(true);
    expect(said.filter((l) => l.startsWith('ok Meko in')).map((l) => l.slice(0, line.length))).toEqual([line]);
  });

  it("only warns when Meko's setup for Claude Code is declined", async () => {
    const { d, said } = deps({ local: true, cloud: LINKED, setupError: "Meko isn't set up for Claude Code. Run: npx …" });
    expect(await installMeko({ noMeko: false }, d)).toBe(true);
    expect(said.join('\n')).toMatch(/warn Meko in Claude Meko isn't set up for Claude Code\. Run: npx … Kode uses Meko/);
    expect(said.some((s) => s.startsWith('ok Meko in Claude'))).toBe(false);
  });
});

describe('installer: the coding agents', () => {
  it.each([
    ['keeps a default that is installed', { claude: '2.1.300', kiro: '2.32.0' }, 'kiro' as const, ['claude', 'kiro'], 'kiro'],
    ['starts sessions on Kiro where only Kiro is installed', { claude: null, kiro: '2.32.0' }, 'claude' as const, ['kiro'], 'kiro'],
    ['leaves out a Kiro too old to run sessions', { claude: '2.1.300', kiro: '2.10.0' }, 'kiro' as const, ['claude'], 'claude'],
  ])('%s', (_, versions, def, found, after) => {
    const c = cfg();
    c.agents.default = def;
    expect(settleAgents(c, versions)).toEqual(found);
    expect(c.agents.default).toBe(after);
  });

  it('stops, saying what to install, when no agent is installed and turned on', () => {
    const c = cfg();
    c.kiro.enabled = false;
    expect(() => settleAgents(c, { claude: null, kiro: '2.32.0' })).toThrow(/^no coding agent found\. Install Claude Code .* or Kiro CLI 2\.32\.0 or later/);
  });

  it("finds Claude Code in ~/.local/bin, where its installer puts it, when the terminal's PATH doesn't have it yet", async () => {
    const home = tempDir('ak-install-home-');
    const bin = join(home, '.local', 'bin');
    mkdirSync(bin, { recursive: true });
    writeFileSync(join(bin, 'claude'), '#!/bin/sh\necho "2.1.319 (Claude Code)"\n');
    chmodSync(join(bin, 'claude'), 0o755);
    vi.stubEnv('PATH', '/usr/bin:/bin');
    try {
      expect((await agentVersions(cfg(), home)).claude).toBe('2.1.319');
    } finally {
      vi.unstubAllEnvs();
    }
  });
});
