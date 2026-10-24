// Moving AgentK's home into Meko's data folder (#236): from ~/.agent-k-local, or a home an older
// install named with AK_HOME, to <Meko's data folder>/extensions/agentk (paths.ts `mekoHome`).
//
// It happens by itself once a release with it has proved healthy (`HomeMove`, run by the server),
// in a one-shot job outside the services (`agentk _move-home`, service.ts `startMoveJob`): it ends
// every session, moves the folder (a rename, or a copy onto another disk), rewrites every path
// stored outside it that named it, and starts the services on the new home, whose recovery resumes
// the sessions. Without services, `serve` does it at its next start. A journal (move.json, carried
// with the folder) records the steps done, so a move that stops halfway goes on where it stopped.
import { execFileSync, spawnSync } from 'node:child_process';
import {
  cpSync, existsSync, lstatSync, mkdirSync, readdirSync, readFileSync, realpathSync, renameSync, rmdirSync, rmSync, statfsSync, statSync,
  symlinkSync, unlinkSync, writeFileSync,
} from 'node:fs';
import { homedir, hostname } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { claudeUserDir } from './claude.js';
import { repointLinks } from './cmdlink.js';
import { loadConfig, saveConfig, type Config } from './config.js';
import { all, openDb, run, tx } from './db.js';
import { ensurePrivateDir } from './fstype.js';
import { waitForServer } from './health.js';
import { NoHostError, SessionHost } from './hostclient.js';
import { heldLease } from './instance.js';
import { moveKiroSessions } from './kiro.js';
import { errMsg } from './log.js';
import { claudePath, expandHome, getPaths, homeIn, MekoHomeError, mekoHome, releaseHome, SHARED_TMP, withLocalBin, type MekoHomeOptions, type Paths } from './paths.js';
import { dropOldNames, hostServices, removeMoveJob, renderServiceFiles, samePath, servicesFor, startMoveJob, type ServiceManager } from './service.js';
import { copyClaudeProjects } from './trust.js';

/** The move's journal, in the home being moved (so it moves with it). */
export const JOURNAL = 'move.json';

const STEPS = ['db', 'paths', 'claude', 'kiro', 'git', 'links', 'config'] as const;
type Step = (typeof STEPS)[number];

export interface MovePlan {
  /** The home as the install names it (the old default, or the AK_HOME it was installed with). */
  from: string;
  to: string;
  /** rename on the same filesystem; copy onto another (from a network home to a local disk). */
  method: 'rename' | 'copy';
}

export interface Journal extends MovePlan {
  /** The folder the data was in when `from` is a symlink (or `from` itself). */
  real: string;
  /** moving: a server must not start; starting: the services are coming up on the new home. */
  phase: 'moving' | 'starting';
  done: Step[];
  /** Where a copied home's original went, removed once the server answers on the new one. */
  moved?: string;
}

export type MoveCheck = { plan: MovePlan } | { skip: string } | null;

export interface Log {
  info(msg: string): void;
  warn(msg: string, detail?: string): void;
}

export function readJournal(home: string): Journal | null {
  try {
    const j = JSON.parse(readFileSync(join(home, JOURNAL), 'utf8')) as Journal;
    return j && typeof j.to === 'string' && typeof j.from === 'string' ? j : null;
  } catch {
    return null;
  }
}

function writeJournal(dir: string, j: Journal): void {
  writeFileSync(join(dir, `${JOURNAL}.tmp`), JSON.stringify(j));
  renameSync(join(dir, `${JOURNAL}.tmp`), join(dir, JOURNAL));
}

function existingAncestor(p: string): string {
  for (let d = p; ; d = dirname(d)) {
    if (existsSync(d) || dirname(d) === d) return d;
  }
}

function isEmptyDir(p: string): boolean {
  try {
    return readdirSync(p).length === 0;
  } catch {
    return false;
  }
}

