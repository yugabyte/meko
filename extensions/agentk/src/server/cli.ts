#!/usr/bin/env node
import { execFileSync, spawn } from 'node:child_process';
import { closeSync, existsSync, openSync, readFileSync, readSync, statSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import { getOwner, issueSetupToken, setupTokenOutstanding } from './auth.js';
import { findMekoCaptureScript } from './capture.js';
import { loginShell, scrubSessionMarkers } from './launcher.js';
import { dbPath, isLoopback, loadConfig, resolveMeko, type Config } from './config.js';
import { Core } from './core.js';
import { kvGet, kvSet, openDb, snapshotDb } from './db.js';
import { errMsg, fileLogger, type Logger } from './log.js';
import { probeHost, probeLocal, serverUrl } from './health.js';
import { MekoClient } from './meko.js';
import { getPaths, SHARED_TMP, tildify, withLocalBin, type Paths } from './paths.js';
import { buildServer, findWebDir } from './server.js';
import { loadTls } from './tls.js';
import { hostServices, servicesFor, startMoveJob } from './service.js';
import { linkCommand } from './cmdlink.js';
import { openBrowser } from './browser.js';
import { NoHostError, SessionHost, startDetached } from './hostclient.js';
import { HostRunningError, startHost } from './host/host.js';
import { BEAT_MS, EXIT_BUSY, heldLease, InstanceBusyError, InstanceLock } from './instance.js';
import { HEALTHY_MS, uncountStart, Updater } from './updater.js';
import { checkMove, finishMove, HomeMove, moveHome, readJournal, rememberMekoHome, stopAll, tmpHomeWarning } from './movehome.js';

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
 * when it's missing: the installer puts `agentk` (and gh, when it installs it) there.
 */
function adoptLoginPath(): void {
  try {
    const p = execFileSync(loginShell(), ['-l', '-c', 'printf %s "$PATH"'], { encoding: 'utf8', timeout: 5000, stdio: ['ignore', 'pipe', 'ignore'] });
    if (p.trim()) process.env.PATH = p.trim();
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
    console.error(`agentk: AgentK's home is moving to ${j.to}; the move starts the server again when it's done. See: ${join(j.to, 'logs', 'move-home.log')}`);
    process.exit(EXIT_BUSY);
  }
  if (existsSync(paths.updatePending) && j?.phase !== 'moving') return paths;
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
    lock = await InstanceLock.acquire(paths.lock, tildify(paths.home), { version: v, port: cfg.server.port });
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
  let shuttingDown = false;
  const shutdown = async (code: number) => {
    if (shuttingDown) return;
    shuttingDown = true;
    clearInterval(beat);
    log.info('shutting down (sessions keep running in the session host)');
    try {
      await app?.close();
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

  const tls = await loadTls(paths, cfg);
  await core.start();
  const webDir = findWebDir(here);
  if (!webDir) log.warn('web assets not found; run `npm run build:web` (or use the Vite dev server)');
  app = await buildServer(core, { tls, webDir, updater: updater.enabled ? updater : null });
  await app.listen({ host: cfg.server.bind, port: cfg.server.port });

  const url = serverUrl(cfg, !!tls);
  log.info(`AgentK ${v} listening on ${url}`);
  if (tls) log.info(`TLS certificate SHA-256 fingerprint: ${tls.fingerprint}${tls.selfSigned ? ' (self-signed)' : ''}`);
  if (isLoopback(cfg.server.bind)) {
    const owner = getOwner(db);
    log.info(`local access: browsers on this computer need no login${owner ? ` (the login for ${owner.username} applies again once it listens on the network)` : ''}`);
  } else if (!getOwner(db)) {
    // Don't replace a link the installer (or `setup-link`) already printed.
    if (setupTokenOutstanding(db)) log.info('waiting for first-time setup: open the link the installer printed, or run `agentk setup-link` for a new one');
    else log.info(`first-time setup: ${url}/setup?token=${issueSetupToken(db)}`);
  }

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

async function setupLink(): Promise<void> {
  const paths = getPaths();
  const cfg = loadConfig(paths);
  const db = openDb(dbPath(paths, cfg));
  const tls = !!(cfg.tls.cert && cfg.tls.key) || !isLoopback(cfg.server.bind);
  const reset = !!getOwner(db);
  console.log(`${serverUrl(cfg, tls)}/setup?token=${issueSetupToken(db)}`);
  console.log(reset
    ? 'This one-time link resets the username, password and authenticator for this install. It expires in 7 days.'
    : 'Open this one-time link to create your login. It expires in 7 days.');
  if (isLoopback(cfg.server.bind)) console.log('No login is needed while AgentK only listens on this computer; the login applies once it listens on the network.');
  db.close();
}

/** Opens the web UI in this computer's browser, or prints its URL when there's none to open. */
async function ui(): Promise<void> {
  const cfg = loadConfig(getPaths());
  const url = serverUrl(cfg, !!(cfg.tls.cert && cfg.tls.key) || !isLoopback(cfg.server.bind));
  const probe = await probeLocal(cfg);
  if (!probe.ok) {
    const at = probeHost(cfg.server.bind);
    throw new Error(`AgentK isn't answering on ${at.includes(':') ? `[${at}]` : at}:${cfg.server.port} (${probe.error}). Start it with: agentk start (or agentk serve)`);
  }
  if (await openBrowser(url)) console.log(`Opened ${url}`);
  else console.log(`Couldn't open a browser here. Open ${url} in yours.`);
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
  const tls = await loadTls(paths, cfg).catch(() => null);
  const url = serverUrl(cfg, !!tls);
  console.log(`AgentK ${version()}`);
  console.log(`home:     ${paths.home}`);
  console.log(`database: ${dbPath(paths, cfg)}`);
  console.log(`url:      ${url}`);
  if (tls) console.log(`tls:      ${tls.fingerprint}${tls.selfSigned ? ' (self-signed)' : ''}`);
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
  const probe = await probeLocal(cfg);
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
                to skip asking; Meko is optional, --no-meko skips it; a Meko running on this computer is
                offered first; --meko-url and --meko-key name one), install the background services
                (systemd or launchd), print the setup link
  uninstall     Stop and remove the background services (--purge also deletes AgentK's home, in Meko's
                data folder, except its session worktrees and designs; add --delete-worktrees to delete
                those too)
  setup-link    Print a one-time link to create (or reset) your login
  status        Show services, URL, certificate fingerprint and sessions
  ui            Open the web UI in your browser (prints its URL when there's no browser to open)
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
  switch (cmd) {
    case 'serve': return serve();
    case 'install': return (await import('./install.js')).runInstall(args);
    case 'uninstall': return (await import('./install.js')).runUninstall(args);
    case 'setup-link': return setupLink();
    case 'selftest': return selftest();
    case 'status': return status();
    case 'ui': return ui();
    case 'stop': return stop();
    case 'start': return start();
    case 'attach': return (await import('./attachcli.js')).runAttach(getPaths(), args);
    case 'host': return host(args);
    case 'logs': return logs(args);
    // The job that moves the home into Meko's data folder (movehome.ts); not for people to run.
    case '_move-home': return (await import('./movehome.js')).runMoveJob(args);
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
