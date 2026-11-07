import { execFile, spawn, type ChildProcess } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { GH_NEEDED, type GhJob, type GhState, type GhStatus } from '../shared/types.js';
import { isLoginPathMissing } from './github.js';
import { HttpError } from './projects.js';

/**
 * AgentK's prerequisites check (git, curl and a coding agent, and gh when asked; it installs what's
 * missing after asking): scripts/deps.sh, which a release has as lib/scripts/deps.sh beside lib/dist.
 */
export function depsScript(here = dirname(fileURLToPath(import.meta.url))): string {
  return join(here, '..', '..', 'scripts', 'deps.sh');
}

/** How long a good check is trusted before a gate checks again (gh can be removed or signed out). */
export const GH_CHECK_TTL_MS = 5 * 60 * 1000;
/** How long each of `gh --version` and `gh auth token` may take. */
const CHECK_TIMEOUT_MS = 10_000;
/** How long the install (deps.sh --only gh) may take. */
const INSTALL_TIMEOUT_MS = 5 * 60 * 1000;
/** How long gh's sign-in waits for the code to be entered: GitHub's device codes last 15 minutes. */
export const LOGIN_TIMEOUT_MS = 15 * 60 * 1000;
/** How long gh may take to print its one-time code before the dialog says to sign in by hand. */
export const CODE_TIMEOUT_MS = 30_000;

/** What the dialog says when the sign-in can't be done from the page. */
export const LOGIN_BY_HAND = 'Run gh auth login on this computer, then check again.';

/** The one-time code and the page to enter it on, from what `gh auth login --web` printed so far. */
export function parseDeviceCode(output: string): { code: string; url: string } | null {
  const code = /one-time code:\s*([A-Z0-9]{4}-[A-Z0-9]{4})/i.exec(output)?.[1];
  if (!code) return null;
  const url = /(https:\/\/\S+\/login\/device)\b/.exec(output)?.[1] ?? 'https://github.com/login/device';
  return { code: code.toUpperCase(), url };
}

/** The last few lines of a command's output, for the dialog: never a sign-in's one-time code, which stays good for 15 minutes. */
function summary(output: string): string {
  const lines = output.split('\n').map((l) => l.trim()).filter((l) => l && !/one-time code/i.test(l));
  const text = lines.slice(-3).join(' ');
  return text.length > 300 ? `${text.slice(0, 299)}…` : text;
}

/**
 * The refusal of <what> while gh is <state>, in words the dialog and an agent's session tool both
 * show. `noPath`: the login shell gave the server no PATH at its start, so gh may be installed
 * where the server doesn't look (github.ts's `ghMissing` says the same).
 */
export function ghNeededMessage(what: string, state: GhState, noPath = isLoginPathMissing()): string {
  if (state === 'signed-out') return `${what} needs gh signed in to GitHub: sign in from AgentK, or run gh auth login.`;
  if (state === 'unknown') return `${what} needs gh, which didn't answer in time: try again.`;
  if (noPath) {
    const allow = process.platform === 'darwin' ? ' If macOS asked whether meko may open a folder, allow it (System Settings › Privacy & Security › Files and Folders).' : '';
    return `${what} needs gh, and AgentK can't find it because it couldn't read PATH from your login shell when it started.${allow} Then run me_koder restart, or install gh from AgentK.`;
  }
  return `${what} needs gh, GitHub's command-line tool, which isn't installed: install it from AgentK, or from https://cli.github.com.`;
}

/** A command's result: its exit code (null when it couldn't start or was killed), and its output. */
export interface RunResult {
  code: number | null;
  /** The command isn't there (ENOENT). */
  missing?: boolean;
  /** Stopped at its time limit (or by a signal). */
  killed?: boolean;
  stdout: string;
  stderr: string;
}

/** Runs a command with no shell, no input and a time limit. */
export function runCommand(cmd: string, args: string[], timeoutMs: number, env: NodeJS.ProcessEnv = process.env): Promise<RunResult> {
  return new Promise((resolve) => {
    execFile(cmd, args, { timeout: timeoutMs, env, maxBuffer: 1024 * 1024 }, (err, stdout, stderr) => {
      const e = err as (NodeJS.ErrnoException & { code?: number | string; killed?: boolean; signal?: string | null }) | null;
      resolve({
        code: e ? (typeof e.code === 'number' ? e.code : null) : 0,
        missing: e?.code === 'ENOENT',
        killed: !!e && (!!e.killed || !!e.signal),
        stdout: String(stdout ?? ''),
        stderr: String(stderr ?? ''),
      });
    });
  });
}

/**
 * gh's state: `gh --version`, then `gh auth token` (local: no call to GitHub), or, from a gh older
 * than 2.17 that has no `auth token` (Ubuntu 22.04's is 2.4), `gh auth status`. Only a gh that isn't
 * there is `missing`; one that doesn't answer in time is `unknown`, checked again at the next gate.
 */
