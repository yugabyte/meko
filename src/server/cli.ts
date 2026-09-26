#!/usr/bin/env node
import { execFileSync } from 'node:child_process';
import { closeSync, existsSync, openSync, readFileSync, readSync, statSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { getOwner, issueSetupToken, setupTokenOutstanding } from './auth.js';
import { findMekoCaptureScript } from './capture.js';
import { loginShell, scrubClaudeMarkers } from './claude.js';
import { dbPath, isLoopback, loadConfig, resolveMeko } from './config.js';
import { Core } from './core.js';
import { kvGet, kvSet, openDb, snapshotDb } from './db.js';
import { errMsg, fileLogger } from './log.js';
import { probeLocal } from './health.js';
import { MekoClient } from './meko.js';
import { getPaths } from './paths.js';
import { buildServer, findWebDir } from './server.js';
import { fqdn, loadTls } from './tls.js';
import { hostServices } from './service.js';
import { Tmux } from './tmux.js';
import { HEALTHY_MS, Updater } from './updater.js';

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
 * so git, gh, tmux and claude resolve the way they do in a terminal.
 */
function adoptLoginPath(): void {
  try {
    const p = execFileSync(loginShell(), ['-l', '-c', 'printf %s "$PATH"'], { encoding: 'utf8', timeout: 5000, stdio: ['ignore', 'pipe', 'ignore'] });
    if (p.trim()) process.env.PATH = p.trim();
  } catch {
    /* keep the inherited PATH */
  }
}

function baseUrl(cfg: ReturnType<typeof loadConfig>, tls: boolean): string {
  const host = isLoopback(cfg.server.bind) ? '127.0.0.1' : fqdn();
  return `${tls ? 'https' : 'http'}://${host}:${cfg.server.port}`;
}

async function serve(): Promise<void> {
  scrubClaudeMarkers();
  adoptLoginPath();
  const paths = getPaths();
  const cfg = loadConfig(paths);
  const v = version();
  const db = openDb(dbPath(paths, cfg));
  const log = fileLogger(join(paths.logs, 'server.log'));
  let core: Core | null = null;
  const meko = new MekoClient(() => resolveMeko(core?.config ?? cfg), v);
  const mekoCaptureScript = cfg.meko.captureTranscripts ? findMekoCaptureScript() : null;
  if (!cfg.meko.captureTranscripts) log.info('session transcripts are not sent to Meko (meko.capture_transcripts is off)');
  else if (!mekoCaptureScript) log.warn('the meko-agent-skills plugin was not found; session transcripts will not be captured to Meko');
  core = new Core({ paths, config: cfg, db, version: v, meko, mekoCaptureScript, tmux: new Tmux(cfg.tmux.socket, paths.tmuxConf), log });

  let app: Awaited<ReturnType<typeof buildServer>> | null = null;
  let shuttingDown = false;
  const shutdown = async (code: number) => {
    if (shuttingDown) return;
    shuttingDown = true;
    log.info('shutting down (sessions keep running in tmux)');
    try {
      await app?.close();
      await core!.stop();
      db.close();
    } catch (e) {
      log.warn('shutdown error', errMsg(e));
    }
    process.exit(code);
  };

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
  if (!getOwner(db)) {
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
  const svc = hostServices();
  if (svc) {
    for (const s of ['server', 'tmux'] as const) console.log(`${svc.id(s).padEnd(32)} ${svc.installed(s) ? svc.state(s).text : 'not installed'}`);
  }
  const panes = await new Tmux(cfg.tmux.socket, paths.tmuxConf).panes().catch((e) => {
    console.log(`tmux:     couldn't list sessions (${errMsg(e)})`);
    return null;
  });
  if (panes) console.log(`tmux sessions: ${panes.size} (${[...panes.values()].filter((p) => !p.dead).length} running)`);
  const probe = await probeLocal(cfg);
  console.log(`server:   ${probe.ok ? `responding (version ${probe.version})` : `not responding (${probe.error})`}`);
  console.log(`logs:     ${join(paths.logs, 'server.log')}`);
}

function help(): void {
  console.log(`AgentK Local ${version()}

Usage: agent-k-local <command>

  serve         Run the server (the background service runs this for you)
  install       Configure this host, install the background services (systemd or launchd), print the setup link
  uninstall     Stop and remove the background services (--purge also deletes ~/.agent-k-local except its
                session worktrees and designs; add --delete-worktrees to delete those too)
  setup-link    Print a one-time link to create (or reset) your login
  status        Show services, URL, certificate fingerprint and sessions
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
