import type { HarnessId } from '../shared/harnesses.js';
import type { HarnessMeko, MekoStatus } from '../shared/types.js';
import { kvDelete, kvGet, kvSet, type Db } from './db.js';
import { errMsg, type Logger } from './log.js';
import { originOf, type LocalMeko } from './mekolocal.js';
import { mekoUrlHost } from './mekosetup.js';

/**
 * Meko's list of the agents on this computer and whether each reaches Meko (`GET /api/v1/agents`,
 * Meko's docs/api.md): Meko owns the agents' connections. AgentK reads it on Meko's API listener
 * with the client token, never posts to it (connecting is the owner's: the page does it, through
 * Meko's web routes), and gates new sessions on it (`Harnesses`), so it keeps the last list read in
 * state.db for when Meko doesn't answer. Never having read one, or a Meko too old to list them
 * (404), gates nothing.
 */

export type MekoAgentState = 'connected' | 'stale' | 'elsewhere' | 'none';

/** One installed agent, as Meko reports it. */
export interface MekoAgent {
  name: string;
  title: string;
  state: MekoAgentState;
  /** Its `meko` server's URL, or null when it has none. */
  url: string | null;
}

/** Meko's answer: where agents connect (`target`: this Meko, or the linked Meko Cloud) and the installed agents. */
export interface MekoAgentList {
  target: { kind: 'local' | 'cloud'; url: string };
  agents: MekoAgent[];
}

export type MekoAgentsRead =
  | { kind: 'list'; list: MekoAgentList }
  /** Meko has no such route (404): too old to list agents. */
  | { kind: 'old' }
  /** Meko didn't answer, or not with a list: the last list read stands. */
  | { kind: 'failed'; error: string };

const STATES: readonly string[] = ['connected', 'stale', 'elsewhere', 'none'];
const KV_KEY = 'meko-agents';
const TIMEOUT_MS = 5000;

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** The list in Meko's `data`, or null when it isn't one. Agents it can't read are left out. */
export function parseMekoAgents(data: unknown): MekoAgentList | null {
  if (!isRecord(data) || !isRecord(data.target) || !Array.isArray(data.agents)) return null;
  const { kind, url } = data.target;
  if ((kind !== 'local' && kind !== 'cloud') || typeof url !== 'string') return null;
  const agents: MekoAgent[] = [];
  for (const a of data.agents) {
    if (!isRecord(a) || typeof a.name !== 'string' || typeof a.state !== 'string' || !STATES.includes(a.state)) continue;
    agents.push({
      name: a.name,
      title: typeof a.title === 'string' ? a.title : a.name,
      state: a.state as MekoAgentState,
      url: typeof a.url === 'string' && a.url ? a.url : null,
    });
  }
  return { target: { kind, url }, agents };
}

type Fetch = (url: string, init: { headers: Record<string, string>; signal: AbortSignal }) => Promise<{ ok: boolean; status: number; json(): Promise<unknown> }>;

/** Reads the list from the Meko running on this computer (`local`: its MCP URL, whose origin is its API listener, and client token). */
export async function readMekoAgents(local: LocalMeko | null, fetchImpl: Fetch = fetch): Promise<MekoAgentsRead> {
  const origin = local ? originOf(local.url) : null;
  if (!local || !origin) return { kind: 'failed', error: "Meko isn't running on this computer" };
  let res: Awaited<ReturnType<Fetch>>;
  try {
    res = await fetchImpl(`${origin}/api/v1/agents`, { headers: { Authorization: `Bearer ${local.token}` }, signal: AbortSignal.timeout(TIMEOUT_MS) });
  } catch (e) {
    return { kind: 'failed', error: errMsg(e) };
  }
  if (res.status === 404) return { kind: 'old' };
  if (!res.ok) return { kind: 'failed', error: `Meko answered ${res.status}` };
  const body = (await res.json().catch(() => null)) as { data?: unknown } | null;
  const list = parseMekoAgents(body?.data);
  return list ? { kind: 'list', list } : { kind: 'failed', error: "Meko's answer isn't a list of agents" };
}