export async function probeGh(run = runCommand): Promise<GhState> {
  const v = await run('gh', ['--version'], CHECK_TIMEOUT_MS);
  if (v.missing) return 'missing';
  if (v.code !== 0) return 'unknown';
  const t = await run('gh', ['auth', 'token', '--hostname', 'github.com'], CHECK_TIMEOUT_MS);
  if (t.code === 0 && t.stdout.trim()) return 'ok';
  if (t.killed) return 'unknown';
  if (!/unknown command/i.test(t.stderr)) return 'signed-out';
  const s = await run('gh', ['auth', 'status', '--hostname', 'github.com'], CHECK_TIMEOUT_MS);
  if (s.killed) return 'unknown';
  return s.code === 0 ? 'ok' : 'signed-out';
}

/** Installs gh alone, with no root: deps.sh's release download on Linux, Homebrew on macOS. */
export async function installGh(script = depsScript()): Promise<{ ok: boolean; output: string }> {
  const r = await runCommand('sh', [script, '--only', 'gh', '--install-deps', '--non-interactive'], INSTALL_TIMEOUT_MS, { ...process.env, NO_COLOR: '1' });
  return { ok: r.code === 0, output: `${r.stdout}\n${r.stderr}` };
}

/** gh's device-flow sign-in, as `GhService.login` drives it. */
export interface LoginProcess {
  /** Each piece of output, stdout and stderr alike. */
  onOutput(fn: (text: string) => void): void;
  /** Once, with the exit code (null when it couldn't start or was killed). */
  onExit(fn: (code: number | null) => void): void;
  kill(): void;
}

/**
 * `gh auth login --web` without a terminal: gh prints its one-time code and GitHub's device page,
 * then waits for the code to be entered there. GH_BROWSER=true keeps it from opening a browser on
 * this computer (the dialog links to the page); the newline answers "Press Enter" when gh asks.
 */
