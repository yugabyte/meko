#!/usr/bin/env node
// The meko_presenter command: the server, and the commands Meko runs for an extension (start, stop,
// status, install, uninstall, and _handoff, which names the socket Meko serves at /ext/presenter/).

import { execFileSync, spawn } from 'node:child_process';
import { chmodSync, closeSync, existsSync, lstatSync, mkdirSync, openSync, readFileSync, realpathSync, renameSync, rmSync, unlinkSync, writeFileSync } from 'node:fs';
import { createConnection } from 'node:net';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { runClaude } from './claude.js';
import { DeckStore } from './decks.js';
import { Jobs } from './jobs.js';
import { findMeko, isLoopback, loadConfig, MekoClient, type PresenterConfig } from './meko.js';
import { getPaths, MEKO_COMMANDS, refuseOtherMeko, withLocalBin, type Paths } from './paths.js';
import { HEALTHY_MS, markHealthy, uncountStart } from './probation.js';
import { Runs } from './runs.js';
import { broadcast, buildServer, findWebDir, type Core } from './server.js';

const here = dirname(fileURLToPath(import.meta.url));

export function version(): string {
  try {
    return (JSON.parse(readFileSync(join(here, '..', '..', 'package.json'), 'utf8')) as { version: string }).version;
  } catch {
    return '0.0.0';
  }
}

/** Whether a command is on PATH (or in ~/.local/bin). */
function onPath(cmd: string): boolean {
  try {
    execFileSync('sh', ['-c', 'command -v "$1"', 'sh', cmd], { stdio: 'ignore', env: { ...process.env, PATH: withLocalBin(process.env.PATH ?? '') } });
    return true;
  } catch {
    return false;
  }
}

/**
 * Whether a pid is a Presenter server: `meko_presenter serve` (the release's launcher, before it
 * execs Node; its `presenter serve` end matches) or `cli.js serve` (Node, as start and the launcher
 * run it).
 */
function isServer(pid: number): boolean {
  try {
    return /(presenter|cli\.[jt]s) serve\b/.test(execFileSync('ps', ['-p', String(pid), '-o', 'args='], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }));
  } catch {
    return false;
  }
}

/**
 * The pid in the pid file, when that process is alive and is Presenter's server: a server that
 * crashed or was killed leaves the file, and its pid can belong to another process by now.
 */
function runningPid(paths: Paths): number | null {
  try {
    const pid = Number(readFileSync(paths.pid, 'utf8').trim());
    if (!Number.isInteger(pid) || pid <= 0) return null;
    process.kill(pid, 0);
    return isServer(pid) ? pid : null;
  } catch {
    return null;
  }
}

/** Whether something answers on the socket. */
function socketAnswers(sock: string): Promise<boolean> {
  return new Promise((resolve) => {
    if (!existsSync(sock)) return resolve(false);
    const c = createConnection(sock);
    c.once('connect', () => (c.end(), resolve(true)));
    c.once('error', () => resolve(false));
  });
}

