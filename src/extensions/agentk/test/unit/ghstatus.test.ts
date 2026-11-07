import { describe, expect, it, vi } from 'vitest';
import { GH_NEEDED, type GhState } from '../../src/shared/types.js';
import { GhService, ghNeededMessage, parseDeviceCode, probeGh, type GhDeps, type LoginProcess, type RunResult } from '../../src/server/ghstatus.js';
import { HttpError } from '../../src/server/projects.js';

/** What `gh auth login --web` (2.97) prints without a terminal, before the code is entered. */
const DEVICE_FLOW = '\n! First copy your one-time code: F364-DFAB\nOpen this URL to continue in your web browser: https://github.com/login/device\n';

/** A gh sign-in the test drives: its output and exit, once the service has started it. */
function fakeLogin() {
  let out: ((t: string) => void) | null = null;
  let exit: (c: number | null) => void = () => undefined;
  const proc: LoginProcess & { killed: boolean } = {
    killed: false,
    onOutput: (fn) => (out = fn),
    onExit: (fn) => (exit = fn),
    kill() {
      this.killed = true;
    },
  };
  const say = async (t: string) => {
    await vi.waitFor(() => expect(out).not.toBeNull());
    out?.(t);
  };
  return { proc, say, exit: (c: number | null) => exit(c) };
}

function service(state: { value: GhState }, extra: Partial<GhDeps> = {}) {
  const probe = vi.fn(async () => state.value);
  const gh = new GhService({ changed: () => undefined, login: async () => 'alice', forgetLogin: () => undefined, knownLogin: () => 'alice', warn: () => undefined, probe, ...extra });
  return { gh, probe };
}

describe('gh', () => {
  it('reads the one-time code and the page from what gh prints', () => {
    expect(parseDeviceCode(DEVICE_FLOW)).toEqual({ code: 'F364-DFAB', url: 'https://github.com/login/device' });
    expect(parseDeviceCode('! First copy your')).toBeNull();
  });

  const UNKNOWN_TOKEN = { code: 1, stderr: 'unknown command "token" for "gh auth"' };
  it.each<[string, GhState, Partial<RunResult>, Partial<RunResult>, Partial<RunResult>?]>([
    ['not there', 'missing', { code: null, missing: true }, { code: null }],
    ['too slow to answer', 'unknown', { code: null, killed: true }, { code: null }],
    ['signed out', 'signed-out', { code: 0 }, { code: 1 }],
    ['signed in', 'ok', { code: 0 }, { code: 0, stdout: 'gho_test-token\n' }],
    // gh before 2.17 has no `auth token`: `auth status` says instead.
    ['older, signed in', 'ok', { code: 0 }, UNKNOWN_TOKEN, { code: 0 }],
    ['older, signed out', 'signed-out', { code: 0 }, UNKNOWN_TOKEN, { code: 1 }],
  ])('finds gh %s', async (_, want, version, token, status = { code: null }) => {
    const answers: Record<string, Partial<RunResult>> = { '--version': version, token, status };
    const run = async (__: string, args: string[]): Promise<RunResult> => ({ code: null, stdout: '', stderr: '', ...answers[args[0] === '--version' ? '--version' : args[1]] });
    expect(await probeGh(run)).toBe(want);
  });

  it('refuses an action with GH_NEEDED until gh is usable, checking again each time, then trusts a fresh check', async () => {
    const state: { value: GhState } = { value: 'missing' };
    const { gh, probe } = service(state);
    const refusal = await gh.require('Open PR').catch((e: unknown) => e);
    expect(refusal).toBeInstanceOf(HttpError);
    expect(refusal).toMatchObject({ status: 409, code: GH_NEEDED, message: expect.stringMatching(/^Open PR needs gh, GitHub's command-line tool/) });
    // With no PATH from the login shell, gh may be installed where the server doesn't look.
    expect(ghNeededMessage('Open PR', 'missing', true)).toMatch(/couldn't read PATH from your login shell.* run me_koder restart/);
    state.value = 'signed-out';
    await expect(gh.require('Merge')).rejects.toThrow(/^Merge needs gh signed in to GitHub/);
    state.value = 'ok';
    await gh.require('Merge');
    await gh.require('Merge');
    expect(probe).toHaveBeenCalledTimes(3);
    expect(gh.status()).toEqual({ state: 'ok', login: 'alice', job: null });
  });

  it('keeps a failed install for the dialog until it closes', async () => {
    const { gh } = service({ value: 'missing' }, { install: async () => ({ ok: false, output: 'curl: (6) Could not resolve host' }) });
    await gh.check();
    expect((await gh.install()).job).toEqual({ kind: 'install', phase: 'failed', message: 'curl: (6) Could not resolve host' });
    expect(gh.cancel().job).toBeNull();
  });

  it('signs in with the one-time code, then sets git up, forgets the login kept and checks again', async () => {
    const state: { value: GhState } = { value: 'signed-out' };
    const login = fakeLogin();
    const setupGit = vi.fn(async () => true);
    const forgetLogin = vi.fn();
    const { gh } = service(state, { startLogin: () => login.proc, setupGit, forgetLogin });
    const started = gh.login();
    await login.say(DEVICE_FLOW);
    expect((await started).job).toEqual({ kind: 'login', phase: 'code', code: 'F364-DFAB', url: 'https://github.com/login/device' });
    await expect(gh.install()).rejects.toThrow(/already being installed or signed in/);
    state.value = 'ok';
    login.exit(0);
    await vi.waitFor(() => expect(gh.status()).toEqual({ state: 'ok', login: 'alice', job: null }));
    expect(setupGit).toHaveBeenCalledOnce();
    // gh may be on another account now than the login kept.
    expect(forgetLogin).toHaveBeenCalled();
  });

  it('stops a sign-in when the dialog closes, and says to sign in by hand when gh fails', async () => {
    const first = fakeLogin();
    const { gh } = service({ value: 'signed-out' }, { startLogin: () => first.proc });
    const started = gh.login();
    await first.say(DEVICE_FLOW);
    await started;
    expect(gh.cancel().job).toBeNull();
    expect(first.proc.killed).toBe(true);

    const second = fakeLogin();
    const again = new GhService({ changed: () => undefined, login: async () => null, forgetLogin: () => undefined, knownLogin: () => null, warn: () => undefined, probe: async () => 'signed-out', startLogin: () => second.proc });
    const failed = again.login();
    await second.say(`${DEVICE_FLOW}error connecting to github.com\n`);
    await failed;
    second.exit(1);
    const job = again.status().job;
    expect(job).toMatchObject({ kind: 'login', phase: 'failed', message: expect.stringContaining('Run gh auth login on this computer') });
    // The code is still good for a while: it isn't shown or kept once the sign-in has failed.
    expect(job?.message).not.toContain('F364-DFAB');
    // A second sign-in asked while one starts is refused, not run beside it.
    const third = fakeLogin();
    const once = new GhService({ changed: () => undefined, login: async () => null, forgetLogin: () => undefined, knownLogin: () => null, warn: () => undefined, probe: async () => 'signed-out', startLogin: () => third.proc });
    const starting = once.login();
    await expect(once.login()).rejects.toThrow(/already being installed or signed in/);
    await third.say(DEVICE_FLOW);
    await starting;
  });
});
