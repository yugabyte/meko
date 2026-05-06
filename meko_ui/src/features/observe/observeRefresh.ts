import { dispatchAppEvent } from "@/lib/browserEvents";
import { DATAPACKS_LIST_UPDATED_EVENT } from "@/lib/mekoApi";
import { clearLangfuseProjectKeysCache } from "@/lib/langfuseProjectKeysBootstrap";

export const OBSERVE_BOOTSTRAP_CACHE_KEY = "meko:observe-bootstrap-cache:v1";
export const OBSERVE_BOOTSTRAP_CACHE_TTL_MS = 5 * 60_000;
export const OBSERVE_IFRAME_VERSION_STORAGE_KEY = "meko:observe-iframe-version:v1";

export type ObserveBootstrapCache = {
  langfuseRelPath: string;
  updatedAt: number;
};

export function readObserveBootstrapCache(): ObserveBootstrapCache | null {
  try {
    const raw = sessionStorage.getItem(OBSERVE_BOOTSTRAP_CACHE_KEY);
    if (!raw) return null;
    return JSON.parse(raw) as ObserveBootstrapCache;
  } catch {
    try {
      sessionStorage.removeItem(OBSERVE_BOOTSTRAP_CACHE_KEY);
    } catch {
      // ignore
    }
    return null;
  }
}

export function writeObserveBootstrapCache(cache: ObserveBootstrapCache): void {
  try {
    sessionStorage.setItem(OBSERVE_BOOTSTRAP_CACHE_KEY, JSON.stringify(cache));
  } catch {}
}

export function isObserveBootstrapFresh(updatedAt: number): boolean {
  return Date.now() - updatedAt <= OBSERVE_BOOTSTRAP_CACHE_TTL_MS;
}

export function clearObserveBootstrapCache(): void {
  try {
    sessionStorage.removeItem(OBSERVE_BOOTSTRAP_CACHE_KEY);
  } catch {}
}

export function getObserveIframeVersion(): number {
  try {
    const raw = localStorage.getItem(OBSERVE_IFRAME_VERSION_STORAGE_KEY);
    if (!raw) return 0;
    const n = Number.parseInt(raw, 10);
    return Number.isFinite(n) && n >= 0 ? n : 0;
  } catch {
    return 0;
  }
}

function bumpObserveIframeVersion(): number {
  const next = getObserveIframeVersion() + 1;
  try {
    localStorage.setItem(OBSERVE_IFRAME_VERSION_STORAGE_KEY, String(next));
  } catch {}
  return next;
}

export function notifyDatapacksListChanged(reason: string): void {
  void reason;
  bumpObserveIframeVersion();
  clearObserveBootstrapCache();
  clearLangfuseProjectKeysCache();
  dispatchAppEvent(DATAPACKS_LIST_UPDATED_EVENT);
}
