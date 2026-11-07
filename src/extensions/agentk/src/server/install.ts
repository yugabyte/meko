import { execFileSync, spawnSync } from 'node:child_process';
import { chmodSync, existsSync, lstatSync, mkdirSync, readdirSync, readlinkSync, rmdirSync, rmSync, unlinkSync } from 'node:fs';
import { hostname, userInfo } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import { createInterface } from 'node:readline/promises';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';
import { HARNESS_CAPS, HARNESS_IDS, versionAtLeast, type HarnessId } from '../shared/harnesses.js';
import { claudeUserDir } from './claude.js';
import { cliVersion, harnessCommand } from './harness/version.js';
import { FRESH_INSTALL_TOUR, dbPath, loadConfig, saveConfig, type Config } from './config.js';
import { assertFree, InstanceBusyError } from './instance.js';
import { openBrowser } from './browser.js';
import { tryMekoToken } from './meko.js';
import { probeLocalMeko, type LocalMekoProbe } from './mekolocal.js';
import { KODE_PATH, MEKO_INSTALL_COMMAND } from '../shared/mekourls.js';
import {
  MekoSetupError, ensureMekoSetup, latestMekoSetup, mekoClaudeGaps, readClaudeMeko, runMekoSetup,
} from './mekosetup.js';
import { claudeTokenGaps } from './mekoclaude.js';
import { expandHome, getPaths, oldHome, tildify, type Paths } from './paths.js';
import { dropOldNames, hostServices, removeServiceLauncher, renderServiceFiles, samePath, ServiceError, SERVICES, writeServiceLauncher, type ServiceManager } from './service.js';
import { SessionHost } from './hostclient.js';
import { COMMAND, commandLinks } from './cmdlink.js';
import { style, ui } from './tty.js';
import { waitForServer } from './health.js';
import { removeKiroAgent } from './kiro.js';
import { ensurePrivateDir, fsType, isNetworkFs, PrivateDirError } from './fstype.js';
import { checkMove, makeHomeParent, mekoDirOf, tmpHomeWarning } from './movehome.js';
import { kiroMekoGaps } from './mekokiro.js';

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

type Line = { question: (q: string) => Promise<string>; close: () => void };

/**
 * The install's questions, asked on the terminal. Its readline opens at the first question, not
 * before: an open readline keeps the terminal in raw mode, and the tools' check (deps.sh) runs
 * first and reads its own answers from /dev/tty, which in raw mode echo nothing and never end at
 * Enter. Meko's first install runs this install right after its own picker, so that check's
 * question is often the first thing asked.
 */
export function makePrompter(
  interactive: boolean,
  tty = process.stdin.isTTY === true,
  open: () => Line = () => createInterface({ input: process.stdin, output: process.stdout }),
): { ask: Prompter; close: () => void } {
  if (!interactive || !tty) {
    return { ask: async (_q, def) => def, close: () => {} };
  }
  let rl: Line | undefined;
  return {
    ask: async (q, def) => {
      rl ??= open();
      const a = (await rl.question(ui.prompt(q, def))).trim();
      return a || def;
    },
    close: () => {
      rl?.close();
      rl = undefined;
    },
  };
}

// ---------------------------------------------------------------------------
// Steps

/** Pick a local-disk location for state.db when the default is on a network filesystem. */
function localDbLocation(): string {
  const user = userInfo().username;
  const xdg = process.env.XDG_STATE_HOME;
  if (xdg) {
    mkdirSync(join(xdg, 'agent-k-local'), { recursive: true, mode: 0o700 });
    chmodSync(join(xdg, 'agent-k-local'), 0o700);
    if (!isNetworkFs(join(xdg, 'agent-k-local'))) return join(xdg, 'agent-k-local', 'state.db');
  }
  const dir = join('/var/tmp', user, 'agent-k-local');
  ensurePrivateDir(dir);
  return join(dir, 'state.db');
}

/** A failed service command, with what to check next and how to go without the services. */
export function serviceFailure(svc: ServiceManager, message: string, logsDir: string): string {
  const where = svc.kind === 'launchd' ? ` and ${tildify(join(logsDir, 'launchd*.log'))}` : '';
  return (
    `${message}\nCheck: ${svc.statusHint('server')}${where}.\n` +
    'Or rerun with --no-service to skip the service setup, and start the server with: meko kode serve'
  );
}

