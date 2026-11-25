// Finds or makes the work queue's database in Meko Cloud (docs/design/work-queue.md#where-its-kept): an AMP
// cluster in the datapack WORK_QUEUE_DATAPACK, reached over Postgres, whose connection record (with the
// password) is a file in that datapack, so each of the user's computers can find it. Every Meko call goes
// through the Meko on this computer, which passes them on to Meko Cloud (`WorkMeko`, over MekoClient).
import { randomBytes } from 'node:crypto';
import { isIPv4, isIPv6 } from 'node:net';
import { WORK_QUEUE_DATAPACK, type WorkAddress, type WorkStoreState, type WorkStoreStatus } from '../shared/types.js';
import { kvDelete, kvGet, kvSet, type Db } from './db.js';
import { errMsg } from './log.js';
import { HttpError } from './projects.js';
import type { WorkQueue } from './workqueue.js';
import { AmpWorkStore, type WorkConn, type WorkSql } from './workstore.js';

/** The cluster's name in the datapack. */
export const WORK_CLUSTER = 'kode-work-queue';
/** The connection record's file name in the datapack. */
export const WORK_RECORD_FILE = 'kode-work-queue.json';
/** How the datapack's description starts; the record's hash follows (`Connection: sha256:<hash>`). */
const DESCRIPTION = 'Kode work queue.';
/** kv: this computer's copy of the connection record (password included), with the Meko Cloud account it's for. */
const RECORD_KEY = 'work-amp';
/** kv: a set-up under way (the address, and the password once made), so a restart picks it up. */
const STEP_KEY = 'work-amp-step';
/** The writer's size: AMP's smallest, which fits the free tier. */
const WRITER_CORES = 0.5;
/** How long set-up waits for the database's address, and how often it asks. */
const WAIT_MS = 10 * 60_000;
const POLL_MS = 10_000;
/**
 * The least time between two reads of the record after a failed query: a minute, doubled after each read
 * that finds nothing new, up to an hour, and a minute again once the database answers.
 */
const RECHECK_MS = 60_000;
const RECHECK_MAX_MS = 60 * 60_000;

/** The connection record: in the datapack as WORK_RECORD_FILE, and on each computer that uses it (kv). */
export interface WorkRecord {
  version: 1;
  datapackId: string;
  clusterId: string;
  branchId: string;
  computeId: string;
  host: string;
  port: number;
  database: string;
  user: string;
  password: string;
  allowed: WorkAddress[];
  updatedAt: number;
}

/** What WorkCloud needs of Meko: its tools, through the Meko on this computer. */
export interface WorkMeko {
  /** An amp_* tool, with the service conversation filled in. */
  call(tool: string, args: Record<string, unknown>, timeoutMs?: number): Promise<unknown>;
  /** The datapack the account owns by this exact name, with its description, or null. */
  findDatapack(name: string): Promise<{ id: string; description: string | null } | null>;
  /** Makes a datapack: its id. */
  createDatapack(name: string): Promise<string>;
  setDescription(datapackId: string, description: string): Promise<void>;
  /** Saves a file in a datapack: its hash. */
  putFile(datapackId: string, name: string, content: string): Promise<string>;
  /** A file by its hash, or null when the datapack doesn't have it. */
  getFile(datapackId: string, hash: string): Promise<string | null>;
}

/** The Meko on this computer's link to Meko Cloud, as it last said: `unknown` when it isn't running or can't say. */
export interface WorkLink {
  state: 'linked' | 'unlinked' | 'unknown';
  account: string;
}

export interface WorkCloudDeps {
  db: Db;
  meko: WorkMeko;
  link: () => WorkLink;
  /** Opens a connection to the database (pgSql; PGlite in the tests). */
  connect: (c: WorkConn) => WorkSql;
  queue: WorkQueue;
  changed: () => void;
  warn: (msg: string, detail?: string) => void;
  /** Waits between asks for the database's address (tests make it instant). */
  sleep?: (ms: number) => Promise<void>;
  waitMs?: number;
}

type Json = Record<string, unknown>;
const isObj = (v: unknown): v is Json => !!v && typeof v === 'object' && !Array.isArray(v);

