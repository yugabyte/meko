import { execFileSync } from 'node:child_process';
import { chmodSync, existsSync, lstatSync, mkdirSync, readdirSync, readFileSync, rmdirSync, rmSync, unlinkSync } from 'node:fs';
import { homedir, hostname, userInfo } from 'node:os';
import { basename, dirname, join } from 'node:path';
import { createInterface } from 'node:readline/promises';
import { parseArgs } from 'node:util';
import { getOwner, issueSetupToken } from './auth.js';
import { mekoPluginInstalls } from './capture.js';
import { claudeUserDir } from './claude.js';
import { DEFAULT_MEKO_URL, dbPath, isLoopback, loadConfig, readClaudeSettingsEnv, resolveMeko, saveConfig, type Config } from './config.js';
import { openDb } from './db.js';
import { checkMeko } from './meko.js';
import { expandHome, getPaths, tildify, type Paths } from './paths.js';
import { hostServices, renderServiceFiles, ServiceError, SERVICES, type ServiceManager } from './service.js';
import { writeTmuxConf } from './tmux.js';
import { style, ui } from './tty.js';
import { waitForServer } from './health.js';
import { loadTls } from './tls.js';

// Linux names from `stat -f -c %T`; macOS names from `mount`.
const NETWORK_FS = new Set(['nfs', 'nfs4', 'cifs', 'smb2', 'smbfs', 'fuse.sshfs', 'afs', 'lustre', 'gpfs', 'afpfs', 'webdav', 'macfuse', 'osxfuse']);

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
export function mekoPluginEnabled(claudeDir: string = claudeUserDir()): { installed: boolean; enabled: boolean } {
  const isMeko = (k: string) => k.startsWith('meko-agent-skills@');
  const installed = (mekoPluginInstalls(claudeDir)?.length ?? 0) > 0;
  let enabled = false;
  try {
    const s = JSON.parse(readFileSync(join(claudeDir, 'settings.json'), 'utf8'));
    const ep = (s?.enabledPlugins ?? {}) as Record<string, unknown>;
    enabled = Object.entries(ep).some(([k, v]) => isMeko(k) && v === true);
  } catch {
    // no settings
  }
  return { installed, enabled };
}

/** The mount point in `df -P <dir>` output (the text after the capacity column). */
export function parseDfMountPoint(df: string): string | null {
  const last = df.trim().split('\n').at(-1) ?? '';
  return /\s\d+%\s+(.+)$/.exec(last)?.[1] ?? null;
}

/** The filesystem type of `mountPoint` in macOS `mount` output ("<dev> on <dir> (<type>, …)"). */
export function parseMountType(mount: string, mountPoint: string): string | null {
  const marker = ` on ${mountPoint} (`;
  for (const line of mount.split('\n')) {
    const i = line.lastIndexOf(marker);
    if (i >= 0) return /^[^,)]+/.exec(line.slice(i + marker.length))?.[0].trim() ?? null;
  }
  return null;
}

export function fsType(dir: string): string | null {
  if (process.platform !== 'darwin') {
    const r = trySh('stat', ['-f', '-c', '%T', dir]);
    return r.ok ? r.out : null;
  }
  // BSD stat has no -c: find the mount with df, then read its type from mount.
  const df = trySh('df', ['-P', dir]);
  const mp = df.ok ? parseDfMountPoint(df.out) : null;
  const mount = mp ? trySh('mount', []) : null;
  return mp && mount?.ok ? parseMountType(mount.out, mp) : null;
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
  ensurePrivateDir(dir);
  return join(dir, 'state.db');
}

/**
 * Creates `dir` for private state in a shared place like /var/tmp, where another user could have
 * made the directories first. Refuses unless `dir` and its parent are real directories (not
 * symlinks) owned by this user and the parent isn't writable by others; then makes `dir` 0700.
 */
