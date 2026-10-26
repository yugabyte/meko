// The linked Meko Cloud's datapacks, for the sidebar: loaded once, and again when Settings › Meko
// Cloud links or unlinks (a window event) or the sidebar's refresh asks.

import { useEffect } from "react";
import { api, ApiError } from "./api";
import type { CloudDatapacks } from "./types";
import { useLoad, type Loaded } from "./useLoad";

const EVENT = "meko:cloud-changed";

// The sidebar shows this many Cloud rows until Show all is pressed.
export const CLOUD_ROWS_SHOWN = 50;

export function notifyCloudChanged(): void {
  window.dispatchEvent(new Event(EVENT));
}

// Whether a listing failed at Meko Cloud: only the route's 502s carry a CLOUD_ code, and they mean a
// link exists. Any other error (a fetch that failed while Meko restarts, an unreadable link) leaves
// an unlinked sidebar as it was.
export function isCloudFailure(e: Error | undefined): boolean {
  return e instanceof ApiError && e.code.startsWith("CLOUD_");
}

// A Meko Cloud datapack shared with the account rather than its own.
export function isShared(grant: string): boolean {
  return grant !== "" && grant !== "owner";
}

export function useCloudDatapacks(): Loaded<CloudDatapacks> {
  // A server from before the route answers 404: the sidebar stays as it was, unlinked.
  const list = useLoad(
    () =>
      api.cloudDatapacks().catch((e: unknown) => {
        if (e instanceof ApiError && e.status === 404) return { linked: false, datapacks: [] };
        throw e;
      }),
    [],
  );
  const { reload } = list;
  useEffect(() => {
    window.addEventListener(EVENT, reload);
    return () => window.removeEventListener(EVENT, reload);
  }, [reload]);
  return list;
}
