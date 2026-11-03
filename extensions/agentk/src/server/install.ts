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
import { DEFAULT_MEKO_URL, dbPath, envMekoWebUrl, loadConfig, onLoopback, ownMekoKey, saveConfig, type Config } from './config.js';
import { assertFree, InstanceBusyError } from './instance.js';
import { checkMeko, tryMekoToken } from './meko.js';
import { isLocalMeko, linkedThroughMeko, probeLocalMeko, type LocalMekoProbe } from './mekolocal.js';
import { handOverMekoKey, type OwnMekoLink } from './mekohandover.js';
import { AGENTK_PATH, MEKO_INSTALL_COMMAND } from '../shared/mekourls.js';
import {
  MekoSetupError, ensureMekoSetup, envKeyGap, latestMekoSetup, mekoClaudeGaps, readClaudeMeko, runMekoSetup,
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
    'Or rerun with --no-service to skip the service setup, and start the server with: me_koder serve'
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

// ---------------------------------------------------------------------------
// install

export interface InstallMekoDeps {
  ask: Prompter;
  /** A person is there to answer. */
  canAsk: boolean;
  /** Checks a key AgentK keeps itself, which it does only under a Meko too old to keep the link. */
  check: (url: string, apiKey: string) => Promise<{ ok: true } | { ok: false; error: string }>;
  /**
   * Sets up Meko for Claude Code; throws MekoSetupError when that's declined or doesn't finish.
   * `keyFromEnv`: the key came from the installer's MEKO_API_KEY. `local`: the server is the Meko
   * on this computer, with its token (for the command to run by hand). Left out when Claude Code
   * isn't installed.
   */
  ensureInClaude?: (url: string, apiKey: string, keyFromEnv: boolean, local: boolean) => Promise<void>;
  /** Sets up Meko for Kiro, the same way; left out when Kiro isn't installed. */
  ensureInKiro?: (url: string, apiKey: string, local: boolean) => Promise<void>;
  /**
   * What `meko token --json` says (`probeLocalMeko`): the Meko running on this computer, and the
   * Meko Cloud link it keeps for its extensions. Neither is stored: the server reads them from Meko.
   */
  probe?: () => Promise<LocalMekoProbe>;
  /** Hands Meko a Meko Cloud key to keep as its link (`handOverMekoKey`: `meko cloud_link`). */
  link: (l: OwnMekoLink) => Promise<{ ok: true } | { ok: false; error: string }>;
  ui: Pick<typeof ui, 'ok' | 'warn' | 'note' | 'section'>;
}

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

/**
 * The installer's Meko step. Meko owns the Meko Cloud link: AgentK uses it when Meko has one, and
 * else the Meko running on this computer (`meko token --json`), and stores neither. The installer
 * doesn't ask for a key: one is linked in Meko's web UI (Settings › Meko Cloud) or with `meko
 * cloud_link`. A key given with `--meko-key` (and `--meko-url` for another server) goes to Meko, which
 * checks it and keeps it; a copy of the local Meko's token (what `meko extension install agentk`
 * passes) is just the local Meko; any other key for a server on this computer is set aside with a
 * warning. AgentK's own key from before Meko kept the link (config.toml, the environment, Claude
 * Code's settings) is handed to Meko once, as the server does at start, and left where it was, for
 * a rollback to the release before. One from the installer's environment that AgentK keeps using
 * (Meko refused it, or is too old to keep a link) is stored in config.toml when there's no Claude
 * Code to hold it, since the service has no MEKO_* environment.
 *
 * Under a Meko too old to keep a link, AgentK keeps a key itself, as it did: a given key is checked
 * and stored in config.toml, and its own key is checked. A given key that fails stops the install
 * unless someone there goes on without it; `--no-meko` only keeps a failure from stopping the
 * install and skips Meko's setup for the agents. Declining Meko's setup for Claude Code or Kiro
 * (each offered only where it's installed) is a warning, not a stop. Updates `cfg.meko`; returns
 * whether there's a Meko to use.
 */
export async function installMeko(cfg: Config, o: { keyArg?: string; urlArg?: string; noMeko: boolean }, d: InstallMekoDeps): Promise<boolean> {
  const linkHint = "Link Meko Cloud in Meko's web UI (Settings › Meko Cloud), or with meko cloud_link.";
  if (o.noMeko) d.ui.warn('--no-meko', "Meko can't be turned off any more: AgentK uses Meko's Meko Cloud link, else the Meko on this computer. A failed Meko check won't stop the install, and Meko isn't set up for the agents.");
  const probe = d.probe ? await d.probe() : { state: 'none' as const, meko: null };
  const local = probe.meko;
  const cloud = probe.cloud;
  // Older installs stored the local Meko's URL and token: the server reads them from Meko now.
  if (isLocalMeko(local, cfg.meko.url, cfg.meko.apiKey)) {
    cfg.meko.url = '';
    cfg.meko.apiKey = '';
  }
  const givenUrl = o.urlArg ?? DEFAULT_MEKO_URL;
  const given = o.keyArg && !isLocalMeko(local, givenUrl, o.keyArg) && !onLoopback(givenUrl)
    ? { url: givenUrl, apiKey: o.keyArg, webUrl: '', apiUrl: '' } : null;
  // A key for a server on this computer is never linked as Meko Cloud: when Meko isn't running, a
  // copy of its own token (what `meko extension install agentk` passes) can't be told apart.
  if (o.keyArg && !given && !isLocalMeko(local, givenUrl, o.keyArg)) {
    d.ui.warn('--meko-key', `not linked: ${hostOf(givenUrl)} is on this computer, so AgentK uses the Meko on this computer.`);
  }
  const own = ownMekoKey(cfg);
  const ownLink = own.apiKey && !isLocalMeko(local, own.url, own.apiKey) && !onLoopback(own.url)
    ? { url: own.url, apiKey: own.apiKey, webUrl: cfg.meko.webUrl || envMekoWebUrl(), apiUrl: cfg.meko.apiUrl || process.env.MEKO_API_URL?.trim() || '' }
    : null;

  d.ui.section('Checks');
  let meko: { url: string; apiKey: string } | null = null;
  let label = '';
  let keyFromEnv = false;
  const giveUp = async (why: string, fix: string): Promise<void> => {
    if (!d.canAsk && !o.noMeko) throw new InstallError(`${why}. ${fix}`);
    d.ui.warn('Meko', why);
    if (!o.noMeko && !/^y/i.test(await d.ask('Continue without it? (y/N)', 'n'))) throw new InstallError(`${why}. ${fix}`);
  };
  if (cloud) {
    // Meko keeps the link: a given key, else AgentK's own from before (once), goes to Meko.
    const hand = given ?? (cloud.set ? null : ownLink);
    if (hand) {
      const r = await d.link(hand);
      if (r.ok) {
        // AgentK's own key stays in config.toml: the server never reads it again, and a rollback to
        // the release before, which reads only it, still finds it. A Meko that keeps the key (its
        // `cloud` has `linked`) reaches Meko Cloud for the agents through its own server.
        meko = cloud.linked !== undefined && local ? { url: local.url, apiKey: local.token } : { url: hand.url, apiKey: hand.apiKey };
        label = `Meko Cloud, linked in Meko (${hostOf(hand.url)})`;
      } else if (given) {
        await giveUp(`Meko didn't link ${hand.url}: ${r.error}`, 'Fix the key, or run the installer without one to use the Meko on this computer.');
      } else {
        // The server tries again at start; meanwhile AgentK keeps the key it had.
        d.ui.warn('Meko Cloud', `Meko didn't take AgentK's key (${r.error}): AgentK keeps using it until it's linked. ${linkHint}`);
        meko = { url: hand.url, apiKey: hand.apiKey };
        label = hostOf(hand.url);
        keyFromEnv = own.keySource === 'env';
      }
    }
    if (!meko && cloud.apiKey) {
      meko = { url: cloud.url, apiKey: cloud.apiKey };
      label = `Meko Cloud, linked in Meko (${hostOf(cloud.url)})`;
    }
    if (!meko && linkedThroughMeko(cloud) && local) {
      meko = { url: local.url, apiKey: local.token };
      label = `Meko Cloud, linked in Meko${cloud.webUrl ? ` (${hostOf(cloud.webUrl)})` : ''}`;
    }
  } else {
    // A Meko too old to keep the link: AgentK keeps a key itself, as before.
    const key = given ?? ownLink;
    if (key) {
      const check = await d.check(key.url, key.apiKey);
      if (check.ok) {
        if (given) {
          cfg.meko.url = given.url === DEFAULT_MEKO_URL ? '' : given.url;
          cfg.meko.apiKey = given.apiKey;
        }
        meko = { url: key.url, apiKey: key.apiKey };
        label = hostOf(key.url);
        keyFromEnv = !given && own.keySource === 'env';
      } else {
        await giveUp(`Meko check failed (${key.url}): ${check.error}`, given ? 'Run the installer again with the right key, or without one to use the Meko on this computer.' : `Fix or remove the key (${own.keySource}).`);
      }
    }
  }
  if (!meko && local) {
    meko = { url: local.url, apiKey: local.token };
    label = `on this computer (${hostOf(local.url)})`;
  }
  if (!meko) {
    d.ui.warn('Meko', `not set up: no Meko Cloud link, and no Meko running on this computer. AgentK uses the Meko on this computer once it runs (meko start). ${linkHint}`);
    return false;
  }
  // A key from the installer's own MEKO_API_KEY that AgentK keeps using: without Claude Code, nothing
  // puts it in Claude Code's settings, and the service has no MEKO_* environment, so config.toml
  // keeps it, as it would a typed one.
  if (keyFromEnv && !d.ensureInClaude && meko.apiKey === process.env.MEKO_API_KEY) {
    cfg.meko.apiKey = meko.apiKey;
    cfg.meko.url = meko.url === DEFAULT_MEKO_URL ? '' : meko.url;
  }
  d.ui.ok('Meko', label);
  // The Meko on this computer, with its own token: the only server `meko token` is the key for.
  const onLocal = !!local && meko.url === local.url && meko.apiKey === local.token;
  if (!cloud?.apiKey && label.startsWith('on this computer') && d.canAsk) d.ui.note(`To keep projects' memory in Meko Cloud instead: ${linkHint}`);

  // --no-meko never set Meko up for the agents, and still doesn't.
  const ensureInClaude = o.noMeko ? undefined : d.ensureInClaude;
  const ensureInKiro = o.noMeko ? undefined : d.ensureInKiro;
  if (ensureInClaude) {
    try {
      await ensureInClaude(meko.url, meko.apiKey, keyFromEnv && meko.apiKey === process.env.MEKO_API_KEY, onLocal);
      d.ui.ok('Meko in Claude', 'plugin, MCP server and key in place');
    } catch (e) {
      if (!(e instanceof MekoSetupError)) throw e;
      d.ui.warn('Meko in Claude', `${e.message} AgentK uses Meko; Claude Code sessions do once it's set up (AgentK's Settings › Harness › Claude Code can do it).`);
    }
  }
  if (ensureInKiro) {
    try {
      await ensureInKiro(meko.url, meko.apiKey, onLocal);
      d.ui.ok('Meko in Kiro', 'MCP server and agent in place');
    } catch (e) {
      if (!(e instanceof MekoSetupError)) throw e;
      d.ui.warn('Meko in Kiro', `${e.message} AgentK uses Meko; Kiro sessions do once it's set up (AgentK's Settings › Harness › Kiro can do it).`);
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
        + `or Kiro CLI ${HARNESS_CAPS.kiro.minVersion} or later (https://kiro.dev/cli/), sign in to it, and run meko extension install agentk again.`,
    );
  }
  if (!found.includes(cfg.agents.default)) cfg.agents.default = found[0];
  return found;
}

/**
 * AgentK's prerequisites check (git, curl, gh, a coding agent and their sign-ins; it installs what's
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
  if (r.status !== 0) throw new InstallError(r.error ? `couldn't check the tools: ${r.error.message}` : 'the tools AgentK needs aren\'t all there (see above).');
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
    ui.title('AgentK setup');
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
    // Browsers reach AgentK only through the Meko on this computer, which serves it at /ext/agentk/
    // with its own sign-in; with no Meko here, nothing could.
    if ((await probeLocalMeko(cfg.meko.home || null)).state === 'none') {
      throw new InstallError('AgentK runs behind the Meko on this computer, which serves it to your browser, and there is ' +
        `no meko command here. Install Meko first (${MEKO_INSTALL_COMMAND}), then run: meko extension install agentk`);
    }

    // 3. The coding agents: at least one, or nothing can run.
    const versions: Partial<Record<HarnessId, string | null>> = {};
    for (const id of HARNESS_IDS) versions[id] = await cliVersion(harnessCommand(cfg, id));
    const agents = settleAgents(cfg, versions);

    // 4. Meko and 5. Meko in each agent
    const canAsk = interactive && !!process.stdin.isTTY;
    const setup = { canAsk, installDeps: values['install-deps'], noInstallDeps: values['no-install-deps'] };
    const urlArg = values['meko-url'];
    if (urlArg !== undefined && !/^https?:\/\/[^\s]+$/.test(urlArg)) throw new InstallError(`--meko-url takes Meko's MCP URL, like http://127.0.0.1:7778/mcp (got "${urlArg}").`);
    await installMeko(cfg, { keyArg: values['meko-key'], urlArg, noMeko: values['no-meko'] }, {
      ask,
      canAsk,
      check: checkMeko,
      probe: () => probeLocalMeko(cfg.meko.home || null),
      link: (l) => handOverMekoKey(cfg.meko.home || null, l),
      ensureInKiro: !agents.includes('kiro') ? undefined : async (url, apiKey, onLocal) => {
        const gaps = async () => kiroMekoGaps();
        await ensureMekoSetup(
          { url, apiKey, client: 'kiro', local: onLocal ? { home: cfg.meko.home || null } : null, ...setup },
          { gaps, latest: () => latestMekoSetup(), run: (pkg) => runMekoSetup(pkg, { url, apiKey, client: 'kiro' }), ask, say, warn: (l, t) => ui.warn(l, t) },
        );
      },
      ensureInClaude: !agents.includes('claude') ? undefined : async (url, apiKey, keyFromEnv, onLocal) => {
        // The plugin and the key where its hooks read it, on this Meko, with a token it accepts
        // (its MCP server is Meko's: meko agent connect). Anything wrong is set up by Meko's own
        // installer, after asking. A key from the environment must also end up in Claude Code's
        // settings: the service has no MEKO_* environment, so it would take a different key from there.
        const gaps = async () => {
          const c = readClaudeMeko(claudeUserDir());
          return [
            ...mekoClaudeGaps(c, url),
            ...envKeyGap(c, apiKey, keyFromEnv),
            ...(await claudeTokenGaps(c, url, apiKey, tryMekoToken)),
          ];
        };
        await ensureMekoSetup(
          { url, apiKey, local: onLocal ? { home: cfg.meko.home || null } : null, ...setup },
          { gaps, latest: () => latestMekoSetup(), run: (pkg) => runMekoSetup(pkg, { url, apiKey }), ask, say, warn: (l, t) => ui.warn(l, t) },
        );
      },
      ui,
    });
    ui.ok('Agents', agents.map((id) => `${HARNESS_CAPS[id].label} ${versions[id]}`).join(', ') + ` (new sessions start on ${HARNESS_CAPS[cfg.agents.default].label})`);
    if (existsSync(expandHome(reposRoot))) ui.ok('Repos folder', reposRoot);
    else ui.warn('Repos folder', `${reposRoot} doesn't exist yet`);

    // 6. Keep SQLite off network filesystems
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

    // 7. Services
    ui.section('Services');
    let running = false;
    if (values['no-service']) {
      ui.warn('Services', 'skipped (--no-service). Start the server with: me_koder serve');
    } else if (installServices(paths)) {
      const probe = await waitForServer(paths);
      if (probe.ok) {
        ui.ok('Server', `answering (version ${probe.version})`);
        running = true;
      } else {
        ui.warn('Server', `not answering yet (${probe.error}). See: me_koder logs`);
      }
    }

    // 8. Where to open it
    const meko = await probeLocalMeko(cfg.meko.home || null);
    ui.blank();
    if (meko.meko?.webUrl) {
      ui.text(style.bold(running ? 'Done. AgentK is running:' : 'Done. AgentK will be at:'));
      ui.blank();
      ui.text(`    ${style.cyan(`${meko.meko.webUrl}${AGENTK_PATH}`)}`);
      ui.blank();
    } else {
      ui.text(style.bold('Done.'));
      ui.blank();
      ui.row('Browser', meko.meko?.webOff
        ? "Meko's web UI is off: turn it on with meko ui start, then open AgentK with me_koder ui"
        : meko.meko
          ? 'the Meko on this computer is too old to serve AgentK: update it with meko update'
          : "Meko isn't running: start it with meko start, then open AgentK with me_koder ui");
    }
    ui.row('Status', 'me_koder status');
    ui.row('Logs', 'me_koder logs -f');
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
