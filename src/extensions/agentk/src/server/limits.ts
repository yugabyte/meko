import type { LimitWindow, PlanLimits } from '../shared/types.js';
import { kvGet, kvSet, type Db } from './db.js';

const KV_KEY = 'plan_limits';
/** Each session's last reading of each window, so a server restart doesn't turn repeats into first readings. */
const SEEN_KEY = 'plan_limits_seen';
/** The Claude account the stored limits belong to (`ClaudeAccount`), so a sign-in to another one starts afresh. */
const ACCOUNT_KEY = 'plan_limits_account';
/**
 * How far below a session's own last reading a reading must be to count as new: two status
 * line runs can reach the server out of order (each is sent in the background), and the older
 * one is at most a reply's use lower. An early reset drops the use by more; a smaller drop
 * shows with the session's next reading, which is higher than this one.
 */
const DRIFT = 10;
/** An unchanged reading is re-saved and re-broadcast at most this often, so "as of" stays close. */
const QUIET_MS = 60_000;

function parseWindow(v: unknown): LimitWindow | null {
  if (!v || typeof v !== 'object') return null;
  const { used_percentage: pct, resets_at: at } = v as Record<string, unknown>;
  if (typeof pct !== 'number' || !Number.isFinite(pct) || typeof at !== 'number' || !Number.isFinite(at) || at <= 0) return null;
  // Documented as Unix seconds; take milliseconds too, in case that ever changes.
  return { usedPercent: Math.min(100, Math.max(0, pct)), resetsAt: at < 1e12 ? at * 1000 : at };
}

/** The plan limits in a status line input (`rate_limits.five_hour` / `.seven_day`), or null when it has none. */
export function parseRateLimits(input: unknown, now: number): PlanLimits | null {
  const rl = input && typeof input === 'object' ? (input as { rate_limits?: unknown }).rate_limits : undefined;
  if (!rl || typeof rl !== 'object') return null;
  const { five_hour, seven_day } = rl as Record<string, unknown>;
  const fiveHour = parseWindow(five_hour);
  const sevenDay = parseWindow(seven_day);
  return fiveHour || sevenDay ? { fiveHour, sevenDay, updatedAt: now } : null;
}

/**
 * The plan limits AgentK's mod reports (`session.measure`'s `rateLimits`: `{ kind, percentUsed,
 * resetsAt }`, the reset time as an ISO date), or null when it has none.
 */
export function parseMeasuredLimits(list: unknown, now: number): PlanLimits | null {
  if (!Array.isArray(list)) return null;
  const windowOf = (kind: string): LimitWindow | null => {
    const w = list.find((x): x is Record<string, unknown> => !!x && typeof x === 'object' && (x as { kind?: unknown }).kind === kind);
    const at = typeof w?.resetsAt === 'string' ? Date.parse(w.resetsAt) : NaN;
    return w ? parseWindow({ used_percentage: w.percentUsed, resets_at: at }) : null;
  };
  const fiveHour = windowOf('five_hour');
  const sevenDay = windowOf('seven_day');
  return fiveHour || sevenDay ? { fiveHour, sevenDay, updatedAt: now } : null;
}

/** Drops windows whose reset time has passed: their numbers no longer apply. */
export function currentLimits(l: PlanLimits | null, now: number): PlanLimits | null {
  if (!l) return null;
  const live = (w: LimitWindow | null) => (w && w.resetsAt > now ? w : null);
  const out = { ...l, fiveHour: live(l.fiveHour), sevenDay: live(l.sevenDay) };
  return out.fiveHour || out.sevenDay ? out : null;
}

const sameWindow = (a: LimitWindow | null, b: LimitWindow | null) =>
  a === b || (!!a && !!b && a.usedPercent === b.usedPercent && a.resetsAt === b.resetsAt);

/**
 * The better of two readings of one window: the later window (the earlier one has reset or is
 * a stale report), and within the same window the higher use, unless `b` is `fresh` (new data,
 * see `PlanLimitsStore`): an early reset by Anthropic keeps the reset time and only lowers the use.
 */
function newerWindow(a: LimitWindow | null, b: LimitWindow | null, fresh = false): LimitWindow | null {
  if (!a || !b) return a ?? b;
  if (a.resetsAt !== b.resetsAt) return a.resetsAt > b.resetsAt ? a : b;
  return fresh || b.usedPercent > a.usedPercent ? b : a;
}

type Seen = Record<string, Pick<PlanLimits, 'fiveHour' | 'sevenDay'>>;

/** Which Claude account is signed in (`accountUuid/organizationUuid`), or null when unknown (signed out, an API key). */
export type ClaudeAccount = () => string | null;

/**
 * The account's plan limits. Every session reports the same account, but an idle session can
 * re-run its status line with an old reading, or one without a window, so reports are merged
 * per window instead of replacing each other. Each session's last reading of each window tells
 * new data from a repeat: a reading that rose since it, or fell by more than `DRIFT`, is new
 * and replaces the window even when lower; one that didn't change changes nothing; and a
 * session's first only raises it, since it may be old. Both are kept in the kv table so they
 * hold across a restart, until they reset.
 */
