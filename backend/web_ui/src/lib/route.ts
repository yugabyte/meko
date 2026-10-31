// The UI's routes, on the History API: /datapacks/<id>/<tab>, /status, /extensions, /settings and
// /settings/meko-cloud.
// meko serve answers every such path with index.html. The gate (Gate.tsx) sends the old sign-in and
// account addresses (/signin, /setup, /account) on into these (lib/auth).

import { useCallback, useEffect, useState } from "react";

export const TABS = [
  "overview",
  "metrics",
  "conversations",
  "memories",
  "knowledge",
  "artifacts",
  "search",
  "connect",
] as const;
export type Tab = (typeof TABS)[number];

export interface Route {
  datapackId?: string;
  tab: Tab;
  // A conversation open in the conversations tab, or the memories tab's scope.
  itemId?: string;
  // A page of the whole install rather than of a datapack.
  view?: "status" | "extensions" | "settings" | "meko-cloud";
  // Something on the page to scroll to and point out, as the address's #fragment: a conversation's
  // turn, by its message id.
  focus?: string;
}

function parse(path: string): Route {
  const [, root, id, tab, item] = path.split("/");
  if (root === "status" || root === "extensions") return { tab: "overview", view: root };
  if (root === "settings" && id === "meko-cloud") return { tab: "overview", view: "meko-cloud" };
  if (root === "settings" && !id) return { tab: "overview", view: "settings" };
  if (root !== "datapacks" || !id) return { tab: "overview" };
  return {
    datapackId: id,
    tab: (TABS as readonly string[]).includes(tab) ? (tab as Tab) : "overview",
    itemId: item || undefined,
  };
}

// Whether path is one of the app's own pages, which it routes itself; anything else on this origin
// (an extension's page, /ext/<name>/) is a page of its own, for the browser to load.
export function isAppPath(path: string): boolean {
  const root = path.split("/")[1];
  return ["", "datapacks", "status", "extensions", "settings"].includes(root);
}

export function href(r: Route): string {
  if (r.view === "meko-cloud") return "/settings/meko-cloud";
  if (r.view) return `/${r.view}`;
  if (!r.datapackId) return "/";
  return `/datapacks/${r.datapackId}/${r.tab}${r.itemId ? `/${r.itemId}` : ""}${r.focus ? `#${r.focus}` : ""}`;
}

// The Settings page, which the sidebar and the "it's off" notes link to.
export const SETTINGS: Route = { tab: "overview", view: "settings" };

const EVENT = "meko:navigate";

export function navigate(r: Route): void {
  history.pushState(null, "", href(r));
  window.dispatchEvent(new Event(EVENT));
}

// Calls on after every navigation, back and forward included, even one to the page already open.
// on must be stable (a state setter, or a useCallback).
export function useNavigated(on: () => void): void {
  useEffect(() => {
    window.addEventListener("popstate", on);
    window.addEventListener(EVENT, on);
    return () => {
      window.removeEventListener("popstate", on);
      window.removeEventListener(EVENT, on);
    };
  }, [on]);
}

export function useRoute(): Route {
  const [route, setRoute] = useState(() => parse(window.location.pathname));
  useNavigated(useCallback(() => setRoute(parse(window.location.pathname)), []));
  return route;
}
