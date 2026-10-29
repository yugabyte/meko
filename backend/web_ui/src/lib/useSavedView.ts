// A page's List or Graph view, remembered in this browser per page, so a reload or a scope change
// keeps the one last picked.

import { useState } from "react";

export type View = "list" | "graph";

export const VIEWS: readonly { id: View; label: string }[] = [
  { id: "list", label: "List" },
  { id: "graph", label: "Graph" },
];

export function useSavedView(key: string): [View, (v: View) => void] {
  const [view, setView] = useState<View>(() => {
    try {
      return localStorage.getItem(key) === "graph" ? "graph" : "list";
    } catch {
      return "list";
    }
  });
  const save = (v: View) => {
    setView(v);
    try {
      localStorage.setItem(key, v);
    } catch {
      // Not remembered: the page still works.
    }
  };
  return [view, save];
}
