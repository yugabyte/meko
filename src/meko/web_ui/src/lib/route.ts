// The UI's routes, on the History API: /datapacks/<id>/<tab>, /projects, /projects/<id>, /status,
// /extensions, /settings, /settings/meko-cloud and /calls (its filters in the query: callFilter).
// meko serve answers every such path with index.html. The gate (Gate.tsx) sends the old sign-in and
// account addresses (/signin, /setup, /account) on into these (lib/auth).

import { useCallback, useEffect, useState } from "react";
import type { CallFilter } from "./types";

export const TABS = [
  "overview",
  "metrics",
  "conversations",
  "memories",
  "knowledge",
  "artifacts",
  "search",
  "gaps",
  "connect",
] as const;
export type Tab = (typeof TABS)[number];

export interface Route {
  datapackId?: string;
  tab: Tab;
  // A conversation open in the conversations tab, or the memories tab's scope.
  itemId?: string;
  // A page of the whole install rather than of a datapack.
  view?: "status" | "extensions" | "settings" | "meko-cloud" | "cloud-gaps" | "projects" | "project" | "calls";
  // The project open on the project view.
  projectId?: string;
  // Something on the page to scroll to and point out, as the address's #fragment: a conversation's
  // turn, by its message id.
  focus?: string;
}

export function parse(path: string): Route {
  const [, root, id, tab, item] = path.split("/");
  if (root === "status" || root === "extensions" || root === "calls") return { tab: "overview", view: root };
  if (root === "settings" && id === "meko-cloud") return { tab: "overview", view: "meko-cloud" };
  if (root === "settings" && !id) return { tab: "overview", view: "settings" };
  if (root === "projects") return id ? { tab: "overview", view: "project", projectId: id } : { tab: "overview", view: "projects" };
  // A Meko Cloud datapack's Context Gaps: its other pages are Meko Cloud's web app's.
  if (root === "gaps" && id) return { tab: "gaps", view: "cloud-gaps", datapackId: id };
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
  return ["", "datapacks", "status", "extensions", "settings", "gaps", "projects", "calls"].includes(root);
}

export function href(r: Route): string {
  if (r.view === "meko-cloud") return "/settings/meko-cloud";
  if (r.view === "cloud-gaps") return `/gaps/${r.datapackId ?? ""}`;
  if (r.view === "project") return `/projects/${r.projectId ?? ""}`;
  if (r.view) return `/${r.view}`;
  if (!r.datapackId) return "/";
  return `/datapacks/${r.datapackId}/${r.tab}${r.itemId ? `/${r.itemId}` : ""}${r.focus ? `#${r.focus}` : ""}`;
}

export const PROJECTS: Route = { tab: "overview", view: "projects" };
export const project = (projectId: string): Route => ({ tab: "overview", view: "project", projectId });

// The Settings page, which the sidebar and the "it's off" notes link to.
export const SETTINGS: Route = { tab: "overview", view: "settings" };

// The Calls page, which Settings links to; its filters are in the address (/calls?agent=<agent id>).
export const CALLS: Route = { tab: "overview", view: "calls" };

// The Calls page's filters, from its address's query.
export function callFilter(search: string): CallFilter {
  const q = new URLSearchParams(search);
  return {
    agent: q.get("agent") || undefined,
    tool: q.get("tool") || undefined,
    client: q.get("client") || undefined,
    failed: q.get("failed") === "true" || undefined,
  };
}

// The Calls page's address for filters.
export function callsHref(f: CallFilter): string {
  const q = new URLSearchParams();
  if (f.agent) q.set("agent", f.agent);
  if (f.tool) q.set("tool", f.tool);
  if (f.client) q.set("client", f.client);
  if (f.failed) q.set("failed", "true");
  const qs = q.toString();
  return `/calls${qs ? `?${qs}` : ""}`;
}

const EVENT = "meko:navigate";

export function navigate(r: Route): void {
  history.pushState(null, "", href(r));
  window.dispatchEvent(new Event(EVENT));
}

// Like navigate, but in place of the address open now, so Back doesn't return to it.
export function redirect(r: Route): void {
  history.replaceState(null, "", href(r));
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