/** The rows of a list reply: the array itself, or the first array in the object. */
function rowsOf(r: unknown): Json[] {
  if (Array.isArray(r)) return r.filter(isObj);
  if (isObj(r)) for (const v of Object.values(r)) if (Array.isArray(v)) return v.filter(isObj);
  return [];
}

/**
 * The first of `keys` holding a plain value (text, a number or a boolean), in the object or the objects in
 * it (a reply may wrap what it describes: `{ cluster: { … } }`, `{ endpoint: { address } }`).
 */
function pick(o: unknown, keys: string[], depth = 2): string | number | boolean | undefined {
  if (!isObj(o)) return undefined;
  for (const k of keys) {
    const v = o[k];
    if ((typeof v === 'string' && v) || typeof v === 'number' || typeof v === 'boolean') return v;
  }
  if (depth > 0) {
    for (const v of Object.values(o)) {
      const got = pick(v, keys, depth - 1);
      if (got !== undefined) return got;
    }
  }
  return undefined;
}

const str = (o: unknown, keys: string[]): string => {
  const v = pick(o, keys);
  return v === undefined || typeof v === 'boolean' ? '' : String(v);
};

const HOST = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$/i;

/** An endpoint's address from AMP's reply: a host field, a `host:port`, or a postgres:// URI. */
export function endpointOf(r: unknown): { host: string; port: number; database: string } | null {
  const raw = str(r, ['external_host', 'ingress_host', 'host', 'hostname', 'external_address', 'address', 'endpoint',
    'connection_uri', 'connection_string', 'uri', 'dsn']);
  if (!raw) return null;
  let host = raw;
  let port = Number(str(r, ['external_port', 'ingress_port', 'port'])) || 5432;
  let database = str(r, ['database', 'dbname', 'database_name']) || 'postgres';
  if (/^postgres(ql)?:\/\//i.test(raw)) {
    try {
      const u = new URL(raw);
      host = u.hostname;
      port = Number(u.port) || port;
      database = decodeURIComponent(u.pathname.replace(/^\//, '')) || database;
    } catch {
      return null;
    }
  } else {
    const m = /^([^:/]+):(\d+)$/.exec(raw);
    if (m) {
      host = m[1];
      port = Number(m[2]);
    }
  }
  if (!HOST.test(host) || !Number.isInteger(port) || port < 1 || port > 65535) return null;
  return { host: host.toLowerCase(), port, database };
}

/**
 * An address the database lets in, as AMP takes it: a CIDR no wider than /24 (IPv4) or /48 (IPv6), or a
 * single address (made /32 or /128). Null for anything else, or an unspecified or IPv4-mapped one.
 */
export function checkCidr(v: unknown): string | null {
  if (typeof v !== 'string') return null;
  const [ip, bits, extra] = v.trim().split('/');
  if (extra !== undefined || !ip) return null;
  const v4 = isIPv4(ip);
  if (!v4 && !isIPv6(ip)) return null;
  const max = v4 ? 32 : 128;
  const n = bits === undefined ? max : /^\d{1,3}$/.test(bits) ? Number(bits) : NaN;
  if (!(n >= (v4 ? 24 : 48) && n <= max)) return null;
  if (v4 ? ip === '0.0.0.0' : /^[0:]+$/.test(ip) || /^::ffff:/i.test(ip)) return null;
  return `${v4 ? ip : ip.toLowerCase()}/${n}`;
}

/**
 * A connection record read from Meko, checked before it's used: it's data from outside, never
 * instructions. Null when it isn't one this Kode can read.
 */
export function readRecord(text: string | null): WorkRecord | null {
  if (!text) return null;
  let r: unknown;
  try {
    r = JSON.parse(text);
  } catch {
    return null;
  }
  if (!isObj(r) || r.version !== 1) return null;
  const s = (k: string) => (typeof r[k] === 'string' && (r[k] as string).length > 0 && (r[k] as string).length <= 512 ? (r[k] as string) : null);
  const ids = ['datapackId', 'clusterId', 'branchId', 'computeId', 'host', 'database', 'user', 'password'].map(s);
  if (ids.some((x) => x == null) || !HOST.test(s('host')!)) return null;
  if (!Number.isInteger(r.port) || (r.port as number) < 1 || (r.port as number) > 65535) return null;
  if (!Array.isArray(r.allowed) || !r.allowed.length || r.allowed.length > 50) return null;
  const allowed: WorkAddress[] = [];
  for (const a of r.allowed) {
    const cidr = isObj(a) ? checkCidr(a.cidr) : null;
    if (!cidr || !isObj(a) || typeof a.label !== 'string') return null;
    allowed.push({ cidr, label: a.label.slice(0, 80) });
  }
  const [datapackId, clusterId, branchId, computeId, host, database, user, password] = ids as string[];
  return {
    version: 1, datapackId, clusterId, branchId, computeId, host: host.toLowerCase(), port: r.port as number, database, user, password, allowed,
    updatedAt: typeof r.updatedAt === 'number' ? r.updatedAt : 0,
  };
}

const conn = (r: WorkRecord): WorkConn => ({ host: r.host, port: r.port, database: r.database, user: r.user, password: r.password });
const sameConn = (a: WorkRecord, b: WorkRecord) => JSON.stringify(conn(a)) === JSON.stringify(conn(b)) && a.computeId === b.computeId;

/** The queue's database in Meko Cloud on this computer: set-up, joining, the addresses, and finding it again. */
export class WorkCloud {
  private status: WorkStoreStatus = 'not-linked';
  private step: string | null = null;
  private error: string | null = null;
  /** A set-up, join or address change under way. */
  private busy: Promise<void> | null = null;
  /** The record another computer saved, while this one hasn't joined (`can-join`). */
  private found: WorkRecord | null = null;
  private lastRecheck = 0;
  private recheckGap = RECHECK_MS;

  constructor(private readonly deps: WorkCloudDeps) {}

  private get db(): Db {
    return this.deps.db;
  }

  /** This computer's copy of the record, with the account it's for. */
  private saved(): (WorkRecord & { account: string }) | null {
    const raw = kvGet(this.db, RECORD_KEY);
    if (!raw) return null;
    const r = readRecord(raw);
    if (!r) return null;
    const account = (JSON.parse(raw) as { account?: unknown }).account;
    return { ...r, account: typeof account === 'string' ? account : '' };
  }

  private save(r: WorkRecord): void {
    kvSet(this.db, RECORD_KEY, JSON.stringify({ ...r, account: this.deps.link().account }));
  }

  state(): WorkStoreState {
    const q = this.deps.queue;
    const unreachable = this.status === 'ready' && q.unreachableError != null;
    return {
      status: unreachable ? 'unreachable' : this.status,
      datapack: WORK_QUEUE_DATAPACK,
      step: this.status === 'setting-up' ? this.step : null,
      error: unreachable ? q.unreachableError : this.error,
      addresses: (this.saved() ?? this.found)?.allowed ?? [],
      readOnly: q.isReadOnly,
    };
  }

  /** Why the queue can't be used yet, as the routes' 409 says it. */
  notReady(): string {
    switch (this.status) {
      case 'can-join':
        return 'Your work queue is in Meko Cloud: join it from this computer in Settings › Work queue.';
      case 'setting-up':
        return 'Your work queue is still being set up in Meko Cloud. Try again in a minute.';
      case 'not-set-up':
        return 'Your work queue is kept in Meko Cloud. Set it up in Settings › Work queue.';
      default:
        return "Your work queue is kept in Meko Cloud. Link Meko to Meko Cloud in Meko's settings, then set it up in Settings › Work queue.";
    }
  }

  private to(status: WorkStoreStatus, step: string | null = null): void {
    this.status = status;
    this.step = step;
    this.deps.changed();
  }

  private stepTo(step: string): void {
    this.to('setting-up', step);
  }

  /**
   * Works out where the queue stands (at start, when Meko's link changes, and when a page shows the queue
   * before set-up): uses this computer's record when it has one, picks up a set-up a restart cut short,
   * or looks in the datapack for a queue another computer set up.
   */
  async lookup(): Promise<void> {
    if (this.busy) return;
    const link = this.deps.link();
    let rec = this.saved();
    if (link.state === 'unlinked' || (rec && link.state === 'linked' && rec.account && rec.account !== link.account)) {
      // Unlinked, or linked to another account, whose queue this one isn't: the queue is empty here until it's set up again.
      if (rec && link.state === 'linked') kvDelete(this.db, RECORD_KEY);
      rec = null;
      if (this.deps.queue.attached) await this.deps.queue.detach();
      if (link.state === 'unlinked') {
        this.found = null;
        this.error = null;
        return this.to('not-linked');
      }
    }
    // With a record of its own, the queue opens even while the Meko on this computer is down.
    if (rec) {
      if (!this.deps.queue.attached) await this.use(rec);
      return;
    }
    if (link.state !== 'linked') return this.to('not-linked');
    const step = this.stepSaved();
    // A set-up for another Meko Cloud account isn't resumed in this one: nobody pressed Set up here.
    if (step && (!step.account || step.account === link.account)) {
      this.begin(() => this.setUp(step.cidr, step.label));
      return;
    }
    try {
      const dp = await this.deps.meko.findDatapack(WORK_QUEUE_DATAPACK);
      this.found = dp ? await this.recordIn(dp.id, dp.description) : null;
      this.error = null;
    } catch (e) {
      this.found = null;
      this.error = `Couldn't look for your work queue in Meko Cloud: ${errMsg(e)}`;
    }
    this.to(this.found ? 'can-join' : 'not-set-up');
  }

  /**
   * Starts using a record: connects, makes or updates the tables, and reads the queue. A failure shows in the
   * state: unreachable, or the database's error (WorkQueue.read).
   */
  private async use(rec: WorkRecord): Promise<void> {
    const q = this.deps.queue;
    if (q.attached) await q.detach();
    const store = new AmpWorkStore(this.deps.connect(conn(rec)));
    this.found = null;
    this.error = null;
    this.recheckGap = RECHECK_MS;
    this.to('ready');
    await q.attach(store, async () => (await store.migrate()).readOnly).catch(() => undefined);
  }

  /**
   * A query couldn't reach the database: another computer may have replaced its endpoint (a new address
   * on the allow-list), so the record is read from the datapack again, at most once a minute, less often
   * while it finds nothing new (RECHECK_MAX_MS). A new record is used only once AMP gives its host for its
   * endpoint: the record is data anyone who can write the datapack could change.
   */
  recheck(): Promise<void> {
    const rec = this.saved();
    if (!rec || this.busy || Date.now() - this.lastRecheck < this.recheckGap || this.deps.link().state !== 'linked') return Promise.resolve();
    this.lastRecheck = Date.now();
    return (async () => {
      const dp = await this.deps.meko.findDatapack(WORK_QUEUE_DATAPACK);
      const now = dp ? await this.recordIn(dp.id, dp.description) : null;
      if (!now || sameConn(now, rec)) {
        if (now && JSON.stringify(now.allowed) !== JSON.stringify(rec.allowed)) this.save({ ...rec, allowed: now.allowed });
        this.recheckGap = Math.min(this.recheckGap * 2, RECHECK_MAX_MS);
        return;
      }
      const w = await this.writer(now.datapackId, now.clusterId, now.branchId, now.computeId).catch(() => null);
      if (!w || w.host !== now.host || w.port !== now.port) {
        this.recheckGap = Math.min(this.recheckGap * 2, RECHECK_MAX_MS);
        this.deps.warn(`work queue: the connection record in ${WORK_QUEUE_DATAPACK} doesn't match the database in Meko Cloud; not using it`);
        return;
      }
      this.save(now);
      await this.use(now);
    })().catch((e) => this.deps.warn('work queue: reading its connection record again failed', errMsg(e)));
  }

  /** The database answered again: the next failure reads the record again a minute after the last read. */
  reached(): void {
    this.recheckGap = RECHECK_MS;
  }

  /** The set-up under way, with the Meko Cloud account it's in ('' from before it was kept). */
  private stepSaved(): { cidr: string; label: string; account: string; password?: string } | null {
    try {
      const s = JSON.parse(kvGet(this.db, STEP_KEY) ?? 'null') as { cidr?: unknown; label?: unknown; account?: unknown; password?: unknown } | null;
      const cidr = checkCidr(s?.cidr);
      return s && cidr
        ? {
          cidr, label: typeof s.label === 'string' ? s.label : '', account: typeof s.account === 'string' ? s.account : '',
          ...(typeof s.password === 'string' ? { password: s.password } : {}),
        }
        : null;
    } catch {
      return null;
    }
  }

  /** When the set-up, join or address change under way (if any) has ended. */
  async settled(): Promise<void> {
    while (this.busy) await this.busy;
  }

  /** Runs a set-up, join or address change in the background; its failure is shown in the state, in words. */
  private begin(fn: () => Promise<void>, onFail?: () => void): void {
    const before = this.status;
    this.error = null;
    this.busy = fn()
      .catch((e) => {
        this.error = errMsg(e);
        this.deps.warn('work queue: setting it up in Meko Cloud failed', errMsg(e));
        onFail?.();
        // Back to where the queue stood; a set-up resumed at start began before lookup said, and Meko is linked then.
        if (this.status === 'setting-up') this.status = before === 'setting-up' || before === 'not-linked' ? 'not-set-up' : before;
      })
      .finally(() => {
        this.busy = null;
        this.step = null;
        this.deps.changed();
      });
  }

  private address(cidr: unknown): string {
    const c = checkCidr(cidr);
    if (!c) throw new HttpError(400, "Give this computer's address as an IP address or a CIDR no wider than /24 (/48 for IPv6), such as 203.0.113.4.");
    return c;
  }

  private label(v: unknown): string {
    return typeof v === 'string' ? v.trim().slice(0, 80) : '';
  }

  /** Settings › Work queue's Set up, or Join once another computer has: in the background (POST /api/work-queue/setup). */
  startSetUp(cidr: unknown, label: unknown): void {
    const c = this.address(cidr);
    if (this.busy) throw new HttpError(409, 'Kode is already setting up your work queue. Wait for it to finish.');
    if (this.saved()) throw new HttpError(409, 'Your work queue is set up on this computer already. Add an address below instead.');
    if (this.deps.link().state !== 'linked') throw new HttpError(409, this.notReady());
    // A set-up that failed after making the database keeps its password, which only this computer has.
    const prev = this.stepSaved();
    const account = this.deps.link().account;
    const password = prev?.password && prev.account === account ? { password: prev.password } : {};
    kvSet(this.db, STEP_KEY, JSON.stringify({ cidr: c, label: this.label(label), account, ...password }));
    this.begin(() => this.setUp(c, this.label(label)), () => {
      // A set-up that failed before making anything starts over next time; one that made the database keeps its password.
      if (!this.stepSaved()?.password) kvDelete(this.db, STEP_KEY);
    });
  }

  /**
   * Sets up the queue, or joins the one another computer set up. Each step first looks for what it would
   * make, so a restart part way through picks up where it stopped.
   */
  private async setUp(cidr: string, label: string): Promise<void> {
    const m = this.deps.meko;
    this.stepTo('Looking for your work queue in Meko Cloud');
    const dp = await m.findDatapack(WORK_QUEUE_DATAPACK);
    const there = dp ? await this.recordIn(dp.id, dp.description) : null;
    if (there) return this.join(there, cidr, label);
    let datapackId = dp?.id;
    const step = this.stepSaved();
    // The database first: one a set-up cut short made is already counted in the plan, so it needs no more room.
    let cluster = datapackId
      ? rowsOf(await m.call('amp_cluster_list', { datapack_id: datapackId })).find((c) => str(c, ['name', 'cluster_name']) === WORK_CLUSTER)
      : undefined;
    // Its password is made here, and saved before the database is, so a restart doesn't lose it.
    if (cluster && !step?.password) throw new Error('Another of your computers is setting up the work queue. Try again in a few minutes.');
    if (!cluster) {
      this.stepTo("Checking your Meko Cloud plan's room for a database");
      await this.checkBudget(datapackId);
    }
    if (!datapackId) {
      this.stepTo(`Making the datapack ${WORK_QUEUE_DATAPACK}`);
      try {
        datapackId = await m.createDatapack(WORK_QUEUE_DATAPACK);
      } catch (e) {
        throw new Error(`Meko Cloud didn't make the datapack ${WORK_QUEUE_DATAPACK}: ${errMsg(e)}. Datapack names are unique across Meko Cloud, so another account may have it already.`, { cause: e });
      }
    }
    const password = step?.password ?? randomBytes(24).toString('base64url');
    kvSet(this.db, STEP_KEY, JSON.stringify({ cidr, label, account: this.deps.link().account, password }));
    if (!cluster) {
      this.stepTo('Making the database');
      const made = await m.call('amp_cluster_create', {
        datapack_id: datapackId, name: WORK_CLUSTER, with_writer: true, connectivity_mode: 'ingress_external', allowed_cidrs: [cidr], password,
      }, 180_000);
      cluster = isObj(made) ? made : {};
    }
    const clusterId = str(cluster, ['cluster_id', 'id']);
    if (!clusterId) throw new Error("Meko Cloud's reply named no database. Try again.");
    const branchId = await this.mainBranch(datapackId, clusterId);
    this.stepTo("Waiting for the database's address (this can take a few minutes)");
    const w = await this.waitWriter(datapackId, clusterId, branchId).catch((e: unknown) => {
      throw new Error(`${errMsg(e)} Set it up again in Settings › Work queue: Kode picks up where it stopped.`, { cause: e });
    });
    const rec: WorkRecord = {
      version: 1, datapackId, clusterId, branchId, computeId: w.id, host: w.host, port: w.port, database: w.database, user: 'cloud_admin', password,
      allowed: [{ cidr, label }], updatedAt: Date.now(),
    };
    this.stepTo('Saving the connection in the datapack');
    await this.publish(rec);
    this.save(rec);
    kvDelete(this.db, STEP_KEY);
    this.stepTo('Making the tables and copying in this computer\'s items');
    await this.use(rec);
  }

  /** Joins a queue another computer set up: lets this computer's address in when it isn't yet. */
  private async join(rec: WorkRecord, cidr: string, label: string): Promise<void> {
    this.stepTo('Checking the connection record');
    // The record is data read from Meko: its host must be the one AMP gives for its endpoint.
    const w = await this.writer(rec.datapackId, rec.clusterId, rec.branchId, rec.computeId).catch(() => null);
    const withThis = rec.allowed.some((a) => a.cidr === cidr) ? rec.allowed : [...rec.allowed, { cidr, label }];
    let next: WorkRecord;
    if (!w) {
      const ids = { datapack_id: rec.datapackId, cluster_id: rec.clusterId, branch_id: rec.branchId };
      if (rowsOf(await this.deps.meko.call('amp_compute_list', ids)).some((c) => str(c, ['compute_id', 'id']) === rec.computeId)) {
        throw new Error("The work queue's database in Meko Cloud has no address yet. Try again in a few minutes.");
      }
      // Its endpoint is gone (an address change stopped part way): a new one, whose host comes from AMP, not the record.
      next = await this.recreate(rec, withThis);
    } else if (w.host !== rec.host || w.port !== rec.port) {
      throw new Error(`The connection record in ${WORK_QUEUE_DATAPACK} doesn't match the database in Meko Cloud. Try again in a few minutes.`);
    } else {
      next = withThis === rec.allowed ? rec : await this.recreate(rec, withThis);
    }
    this.save(next);
    kvDelete(this.db, STEP_KEY);
    this.stepTo('Copying in this computer\'s items');
    await this.use(next);
  }

  /** POST /api/work-queue/addresses: lets another address in, in the background. */
  startAddAddress(cidr: unknown, label: unknown): void {
    const c = this.address(cidr);
    const rec = this.ownRecord();
    if (rec.allowed.some((a) => a.cidr === c)) throw new HttpError(409, `${c} is let in already.`);
    this.begin(() => this.changeAddresses((all) => (all.some((a) => a.cidr === c) ? all : [...all, { cidr: c, label: this.label(label) }])));
  }

  /** POST /api/work-queue/addresses/remove: lets an address out, in the background. */
  startRemoveAddress(cidr: unknown): void {
    const c = checkCidr(cidr);
    const rec = this.ownRecord();
    if (!c || !rec.allowed.some((a) => a.cidr === c)) throw new HttpError(404, "That address isn't on the list.");
    if (rec.allowed.length === 1) throw new HttpError(409, 'Keep at least one address: the database lets in only the ones on the list.');
    this.begin(() => this.changeAddresses((all) => all.filter((a) => a.cidr !== c)));
  }

  private ownRecord(): WorkRecord {
    if (this.busy) throw new HttpError(409, 'Kode is changing your work queue in Meko Cloud already. Wait for it to finish.');
    const rec = this.saved();
    if (!rec) throw new HttpError(409, this.notReady());
    if (this.deps.link().state !== 'linked') throw new HttpError(409, "Changing the addresses needs the Meko on this computer, linked to Meko Cloud. Start Meko, or link it in Meko's settings.");
    return rec;
  }

  /** Changes the list from the latest record in the datapack (another computer may have changed it), then uses the new endpoint. */
  private async changeAddresses(change: (all: WorkAddress[]) => WorkAddress[]): Promise<void> {
    const before = this.status;
    this.stepTo('Reading the connection record');
    const own = this.saved()!;
    const dp = await this.deps.meko.findDatapack(WORK_QUEUE_DATAPACK);
    const latest = (dp ? await this.recordIn(dp.id, dp.description) : null) ?? own;
    const allowed = change(latest.allowed);
    if (!allowed.length) throw new Error('Keep at least one address.');
    const next = await this.recreate(latest, allowed);
    this.save(next);
    this.status = before;
    await this.use(next);
  }

  /**
   * Replaces the database's endpoint with one that lets `allowed` in: AMP can't change an endpoint's list,
   * and a branch takes one writer (D10). The new one gets the same password, which the data keeps anyway.
   */
  private async recreate(rec: WorkRecord, allowed: WorkAddress[]): Promise<WorkRecord> {
    const m = this.deps.meko;
    this.stepTo('Replacing the database\'s endpoint with one that lets the addresses in');
    await this.checkBudget(rec.datapackId, true);
    const ids = { datapack_id: rec.datapackId, cluster_id: rec.clusterId, branch_id: rec.branchId };
    // A branch takes one writer, so the old one goes before the new one is made.
    let removed = false;
    for (const c of rowsOf(await m.call('amp_compute_list', ids))) {
      const id = str(c, ['compute_id', 'id']);
      if (id && str(c, ['role', 'compute_role']) !== 'read_only') {
        await m.call('amp_compute_delete', { ...ids, compute_id: id }, 180_000);
        removed = true;
      }
    }
    let w: Awaited<ReturnType<WorkCloud['waitWriter']>>;
    try {
      await m.call('amp_compute_create', {
        ...ids, role: 'read_write', connectivity_mode: 'ingress_external', allowed_cidrs: allowed.map((a) => a.cidr), password: rec.password,
      }, 180_000);
      this.stepTo("Waiting for the endpoint's address (this can take a few minutes)");
      w = await this.waitWriter(rec.datapackId, rec.clusterId, rec.branchId);
    } catch (e) {
      if (!removed) throw e;
      // The record still names the endpoint that's gone, so the same press again (Let it in, Remove or Join) makes a new one.
      throw new Error(`Meko Cloud removed the database's old endpoint, but the new one isn't ready: ${errMsg(e)} Your work queue can't be reached until it is: do the same again in Settings › Work queue to finish.`, { cause: e });
    }
    const next: WorkRecord = { ...rec, computeId: w.id, host: w.host, port: w.port, database: w.database, allowed, updatedAt: Date.now() };
    this.stepTo('Saving the connection in the datapack');
    await this.publish(next);
    return next;
  }

  /** Saves the record in the datapack, and its hash in the description, where every computer looks for it (D11). */
  private async publish(rec: WorkRecord): Promise<void> {
    const hash = await this.deps.meko.putFile(rec.datapackId, WORK_RECORD_FILE, JSON.stringify(rec));
    await this.deps.meko.setDescription(rec.datapackId,
      `${DESCRIPTION} Connection: sha256:${hash}. It holds the work queue database's password: don't share this datapack.`);
  }

  /** The record a datapack's description points to, checked; null when there's none (yet) or it can't be read. */
  private async recordIn(datapackId: string, description: string | null): Promise<WorkRecord | null> {
    const hash = /sha256:([0-9a-f]{64})/.exec(description ?? '')?.[1];
    if (!hash) return null;
    const rec = readRecord(await this.deps.meko.getFile(datapackId, hash));
    if (!rec) this.deps.warn(`work queue: the connection record in ${WORK_QUEUE_DATAPACK} couldn't be read`);
    return rec && rec.datapackId === datapackId ? rec : null;
  }

  /** Refuses, in words, when the account's plan has no room for the writer: AMP's 402 is never retried. */
  private async checkBudget(datapackId: string | undefined, replacing = false): Promise<void> {
    const b = await this.deps.meko.call('amp_core_budget', datapackId ? { datapack_id: datapackId } : {});
    const used = Number(pick(b, ['used_cores']));
    const max = Number(pick(b, ['max_cores']));
    if (!Number.isFinite(used) || !Number.isFinite(max) || max <= 0) return;
    // Replacing the endpoint deletes the old one first, so its cores are free again.
    const need = replacing ? 0 : WRITER_CORES;
    if (used + need <= max + 1e-9) return;
    const tier = str(b, ['tier']);
    const next = str(b, ['next_tier']);
    throw new Error(`Your Meko Cloud${tier ? ` ${tier}` : ''} plan allows ${max} vCPU for databases, and ${used} are in use. The work queue's database needs ${WRITER_CORES}: free some up${next ? `, or move to the ${next} plan` : ''}, then try again.`);
  }

  /** The cluster's main branch: the default one (AMP names it main). */
  private async mainBranch(datapackId: string, clusterId: string): Promise<string> {
    const rows = rowsOf(await this.deps.meko.call('amp_branch_list', { datapack_id: datapackId, cluster_id: clusterId }));
    const main = rows.find((b) => pick(b, ['is_default', 'default'], 0) === true) ?? rows.find((b) => str(b, ['name', 'branch_name']) === 'main') ?? rows[0];
    const id = main ? str(main, ['branch_id', 'id']) : '';
    if (!id) throw new Error("Meko Cloud didn't list the database's main branch. Try again.");
    return id;
  }

  /** The writer's address, when AMP gives one; null when it has none (yet) or isn't there. */
  private async writer(datapackId: string, clusterId: string, branchId: string, computeId: string): Promise<({ id: string } & NonNullable<ReturnType<typeof endpointOf>>) | null> {
    const r = await this.deps.meko.call('amp_compute_get', { datapack_id: datapackId, cluster_id: clusterId, branch_id: branchId, compute_id: computeId });
    const e = endpointOf(r);
    return e ? { id: computeId, ...e } : null;
  }

  /** Waits for the branch's writer to have an address. */
  private async waitWriter(datapackId: string, clusterId: string, branchId: string): Promise<{ id: string } & NonNullable<ReturnType<typeof endpointOf>>> {
    const sleep = this.deps.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
    const until = Date.now() + (this.deps.waitMs ?? WAIT_MS);
    for (;;) {
      const rows = rowsOf(await this.deps.meko.call('amp_compute_list', { datapack_id: datapackId, cluster_id: clusterId, branch_id: branchId }));
      const w = rows.find((c) => str(c, ['role', 'compute_role']) !== 'read_only');
      const id = w ? str(w, ['compute_id', 'id']) : '';
      const got = id ? (endpointOf(w) ? { id, ...endpointOf(w)! } : await this.writer(datapackId, clusterId, branchId, id)) : null;
      if (got) return got;
      if (Date.now() >= until) throw new Error("Meko Cloud didn't give the database an address within 10 minutes.");
      await sleep(POLL_MS);
    }
  }
}
