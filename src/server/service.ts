import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, unlinkSync, writeFileSync } from 'node:fs';
import { homedir, userInfo } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * The two background services: the web server, and the session host that holds
 * every Claude session (host/host.ts). They are separate so restarting or updating
 * the server never ends a session. systemd user units run them on Linux,
 * LaunchAgents on macOS; everything else talks to them through `ServiceManager`.
 *
 * Versions before the session host ran sessions in tmux, in a tmux service. It is
 * only ever removed now (`removeLegacyTmux`). Remove that support after 2026-10-14.
 */
export type Service = 'server' | 'host';

/** Install and start order: the host first, so the server finds it. */
export const SERVICES: readonly Service[] = ['host', 'server'];

/** A problem the person installing can fix; the message says how. */
export class ServiceError extends Error {}

export interface BootCheck {
  ok: boolean;
  label: string;
  detail: string;
  note?: string;
}

export interface ServiceManager {
  readonly kind: 'systemd' | 'launchd';
  /** The unit name or job label, as typed into systemctl or launchctl. */
  id(s: Service): string;
  /** Where the unit file or property list lives. */
  file(s: Service): string;
  /** Throws a ServiceError when this host can't run the services, saying how to fix it. */
  preflight(): void;
  installed(s: Service): boolean;
  /**
   * Writes both service files and starts them. A running host is never
   * restarted (that would end every session); the server always is, so a
   * reinstall or upgrade runs the new version.
   */
  install(files: Record<Service, string>): void;
  /**
   * For a server that updated itself from a version without the host service: writes both
   * files and starts the host, but never restarts the server (the caller is that server).
   */
  addHost(files: Record<Service, string>): void;
  /** Restarts a service (for the host, this ends every Claude; recovery resumes them). */
  restart(s: Service): void;
  /** Whether the tmux service of versions before the session host is still installed. */
  legacyTmuxInstalled(): boolean;
  /** Stops and deletes the tmux service of versions before the session host. */
  removeLegacyTmux(): void;
  /** Starts a service that isn't running; leaves a running one alone. */
  start(s: Service): void;
  state(s: Service): { running: boolean; text: string };
  /** Stops both services (and an old tmux one) and deletes their files. */
  remove(): void;
  /** Whether the services come back after a reboot, turning that on where we can. */
  bootCheck(): BootCheck;
  /** A command that shows why a service isn't running. */
  statusHint(s: Service): string;
}

export type Runner = (cmd: string, args: string[]) => { ok: boolean; out: string; err: string };

export const runCommand: Runner = (cmd, args) => {
  try {
    const out = execFileSync(cmd, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], maxBuffer: 16 * 1024 * 1024 });
    return { ok: true, out: out.trim(), err: '' };
  } catch (e) {
    const x = e as { stdout?: string; stderr?: string; message: string };
    return { ok: false, out: String(x.stdout ?? '').trim(), err: String(x.stderr || x.message).trim() };
  }
};

function onPath(run: Runner, bin: string): boolean {
  const r = run('sh', ['-c', `command -v ${bin}`]);
  return r.ok && !!r.out;
}

/** Runs a command that has to work. Callers add what to check next (install.ts `serviceFailure`). */
function must(run: Runner, cmd: string, args: string[]): string {
  const r = run(cmd, args);
  if (!r.ok) throw new ServiceError(`${cmd} ${args.join(' ')} failed: ${r.err || 'no error message'}`);
  return r.out;
}

const sleepSync = (ms: number) => void Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);

// ---------------------------------------------------------------------------
// Linux: systemd user units

const SYSTEMD_UNITS: Record<Service, string> = {
  server: 'agent-k-local.service',
  host: 'agent-k-local-host.service',
};
/** The tmux unit of versions before the session host. */
const SYSTEMD_TMUX = 'agent-k-local-tmux.service';

export class SystemdServices implements ServiceManager {
  readonly kind = 'systemd';

  constructor(
    private run: Runner = runCommand,
    private env: NodeJS.ProcessEnv = process.env,
    private home: string = homedir(),
    private user: () => string = () => userInfo().username,
  ) {}

