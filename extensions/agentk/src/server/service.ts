import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, rmSync, unlinkSync, writeFileSync } from 'node:fs';
import { homedir, userInfo } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * The two background services: the web server, and the session host that holds
 * every Claude session (host/host.ts). They are separate so restarting or updating
 * the server never ends a session. systemd user units run them on Linux,
 * LaunchAgents on macOS; everything else talks to them through `ServiceManager`.
 *
 * Versions before the session host ran sessions in tmux, in a tmux service. Its Claudes must never
 * run while the host runs them too, so the server refuses to start while it's installed, and
 * `install` and `uninstall` remove it (`removeLegacyTmux`), which ends them.
 *
 * Until the rename to AgentK the services were named agent-k-local (`agent-k-local.service`,
 * `com.yugabyte.agent-k-local`). A service installed under its old name keeps it, and `id` and
 * `file` name it, until `install` moves it to its new one (`dropOldName`).
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
  /** The unit name or job label, as typed into systemctl or launchctl: its old one while it's installed under that. */
  id(s: Service): string;
  /** Where the unit file or property list lives. */
  file(s: Service): string;
  /** Throws a ServiceError when this host can't run the services, saying how to fix it. */
  preflight(): void;
  installed(s: Service): boolean;
  /** Whether the service is installed under its name from before the rename to AgentK. */
  oldName(s: Service): boolean;
  /** Stops the service installed under its old name and deletes its file, so the next `install` writes it under its new one. */
  dropOldName(s: Service): void;
  /**
   * Writes both service files and starts them. A running host is never
   * restarted (that would end every session); the server always is, so a
   * reinstall or upgrade runs the new version.
   */
  install(files: Record<Service, string>): void;
  /** Restarts a service (for the host, this ends every Claude; recovery resumes them). */
  restart(s: Service): void;
  /** Whether the tmux service of versions before the session host is installed. */
  legacyTmuxInstalled(): boolean;
  /** Stops and deletes the tmux service of versions before the session host, ending every Claude in it. */
  removeLegacyTmux(): void;
  /** Starts a service that isn't running; leaves a running one alone. */
  start(s: Service): void;
  /** Stops a service until `start`, the next login or a reinstall (its supervisor doesn't bring it back sooner). */
  stop(s: Service): void;
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
  server: 'agentk.service',
  host: 'agentk-host.service',
};
/** The units' names before the rename to AgentK. */
const SYSTEMD_OLD_UNITS: Record<Service, string> = {
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
    return this.oldName(s) ? SYSTEMD_OLD_UNITS[s] : SYSTEMD_UNITS[s];
  }

  private dir(): string {
    return join(this.env.XDG_CONFIG_HOME || join(this.home, '.config'), 'systemd', 'user');
  }

  oldName(s: Service): boolean {
    return !existsSync(join(this.dir(), SYSTEMD_UNITS[s])) && existsSync(join(this.dir(), SYSTEMD_OLD_UNITS[s]));
  }

  dropOldName(s: Service): void {
    if (!this.oldName(s)) return;
    this.run('systemctl', ['--user', 'disable', '--now', SYSTEMD_OLD_UNITS[s]]);
    rmSync(join(this.dir(), SYSTEMD_OLD_UNITS[s]), { force: true });
    this.run('systemctl', ['--user', 'daemon-reload']);
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

  start(s: Service): void {
    this.run('systemctl', ['--user', 'start', this.id(s)]);
  }

  restart(s: Service): void {
    must(this.run, 'systemctl', ['--user', 'restart', this.id(s)]);
  }

  stop(s: Service): void {
    must(this.run, 'systemctl', ['--user', 'stop', this.id(s)]);
  }

  state(s: Service): { running: boolean; text: string } {
    // is-active prints the state on stdout and exits non-zero unless it is active.
    const text = this.run('systemctl', ['--user', 'is-active', this.id(s)]).out || 'inactive';
    return { running: text === 'active', text };
  }

  remove(): void {
    for (const s of ['server', 'host'] as const) {
      // Both names, should a service be installed under each.
      if (this.oldName(s)) this.dropOldName(s);
      this.run('systemctl', ['--user', 'disable', '--now', this.id(s)]);
      if (existsSync(this.file(s))) unlinkSync(this.file(s));
    }
    if (this.legacyTmuxInstalled()) this.removeLegacyTmux();
    else this.run('systemctl', ['--user', 'daemon-reload']);
  }

  legacyTmuxInstalled(): boolean {
    return existsSync(join(this.dir(), SYSTEMD_TMUX));
  }

  removeLegacyTmux(): void {
    this.run('systemctl', ['--user', 'disable', '--now', SYSTEMD_TMUX]);
    rmSync(join(this.dir(), SYSTEMD_TMUX), { force: true });
    this.run('systemctl', ['--user', 'daemon-reload']);
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
      note: `Without it, AgentK only runs while you're logged in. Ask an admin: sudo loginctl enable-linger ${user}`,
    };
  }

  statusHint(s: Service): string {
    return `systemctl --user status ${this.id(s)}`;
  }
}