export class PlanLimitsStore {
  private last: PlanLimits | null = null;
  private savedAt = 0;
  private seen: Seen = {};
  /** The account `last` belongs to; null until one is known. */
  private account: string | null = null;
  /**
   * Each session's last reading from before a change of account: Claude keeps showing it until
   * its next reply, so a repeat of it is the old account's and is ignored.
   */
  private before: Seen = {};

  constructor(private db: Db, private changed: () => void, private signedIn: ClaudeAccount = () => null) {
    const read = <T>(key: string, empty: T): T => {
      try {
        return (JSON.parse(kvGet(db, key) ?? 'null') as T | null) ?? empty;
      } catch {
        return empty;
      }
    };
    this.last = read<PlanLimits | null>(KV_KEY, null);
    this.seen = read<Seen>(SEEN_KEY, {});
    this.account = kvGet(db, ACCOUNT_KEY) || null;
  }

  /**
   * Starts afresh when Claude is signed in to another account: the limits are the old account's.
   * Returns whether it did. Being signed out (no account) isn't a change: the next sign-in is.
   */
  private checkAccount(): boolean {
    const now = this.signedIn();
    if (!now || now === this.account) return false;
    const first = this.account === null;
    this.account = now;
    kvSet(this.db, ACCOUNT_KEY, now);
    if (first) return false; // an upgrade: the limits AgentK has are this account's
    this.before = this.seen;
    this.seen = {};
    this.last = null;
    this.savedAt = 0;
    kvSet(this.db, SEEN_KEY, '{}');
    kvSet(this.db, KV_KEY, 'null');
    return true;
  }

  /** Takes a session's status line input (see `take`). */
  report(session: string, input: unknown, now = Date.now()): void {
    this.take(session, parseRateLimits(input, now), now);
  }

  /** Takes the limits AgentK's mod measured in a session (see `take`). */
  reportMeasured(session: string, rateLimits: unknown, now = Date.now()): void {
    this.take(session, parseMeasuredLimits(rateLimits, now), now);
  }

  /**
   * Takes a reading; saves and broadcasts when the merged numbers moved, or when a reading
   * confirms them and a minute has passed (so "as of" stays close).
   */
  private take(session: string, reading: PlanLimits | null, now: number): void {
    if (this.checkAccount()) this.changed();
    const reported = currentLimits(reading, now);
    if (!reported) return; // nothing, or only windows that have already reset
    const old = this.before[session];
    if (old) {
      // The session's first reading since the change of account: a repeat of its last is the old account's.
      if (sameWindow(old.fiveHour, reported.fiveHour ?? old.fiveHour) && sameWindow(old.sevenDay, reported.sevenDay ?? old.sevenDay)) return;
      delete this.before[session];
    }
    const before = this.seen[session];
    const prev = currentLimits(this.last, now);
    const merged = (k: 'fiveHour' | 'sevenDay') => {
      const was = before?.[k];
      const read = reported[k];
      if (!was || !read || was.resetsAt !== read.resetsAt) return newerWindow(prev?.[k] ?? null, read);
      if (read.usedPercent === was.usedPercent) return prev?.[k] ?? read; // the session's repeat says nothing new
      return newerWindow(prev?.[k] ?? null, read, read.usedPercent > was.usedPercent || was.usedPercent - read.usedPercent > DRIFT);
    };
    this.remember(session, reported, now);
    const next: PlanLimits = { fiveHour: merged('fiveHour'), sevenDay: merged('sevenDay'), updatedAt: now };
    const moved = !prev || !sameWindow(prev.fiveHour, next.fiveHour) || !sameWindow(prev.sevenDay, next.sevenDay);
    if (!moved) {
      // An older reading (a lower use, or a window that's missing) says nothing new.
      const confirms = sameWindow(reported.fiveHour, next.fiveHour) && sameWindow(reported.sevenDay, next.sevenDay);
      if (!confirms || now - this.savedAt < QUIET_MS) return;
    }
    this.last = next;
    this.savedAt = now;
    kvSet(this.db, KV_KEY, JSON.stringify(next));
    this.changed();
  }

  /** Keeps a session's reading of each window it has (a report without one keeps the last). */
  private remember(session: string, reported: PlanLimits, now: number): void {
    const before = this.seen[session];
    const fiveHour = reported.fiveHour ?? before?.fiveHour ?? null;
    const sevenDay = reported.sevenDay ?? before?.sevenDay ?? null;
    if (before && sameWindow(before.fiveHour, fiveHour) && sameWindow(before.sevenDay, sevenDay)) return;
    const seen: Seen = { [session]: { fiveHour, sevenDay } };
    // Readings of sessions whose windows have all reset are no use any more.
    for (const [id, l] of Object.entries(this.seen)) if (id !== session && currentLimits({ ...l, updatedAt: 0 }, now)) seen[id] = l;
    this.seen = seen;
    kvSet(this.db, SEEN_KEY, JSON.stringify(seen));
  }

  current(now = Date.now()): PlanLimits | null {
    this.checkAccount();
    return currentLimits(this.last, now);
  }
}
