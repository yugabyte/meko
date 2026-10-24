import { execFileSync } from 'node:child_process';
import { chmodSync, existsSync, lstatSync, mkdirSync, readdirSync, readlinkSync, rmdirSync, rmSync, unlinkSync } from 'node:fs';
import { isIP } from 'node:net';
import { hostname, userInfo } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import { createInterface } from 'node:readline/promises';
import { parseArgs } from 'node:util';
import { getOwner, issueSetupToken } from './auth.js';
import { HARNESS_CAPS, HARNESS_IDS, type HarnessId } from '../shared/harnesses.js';
import { claudeUserDir } from './claude.js';
import { cliVersion, harnessCommand, versionAtLeast } from './harness/version.js';
import {
  DEFAULT_MEKO_URL, dbPath, isLoopback, loadConfig, readClaudeSettingsEnv, resolveMeko, resolveMekoWebUrl, resolveMekoWithSource, saveConfig, type Config,
} from './config.js';
import { openDb } from './db.js';
import { assertFree, InstanceBusyError } from './instance.js';
import { checkMeko, tryMekoToken } from './meko.js';
import { findLocalMeko, isLocalMeko, type LocalMeko } from './mekolocal.js';
import { claudeConfigPath } from './trust.js';
import {
  MekoSetupError, claudeHasMekoMcp, ensureMekoSetup, envKeyGap, latestMekoSetup, mekoClaudeGaps, mekoKeyForConfig, readClaudeMeko, runMekoSetup,
} from './mekosetup.js';
import { claudeTokenGaps } from './mekoclaude.js';
import { expandHome, getPaths, oldHome, tildify, type Paths } from './paths.js';
import { dropOldNames, hostServices, renderServiceFiles, samePath, ServiceError, SERVICES, type ServiceManager } from './service.js';
import { SessionHost } from './hostclient.js';
import { COMMAND, commandLinks } from './cmdlink.js';
import { style, ui } from './tty.js';
import { waitForServer } from './health.js';
import { loadTls } from './tls.js';
import { removeKiroAgent } from './kiro.js';
import { ensurePrivateDir, fsType, isNetworkFs, PrivateDirError } from './fstype.js';
import { checkMove, makeHomeParent, mekoDirOf, tmpHomeWarning } from './movehome.js';
import { kiroAgentLoadsMcp, kiroMekoGaps, readKiroMeko } from './mekokiro.js';

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
    'Or rerun with --no-service to skip the service setup, and start the server with: agentk serve'
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
  for (const [old, now] of dropOldNames(svc)) ui.ok(now, `replaces ${old}, its name before the rename to AgentK`);
  try {
    svc.install(renderServiceFiles(svc, { AK_HOME: paths.home, HOST: hostname() }));
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

/**
 * True when nobody is at a desktop here: an SSH login, or Linux with no display. The browser is
 * then on another device, so the install defaults to remote access.
 */
export function isHeadless(env: NodeJS.ProcessEnv = process.env, platform: NodeJS.Platform = process.platform): boolean {
  if (env.SSH_CONNECTION || env.SSH_CLIENT || env.SSH_TTY) return true;
  return platform !== 'darwin' && !env.DISPLAY && !env.WAYLAND_DISPLAY;
}

/** The answer the Access question starts with for a listen address. */
export function accessDefault(bind: string): string {
  if (isLoopback(bind)) return 'local';
  return bind === '0.0.0.0' || bind === '::' ? 'remote' : bind;
}

/**
 * The listen address for an answer to the Access question: local, remote, an IP address, or the
 * address it listens on now (which may be a host name). Local and remote keep the current address
 * when it already is one (`::1`, or `::` for IPv6 as well as IPv4).
 */
export function accessBind(answer: string, current = ''): string {
  const a = answer.trim();
  if (/^l(ocal)?$/i.test(a)) return isLoopback(current) ? current : '127.0.0.1';
  if (/^r(emote)?$/i.test(a)) return current === '0.0.0.0' || current === '::' ? current : '0.0.0.0';
  if (isIP(a) || a === 'localhost' || (a && a === current)) return a;
  throw new InstallError(`Answer local or remote (or an address to listen on), not "${a}".`);
}

function publicHost(): string {
  const r = trySh('hostname', ['-f']);
  return r.ok && r.out ? r.out : hostname();
}

// ---------------------------------------------------------------------------
// install

export interface InstallMekoDeps {
  ask: Prompter;
  /** A person is there to answer. */
  canAsk: boolean;
  check: (url: string, apiKey: string) => Promise<{ ok: true } | { ok: false; error: string }>;
  /**
   * Sets up Meko for Claude Code; throws MekoSetupError when that's declined or doesn't finish.
   * `keyFromEnv`: the key came from the installer's MEKO_API_KEY, so it isn't stored in config.toml.
   * Left out when Claude Code isn't installed.
   */
  ensureInClaude?: (url: string, apiKey: string, keyFromEnv: boolean) => Promise<void>;
  /** Sets up Meko for Kiro, the same way; left out when Kiro isn't installed. */
  ensureInKiro?: (url: string, apiKey: string) => Promise<void>;
  /**
   * The Meko running on this computer (`findLocalMeko`): used when there's no key, offered when a
   * key fails, and never stored, since AgentK reads it from Meko when it runs.
   */
  localMeko?: () => Promise<LocalMeko | null>;
  ui: Pick<typeof ui, 'ok' | 'warn' | 'note' | 'section'>;
}

/**
 * The installer's Meko step. AgentK uses a Meko Cloud (or other) key when there is one, and else
 * the Meko running on this computer (`meko token --json`), which isn't stored: the server reads it
 * from Meko. With no key, it asks for one and Enter takes the Meko on this computer; with no one to
 * ask, that Meko is taken. `--meko-url` and `--meko-key` name a Meko (`meko install_extension
 * agentk` passes the local one's, which isn't stored either). A key that's given must work, unless
 * someone there switches to the Meko on this computer or goes on without Meko. With neither a key
 * nor a Meko here, the install goes on and the server uses Meko once it runs. `--no-meko` is
 * ignored. Declining Meko's setup for Claude Code or Kiro (each offered only where it's
 * installed) is a warning, not a stop. Updates `cfg.meko`; returns whether there's a Meko to use.
 */
export async function installMeko(cfg: Config, o: { keyArg?: string; urlArg?: string; noMeko: boolean }, d: InstallMekoDeps): Promise<boolean> {
  const later = 'AgentK uses the Meko on this computer once it runs (meko start), or add a Meko Cloud key on the Meko tab in Settings.';
  if (o.noMeko) d.ui.warn('--no-meko', 'ignored: Meko can no longer be turned off. Without a key, AgentK uses the Meko on this computer.');
  const local = d.localMeko ? await d.localMeko() : null;
  const hostOf = (url: string) => {
    try {
      return new URL(url).host;
    } catch {
      return url;
    }
  };
  // Older installs stored the local Meko's URL and token: the server reads them from Meko now.
  if (isLocalMeko(local, cfg.meko.url, cfg.meko.apiKey)) {
    cfg.meko.url = '';
    cfg.meko.apiKey = '';
  }
  let meko = resolveMeko(cfg);
  // A key goes only where it already goes: a URL given without a key starts keyless, so the key is
  // asked for (or given with --meko-key) rather than sent to the new host.
  if (o.urlArg) meko = { url: o.urlArg, apiKey: '' };
  let storeKey = false;
  if (o.keyArg) {
    meko = { ...meko, apiKey: o.keyArg };
    storeKey = true;
  } else if (!meko.apiKey) {
    const web = resolveMekoWebUrl(cfg);
    if (d.canAsk) d.ui.note(`Meko gives projects shared memory.${web && !local ? ` No account or key yet? Create them at ${web}.` : ''}`);
    const what = meko.url === DEFAULT_MEKO_URL ? 'Meko Cloud API key' : `Meko API key for ${hostOf(meko.url)}`;
    const k = (await d.ask(local ? `${what} (Enter to use the Meko on this computer, ${hostOf(local.url)})` : `${what} (Enter to skip)`, '')).trim();
    if (k) {
      meko = { ...meko, apiKey: k };
      storeKey = true;
    } else if (local) {
      meko = { url: local.url, apiKey: local.token };
    } else {
      d.ui.section('Checks');
      d.ui.warn('Meko', `not set up: no key, and no Meko running on this computer. ${later}`);
      return false;
    }
  }
  // The local Meko's own URL and token (what `meko install_extension` passes) aren't stored.
  let onLocal = isLocalMeko(local, meko.url, meko.apiKey);
  if (onLocal) storeKey = false;

  d.ui.section('Checks');
  let check = await d.check(meko.url, meko.apiKey);
  if (!check.ok && local && !onLocal && d.canAsk) {
    d.ui.warn('Meko', `Meko check failed (${meko.url}): ${check.error}`);
    if (!/^n/i.test(await d.ask(`Use the Meko on this computer (${hostOf(local.url)}) instead? (Y/n)`, 'y'))) {
      // Not stored, so the key that failed isn't used (unless the environment or Claude Code's settings have it).
      meko = { url: local.url, apiKey: local.token };
      onLocal = true;
      storeKey = false;
      if (cfg.meko.apiKey) {
        cfg.meko.url = '';
        cfg.meko.apiKey = '';
      }
      check = await d.check(meko.url, meko.apiKey);
    }
  }
  if (!check.ok) {
    const why = `Meko check failed (${meko.url}): ${check.error}`;
    if (!d.canAsk) throw new InstallError(`${why}. Fix the key, or run the installer without one to use the Meko on this computer.`);
    d.ui.warn('Meko', why);
    if (!/^y/i.test(await d.ask('Continue without Meko? (y/N)', 'n'))) {
      throw new InstallError(`${why}. Run the installer again with the right key, or without one to use the Meko on this computer.`);
    }
    if (cfg.meko.apiKey === meko.apiKey) {
      cfg.meko.url = '';
      cfg.meko.apiKey = '';
    }
    d.ui.warn('Meko', `not set up: the key wasn't kept. ${later}`);
    return false;
  }
  d.ui.ok('Meko', onLocal ? `on this computer (${hostOf(meko.url)})` : meko.url.replace(/^https?:\/\//, ''));

  let inClaude = !!d.ensureInClaude;
  if (d.ensureInClaude) {
    try {
      // Not typed, and not from config.toml or Claude Code's settings: only this process has it.
      const keyFromEnv = !storeKey && resolveMekoWithSource(cfg).keySource === 'env' && meko.apiKey === process.env.MEKO_API_KEY;
      await d.ensureInClaude(meko.url, meko.apiKey, keyFromEnv);
    } catch (e) {
      if (!(e instanceof MekoSetupError)) throw e;
      inClaude = false;
      d.ui.warn('Meko in Claude', `${e.message} AgentK uses Meko; Claude Code sessions do once it's set up (the Meko tab in Settings can do it).`);
    }
  }
  const settingsEnv = readClaudeSettingsEnv();
  // Without Claude Code, nothing puts a key from the installer's environment into its settings, and
  // the service has no MEKO_* environment: config.toml keeps it, as it would a typed one.
  const noClaude = !d.ensureInClaude;
  const keep = onLocal ? 'unchanged' : mekoKeyForConfig({
    apiKey: meko.apiKey, url: meko.url, typed: storeKey || noClaude, settingsKey: settingsEnv.MEKO_API_KEY, settingsUrl: settingsEnv.MEKO_MCP_URL,
    envKey: noClaude ? undefined : process.env.MEKO_API_KEY, defaultUrl: DEFAULT_MEKO_URL,
  });
  if (keep !== 'unchanged') {
    cfg.meko.apiKey = keep.apiKey;
    cfg.meko.url = keep.url;
  }
  if (inClaude) d.ui.ok('Meko in Claude', 'plugin, MCP server and key in place');
  if (d.ensureInKiro) {
    try {
      await d.ensureInKiro(meko.url, meko.apiKey);
      d.ui.ok('Meko in Kiro', 'MCP server and agent in place');
    } catch (e) {
      if (!(e instanceof MekoSetupError)) throw e;
      d.ui.warn('Meko in Kiro', `${e.message} AgentK uses Meko; Kiro sessions do once it's set up (the Meko tab in Settings can do it).`);
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
      `no coding agent found. Install Claude Code (https://claude.com/claude-code), OpenCode ${HARNESS_CAPS.opencode.minVersion} or later (https://opencode.ai) `
        + `or Kiro CLI ${HARNESS_CAPS.kiro.minVersion} or later (https://kiro.dev/cli/), sign in to it, and run the installer again.`,
    );
  }
  if (!found.includes(cfg.agents.default)) cfg.agents.default = found[0];
  return found;
}

export async function runInstall(args: string[]): Promise<void> {
  const { values } = parseArgs({
    args,
    options: {
      'non-interactive': { type: 'boolean', default: false },
      'repos-root': { type: 'string' },
      'meko-key': { type: 'string' },
      'meko-url': { type: 'string' },
      'no-meko': { type: 'boolean', default: false },
      bind: { type: 'string' },
      port: { type: 'string' },
      'no-service': { type: 'boolean', default: false },
      'install-deps': { type: 'boolean', default: false },
      'no-install-deps': { type: 'boolean', default: false },
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
    // `install_extension` names: the server and a move go by it, never resolving it again.
    const inMeko = mekoDirOf(paths.home);
    if (inMeko) cfg.meko.home = inMeko;
    else if (process.env.MEKO_HOME) cfg.meko.home = resolve(process.env.MEKO_HOME);

    // 1. Basics
    if (!process.env.AK_INSTALLER) ui.title('AgentK setup');
    ui.section('Setup');
    if (interactive && process.stdin.isTTY) ui.note('Press Enter to keep the value in brackets.');
    const reposRoot = values['repos-root'] ?? (await ask('Repos folder', cfg.repos.roots[0] ?? '~/code'));
    cfg.repos.roots = fresh ? [reposRoot] : [reposRoot, ...cfg.repos.roots.filter((r) => r !== reposRoot)];
    if (fresh) cfg.repos.cloneInto = reposRoot;
    if (values.bind) {
      cfg.server.bind = values.bind;
    } else {
      // A fresh install on a desktop is for this computer only; over SSH or with no display,
      // the browser is somewhere else.
      const def = fresh && !process.env.AK_BIND ? (isHeadless() ? 'remote' : 'local') : accessDefault(cfg.server.bind);
      if (interactive && process.stdin.isTTY) {
        ui.note('Local: only this computer, no login. Remote: other devices too (like your phone), over HTTPS with a login.');
      }
      for (;;) {
        try {
          cfg.server.bind = accessBind(await ask('Access', def), cfg.server.bind);
          break;
        } catch (e) {
          if (!(e instanceof InstallError && interactive && process.stdin.isTTY)) throw e;
          ui.warn('Access', e.message);
        }
      }
    }
    const portStr = values.port ?? (await ask('Port', String(cfg.server.port)));
    const port = Number(portStr);
    if (!Number.isInteger(port) || port < 1 || port > 65535) throw new InstallError(`invalid port: ${portStr}`);
    cfg.server.port = port;
    if (!(interactive && process.stdin.isTTY)) {
      ui.row('Repos folder', reposRoot);
      ui.row('Access', `${isLoopback(cfg.server.bind) ? 'local' : 'remote'}, on ${cfg.server.bind}:${cfg.server.port}`);
    }

    // 2. The coding agents: at least one, or nothing can run.
    const versions: Partial<Record<HarnessId, string | null>> = {};
    for (const id of HARNESS_IDS) versions[id] = await cliVersion(harnessCommand(cfg, id));
    const agents = settleAgents(cfg, versions);

    // 3. Meko and 4. Meko in each agent
    const canAsk = interactive && !!process.stdin.isTTY;
    const setup = { canAsk, installDeps: values['install-deps'], noInstallDeps: values['no-install-deps'] };
    const urlArg = values['meko-url'];
    if (urlArg !== undefined && !/^https?:\/\/[^\s]+$/.test(urlArg)) throw new InstallError(`--meko-url takes Meko's MCP URL, like http://127.0.0.1:7778/mcp (got "${urlArg}").`);
    await installMeko(cfg, { keyArg: values['meko-key'], urlArg, noMeko: values['no-meko'] }, {
      ask,
      canAsk,
      check: checkMeko,
      localMeko: () => findLocalMeko(cfg.meko.home || null),
      ensureInKiro: !agents.includes('kiro') ? undefined : async (url, apiKey) => {
        const gaps = async () => kiroMekoGaps(readKiroMeko(), url, kiroAgentLoadsMcp());
        await ensureMekoSetup(
          { url, apiKey, client: 'kiro', ...setup },
          { gaps, latest: () => latestMekoSetup(), run: (pkg) => runMekoSetup(pkg, { url, apiKey, client: 'kiro' }), ask, say, warn: (l, t) => ui.warn(l, t) },
        );
      },
      ensureInClaude: !agents.includes('claude') ? undefined : async (url, apiKey, keyFromEnv) => {
        // The plugin, its MCP server and the key where its hooks read it, all on this Meko, with
        // tokens it accepts. Anything wrong is set up by Meko's own installer, after asking. A key
        // from the environment must also end up in Claude Code's settings: the service has no
        // MEKO_* environment, so it would take a different key from there.
        const gaps = async () => {
          const c = readClaudeMeko(claudeUserDir(), claudeConfigPath());
          return [
            ...mekoClaudeGaps(c, url, () => claudeHasMekoMcp(cfg.claude.command)),
            ...envKeyGap(c, apiKey, keyFromEnv),
            ...(await claudeTokenGaps(c, url, apiKey, tryMekoToken)),
          ];
        };
        await ensureMekoSetup(
          { url, apiKey, ...setup },
          { gaps, latest: () => latestMekoSetup(), run: (pkg) => runMekoSetup(pkg, { url, apiKey }), ask, say, warn: (l, t) => ui.warn(l, t) },
        );
      },
      ui,
    });
    ui.ok('Agents', agents.map((id) => `${HARNESS_CAPS[id].label} ${versions[id]}`).join(', ') + ` (new sessions start on ${HARNESS_CAPS[cfg.agents.default].label})`);
    if (existsSync(expandHome(reposRoot))) ui.ok('Repos folder', reposRoot);
    else ui.warn('Repos folder', `${reposRoot} doesn't exist yet`);

    // 5. Keep SQLite off network filesystems
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
    const tmpWarning = tmpHomeWarning(paths.home);
    if (tmpWarning) ui.warn('Home', tmpWarning);
    const move = checkMove(paths, cfg);
    if (move && 'plan' in move) ui.note(`AgentK's home moves to ${tildify(move.plan.to)} once the server has started: every session ends and resumes there.`);
    else if (move) ui.warn('Home', `stays in ${tildify(paths.home)} for now: ${move.skip}`);
    // Create the certificate now (the server reuses it), so a problem with it shows up here.
    const tls = await loadTls(paths, cfg);
    if (tls) ui.ok('Certificate', tls.selfSigned ? 'self-signed, created for this host' : 'from config');

    // 6. Services
    ui.section('Services');
    if (!isLoopback(cfg.server.bind) && macFirewallOn()) {
      ui.note('The macOS firewall is on. If other devices (like your phone) can\'t connect, allow incoming connections for');
      ui.note(`${tildify(join(paths.app, 'current', 'node', 'bin', 'node'))} in System Settings > Network > Firewall > Options.`);
    }
    let running = false;
    if (values['no-service']) {
      ui.warn('Services', 'skipped (--no-service). Start the server with: agentk serve');
    } else if (installServices(paths)) {
      const probe = await waitForServer(cfg);
      if (probe.ok) {
        ui.ok('Server', `answering on port ${cfg.server.port} (version ${probe.version})`);
        running = true;
      } else {
        ui.warn('Server', `not answering yet (${probe.error}). See: agentk logs`);
      }
    }

    // 7. Setup link and next steps
    const handle = openDb(db);
    try {
      const scheme = tls ? 'https' : 'http';
      const host = isLoopback(cfg.server.bind) ? '127.0.0.1' : publicHost();
      const base = `${scheme}://${host}:${cfg.server.port}`;
      const local = isLoopback(cfg.server.bind);
      ui.blank();
      // Local access needs no login (see localAccess in auth.ts), so there's no setup link to open.
      if (local || getOwner(handle)) {
        ui.text(style.bold(running ? 'Done. AgentK is running:' : 'Done. AgentK will be at:'));
        ui.blank();
        ui.text(`    ${style.cyan(`${base}/`)}`);
      } else {
        ui.text(style.bold('Done. Open this link to create your login (it works once):'));
        ui.blank();
        ui.text(`    ${style.cyan(`${base}/setup?token=${issueSetupToken(handle)}`)}`);
      }
      ui.blank();
      if (local) {
        ui.row('Sign-in', getOwner(handle)
          ? `none on this computer; the login for ${getOwner(handle)!.username} is kept for remote access`
          : 'none: it only listens on this computer');
      }
      if (tls?.selfSigned) ui.row('Certificate', 'self-signed, so your browser warns once');
      if (!local) ui.row('New link', 'agentk setup-link');
      ui.row('Status', 'agentk status');
      ui.row('Logs', 'agentk logs -f');
      ui.blank();
    } finally {
      handle.close();
    }
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
 * Deletes everything in the AgentK home, except the session worktrees and design files
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
  const { ask, close } = makePrompter(!values['non-interactive']);
  try {
    say('This stops AgentK and ends every running session it hosts (Claude Code or Kiro).');
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
    // A session host started without a service (--no-service) ends with its sessions too.
    await new SessionHost(paths, { services: () => null }).stop().catch(() => undefined);
    for (const link of commandLinks()) {
      try {
        unlinkSync(link);
      } catch {
        // not installed there
      }
    }
    // The Kiro agent AgentK wrote is its own: Kiro sessions ran as it.
    removeKiroAgent();
    if (purge) {
      const db = dbPath(paths, loadConfig(paths));
      const kept = purgeHome(paths, deleteWork);
      if (!db.startsWith(paths.home + '/')) removeDb(db);
      // The link the move into Meko's data folder left at the old home goes with what it pointed at.
      if (!kept.length) removeOldHomeLink(paths.home);
      say(kept.length ? `Removed ${tildify(paths.home)} except ${kept.map(tildify).join(' and ')}.` : `Removed ${tildify(paths.home)}.`);
      if (deleteWork) say('Run `git worktree prune` in each repo to forget the deleted worktrees.');
    } else {
      say(`State kept in ${tildify(paths.home)} (use --purge to delete it).`);
    }
  } finally {
    close();
  }
}
