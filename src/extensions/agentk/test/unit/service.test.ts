import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readdirSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { EXIT_BUSY } from '../../src/server/instance.js';
import {
  dropOldNames,
  hostServices,
  LaunchdServices,
  parseLaunchdState,
  refreshServiceFiles,
  removeServiceLauncher,
  renderServiceFiles,
  renderTemplate,
  ServiceError,
  serviceHome,
  serviceLauncher,
  servicesFor,
  SystemdServices,
  writeServiceLauncher,
  type Runner,
} from '../../src/server/service.js';
import { tempDir } from '../helpers.js';

const ok = { ok: true, out: '', err: '' };
const fail = (err: string) => ({ ok: false, out: '', err });
const files = { server: 'server file\n', host: 'host file\n' };

const PRINT_RUNNING = `gui/501/com.yugabyte.me_kode = {
\tactive count = 1
\tpath = /Users/me/Library/LaunchAgents/com.yugabyte.me_kode.plist
\ttype = LaunchAgent
\tstate = running

\tprogram = /Users/me/.agent-k-local/service/me_kode
\tendpoints = {
\t\tstate = active
\t}
}`;

describe('parseLaunchdState', () => {
  it('reads the job state, not a nested one', () => {
    expect(parseLaunchdState(PRINT_RUNNING)).toBe('running');
    expect(parseLaunchdState(PRINT_RUNNING.replace('state = running', 'state = not running'))).toBe('not running');
    expect(parseLaunchdState('nothing here')).toBeNull();
  });
});

describe('hostServices', () => {
  it('picks the service manager for the platform', () => {
    expect(hostServices('linux')?.kind).toBe('systemd');
    expect(hostServices('darwin')?.kind).toBe('launchd');
    expect(hostServices('win32')).toBeNull();
  });
});

describe('service templates', () => {
  it('escapes values for property lists only', () => {
    expect(renderTemplate('<string>@AK_HOME@</string>', 'launchd', { AK_HOME: '/Users/a&b', HOST: 'mac' })).toBe('<string>/Users/a&amp;b</string>');
    expect(renderTemplate('Environment=AK_HOME=@AK_HOME@', 'systemd', { AK_HOME: '/home/a&b', HOST: 'box' })).toBe('Environment=AK_HOME=/home/a&b');
  });

  it('renders the launchd jobs: the session host and the server, both kept alive', () => {
    const svc = new LaunchdServices(() => ok, '/Users/me', 501);
    const out = renderServiceFiles(svc, { AK_HOME: '/Users/me/.agent-k-local', HOST: 'mac' });
    for (const text of Object.values(out)) {
      expect(text).not.toContain('@');
      expect(text).toContain('<key>KeepAlive</key>\n  <true/>');
      expect(text).toContain('<string>en_US.UTF-8</string>');
    }
    expect(out.host).toContain('<string>com.yugabyte.me_kode.host</string>');
    // The launcher, which updates never replace, so macOS doesn't announce the jobs again after each one.
    expect(out.host).toMatch(/<string>\/Users\/me\/.agent-k-local\/service\/me_kode<\/string>\s*<string>host<\/string>/);
    expect(out.server).toMatch(/<string>\/Users\/me\/.agent-k-local\/service\/me_kode<\/string>\s*<string>serve<\/string>/);
    for (const text of Object.values(out)) expect(text).not.toMatch(/app\/current\/bin\/[\w-]+<\/string>/);
    // The server never starts a host of its own under its job, so nothing it starts must outlive it.
    expect(out.server).not.toContain('AbandonProcessGroup');
  });

  it('renders the systemd units: the session host, and the server that wants it, run only on this host and not restarted when busy', () => {
    const svc = new SystemdServices(() => ok, {}, '/home/me');
    const out = renderServiceFiles(svc, { AK_HOME: '/home/me/.agent-k-local', HOST: 'box' });
    // The launcher, as on macOS: it runs a release from before the program was me_kode too.
    expect(out.host).toContain('ExecStart=/home/me/.agent-k-local/service/me_kode host\n');
    expect(out.server).toContain('ExecStart=/home/me/.agent-k-local/service/me_kode serve\n');
    expect(out.server).toContain('Wants=me_kode-host.service\n');
    expect(out.server).not.toContain('tmux');
    // A home shared over NFS: every other host that mounts it would start its own server on it.
    for (const text of Object.values(out)) {
      expect(text).not.toContain('@');
      expect(text).toMatch(/^\[Unit\]\n(?:.*\n)*?ConditionHost=box\n(?:.*\n)*?\[Service\]/m);
    }
    expect(out.server).toContain(`RestartPreventExitStatus=${EXIT_BUSY}\n`);
    // A second host on one home exits busy too.
    expect(out.host).toContain(`RestartPreventExitStatus=${EXIT_BUSY}\n`);
  });
});

