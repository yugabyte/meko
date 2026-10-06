import { describe, expect, it } from 'vitest';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';
import { installMeko, type InstallMekoDeps } from '../../src/server/install.js';
import { MekoSetupError } from '../../src/server/mekosetup.js';

/** The installer's Meko step with scripted answers, recording what it said. */
function deps(o: { answers?: string[]; canAsk?: boolean; checkError?: string; setupError?: string } = {}) {
  const said: string[] = [];
  const answers = [...(o.answers ?? [])];
  const calls: string[] = [];
  const d: InstallMekoDeps = {
    canAsk: o.canAsk ?? true,
    ask: async (q, def) => {
      calls.push(`ask ${q}`);
      return answers.length ? answers.shift()! : def;
    },
    check: async () => {
      calls.push('check');
      return o.checkError ? { ok: false, error: o.checkError } : { ok: true };
    },
    ensureInClaude: async (_url, _key, keyFromEnv) => {
      calls.push(keyFromEnv ? 'claude (key from env)' : 'claude');
      if (o.setupError) throw new MekoSetupError(o.setupError);
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
  return { d, said, calls };
}

const cfg = (): Config => structuredClone(DEFAULT_CONFIG);

describe('installer: Meko is optional', () => {
  it('goes on without Meko when the key prompt is skipped', async () => {
    const c = cfg();
    const { d, said, calls } = deps({ answers: [''] });
    expect(await installMeko(c, { noMeko: false }, d)).toBe(false);
    expect(calls).toEqual(['ask Meko API key (Enter to skip)']);
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

  it("tells Claude Code's check when the key came only from the installer's environment", async () => {
    process.env.MEKO_API_KEY = 'mko_tkn_env';
    try {
      const fromEnv = deps({ canAsk: false });
      expect(await installMeko(cfg(), { noMeko: false }, fromEnv.d)).toBe(true);
      expect(fromEnv.calls).toEqual(['check', 'claude (key from env)']);
      const typed = deps({ canAsk: false });
      expect(await installMeko(cfg(), { keyArg: 'mko_tkn_typed', noMeko: false }, typed.d)).toBe(true);
      expect(typed.calls).toEqual(['check', 'claude']);
    } finally {
      delete process.env.MEKO_API_KEY;
    }
  });

  it("only warns when Meko's setup for Claude Code is declined", async () => {
    const c = cfg();
    const { d, said } = deps({ canAsk: false, setupError: "Meko isn't set up for Claude Code. Run: npx …" });
    expect(await installMeko(c, { keyArg: 'mko_tkn_good', noMeko: false }, d)).toBe(true);
    expect(said.join('\n')).toMatch(/warn Meko in Claude Meko isn't set up for Claude Code\. Run: npx … AgentK uses Meko/);
    expect(said.some((s) => s.startsWith('ok Meko in Claude'))).toBe(false);
  });
});
