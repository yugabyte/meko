#!/usr/bin/env node
import { execFileSync } from 'node:child_process';
import { closeSync, existsSync, openSync, readFileSync, readSync, statSync } from 'node:fs';
import { homedir, hostname } from 'node:os';
import { dirname, join } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import { getOwner, issueSetupToken, setupTokenOutstanding } from './auth.js';
import { findMekoCaptureScript } from './capture.js';
import { loginShell, scrubSessionMarkers } from './launcher.js';
import { dbPath, isLoopback, loadConfig, resolveMeko } from './config.js';
import { Core } from './core.js';
import { kvGet, kvSet, openDb, snapshotDb } from './db.js';
import { errMsg, fileLogger } from './log.js';
import { probeLocal } from './health.js';
import { MekoClient } from './meko.js';
import { getPaths, tildify, withLocalBin } from './paths.js';
import { buildServer, findWebDir } from './server.js';
import { fqdn, loadTls } from './tls.js';
import { hostServices, servicesFor } from './service.js';
import { NoHostError, SessionHost, startDetached } from './hostclient.js';
import { HostRunningError, startHost } from './host/host.js';
import { BEAT_MS, EXIT_BUSY, heldLease, InstanceBusyError, InstanceLock } from './instance.js';
import { HEALTHY_MS, uncountStart, Updater } from './updater.js';

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
 * when it's missing: the installer puts `agent-k-local` (and gh, when it installs it) there.
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

function baseUrl(cfg: ReturnType<typeof loadConfig>, tls: boolean): string {
  const host = isLoopback(cfg.server.bind) ? '127.0.0.1' : fqdn();
  return `${tls ? 'https' : 'http'}://${host}:${cfg.server.port}`;
}

async function serve(): Promise<void> {
  scrubSessionMarkers();
  adoptLoginPath();
  const paths = getPaths();
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
    console.error(`agent-k-local: ${e.message}`);
    uncountStart(paths, v);
    process.exit(EXIT_BUSY);
  }
  const db = openDb(dbPath(paths, cfg));
  const log = fileLogger(join(paths.logs, 'server.log'));
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
    log.error(`another AgentK Local server took over ${paths.home} (this one stalled for too long); stopping`);
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

  const url = baseUrl(cfg, !!tls);
  log.info(`AgentK Local ${v} listening on ${url}`);
  if (tls) log.info(`TLS certificate SHA-256 fingerprint: ${tls.fingerprint}${tls.selfSigned ? ' (self-signed)' : ''}`);
  if (isLoopback(cfg.server.bind)) {
    const owner = getOwner(db);
    log.info(`local access: browsers on this computer need no login${owner ? ` (the login for ${owner.username} applies again once it listens on the network)` : ''}`);
  } else if (!getOwner(db)) {
    // Don't replace a link the installer (or `setup-link`) already printed.
    if (setupTokenOutstanding(db)) log.info('waiting for first-time setup: open the link the installer printed, or run `agent-k-local setup-link` for a new one');
    else log.info(`first-time setup: ${url}/setup?token=${issueSetupToken(db)}`);
  }

  updater.start();
  setTimeout(() => updater.markHealthy(), HEALTHY_MS).unref();
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
  console.log(`${baseUrl(cfg, tls)}/setup?token=${issueSetupToken(db)}`);
  console.log(reset
    ? 'This one-time link resets the username, password and authenticator for this install. It expires in 7 days.'
    : 'Open this one-time link to create your login. It expires in 7 days.');
  if (isLoopback(cfg.server.bind)) console.log('No login is needed while AgentK Local only listens on this computer; the login applies once it listens on the network.');
  db.close();
}

/** Prints the server log; -f keeps following it, -n sets how many lines to show first. */
async function logs(args: string[]): Promise<void> {
  const file = join(getPaths().logs, 'server.log');
  const follow = args.includes('-f') || args.includes('--follow');
  const ni = args.findIndex((a) => a === '-n' || a === '--lines');
  const n = ni >= 0 ? Math.max(1, Number(args[ni + 1]) || 50) : 50;
  if (!existsSync(file)) {
    console.error(`No log yet at ${file}. Is the server running? (agent-k-local status)`);
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
  console.log(`AgentK Local ${version()} selftest ok`);
}

async function status(): Promise<void> {
  const paths = getPaths();
  const cfg = loadConfig(paths);
  const tls = await loadTls(paths, cfg).catch(() => null);
  const url = baseUrl(cfg, !!tls);
  console.log(`AgentK Local ${version()}`);
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
          `The AgentK Local server for this home is running (pid ${server.pid} on ${server.host}), and it would start the ` +
            `session host again and resume every session. Stop both with: agent-k-local stop, or use: agent-k-local host restart`,
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
    console.error(`agent-k-local: ${e.message}`);
    process.exit(EXIT_BUSY);
  }
  process.on('SIGTERM', exit);
  process.on('SIGINT', exit);
}