export function startGhLogin(): LoginProcess {
  const child: ChildProcess = spawn('gh', ['auth', 'login', '--hostname', 'github.com', '--git-protocol', 'https', '--web'], {
    env: { ...process.env, GH_BROWSER: 'true', BROWSER: 'true' },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  child.stdin?.on('error', () => undefined);
  child.stdin?.end('\n');
  return {
    onOutput(fn) {
      child.stdout?.on('data', (d: Buffer) => fn(d.toString()));
      child.stderr?.on('data', (d: Buffer) => fn(d.toString()));
    },
    onExit(fn) {
      let done = false;
      const finish = (code: number | null) => {
        if (!done) {
          done = true;
          fn(code);
        }
      };
      child.on('error', () => finish(null));
      child.on('close', (code) => finish(code));
    },
    kill() {
      child.kill('SIGTERM');
    },
  };
}

/** `gh auth setup-git`: git asks gh for github.com's credentials, as gh's own interactive sign-in sets up by default. */
export async function setupGit(): Promise<boolean> {
  return (await runCommand('gh', ['auth', 'setup-git', '--hostname', 'github.com'], CHECK_TIMEOUT_MS)).code === 0;
}

export interface GhDeps {
  /** Something visible changed: the state is pushed to the browser. */
  changed: () => void;
  /** The login gh is signed in as (Core.ghLogin, which keeps the first answer until `forgetLogin`). */
  login: () => Promise<string | null>;
  /** Forgets the login kept, so the next `login` asks gh again: gh signed out, or signed in from the dialog, maybe as another account. */
  forgetLogin: () => void;
  /** The login already known, for the status. */
  knownLogin: () => string | null;
  warn: (msg: string, detail?: string) => void;
  probe?: () => Promise<GhState>;
  install?: () => Promise<{ ok: boolean; output: string }>;
  startLogin?: () => LoginProcess;
  setupGit?: () => Promise<boolean>;
  now?: () => number;
}

/**
 * gh, GitHub's command-line tool, which AgentK needs for everything on GitHub but doesn't need to be
 * installed: whether it's usable now (`state`, kept while good for GH_CHECK_TTL_MS and checked again
 * by any gate that finds it not good, so gh set up outside AgentK counts at once), the gate every
 * action that works on GitHub goes through (`require`), and the GitHub dialog's install and sign-in.
 */
export class GhService {
  private state: GhState = 'unknown';
  private checkedAt = 0;
  private checking: Promise<GhState> | null = null;
  private job: GhJob | null = null;
  private proc: LoginProcess | null = null;
  private timers: NodeJS.Timeout[] = [];
  private readonly now: () => number;

  constructor(private deps: GhDeps) {
    this.now = deps.now ?? Date.now;
  }

  status(): GhStatus {
    return { state: this.state, login: this.state === 'ok' ? this.deps.knownLogin() : null, job: this.job };
  }

  /** Reads gh's state again (one read at a time), and pushes a change. */
  check(): Promise<GhState> {
    this.checking ??= (async () => {
      try {
        const before = this.state;
        this.state = await (this.deps.probe ?? probeGh)();
        this.checkedAt = this.now();
        if (before === 'ok' && (this.state === 'signed-out' || this.state === 'missing')) this.deps.forgetLogin();
        if (this.state !== before) {
          if (this.state === 'ok') void this.deps.login();
          this.deps.changed();
        }
        return this.state;
      } finally {
        this.checking = null;
      }
    })();
    return this.checking;
  }

  /** gh's state, checked again first unless a good check is fresh. */
  async ready(): Promise<GhState> {
    if (this.state !== 'ok' || this.now() - this.checkedAt > GH_CHECK_TTL_MS) await this.check();
    return this.state;
  }

  /** Refuses <what> (409, GH_NEEDED) unless gh is installed and signed in (`ready`). */
  async require(what: string): Promise<void> {
    const state = await this.ready();
    if (state !== 'ok') throw new HttpError(409, ghNeededMessage(what, state), GH_NEEDED);
  }

  private busy(): boolean {
    return this.job?.phase === 'running' || this.job?.phase === 'code';
  }

  private setJob(job: GhJob | null): void {
    this.job = job;
    this.deps.changed();
  }

  /** Installs gh (deps.sh --only gh), then checks again. The install failing is an outcome (`job`), not an error. */
  async install(): Promise<GhStatus> {
    if (this.busy()) throw new HttpError(409, 'gh is already being installed or signed in.');
    this.setJob({ kind: 'install', phase: 'running' });
    let r: { ok: boolean; output: string };
    try {
      r = await (this.deps.install ?? installGh)();
    } catch (e) {
      r = { ok: false, output: (e as Error).message };
    }
    const state = await this.check();
    if (r.ok && state !== 'missing') this.setJob(null);
    else {
      this.deps.warn('installing gh failed', r.output);
      this.setJob({ kind: 'install', phase: 'failed', message: summary(r.output) || "gh couldn't be installed." });
    }
    return this.status();
  }

  /**
   * Starts gh's sign-in and answers once gh has given its one-time code (or failed to): the job's
   * `code` and `url`. gh then waits for the code to be entered on GitHub; when it exits 0, git is set
   * up to use it and gh is checked again.
   */
  async login(): Promise<GhStatus> {
    if (this.busy()) throw new HttpError(409, 'gh is already being installed or signed in.');
    // Taken before the check's await, so a second sign-in asked meanwhile is refused above.
    this.setJob({ kind: 'login', phase: 'running' });
    if (this.state !== 'ok') await this.check();
    if (this.state === 'missing') {
      this.setJob(null);
      throw new HttpError(409, 'Install gh first.');
    }
    const proc = (this.deps.startLogin ?? startGhLogin)();
    this.proc = proc;
    let output = '';
    let shown: () => void = () => undefined;
    const codeShown = new Promise<void>((resolve) => {
      shown = resolve;
    });
    const mine = () => this.proc === proc;
    const stop = (job: GhJob | null) => {
      if (!mine()) return;
      this.proc = null;
      this.clearTimers();
      proc.kill();
      this.setJob(job);
      shown();
    };
    proc.onOutput((text) => {
      output += text;
      if (!mine() || this.job?.phase !== 'running') return;
      const found = parseDeviceCode(output);
      if (!found) return;
      this.setJob({ kind: 'login', phase: 'code', code: found.code, url: found.url });
      shown();
    });
    proc.onExit((code) => {
      if (!mine()) return;
      this.proc = null;
      this.clearTimers();
      if (code !== 0) {
        // Not its output, which has the one-time code in it.
        this.deps.warn('gh auth login failed', `exit ${code ?? 'signal'}`);
        this.setJob({ kind: 'login', phase: 'failed', message: `${summary(output) || "gh couldn't sign in."} ${LOGIN_BY_HAND}` });
        shown();
        return;
      }
      void (async () => {
        if (!(await (this.deps.setupGit ?? setupGit)())) this.deps.warn("gh auth setup-git failed: git pushes to github.com may ask for credentials");
        // Maybe another account than the one kept: ask gh for its login again.
        this.deps.forgetLogin();
        await this.check();
        if (this.state === 'ok') void this.deps.login();
        this.setJob(null);
        shown();
      })();
    });
    this.timers.push(
      setTimeout(() => {
        if (this.job?.phase === 'running') stop({ kind: 'login', phase: 'failed', message: `gh didn't give a sign-in code. ${LOGIN_BY_HAND}` });
      }, CODE_TIMEOUT_MS),
      setTimeout(() => stop({ kind: 'login', phase: 'failed', message: 'The sign-in code expired. Sign in again.' }), LOGIN_TIMEOUT_MS),
    );
    for (const t of this.timers) t.unref();
    await codeShown;
    return this.status();
  }

  /** Stops a sign-in under way, and forgets an install or sign-in that failed (the dialog closed). An install runs on. */
  cancel(): GhStatus {
    if (this.proc) {
      const proc = this.proc;
      this.proc = null;
      this.clearTimers();
      proc.kill();
      this.setJob(null);
    } else if (this.job?.phase === 'failed') {
      this.setJob(null);
    }
    return this.status();
  }

  private clearTimers(): void {
    for (const t of this.timers) clearTimeout(t);
    this.timers = [];
  }

  /** Stops a sign-in under way, for the server's stop. */
  stop(): void {
    this.cancel();
  }
}