async function serve(paths: Paths): Promise<void> {
  mkdirSync(paths.home, { recursive: true, mode: 0o700 });
  const sockDir = dirname(paths.webSock);
  mkdirSync(sockDir, { recursive: true, mode: 0o700 });
  // For a long home the socket's folder is under /tmp, where another user could have made it first.
  const st = lstatSync(sockDir);
  if (!st.isDirectory() || (typeof process.getuid === 'function' && st.uid !== process.getuid())) {
    throw new Error(`${sockDir} isn't a folder of this user's. Remove it, then start Presenter again.`);
  }
  chmodSync(sockDir, 0o700);
  const v = version();
  const meko = MekoClient.forPaths(paths, v);
  const events = new Set<import('ws').WebSocket>();
  const decks = new DeckStore(paths, meko, (state) => broadcast({ events }, { type: 'deck', state }));
  const claude = process.env.PRESENTER_CLAUDE ? JSON.parse(process.env.PRESENTER_CLAUDE) as string[] : ['claude'];
  const jobs = new Jobs({
    decks, meko, claude, run: runClaude,
    mekoCreds: async () => (await findMeko(paths)).creds,
    emit: (e) => broadcast({ events }, e),
  });
  const runs = new Runs(decks, join(paths.run, 'runs'));
  const core: Core = { version: v, meko, decks, jobs, runs, events, claudeFound: () => onPath(claude[0]!) };
  const app = await buildServer(core, { webDir: findWebDir(here) });
  if (await socketAnswers(paths.webSock)) throw new Error(`Another Presenter is serving ${paths.webSock}.`);
  if (existsSync(paths.webSock)) unlinkSync(paths.webSock);
  await app.listen({ path: paths.webSock });
  chmodSync(paths.webSock, 0o600);
  writeFileSync(paths.pid, String(process.pid), { mode: 0o600 });
  console.log(`Presenter ${v} is serving ${paths.webSock}`);
  const stop = async () => {
    // Claude's runs end with the server (their results would be lost), and a page that doesn't
    // answer (a sleeping laptop) would hold close for ws's 30 s.
    jobs.cancelAll();
    for (const ws of app.websocketServer.clients) ws.terminate();
    await app.close().catch(() => undefined);
    await meko.close();
    try {
      if (readFileSync(paths.pid, 'utf8').trim() === String(process.pid)) unlinkSync(paths.pid);
    } catch {
      /* already gone */
    }
    process.exit(0);
  };
  // A release on probation (an update Meko just switched to) has proved itself after HEALTHY_MS; a
  // deliberate stop of it isn't a failed start.
  setTimeout(() => markHealthy(), HEALTHY_MS).unref();
  process.once('SIGTERM', () => {
    uncountStart();
    void stop();
  });
  process.once('SIGINT', () => void stop());
}

async function start(paths: Paths): Promise<void> {
  if (runningPid(paths)) return void console.log('Presenter is running');
  mkdirSync(paths.logs, { recursive: true, mode: 0o700 });
  mkdirSync(paths.run, { recursive: true, mode: 0o700 });
  const log = openSync(join(paths.logs, 'server.log'), 'a', 0o600);
  // A release starts it through its launcher (packaging/meko_presenter), which counts the starts of a
  // version on probation and rolls back one that keeps failing. Otherwise Node's own flags too (a
  // loader under `tsx`), so the server runs the way this command did.
  const launcher = process.env.PRESENTER_LAUNCHER;
  const [cmd, args] = launcher ? [launcher, ['serve']] : [process.execPath, [...process.execArgv, fileURLToPath(import.meta.url), 'serve']];
  const child = spawn(cmd, args, { detached: true, stdio: ['ignore', log, log], env: process.env });
  child.unref();
  closeSync(log);
  for (let i = 0; i < 100; i++) {
    if (await socketAnswers(paths.webSock)) return void console.log('Presenter started');
    if (child.exitCode !== null) break;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`Presenter didn't start: see ${join(paths.logs, 'server.log')}`);
}

async function stop(paths: Paths): Promise<void> {
  const pid = runningPid(paths);
  if (!pid) return void console.log('Presenter isn\'t running');
  process.kill(pid, 'SIGTERM');
  for (let i = 0; i < 100; i++) {
    if (!runningPid(paths)) return void console.log('Presenter stopped');
    await new Promise((r) => setTimeout(r, 100));
  }
  process.kill(pid, 'SIGKILL');
  // Its own stop never ran, so the pid file is left for this to remove.
  rmSync(paths.pid, { force: true });
  console.log('Presenter stopped');
}

async function status(paths: Paths): Promise<void> {
  const pid = runningPid(paths);
  const up = await socketAnswers(paths.webSock);
  console.log(`Presenter ${version()}`);
  console.log(`Home: ${paths.home}`);
  console.log(`Server: ${pid && up ? `running (pid ${pid}), serving ${paths.webSock}` : 'not running'}`);
  console.log('Pages: /ext/presenter/ on Meko\'s web listener (meko ui /ext/presenter/)');
  console.log(`Claude Code: ${onPath('claude') ? 'installed' : 'not found: install it to build decks'}`);
  if (!pid || !up) process.exitCode = 3;
}

