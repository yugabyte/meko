#!/usr/bin/env node
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { getOwner, issueSetupToken } from './auth.js';
import { loginShell, scrubClaudeMarkers } from './claude.js';
import { dbPath, isLoopback, loadConfig, resolveMeko } from './config.js';
import { Core } from './core.js';
import { kvGet, kvSet, openDb, snapshotDb } from './db.js';
import { consoleLogger, errMsg } from './log.js';
import { MekoClient } from './meko.js';
import { getPaths } from './paths.js';
import { buildServer, findWebDir } from './server.js';
import { fqdn, loadTls } from './tls.js';
import { Tmux, systemdUnitInstalled } from './tmux.js';
import { Updater } from './updater.js';

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
 * Under systemd the environment is bare. Take PATH from the user's login shell
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
  const log = consoleLogger;
  let core: Core | null = null;
  const meko = new MekoClient(() => resolveMeko(core?.config ?? cfg), v);
  core = new Core({ paths, config: cfg, db, version: v, meko, tmux: new Tmux(cfg.tmux.socket, paths.tmuxConf), log });

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
  if (!getOwner(db)) log.info(`first-time setup: ${url}/setup?token=${issueSetupToken(db)}`);

  updater.start();
  setTimeout(() => updater.markHealthy(), 30000).unref();
  process.on('SIGTERM', () => void shutdown(0));
  process.on('SIGINT', () => void shutdown(0));
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

async function selftest(): Promise<void> {
  const { DatabaseSync } = await import('node:sqlite');
  new DatabaseSync(':memory:').prepare('SELECT 1').get();
  const pty = await import('node-pty');
  if (typeof pty.spawn !== 'function') throw new Error('node-pty failed to load');
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
  for (const unit of ['agent-k-local.service', 'agent-k-local-tmux.service']) {
    if (!systemdUnitInstalled(unit)) {
      console.log(`${unit.padEnd(28)} not installed`);
      continue;
    }
    let state: string;
    try {
      state = execFileSync('systemctl', ['--user', 'is-active', unit], { encoding: 'utf8' }).trim();
    } catch (e) {
      state = String((e as { stdout?: string }).stdout ?? 'inactive').trim() || 'inactive';
    }
    console.log(`${unit.padEnd(28)} ${state}`);
  }
  const panes = await new Tmux(cfg.tmux.socket, paths.tmuxConf).panes();
  console.log(`tmux sessions: ${panes.size} (${[...panes.values()].filter((p) => !p.dead).length} running)`);
  try {
    const r = await fetch(`${url}/healthz`, { signal: AbortSignal.timeout(3000) });
    console.log(`server:   ${r.ok ? 'responding' : `HTTP ${r.status}`}`);
  } catch {
    console.log('server:   not responding (a self-signed certificate also prevents this check)');
  }
}

function help(): void {
  console.log(`AgentK Local ${version()}

Usage: agent-k-local <command>

  serve         Run the server (systemd runs this for you)
  install       Configure this host, install the systemd user units, print the setup link
  uninstall     Stop and remove the systemd user units (--purge also deletes ~/.agent-k-local)
  setup-link    Print a one-time link to create (or reset) your login
  status        Show services, URL, certificate fingerprint and sessions
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
