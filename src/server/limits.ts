import type { LimitWindow, PlanLimits } from '../shared/types.js';
import { kvGet, kvSet, type Db } from './db.js';

const KV_KEY = 'plan_limits';
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
 * a stale report), and within the same window the higher use, since use only grows until reset.
 * `fresh` (`b` is new data, see `PlanLimitsStore`) lets a lower reading of the same window win:
 * an early reset by Anthropic keeps the reset time and only lowers the use.
 */
function newerWindow(a: LimitWindow | null, b: LimitWindow | null, fresh = false): LimitWindow | null {
  if (!a || !b) return a ?? b;
  if (a.resetsAt !== b.resetsAt) return a.resetsAt > b.resetsAt ? a : b;
  return fresh || b.usedPercent > a.usedPercent ? b : a;
}

/**
 * The account's plan limits. Every session reports the same account, but an idle session can
 * re-run its status line with an old reading, or one without a window, so reports are merged
 * per window instead of replacing each other. A reading of a window that changed since the
 * same session's last one is new data and replaces that window, even when lower; one that
 * didn't changes nothing, and a session's first (since the server started or its Claude did)
 * only raises it, since it may be old. Kept in the kv table so they show again after a
 * restart, until they reset.
 */
export class PlanLimitsStore {
  private last: PlanLimits | null = null;
  private savedAt = 0;
  /** Each session's last reading, to tell new data from a repeat. */
  private seen = new Map<string, PlanLimits>();

  constructor(private db: Db, private changed: () => void) {
    try {
      this.last = JSON.parse(kvGet(db, KV_KEY) ?? 'null') as PlanLimits | null;
    } catch {
      this.last = null;
    }
  }

  /** Takes a session's status line input (see `take`). */
  report(session: string, input: unknown, now = Date.now()): void {
    this.take(session, parseRateLimits(input, now), now);
  }

  /** Takes the limits AgentK's mod measured in a session (see `take`). */
  reportMeasured(session: string, rateLimits: unknown, now = Date.now()): void {
    this.take(session, parseMeasuredLimits(rateLimits, now), now);
  }

  /** A session's Claude ended: its next reading is a first one again. */
  forget(session: string): void {
    this.seen.delete(session);
  }

  /**
   * Takes a reading; saves and broadcasts when the merged numbers moved, or when a reading
   * confirms them and a minute has passed (so "as of" stays close).
   */
  private take(session: string, reading: PlanLimits | null, now: number): void {
    const reported = currentLimits(reading, now);
    if (!reported) return; // nothing, or only windows that have already reset
    const before = this.seen.get(session);
    this.seen.set(session, reported);
    const prev = currentLimits(this.last, now);
    const merged = (k: 'fiveHour' | 'sevenDay') => {
      const was = before?.[k];
      const read = reported[k];
      if (was && read && sameWindow(was, read)) return prev?.[k] ?? read; // the session's repeat says nothing new
      return newerWindow(prev?.[k] ?? null, read, !!was && !!read);
    };
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

  current(now = Date.now()): PlanLimits | null {
    return currentLimits(this.last, now);
  }
}