/** Installs and (re)starts both background services. Returns false when skipped. */
function installServices(paths: Paths): boolean {
  const svc = hostServices();
  if (!svc) throw new InstallError(`background services aren't supported on ${process.platform}; rerun with --no-service to skip the service setup.`);
  svc.preflight();
  if (!existsSync(join(paths.app, 'current', 'bin', COMMAND))) {
    ui.warn('Services', 'skipped: not a release install (running from a source checkout?)');
    return false;
  }
  mkdirSync(paths.logs, { recursive: true }); // launchd opens its log files before starting a job
  // A version before the session host: stop its server, then its tmux service and the Claudes in
  // it, so none runs twice; the new server's recovery resumes them in the session host. With the
  // host service installed, stop just the server (it may be that version's, which would start its
  // sessions in tmux again) and the tmux service, so the host and its sessions keep running.
  if (svc.legacyTmuxInstalled()) {
    if (svc.installed('host')) {
      if (svc.installed('server')) svc.stop('server');
      svc.removeLegacyTmux();
      ui.ok('tmux', "removed an older version's tmux service; any session still in it resumes in the session host");
    } else {
      svc.remove();
      ui.ok('tmux', "removed an older version's tmux service; its sessions resume in the session host");
    }
  }
  for (const [old, now] of dropOldNames(svc)) ui.ok(now, `replaces ${old}, its name before`);
  try {
    writeServiceLauncher(paths.home);
    svc.install(renderServiceFiles(svc, { KODE_HOME: paths.home, HOST: hostname() }));
  } catch (e) {
    if (e instanceof ServiceError) throw new ServiceError(serviceFailure(svc, e.message, paths.logs), { cause: e });
    throw e;
  }
  // Only what needs a look: services that run, and start at login, go without saying.
  for (const s of SERVICES) {
    const st = svc.state(s);
    if (!st.running) ui.warn(svc.id(s), `${st.text}. Check: ${svc.statusHint(s)}`);
  }
  const boot = svc.bootCheck();
  if (!boot.ok) {
    ui.warn(boot.label, boot.detail);
    if (boot.note) ui.note(boot.note);
  }
  return true;
}

// ---------------------------------------------------------------------------
// install

export interface InstallMekoDeps {
  /**
   * Sets up Meko for Claude Code with the Meko on this computer; throws MekoSetupError when that's
   * declined or doesn't finish. Left out when Claude Code isn't installed.
   */
  ensureInClaude?: (url: string, apiKey: string) => Promise<void>;
  /** Sets up Meko for Kiro, the same way; left out when Kiro isn't installed. */
  ensureInKiro?: (url: string, apiKey: string) => Promise<void>;
  /** What `meko token --json` says (`probeLocalMeko`): the Meko running on this computer, which isn't stored. */
  probe?: () => Promise<LocalMekoProbe>;
  ui: Pick<typeof ui, 'ok' | 'warn' | 'section'>;
}

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

/**
 * The installer's Meko step. Kode uses the Meko running on this computer (`meko token --json`)
 * and stores nothing of it: that Meko serves its own datapacks and passes the rest on to Meko Cloud
 * once Meko Cloud is linked in Meko (Settings › Meko Cloud, or `meko cloud-link`). `--no-meko` skips
 * Meko's setup for the agents. Declining Meko's setup for Claude Code or Kiro (each offered only
 * where it's installed) is a warning, not a stop. Returns whether there's a Meko to use.
 */
