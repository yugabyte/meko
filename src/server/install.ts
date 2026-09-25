import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, rmSync, unlinkSync, writeFileSync } from 'node:fs';
import { homedir, hostname, userInfo } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { createInterface } from 'node:readline/promises';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';
import { getOwner, issueSetupToken } from './auth.js';
import { dbPath, isLoopback, loadConfig, readClaudeSettingsEnv, resolveMeko, saveConfig, type Config } from './config.js';
import { openDb } from './db.js';
import { checkMeko } from './meko.js';
import { expandHome, getPaths, tildify, type Paths } from './paths.js';
import { writeTmuxConf } from './tmux.js';
import { style, ui } from './tty.js';
import { waitForServer } from './health.js';
import { loadTls } from './tls.js';

const SERVER_UNIT = 'agent-k-local.service';
const TMUX_UNIT = 'agent-k-local-tmux.service';
const NETWORK_FS = new Set(['nfs', 'nfs4', 'cifs', 'smb2', 'smbfs', 'fuse.sshfs', 'afs', 'lustre', 'gpfs']);

class InstallError extends Error {}

const say = (msg = '') => process.stdout.write(msg + '\n');

function sh(cmd: string, args: string[], opts: { quiet?: boolean } = {}): string {
  return execFileSync(cmd, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', opts.quiet ? 'ignore' : 'pipe'] }).trim();
}

function trySh(cmd: string, args: string[]): { ok: true; out: string } | { ok: false; err: string } {
  try {
    return { ok: true, out: sh(cmd, args) };
  } catch (e) {
    const err = e as { stderr?: string; message: string };
    return { ok: false, err: (err.stderr || err.message).toString().trim() };
  }
}

function which(bin: string): string | null {
  const r = trySh('sh', ['-c', `command -v ${bin}`]);
  return r.ok && r.out ? r.out : null;
}

type Prompter = (question: string, def: string) => Promise<string>;

function makePrompter(interactive: boolean): { ask: Prompter; close: () => void } {
  if (!interactive || !process.stdin.isTTY) {
    return { ask: async (_q, def) => def, close: () => {} };
  }
  const rl = createInterface({ input: process.stdin, output: process.stdout });
  return {
    ask: async (q, def) => {
      const a = (await rl.question(ui.prompt(q, def))).trim();
      return a || def;
    },
    close: () => rl.close(),
  };
}

// ---------------------------------------------------------------------------
// Steps

/** True when the meko-agent-skills plugin is installed and enabled for Claude Code. */
export function mekoPluginEnabled(claudeDir: string = join(homedir(), '.claude')): { installed: boolean; enabled: boolean } {
  const isMeko = (k: string) => k.startsWith('meko-agent-skills@');
  let installed = false;
  let enabled = false;
  try {
    const j = JSON.parse(readFileSync(join(claudeDir, 'plugins', 'installed_plugins.json'), 'utf8'));
    const plugins = j && typeof j.plugins === 'object' ? j.plugins : j;
    installed = Object.keys(plugins ?? {}).some(isMeko);
  } catch {
    // no plugin registry
  }
  try {
    const s = JSON.parse(readFileSync(join(claudeDir, 'settings.json'), 'utf8'));
    const ep = (s?.enabledPlugins ?? {}) as Record<string, unknown>;
    enabled = Object.entries(ep).some(([k, v]) => isMeko(k) && v === true);
  } catch {
    // no settings
  }
  return { installed, enabled };
}

export function fsType(dir: string): string | null {
  const r = trySh('stat', ['-f', '-c', '%T', dir]);
  return r.ok ? r.out : null;
}

export function isNetworkFs(dir: string): boolean {
  const t = fsType(dir);
  return !!t && NETWORK_FS.has(t);
}

/** Pick a local-disk location for state.db when the default is on a network filesystem. */
function localDbLocation(): string {
  const user = userInfo().username;
  const xdg = process.env.XDG_STATE_HOME;
  if (xdg) {
    mkdirSync(join(xdg, 'agent-k-local'), { recursive: true });
    if (!isNetworkFs(join(xdg, 'agent-k-local'))) return join(xdg, 'agent-k-local', 'state.db');
  }
  const dir = join('/var/tmp', user, 'agent-k-local');
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  return join(dir, 'state.db');
}

function templatesDir(): string {
  const candidates = [
    process.env.AK_APP_DIR ? join(process.env.AK_APP_DIR, 'lib', 'packaging', 'systemd') : '',
    // dev checkout: src/server/install.ts or dist/server/install.js → repo root
    resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', 'packaging', 'systemd'),
  ].filter(Boolean);
  const found = candidates.find((d) => existsSync(join(d, SERVER_UNIT)));
  if (!found) throw new InstallError(`systemd unit templates not found (looked in ${candidates.join(', ')})`);
  return found;
}

function renderUnit(template: string, vars: { AK_HOME: string; TMUX: string }): string {
  return template.replaceAll('@AK_HOME@', vars.AK_HOME).replaceAll('@TMUX@', vars.TMUX);
}

function userUnitDir(): string {
  const base = process.env.XDG_CONFIG_HOME || join(homedir(), '.config');
  return join(base, 'systemd', 'user');
}

/** Installs and (re)starts both user units. Returns false when skipped. */
function installServices(paths: Paths): boolean {
  const tmux = which('tmux');
  if (!tmux) throw new InstallError('tmux was not found on PATH.');
  if (!which('systemctl')) throw new InstallError('systemctl was not found; rerun with --no-service to skip the service setup.');
  if (!existsSync(join(paths.app, 'current', 'bin', 'agent-k-local'))) {
    ui.warn('Services', 'skipped: not a release install (running from a source checkout?)');
    return false;
  }
  const dir = userUnitDir();
  mkdirSync(dir, { recursive: true });
  const tpl = templatesDir();
  for (const unit of [TMUX_UNIT, SERVER_UNIT]) {
    const text = renderUnit(readFileSync(join(tpl, unit), 'utf8'), { AK_HOME: paths.home, TMUX: tmux });
    writeFileSync(join(dir, unit), text);
  }
  sh('systemctl', ['--user', 'daemon-reload']);
  // tmux first; `enable --now` never restarts a running tmux, so sessions survive reinstalls.
  sh('systemctl', ['--user', 'enable', '--now', TMUX_UNIT]);
  sh('systemctl', ['--user', 'enable', SERVER_UNIT]);
  // Restart (not just start) the server so a reinstall or upgrade runs the new version.
  sh('systemctl', ['--user', 'restart', SERVER_UNIT]);
  for (const unit of [TMUX_UNIT, SERVER_UNIT]) {
    const st = trySh('systemctl', ['--user', 'is-active', unit]);
    const state = st.ok ? st.out : 'not running';
    if (state === 'active') ui.ok(unit, 'running');
    else ui.warn(unit, `${state}. Check: systemctl --user status ${unit}`);
  }

  const user = userInfo().username;
  const linger = trySh('loginctl', ['show-user', user, '-p', 'Linger', '--value']);
  if (linger.ok && linger.out === 'yes') {
    ui.ok('Starts at boot', 'lingering is on for your account');
    return true;
  }
  const en = trySh('loginctl', ['enable-linger', user]);
  if (en.ok) {
    ui.ok('Starts at boot', 'lingering enabled for your account');
  } else {
    ui.warn('Starts at boot', `no: couldn't enable lingering (${en.err || 'permission denied'}).`);
    ui.note(`Without it, AgentK Local only runs while you're logged in. Ask an admin: sudo loginctl enable-linger ${user}`);
  }
  return true;
}

function publicHost(): string {
  const r = trySh('hostname', ['-f']);
  return r.ok && r.out ? r.out : hostname();
}

// ---------------------------------------------------------------------------
// install

export async function runInstall(args: string[]): Promise<void> {
  const { values } = parseArgs({
    args,
    options: {
      'non-interactive': { type: 'boolean', default: false },
      'repos-root': { type: 'string' },
      'meko-key': { type: 'string' },
      bind: { type: 'string' },
      port: { type: 'string' },
      'no-service': { type: 'boolean', default: false },
    },
    strict: true,
  });
  const interactive = !values['non-interactive'];
  const paths = getPaths();
  const { ask, close } = makePrompter(interactive);

  try {
    mkdirSync(paths.home, { recursive: true, mode: 0o700 });
    const fresh = !existsSync(paths.config);
    const cfg: Config = loadConfig(paths);

    // 1. Basics
    if (!process.env.AK_INSTALLER) ui.title('AgentK Local setup');
    ui.section('Setup');
    if (interactive && process.stdin.isTTY) ui.note('Press Enter to keep the value in brackets.');
    const reposRoot = values['repos-root'] ?? (await ask('Repos folder', cfg.repos.roots[0] ?? '~/code'));
    cfg.repos.roots = fresh ? [reposRoot] : [reposRoot, ...cfg.repos.roots.filter((r) => r !== reposRoot)];
    if (fresh) cfg.repos.cloneInto = reposRoot;
    cfg.server.bind = values.bind ?? (await ask('Listen address', cfg.server.bind));
    const portStr = values.port ?? (await ask('Port', String(cfg.server.port)));
    const port = Number(portStr);
    if (!Number.isInteger(port) || port < 1 || port > 65535) throw new InstallError(`invalid port: ${portStr}`);
    cfg.server.port = port;
    if (!(interactive && process.stdin.isTTY)) {
      ui.row('Repos folder', reposRoot);
      ui.row('Listen on', `${cfg.server.bind}:${cfg.server.port}`);
    }

    // 2. Meko (required)
    let meko = resolveMeko(cfg);
    let storeKey = false;
    if (values['meko-key']) {
      meko = { ...meko, apiKey: values['meko-key'] };
      storeKey = true;
    } else if (!meko.apiKey) {
      const k = await ask('Meko API key', '');
      if (!k) throw new InstallError('A Meko API key is required. Pass --meko-key or set MEKO_API_KEY.');
      meko = { ...meko, apiKey: k };
      storeKey = true;
    }

    ui.section('Checks');
    const check = await checkMeko(meko.url, meko.apiKey);
    if (!check.ok) throw new InstallError(`Meko check failed (${meko.url}): ${check.error}`);
    const fromEnv = process.env.MEKO_API_KEY === meko.apiKey || readClaudeSettingsEnv().MEKO_API_KEY === meko.apiKey;
    if (storeKey && !fromEnv) cfg.meko.apiKey = meko.apiKey;
    ui.ok('Meko', `${meko.url.replace(/^https?:\/\//, '')}${fromEnv ? ' (key from Claude Code settings)' : ''}`);

    // 3. Meko Claude Code plugin
    const plugin = mekoPluginEnabled();
    if (!plugin.installed || !plugin.enabled) {
      throw new InstallError(
        `The meko-agent-skills Claude Code plugin is ${plugin.installed ? 'installed but not enabled' : 'not installed'}. Install it with:\n` +
          '  claude plugin marketplace add yugabyte/meko-mcp-server\n' +
          '  claude plugin install meko-agent-skills --scope user',
      );
    }
    ui.ok('Meko plugin', 'meko-agent-skills enabled');
    if (existsSync(expandHome(reposRoot))) ui.ok('Repos folder', reposRoot);
    else ui.warn('Repos folder', `${reposRoot} doesn't exist yet`);

    // 4. Keep SQLite off network filesystems
    let db = dbPath(paths, cfg);
    mkdirSync(dirname(db), { recursive: true });
    if (isNetworkFs(dirname(db))) {
      const moved = localDbLocation();
      ui.warn('Database', `${tildify(moved)} (moved off ${fsType(dirname(db))}: SQLite locking is unreliable on network filesystems)`);
      cfg.state.dbPath = moved;
      db = moved;
    } else {
      ui.ok('Database', tildify(db));
    }

    saveConfig(paths, cfg);
    ui.ok('Config', tildify(paths.config));
    // Create the certificate now (the server reuses it) so its fingerprint can be shown below.
    const tls = await loadTls(paths, cfg);
    if (tls) ui.ok('Certificate', tls.selfSigned ? 'self-signed, created for this host' : 'from config');

    // 5. tmux config
    writeTmuxConf(paths);

    // 6. Services
    ui.section('Services');
    let running = false;
    if (values['no-service']) {
      ui.warn('Services', 'skipped (--no-service). Start the server with: agent-k-local serve');
    } else if (installServices(paths)) {
      const probe = await waitForServer(cfg);
      if (probe.ok) {
        ui.ok('Server', `answering on port ${cfg.server.port} (version ${probe.version})`);
        running = true;
      } else {
        ui.warn('Server', `not answering yet (${probe.error}). See: agent-k-local logs`);
      }
    }

    // 7. Setup link and next steps
    const handle = openDb(db);
    try {
      const scheme = isLoopback(cfg.server.bind) ? 'http' : 'https';
      const host = isLoopback(cfg.server.bind) ? '127.0.0.1' : publicHost();
      const base = `${scheme}://${host}:${cfg.server.port}`;
      ui.blank();
      if (getOwner(handle)) {
        ui.text(style.bold(running ? 'Done. AgentK Local is running:' : 'Done. AgentK Local will be at:'));
        ui.blank();
        ui.text(`    ${style.cyan(`${base}/`)}`);
      } else {
        ui.text(style.bold('Done. Open this link to create your login (it works once):'));
        ui.blank();
        ui.text(`    ${style.cyan(`${base}/setup?token=${issueSetupToken(handle)}`)}`);
      }
      ui.blank();
      if (tls?.selfSigned) {
        ui.row('Certificate', 'self-signed, so your browser warns once. Check the SHA-256 fingerprint:');
        ui.row('', tls.fingerprint);
      }
      ui.row('New link', 'agent-k-local setup-link');
      ui.row('Status', 'agent-k-local status');
      ui.row('Logs', 'agent-k-local logs -f');
      ui.blank();
    } finally {
      handle.close();
    }
  } catch (e) {
    if (e instanceof InstallError) {
      process.stderr.write('\n');
      ui.fail('Install failed', e.message);
      process.exitCode = 1;
      return;
    }
    throw e;
  } finally {
    close();
  }
}

// ---------------------------------------------------------------------------
// uninstall

export async function runUninstall(args: string[]): Promise<void> {
  const { values } = parseArgs({
    args,
    options: {
      'non-interactive': { type: 'boolean', default: false },
      purge: { type: 'boolean', default: false },
    },
    strict: true,
  });
  const paths = getPaths();
  const { ask, close } = makePrompter(!values['non-interactive']);
  try {
    say('This stops agent-k-local and ends every running Claude session it hosts.');
    say('Worktrees and branches are kept.');
    const ok = values['non-interactive'] ? 'y' : await ask('Continue? (y/N)', 'n');
    if (!/^y(es)?$/i.test(ok)) {
      say('Cancelled.');
      return;
    }
    const dir = userUnitDir();
    if (which('systemctl')) {
      for (const unit of [SERVER_UNIT, TMUX_UNIT]) {
        trySh('systemctl', ['--user', 'disable', '--now', unit]);
        const f = join(dir, unit);
        if (existsSync(f)) unlinkSync(f);
      }
      trySh('systemctl', ['--user', 'daemon-reload']);
      say('Services removed.');
    }
    const link = join(homedir(), '.local', 'bin', 'agent-k-local');
    try {
      unlinkSync(link);
    } catch {
      // not installed there
    }
    if (values.purge) {
      const cfg = loadConfig(paths);
      const db = dbPath(paths, cfg);
      rmSync(paths.home, { recursive: true, force: true });
      if (!db.startsWith(paths.home + '/')) rmSync(dirname(db), { recursive: true, force: true });
      say(`Removed ${tildify(paths.home)}.`);
    } else {
      say(`State kept in ${tildify(paths.home)} (use --purge to delete it).`);
    }
  } finally {
    close();
  }
}
