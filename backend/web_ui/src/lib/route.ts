// The UI's routes, on the History API: /datapacks/<id>/<tab>, /status, /extensions, /account and
// /settings/meko-cloud.
// meko serve answers every such path with index.html. /signin and /setup are the sign-in screens',
// outside the app (main.tsx).

import { useCallback, useEffect, useState } from "react";

export const TABS = ["overview", "analytics", "search", "memories", "knowledge", "conversations", "artifacts", "connect"] as const;
export type Tab = (typeof TABS)[number];

export interface Route {
  datapackId?: string;
  tab: Tab;
  // A conversation open in the conversations tab, or the memories tab's scope.
  itemId?: string;
  // A page of the whole install rather than of a datapack.
  view?: "status" | "extensions" | "account" | "meko-cloud";
}

function parse(path: string): Route {
  const [, root, id, tab, item] = path.split("/");
  if (root === "status" || root === "extensions" || root === "account") return { tab: "overview", view: root };
  if (root === "settings" && id === "meko-cloud") return { tab: "overview", view: "meko-cloud" };
  if (root !== "datapacks" || !id) return { tab: "overview" };
  return {
    datapackId: id,
    tab: (TABS as readonly string[]).includes(tab) ? (tab as Tab) : "overview",
    itemId: item || undefined,
  };
}

export function href(r: Route): string {
  if (r.view === "meko-cloud") return "/settings/meko-cloud";
  if (r.view) return `/${r.view}`;
  if (!r.datapackId) return "/";
  return `/datapacks/${r.datapackId}/${r.tab}${r.itemId ? `/${r.itemId}` : ""}`;
}

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