describe('servicesFor', () => {
  let dir: string;
  beforeEach(() => {
    dir = tempDir('akl-svcfor-');
  });
  afterEach(() => rmSync(dir, { recursive: true, force: true }));

  it("uses the services only when they run this home, so a dev server never waits on an install's", () => {
    const svc = new SystemdServices(() => ok, { XDG_CONFIG_HOME: dir }, '/home/me');
    expect(servicesFor('/home/me/.agent-k-local', svc)).toBe(svc); // nothing installed yet
    svc.install({ server: 'Environment=AK_HOME=/home/me/.agent-k-local\n', host: 'host\n' });
    expect(servicesFor('/home/me/.agent-k-local', svc)).toBe(svc);
    expect(servicesFor('/home/me/code/agent-k-local/.dev-home', svc)).toBeNull();
    // A home whose path starts another's is still another home.
    expect(servicesFor('/home/me/.agent-k', svc)).toBeNull();
    expect(servicesFor('/x', null)).toBeNull();
  });

  it("matches a unit that names the home through a link (a release's home is its real path)", () => {
    const real = join(dir, 'meko', 'extensions', 'agentk');
    mkdirSync(real, { recursive: true });
    symlinkSync(real, join(dir, 'old-home'));
    const svc = new SystemdServices(() => ok, { XDG_CONFIG_HOME: join(dir, 'config') }, dir);
    svc.install({ server: `Environment=AK_HOME=${join(dir, 'old-home')}\n`, host: 'host\n' });
    expect(servicesFor(real, svc)).toBe(svc);
  });

  it("reads a unit's or a property list's AK_HOME exactly", () => {
    const vars = { AK_HOME: '/Users/a&b/.agent-k-local', HOST: 'mac' };
    const launchd = renderServiceFiles(new LaunchdServices(() => ok, '/Users/a&b', 501), vars);
    expect(serviceHome(launchd.server, 'launchd')).toBe('/Users/a&b/.agent-k-local');
    expect(serviceHome(launchd.host, 'launchd')).toBe('/Users/a&b/.agent-k-local');
    const systemd = renderServiceFiles(new SystemdServices(() => ok, {}, '/home/me'), { AK_HOME: '/home/me/.agent-k-local', HOST: 'box' });
    expect(serviceHome(systemd.server, 'systemd')).toBe('/home/me/.agent-k-local');
    expect(serviceHome('ExecStart=/home/me/.agent-k-local/app/current/bin/agentk serve', 'systemd')).toBeNull();
  });
});

