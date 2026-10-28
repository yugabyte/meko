/** Most calls kept open per session: more than this is a CLI that never reports their ends. */
const MAX_KEYS = 20;
/** Longest key kept. */
const MAX_KEY = 200;

/**
 * The Meko calls under way in each session as its CLI's hooks report them (Kiro's, OpenCode's: the
 * ones without AgentK's mod, whose calls `ModLinks` keeps), for the dot's Meko m. A call opens at
 * its `PreToolUse` and closes at its `PostToolUse`, matched by the key the harness gives
 * (`Harness.mekoTool`). Kiro's calls have no id, so a key can be open more than once and is counted.
 * The turn's end and a relaunch clear the session's calls, for an end that never came (Kiro runs
 * `preToolUse` before asking the user, so a call they refuse has no end).
 */
export class MekoCalls {
  private open = new Map<string, Map<string, number>>();

  /** True when the call was taken (not a bad key, nor a new one past the cap). */
  start(session: string, key: string): boolean {
    if (!key || key.length > MAX_KEY) return false;
    let calls = this.open.get(session);
    if (!calls) this.open.set(session, (calls = new Map()));
    if (!calls.has(key) && calls.size >= MAX_KEYS) return false;
    calls.set(key, (calls.get(key) ?? 0) + 1);
    return true;
  }

  /** One call of `key` ended; one that never started is ignored. */
  end(session: string, key: string): void {
    const calls = this.open.get(session);
    const n = calls?.get(key);
    if (!calls || !n) return;
    if (n > 1) calls.set(key, n - 1);
    else calls.delete(key);
    if (!calls.size) this.open.delete(session);
  }

  /** True while the session has a call open. */
  active(session: string): boolean {
    return this.open.has(session);
  }

  clear(session: string): void {
    this.open.delete(session);
  }
}

/** The least time the dot's Meko m shows from a call's start, so a quick call is still seen. */
export const MEKO_MIN_SHOW_MS = 1500;

/**
 * Keeps the dot's Meko m up for MEKO_MIN_SHOW_MS from each call's start (any agent's), however soon
 * the call ends. State is pushed at most every 100 ms (server.ts), so a call that starts and ends
 * within that would never show at all. A later start moves the end on; `onChange` is told when a
 * hold runs out. The turn's end and a relaunch clear it.
 */
export class MekoHold {
  private held = new Map<string, { until: number; timer: NodeJS.Timeout }>();

  constructor(
    private onChange: (session: string) => void,
    private now: () => number = Date.now,
  ) {}

  started(session: string): void {
    this.clear(session);
    const timer = setTimeout(() => {
      this.held.delete(session);
      this.onChange(session);
    }, MEKO_MIN_SHOW_MS);
    timer.unref?.();
    this.held.set(session, { until: this.now() + MEKO_MIN_SHOW_MS, timer });
  }

  /** True while a call that started less than MEKO_MIN_SHOW_MS ago keeps the m up. */
  active(session: string): boolean {
    const h = this.held.get(session);
    return !!h && this.now() < h.until;
  }

  clear(session: string): void {
    const h = this.held.get(session);
    if (!h) return;
    clearTimeout(h.timer);
    this.held.delete(session);
  }
}