/**
 * `install --meko-url <url> --meko-key <key> [--non-interactive]`: what `meko install_extension
 * presenter` runs. config.json keeps a Meko Cloud URL and key. The local Meko's, which `meko
 * install_extension` passes, aren't kept: Presenter asks `meko token --json` for them each time.
 */
function install(paths: Paths, args: string[]): void {
  const at = (flag: string) => {
    const i = args.indexOf(flag);
    return i >= 0 ? args[i + 1] : undefined;
  };
  const url = at('--meko-url');
  const key = at('--meko-key');
  mkdirSync(paths.home, { recursive: true, mode: 0o700 });
  const cfg: PresenterConfig = loadConfig(paths);
  if (url && key) {
    if (!/^https?:\/\//.test(url)) throw new Error('--meko-url must be an http or https URL');
    if (isLoopback(url)) delete cfg.meko;
    else cfg.meko = { url, key };
  }
  // A new 0600 file renamed over the old one: a config.json made by hand doesn't keep a looser mode.
  const tmp = `${paths.config}.tmp`;
  rmSync(tmp, { force: true });
  writeFileSync(tmp, JSON.stringify(cfg, null, 2) + '\n', { mode: 0o600 });
  renameSync(tmp, paths.config);
  console.log(`Presenter is set up in ${paths.home}${url ? `, using the Meko at ${url}` : ''}`);
  if (!onPath('claude')) console.log('Install Claude Code (claude) to build decks; playing and editing them work without it');
}

/** `uninstall [--purge] [--non-interactive]`: stops the server; --purge removes the home and its drafts. */
async function uninstall(paths: Paths, args: string[]): Promise<void> {
  await stop(paths);
  if (args.includes('--purge')) {
    rmSync(paths.home, { recursive: true, force: true });
    console.log(`Removed ${paths.home}. Saved decks stay in their Meko datapacks`);
  }
}

const HELP = `meko_presenter: Presenter, interactive, branching presentations from Meko datapacks

  meko_presenter serve        run the server in the foreground
  meko_presenter start        start the server in the background
  meko_presenter stop         stop it
  meko_presenter status       whether it runs, and where
  meko_presenter install      --meko-url <url> --meko-key <key> [--non-interactive]
  meko_presenter uninstall    [--purge] [--non-interactive]
  meko_presenter version      [--json]

Meko serves the pages at /ext/presenter/ on its web listener.`;

export async function main(argv: string[]): Promise<void> {
  const [cmd, ...args] = argv;
  const paths = getPaths();
  if (cmd && MEKO_COMMANDS.includes(cmd)) refuseOtherMeko(paths);
  switch (cmd) {
    case 'serve': return serve(paths);
    case 'start': return start(paths);
    case 'stop': return stop(paths);
    case 'status': return status(paths);
    case 'install': return install(paths, args);
    case 'uninstall': return uninstall(paths, args);
    // Where the server listens, for Meko's web listener; not for people to run.
    case '_handoff':
      console.log(JSON.stringify({ socket: paths.webSock, legacy: null }));
      return;
    case 'version':
    case '--version':
      // --json is what Meko checks a release it unpacks with, as its own: {"version": "<version>"}.
      console.log(args.includes('--json') ? JSON.stringify({ version: version() }) : version());
      return;
    case undefined:
    case 'help':
    case '--help':
    case '-h':
      console.log(HELP);
      return;
    default:
      throw new Error(`Unknown command ${cmd}. Run meko_presenter help.`);
  }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === realpathSync(process.argv[1])) {
  main(process.argv.slice(2)).catch((e: Error) => {
    console.error(`meko_presenter: ${e.message}`);
    process.exit(1);
  });
}
