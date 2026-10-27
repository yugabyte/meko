// Meko's own updates in the browser: the status the sidebar's chip and the Status page's Updates
// card share (reloaded in both when either changes it, a window event), and the update this tab
// asked for, remembered across the page's reload into the new version.

import { useEffect } from "react";
import { api } from "./api";
import type { UpdateStatus } from "./types";
import { useLoad, type Loaded } from "./useLoad";

const EVENT = "meko:update";
// The sidebar's check of what the server last found; the server itself checks every few hours.
const SIDEBAR_POLL_MS = 30 * 60_000;
const TARGET_KEY = "meko:update-target";

export function notifyUpdateChanged(): void {
  window.dispatchEvent(new Event(EVENT));
}

// The update status, reloaded when a view changes it, and every poll ms when given.
export function useUpdateStatus(poll?: number): Loaded<UpdateStatus> {
  const status = useLoad(() => api.update(), []);
  const { reload } = status;
  useEffect(() => {
    window.addEventListener(EVENT, reload);
    return () => window.removeEventListener(EVENT, reload);
  }, [reload]);
  useEffect(() => {
    if (!poll) return;
    const t = setInterval(reload, poll);
    return () => clearInterval(t);
  }, [poll, reload]);
  return status;
}

// Whether a newer release is out, for the sidebar. An older server, without the route, says no.
export function useUpdateAvailable(): UpdateStatus | undefined {
  const s = useUpdateStatus(SIDEBAR_POLL_MS).data;
  return s?.release && s.available && !s.pending ? s : undefined;
}

// The update this tab is restarting Meko into.
export interface UpdateTarget {
  from: string;
  to: string;
}

export function saveTarget(t: UpdateTarget): void {
  try {
    sessionStorage.setItem(TARGET_KEY, JSON.stringify(t));
  } catch {
    // Without storage the page just doesn't say how the update went after the reload.
  }
}

// The update asked for before the page reloaded; clearTarget forgets it once the page has read it.
export function readTarget(): UpdateTarget | null {
  try {
    const raw = sessionStorage.getItem(TARGET_KEY);
    const t = raw ? (JSON.parse(raw) as UpdateTarget) : null;
    return t && typeof t.from === "string" && typeof t.to === "string" ? t : null;
  } catch {
    return null;
  }
}

export function clearTarget(): void {
  try {
    sessionStorage.removeItem(TARGET_KEY);
  } catch {
    // Nothing to forget.
  }
}
