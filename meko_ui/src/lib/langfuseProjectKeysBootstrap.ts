/**
 * Client-side JIT for Langfuse project keys on tech-preview default datapacks (`meko_default_datapack*`).
 * Shared by overview (all listed packs), detail (current pack), and the legacy reference page.
 */
import { useEffect } from "react";
import { ensureLangfuseProjectKeys } from "@/lib/mekoApi";

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
    sessionStorage.removeItem(LANGFUSE_PROJECT_KEYS_CACHE_KEY);
    return {};
  }
}

function writeLangfuseProjectKeysCache(cache: Record<string, number>) {
  sessionStorage.setItem(LANGFUSE_PROJECT_KEYS_CACHE_KEY, JSON.stringify(cache));
}

/** Same naming rule as legacy Datapacks: default / tech-preview packs get Langfuse JIT keys. */
export function isTechPreviewDefaultDatapackName(name: string): boolean {
  return name === "meko_default_datapack" || name.startsWith("meko_default_datapack_");
}

/**
 * Best-effort POST …/langfuse/project-keys for tech-preview default packs, with a
 * per-name sessionStorage TTL so we do not hammer the API. Errors are console-only.
 */
export function scheduleEnsureLangfuseProjectKeysForDatapack(datapackName: string): void {
  if (!datapackName.trim() || !isTechPreviewDefaultDatapackName(datapackName)) {
    return;
  }
  const cache = readLangfuseProjectKeysCache();
  const updatedAt = cache[datapackName];
  if (updatedAt && isFresh(updatedAt, LANGFUSE_PROJECT_KEYS_TTL_MS)) {
    return;
  }

  void ensureLangfuseProjectKeys(datapackName)
    .then(() => {
      const next = readLangfuseProjectKeysCache();
      next[datapackName] = Date.now();
      writeLangfuseProjectKeysCache(next);
    })
    .catch((err) => {
      console.warn("Langfuse project provisioning failed:", err);
    });
}

/** Runs {@link scheduleEnsureLangfuseProjectKeysForDatapack} when `datapackName` changes. */
export function useEnsureLangfuseProjectKeysForTechPreviewDatapack(datapackName: string): void {
  useEffect(() => {
    scheduleEnsureLangfuseProjectKeysForDatapack(datapackName);
  }, [datapackName]);
}
