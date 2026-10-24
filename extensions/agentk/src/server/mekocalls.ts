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

  start(session: string, key: string): void {
    if (!key || key.length > MAX_KEY) return;
    let calls = this.open.get(session);
    if (!calls) this.open.set(session, (calls = new Map()));
    if (!calls.has(key) && calls.size >= MAX_KEYS) return;
    calls.set(key, (calls.get(key) ?? 0) + 1);
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
