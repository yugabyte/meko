// The extensions list the sidebar and the Extensions page share: loaded once, polled every 2 s while
// an action runs, and reloaded in every view when an action starts or ends (a window event).

import { useEffect, useRef } from "react";
import { api } from "./api";
import type { Extension } from "./types";
import { useLoad, type Loaded } from "./useLoad";

const EVENT = "meko:extensions";
const POLL_MS = 2000;

export function notifyExtensionsChanged(): void {
  window.dispatchEvent(new Event(EVENT));
}

// An older server lists only the extensions it serves, all installed.
export function isInstalled(e: Extension): boolean {
  return e.installed ?? true;
}

export function useExtensions(): Loaded<Extension[]> {
  const list = useLoad(() => api.extensions(), []);
  const { reload } = list;
  useEffect(() => {
    window.addEventListener(EVENT, reload);
    return () => window.removeEventListener(EVENT, reload);
  }, [reload]);
  const busy = list.data?.some((e) => e.job?.state === "running") ?? false;
  useEffect(() => {
    if (!busy) return;
    const t = setInterval(reload, POLL_MS);
    return () => clearInterval(t);
  }, [busy, reload]);
  // An action that ended may have changed what the other views show.
  const wasBusy = useRef(false);
  useEffect(() => {
    if (wasBusy.current && !busy) notifyExtensionsChanged();
    wasBusy.current = busy;
  }, [busy]);
  return list;
}
