/**
 * Connecting an agent to Meko. Meko owns the agents' connections: the page posts Meko's own route,
 * root-absolute on Meko's origin (not under BASE) with its sign-in, as `mekoupdate.ts` does for
 * updates (`POST /api/v1/agents/:id/connect`, in Meko's `{ data, error }` envelope), then asks AgentK
 * to read Meko's list again (`POST /api/harnesses/refresh`). AgentK's server never posts to Meko.
 */
import type { HarnessId } from '../shared/harnesses';
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
 * Set up Claude Code or Kiro with this Meko: Meko connects its MCP server first when it isn't, then
 * AgentK's setup route runs Meko's setup package for what Meko's capture needs (nothing when that's there).
 */
export async function setUpWithMeko(id: 'claude' | 'kiro', meko: HarnessMeko | null | undefined): Promise<void> {
  if (connectable(meko)) await connectAgent(id);
  await (id === 'claude' ? api.mekoClaudeSetup() : api.mekoKiroSetup());
}