/** The bytes of the files under `dir`, links and special files not followed. */
export function dirSize(dir: string): number {
  let n = 0;
  const walk = (d: string) => {
    let entries;
    try {
      entries = readdirSync(d, { withFileTypes: true });
    } catch {
      return;
    }
    for (const e of entries) {
      const p = join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.isFile()) {
        try {
          n += lstatSync(p).size;
        } catch {
          // gone meanwhile
        }
      }
    }
  };
  walk(dir);
  return n;
}

function gitAtLeast230(version: string | null): boolean {
  const m = version && /(\d+)\.(\d+)/.exec(version);
  return !!m && (Number(m[1]) > 2 || (Number(m[1]) === 2 && Number(m[2]) >= 30));
}

export interface CheckDeps {
  gitVersion?: () => string | null;
  /** Free bytes on the filesystem that holds a path. */
  free?: (dir: string) => number;
  size?: (dir: string) => number;
  /** The device a path is on (`stat`'s dev). */
  device?: (p: string) => number;
  /** The release folder of the running version (`AK_APP_DIR`); none: a source checkout, which never moves. */
  appDir?: string | undefined;
  /** How Meko's data folder is found (tests). */
  meko?: MekoHomeOptions;
  /** What `meko destroy -h` prints, or null with no Meko installed (`mekoDestroyHelp`). */
  mekoHelp?: () => string | null;
}

/** The Meko data folder a home is in (`<data folder>/extensions/agentk`), or null for any other home. */
export function mekoDirOf(home: string): string | null {
  return basename(home) === 'agentk' && basename(dirname(home)) === 'extensions' ? dirname(dirname(home)) : null;
}

/**
 * What the Meko installed here prints for `meko destroy -h` (its flags, and nothing done), or null
 * when there's no `meko` on PATH or in ~/.local/bin, where Meko's installer links it.
 */
export function mekoDestroyHelp(env: NodeJS.ProcessEnv = process.env): string | null {
  const r = spawnSync('meko', ['destroy', '-h'], { env: { ...env, PATH: withLocalBin(env.PATH ?? '', env.HOME || homedir()) }, encoding: 'utf8', timeout: 5000 });
  if (r.error) return null;
  return `${r.stdout ?? ''}${r.stderr ?? ''}`;
}

/**
 * Whether this home moves, and how: null when there's nothing to do (it's in Meko's data folder
 * already, or this isn't a release install), a reason when it can't move now, else the plan.
 */
