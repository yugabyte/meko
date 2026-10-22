// The app's keyboard shortcuts: "/" jumps to the open datapack's Search tab and puts the cursor in
// its box. Esc closes a dialog (Dialog handles it). Keys typed into a field are left alone.

import { useEffect } from "react";
import { navigate } from "./route";

export const SEARCH_INPUT_ID = "search-input";

export function useShortcuts(datapackId: string | undefined) {
  useEffect(() => {
    const on = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null;
      const typing = el?.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el?.tagName ?? "");
      if (e.key !== "/" || typing || e.metaKey || e.ctrlKey || e.altKey || !datapackId) return;
      e.preventDefault();
      navigate({ datapackId, tab: "search" });
      // The Search page mounts on navigate; focus its box once it has.
      setTimeout(() => document.getElementById(SEARCH_INPUT_ID)?.focus(), 0);
    };
    window.addEventListener("keydown", on);
    return () => window.removeEventListener("keydown", on);
  }, [datapackId]);
}