/** A harness's `HarnessStatus.meko` in a list; `missing` when Meko didn't find it installed. */
export function harnessMeko(list: MekoAgentList, id: HarnessId): HarnessMeko {
  const a = list.agents.find((x) => x.name === id);
  return a ? { state: a.state, url: a.url } : { state: 'missing', url: null };
}

/**
 * What keeps an agent's `meko` MCP server off Meko, as one sentence (none while connected, or while
 * nothing is gated: `meko` null). `label` names the agent; `target` is where Meko connects agents.
 */
export function mekoMcpGaps(label: string, meko: HarnessMeko | null, target: string | null): string[] {
  switch (meko?.state) {
    case undefined:
    case 'connected':
      return [];
    case 'none':
      return [`${label} has no meko MCP server.`];
    case 'elsewhere':
      return [`${label}'s meko MCP server points at ${mekoUrlHost(meko.url ?? '')}${target ? `, not ${mekoUrlHost(target)}` : ''}.`];
    case 'stale':
      return [`${label}'s meko MCP server needs reconnecting to Meko.`];
    case 'missing':
      return [`Meko didn't find ${label} on this computer.`];
  }
}

/**
 * Meko's list as AgentK last read it, kept in state.db's kv so a restart while Meko is down still
 * gates on it. Read only on events (startup, Settings open, Check again, after an agent's Meko setup,
 * a change of Meko's link, a new session that the list kept would refuse): nothing polls.
 */
export class MekoAgents {
  private list: MekoAgentList | null = null;
  private old = false;
  /** The read under way (or done), which the next one waits for. */
  private chain: Promise<void> = Promise.resolve();
  /** The read that will start after it, which every refresh asked meanwhile shares. */
  private next: Promise<void> | null = null;

  constructor(private o: { db: Db; read: () => Promise<MekoAgentsRead>; changed: () => void; log: Logger }) {
    try {
      this.list = parseMekoAgents(JSON.parse(kvGet(o.db, KV_KEY) ?? 'null'));
    } catch {
      this.list = null;
    }
  }

  /** The list new sessions are gated on, or null when nothing is gated (never read, or Meko too old). */
  current(): MekoAgentList | null {
    return this.old ? null : this.list;
  }

  /** `MekoStatus.agentList`. */
  known(): MekoStatus['agentList'] {
    return this.old ? 'old' : this.list ? 'read' : 'unread';
  }

  /**
   * Reads the list again; a change is pushed (`changed`). Never throws. Reads run one at a time, and
   * one asked for while another runs reads again after it (sharing that read with any other asked
   * meanwhile), so a refresh after Connect never answers with the list from before it.
   */
  refresh(): Promise<void> {
    if (!this.next) {
      const next = this.chain.then(() => {
        this.next = null;
        return this.read();
      });
      this.next = next;
      this.chain = next;
    }
    return this.next;
  }

  private async read(): Promise<void> {
    const before = JSON.stringify([this.old, this.list]);
    let r: MekoAgentsRead;
    try {
      r = await this.o.read();
    } catch (e) {
      r = { kind: 'failed', error: errMsg(e) };
    }
    if (r.kind === 'failed') {
      this.o.log.warn(`reading Meko's list of agents failed${this.list ? '; using the last one read' : ''}`, r.error);
      return;
    }
    try {
      if (r.kind === 'old') {
        this.old = true;
        this.list = null;
        kvDelete(this.o.db, KV_KEY);
      } else {
        this.old = false;
        this.list = r.list;
        kvSet(this.o.db, KV_KEY, JSON.stringify(r.list));
      }
    } catch (e) {
      // The database closed under a read that finished after stop().
      this.o.log.warn("keeping Meko's list of agents failed", errMsg(e));
    }
    if (JSON.stringify([this.old, this.list]) !== before) this.o.changed();
  }
}
