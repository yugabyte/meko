import { describe, expect, it } from 'vitest';
import { DEFAULT_CONFIG, DEFAULT_MEKO_URL, type Config } from '../../src/server/config.js';
import { installMeko, settleAgents, type InstallMekoDeps } from '../../src/server/install.js';
import type { MekoCloudLink } from '../../src/server/mekolocal.js';
import { MekoSetupError } from '../../src/server/mekosetup.js';

const LOCAL = { url: 'http://127.0.0.1:7778/mcp', token: 'local-token' };
const UNSET: MekoCloudLink = { set: false, url: '', apiKey: '', webUrl: '', apiUrl: '' };
const LINKED: MekoCloudLink = { set: true, url: DEFAULT_MEKO_URL, apiKey: 'mko_tkn_linked', webUrl: 'https://cloud.mekodata.ai', apiUrl: 'https://cloud.mekodata.ai/api/v1' };

/**
 * The installer's Meko step with scripted answers, recording what it did. `cloud` is the link
 * `meko token --json` reports (undefined: a Meko too old to keep one); `linkError` refuses a
 * `meko cloud_link`.
 */
function deps(o: {
  answers?: string[]; canAsk?: boolean; checkError?: string; linkError?: string; setupError?: string; agents?: ('claude' | 'kiro')[];
  local?: boolean; cloud?: MekoCloudLink;
} = {}) {
  const agents = o.agents ?? ['claude'];
  const said: string[] = [];
  const answers = [...(o.answers ?? [])];
  const calls: string[] = [];
  const d: InstallMekoDeps = {
    canAsk: o.canAsk ?? true,
    ask: async (q, def) => {
      calls.push(`ask ${q}`);
      return answers.length ? answers.shift()! : def;
    },
    check: async (url, key) => {
      calls.push(`check ${url} ${key}`);
      return o.checkError ? { ok: false, error: o.checkError } : { ok: true };
    },
    link: async (l) => {
      calls.push(`link ${l.url} ${l.apiKey}${l.webUrl ? ` ${l.webUrl}` : ''}`);
      return o.linkError ? { ok: false, error: o.linkError } : { ok: true };
    },
    ensureInClaude: !agents.includes('claude') ? undefined : async (url, key, keyFromEnv, local) => {
      calls.push(`claude ${url} ${key}${keyFromEnv ? ' (key from env)' : ''}${local ? ' (local)' : ''}`);
      if (o.setupError) throw new MekoSetupError(o.setupError);
    },
    ensureInKiro: !agents.includes('kiro') ? undefined : async (url, key, local) => {
      calls.push(`kiro ${url} ${key}${local ? ' (local)' : ''}`);
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
      note: (t) => {
        said.push(`note ${t}`);
        return true;
      },
      section: () => true,
    },
  };
  return { d, said, calls };
}

const cfg = (): Config => structuredClone(DEFAULT_CONFIG);