  id(s: Service): string {
    return SYSTEMD_UNITS[s];
  }

  private dir(): string {
    return join(this.env.XDG_CONFIG_HOME || join(this.home, '.config'), 'systemd', 'user');
  }

  file(s: Service): string {
    return join(this.dir(), this.id(s));
  }

  preflight(): void {
    if (!onPath(this.run, 'systemctl')) throw new ServiceError('systemctl was not found; rerun with --no-service to skip the service setup.');
  }

  installed(s: Service): boolean {
    return existsSync(this.file(s));
  }

  install(files: Record<Service, string>): void {
    mkdirSync(this.dir(), { recursive: true });
    for (const s of SERVICES) writeFileSync(this.file(s), files[s]);
    must(this.run, 'systemctl', ['--user', 'daemon-reload']);
    // `enable --now` never restarts a running host, so sessions survive reinstalls.
    must(this.run, 'systemctl', ['--user', 'enable', '--now', this.id('host')]);
    must(this.run, 'systemctl', ['--user', 'enable', this.id('server')]);
    must(this.run, 'systemctl', ['--user', 'restart', this.id('server')]);
  }

  addHost(files: Record<Service, string>): void {
    mkdirSync(this.dir(), { recursive: true });
    for (const s of SERVICES) writeFileSync(this.file(s), files[s]);
    must(this.run, 'systemctl', ['--user', 'daemon-reload']);
    must(this.run, 'systemctl', ['--user', 'enable', '--now', this.id('host')]);
  }

  start(s: Service): void {
    this.run('systemctl', ['--user', 'start', this.id(s)]);
  }

  restart(s: Service): void {
    must(this.run, 'systemctl', ['--user', 'restart', this.id(s)]);
  }

  private tmuxFile(): string {
    return join(this.dir(), SYSTEMD_TMUX);
  }

  legacyTmuxInstalled(): boolean {
    return existsSync(this.tmuxFile());
  }

  removeLegacyTmux(): void {
    this.run('systemctl', ['--user', 'disable', '--now', SYSTEMD_TMUX]);
    if (existsSync(this.tmuxFile())) unlinkSync(this.tmuxFile());
    this.run('systemctl', ['--user', 'daemon-reload']);
  }

  state(s: Service): { running: boolean; text: string } {
    // is-active prints the state on stdout and exits non-zero unless it is active.
    const text = this.run('systemctl', ['--user', 'is-active', this.id(s)]).out || 'inactive';
    return { running: text === 'active', text };
  }

  remove(): void {
    for (const s of ['server', 'host'] as const) {
      this.run('systemctl', ['--user', 'disable', '--now', this.id(s)]);
      if (existsSync(this.file(s))) unlinkSync(this.file(s));
    }
    if (this.legacyTmuxInstalled()) this.removeLegacyTmux();
    else this.run('systemctl', ['--user', 'daemon-reload']);
  }

  bootCheck(): BootCheck {
    const user = this.user();
    const label = 'Starts at boot';
    const linger = this.run('loginctl', ['show-user', user, '-p', 'Linger', '--value']);
    if (linger.ok && linger.out === 'yes') return { ok: true, label, detail: 'lingering is on for your account' };
    const en = this.run('loginctl', ['enable-linger', user]);
    if (en.ok) return { ok: true, label, detail: 'lingering enabled for your account' };
    return {
      ok: false,
      label,
      detail: `no: couldn't enable lingering (${en.err || 'permission denied'}).`,
      note: `Without it, AgentK Local only runs while you're logged in. Ask an admin: sudo loginctl enable-linger ${user}`,
    };
  }

  statusHint(s: Service): string {
    return `systemctl --user status ${this.id(s)}`;
  }
}

// ---------------------------------------------------------------------------
// macOS: LaunchAgents in the gui/<uid> domain