export async function installMeko(o: { noMeko: boolean }, d: InstallMekoDeps): Promise<boolean> {
  if (o.noMeko) d.ui.warn('--no-meko', "Meko can't be turned off any more: Kode uses the Meko on this computer. Meko isn't set up for the agents.");
  const local = d.probe ? (await d.probe()).meko : null;
  d.ui.section('Checks');
  if (!local) {
    d.ui.warn('Meko', "not running on this computer: Kode uses it once it runs (meko start).");
    return false;
  }
  d.ui.ok('Meko', `on this computer (${hostOf(local.url)})`);
  // --no-meko never set Meko up for the agents, and still doesn't.
  const ensureInClaude = o.noMeko ? undefined : d.ensureInClaude;
  const ensureInKiro = o.noMeko ? undefined : d.ensureInKiro;
  if (ensureInClaude) {
    try {
      await ensureInClaude(local.url, local.token);
      d.ui.ok('Meko in Claude', 'plugin, MCP server and key in place');
    } catch (e) {
      if (!(e instanceof MekoSetupError)) throw e;
      d.ui.warn('Meko in Claude', `${e.message} Kode uses Meko; Claude Code sessions do once it's set up (Kode's Settings › Harness › Claude Code can do it).`);
    }
  }
  if (ensureInKiro) {
    try {
      await ensureInKiro(local.url, local.token);
      d.ui.ok('Meko in Kiro', 'MCP server and agent in place');
    } catch (e) {
      if (!(e instanceof MekoSetupError)) throw e;
      d.ui.warn('Meko in Kiro', `${e.message} Kode uses Meko; Kiro sessions do once it's set up (Kode's Settings › Harness › Kiro can do it).`);
    }
  }
  return true;
}

/**
 * The installer's agents check: the coding agents turned on in config.toml whose `--version`
 * answers (with its `minVersion` or later), in HARNESS_IDS order. With none the install stops,
 * saying what to install. A default that isn't among them becomes the first that is, so a machine
 * with only Kiro starts its sessions on Kiro.
 */
export function settleAgents(cfg: Config, versions: Partial<Record<HarnessId, string | null>>): HarnessId[] {
  const found = HARNESS_IDS.filter((id) => {
    const v = versions[id];
    const min = HARNESS_CAPS[id].minVersion;
    return cfg[id].enabled && !!v && (!min || versionAtLeast(v, min));
  });
  if (!found.length) {
    throw new InstallError(
      `no coding agent found. Install Claude Code (https://claude.com/claude-code), OpenCode ${HARNESS_CAPS.opencode.minVersion} or later `
        + `(https://opencode.ai; from an older one, ${HARNESS_CAPS.opencode.upgrade}) `
        + `or Kiro CLI ${HARNESS_CAPS.kiro.minVersion} or later (https://kiro.dev/cli/), sign in to it, and run meko extension install kode again.`,
    );
  }
  if (!found.includes(cfg.agents.default)) cfg.agents.default = found[0];
  return found;
}

/**
 * Kode's prerequisites check (git, curl, gh, a coding agent and their sign-ins; it installs what's
 * missing after asking): scripts/deps.sh, which a release has as lib/scripts/deps.sh beside lib/dist.
 */
export function depsScript(here = dirname(fileURLToPath(import.meta.url))): string {
  return join(here, '..', '..', 'scripts', 'deps.sh');
}

/** Runs the prerequisites check on this terminal; a check that fails has said why. */
function checkTools(args: string[]): void {
  const script = depsScript();
  if (!existsSync(script)) {
    ui.warn('Tools', `not checked: ${script} is missing`);
    return;
  }
  const r = spawnSync('sh', [script, ...args], { stdio: 'inherit' });
  if (r.status !== 0) throw new InstallError(r.error ? `couldn't check the tools: ${r.error.message}` : 'the tools Kode needs aren\'t all there (see above).');
}

/**
 * The repos folder the install was given, made when it isn't there yet (with its parents), since
 * Kode clones into it. It says whether it made it, or why it couldn't.
 */
export function reposFolder(root: string): { created: boolean; error?: string } {
  const dir = expandHome(root);
  if (existsSync(dir)) return { created: false };
  try {
    mkdirSync(dir, { recursive: true });
    return { created: true };
  } catch (e) {
    return { created: false, error: (e as Error).message };
  }
}

/**
 * A fresh install starts the Kode tour at the server's first start (`[welcome] tour`); an upgrade
 * or a reinstall doesn't, and keeps the user's projects as they are.
 */
export function welcomeTour(cfg: Config, fresh: boolean): void {
  if (fresh) cfg.welcome.tour = FRESH_INSTALL_TOUR;
}