describe('SystemdServices', () => {
  let dir: string;
  beforeEach(() => {
    dir = tempDir('akl-svc-');
  });
  afterEach(() => rmSync(dir, { recursive: true, force: true }));

  function make(respond: (args: string[]) => ReturnType<Runner> = () => ok) {
    const calls: string[] = [];
    const run: Runner = (cmd, args) => {
      calls.push([cmd, ...args].join(' '));
      return respond([cmd, ...args]);
    };
    return { svc: new SystemdServices(run, { XDG_CONFIG_HOME: dir }, '/home/me', () => 'me'), calls };
  }

  it('writes both units, starts the host without restarting it, and restarts the server', () => {
    const { svc, calls } = make();
    svc.install(files);
    expect(readFileSync(join(dir, 'systemd', 'user', 'me_kode-host.service'), 'utf8')).toBe(files.host);
    expect(readFileSync(svc.file('server'), 'utf8')).toBe(files.server);
    expect(calls).toEqual([
      'systemctl --user daemon-reload',
      'systemctl --user enable --now me_kode-host.service',
      'systemctl --user enable me_kode.service',
      'systemctl --user restart me_kode.service',
    ]);
    expect(svc.installed('host')).toBe(true);
  });

  it('stops a unit', () => {
    const { svc, calls } = make();
    svc.stop('server');
    expect(calls).toEqual(['systemctl --user stop me_kode.service']);
  });

  it('reports a failed systemctl call as a ServiceError', () => {
    const { svc } = make((a) => (a.includes('restart') ? fail('Unit failed') : ok));
    expect(() => svc.install(files)).toThrow(ServiceError);
    expect(() => svc.install(files)).toThrow(/restart me_kode.service failed: Unit failed/);
  });

  it('reads the state that is-active prints, even when it exits non-zero', () => {
    expect(make(() => ({ ok: true, out: 'active', err: '' })).svc.state('server')).toEqual({ running: true, text: 'active' });
    expect(make(() => ({ ok: false, out: 'failed', err: '' })).svc.state('server')).toEqual({ running: false, text: 'failed' });
    expect(make(() => fail('')).svc.state('host')).toEqual({ running: false, text: 'inactive' });
  });

  it('turns lingering on for boot, and explains when it cannot', () => {
    expect(make((a) => (a[1] === 'show-user' ? { ok: true, out: 'yes', err: '' } : ok)).svc.bootCheck()).toMatchObject({ ok: true, detail: 'lingering is on for your account' });
    expect(make((a) => (a[1] === 'show-user' ? { ok: true, out: 'no', err: '' } : ok)).svc.bootCheck()).toMatchObject({ ok: true, detail: 'lingering enabled for your account' });
    const denied = make((a) => (a[1] === 'enable-linger' ? fail('Access denied') : { ok: true, out: 'no', err: '' })).svc.bootCheck();
    expect(denied).toMatchObject({ ok: false });
    expect(denied.note).toContain('sudo loginctl enable-linger me');
  });

  it('needs systemctl', () => {
    expect(() => make((a) => (a[0] === 'sh' ? fail('') : ok)).svc.preflight()).toThrow(/systemctl was not found/);
  });

  it('keeps units installed before the rename to AgentK under their old names until install moves them, and never a running host', () => {
    const units = join(dir, 'systemd', 'user');
    mkdirSync(units, { recursive: true });
    writeFileSync(join(units, 'agent-k-local.service'), 'old server\n');
    writeFileSync(join(units, 'agent-k-local-host.service'), 'old host\n');
    const vars = { AK_HOME: '/home/me/.agent-k-local', HOST: 'box' };
    const hostActive = (a: string[]) => (a.join(' ') === 'systemctl --user is-active agent-k-local-host.service' ? { ok: true, out: 'active', err: '' } : ok);
    const { svc, calls } = make(hostActive);
    expect([svc.id('server'), svc.id('host')]).toEqual(['agent-k-local.service', 'agent-k-local-host.service']);
    expect(svc.statusHint('host')).toBe('systemctl --user status agent-k-local-host.service');

    expect(dropOldNames(svc)).toEqual([['agent-k-local.service', 'me_kode.service']]);
    expect(calls).toEqual([
      'systemctl --user is-active agent-k-local-host.service',
      'systemctl --user disable --now agent-k-local.service',
      'systemctl --user daemon-reload',
    ]);
    expect(existsSync(join(units, 'agent-k-local.service'))).toBe(false);
    // The server's new unit wants the host by the name it still runs under.
    expect(renderServiceFiles(svc, vars).server).toContain('Wants=agent-k-local-host.service\n');

    // Once the host stops, the next install moves it too; uninstalling removes either name.
    const stopped = make().svc;
    expect(dropOldNames(stopped)).toEqual([['agent-k-local-host.service', 'me_kode-host.service']]);
    expect(renderServiceFiles(stopped, vars).server).toContain('Wants=me_kode-host.service\n');
    writeFileSync(join(units, 'agent-k-local-host.service'), 'old host\n');
    stopped.remove();
    expect(readdirSync(units)).toEqual([]);
  });

  it('keeps units installed as agentk under that name until install moves them, and drops both old names', () => {
    const units = join(dir, 'systemd', 'user');
    mkdirSync(units, { recursive: true });
    writeFileSync(join(units, 'agentk.service'), 'server\n');
    writeFileSync(join(units, 'agentk-host.service'), 'host\n');
    // Left by a version before the rename to AgentK too: agentk's is the one in use.
    writeFileSync(join(units, 'agent-k-local.service'), 'older server\n');
    const { svc, calls } = make();
    expect([svc.id('server'), svc.id('host')]).toEqual(['agentk.service', 'agentk-host.service']);
    expect(renderServiceFiles(svc, { AK_HOME: '/home/me/.agent-k-local', HOST: 'box' }).server).toContain('Wants=agentk-host.service\n');
    expect(dropOldNames(svc)).toEqual([['agentk-host.service', 'me_kode-host.service'], ['agentk.service', 'me_kode.service']]);
    expect(calls).toEqual([
      'systemctl --user is-active agentk-host.service',
      'systemctl --user disable --now agentk-host.service',
      'systemctl --user daemon-reload',
      'systemctl --user disable --now agentk.service',
      'systemctl --user disable --now agent-k-local.service',
      'systemctl --user daemon-reload',
    ]);
    expect(readdirSync(units)).toEqual([]);
  });

  it('keeps units installed as me_koder under that name until install moves them, and drops the older names too', () => {
    const units = join(dir, 'systemd', 'user');
    mkdirSync(units, { recursive: true });
    writeFileSync(join(units, 'me_koder.service'), 'server\n');
    writeFileSync(join(units, 'me_koder-host.service'), 'host\n');
    // Left by a version from before: me_koder's is the one in use.
    writeFileSync(join(units, 'agentk.service'), 'older server\n');
    const { svc, calls } = make();
    expect([svc.id('server'), svc.id('host')]).toEqual(['me_koder.service', 'me_koder-host.service']);
    expect(svc.oldName('server')).toBe(true);
    expect(renderServiceFiles(svc, { AK_HOME: '/home/me/.agent-k-local', HOST: 'box' }).server).toContain('Wants=me_koder-host.service\n');
    expect(dropOldNames(svc)).toEqual([['me_koder-host.service', 'me_kode-host.service'], ['me_koder.service', 'me_kode.service']]);
    expect(calls).toEqual([
      'systemctl --user is-active me_koder-host.service',
      'systemctl --user disable --now me_koder-host.service',
      'systemctl --user daemon-reload',
      'systemctl --user disable --now me_koder.service',
      'systemctl --user disable --now agentk.service',
      'systemctl --user daemon-reload',
    ]);
    expect(readdirSync(units)).toEqual([]);
  });

  it('removes both units, and the tmux unit of a version before the session host', () => {
    const { svc, calls } = make();
    svc.install(files);
    const tmuxUnit = join(dirname(svc.file('server')), 'agent-k-local-tmux.service');
    writeFileSync(tmuxUnit, 'tmux unit\n');
    expect(svc.legacyTmuxInstalled()).toBe(true);
    calls.length = 0;
    svc.remove();
    expect(existsSync(svc.file('server'))).toBe(false);
    expect(existsSync(svc.file('host'))).toBe(false);
    expect(svc.legacyTmuxInstalled()).toBe(false);
    expect(calls).toEqual([
      'systemctl --user disable --now me_kode.service',
      'systemctl --user disable --now me_kode-host.service',
      'systemctl --user disable --now agent-k-local-tmux.service',
      'systemctl --user daemon-reload',
    ]);
  });
});

