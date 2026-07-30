/**
 * Client-side JIT for Langfuse project keys on tech-preview default datapacks (`${DEFAULT_ACCOUNT_DATAPACK_NAME}*`).
 * Shared by overview (all listed packs), detail (current pack), and the legacy reference page.
 */
import { useEffect } from "react";
import { ensureLangfuseProjectKeys } from "@/lib/api/mekoApi";
import { DEFAULT_ACCOUNT_DATAPACK_NAME } from "@/app/routes/paths";

const LANGFUSE_PROJECT_KEYS_CACHE_KEY = "meko:langfuse-project-keys-cache:v1";
const LANGFUSE_PROJECT_KEYS_TTL_MS = 5 * 60_000;

function isFresh(updatedAt: number, ttlMs: number) {
  return Date.now() - updatedAt <= ttlMs;
}

function readLangfuseProjectKeysCache(): Record<string, number> {
  const raw = sessionStorage.getItem(LANGFUSE_PROJECT_KEYS_CACHE_KEY);
  if (!raw) return {};
  try {
    return JSON.parse(raw) as Record<string, number>;
  } catch {
    clearLangfuseProjectKeysCache();
    return {};
  }
}

function writeLangfuseProjectKeysCache(cache: Record<string, number>) {
  sessionStorage.setItem(LANGFUSE_PROJECT_KEYS_CACHE_KEY, JSON.stringify(cache));
}

/**
 * Drop the per-name "we already provisioned Langfuse keys for this pack" markers.
 * Call this when the datapack list changes so deleted/renamed packs do not linger
 * in the cache and newly-created packs are re-evaluated by the bootstrap flow.
 */
export function clearLangfuseProjectKeysCache(): void {
  sessionStorage.removeItem(LANGFUSE_PROJECT_KEYS_CACHE_KEY);
}

/** Same naming rule as legacy Datapacks: default / tech-preview packs get Langfuse JIT keys. */
export function isTechPreviewDefaultDatapackName(name: string): boolean {
  return name === DEFAULT_ACCOUNT_DATAPACK_NAME || name.startsWith(`${DEFAULT_ACCOUNT_DATAPACK_NAME}_`);
}

/**
 * Best-effort POST …/langfuse/project-keys for tech-preview default packs, with a
 * per-id sessionStorage TTL so we do not hammer the API. Errors are console-only.
 */
export function scheduleEnsureLangfuseProjectKeysForDatapack(datapackName: string, datapackId: string): void {
  if (!datapackName.trim() || !isTechPreviewDefaultDatapackName(datapackName)) {
    return;
  }
  const cache = readLangfuseProjectKeysCache();
  const updatedAt = cache[datapackId];
  if (updatedAt && isFresh(updatedAt, LANGFUSE_PROJECT_KEYS_TTL_MS)) {
    return;
  }

  void ensureLangfuseProjectKeys(datapackId, {})
    .then(() => {
      const next = readLangfuseProjectKeysCache();
      next[datapackId] = Date.now();
      writeLangfuseProjectKeysCache(next);
    })
    .catch((err) => {
      console.warn("Langfuse project provisioning failed:", err);
    });
}

/**
 * Runs {@link scheduleEnsureLangfuseProjectKeysForDatapack} when `datapackName` changes,
 * but only when the current user is the datapack owner.  Pass `isOwner=true` once the
 * datapack response is available; `null/undefined` means "not yet known" and is treated
 * as a no-op so shared recipients never hit the owner-only endpoint.
 */
export function useEnsureLangfuseProjectKeysForTechPreviewDatapack(
  datapackName: string,
  datapackId: string,
  isOwner?: boolean | null,
): void {
  useEffect(() => {
    // Skip until we know the grant status, and skip for non-owners.
    if (isOwner !== true) return;
    scheduleEnsureLangfuseProjectKeysForDatapack(datapackName, datapackId);
  }, [datapackName, datapackId, isOwner]);
}
