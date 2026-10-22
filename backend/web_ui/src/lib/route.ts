// The UI's routes, on the History API: /datapacks/<id>/<tab>. meko serve answers every such path
// with index.html.

import { useEffect, useState } from "react";

export const TABS = ["overview", "analytics", "search", "memories", "knowledge", "conversations", "artifacts", "connect"] as const;
export type Tab = (typeof TABS)[number];

export interface Route {
  datapackId?: string;
  tab: Tab;
  // A conversation open in the conversations tab.
  itemId?: string;
}

function parse(path: string): Route {
  const [, root, id, tab, item] = path.split("/");
  if (root !== "datapacks" || !id) return { tab: "overview" };
  return {
    datapackId: id,
    tab: (TABS as readonly string[]).includes(tab) ? (tab as Tab) : "overview",
    itemId: item || undefined,
  };
}

export function href(r: Route): string {
  if (!r.datapackId) return "/";
  return `/datapacks/${r.datapackId}/${r.tab}${r.itemId ? `/${r.itemId}` : ""}`;
}

const EVENT = "meko:navigate";

export function navigate(r: Route): void {
  history.pushState(null, "", href(r));
  window.dispatchEvent(new Event(EVENT));
}

export function useRoute(): Route {
  const [route, setRoute] = useState(() => parse(window.location.pathname));
  useEffect(() => {
    const on = () => setRoute(parse(window.location.pathname));
    window.addEventListener("popstate", on);
    window.addEventListener(EVENT, on);
    return () => {
      window.removeEventListener("popstate", on);
      window.removeEventListener(EVENT, on);
    };
  }, []);
  return route;
}