describe('LaunchdServices', () => {
  let home: string;
  beforeEach(() => {
    home = tempDir('akl-launchd-');
  });
  afterEach(() => rmSync(home, { recursive: true, force: true }));

  /** A stand-in for launchctl that tracks which jobs are loaded in gui/501. */
  function make(opts: { gui?: boolean; loaded?: string[] } = {}) {
    const loaded = new Set(opts.loaded ?? []);
    const calls: string[] = [];
    const label = (target: string) => target.split('/').slice(2).join('/');
    const run: Runner = (cmd, args) => {
      if (cmd !== 'launchctl') return fail(`unexpected ${cmd}`);
      const [verb, ...rest] = args;
      if (verb !== 'print') calls.push(args.join(' '));
      const target = rest.at(-1) ?? '';
      switch (verb) {
        case 'print':
          if (target === 'gui/501') return opts.gui === false ? fail('Could not find domain') : ok;
          return loaded.has(label(target)) ? { ok: true, out: PRINT_RUNNING, err: '' } : fail('Could not find service');
        case 'bootstrap':
          loaded.add(basename(target, '.plist'));
          return ok;
        case 'bootout':
          loaded.delete(label(target));
          return ok;
        default:
          return ok;
      }
    };
    return { svc: new LaunchdServices(run, home, 501, () => {}), calls, loaded };
  }

  const agents = () => join(home, 'Library', 'LaunchAgents');

  it('loads both jobs from ~/Library/LaunchAgents on a fresh install, the host first', () => {
    const { svc, calls, loaded } = make();
    expect(svc.state('host')).toEqual({ running: false, text: 'not loaded' });
    svc.install(files);
    const host = join(agents(), 'com.yugabyte.me_kode.host.plist');
    expect(calls).toEqual([`bootstrap gui/501 ${host}`, `bootstrap gui/501 ${join(agents(), 'com.yugabyte.me_kode.plist')}`]);
    expect([...loaded]).toEqual(['com.yugabyte.me_kode.host', 'com.yugabyte.me_kode']);
    expect(readFileSync(host, 'utf8')).toBe(files.host);
    expect(svc.state('server')).toEqual({ running: true, text: 'running' });
  });

  it('never reloads a loaded host job, and reloads the server even when its file is unchanged', () => {
    // The server's start may have written the file without reloading the job (refresh).
    const { svc, calls } = make({ loaded: ['com.yugabyte.me_kode.host', 'com.yugabyte.me_kode'] });
    mkdirSync(agents(), { recursive: true });
    writeFileSync(svc.file('host'), 'older host file\n');
    writeFileSync(svc.file('server'), files.server);
    svc.install(files);
    expect(calls).toEqual(['bootout gui/501/com.yugabyte.me_kode', `bootstrap gui/501 ${svc.file('server')}`]);
    expect(readFileSync(svc.file('host'), 'utf8')).toBe(files.host);
  });

  it('unloads a job to stop it, and leaves an unloaded one alone', () => {
    const { svc, calls, loaded } = make({ loaded: ['com.yugabyte.me_kode'] });
    svc.stop('server');
    svc.stop('host');
    expect(calls).toEqual(['bootout gui/501/com.yugabyte.me_kode']);
    expect(loaded.size).toBe(0);
  });

  it('says so when a job is still loaded after bootout', () => {
    // launchctl print keeps finding the job: bootout failed, or it's still exiting.
    const stuck = new LaunchdServices(() => ok, home, 501, () => {});
    expect(() => stuck.stop('server')).toThrow(/com.yugabyte.me_kode didn't stop/);
  });

  it('reloads the server job when its file changed', () => {
    const { svc, calls } = make({ loaded: ['com.yugabyte.me_kode.host', 'com.yugabyte.me_kode'] });
    mkdirSync(agents(), { recursive: true });
    writeFileSync(svc.file('server'), 'older server file\n');
    svc.install(files);
    expect(calls).toEqual([
      'bootout gui/501/com.yugabyte.me_kode',
      `bootstrap gui/501 ${svc.file('server')}`,
    ]);
  });

  it('leaves files whose content is the same alone, since macOS announces a rewritten one as a new background item', () => {
    const { svc } = make({ loaded: ['com.yugabyte.me_kode.host', 'com.yugabyte.me_kode'] });
    svc.install(files);
    const before = statSync(svc.file('host'), { bigint: true });
    svc.install(files);
    expect(svc.refresh(files)).toEqual([]);
    const after = statSync(svc.file('host'), { bigint: true });
    expect([after.ino, after.mtimeNs]).toEqual([before.ino, before.mtimeNs]);
  });

  it('refreshes changed files of installed jobs without reloading them', () => {
    const { svc, calls } = make({ loaded: ['com.yugabyte.me_kode.host', 'com.yugabyte.me_kode'] });
    mkdirSync(agents(), { recursive: true });
    writeFileSync(svc.file('server'), 'older server file\n');
    writeFileSync(svc.file('host'), files.host);
    expect(svc.refresh(files)).toEqual(['server']);
    expect(readFileSync(svc.file('server'), 'utf8')).toBe(files.server);
    expect(calls).toEqual([]);
    // A job that isn't installed stays that way.
    rmSync(svc.file('host'));
    expect(svc.refresh({ ...files, server: 'newer\n' })).toEqual(['server']);
    expect(existsSync(svc.file('host'))).toBe(false);
  });

  it('refuses to install without a desktop login', () => {
    const { svc } = make({ gui: false });
    expect(() => svc.preflight()).toThrow(ServiceError);
    expect(() => svc.preflight()).toThrow(/Log in to the desktop/);
    expect(() => make().svc.preflight()).not.toThrow();
  });

  it('starts a job without disturbing a running one', () => {
    const running = make({ loaded: ['com.yugabyte.me_kode.host'] });
    running.svc.start('host');
    expect(running.calls).toEqual(['kickstart gui/501/com.yugabyte.me_kode.host']);

    const unloaded = make();
    mkdirSync(dirname(unloaded.svc.file('host')), { recursive: true });
    writeFileSync(unloaded.svc.file('host'), files.host);
    unloaded.svc.start('host');
    expect(unloaded.calls).toEqual([`bootstrap gui/501 ${unloaded.svc.file('host')}`]);
  });

  it('labels a job installed before the rename to AgentK by its old label until install moves it', () => {
    const { svc, calls, loaded } = make({ loaded: ['com.yugabyte.agent-k-local'] });
    mkdirSync(agents(), { recursive: true });
    writeFileSync(join(agents(), 'com.yugabyte.agent-k-local.plist'), 'old server\n');
    expect(svc.id('server')).toBe('com.yugabyte.agent-k-local');
    const vars = { AK_HOME: '/Users/me/.agent-k-local', HOST: 'mac' };
    // The file under the old name keeps its label, or launchctl wouldn't find the job it loads.
    expect(renderServiceFiles(svc, vars).server).toContain('<key>Label</key>\n  <string>com.yugabyte.agent-k-local</string>');
    expect(dropOldNames(svc)).toEqual([['com.yugabyte.agent-k-local', 'com.yugabyte.me_kode']]);
    expect(calls).toEqual(['bootout gui/501/com.yugabyte.agent-k-local']);
    expect(loaded.size).toBe(0);
    expect(existsSync(join(agents(), 'com.yugabyte.agent-k-local.plist'))).toBe(false);
    expect(renderServiceFiles(svc, vars).server).toContain('<key>Label</key>\n  <string>com.yugabyte.me_kode</string>');
  });

  it('labels a job installed as me_koder by that label until install moves it', () => {
    const { svc, calls, loaded } = make({ loaded: ['com.yugabyte.me_koder.host'] });
    mkdirSync(agents(), { recursive: true });
    writeFileSync(join(agents(), 'com.yugabyte.me_koder.host.plist'), 'host\n');
    expect(svc.id('host')).toBe('com.yugabyte.me_koder.host');
    expect(svc.id('server')).toBe('com.yugabyte.me_kode');
    // A running host keeps its old label: stopping it would end every session.
    expect(dropOldNames(svc)).toEqual([]);
    loaded.clear();
    expect(dropOldNames(svc)).toEqual([['com.yugabyte.me_koder.host', 'com.yugabyte.me_kode.host']]);
    expect(calls).toEqual([]); // not loaded, so nothing to boot out
    expect(existsSync(join(agents(), 'com.yugabyte.me_koder.host.plist'))).toBe(false);
  });

  it('removes both jobs and their files, and the tmux job of a version before the session host', () => {
    const { svc, calls, loaded } = make();
    svc.install(files);
    writeFileSync(join(agents(), 'com.yugabyte.agent-k-local.tmux.plist'), 'tmux job\n');
    loaded.add('com.yugabyte.agent-k-local.tmux');
    expect(svc.legacyTmuxInstalled()).toBe(true);
    calls.length = 0;
    svc.remove();
    expect(calls).toEqual([
      'bootout gui/501/com.yugabyte.me_kode', 'bootout gui/501/com.yugabyte.me_kode.host', 'bootout gui/501/com.yugabyte.agent-k-local.tmux',
    ]);
    expect(loaded.size).toBe(0);
    expect(existsSync(svc.file('server'))).toBe(false);
    expect(existsSync(svc.file('host'))).toBe(false);
    expect(svc.legacyTmuxInstalled()).toBe(false);
  });
});

describe('the LaunchAgents launcher', () => {
  let dir: string;
  beforeEach(() => {
    dir = tempDir('akl-launcher-');
  });
  afterEach(() => {
    vi.unstubAllEnvs();
    rmSync(dir, { recursive: true, force: true });
  });

  /** A release home whose app/current/bin/<command> (me_kode, or agentk before) prints its name and arguments. */
  function releaseHome(name: string, command = 'me_kode'): string {
    const home = join(dir, name);
    mkdirSync(join(home, 'app', '1.0', 'bin'), { recursive: true });
    writeFileSync(join(home, 'app', '1.0', 'bin', command), `#!/bin/sh\nprintf '%s|' ${command} "$@"\n`, { mode: 0o755 });
    symlinkSync('1.0', join(home, 'app', 'current'));
    return home;
  }

  it("runs app/current/bin/me_kode with the job's arguments, and is written only when it changes", () => {
    const home = releaseHome("it's home");
    expect(writeServiceLauncher(home)).toBe(true);
    const launcher = serviceLauncher(home);
    expect(launcher).toBe(join(home, 'service', 'me_kode'));
    expect(statSync(launcher).mode & 0o777).toBe(0o755);
    expect(execFileSync(launcher, ['serve', 'a b'], { encoding: 'utf8' })).toBe('me_kode|serve|a b|');
    const before = statSync(launcher, { bigint: true });
    expect(writeServiceLauncher(home)).toBe(false);
    const after = statSync(launcher, { bigint: true });
    expect([after.ino, after.mtimeNs]).toEqual([before.ino, before.mtimeNs]);
    removeServiceLauncher(home);
    expect(existsSync(join(home, 'service'))).toBe(false);
  });

  it('runs bin/agentk in a release from before the program was me_kode (one rolled back to)', () => {
    const home = releaseHome('home', 'agentk');
    writeServiceLauncher(home);
    expect(execFileSync(serviceLauncher(home), ['host'], { encoding: 'utf8' })).toBe('agentk|host|');
  });

  it('runs bin/agentk in a release whose program was me_koder, which Meko has not given the new name', () => {
    const home = releaseHome('home', 'me_koder');
    symlinkSync('me_koder', join(home, 'app', '1.0', 'bin', 'agentk'));
    writeServiceLauncher(home);
    expect(execFileSync(serviceLauncher(home), ['serve'], { encoding: 'utf8' })).toBe('me_koder|serve|');
  });

  it("deletes the launcher by every name it had, which the units and jobs installed then run, at uninstall", () => {
    const home = releaseHome('home');
    writeServiceLauncher(home);
    for (const old of ['me_koder', 'me_agentk']) writeFileSync(join(home, 'service', old), '#!/bin/sh\n', { mode: 0o755 });
    // Written again, the launcher leaves its old names alone: units and jobs installed then still run them.
    rmSync(serviceLauncher(home));
    writeServiceLauncher(home);
    expect(readdirSync(join(home, 'service')).sort()).toEqual(['me_agentk', 'me_kode', 'me_koder']);
    removeServiceLauncher(home);
    expect(existsSync(join(home, 'service'))).toBe(false);
  });

  it.each([
    // Before the launcher (#430), the jobs ran the release's program.
    ['app/current/bin/agentk', false],
    // The launcher's names before the program was me_kode: the jobs launchd loaded run them until
    // the next login, so they stay until uninstall.
    ['service/me_agentk', true],
    ['service/me_koder', true],
  ])("moves a release's LaunchAgents that ran %s onto the launcher at the server's start, under the label they're installed with", (program, oldLauncher) => {
    const home = releaseHome('home');
    const svc = new LaunchdServices(() => ok, join(dir, 'user'), 501, () => {});
    const vars = { AK_HOME: home, HOST: 'mac' };
    const current = renderServiceFiles(svc, vars);
    const old = (text: string) => text.replaceAll(`${home}/service/me_kode`, `${home}/${program}`);
    mkdirSync(dirname(svc.file('server')), { recursive: true });
    writeFileSync(svc.file('server'), old(current.server));
    writeFileSync(svc.file('host'), old(current.host));
    if (oldLauncher) {
      mkdirSync(join(home, 'service'));
      writeFileSync(join(home, program), '#!/bin/sh\n', { mode: 0o755 });
    }

    expect(refreshServiceFiles(home, svc, 'mac')).toEqual([]); // not a release (no AK_APP_DIR)
    vi.stubEnv('AK_APP_DIR', join(home, 'app', '1.0'));
    expect(refreshServiceFiles(home, null, 'mac')).toEqual([]);
    expect(refreshServiceFiles(home, new SystemdServices(() => ok, { XDG_CONFIG_HOME: dir }, dir), 'box')).toEqual([]);
    expect(refreshServiceFiles(home, svc, 'mac')).toEqual(['host', 'server']);
    expect(readFileSync(svc.file('server'), 'utf8')).toBe(current.server);
    expect(readFileSync(svc.file('host'), 'utf8')).toBe(current.host);
    expect(existsSync(serviceLauncher(home))).toBe(true);
    expect(refreshServiceFiles(home, svc, 'mac')).toEqual([]);
    if (oldLauncher) {
      expect(existsSync(join(home, program))).toBe(true);
      removeServiceLauncher(home);
      expect(existsSync(join(home, 'service'))).toBe(false);
    }
  });
});
