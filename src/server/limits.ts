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
 * The account's plan limits. Every session reports the same account, so the newest report
 * wins. Kept in the kv table so they show again after a restart, until they reset.
 */
export class PlanLimitsStore {
  private last: PlanLimits | null = null;
  private savedAt = 0;

  constructor(private db: Db, private changed: () => void) {
    try {
      this.last = JSON.parse(kvGet(db, KV_KEY) ?? 'null') as PlanLimits | null;
    } catch {
      this.last = null;
    }
  }

  /** Takes a status line input; saves and broadcasts when the numbers moved, or a minute has passed. */
  report(input: unknown, now = Date.now()): void {
    const next = parseRateLimits(input, now);
    if (!next) return;
    const same = !!this.last && sameWindow(this.last.fiveHour, next.fiveHour) && sameWindow(this.last.sevenDay, next.sevenDay);
    this.last = next;
    if (same && now - this.savedAt < QUIET_MS) return;
    this.savedAt = now;
    kvSet(this.db, KV_KEY, JSON.stringify(next));
    this.changed();
  }

  current(now = Date.now()): PlanLimits | null {
    return currentLimits(this.last, now);
  }
}
