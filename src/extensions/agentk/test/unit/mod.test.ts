import { execFileSync, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const MOD = fileURLToPath(new URL('../../src/mod/agentk', import.meta.url));
const VIEW = fileURLToPath(new URL('../../src/mod/agentk-view', import.meta.url));

/** Claude Code, when it's installed here (CI has none): the mod is checked by Claude Code itself. */
function claude(): string | null {
  try {
    execFileSync('claude', ['--version'], { stdio: 'ignore', timeout: 20_000 });
    return 'claude';
  } catch {
    return null;
  }
}

const bin = claude();

describe.skipIf(!bin)("Kode's mod, checked by Claude Code", () => {
  const run = (...args: string[]) => spawnSync(bin!, args, { encoding: 'utf8', timeout: 120_000 });

  it('validates: its hooks and every call on $ are ones the engine takes', () => {
    const r = run('plugin', 'validate', MOD);
    expect(r.status, r.stdout + r.stderr).toBe(0);
    expect(r.stdout).toMatch(/hooks: session\.start, session\.measure/);
  });

  it('passes its own tests (src/mod/agentk/tests)', (ctx) => {
    const r = run('plugin', 'test', MOD);
    // Claude Code runs a plugin's tests only while its rollout switch serves hooks modules on.
    if (/hooks modules are turned off/.test(r.stdout + r.stderr)) ctx.skip();
    expect(r.status, r.stdout + r.stderr).toBe(0);
  }, 150_000);
});

describe.skipIf(!bin)("Kode's view mod, checked by Claude Code", () => {
  const run = (...args: string[]) => spawnSync(bin!, args, { encoding: 'utf8', timeout: 120_000 });

  it('validates: it hooks only the drawing of prompts, and reads only its two variables (by their old names too)', () => {
    const r = run('plugin', 'validate', VIEW);
    expect(r.status, r.stdout + r.stderr).toBe(0);
    // The old names too: a session launched before Kode was called Kode has them.
    expect(r.stdout).toMatch(/env reads: AK_FIRST_PROMPT, AK_HIDDEN_PROMPT, KODE_FIRST_PROMPT, KODE_HIDDEN_PROMPT/);
  });

  it('passes its own tests (src/mod/agentk-view/tests)', (ctx) => {
    const r = run('plugin', 'test', VIEW);
    if (/hooks modules are turned off/.test(r.stdout + r.stderr)) ctx.skip();
    expect(r.status, r.stdout + r.stderr).toBe(0);
  }, 150_000);
});
