import { autoConnectDue, isHarnessId, type HarnessId } from '../shared/harnesses.js';
import type { HarnessStatus } from '../shared/types.js';
import { kvGet, kvSet, type Db } from './db.js';

/**
 * The agents AgentK has connected to Meko by itself (`HarnessCaps.autoConnect`, `HarnessStatus.autoConnect`),
 * once each, ever. The page does the connect, through Meko's owner-only route (AgentK's server can't:
 * its client token is refused), after claiming it here, so several open pages connect an agent once,
 * and an agent the user disconnects afterwards stays disconnected. Kept in state.db's kv as the ids
 * and when each was tried; a failed try is the user's to retry, with Connect to Meko.
 */

const KV_KEY = 'meko-auto-connect';

export class MekoAutoConnect {
  private tried: Partial<Record<HarnessId, string>>;

  constructor(private db: Db) {
    this.tried = {};
    try {
      const v: unknown = JSON.parse(kvGet(db, KV_KEY) ?? '{}');
      if (v && typeof v === 'object' && !Array.isArray(v)) {
        for (const [id, at] of Object.entries(v)) if (isHarnessId(id) && typeof at === 'string') this.tried[id] = at;
      }
    } catch {
      // Unreadable: start over. At worst an agent is connected once more.
    }
  }

  /** The page should connect it: due (`autoConnectDue`), and not tried before. */
  due(h: Omit<HarnessStatus, 'autoConnect'>): boolean {
    return autoConnectDue(h) && !this.tried[h.id];
  }

  /** Whether the asking page connects it: true once, while it's due; recorded before it answers. */
  claim(h: Omit<HarnessStatus, 'autoConnect'>, now = new Date()): boolean {
    if (!this.due(h)) return false;
    this.tried[h.id] = now.toISOString();
    kvSet(this.db, KV_KEY, JSON.stringify(this.tried));
    return true;
  }
}
