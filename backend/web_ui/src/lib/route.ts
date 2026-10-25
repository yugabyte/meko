// The UI's routes, on the History API: /datapacks/<id>/<tab>, /status, /extensions and /account.
// meko serve answers every such path with index.html. /signin and /setup are the sign-in screens',
// outside the app (main.tsx).

import { useEffect, useState } from "react";

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
  view?: "status" | "extensions" | "account";
}

function parse(path: string): Route {
  const [, root, id, tab, item] = path.split("/");
  if (root === "status" || root === "extensions" || root === "account") return { tab: "overview", view: root };
  if (root !== "datapacks" || !id) return { tab: "overview" };
  return {
    datapackId: id,
    tab: (TABS as readonly string[]).includes(tab) ? (tab as Tab) : "overview",
    itemId: item || undefined,
  };
}

export function href(r: Route): string {
  if (r.view) return `/${r.view}`;
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