export function checkMove(paths: Paths, cfg: Config, d: CheckDeps = {}): MoveCheck {
  const appDir = 'appDir' in d ? d.appDir : process.env.AK_APP_DIR;
  // A home in a Meko data folder already stays: the installer put it where Meko's rule said in
  // its shell, which a service may not see the same way (MEKO_HOME, XDG_STATE_HOME).
  if (!releaseHome(appDir) || mekoDirOf(paths.home)) return null;
  let to: string;
  try {
    to = homeIn(mekoHome({ configured: cfg.meko.home, ...d.meko }));
  } catch (e) {
    if (e instanceof MekoHomeError) return { skip: e.message };
    throw e;
  }
  const from = paths.home;
  const real = realpathSync(from);
  if (from === to || real === to || (existsSync(to) && realpathSync(to) === real)) return null;
  if (existsSync(to) && !isEmptyDir(to)) return { skip: `${to} isn't empty: move what's in it away, and AgentK moves its home there at its next start` };
  const git = (d.gitVersion ?? (() => {
    try {
      return execFileSync('git', ['--version'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
    } catch {
      return null;
    }
  }))();
  if (!gitAtLeast230(git)) return { skip: `moving the home needs git 2.30 or later (for git worktree repair), and this is ${git?.trim() || 'no git'}` };
  // A Meko from before `destroy` kept extension data would delete the moved home with its own.
  const help = (d.mekoHelp ?? mekoDestroyHelp)();
  if (help !== null && !help.includes('with-extensions')) {
    return { skip: "the Meko installed here is older than one whose `meko destroy` keeps AgentK's data: update it (meko update), and AgentK moves at its next start" };
  }
  const device = d.device ?? ((p: string) => statSync(p).dev);
  const method = device(real) === device(existingAncestor(to)) ? 'rename' : 'copy';
  if (method === 'copy') {
    const need = Math.ceil(((d.size ?? dirSize)(real) * 11) / 10);
    const free = (d.free ?? ((p) => {
      const s = statfsSync(p);
      return s.bavail * s.bsize;
    }))(existingAncestor(to));
    if (free < need) return { skip: `moving the home to ${to} needs ${Math.ceil(need / 2 ** 20)} MB there, and ${Math.floor(free / 2 ** 20)} MB is free` };
  }
  return { plan: { from, to, method } };
}

/**
 * The commands Meko runs for an extension that `refuseOtherMeko` checks: start, stop, status and
 * `destroy`'s uninstall. Not `install`: the installer updates the existing home in place whatever
 * MEKO_HOME says, and has switched `app/current` before it runs `agentk install`, which then keeps
 * the Meko folder the home is in over MEKO_HOME (`runInstall`).
 */
export const MEKO_COMMANDS = ['uninstall', 'start', 'stop', 'status'];

/**
 * Refuses a command Meko runs for another data folder than the one this home is in. A release's
 * home is the folder it's installed in, whatever MEKO_HOME says, so without this a Meko that found
 * this AgentK for another folder (its tests, a second Meko) would stop or uninstall this one and
 * repoint its `[meko] home`. A home that isn't in a Meko data folder yet takes Meko's from MEKO_HOME.
 */
export function refuseOtherMeko(paths: Paths, env: NodeJS.ProcessEnv = process.env): void {
  const own = mekoDirOf(paths.home);
  if (!env.MEKO_HOME || !own || samePath(resolve(env.MEKO_HOME), own)) return;
  throw new Error(`This AgentK belongs to the Meko in ${own}, and MEKO_HOME names ${resolve(env.MEKO_HOME)}: nothing was done. Run it with MEKO_HOME=${own}, or without MEKO_HOME.`);
}

/**
 * Keeps the data folder Meko names in MEKO_HOME, which it sets on every AgentK command it runs, as
 * `[meko] home`: AgentK's own services don't have it, and the move goes to that folder. Only for an
 * install that has a config; Meko's default needs nothing.
 */
export function rememberMekoHome(paths: Paths, env: NodeJS.ProcessEnv = process.env): void {
  if (!env.MEKO_HOME || !existsSync(paths.config)) return;
  const cfg = loadConfig(paths, () => undefined);
  const dir = resolve(env.MEKO_HOME);
  if (cfg.meko.home === dir) return;
  cfg.meko.home = dir;
  saveConfig(paths, cfg);
}

/** The home's warning when it's in /var/tmp, which many Linux systems clean by age. */
export function tmpHomeWarning(home: string): string | null {
  if (!home.startsWith(SHARED_TMP + '/')) return null;
  return `AgentK's home ${home} is in ${SHARED_TMP}, which many Linux systems clean by age (often after 30 days), and that can delete files in a worktree nothing has touched since. Set XDG_STATE_HOME to a folder on a local disk, and AgentK and Meko use that instead.`;
}

/** Makes the folders the home goes in: Meko's data folder private in a shared place, as Meko does. */
export function makeHomeParent(home: string): void {
  const meko = dirname(dirname(home));
  if (meko.startsWith(SHARED_TMP + '/')) ensurePrivateDir(meko, undefined, undefined, 'Remove it, or set MEKO_HOME to a local folder only you can write to.');
  mkdirSync(dirname(home), { recursive: true, mode: 0o700 });
}

// ---------------------------------------------------------------------------
// Stopping everything

/** Whether a pid is an AgentK server: after a crash its lease can name a pid another process has now. */
export function isServer(pid: number): boolean {
  try {
    return /(agentk|agent-k-local|cli\.[jt]s) serve\b/.test(execFileSync('ps', ['-p', String(pid), '-o', 'args='], { encoding: 'utf8' }));
  } catch {
    return false;
  }
}

/**
 * Stops the server, then the session host, ending every session: a running server would start the
 * host again and resume them. Services stop through their supervisor, which would restart a killed
 * process. Returns whether anything was running and how many sessions there were.
 */
export async function stopAll(paths: Paths, svc: ServiceManager | null = servicesFor(paths.home)): Promise<{ stopped: boolean; sessions: number }> {
  const client = new SessionHost(paths, { services: () => null });
  const sessions = (await client.panes().catch(() => new Map())).size;
  let stopped = false;
  if (svc?.installed('server')) {
    stopped = svc.state('server').running;
    svc.stop('server');
  }
  // A server outside the service (`serve` by hand, or on another host sharing this home) holds the lease too.
  const server = heldLease(paths.lock);
  if (server) {
    if (server.host !== hostname()) throw new Error(`The AgentK server for this home runs on ${server.host} (pid ${server.pid}): stop it there.`);
    if (isServer(server.pid)) {
      // Its SIGTERM handler shuts down cleanly and releases the lease.
      process.kill(server.pid, 'SIGTERM');
      for (let i = 0; i < 200 && heldLease(paths.lock); i++) await sleep(100);
      if (heldLease(paths.lock)) throw new Error(`The server (pid ${server.pid}) didn't stop within 20 seconds. See: agentk logs`);
      stopped = true;
    }
  }
  if (svc?.installed('host')) {
    stopped ||= svc.state('host').running;
    svc.stop('host');
  }
  // A host the service didn't start (a detached one from `serve` before install) still answers on the socket.
  try {
    await client.stop();
    stopped = true;
  } catch (e) {
    if (!(e instanceof NoHostError)) throw e;
  }
  return { stopped, sessions };
}

// ---------------------------------------------------------------------------
// The move

/** Maps a path under any spelling of the old home to the same place under the new one, else null. */
export function homeMapper(j: Pick<Journal, 'from' | 'real' | 'to'>): (p: string) => string | null {
  const olds = [...new Set([j.from, j.real, claudePath(j.from), claudePath(j.real)])];
  return (p) => {
    for (const o of olds) if (p === o || p.startsWith(o + '/')) return j.to + p.slice(o.length);
    return null;
  };
}

/** Claude Code's folder for a cwd's transcripts and memory (as `transcriptPathFor`). */
const projectKey = (cwd: string) => cwd.replace(/[^A-Za-z0-9]/g, '-');

/**
 * Moves `from`'s entries into `to`, folders merged the same way; an entry `to` already has is left
 * in `from`, which goes only once it's empty.
 */
function mergeDir(from: string, to: string): void {
  if (!existsSync(to)) {
    renameSync(from, to);
    return;
  }
  for (const name of readdirSync(from)) {
    const [f, t] = [join(from, name), join(to, name)];
    if (!existsSync(t)) renameSync(f, t);
    else if (isRealDir(f) && isRealDir(t)) mergeDir(f, t);
  }
  try {
    rmdirSync(from);
  } catch {
    // something in it was in `to` already: kept
  }
}

/** Renames `from` to `to`, or copies it and removes the original across filesystems. */
function moveFile(from: string, to: string): void {
  try {
    renameSync(from, to);
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code !== 'EXDEV') throw e;
    cpSync(from, to, { preserveTimestamps: true });
    unlinkSync(from);
  }
}

export interface MoveDeps {
  log: Log;
  /** Ends the server and every session (`stopAll`). */
  stop: () => Promise<void>;
  /** The services to move to the new home, or null without them. */
  services: ServiceManager | null;
  /** Waits for the server on the new home (with services). */
  waitHealthy?: (paths: Paths) => Promise<boolean>;
  /** Runs `git -C <dir> worktree repair`. */
  repair?: (dir: string) => void;
  claudeDir?: string;
  kiroHome?: string;
  home?: string;
}

/** The steps' own work, each one safe to run again. */
function runStep(step: Step, j: Journal, d: MoveDeps): void {
  const paths = getPaths(j.to);
  const map = homeMapper(j);
  switch (step) {
    case 'db': {
      // A database kept apart (an install whose home was on a network filesystem) joins the home:
      // it's on a local disk now, and AgentK's data has one place.
      const cfg = loadConfig(paths, (m) => d.log.warn(m));
      if (!cfg.state.dbPath) return;
      const db = map(expandHome(cfg.state.dbPath)) ?? expandHome(cfg.state.dbPath);
      if (existsSync(db)) {
        // Everything in the main file first, so the side files needn't travel.
        const h = openDb(db);
        h.exec('PRAGMA wal_checkpoint(TRUNCATE)');
        h.close();
        const dir = dirname(db);
        const name = basename(db);
        for (const f of readdirSync(dir)) {
          if (f === name || f.startsWith(`${name}-`) || f.startsWith('state.db.bak-')) {
            moveFile(join(dir, f), join(j.to, f === name ? 'state.db' : f.replace(name, 'state.db')));
          }
        }
        try {
          rmdirSync(dir);
        } catch {
          // other files live there
        }
      }
      cfg.state.dbPath = '';
      saveConfig(paths, cfg);
      d.log.info(`moved the database into ${j.to}`);
      return;
    }
    case 'paths':
      rewriteDb(paths, j, d);
      return;
    case 'claude': {
      const n = copyClaudeProjects(map);
      if (n) d.log.info(`copied ${n} folder entries in Claude Code's config to their new paths`);
      return;
    }
    case 'kiro': {
      const n = moveKiroSessions(map, d.kiroHome);
      if (n) d.log.info(`pointed ${n} Kiro sessions at their moved folders`);
      return;
    }
    case 'git': {
      const repair = d.repair ?? ((dir: string) => void execFileSync('git', ['-C', dir, 'worktree', 'repair'], { stdio: 'ignore', timeout: 60_000 }));
      const root = join(j.to, 'worktrees');
      const dirs = (p: string) => {
        try {
          return readdirSync(p, { withFileTypes: true }).filter((e) => e.isDirectory()).map((e) => join(p, e.name));
        } catch {
          return [];
        }
      };
      for (const dir of dirs(root).flatMap(dirs)) {
        try {
          repair(dir);
        } catch (e) {
          // One worktree git can't fix doesn't stop the move: its session says so when it starts.
          d.log.warn(`git worktree repair failed in ${dir}`, errMsg(e));
        }
      }
      return;
    }
    case 'links': {
      const moved = repointLinks(map, d.home);
      if (moved.length) d.log.info(`pointed ${moved.join(' and ')} at the new home`);
      return;
    }
    case 'config': {
      const cfg = loadConfig(paths, (m) => d.log.warn(m));
      const cert = map(expandHome(cfg.tls.cert));
      const key = map(expandHome(cfg.tls.key));
      if (!cert && !key) return;
      if (cfg.tls.cert && cert) cfg.tls.cert = cert;
      if (cfg.tls.key && key) cfg.tls.key = key;
      saveConfig(paths, cfg);
    }
  }
}

/**
 * The database's paths into the old home, and the Claude Code transcript folders of the worktrees
 * in it (named after each one's path; `--resume` and usage reading look there): moved together,
 * since the folders' new names come from the worktrees' old and new paths. Each session that will
 * resume gets a line in its activity.
 */
function rewriteDb(paths: Paths, j: Journal, d: MoveDeps): void {
  const map = homeMapper(j);
  const db = openDb(paths.defaultDb);
  try {
    const projects = join(d.claudeDir ?? claudeUserDir(), 'projects');
    // Worktree folders before and after, from what the database names (moved or not yet).
    const back = (p: string) => (p.startsWith(j.to + '/') ? j.real + p.slice(j.to.length) : p);
    const trees = new Map<string, string>();
    for (const r of all<{ p: string }>(db, 'SELECT worktree_path AS p FROM sessions UNION SELECT path AS p FROM worktrees')) {
      const old = back(r.p);
      const now = map(old);
      if (now) trees.set(old, now);
    }
    // Claude keyed each folder on the cwd it started in: any spelling of the old home, as stored
    // or in its local spelling (claudePath), each now the new path's.
    const folders: [string, string][] = [];
    for (const now of trees.values()) {
      const rel = now.slice(j.to.length);
      for (const base of new Set([j.from, j.real])) {
        for (const [o, n] of [[base + rel, now], [claudePath(base + rel), claudePath(now)]]) {
          const pair: [string, string] = [join(projects, projectKey(o)), join(projects, projectKey(n))];
          if (pair[0] !== pair[1] && !folders.some((f) => f[0] === pair[0])) folders.push(pair);
        }
      }
    }
    for (const [o, n] of folders) if (existsSync(o)) mergeDir(o, n);
    const moveAny = (p: string) => {
      const m = map(p);
      if (m) return m;
      for (const [o, n] of folders) if (p.startsWith(o + '/')) return n + p.slice(o.length);
      return null;
    };
    tx(db, () => {
      for (const [table, col] of [
        ['sessions', 'worktree_path'], ['sessions', 'transcript_path'], ['sessions', 'repo_path'], ['projects', 'repo_path'],
        ['worktrees', 'path'], ['worktrees', 'repo'], ['usage_files', 'path'],
      ] as const) {
        for (const r of all<{ id: number; v: string }>(db, `SELECT rowid AS id, ${col} AS v FROM ${table} WHERE ${col} IS NOT NULL`)) {
          const to = moveAny(r.v);
          if (to) run(db, `UPDATE ${table} SET ${col} = ? WHERE rowid = ?`, to, r.id);
        }
      }
      for (const r of all<{ id: string; v: string }>(db, 'SELECT id, start_images AS v FROM sessions WHERE start_images IS NOT NULL')) {
        let list: unknown;
        try {
          list = JSON.parse(r.v);
        } catch {
          continue;
        }
        if (!Array.isArray(list)) continue;
        const moved = list.map((p) => (typeof p === 'string' ? moveAny(p) ?? p : p));
        if (JSON.stringify(moved) !== r.v) run(db, 'UPDATE sessions SET start_images = ? WHERE id = ?', JSON.stringify(moved), r.id);
      }
      const now = Date.now();
      // Once per session, should this step run again.
      for (const r of all<{ id: string; project_id: string }>(db, "SELECT id, project_id FROM sessions WHERE desired = 'running' AND NOT EXISTS (SELECT 1 FROM session_events e WHERE e.session_id = sessions.id AND e.kind = 'home-moved')")) {
        run(db, 'INSERT INTO session_events(session_id, project_id, kind, text, payload_json, at) VALUES (?, ?, ?, ?, NULL, ?)',
          r.id, r.project_id, 'home-moved', `AgentK moved its home to ${j.to}; the session resumed there`, now);
      }
    });
    d.log.info(`rewrote the database's paths for ${j.to} (${trees.size} worktrees)`);
  } finally {
    db.close();
  }
}

/**
 * Puts the home in its new place: renamed, or copied (to `<new>.partial`, then renamed) with the
 * original moved aside. The journal is written into the home first, so it goes along. Safe to run
 * again at any point: what's done already is skipped.
 */
function place(j: Journal, d: MoveDeps): void {
  if (!existsSync(join(j.to, JOURNAL))) {
    makeHomeParent(j.to);
    if (existsSync(j.to) && isEmptyDir(j.to)) rmdirSync(j.to);
    writeJournal(j.real, j);
    if (j.method === 'rename') {
      renameSync(j.real, j.to);
    } else {
      // A copy cut off before is started again: the original is untouched until the copy is whole.
      const partial = `${j.to}.partial`;
      rmSync(partial, { recursive: true, force: true });
      cpSync(j.real, partial, {
        recursive: true,
        verbatimSymlinks: true,
        preserveTimestamps: true,
        // Sockets and pipes (the hook and host sockets) can't be copied, and the new server makes its own.
        filter: (src) => {
          try {
            const st = lstatSync(src);
            return !st.isSocket() && !st.isFIFO();
          } catch {
            return false;
          }
        },
      });
      renameSync(partial, j.to);
    }
  }
  if (j.method === 'copy') {
    j.moved = `${j.real}.moved`;
    writeJournal(j.to, j);
    if (isRealDir(j.real)) {
      rmSync(j.moved, { recursive: true, force: true });
      renameSync(j.real, j.moved);
    }
  }
  // The old path keeps working (an interrupted move, a rollback, scripts): a link to the new home.
  try {
    if (lstatSync(j.from).isSymbolicLink()) unlinkSync(j.from);
  } catch {
    // not there
  }
  if (!existsSync(j.from)) symlinkSync(j.to, j.from);
  d.log.info(`${j.method === 'rename' ? 'moved' : 'copied'} ${j.real} to ${j.to}; ${j.from} links to it`);
}

function isRealDir(p: string): boolean {
  try {
    return lstatSync(p).isDirectory();
  } catch {
    return false;
  }
}

/**
 * Moves the home (or goes on with a move a journal names), then starts the services on the new
 * home and, once the server answers there, removes what's left of the old one. Returns the new home.
 */
export async function moveHome(plan: MovePlan, d: MoveDeps): Promise<string> {
  let j = readJournal(plan.to) ?? readJournal(plan.from);
  if (!j || j.to !== plan.to) j = { ...plan, real: realpathSync(plan.from), phase: 'moving', done: [] };
  // Before the folder is in place, and again when a move cut off then goes on: nothing may run on it.
  if (!existsSync(join(j.to, JOURNAL)) || j.phase === 'moving') await d.stop();
  place(j, d);
  const warning = tmpHomeWarning(j.to);
  if (warning) d.log.warn(warning);
  for (const step of STEPS) {
    if (j.done.includes(step)) continue;
    runStep(step, j, d);
    j.done.push(step);
    writeJournal(j.to, j);
  }
  // Releases run from the new home from here on: the services' units, and this process's own
  // templates (renderServiceFiles reads them from AK_APP_DIR).
  const appDir = process.env.AK_APP_DIR && homeMapper(j)(process.env.AK_APP_DIR);
  if (appDir) process.env.AK_APP_DIR = appDir;
  j.phase = 'starting';
  writeJournal(j.to, j);
  const svc = d.services;
  if (svc && (svc.installed('server') || svc.installed('host'))) {
    for (const [old, now] of dropOldNames(svc)) d.log.info(`${now} replaces ${old}, its name before the rename to AgentK`);
    svc.install(renderServiceFiles(svc, { AK_HOME: j.to, HOST: hostname() }));
    d.log.info(`the services run AgentK from ${j.to}`);
    if (d.waitHealthy && !(await d.waitHealthy(getPaths(j.to)))) {
      d.log.warn(`the server on ${j.to} isn't answering yet; ${j.moved ? `${j.moved} is kept until it does` : 'it finishes the move when it starts'}`);
      return j.to;
    }
  }
  finishMove(j.to, d.log);
  return j.to;
}

/** A move's last step, once the server runs on the new home: the old copy and the journal go. */
export function finishMove(home: string, log: Log): void {
  const j = readJournal(home);
  if (!j || j.phase !== 'starting') return;
  if (j.moved) {
    rmSync(j.moved, { recursive: true, force: true });
    log.info(`removed ${j.moved}, the home's copy from before the move`);
  }
  rmSync(join(home, JOURNAL), { force: true });
  log.info(`AgentK's home moved from ${j.from} to ${j.to}`);
}

// ---------------------------------------------------------------------------
// When it happens

/**
 * Starts the move once this release has proved healthy (no update-pending.json), whatever the
 * sessions are doing: at the server's start, and again when the launcher's pending file goes. A move
 * that can't happen says why once per start, and the install keeps running where it is.
 */
export class HomeMove {
  private started = false;
  private said = new Set<string>();

  constructor(
    private paths: Paths,
    private log: Log,
    private o: { services?: () => ServiceManager | null; check?: (paths: Paths, cfg: Config) => MoveCheck; start?: typeof startMoveJob } = {},
  ) {}

  check(): void {
    if (this.started || existsSync(this.paths.updatePending)) return;
    const svc = (this.o.services ?? (() => servicesFor(this.paths.home)))();
    if (!svc?.installed('server')) return; // `serve` moves a home without services at its start
    let r: MoveCheck;
    try {
      r = (this.o.check ?? checkMove)(this.paths, loadConfig(this.paths, () => undefined));
    } catch (e) {
      r = { skip: errMsg(e) };
    }
    if (!r) return;
    if ('skip' in r) {
      if (!this.said.has(r.skip)) this.log.warn(`AgentK's home stays in ${this.paths.home} for now: ${r.skip}`);
      this.said.add(r.skip);
      return;
    }
    this.started = true;
    const bin = join(process.env.AK_APP_DIR ?? join(this.paths.app, 'current'), 'bin', 'agentk');
    // The job logs beside the new home (service.ts moveJobLog), so its folder is made first.
    this.log.info(`moving AgentK's home to ${r.plan.to} (${r.plan.method}): every session ends and resumes there`);
    try {
      if (!this.o.start) makeHomeParent(r.plan.to);
      (this.o.start ?? startMoveJob)(svc.kind, { bin, from: this.paths.home, to: r.plan.to });
    } catch (e) {
      this.started = false;
      this.log.warn("couldn't start moving AgentK's home", errMsg(e));
    }
  }
}

// ---------------------------------------------------------------------------
// The job, and `serve` without services

/** The move job's own run (`agentk _move-home --to <dir>`). */
export async function runMoveJob(args: string[]): Promise<void> {
  const i = args.indexOf('--to');
  const to = i >= 0 ? args[i + 1] : undefined;
  if (!to) throw new Error('usage: agentk _move-home --to <dir> (run by AgentK itself)');
  const from = process.env.AK_HOME;
  if (!from) throw new Error('AK_HOME names the home to move');
  const log: Log = {
    info: (m) => console.log(`${new Date().toISOString()} ${m}`),
    warn: (m, detail) => console.error(`${new Date().toISOString()} WARN ${m}${detail ? `: ${detail}` : ''}`),
  };
  const paths = getPaths(from);
  const svc = hostServices();
  let plan: MovePlan | null = readJournal(to) ?? readJournal(from);
  if (!plan) {
    const r = checkMove(paths, loadConfig(paths, (m) => log.warn(m)));
    if (!r || !('plan' in r) || r.plan.to !== to) {
      log.warn(`not moving AgentK's home to ${to}: ${r && 'skip' in r ? r.skip : 'it needs no move'}`);
      if (svc) removeMoveJob(svc.kind);
      return;
    }
    plan = r.plan;
  }
  const home = await moveHome(plan, {
    log,
    stop: async () => void (await stopAll(paths, svc)),
    services: svc,
    waitHealthy: async (p) => (await waitForServer(loadConfig(p, () => undefined), 60_000)).ok,
  }).catch((e: unknown) => {
    log.warn("moving AgentK's home failed; it's tried again", errMsg(e));
    process.exit(1);
  });
  log.info(`done: AgentK's home is ${home}`);
  if (svc) removeMoveJob(svc.kind);
}