describe("installer: Meko's Meko Cloud link, or the Meko on this computer", () => {
  it('asks for no key, and goes on without Meko when there is no link and no Meko on this computer', async () => {
    const { d, said, calls } = deps({ cloud: UNSET });
    expect(await installMeko(cfg(), { noMeko: false }, d)).toBe(false);
    expect(calls).toEqual([]);
    expect(said.join('\n')).toMatch(/warn Meko not set up: no Meko Cloud link, and no Meko running on this computer.*meko cloud_link/);
  });

  it.each([
    ['the Meko on this computer', { local: true, cloud: UNSET }, `claude ${LOCAL.url} local-token (local)`, /ok Meko on this computer \(127\.0\.0\.1:7778\)/],
    ["Meko's link", { local: true, cloud: LINKED }, `claude ${DEFAULT_MEKO_URL} mko_tkn_linked`, /ok Meko Meko Cloud, linked in Meko \(mcp\.mekodata\.ai\)/],
  ])('uses %s and stores nothing', async (_, o, claude, line) => {
    const c = cfg();
    const { d, said, calls } = deps(o);
    expect(await installMeko(c, { noMeko: false }, d)).toBe(true);
    expect(calls).toEqual([claude]);
    expect(said.join('\n')).toMatch(line);
    expect(c.meko).toEqual(DEFAULT_CONFIG.meko);
  });

  it('hands a key it is given to Meko, and takes a copy of the local Meko\'s token for the local Meko', async () => {
    const c = cfg();
    c.meko.apiKey = 'mko_tkn_old';
    const given = deps({ local: true, cloud: UNSET });
    expect(await installMeko(c, { keyArg: 'mko_tkn_given', urlArg: 'https://meko.example.com/mcp', noMeko: false }, given.d)).toBe(true);
    expect(given.calls).toEqual(['link https://meko.example.com/mcp mko_tkn_given', 'claude https://meko.example.com/mcp mko_tkn_given']);
    // The given key isn't stored, and the old one stays for a rollback.
    expect(c.meko.apiKey).toBe('mko_tkn_old');
    // What `meko install_extension agentk` passes.
    const copy = deps({ local: true, cloud: UNSET });
    expect(await installMeko(cfg(), { keyArg: 'local-token', urlArg: LOCAL.url, noMeko: false }, copy.d)).toBe(true);
    expect(copy.calls).toEqual([`claude ${LOCAL.url} local-token (local)`]);
    expect(copy.said.some((l) => l.startsWith('warn --meko-key'))).toBe(false);
    // Any other key for a server on this computer is set aside, saying so.
    const loopback = deps({ local: true, cloud: UNSET });
    expect(await installMeko(cfg(), { keyArg: 'mko_tkn_x', urlArg: 'http://127.0.0.1:9000/mcp', noMeko: false }, loopback.d)).toBe(true);
    expect(loopback.calls).toEqual([`claude ${LOCAL.url} local-token (local)`]);
    expect(loopback.said.join('\n')).toMatch(/warn --meko-key not linked: 127\.0\.0\.1:9000 is on this computer/);
  });

  it("hands AgentK's own key to a Meko that never had a link, and keeps it when Meko won't take it", async () => {
    const c = cfg();
    c.meko.apiKey = 'mko_tkn_own';
    c.meko.webUrl = 'https://cloud.mekodata.ai';
    const ok = deps({ local: true, cloud: UNSET });
    expect(await installMeko(c, { noMeko: false }, ok.d)).toBe(true);
    expect(ok.calls).toEqual([`link ${DEFAULT_MEKO_URL} mko_tkn_own https://cloud.mekodata.ai`, `claude ${DEFAULT_MEKO_URL} mko_tkn_own`]);
    // Left in config.toml, for a rollback to the release before; the server never reads it again.
    expect(c.meko).toMatchObject({ apiKey: 'mko_tkn_own', webUrl: 'https://cloud.mekodata.ai' });

    const kept = cfg();
    kept.meko.apiKey = 'mko_tkn_own';
    const refused = deps({ local: true, cloud: UNSET, linkError: '401 Unauthorized', canAsk: false });
    expect(await installMeko(kept, { noMeko: false }, refused.d)).toBe(true);
    expect(refused.calls).toEqual([`link ${DEFAULT_MEKO_URL} mko_tkn_own`, `claude ${DEFAULT_MEKO_URL} mko_tkn_own`]);
    expect(refused.said.join('\n')).toMatch(/warn Meko Cloud Meko didn't take AgentK's key \(401 Unauthorized\)/);
    expect(kept.meko.apiKey).toBe('mko_tkn_own');

    // Once Meko has had a link (unlinked since), AgentK's own key isn't offered.
    const unlinked = deps({ local: true, cloud: { ...UNSET, set: true } });
    expect(await installMeko(kept, { noMeko: false }, unlinked.d)).toBe(true);
    expect(unlinked.calls).toEqual([`claude ${LOCAL.url} local-token (local)`]);
  });

  it("hands Meko a Meko Cloud key found next to the local Meko's URL, with Meko Cloud's URL", async () => {
    // What a setup run against the local Meko without a key leaves: its URL next to the Meko Cloud key.
    const c = cfg();
    c.meko.url = LOCAL.url;
    c.meko.apiKey = 'mko_tkn_own';
    const { d, calls } = deps({ local: true, cloud: UNSET });
    expect(await installMeko(c, { noMeko: false }, d)).toBe(true);
    expect(calls).toEqual([`link ${DEFAULT_MEKO_URL} mko_tkn_own`, `claude ${DEFAULT_MEKO_URL} mko_tkn_own`]);
    // Not dropped as a copy of the local Meko's token.
    expect(c.meko).toMatchObject({ url: LOCAL.url, apiKey: 'mko_tkn_own' });
  });

  it('stops on a given key Meko refuses with no one to ask, and goes on without it when someone says so', async () => {
    const opts = { keyArg: 'mko_tkn_bad', noMeko: false };
    await expect(installMeko(cfg(), opts, deps({ canAsk: false, local: true, cloud: UNSET, linkError: '401' }).d)).rejects.toThrow(/Meko didn't link .*: 401. Fix the key/);
    const there = deps({ local: true, cloud: UNSET, linkError: '401', answers: ['y'] });
    expect(await installMeko(cfg(), opts, there.d)).toBe(true);
    expect(there.calls).toEqual([`link ${DEFAULT_MEKO_URL} mko_tkn_bad`, 'ask Continue without it? (y/N)', `claude ${LOCAL.url} local-token (local)`]);
    // --no-meko: no stop, and the agents aren't set up.
    const noMeko = deps({ canAsk: false, local: true, cloud: UNSET, linkError: '401' });
    expect(await installMeko(cfg(), { ...opts, noMeko: true }, noMeko.d)).toBe(true);
    expect(noMeko.calls).toEqual([`link ${DEFAULT_MEKO_URL} mko_tkn_bad`]);
  });

  it('keeps a key itself, checked, under a Meko too old to keep the link', async () => {
    const c = cfg();
    const given = deps({ local: true });
    expect(await installMeko(c, { keyArg: 'mko_tkn_given', noMeko: false }, given.d)).toBe(true);
    expect(given.calls).toEqual([`check ${DEFAULT_MEKO_URL} mko_tkn_given`, `claude ${DEFAULT_MEKO_URL} mko_tkn_given`]);
    expect(c.meko.apiKey).toBe('mko_tkn_given');
    await expect(installMeko(cfg(), { keyArg: 'mko_tkn_bad', noMeko: false }, deps({ canAsk: false, checkError: 'invalid token' }).d))
      .rejects.toThrow(/Meko check failed .*: invalid token/);
    process.env.MEKO_API_KEY = 'mko_tkn_env';
    try {
      const fromEnv = deps({ canAsk: false });
      const withClaude = cfg();
      expect(await installMeko(withClaude, { noMeko: false }, fromEnv.d)).toBe(true);
      expect(fromEnv.calls).toEqual([`check ${DEFAULT_MEKO_URL} mko_tkn_env`, `claude ${DEFAULT_MEKO_URL} mko_tkn_env (key from env)`]);
      expect(withClaude.meko.apiKey).toBe('');
      // Only Kiro: nothing puts it in Claude Code's settings, and the service has no MEKO_*
      // environment, so config.toml keeps it. The same when Meko refuses the handover.
      const kiroOnly = cfg();
      expect(await installMeko(kiroOnly, { noMeko: false }, deps({ canAsk: false, agents: ['kiro'] }).d)).toBe(true);
      expect(kiroOnly.meko.apiKey).toBe('mko_tkn_env');
      const refusedKiroOnly = cfg();
      const refusedDeps = deps({ canAsk: false, agents: ['kiro'], local: true, cloud: UNSET, linkError: '401' });
      expect(await installMeko(refusedKiroOnly, { noMeko: false }, refusedDeps.d)).toBe(true);
      expect(refusedKiroOnly.meko.apiKey).toBe('mko_tkn_env');
    } finally {
      delete process.env.MEKO_API_KEY;
    }
  });

  it.each([
    ['only Claude Code', ['claude' as const], 'ok Meko in Claude'],
    ['only Kiro', ['kiro' as const], 'ok Meko in Kiro'],
  ])('sets Meko up in the agents that are installed: %s', async (_, agents, line) => {
    const { d, said } = deps({ agents, local: true, cloud: LINKED });
    expect(await installMeko(cfg(), { noMeko: false }, d)).toBe(true);
    expect(said.filter((l) => l.startsWith('ok Meko in')).map((l) => l.slice(0, line.length))).toEqual([line]);
  });

  it("only warns when Meko's setup for Claude Code is declined", async () => {
    const { d, said } = deps({ canAsk: false, local: true, cloud: LINKED, setupError: "Meko isn't set up for Claude Code. Run: npx …" });
    expect(await installMeko(cfg(), { noMeko: false }, d)).toBe(true);
    expect(said.join('\n')).toMatch(/warn Meko in Claude Meko isn't set up for Claude Code\. Run: npx … AgentK uses Meko/);
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
});