/**
 * Whether the install opens Kode in the browser at its end: only a fresh, interactive install
 * whose server answers, so a new user lands in the tour. `--non-interactive` (Meko's Extensions
 * page, which is already in a browser) and upgrades open nothing.
 */
export function opensBrowser(o: { fresh: boolean; interactive: boolean; running: boolean }): boolean {
  return o.fresh && o.interactive && o.running;
}

export async function runInstall(args: string[]): Promise<void> {
  const { values } = parseArgs({
    args,
    options: {
      'non-interactive': { type: 'boolean', default: false },
      'repos-root': { type: 'string' },
      // Meko's `meko extension install kode` passes its own URL and token, and refuses a Kode
      // without --meko-url (its `tooOld`). Kode reads the Meko on this computer itself, so both go unused.
      'meko-key': { type: 'string' },
      'meko-url': { type: 'string' },
      'no-meko': { type: 'boolean', default: false },
      'no-service': { type: 'boolean', default: false },
      'install-deps': { type: 'boolean', default: false },
      'no-install-deps': { type: 'boolean', default: false },
      agent: { type: 'string' },
    },
    strict: true,
  });
  const interactive = !values['non-interactive'];
  const paths = getPaths();
  const { ask, close } = makePrompter(interactive);

  try {
    // In Meko's data folder, which may not be there yet (made private as Meko does in /var/tmp).
    makeHomeParent(paths.home);
    mkdirSync(paths.home, { recursive: true, mode: 0o700 });
    // A shared home in use on another host: installing here would pin its services to this host.
    try {
      assertFree(paths.lock, tildify(paths.home));
    } catch (e) {
      if (e instanceof InstanceBusyError) throw new InstallError(e.message);
      throw e;
    }
    const fresh = !existsSync(paths.config);
    const cfg: Config = loadConfig(paths);
    // The Meko data folder this install is in, as the installer found it (from MEKO_HOME or
    // XDG_STATE_HOME in this shell, which the services don't have), or the one Meko's
    // `extension install` names: the server and a move go by it, never resolving it again.
    const inMeko = mekoDirOf(paths.home);
    if (inMeko) cfg.meko.home = inMeko;
    else if (process.env.MEKO_HOME) cfg.meko.home = resolve(process.env.MEKO_HOME);

    // 1. The tools: git, curl, gh and a coding agent, installed after asking when missing.
    ui.title('Kode setup');
    checkTools([
      ...(interactive ? [] : ['--non-interactive']),
      ...(values['install-deps'] ? ['--install-deps'] : []),
      ...(values['no-install-deps'] ? ['--no-install-deps'] : []),
      ...(values.agent ? ['--agent', values.agent] : []),
    ]);

    // 2. Basics
    ui.section('Setup');
    if (interactive && process.stdin.isTTY) ui.note('Press Enter to keep the value in brackets.');
    const reposRoot = values['repos-root'] ?? (await ask('Repos folder', cfg.repos.roots[0] ?? '~/code'));
    cfg.repos.roots = fresh ? [reposRoot] : [reposRoot, ...cfg.repos.roots.filter((r) => r !== reposRoot)];
    if (fresh) cfg.repos.cloneInto = reposRoot;
    if (!(interactive && process.stdin.isTTY)) ui.row('Repos folder', reposRoot);
    // Browsers reach Kode only through the Meko on this computer, which serves it at /ext/kode/
    // with its own sign-in; with no Meko here, nothing could.
    if ((await probeLocalMeko(cfg.meko.home || null)).state === 'none') {
      throw new InstallError('Kode runs behind the Meko on this computer, which serves it to your browser, and there is ' +
        `no meko command here. Install Meko first (${MEKO_INSTALL_COMMAND}), then run: meko extension install kode`);
    }

    // 3. The coding agents: at least one, or nothing can run.
    const versions: Partial<Record<HarnessId, string | null>> = {};
    for (const id of HARNESS_IDS) versions[id] = await cliVersion(harnessCommand(cfg, id));
    const agents = settleAgents(cfg, versions);

    // 4. Meko and 5. Meko in each agent
    const canAsk = interactive && !!process.stdin.isTTY;
    const setup = { canAsk, installDeps: values['install-deps'], noInstallDeps: values['no-install-deps'] };
    const local = { home: cfg.meko.home || null };
    await installMeko({ noMeko: values['no-meko'] }, {
      probe: () => probeLocalMeko(local.home),
      ensureInKiro: !agents.includes('kiro') ? undefined : async (url, apiKey) => {
        const gaps = async () => kiroMekoGaps();
        // Its output is kept, and shown only if it fails (ensureMekoSetup).
        const output: string[] = [];
        await ensureMekoSetup(
          { url, apiKey, client: 'kiro', local, ...setup },
          { gaps, latest: () => latestMekoSetup(), run: (pkg) => runMekoSetup(pkg, { url, apiKey, client: 'kiro', log: (l) => output.push(l) }), output: () => output, ask, say, spin: ui.spin },
        );
      },
      ensureInClaude: !agents.includes('claude') ? undefined : async (url, apiKey) => {
        // The plugin and the key where its hooks read it, on this Meko, with a token it accepts
        // (its MCP server is Meko's: meko agent connect). Anything wrong is set up by Meko's own
        // installer, after asking.
        const gaps = async () => {
          const c = readClaudeMeko(claudeUserDir());
          return [...mekoClaudeGaps(c, url), ...(await claudeTokenGaps(c, url, apiKey, tryMekoToken))];
        };
        const output: string[] = [];
        await ensureMekoSetup(
          { url, apiKey, local, ...setup },
          { gaps, latest: () => latestMekoSetup(), run: (pkg) => runMekoSetup(pkg, { url, apiKey, log: (l) => output.push(l) }), output: () => output, ask, say, spin: ui.spin },
        );
      },
      ui,
    });
    ui.ok('Agents', agents.map((id) => `${HARNESS_CAPS[id].label} ${versions[id]}`).join(', ') + ` (new sessions start on ${HARNESS_CAPS[cfg.agents.default].label})`);
    const repos = reposFolder(reposRoot);
    if (repos.error) ui.warn('Repos folder', `${reposRoot} doesn't exist, and couldn't be created: ${repos.error}`);
    else ui.ok('Repos folder', repos.created ? `${reposRoot} (created)` : reposRoot);

    // 6. Keep SQLite off network filesystems
    let db = dbPath(paths, cfg);
    mkdirSync(dirname(db), { recursive: true });
    if (isNetworkFs(dirname(db))) {
      const moved = localDbLocation();
      ui.warn('Database', `${tildify(moved)} (moved off ${fsType(dirname(db))}: SQLite locking is unreliable on network filesystems)`);
      cfg.state.dbPath = moved;
      db = moved;
    }

    welcomeTour(cfg, fresh);
    saveConfig(paths, cfg);
    ui.ok('Config', tildify(paths.config));
    const tmpWarning = tmpHomeWarning(paths.home);
    if (tmpWarning) ui.warn('Home', tmpWarning);
    const move = checkMove(paths, cfg);
    if (move && 'plan' in move) ui.note(`Kode's home moves to ${tildify(move.plan.to)} once the server has started: every session ends and resumes there.`);
    else if (move) ui.warn('Home', `stays in ${tildify(paths.home)} for now: ${move.skip}`);

    // 7. Services: rows only for what needs a look (the summary's "Kode is running" says the rest).
    let running = false;
    if (values['no-service']) {
      ui.warn('Services', 'skipped (--no-service). Start the server with: meko kode serve');
    } else if (installServices(paths)) {
      const probe = await waitForServer(paths);
      if (probe.ok) running = true;
      else ui.warn('Server', `not answering yet (${probe.error}). See: meko kode logs`);
    }

    // 8. Where to open it
    const meko = await probeLocalMeko(cfg.meko.home || null);
    ui.blank();
    if (meko.meko?.webUrl) {
      ui.text(style.bold(running ? 'Done. Kode is running:' : 'Done. Kode will be at:'));
      ui.blank();
      ui.text(`    ${style.blue(`${meko.meko.webUrl}${KODE_PATH}`)}`);
      ui.blank();
      // A new user lands in the tour; over SSH or with no display there's nothing to open here.
      if (opensBrowser({ fresh, interactive, running }) && (await openBrowser(`${meko.meko.webUrl}${KODE_PATH}`))) {
        ui.note('Opened it in your browser, where the Kode tour starts.');
      }
    } else {
      ui.text(style.bold(running ? 'Done. Kode is running.' : 'Done.'));
      ui.blank();
      ui.row('Browser', meko.meko?.webOff
        ? "Meko's web UI is off: turn it on with meko ui start, then open Kode with meko kode ui"
        : meko.meko
          ? 'the Meko on this computer is too old to serve Kode: update it with meko update'
          : "Meko isn't running: start it with meko start, then open Kode with meko kode ui");
    }
    if (meko.meko?.webUrl) ui.row('Open', 'meko kode ui');
    ui.row('Status', 'meko kode status');
    ui.row('Logs', 'meko kode logs -f');
    ui.blank();
  } catch (e) {
    if (e instanceof InstallError || e instanceof ServiceError || e instanceof PrivateDirError) {
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
 * Deletes everything in the Kode home, except the session worktrees and design files
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

/** Removes `~/.agent-k-local` when it's the link to `home` that the move left. */
export function removeOldHomeLink(home: string, old: string = oldHome()): void {
  try {
    if (lstatSync(old).isSymbolicLink() && samePath(readlinkSync(old), home)) unlinkSync(old);
  } catch {
    // not there
  }
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
  const asked = !values['non-interactive'];
  const { ask, close } = makePrompter(asked);
  try {
    // Run by Meko (non-interactive), a purge's explanation is Meko's, which it gave before asking.
    if (asked || !purge) say('This stops Kode and ends every running session it hosts (Claude Code or Kiro).');
    if (purge && asked) say(`--purge deletes ${tildify(paths.home)}: config, database, certificates, logs and installed versions.`);
    const wt = tildify(paths.worktrees);
    if (purge && !asked) {
      // Nothing more: Meko said what a purge deletes.
    } else if (deleteWork) {
      const trees = sessionWorktrees(paths.worktrees);
      const dirty = trees.filter(hasUncommittedWork).length;
      say(`--delete-worktrees also deletes ${trees.length} session worktree${trees.length === 1 ? '' : 's'} in ${wt}` +
        `${dirty ? `, ${dirty} with uncommitted changes that will be lost` : ''}, and the sessions' design files. Branches are kept.`);
    } else if (purge) {
      say(`Session worktrees (${wt}), design files (${tildify(paths.designs)}) and branches are kept; add --delete-worktrees to delete the worktrees and design files too.`);
    } else {
      say('Git worktrees and branches are kept.');
    }
    const ok = values['non-interactive'] ? 'y' : await ask('Continue? (y/N)', 'n');
    if (!/^y(es)?$/i.test(ok)) {
      say('Cancelled.');
      return;
    }
    const svc = hostServices();
    if (svc && (svc.kind === 'launchd' || which('systemctl'))) {
      svc.remove();
      removeServiceLauncher(paths.home);
      say('Services removed.');
    }
    // A session host started without a service (--no-service) ends with its sessions too.
    await new SessionHost(paths, { services: () => null }).stop().catch(() => undefined);
    for (const link of commandLinks()) {
      try {
        unlinkSync(link);
      } catch {
        // not installed there
      }
    }
    // The Kiro agent Kode wrote is its own: Kiro sessions ran as it.
    removeKiroAgent();
    if (purge) {
      const db = dbPath(paths, loadConfig(paths));
      const kept = purgeHome(paths, deleteWork);
      if (!db.startsWith(paths.home + '/')) removeDb(db);
      // The link the move into Meko's data folder left at the old home goes with what it pointed at.
      if (!kept.length) removeOldHomeLink(paths.home);
      say(kept.length ? `Removed ${tildify(paths.home)} except ${kept.map(tildify).join(' and ')}.` : `Removed ${tildify(paths.home)}.`);
      if (deleteWork) say('Run `git worktree prune` in each repo to forget the deleted worktrees.');
    } else if (asked || !mekoDirOf(paths.home)) {
      // Run by Meko, its own last line says where the data stays, but only for a home in its data
      // folder (`extensions/kode`): one that hasn't moved there yet is Kode's to name.
      say(`State kept in ${tildify(paths.home)} (use --purge to delete it).`);
    }
  } finally {
    close();
  }
}
