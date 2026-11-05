// The Meko Cloud link as the sidebar uses it: whether it's linked (from Settings › Meko Cloud's
// route), read again when Settings › Meko Cloud links or unlinks (a window event), and how a row of
// GET /datapacks opens.

import { useEffect } from "react";
import { api } from "./api";
import type { Route } from "./route";
import type { Datapack } from "./types";
import { useLoad, type Loaded } from "./useLoad";

const EVENT = "meko:cloud-changed";

// The sidebar shows this many datapacks until Show all is pressed.
export const ROWS_SHOWN = 50;

export function notifyCloudChanged(): void {
  window.dispatchEvent(new Event(EVENT));
}

// Runs fn when Settings › Meko Cloud links or unlinks.
export function useCloudChanged(fn: () => void): void {
  useEffect(() => {
    window.addEventListener(EVENT, fn);
    return () => window.removeEventListener(EVENT, fn);
  }, [fn]);
}

// Whether Meko Cloud is linked. A link that can't be read reads as unlinked.
export function useCloudLinked(): Loaded<boolean> {
  const linked = useLoad(
    () =>
      api.mekoCloud().then(
        (c) => c.linked,
        () => false,
      ),
    [],
  );
  useCloudChanged(linked.reload);
  return linked;
}

// Where a datapack's row opens: here (this UI's own pages), on the web (a full URL, in a new tab), or
// nowhere (a web app that isn't known). A row without web_url is from a server older than the UI,
// whose rows are all its own.
export type RowLink = { kind: "here" } | { kind: "web"; url: string } | { kind: "none" };

export function rowLink(d: Datapack): RowLink {
  if (d.web_url === undefined || d.web_url?.startsWith("/")) return { kind: "here" };
  if (d.web_url === null || d.web_url === "") return { kind: "none" };
  return { kind: "web", url: d.web_url };
}

// Where one of this UI's datapack addresses goes when the listing opens that datapack in Meko Cloud's
// web app: extensions link every datapack at /datapacks/<id>, whichever side has it. Its Context Gaps
// go to this UI's page for a Meko Cloud datapack's (/gaps/<id>), any other tab to the row's web_url.
// Null while it opens here, or the listing doesn't have it (yet).
export function sentOn(route: Route, rows: Datapack[] | undefined): { route: Route } | { url: string } | null {
  if (route.view || !route.datapackId) return null;
  const d = rows?.find((r) => r.datapack_id === route.datapackId);
  const link = d ? rowLink(d) : null;
  if (link?.kind !== "web") return null;
  return route.tab === "gaps" ? { route: { tab: "gaps", view: "cloud-gaps", datapackId: route.datapackId } } : { url: link.url };
}

// Whether a datapack's name holds the sidebar's Find a datapack query, ignoring case; an empty
// query holds every name.
export function matchesName(name: string, query: string): boolean {
  return name.toLowerCase().includes(query.trim().toLowerCase());
}

// A datapack shared with the user rather than their own.
export function isShared(grant: string | undefined): boolean {
  return !!grant && grant !== "owner";
}
