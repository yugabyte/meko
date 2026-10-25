#!/usr/bin/env node
import { execFileSync, spawn } from 'node:child_process';
import { closeSync, existsSync, openSync, readFileSync, readSync, rmSync, statSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import { findMekoCaptureScript } from './capture.js';
import { loginShell, scrubSessionMarkers } from './launcher.js';
import { dbPath, loadConfig, resolveMeko, type Config } from './config.js';
import { Core } from './core.js';
import { kvGet, kvSet, openDb, snapshotDb } from './db.js';
import { errMsg, fileLogger, type Logger } from './log.js';
import { handoff } from './handoff.js';
import { probeLocal } from './health.js';
import { listenPrivate, socketId } from './hooks.js';
import { MekoClient } from './meko.js';
import { getPaths, SHARED_TMP, tildify, withLocalBin, type Paths } from './paths.js';
import { buildServer, findWebDir } from './server.js';
import { AGENTK_PATH as BASE_PATH } from '../shared/mekourls.js';
import { hostServices, moveJobLog, servicesFor, startMoveJob } from './service.js';
import { linkCommand } from './cmdlink.js';
import { openBrowser } from './browser.js';
import { NoHostError, SessionHost, startDetached } from './hostclient.js';
import { HostRunningError, startHost } from './host/host.js';
import { BEAT_MS, EXIT_BUSY, heldLease, InstanceBusyError, InstanceLock } from './instance.js';
import { HEALTHY_MS, uncountStart, Updater } from './updater.js';
import { probeLocalMeko } from './mekolocal.js';
import {
  checkMove, finishMove, HomeMove, MEKO_COMMANDS, mekoDirOf, moveHome, readJournal, refuseOtherMeko, rememberMekoHome, stopAll, tmpHomeWarning,
} from './movehome.js';

const here = dirname(fileURLToPath(import.meta.url));

function version(): string {
  if (process.env.AK_VERSION && process.env.AK_VERSION !== 'unknown') return process.env.AK_VERSION;
  try {
    return (JSON.parse(readFileSync(join(here, '..', '..', 'package.json'), 'utf8')) as { version: string }).version;
  } catch {
    return '0.0.0';
  }
}

/**
 * Under systemd or launchd the environment is bare. Take PATH from the user's login shell
 * so git, gh and claude resolve the way they do in a terminal. ~/.local/bin is added
 * when it's missing: the installer puts `agentk` (and gh, when it installs it) there. MEKO_HOME
 * and XDG_STATE_HOME come from there too when unset, since they decide where Meko's data folder
 * is (paths.ts `mekoHome`), as they did for the installer.
 */
function adoptLoginPath(): void {
  try {
    const out = execFileSync(loginShell(), ['-l', '-c', 'printf "%s\\0%s\\0%s" "$PATH" "${MEKO_HOME-}" "${XDG_STATE_HOME-}"'], { encoding: 'utf8', timeout: 5000, stdio: ['ignore', 'pipe', 'ignore'] });
    const [p, meko, state] = out.split('\0');
    if (p?.trim()) process.env.PATH = p.trim();
    if (meko && !process.env.MEKO_HOME) process.env.MEKO_HOME = meko;
    if (state && !process.env.XDG_STATE_HOME) process.env.XDG_STATE_HOME = state;
  } catch {
    /* keep the inherited PATH */
  }
  process.env.PATH = withLocalBin(process.env.PATH ?? '', homedir());
}

/**
 * Before `serve` opens anything: a move of the home into Meko's data folder that's under way (a
 * journal says so) must finish first. With services, its job does that (started again here, after a
 * reboot say) and this start stops; without them, `serve` moves the home itself, as it does at the
 * first start without services of a release that brings the move. Returns the home to serve.
 */
async function moveFirst(paths: Paths): Promise<Paths> {
  const j = readJournal(paths.home);
  const svc = servicesFor(paths.home);
  const say = { info: (m: string) => console.error(`agentk: ${m}`), warn: (m: string, d?: string) => console.error(`agentk: ${m}${d ? `: ${d}` : ''}`) };
  if (svc?.installed('server')) {
    if (j?.phase !== 'moving') return paths;
    startMoveJob(svc.kind, { bin: join(process.env.AK_APP_DIR ?? join(paths.app, 'current'), 'bin', 'agentk'), from: j.from, to: j.to });
    console.error(`agentk: AgentK's home is moving to ${j.to}; the move starts the server again when it's done. See: ${moveJobLog(j.to)}`);
    process.exit(EXIT_BUSY);
  }
  if (existsSync(paths.updatePending) && j?.phase !== 'moving') return paths;
  // Another server holds the home (here or on another host): this start is refused by the lease
  // below, as ever, and touches nothing first.
  if (heldLease(paths.lock)) return paths;
  const r = j?.phase === 'moving' ? { plan: j } : checkMove(paths, loadConfig(paths, () => undefined));
  if (!r) return paths;
  if ('skip' in r) {
    say.warn(`AgentK's home stays in ${paths.home} for now`, r.skip);
    return paths;
  }
  const to = await moveHome(r.plan, { log: say, stop: async () => void (await stopAll(paths, null)), services: null });
  return getPaths(to);
}

/**
 * Keeps systemd-tmpfiles' age cleanup off a home in /var/tmp (on systemd versions that skip folders
 * with a lock on them): a `flock --shared` held while the server runs. It ends with the server, as
 * its stdin closes. Without flock (macOS, where nothing cleans /var/tmp by age), nothing.
 */
function holdTmpLock(home: string, log: Logger): void {
  if (process.platform !== 'linux' || !home.startsWith(SHARED_TMP + '/')) return;
  const warning = tmpHomeWarning(home);
  if (warning) log.warn(warning);
  const child = spawn('flock', ['--shared', home, 'cat'], { stdio: ['pipe', 'ignore', 'ignore'] });
  child.on('error', (e) => log.warn(`couldn't lock ${home} against /var/tmp's cleanup`, errMsg(e)));
  child.unref();
}

async function serve(): Promise<void> {
  scrubSessionMarkers();
  adoptLoginPath();
  const paths = await moveFirst(getPaths());
  const cfg = loadConfig(paths);
  const v = version();
  // Before anything else touches the home: another server using it (here, or on another host
  // that mounts it) would lose its hook socket and share the database.
  let lock: InstanceLock;
  try {
    lock = await InstanceLock.acquire(paths.lock, tildify(paths.home), { version: v, port: 0 });
  } catch (e) {
    if (!(e instanceof InstanceBusyError)) throw e;
    // Not in server.log: that belongs to the server holding the home.
    console.error(`agentk: ${e.message}`);
    uncountStart(paths, v);
    process.exit(EXIT_BUSY);
  }
  const log = fileLogger(join(paths.logs, 'server.log'));
  try {
    await startServer(paths, cfg, v, lock, log);
  } catch (e) {
    // In server.log, not only on stderr, which may reach no one (a journal that isn't kept): a
    // start that keeps failing after an update (a migration, the guard in Core.start) is rolled
    // back by the launcher, and this says why. The lease goes too, so the next start needn't wait.
    log.error(`AgentK ${v} couldn't start: ${errMsg(e)}`);
    lock.release();
    process.exit(1);
  }
}

/** A start once it holds the home's lease: the database, Core, the web server, and the updater. */
async function startServer(paths: Paths, cfg: Config, v: string, lock: InstanceLock, log: Logger): Promise<void> {
  try {
    if (linkCommand(paths)) log.info('added the agentk command (~/.local/bin/agentk) beside agent-k-local, its name before the rename');
  } catch (e) {
    log.warn(`couldn't add the agentk command to ~/.local/bin: ${errMsg(e)}`);
  }
  const db = openDb(dbPath(paths, cfg));
  let core: Core | null = null;
  const meko = new MekoClient(() => resolveMeko(core?.config ?? cfg), v);
  const mekoCaptureScript = cfg.meko.captureTranscripts ? findMekoCaptureScript() : null;
  if (!cfg.meko.captureTranscripts) log.info('session transcripts are not sent to Meko (meko.capture_transcripts is off)');
  else if (!mekoCaptureScript) log.warn('the meko-agent-skills plugin was not found; session transcripts will not be captured to Meko');
  core = new Core({ paths, config: cfg, db, version: v, meko, mekoCaptureScript, log });

  let app: Awaited<ReturnType<typeof buildServer>> | null = null;
  /** The web socket file this server bound (socketId), to tell it from one another server put there since. */
  let webSockId: string | null = null;
  let shuttingDown = false;
  const shutdown = async (code: number) => {
    if (shuttingDown) return;
    shuttingDown = true;
    clearInterval(beat);
    log.info('shutting down (sessions keep running in the session host)');
    try {
      await app?.close();
      // Only while the file is still the one it bound: on a lost lease, another server on this home
      // may have bound its own since. Gone, Meko says AgentK isn't running.
      if (webSockId && socketId(paths.webSock) === webSockId) rmSync(paths.webSock, { force: true });
      await core?.stop();
      db.close();
    } catch (e) {
      log.warn('shutdown error', errMsg(e));
    } finally {
      // Even after an error: a lease left behind keeps other hosts out until it goes stale.
      lock.release();
    }
    process.exit(code);
  };
  const beat = setInterval(() => {
    if (shuttingDown || lock.beat()) return;
    log.error(`another AgentK server took over ${paths.home} (this one stalled for too long); stopping`);
    void shutdown(1);
  }, BEAT_MS);
  beat.unref();

  const updater = new Updater({
    paths,
    config: () => core!.config,
    version: v,
    kvGet: (k) => kvGet(db, k),
    kvSet: (k, val) => kvSet(db, k, val),
    snapshotDb: (dest) => snapshotDb(db, dest),
    log: (m) => log.info(m),
    onChange: () => core!.changed(),
    requestRestart: () => void shutdown(0),
  });
  core.updateStatus = () => updater.status();

  await core.start();
  const webDir = findWebDir(here);
  if (!webDir) log.warn('web assets not found; run `npm run build:web` (or use the Vite dev server)');
  app = await buildServer(core, { webDir, updater: updater.enabled ? updater : null });
  // Its only listener: Meko serves it to browsers at BASE_PATH, after its own sign-in.
  await app.ready();
  await listenPrivate(app.server, paths.webSock, 'the web socket');
  webSockId = socketId(paths.webSock);
  log.info(`AgentK ${v} listening on ${paths.webSock}`);
  void probeLocalMeko(mekoDirOf(paths.home) ?? (cfg.meko.home || null)).then((p) => {
    if (p.meko?.webUrl) log.info(`browsers open it at ${p.meko.webUrl}${BASE_PATH}`);
    else if (p.meko) log.warn('the Meko on this computer is too old to serve AgentK to browsers: update it with meko update');
    else log.warn('no Meko is running on this computer, so no browser can reach AgentK: start it with meko start');
  });

  updater.start();
  // The move into Meko's data folder: now when this version isn't being tried out, else once it has
  // proved healthy. A move that finished starting the services ends here.
  finishMove(paths.home, log);
  holdTmpLock(paths.home, log);
  const homeMove = new HomeMove(paths, log);
  homeMove.check();
  setTimeout(() => {
    updater.markHealthy();
    homeMove.check();
  }, HEALTHY_MS).unref();
  // A deliberate stop isn't a failed start of a version being tried out. Counted once: a second
  // signal (Ctrl+C twice) or a stop during an update's restart mustn't take off another start.
  const stop = () => {
    if (!shuttingDown) updater.stoppedCleanly();
    void shutdown(0);
  };
  process.on('SIGTERM', stop);
  process.on('SIGINT', stop);
}

/** Where browsers open AgentK: Meko's web URL with BASE_PATH, or why there's none. */
async function webUrl(paths: Paths, cfg: Config): Promise<{ url: string | null; why: string }> {
  const p = await probeLocalMeko(mekoDirOf(paths.home) ?? (cfg.meko.home || null));
  if (p.meko?.webUrl) return { url: `${p.meko.webUrl}${BASE_PATH}`, why: '' };
  if (p.meko) return { url: null, why: 'the Meko on this computer is too old to serve AgentK: update it with meko update' };
  if (p.state === 'stopped') return { url: null, why: "Meko isn't running: start it with meko start" };
  return { url: null, why: 'no Meko on this computer serves AgentK: install Meko, then run meko install_extension agentk' };
}

/** Opens AgentK in this computer's browser through Meko (`meko ui`), or prints its URL. */
async function ui(): Promise<void> {
  const paths = getPaths();
  const cfg = loadConfig(paths);
  const probe = await probeLocal(paths);
  if (!probe.ok) throw new Error(`AgentK isn't answering (${probe.error}). Start it with: agentk start (or agentk serve)`);
  const web = await webUrl(paths, cfg);
  if (!web.url) throw new Error(`AgentK is running, but ${web.why}.`);
  if (await openBrowser(web.url)) console.log(`Opened ${web.url}`);
  else console.log(`Couldn't open a browser here. Open ${web.url} in yours.`);
}

/** `agentk _handoff --json`, which Meko runs at its start (handoff.ts): not for people to run. */
function runHandoff(): void {
  const paths = getPaths();
  console.log(JSON.stringify(handoff(paths, loadConfig(paths, () => undefined))));
}

/** Prints the server log; -f keeps following it, -n sets how many lines to show first. */
async function logs(args: string[]): Promise<void> {
  const file = join(getPaths().logs, 'server.log');
  const follow = args.includes('-f') || args.includes('--follow');
  const ni = args.findIndex((a) => a === '-n' || a === '--lines');
  const n = ni >= 0 ? Math.max(1, Number(args[ni + 1]) || 50) : 50;
  if (!existsSync(file)) {
    console.error(`No log yet at ${file}. Is the server running? (agentk status)`);
    process.exitCode = 1;
    return;
  }
  const text = readFileSync(file, 'utf8');
  process.stdout.write(text.split('\n').slice(-n - 1).join('\n'));
  if (!follow) return;
  let pos = statSync(file).size;
  setInterval(() => {
    let size: number;
    try {
      size = statSync(file).size;
    } catch {
      return;
    }
    if (size < pos) pos = 0; // rotated
    if (size === pos) return;
    const fd = openSync(file, 'r');
    const buf = Buffer.alloc(size - pos);
    readSync(fd, buf, 0, buf.length, pos);
    closeSync(fd);
    pos = size;
    process.stdout.write(buf.toString('utf8'));
  }, 500);
  await new Promise(() => {});
}

async function selftest(): Promise<void> {
  const { DatabaseSync } = await import('node:sqlite');
  new DatabaseSync(':memory:').prepare('SELECT 1').get();
  const pty = await import('node-pty');
  if (typeof pty.spawn !== 'function') throw new Error('node-pty failed to load');
  // Spawn a real process: node-pty can load and still fail here (e.g. spawn-helper not executable on macOS).
  await new Promise<void>((resolve, reject) => {
    const p = pty.spawn('/bin/sh', ['-c', 'exit 7'], { name: 'xterm', cols: 80, rows: 24, cwd: '/', env: { PATH: process.env.PATH ?? '/usr/bin:/bin' } });
    const timer = setTimeout(() => reject(new Error('node-pty: the test process did not exit')), 10_000);
    p.onExit(({ exitCode }) => {
      clearTimeout(timer);
      if (exitCode === 7) resolve();
      else reject(new Error(`node-pty: the test process exited with ${exitCode}, expected 7`));
    });
  });
  if (!findWebDir(here)) throw new Error('web assets are missing');
  console.log(`AgentK ${version()} selftest ok`);
}

async function status(): Promise<void> {
  const paths = getPaths();
  rememberMekoHome(paths);
  const cfg = loadConfig(paths);
  const web = await webUrl(paths, cfg);
  console.log(`AgentK ${version()}`);
  console.log(`home:     ${paths.home}`);
  console.log(`database: ${dbPath(paths, cfg)}`);
  console.log(`url:      ${web.url ?? `none (${web.why})`}`);
  // Only this home's services: another install on the account has its own.
  const svc = servicesFor(paths.home);
  if (svc) {
    for (const s of ['server', 'host'] as const) console.log(`${svc.id(s).padEnd(32)} ${svc.installed(s) ? svc.state(s).text : 'not installed'}`);
  } else if (hostServices()) {
    console.log('services: none for this home (the installed services run another one)');
  }
  const host = new SessionHost(paths, { services: () => null });
  const up = await host.serverRunning().catch((e) => {
    console.log(`host:     not answering (${errMsg(e)})`);
    return null;
  });
  if (up) {
    const panes = await host.panes().catch((e) => {
      console.log(`sessions: couldn't list them (${errMsg(e)})`);
      return null;
    });
    console.log(`host:     version ${host.hello?.version}, pid ${host.hello?.pid}`);
    if (panes) console.log(`sessions: ${panes.size} (${[...panes.values()].filter((p) => !p.dead).length} running)`);
  } else if (up === false) {
    console.log('host:     not running');
  }
  const probe = await probeLocal(paths);
  console.log(`server:   ${probe.ok ? `responding (version ${probe.version})` : `not responding (${probe.error})`}`);
  console.log(`logs:     ${join(paths.logs, 'server.log')}`);
}

/** `host`, `host stop` and `host restart`. */
async function host(args: string[]): Promise<void> {
  const paths = getPaths();
  const client = new SessionHost(paths, { services: () => null });
  const svc = servicesFor(paths.home);
  if (args[0] === 'stop' || args[0] === 'restart') {
    if (args[0] === 'stop') {
      // A running server would start the host again at once and resume every session in it.
      const server = heldLease(paths.lock);
      if (server) {
        throw new Error(
          `The AgentK server for this home is running (pid ${server.pid} on ${server.host}), and it would start the ` +
            `session host again and resume every session. Stop both with: agentk stop, or use: agentk host restart`,
        );
      }
    }
    const n = (await client.panes().catch(() => new Map())).size;
    try {
      await client.stop();
    } catch (e) {
      if (!(e instanceof NoHostError)) throw e;
      if (args[0] === 'stop') {
        console.log('No session host is running.');
        return;
      }
    }
    if (args[0] === 'stop') {
      console.log(`Stopped the session host${n ? ` and the ${n} session(s) in it. They resume when the server starts` : ''}.`);
      return;
    }
    // A service restarts its host by itself; without one, start it again here.
    if (svc?.installed('host')) svc.restart('host');
    else startDetached(paths);
    console.log(`Restarted the session host${n ? `; the server resumes the ${n} session(s) in a few seconds` : ''}.`);
    return;
  }
  if (args.length) throw new Error(`Unknown host command: ${args[0]} (stop or restart)`);
  scrubSessionMarkers();
  adoptLoginPath();
  const log = fileLogger(join(paths.logs, 'host.log'));
  // A terminal it was started from may close: the host keeps going.
  process.on('SIGHUP', () => undefined);
  let handle: Awaited<ReturnType<typeof startHost>> | null = null;
  // After a stop request (or a lost lease): let go of the socket and the lease, then exit.
  const exit = () => void (handle?.close() ?? Promise.resolve()).finally(() => process.exit(0));
  try {
    handle = await startHost({ paths, version: version(), appDir: process.env.AK_APP_DIR ?? null, log, onStop: exit });
  } catch (e) {
    if (!(e instanceof HostRunningError)) throw e;
    console.error(`agentk: ${e.message}`);
    process.exit(EXIT_BUSY);
  }
  process.on('SIGTERM', exit);
  process.on('SIGINT', exit);
}

/**
 * Stops the server, then the session host: a running server would start the host again and resume
 * every session. Their supervisors restart a killed process, so installed services stop through them.
 */
async function stop(): Promise<void> {
  const paths = getPaths();
  rememberMekoHome(paths);
  const svc = servicesFor(paths.home);
  const { stopped, sessions: n } = await stopAll(paths, svc);
  const resume = svc?.installed('server') ? 'agentk start' : 'agentk serve';
  if (!stopped) console.log('AgentK is not running.');
  else console.log(`Stopped AgentK${n ? ` and the ${n} session(s) in it. They resume at: ${resume}` : ''}.`);
}

/** Starts the services `stop` stopped. */
async function start(): Promise<void> {
  const paths = getPaths();
  rememberMekoHome(paths);
  const svc = servicesFor(paths.home);
  if (!svc?.installed('server')) throw new Error('No background services run this home: run agentk serve, or install them with agentk install.');
  for (const s of ['host', 'server'] as const) {
    if (!svc.installed(s)) continue;
    svc.start(s);
    for (let i = 0; i < 50 && !svc.state(s).running; i++) await sleep(100);
    if (!svc.state(s).running) throw new Error(`${svc.id(s)} didn't start. Check: ${svc.statusHint(s)}`);
  }
  console.log('Started AgentK; the server resumes the sessions in a few seconds. Check: agentk status');
}

function help(): void {
  console.log(`AgentK ${version()}

Usage: agentk <command>

  serve         Run the server (the background service runs this for you)
  install       Configure this host, set up Meko for Claude Code if needed (asks first; --install-deps
                to skip asking; AgentK uses Meko's Meko Cloud link, else the Meko running on this
                computer; --meko-key, with --meko-url for another server, links one in Meko), and install
                the background services (systemd or launchd). Browsers reach AgentK through the Meko on
                this computer, at /ext/agentk/
  uninstall     Stop and remove the background services (--purge also deletes AgentK's home, in Meko's
                data folder, except its session worktrees and designs; add --delete-worktrees to delete
                those too)
  status        Show services, the URL (Meko's, at /ext/agentk/) and sessions
  ui            Open AgentK in your browser through Meko (prints its URL when there's no browser to open)
  stop          Stop the server and the session host, ending every session (they resume at start, or
                serve without services; the services also start again at the next login)
  start         Start the background services again after stop
  attach [name] Show a session's terminal here (Ctrl+\\ detaches; Claude keeps running); no name lists them
  host          Run the session host that holds every Claude (the background service runs this for you);
                host restart ends every session and resumes them; host stop ends them (with the server stopped)
  logs [-f]     Print the server log (-f to follow, -n <lines>)
  selftest      Check that the runtime and bundled modules load
  version       Print the version`);
}

async function main(): Promise<void> {
  const [cmd, ...args] = process.argv.slice(2);
  if (cmd && MEKO_COMMANDS.includes(cmd)) refuseOtherMeko(getPaths());
  switch (cmd) {
    case 'serve': return serve();
    case 'install': return (await import('./install.js')).runInstall(args);
    case 'uninstall': return (await import('./install.js')).runUninstall(args);
    case 'setup-link':
      throw new Error('Meko signs people in to AgentK now: run meko setup_link for a link that sets up (or resets) the login.');
    case 'selftest': return selftest();
    case 'status': return status();
    case 'ui': return ui();
    case 'stop': return stop();
    case 'start': return start();
    case 'attach': return (await import('./attachcli.js')).runAttach(getPaths(), args);
    case 'host': return host(args);
    case 'logs': return logs(args);
    // AgentK's session tools as a stdio MCP server; sessions start it as mcpmain.js (tools.ts).
    case 'mcp': return (await import('./mcp.js')).runMcp(version());
    // The job that moves the home into Meko's data folder (movehome.ts); not for people to run.
    case '_move-home': return (await import('./movehome.js')).runMoveJob(args);
    // Where the server listens and the web settings it had, for Meko (handoff.ts); not for people to run.
    case '_handoff': return runHandoff();
    case 'version':
    case '--version':
    case '-v':
      console.log(version());
      return;
    case undefined:
    case 'help':
    case '--help':
    case '-h':
      return help();
    default:
      console.error(`Unknown command: ${cmd}\n`);
      help();
      process.exit(2);
  }
}

main().catch((e) => {
  console.error(errMsg(e));
  process.exit(1);
});