// ---------------------------------------------------------------------------
// macOS: LaunchAgents in the gui/<uid> domain

const LAUNCHD_LABELS: Record<Service, string> = {
  server: 'com.yugabyte.agentk',
  host: 'com.yugabyte.agentk.host',
};
/** The jobs' labels before the rename to AgentK. */
const LAUNCHD_OLD_LABELS: Record<Service, string> = {
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
    return this.oldName(s) ? LAUNCHD_OLD_LABELS[s] : LAUNCHD_LABELS[s];
  }

  private plist(label: string): string {
    return join(this.home, 'Library', 'LaunchAgents', `${label}.plist`);
  }

  oldName(s: Service): boolean {
    return !existsSync(this.plist(LAUNCHD_LABELS[s])) && existsSync(this.plist(LAUNCHD_OLD_LABELS[s]));
  }

  dropOldName(s: Service): void {
    if (!this.oldName(s)) return;
    this.unloadLabel(LAUNCHD_OLD_LABELS[s]);
    rmSync(this.plist(LAUNCHD_OLD_LABELS[s]), { force: true });
  }

  private domain(): string {
    return `gui/${this.uid}`;
  }

  private target(s: Service): string {
    return `${this.domain()}/${this.id(s)}`;
  }

  file(s: Service): string {
    return this.plist(this.id(s));
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

  start(s: Service): void {
    if (this.loaded(s)) this.run('launchctl', ['kickstart', this.target(s)]); // without -k it leaves a running job alone
    else if (this.installed(s)) this.run('launchctl', ['bootstrap', this.domain(), this.file(s)]);
  }

  restart(s: Service): void {
    if (this.loaded(s)) must(this.run, 'launchctl', ['kickstart', '-k', this.target(s)]);
    else this.bootstrap(s);
  }

  stop(s: Service): void {
    // KeepAlive restarts a killed job: unload it (`start`, the next login or a reinstall loads it again).
    this.unload(s);
    // unload waits 5 s; the server's ExitTimeOut is 15 s.
    for (let i = 0; i < 150 && this.loaded(s); i++) this.pause(100);
    if (this.loaded(s)) throw new ServiceError(`${this.id(s)} didn't stop. Check: ${this.statusHint(s)}`);
  }

  state(s: Service): { running: boolean; text: string } {
    const r = this.run('launchctl', ['print', this.target(s)]);
    if (!r.ok) return { running: false, text: 'not loaded' };
    const text = parseLaunchdState(r.out) ?? 'unknown';
    return { running: text === 'running', text };
  }

  remove(): void {
    for (const s of ['server', 'host'] as const) {
      if (this.oldName(s)) this.dropOldName(s);
      this.unload(s);
      if (existsSync(this.file(s))) unlinkSync(this.file(s));
    }
    if (this.legacyTmuxInstalled()) this.removeLegacyTmux();
  }

  private tmuxFile(): string {
    return join(this.home, 'Library', 'LaunchAgents', `${LAUNCHD_TMUX}.plist`);
  }

  legacyTmuxInstalled(): boolean {
    return existsSync(this.tmuxFile());
  }

  removeLegacyTmux(): void {
    this.unloadLabel(LAUNCHD_TMUX);
    rmSync(this.tmuxFile(), { force: true });
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

/**
 * Before `install` writes the services, drops the ones installed under their names from before the
 * rename to AgentK, so they're written under their new ones: the server's always (`install`
 * restarts it anyway), the host's only while it isn't running, since stopping it would end every
 * session. A running host keeps its old name, which the server's unit then names, until it stops.
 * Returns each move, old name first.
 */
export function dropOldNames(svc: ServiceManager): [string, string][] {
  const moved: [string, string][] = [];
  for (const s of SERVICES) {
    if (!svc.oldName(s) || (s === 'host' && svc.state('host').running)) continue;
    const old = svc.id(s);
    svc.dropOldName(s);
    moved.push([old, svc.id(s)]);
  }
  return moved;
}

// ---------------------------------------------------------------------------
// The one-shot job that moves AgentK's home into Meko's data folder (movehome.ts). It stops the
// server and the session host, so it runs outside both services: as a transient systemd unit, or
// a LaunchAgent of its own that removes itself. Both run it again after a failure, and each run
// goes on from the move's journal.

export const MOVE_JOB = { systemd: 'agentk-move-home.service', launchd: 'com.yugabyte.agentk.move-home' } as const;

export interface MoveJob {
  /** The release's `agentk` to run (`_move-home --to <to>`). */
  bin: string;
  to: string;
  /** The home being moved, for the job's AK_HOME and its log. */
  from: string;
}

function moveJobPlist(home: string): string {
  return join(home, 'Library', 'LaunchAgents', `${MOVE_JOB.launchd}.plist`);
}

/** Whether the move job is running (or waiting to run again after a failure). */
export function moveJobRunning(kind: ServiceManager['kind'], run: Runner = runCommand, uid = process.getuid?.() ?? 0): boolean {
  if (kind === 'systemd') {
    const r = run('systemctl', ['--user', 'is-active', MOVE_JOB.systemd]);
    return r.ok || /^(activating|reloading)/.test(r.out);
  }
  return run('launchctl', ['print', `gui/${uid}/${MOVE_JOB.launchd}`]).ok;
}

/** Starts the move job unless it's running already. */
export function startMoveJob(kind: ServiceManager['kind'], job: MoveJob, run: Runner = runCommand, home = homedir(), uid = process.getuid?.() ?? 0): void {
  if (moveJobRunning(kind, run, uid)) return;
  const log = join(job.from, 'logs', 'move-home.log');
  if (kind === 'systemd') {
    // A unit that failed before stays loaded as failed: clear it, or systemd-run refuses the name.
    run('systemctl', ['--user', 'reset-failed', MOVE_JOB.systemd]);
    must(run, 'systemd-run', [
      '--user', '--unit', MOVE_JOB.systemd, '--collect', '--quiet',
      '-p', 'Restart=on-failure', '-p', 'RestartSec=30', '-p', `StandardOutput=append:${log}`, '-p', `StandardError=append:${log}`,
      '--setenv', `AK_HOME=${job.from}`, '--setenv', `PATH=${process.env.PATH ?? '/usr/bin:/bin'}`,
      job.bin, '_move-home', '--to', job.to,
    ]);
    return;
  }
  const plist = moveJobPlist(home);
  const s = (v: string) => `<string>${xmlEscape(v)}</string>`;
  mkdirSync(dirname(plist), { recursive: true });
  writeFileSync(plist, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>${s(MOVE_JOB.launchd)}
  <key>ProgramArguments</key>
  <array>${s(job.bin)}${s('_move-home')}${s('--to')}${s(job.to)}</array>
  <key>EnvironmentVariables</key>
  <dict><key>AK_HOME</key>${s(job.from)}<key>PATH</key>${s(process.env.PATH ?? '/usr/bin:/bin')}</dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>StandardOutPath</key>${s(log)}
  <key>StandardErrorPath</key>${s(log)}
</dict>
</plist>
`);
  must(run, 'launchctl', ['bootstrap', `gui/${uid}`, plist]);
}

/**
 * The move job's last step on macOS: its LaunchAgent goes, so it doesn't run again at the next
 * login. Booting it out ends this process, so it comes last. A transient systemd unit needs nothing.
 */
export function removeMoveJob(kind: ServiceManager['kind'], run: Runner = runCommand, home = homedir(), uid = process.getuid?.() ?? 0): void {
  if (kind !== 'launchd') return;
  rmSync(moveJobPlist(home), { force: true });
  run('launchctl', ['bootout', `gui/${uid}/${MOVE_JOB.launchd}`]);
}

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
    return serviceHome(readFileSync(file, 'utf8'), svc.kind) === home ? svc : null;
  } catch {
    return null;
  }
}

/** The AK_HOME a unit file or property list sets, or null when it sets none. */
export function serviceHome(text: string, kind: ServiceManager['kind']): string | null {
  if (kind === 'systemd') return /^Environment=AK_HOME=(.*)$/m.exec(text)?.[1].trim() ?? null;
  const v = /<key>AK_HOME<\/key>\s*<string>([^<]*)<\/string>/.exec(text)?.[1];
  return v === undefined ? null : v.replaceAll('&lt;', '<').replaceAll('&gt;', '>').replaceAll('&amp;', '&');
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

/** The services' names a template refers to: the ones they're installed under (`ServiceManager.id`). */
interface ServiceIds {
  SERVER_ID: string;
  HOST_ID: string;
}

/** Fills in @AK_HOME@, @HOST@ and the services' ids (XML-escaped in property lists). */
export function renderTemplate(template: string, kind: ServiceManager['kind'], vars: TemplateVars & Partial<ServiceIds>): string {
  const esc = kind === 'launchd' ? xmlEscape : (s: string) => s;
  let out = template;
  for (const [k, v] of Object.entries(vars)) out = out.replaceAll(`@${k}@`, esc(v));
  return out;
}

/** The template files in packaging/<kind>/, by their new names whatever a service is installed under. */
const TEMPLATES: Record<ServiceManager['kind'], Record<Service, string>> = {
  systemd: SYSTEMD_UNITS,
  launchd: { server: `${LAUNCHD_LABELS.server}.plist`, host: `${LAUNCHD_LABELS.host}.plist` },
};

/** Renders both service files from the templates shipped in packaging/<kind>/. */
export function renderServiceFiles(svc: ServiceManager, vars: TemplateVars): Record<Service, string> {
  const names = TEMPLATES[svc.kind];
  const dir = templatesDir(svc.kind, names.server);
  const ids = { SERVER_ID: svc.id('server'), HOST_ID: svc.id('host') };
  const one = (s: Service) => renderTemplate(readFileSync(join(dir, names[s]), 'utf8'), svc.kind, { ...vars, ...ids });
  return { server: one('server'), host: one('host') };
}
