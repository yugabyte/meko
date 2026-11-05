/**
 * Connecting an agent to Meko. Meko owns the agents' connections: the page posts Meko's own route,
 * root-absolute on Meko's origin (not under BASE) with its sign-in, as `mekoupdate.ts` does for
 * updates (`POST /api/v1/agents/:id/connect`, in Meko's `{ data, error }` envelope), then asks AgentK
 * to read Meko's list again (`POST /api/harnesses/refresh`). AgentK's server never posts to Meko.
 * The page also connects Kiro and OpenCode by itself, once each (`autoConnectToMeko`).
 */
import { HARNESS_CAPS, type HarnessId } from '../shared/harnesses';
import type { HarnessMeko, HarnessStatus } from '../shared/types';
import { api } from './api';

/** What a Meko too old to list or connect agents (404) means. */
export const UPDATE_MEKO_AGENTS = 'Update Meko (meko update) to manage agent connections.';

/** Meko can connect it: it's installed, and its `meko` MCP server isn't Meko's target. */
export function connectable(m: HarnessMeko | null | undefined): boolean {
  return m?.state === 'none' || m?.state === 'stale' || m?.state === 'elsewhere';
}

/** Connects an agent's `meko` MCP server through Meko's route; Meko's notes (a server it replaced), if any. */
export async function connectAgent(id: HarnessId): Promise<string[]> {
  let res: Response;
  try {
    res = await fetch(`/api/v1/agents/${id}/connect`, { method: 'POST', credentials: 'same-origin' });
  } catch {
    throw new Error('Can’t reach Meko.');
  }
  const body = (await res.json().catch(() => null)) as { data?: { notes?: unknown }; error?: { message?: unknown } } | null;
  if (!res.ok) {
    if (res.status === 404 && !body?.error) throw new Error(UPDATE_MEKO_AGENTS);
    const msg = body?.error?.message;
    throw new Error(typeof msg === 'string' && msg ? msg : `Meko answered ${res.status}.`);
  }
  const notes = body?.data?.notes;
  return Array.isArray(notes) ? notes.filter((n): n is string => typeof n === 'string') : [];
}

/** Connect to Meko: Meko connects it, then AgentK reads Meko's list (and the versions) again. */
export async function connectToMeko(id: HarnessId): Promise<{ notes: string[]; harnesses: HarnessStatus[] }> {
  const notes = await connectAgent(id);
  return { notes, harnesses: await api.refreshHarnesses() };
}

/**
 * Connects Kiro or OpenCode to Meko by itself (`HarnessStatus.autoConnect`), once per agent: the server
 * says whether this page is the one to (`go`, recorded there, so another page, or a later disconnect,
 * never repeats it), then Connect to Meko's own steps. The toast's line, or null when another page
 * claimed it or it's no longer due; a failure throws with the fix, and isn't retried.
 */
export async function autoConnectToMeko(id: HarnessId): Promise<string | null> {
  if (!(await api.claimAutoConnect(id)).go) return null;
  const label = HARNESS_CAPS[id].label;
  let notes: string[];
  try {
    notes = await connectAgent(id);
  } catch (e) {
    const why = (e instanceof Error ? e.message : String(e)).replace(/\.$/, '');
    throw new Error(`Couldn’t connect ${label} to Meko: ${why}. Connect it in Settings › Harness.`, { cause: e });
  }
  // Connected: a refresh that fails (AgentK restarting) isn't a failed connect; the next push says it.
  await api.refreshHarnesses().catch(() => undefined);
  return [`Connected ${label} to Meko.`, ...notes].join(' ');
}

/**
 * Set up Claude Code or Kiro with this Meko: Meko connects its MCP server first when it isn't, then
 * AgentK's setup route runs Meko's setup package for what Meko's capture needs (nothing when that's there).
 */
export async function setUpWithMeko(id: 'claude' | 'kiro', meko: HarnessMeko | null | undefined): Promise<void> {
  if (connectable(meko)) await connectAgent(id);
  await (id === 'claude' ? api.mekoClaudeSetup() : api.mekoKiroSetup());
}