export function ensurePrivateDir(dir: string, uid: number | undefined = process.getuid?.(), gid: number | undefined = process.getgid?.()): void {
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  const fix = 'Remove it, or set state.db_path in config.toml to a local folder only you can write to.';
  for (const d of [dirname(dir), dir]) {
    const st = lstatSync(d);
    if (st.isSymbolicLink() || !st.isDirectory()) throw new InstallError(`${d} is not a directory (it may be a symlink). ${fix}`);
    if (uid !== undefined && st.uid !== uid) throw new InstallError(`${d} is owned by another user. ${fix}`);
    // Group write is fine for our own primary group (a umask 002 system with a group per user).
    const othersWrite = st.mode & 0o002 || (st.mode & 0o020 && (gid === undefined || st.gid !== gid));
    if (d !== dir && othersWrite) throw new InstallError(`${d} can be written to by other users. ${fix}`);
  }
  chmodSync(dir, 0o700);
}

/** A failed service command, with what to check next and how to go without the services. */
export function serviceFailure(svc: ServiceManager, message: string, logsDir: string): string {
  const where = svc.kind === 'launchd' ? ` and ${tildify(join(logsDir, 'launchd*.log'))}` : '';
  return (
    `${message}\nCheck: ${svc.statusHint('server')}${where}.\n` +
    'Or rerun with --no-service to skip the service setup, and start the server with: agent-k-local serve'
  );
}

/** Installs and (re)starts both background services. Returns false when skipped. */
function installServices(paths: Paths): boolean {
  const tmux = which('tmux');
  if (!tmux) throw new InstallError('tmux was not found on PATH.');
  const svc = hostServices();
  if (!svc) throw new InstallError(`background services aren't supported on ${process.platform}; rerun with --no-service to skip the service setup.`);
  svc.preflight();
  if (!existsSync(join(paths.app, 'current', 'bin', 'agent-k-local'))) {
    ui.warn('Services', 'skipped: not a release install (running from a source checkout?)');
    return false;
  }
  mkdirSync(paths.logs, { recursive: true }); // launchd opens its log files before starting a job
  try {
    svc.install(renderServiceFiles(svc, { AK_HOME: paths.home, TMUX: tmux }));
  } catch (e) {
    if (e instanceof ServiceError) throw new ServiceError(serviceFailure(svc, e.message, paths.logs), { cause: e });
    throw e;
  }
  for (const s of SERVICES) {
    const st = svc.state(s);
    if (st.running) ui.ok(svc.id(s), 'running');
    else ui.warn(svc.id(s), `${st.text}. Check: ${svc.statusHint(s)}`);
  }
  const boot = svc.bootCheck();
  if (boot.ok) ui.ok(boot.label, boot.detail);
  else ui.warn(boot.label, boot.detail);
  if (boot.note) ui.note(boot.note);
  return true;
}