const LAUNCHD_LABELS: Record<Service, string> = {
  server: 'com.yugabyte.agent-k-local',
  host: 'com.yugabyte.agent-k-local.host',
};
/** The tmux job of versions before the session host. */
const LAUNCHD_TMUX = 'com.yugabyte.agent-k-local.tmux';

/** The job's own `state = …` line from `launchctl print` (the first one; nested blocks come later). */
export function parseLaunchdState(print: string): string | null {
  return /^\s*state = (.+)$/m.exec(print)?.[1].trim() ?? null;
}

export class LaunchdServices implements ServiceManager {
  readonly kind = 'launchd';

  constructor(
    private run: Runner = runCommand,
    private home: string = homedir(),
    private uid: number = process.getuid?.() ?? 0,
    private pause: (ms: number) => void = sleepSync,
  ) {}

  id(s: Service): string {
    return LAUNCHD_LABELS[s];
  }

  private domain(): string {
    return `gui/${this.uid}`;
  }

  private target(s: Service): string {
    return `${this.domain()}/${this.id(s)}`;
  }

  file(s: Service): string {
    return join(this.home, 'Library', 'LaunchAgents', `${this.id(s)}.plist`);
  }

  preflight(): void {
    // The gui domain exists only while you're logged in to the desktop. It is also
    // what lets Claude read its login from the Keychain.
    if (!this.run('launchctl', ['print', this.domain()]).ok) {
      throw new ServiceError(
        "You aren't logged in to this Mac's desktop, so there is nowhere to run the background services " +
          "(and Claude couldn't read its login from your Keychain). Log in to the desktop, in person or with Screen Sharing, then run the install again.",
      );
    }
  }

  installed(s: Service): boolean {
    return existsSync(this.file(s));
  }

  private loaded(s: Service): boolean {
    return this.run('launchctl', ['print', this.target(s)]).ok;
  }

  private bootstrap(s: Service): void {
    must(this.run, 'launchctl', ['bootstrap', this.domain(), this.file(s)]);
  }

  /** Unloads a job and waits for launchd to finish (bootout returns before the job is gone). */
  private unload(s: Service): void {
    this.unloadLabel(this.id(s));
  }

  private unloadLabel(label: string): void {
    const target = `${this.domain()}/${label}`;
    if (!this.run('launchctl', ['print', target]).ok) return;
    this.run('launchctl', ['bootout', target]);
    for (let i = 0; i < 50 && this.run('launchctl', ['print', target]).ok; i++) this.pause(100);
  }

  private waitRunning(s: Service): void {
    for (let i = 0; i < 30 && !this.state(s).running; i++) this.pause(100);
  }

  install(files: Record<Service, string>): void {
    mkdirSync(dirname(this.file('server')), { recursive: true });
    for (const s of SERVICES) {
      const f = this.file(s);
      const changed = !existsSync(f) || readFileSync(f, 'utf8') !== files[s];
      writeFileSync(f, files[s]);
      if (s === 'host') {
        // Never reload a loaded host job: that ends every session. A changed file applies at the next login.
        if (!this.loaded(s)) this.bootstrap(s);
      } else if (!changed && this.loaded(s)) {
        must(this.run, 'launchctl', ['kickstart', '-k', this.target(s)]);
      } else {
        // launchd keeps the definition it loaded; reload the job to pick up the new file.
        this.unload(s);
        this.bootstrap(s);
      }
    }
    for (const s of SERVICES) this.waitRunning(s);
  }

  addHost(files: Record<Service, string>): void {
    mkdirSync(dirname(this.file('server')), { recursive: true });
    // The server's file applies the next time launchd loads it; reloading it now would stop us.
    for (const s of SERVICES) writeFileSync(this.file(s), files[s]);
    if (!this.loaded('host')) this.bootstrap('host');
    this.waitRunning('host');
  }

  start(s: Service): void {
    if (this.loaded(s)) this.run('launchctl', ['kickstart', this.target(s)]); // without -k it leaves a running job alone
    else if (this.installed(s)) this.run('launchctl', ['bootstrap', this.domain(), this.file(s)]);
  }

