import { describe, expect, it } from 'vitest';
import { MIN_MOD_CLAUDE } from '../../src/server/claude.js';
import { CLAUDE_VERSION_TTL_MS, ClaudeCode } from '../../src/server/claudecode.js';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';

/** A ClaudeCode over a fake `claude`: `installed` is what --version prints, `update` what `claude update` does. */
function setup(installed: string | null, update: (c: { installed: string | null }) => { ok: boolean; output: string } = () => ({ ok: true, output: '' })) {
  const cfg: Config = structuredClone(DEFAULT_CONFIG);
  const state = { installed, reads: 0, changes: 0, now: 1_000_000, updates: 0 };
  const cc = new ClaudeCode({
    config: () => cfg,
    changed: () => state.changes++,
    readVersion: async () => {
      state.reads++;
      return state.installed;
    },
    runUpdate: async () => {
      state.updates++;
      return update(state);
    },
    now: () => state.now,
  });
  return { cc, cfg, state };
}

describe('the Claude Code sessions launch with', () => {
  it("runs no claude --version while it's turned off, and has no version then", async () => {
    const { cc, cfg, state } = setup('2.1.290');
    await cc.version();
    cfg.claude.enabled = false;
    expect(await cc.version(true)).toBeNull();
    expect(state.reads).toBe(1);
    expect(cc.checked()).toBe(true);
    expect(cc.status().version).toBeNull();
  });

  it('reports its version and whether it runs the mod, reading it again only once the last read is stale', async () => {
    const { cc, state } = setup('2.1.290');
    expect(cc.status()).toMatchObject({ version: null, supportsMod: null, minModVersion: MIN_MOD_CLAUDE, modEnabled: true });
    expect(cc.checked()).toBe(false);
    expect(await cc.supportsMod()).toBe(true);
    expect(cc.checked()).toBe(true);
    expect(cc.status()).toMatchObject({ version: '2.1.290', supportsMod: true, updating: false, lastUpdate: null });
    expect(state.changes).toBe(1);
    await cc.version();
    expect(state.reads).toBe(1);
    state.installed = '2.1.291';
    state.now += CLAUDE_VERSION_TTL_MS + 1;
    expect(await cc.version()).toBe('2.1.291');
    expect(state.reads).toBe(2);
    expect(state.changes).toBe(2);
  });

  it('is too old for the mod below 2.1.287, and unknown when its version can\'t be read', async () => {
    const old = setup('2.1.280');
    expect(await old.cc.supportsMod()).toBe(false);
    expect(old.cc.status()).toMatchObject({ version: '2.1.280', supportsMod: false });
    const none = setup(null);
    expect(await none.cc.supportsMod()).toBe(false);
    expect(none.cc.status()).toMatchObject({ version: null, supportsMod: null });
  });

  it('says when the mod is turned off, whatever the version', async () => {
    const { cc, cfg } = setup('2.1.280');
    cfg.claude.mod = false;
    await cc.version();
    expect(cc.status()).toMatchObject({ modEnabled: false, supportsMod: false });
  });

  it('updates with claude update, then reads the version again', async () => {
    const { cc, state } = setup('2.1.280', (s) => {
      s.installed = '2.1.291';
      return { ok: true, output: 'Current version: 2.1.280\nChecking for updates...\nSuccessfully updated from 2.1.280 to version 2.1.291\n' };
    });
    await cc.version();
    const after = await cc.update();
    expect(after).toMatchObject({ version: '2.1.291', supportsMod: true, updating: false });
    expect(after.lastUpdate).toEqual({
      ok: true, from: '2.1.280', to: '2.1.291', at: state.now,
      message: 'Current version: 2.1.280 Checking for updates... Successfully updated from 2.1.280 to version 2.1.291',
    });
  });

  it('reports a failed update as its outcome, with what Claude Code printed', async () => {
    const { cc } = setup('2.1.280', () => ({ ok: false, output: 'Error: EACCES: permission denied, open /usr/local/lib' }));
    await cc.version();
    const after = await cc.update();
    expect(after.lastUpdate).toMatchObject({ ok: false, from: '2.1.280', to: '2.1.280', message: 'Error: EACCES: permission denied, open /usr/local/lib' });
    expect(after.supportsMod).toBe(false);
  });

  it('runs one update at a time, and shows it running', async () => {
    let finish: (r: { ok: boolean; output: string }) => void = () => undefined;
    const cfg: Config = structuredClone(DEFAULT_CONFIG);
    const cc = new ClaudeCode({
      config: () => cfg,
      changed: () => undefined,
      readVersion: async () => '2.1.280',
      runUpdate: () => new Promise((r) => (finish = r)),
    });
    const first = cc.update();
    expect(cc.status().updating).toBe(true);
    await expect(cc.update()).rejects.toMatchObject({ status: 409 });
    finish({ ok: true, output: 'Claude Code is up to date (2.1.280)' });
    expect((await first).updating).toBe(false);
  });
});