/** True when the macOS application firewall is on (it asks before `node` may accept connections). */
function macFirewallOn(): boolean {
  if (process.platform !== 'darwin') return false;
  const r = trySh('/usr/libexec/ApplicationFirewall/socketfilterfw', ['--getglobalstate']);
  return r.ok && /enabled/i.test(r.out);
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
    if (storeKey && !fromEnv) {
      cfg.meko.apiKey = meko.apiKey;
      // The URL goes with the key: resolveMeko doesn't pair a key in config with a URL from elsewhere.
      if (meko.url !== DEFAULT_MEKO_URL) cfg.meko.url = meko.url;
    }
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
    if (!isLoopback(cfg.server.bind) && macFirewallOn()) {
      ui.note('The macOS firewall is on. If other devices (like your phone) can\'t connect, allow incoming connections for');
      ui.note(`${tildify(join(paths.app, 'current', 'node', 'bin', 'node'))} in System Settings > Network > Firewall > Options.`);
    }
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
      const scheme = tls ? 'https' : 'http';
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
    if (e instanceof InstallError || e instanceof ServiceError) {
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

/** Session worktree folders: <worktrees>/<project>/<session>. */
export function sessionWorktrees(root: string): string[] {
  const dirs = (d: string) => {
    try {
      return readdirSync(d, { withFileTypes: true }).filter((e) => e.isDirectory()).map((e) => join(d, e.name));
    } catch {
      return [];
    }
  };
  return dirs(root).flatMap(dirs);
}

/** True when a worktree has uncommitted or untracked changes, or git can't tell. */
function hasUncommittedWork(dir: string): boolean {
  try {
    return execFileSync('git', ['-C', dir, 'status', '--porcelain'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'], timeout: 30_000 }).trim() !== '';
  } catch {
    return true;
  }
}

/**
 * Deletes everything in the AgentK Local home, except the session worktrees and design files
 * unless `all`. Returns the folders it kept (empty ones are removed, and then the home too).
 */
export function purgeHome(paths: Paths, all: boolean): string[] {
  const keep = all ? [] : [paths.worktrees, paths.designs];
  let names: string[];
  try {
    names = readdirSync(paths.home);
  } catch {
    return [];
  }
  for (const n of names) {
    const p = join(paths.home, n);
    if (!keep.includes(p)) rmSync(p, { recursive: true, force: true });
  }
  const kept = keep.filter((d) => {
    try {
      if (readdirSync(d).length) return true;
      rmdirSync(d);
    } catch {
      // not there
    }
    return false;
  });
  if (!kept.length) rmSync(paths.home, { recursive: true, force: true });
  return kept;
}

/**
 * Deletes a database kept outside the home: the file, its SQLite side files and the update
 * snapshots next to it. Its folder is removed only when that leaves it empty.
 */
export function removeDb(db: string): void {
  const dir = dirname(db);
  const name = basename(db);
  for (const f of [name, `${name}-wal`, `${name}-shm`, `${name}-journal`]) rmSync(join(dir, f), { force: true });
  try {
    for (const f of readdirSync(dir)) if (f.startsWith('state.db.bak-')) rmSync(join(dir, f), { force: true });
    rmdirSync(dir);
  } catch {
    // gone already, or other files live there
  }
}

export async function runUninstall(args: string[]): Promise<void> {
  const { values } = parseArgs({
    args,
    options: {
      'non-interactive': { type: 'boolean', default: false },
      purge: { type: 'boolean', default: false },
      'delete-worktrees': { type: 'boolean', default: false },
    },
    strict: true,
  });
  const paths = getPaths();
  const purge = values.purge;
  const deleteWork = purge && values['delete-worktrees'];
  if (values['delete-worktrees'] && !purge) {
    ui.fail('Uninstall', '--delete-worktrees only works with --purge.');
    process.exitCode = 2;
    return;
  }
  const { ask, close } = makePrompter(!values['non-interactive']);
  try {
    say('This stops AgentK Local and ends every running Claude session it hosts.');
    if (purge) say(`--purge deletes ${tildify(paths.home)}: config, database, certificates, logs and installed versions.`);
    const wt = tildify(paths.worktrees);
    if (deleteWork) {
      const trees = sessionWorktrees(paths.worktrees);
      const dirty = trees.filter(hasUncommittedWork).length;
      say(`--delete-worktrees also deletes ${trees.length} session worktree${trees.length === 1 ? '' : 's'} in ${wt}` +
        `${dirty ? `, ${dirty} with uncommitted changes that will be lost` : ''}, and the sessions' design files. Branches are kept.`);
    } else if (purge) {
      say(`Session worktrees (${wt}), design files (${tildify(paths.designs)}) and branches are kept; add --delete-worktrees to delete the worktrees and design files too.`);
    } else {
      say('Worktrees and branches are kept.');
    }
    const ok = values['non-interactive'] ? 'y' : await ask('Continue? (y/N)', 'n');
    if (!/^y(es)?$/i.test(ok)) {
      say('Cancelled.');
      return;
    }
    const svc = hostServices();
    if (svc && (svc.kind === 'launchd' || which('systemctl'))) {
      svc.remove();
      say('Services removed.');
    }
    const link = join(homedir(), '.local', 'bin', 'agent-k-local');
    try {
      unlinkSync(link);
    } catch {
      // not installed there
    }
    if (purge) {
      const db = dbPath(paths, loadConfig(paths));
      const kept = purgeHome(paths, deleteWork);
      if (!db.startsWith(paths.home + '/')) removeDb(db);
      say(kept.length ? `Removed ${tildify(paths.home)} except ${kept.map(tildify).join(' and ')}.` : `Removed ${tildify(paths.home)}.`);
      if (deleteWork) say('Run `git worktree prune` in each repo to forget the deleted worktrees.');
    } else {
      say(`State kept in ${tildify(paths.home)} (use --purge to delete it).`);
    }
  } finally {
    close();
  }
}
