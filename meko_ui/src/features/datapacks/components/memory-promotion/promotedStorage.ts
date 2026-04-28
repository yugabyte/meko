// Persists which memories the user already promoted to KB, keyed by account id + datapack id in localStorage.
// Stores full Memory objects so promoted rows can render even when they are not on the current API page.

import type { Memory } from "@/lib/mekoApi";

/** Bumped when persisted {@link Memory} JSON shape changes (v3: flat `data` / `agent_id`). */
export const PROMOTED_STORAGE_PREFIX = "meko:promoted-memories:v3";

export type PromotedMemory = {
  memory: Memory;
  promotedAt: number;
};

export function promotedStorageKey(accountId: string, datapackId: string): string {
  return `${PROMOTED_STORAGE_PREFIX}:${accountId}:${datapackId}`;
}

export function readPromotedCache(accountId: string, datapackId: string): Map<string, PromotedMemory> {
  if (!accountId || !datapackId) return new Map();
  try {
    const raw = localStorage.getItem(promotedStorageKey(accountId, datapackId));
    if (!raw) return new Map();
    const parsed = JSON.parse(raw);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      const out = new Map<string, PromotedMemory>();
      for (const [id, entry] of Object.entries(parsed as Record<string, unknown>)) {
        if (
          entry &&
          typeof entry === "object" &&
          "memory" in entry &&
          typeof (entry as { memory?: { id?: unknown } }).memory?.id === "string"
        ) {
          const e = entry as PromotedMemory;
          out.set(id, {
            memory: e.memory,
            promotedAt: typeof e.promotedAt === "number" ? e.promotedAt : Date.now(),
          });
        }
      }
      return out;
    }
  } catch {
    localStorage.removeItem(promotedStorageKey(accountId, datapackId));
  }
  return new Map();
}

export function writePromotedCache(accountId: string, datapackId: string, cache: Map<string, PromotedMemory>) {
  if (!accountId || !datapackId) return;
  const obj: Record<string, PromotedMemory> = {};
  cache.forEach((entry, id) => {
    obj[id] = entry;
  });
  localStorage.setItem(promotedStorageKey(accountId, datapackId), JSON.stringify(obj));
}