  restart(s: Service): void {
    if (this.loaded(s)) must(this.run, 'launchctl', ['kickstart', '-k', this.target(s)]);
    else this.bootstrap(s);
  }

  private tmuxFile(): string {
    return join(this.home, 'Library', 'LaunchAgents', `${LAUNCHD_TMUX}.plist`);
  }

  legacyTmuxInstalled(): boolean {
    return existsSync(this.tmuxFile());
  }

  removeLegacyTmux(): void {
    this.unloadLabel(LAUNCHD_TMUX);
    if (existsSync(this.tmuxFile())) unlinkSync(this.tmuxFile());
  }

  state(s: Service): { running: boolean; text: string } {
    const r = this.run('launchctl', ['print', this.target(s)]);
    if (!r.ok) return { running: false, text: 'not loaded' };
    const text = parseLaunchdState(r.out) ?? 'unknown';
    return { running: text === 'running', text };
  }

  remove(): void {
    for (const s of ['server', 'host'] as const) {
      this.unload(s);
      if (existsSync(this.file(s))) unlinkSync(this.file(s));
    }
    if (this.legacyTmuxInstalled()) this.removeLegacyTmux();
  }

  bootCheck(): BootCheck {
    return {
      ok: true,
      label: 'Starts',
      detail: 'when you log in to this Mac',
      note: 'It stops when you log out, and sessions pause while the Mac sleeps.',
    };
  }

  statusHint(s: Service): string {
    return `launchctl print ${this.target(s)}`;
  }
}

// ---------------------------------------------------------------------------

/** The service manager for this host, or null where we have none. */
export function hostServices(platform: NodeJS.Platform = process.platform): ServiceManager | null {
  if (platform === 'linux') return new SystemdServices();
  if (platform === 'darwin') return new LaunchdServices();
  return null;
}

/**
 * The service manager when this host's services run `home`, else null: a dev server (or a second
 * home) must neither start nor wait on the services of another install on the same account.
 * With no services installed yet it is the host's manager.
 */
export function servicesFor(home: string, svc: ServiceManager | null = hostServices()): ServiceManager | null {
  if (!svc) return null;
  const file = svc.installed('server') ? svc.file('server') : svc.installed('host') ? svc.file('host') : null;
  if (!file) return svc;
  try {
    const text = readFileSync(file, 'utf8');
    return text.includes(svc.kind === 'launchd' ? xmlEscape(home) : home) ? svc : null;
  } catch {
    return null;
  }
}

function templatesDir(kind: ServiceManager['kind'], probe: string): string {
  const candidates = [
    process.env.AK_APP_DIR ? join(process.env.AK_APP_DIR, 'lib', 'packaging', kind) : '',
    // dev checkout: src/server/service.ts or dist/server/service.js → repo root
    resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', 'packaging', kind),
  ].filter(Boolean);
  const found = candidates.find((d) => existsSync(join(d, probe)));
  if (!found) throw new ServiceError(`${kind} templates not found (looked in ${candidates.join(', ')})`);
  return found;
}

const xmlEscape = (s: string) => s.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');

export interface TemplateVars {
  AK_HOME: string;
  /** This host's name: the systemd units run only here (ConditionHost). */
  HOST: string;
}

/** Fills in @AK_HOME@ and @HOST@ (XML-escaped in property lists). */
export function renderTemplate(template: string, kind: ServiceManager['kind'], vars: TemplateVars): string {
  const esc = kind === 'launchd' ? xmlEscape : (s: string) => s;
  return template.replaceAll('@AK_HOME@', esc(vars.AK_HOME)).replaceAll('@HOST@', esc(vars.HOST));
}

/** Renders both service files from the templates shipped in packaging/<kind>/. */
export function renderServiceFiles(svc: ServiceManager, vars: TemplateVars): Record<Service, string> {
  const dir = templatesDir(svc.kind, basename(svc.file('server')));
  const one = (s: Service) => renderTemplate(readFileSync(join(dir, basename(svc.file(s))), 'utf8'), svc.kind, vars);
  return { server: one('server'), host: one('host') };
}
