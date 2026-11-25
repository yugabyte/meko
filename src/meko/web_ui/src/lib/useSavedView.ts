// A choice remembered in this browser, such as a page's List or Graph view, so a reload or a scope
// change keeps the one last picked.

import { useState } from "react";

export type View = "list" | "graph";

export const VIEWS: readonly { id: View; label: string }[] = [
  { id: "list", label: "List" },
  { id: "graph", label: "Graph" },
];

// One of ids, remembered in this browser under key; fallback until one is picked.
export function useSaved<T extends string>(key: string, ids: readonly T[], fallback: T): [T, (v: T) => void] {
  const [value, setValue] = useState<T>(() => {
    try {
      const v = localStorage.getItem(key) as T | null;
      return v !== null && ids.includes(v) ? v : fallback;
    } catch {
      return fallback;
    }
  });
  const save = (v: T) => {
    setValue(v);
    try {
      localStorage.setItem(key, v);
    } catch {
      // Not remembered: the page still works.
    }
  };
  return [value, save];
}

export const useSavedView = (key: string) => useSaved<View>(key, ["list", "graph"], "list");
