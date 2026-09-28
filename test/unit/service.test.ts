import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { EXIT_BUSY } from '../../src/server/instance.js';
import {
  hostServices,
  LaunchdServices,
  parseLaunchdState,
  renderServiceFiles,
  renderTemplate,
  ServiceError,
  SystemdServices,
  type Runner,
} from '../../src/server/service.js';
import { tempDir } from '../helpers.js';

const ok = { ok: true, out: '', err: '' };
const fail = (err: string) => ({ ok: false, out: '', err });
const files = { server: 'server file\n', tmux: 'tmux file\n' };

const PRINT_RUNNING = `gui/501/com.yugabyte.agent-k-local = {
\tactive count = 1
\tpath = /Users/me/Library/LaunchAgents/com.yugabyte.agent-k-local.plist
\ttype = LaunchAgent
\tstate = running

\tprogram = /Users/me/.agent-k-local/app/current/bin/agent-k-local
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
    expect(renderTemplate('<string>@AK_HOME@</string>', 'launchd', { AK_HOME: '/Users/a&b', TMUX: '', HOST: 'mac' })).toBe('<string>/Users/a&amp;b</string>');
    expect(renderTemplate('Environment=AK_HOME=@AK_HOME@', 'systemd', { AK_HOME: '/home/a&b', TMUX: '', HOST: 'box' })).toBe('Environment=AK_HOME=/home/a&b');
  });

  it('renders the launchd jobs: tmux in the foreground, the server kept alive', () => {
    const svc = new LaunchdServices(() => ok, '/Users/me', 501);
    const out = renderServiceFiles(svc, { AK_HOME: '/Users/me/.agent-k-local', TMUX: '/opt/homebrew/bin/tmux', HOST: 'mac' });
    for (const text of Object.values(out)) {
      expect(text).not.toContain('@');
      expect(text).toContain('<key>KeepAlive</key>\n  <true/>');
      expect(text).toContain('<string>en_US.UTF-8</string>');
    }
    expect(out.tmux).toContain('<string>com.yugabyte.agent-k-local.tmux</string>');
    expect(out.tmux).toMatch(/<string>\/opt\/homebrew\/bin\/tmux<\/string>\s*<string>-D<\/string>\s*<string>-L<\/string>\s*<string>agentk<\/string>/);
    expect(out.server).toContain('<string>/Users/me/.agent-k-local/app/current/bin/agent-k-local</string>');
    expect(out.server).toContain('<key>AbandonProcessGroup</key>\n  <true/>');
  });

  it('renders the systemd units with a keepalive every sleep understands', () => {
    const svc = new SystemdServices(() => ok, {}, '/home/me');
    const out = renderServiceFiles(svc, { AK_HOME: '/home/me/.agent-k-local', TMUX: '/usr/bin/tmux', HOST: 'box' });
    expect(out.tmux).toContain('ExecStart=/usr/bin/tmux -L agentk -f /home/me/.agent-k-local/tmux.conf new-session -d -s _ak "sleep 2147483647"');
    expect(out.server).toContain('ExecStart=/home/me/.agent-k-local/app/current/bin/agent-k-local serve');
  });

  it('runs the systemd units only on the host they were installed on, and never restarts a refused server', () => {
    const svc = new SystemdServices(() => ok, {}, '/home/me');
    const out = renderServiceFiles(svc, { AK_HOME: '/home/me/.agent-k-local', TMUX: '/usr/bin/tmux', HOST: 'box' });
    // A home shared over NFS: every other host that mounts it would start its own server on it.
    for (const text of Object.values(out)) {
      expect(text).not.toContain('@');
      expect(text).toMatch(/^\[Unit\]\n(?:.*\n)*?ConditionHost=box\n(?:.*\n)*?\[Service\]/m);
    }
    expect(out.server).toContain(`RestartPreventExitStatus=${EXIT_BUSY}\n`);
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

  it('writes both units, starts tmux without restarting it, and restarts the server', () => {
    const { svc, calls } = make();
    svc.install(files);
    expect(readFileSync(join(dir, 'systemd', 'user', 'agent-k-local-tmux.service'), 'utf8')).toBe(files.tmux);
    expect(readFileSync(svc.file('server'), 'utf8')).toBe(files.server);
    expect(calls).toEqual([
      'systemctl --user daemon-reload',
      'systemctl --user enable --now agent-k-local-tmux.service',
      'systemctl --user enable agent-k-local.service',
      'systemctl --user restart agent-k-local.service',
    ]);
    expect(svc.installed('tmux')).toBe(true);
  });

  it('reports a failed systemctl call as a ServiceError', () => {
    const { svc } = make((a) => (a.includes('restart') ? fail('Unit failed') : ok));
    expect(() => svc.install(files)).toThrow(ServiceError);
    expect(() => svc.install(files)).toThrow(/restart agent-k-local.service failed: Unit failed/);
  });

  it('reads the state that is-active prints, even when it exits non-zero', () => {
    expect(make(() => ({ ok: true, out: 'active', err: '' })).svc.state('server')).toEqual({ running: true, text: 'active' });
    expect(make(() => ({ ok: false, out: 'failed', err: '' })).svc.state('server')).toEqual({ running: false, text: 'failed' });
    expect(make(() => fail('')).svc.state('tmux')).toEqual({ running: false, text: 'inactive' });
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

  it('removes both units', () => {
    const { svc, calls } = make();
    svc.install(files);
    calls.length = 0;
    svc.remove();
    expect(existsSync(svc.file('server'))).toBe(false);
    expect(existsSync(svc.file('tmux'))).toBe(false);
    expect(calls).toEqual([
      'systemctl --user disable --now agent-k-local.service',
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

  it('keeps its property lists in ~/Library/LaunchAgents', () => {
    const { svc } = make();
    expect(svc.file('server')).toBe(join(agents(), 'com.yugabyte.agent-k-local.plist'));
    expect(svc.file('tmux')).toBe(join(agents(), 'com.yugabyte.agent-k-local.tmux.plist'));
  });

  it('loads both jobs on a fresh install, tmux first', () => {
    const { svc, calls, loaded } = make();
    svc.install(files);
    expect(calls).toEqual([
      `bootstrap gui/501 ${svc.file('tmux')}`,
      `bootstrap gui/501 ${svc.file('server')}`,
    ]);
    expect([...loaded]).toEqual(['com.yugabyte.agent-k-local.tmux', 'com.yugabyte.agent-k-local']);
    expect(readFileSync(svc.file('tmux'), 'utf8')).toBe(files.tmux);
    expect(svc.state('server')).toEqual({ running: true, text: 'running' });
  });

  it('never reloads a loaded tmux job, and restarts an unchanged server in place', () => {
    const { svc, calls } = make({ loaded: ['com.yugabyte.agent-k-local.tmux', 'com.yugabyte.agent-k-local'] });
    mkdirSync(agents(), { recursive: true });
    writeFileSync(svc.file('tmux'), 'older tmux file\n');
    writeFileSync(svc.file('server'), files.server);
    svc.install(files);
    expect(calls).toEqual(['kickstart -k gui/501/com.yugabyte.agent-k-local']);
    expect(readFileSync(svc.file('tmux'), 'utf8')).toBe(files.tmux);
  });

  it('reloads the server job when its file changed', () => {
    const { svc, calls } = make({ loaded: ['com.yugabyte.agent-k-local.tmux', 'com.yugabyte.agent-k-local'] });
    mkdirSync(agents(), { recursive: true });
    writeFileSync(svc.file('server'), 'older server file\n');
    svc.install(files);
    expect(calls).toEqual([
      'bootout gui/501/com.yugabyte.agent-k-local',
      `bootstrap gui/501 ${svc.file('server')}`,
    ]);
  });

  it('refuses to install without a desktop login', () => {
    const { svc } = make({ gui: false });
    expect(() => svc.preflight()).toThrow(ServiceError);
    expect(() => svc.preflight()).toThrow(/Log in to the desktop/);
    expect(() => make().svc.preflight()).not.toThrow();
  });

  it('starts a job without disturbing a running one', () => {
    const running = make({ loaded: ['com.yugabyte.agent-k-local.tmux'] });
    running.svc.start('tmux');
    expect(running.calls).toEqual(['kickstart gui/501/com.yugabyte.agent-k-local.tmux']);

    const unloaded = make();
    mkdirSync(dirname(unloaded.svc.file('tmux')), { recursive: true });
    writeFileSync(unloaded.svc.file('tmux'), files.tmux);
    unloaded.svc.start('tmux');
    expect(unloaded.calls).toEqual([`bootstrap gui/501 ${unloaded.svc.file('tmux')}`]);
  });

  it('reports an unloaded job', () => {
    expect(make().svc.state('tmux')).toEqual({ running: false, text: 'not loaded' });
  });

  it('removes both jobs and their files', () => {
    const { svc, calls, loaded } = make();
    svc.install(files);
    calls.length = 0;
    svc.remove();
    expect(calls).toEqual(['bootout gui/501/com.yugabyte.agent-k-local', 'bootout gui/501/com.yugabyte.agent-k-local.tmux']);
    expect(loaded.size).toBe(0);
    expect(existsSync(svc.file('server'))).toBe(false);
    expect(existsSync(svc.file('tmux'))).toBe(false);
  });

  it('says the services start at login', () => {
    expect(make().svc.bootCheck()).toMatchObject({ ok: true, label: 'Starts', detail: 'when you log in to this Mac' });
  });
});
