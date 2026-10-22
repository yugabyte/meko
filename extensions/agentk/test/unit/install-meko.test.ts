import { describe, expect, it } from 'vitest';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';
import { installMeko, settleAgents, type InstallMekoDeps } from '../../src/server/install.js';
import { MekoSetupError } from '../../src/server/mekosetup.js';

/** The installer's Meko step with scripted answers, recording what it said. */
function deps(o: { answers?: string[]; canAsk?: boolean; checkError?: string; setupError?: string; agents?: ('claude' | 'kiro')[]; local?: boolean } = {}) {
  const agents = o.agents ?? ['claude'];
  const said: string[] = [];
  const answers = [...(o.answers ?? [])];
  const calls: string[] = [];
  const checked: string[] = [];
  const d: InstallMekoDeps = {
    canAsk: o.canAsk ?? true,
    ask: async (q, def) => {
      calls.push(`ask ${q}`);
      return answers.length ? answers.shift()! : def;
    },
    check: async (url, key) => {
      calls.push('check');
      checked.push(`${url} ${key}`);
      return o.checkError ? { ok: false, error: o.checkError } : { ok: true };
    },
    ensureInClaude: !agents.includes('claude') ? undefined : async (_url, _key, keyFromEnv) => {
      calls.push(keyFromEnv ? 'claude (key from env)' : 'claude');
      if (o.setupError) throw new MekoSetupError(o.setupError);
    },
    ensureInKiro: !agents.includes('kiro') ? undefined : async () => {
      calls.push('kiro');
      if (o.setupError) throw new MekoSetupError(o.setupError);
    },
    localMeko: async () => {
      calls.push('local');
      return o.local ? { url: 'http://127.0.0.1:7778/mcp', token: 'local-token' } : null;
    },
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
  return { d, said, calls, checked };
}

const cfg = (): Config => structuredClone(DEFAULT_CONFIG);

describe('installer: Meko is optional', () => {
  it('goes on without Meko when the key prompt is skipped', async () => {
    const c = cfg();
    const { d, said, calls } = deps({ answers: [''] });
    expect(await installMeko(c, { noMeko: false }, d)).toBe(false);
    expect(calls).toEqual(['local', 'ask Meko API key (Enter to skip)']);
    expect(said.join('\n')).toMatch(/warn Meko off: no API key\. Projects work without datapacks/);
    expect(c.meko.apiKey).toBe('');
  });

  it('goes on with a warning when there is no key and no one to ask', async () => {
    const { d, said, calls } = deps({ canAsk: false });
    expect(await installMeko(cfg(), { noMeko: false }, d)).toBe(false);
    expect(calls).not.toContain('check');
    expect(said.some((s) => s.startsWith('warn Meko off'))).toBe(true);
  });

  it('turns Meko off with --no-meko, whatever key there is', async () => {
    const c = cfg();
    c.meko.apiKey = 'mko_tkn_x';
    const { d, calls } = deps();
    expect(await installMeko(c, { noMeko: true }, d)).toBe(false);
    expect(c.meko.enabled).toBe(false);
    expect(calls).toEqual([]);
  });

  it('keeps Meko off when it was turned off in Settings, unless a key is given now', async () => {
    const off = cfg();
    off.meko.enabled = false;
    off.meko.apiKey = 'mko_tkn_kept';
    const kept = deps({ answers: [''] });
    expect(await installMeko(off, { noMeko: false }, kept.d)).toBe(false);
    expect(off.meko.enabled).toBe(false);
    expect(kept.calls).toEqual(['ask Meko is turned off. API key to turn it on (Enter to keep it off)']);

    const quiet = deps({ canAsk: false });
    expect(await installMeko(off, { noMeko: false }, quiet.d)).toBe(false);
    expect(off.meko.enabled).toBe(false);

    const on = deps({ answers: ['mko_tkn_new'] });
    expect(await installMeko(off, { noMeko: false }, on.d)).toBe(true);
    expect(off.meko).toMatchObject({ enabled: true, apiKey: 'mko_tkn_new' });
  });

  it.each([
    ['asks first, and takes it', true, ['y'], ['local', 'ask Use Meko on this computer (127.0.0.1:7778)? (Y/n)', 'check', 'claude']],
    ['takes it with no one to ask', false, [], ['local', 'check', 'claude']],
  ])('offers the Meko running on this computer when there is no key: %s', async (_name, canAsk, answers, want) => {
    const c = cfg();
    const { d, calls, checked } = deps({ canAsk, answers, local: true });
    expect(await installMeko(c, { noMeko: false }, d)).toBe(true);
    expect(calls).toEqual(want);
    expect(checked).toEqual(['http://127.0.0.1:7778/mcp local-token']);
    expect(c.meko).toMatchObject({ enabled: true, url: 'http://127.0.0.1:7778/mcp', apiKey: 'local-token' });
  });

  it('goes on to the Meko Cloud key when the Meko on this computer is declined', async () => {
    const c = cfg();
    const { d, calls, checked } = deps({ answers: ['n', 'mko_tkn_cloud'], local: true });
    expect(await installMeko(c, { noMeko: false }, d)).toBe(true);
    expect(calls.slice(0, 3)).toEqual(['local', 'ask Use Meko on this computer (127.0.0.1:7778)? (Y/n)', 'ask Meko API key (Enter to skip)']);
    expect(checked).toEqual(['https://mcp.mekodata.ai/mcp mko_tkn_cloud']);
  });

  it('uses the URL and key it is given, and looks for no other Meko', async () => {
    const c = cfg();
    const { d, calls, checked } = deps({ canAsk: false, local: true });
    expect(await installMeko(c, { keyArg: 'tok', urlArg: 'http://127.0.0.1:7790/mcp', noMeko: false }, d)).toBe(true);
    expect(calls).toEqual(['check', 'claude']);
    expect(checked).toEqual(['http://127.0.0.1:7790/mcp tok']);
    expect(c.meko).toMatchObject({ url: 'http://127.0.0.1:7790/mcp', apiKey: 'tok' });
  });

  it('never sends a saved key to a URL given without one', async () => {
    const c = cfg();
    c.meko.apiKey = 'mko_tkn_saved';
    const { d, checked } = deps({ answers: ['other-key'] });
    expect(await installMeko(c, { urlArg: 'http://other.example/mcp', noMeko: false }, d)).toBe(true);
    expect(checked).toEqual(['http://other.example/mcp other-key']);
    const quiet = deps({ canAsk: false });
    const c2 = cfg();
    c2.meko.apiKey = 'mko_tkn_saved';
    expect(await installMeko(c2, { urlArg: 'http://other.example/mcp', noMeko: false }, quiet.d)).toBe(false);
    expect(quiet.checked).toEqual([]);
  });

  it('checks a key it is given and sets up Claude Code', async () => {
    const c = cfg();
    const { d, said, calls } = deps({ canAsk: false });
    expect(await installMeko(c, { keyArg: 'mko_tkn_good', noMeko: false }, d)).toBe(true);
    expect(calls).toEqual(['check', 'claude']);
    expect(c.meko).toMatchObject({ enabled: true, apiKey: 'mko_tkn_good' });
    expect(said.some((s) => s.startsWith('ok Meko in Claude'))).toBe(true);
  });

  it('stops on a key that fails with no one to ask, and offers to go on without Meko when someone is there', async () => {
    const off = deps({ canAsk: false, checkError: 'invalid token' });
    await expect(installMeko(cfg(), { keyArg: 'bad', noMeko: false }, off.d)).rejects.toThrow(/Meko check failed .*invalid token\. Fix the key, or pass --no-meko/);

    const c = cfg();
    const on = deps({ answers: ['y'], checkError: 'invalid token' });
    expect(await installMeko(c, { keyArg: 'bad', noMeko: false }, on.d)).toBe(false);
    expect(c.meko.enabled).toBe(false);
    expect(c.meko.apiKey).toBe('');

    const no = deps({ answers: ['n'], checkError: 'invalid token' });
    await expect(installMeko(cfg(), { keyArg: 'bad', noMeko: false }, no.d)).rejects.toThrow(/Run the installer again/);
  });

  it("tells Claude Code's check when the key came only from the installer's environment, and keeps it in config.toml without Claude Code", async () => {
    process.env.MEKO_API_KEY = 'mko_tkn_env';
    try {
      const fromEnv = deps({ canAsk: false });
      const withClaude = cfg();
      expect(await installMeko(withClaude, { noMeko: false }, fromEnv.d)).toBe(true);
      expect(fromEnv.calls).toEqual(['check', 'claude (key from env)']);
      expect(withClaude.meko.apiKey).toBe('');
      // Only Kiro: the service, which has no MEKO_* environment, reads it from config.toml.
      const kiroOnly = cfg();
      expect(await installMeko(kiroOnly, { noMeko: false }, deps({ canAsk: false, agents: ['kiro'] }).d)).toBe(true);
      expect(kiroOnly.meko.apiKey).toBe('mko_tkn_env');
      const typed = deps({ canAsk: false });
      expect(await installMeko(cfg(), { keyArg: 'mko_tkn_typed', noMeko: false }, typed.d)).toBe(true);
      expect(typed.calls).toEqual(['check', 'claude']);
    } finally {
      delete process.env.MEKO_API_KEY;
    }
  });

  it.each([
    ['only Claude Code', ['claude' as const], ['check', 'claude'], 'ok Meko in Claude'],
    ['only Kiro', ['kiro' as const], ['check', 'kiro'], 'ok Meko in Kiro'],
  ])("sets Meko up in the agents that are installed: %s", async (_, agents, expected, line) => {
    const { d, said, calls } = deps({ agents });
    expect(await installMeko(cfg(), { keyArg: 'mko_tkn_good', noMeko: false }, d)).toBe(true);
    expect(calls).toEqual(expected);
    expect(said.filter((l) => l.startsWith('ok Meko in')).map((l) => l.slice(0, line.length))).toEqual([line]);
  });

  it("only warns when Meko's setup for Claude Code is declined", async () => {
    const c = cfg();
    const { d, said } = deps({ canAsk: false, setupError: "Meko isn't set up for Claude Code. Run: npx …" });
    expect(await installMeko(c, { keyArg: 'mko_tkn_good', noMeko: false }, d)).toBe(true);
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
