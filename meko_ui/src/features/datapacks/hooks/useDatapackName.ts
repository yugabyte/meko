// Resolves a datapack's display name from its stable UUID. Used by
// routes that only carry `:datapack_id` in the URL and need the
// human-readable name for breadcrumbs / headers without otherwise
// loading the full datapack. Returns "" until the lookup resolves
// (callers render a blank label rather than leaking the UUID).
//
// Results are cached per id and concurrent lookups are de-duplicated, so
// mounting several consumers for the same datapack (or navigating between
// its sub-pages) issues at most one request.
import { useEffect, useState } from "react";
import { getDatapack } from "@/lib/api/mekoApi";

const nameCache = new Map<string, string>();
const inFlight = new Map<string, Promise<string>>();

/** Seed/refresh the cached name for a datapack — call after a successful
 *  rename so consumers don't show a stale name until a full reload. */
export function setCachedDatapackName(datapackId: string, name: string): void {
  nameCache.set(datapackId, name);
}

function fetchDatapackName(datapackId: string): Promise<string> {
  const existing = inFlight.get(datapackId);
  if (existing) return existing;

  const request = getDatapack(datapackId)
    .then((dp) => {
      const name = dp.datapack_name ?? "";
      nameCache.set(datapackId, name);
      return name;
    })
    // Cache the failure (as "") too, so a deleted/unreachable id doesn't
    // re-hit the backend on every remount.
    .catch(() => {
      nameCache.set(datapackId, "");
      return "";
    })
    .finally(() => {
      inFlight.delete(datapackId);
    });

  inFlight.set(datapackId, request);
  return request;
}

export function useDatapackName(datapackId: string): string {
  const [name, setName] = useState(() => nameCache.get(datapackId) ?? "");

  // Derived-state-during-render: the useState initializer only runs on mount,
  // so without this the hook would return the previous datapack's name for one
  // render after datapackId changes. Resync synchronously to avoid that flash.
  const [prevId, setPrevId] = useState(datapackId);
  if (datapackId !== prevId) {
    setPrevId(datapackId);
    setName(nameCache.get(datapackId) ?? "");
  }

  useEffect(() => {
    if (!datapackId) {
      setName("");
      return;
    }
    const cached = nameCache.get(datapackId);
    if (cached !== undefined) {
      setName(cached);
      return;
    }

    setName("");
    let active = true;
    void fetchDatapackName(datapackId).then((resolved) => {
      if (active) setName(resolved);
    });
    return () => {
      active = false;
    };
  }, [datapackId]);

  return name;
}