/** Whether a pid is an AgentK Local server: after a crash its lease can name a pid another process has now. */
function isServer(pid: number): boolean {
  try {
    return /(agent-k-local|cli\.[jt]s) serve\b/.test(execFileSync('ps', ['-p', String(pid), '-o', 'args='], { encoding: 'utf8' }));
  } catch {
    return false;
  }
}

/**
 * Stops the server, then the session host: a running server would start the host again and resume
 * every session. Their supervisors restart a killed process, so installed services stop through them.
 */
async function stop(): Promise<void> {
  const paths = getPaths();
  const client = new SessionHost(paths, { services: () => null });
  const svc = servicesFor(paths.home);
  const n = (await client.panes().catch(() => new Map())).size;
  let stopped = false;
  if (svc?.installed('server')) {
    stopped = svc.state('server').running;
    svc.stop('server');
  }
  // A server outside the service (`serve` by hand, or on another host sharing this home) holds the lease too.
  const server = heldLease(paths.lock);
  if (server) {
    if (server.host !== hostname()) throw new Error(`The AgentK Local server for this home runs on ${server.host} (pid ${server.pid}): stop it there.`);
    if (isServer(server.pid)) {
      // Its SIGTERM handler shuts down cleanly and releases the lease.
      process.kill(server.pid, 'SIGTERM');
      for (let i = 0; i < 200 && heldLease(paths.lock); i++) await sleep(100);
      if (heldLease(paths.lock)) throw new Error(`The server (pid ${server.pid}) didn't stop within 20 seconds. See: agent-k-local logs`);
      stopped = true;
    }
  }
  if (svc?.installed('host')) {
    stopped ||= svc.state('host').running;
    svc.stop('host');
  } else {
    try {
      await client.stop();
      stopped = true;
    } catch (e) {
      if (!(e instanceof NoHostError)) throw e;
    }
  }
  const resume = svc?.installed('server') ? 'agent-k-local start' : 'agent-k-local serve';
  if (!stopped) console.log('AgentK Local is not running.');
  else console.log(`Stopped AgentK Local${n ? ` and the ${n} session(s) in it. They resume at: ${resume}` : ''}.`);
}

/** Starts the services `stop` stopped. */
async function start(): Promise<void> {
  const svc = servicesFor(getPaths().home);
  if (!svc?.installed('server')) throw new Error('No background services run this home: run agent-k-local serve, or install them with agent-k-local install.');
  for (const s of ['host', 'server'] as const) {
    if (!svc.installed(s)) continue;
    svc.start(s);
    for (let i = 0; i < 50 && !svc.state(s).running; i++) await sleep(100);
    if (!svc.state(s).running) throw new Error(`${svc.id(s)} didn't start. Check: ${svc.statusHint(s)}`);
  }
  console.log('Started AgentK Local; the server resumes the sessions in a few seconds. Check: agent-k-local status');
}

function help(): void {
  console.log(`AgentK Local ${version()}

Usage: agent-k-local <command>

  serve         Run the server (the background service runs this for you)
  install       Configure this host, set up Meko for Claude Code if needed (asks first; --install-deps
                to skip asking; Meko is optional, --no-meko skips it), install the background services
                (systemd or launchd), print the setup link
  uninstall     Stop and remove the background services (--purge also deletes ~/.agent-k-local except its
                session worktrees and designs; add --delete-worktrees to delete those too)
  setup-link    Print a one-time link to create (or reset) your login
  status        Show services, URL, certificate fingerprint and sessions
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
    case 'stop': return stop();
    case 'start': return start();
    case 'attach': return (await import('./attachcli.js')).runAttach(getPaths(), args);
    case 'host': return host(args);
    case 'logs': return logs(args);
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
