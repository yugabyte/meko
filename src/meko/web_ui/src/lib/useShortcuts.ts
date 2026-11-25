// The app's keyboard shortcuts: "/" jumps to the open datapack's Search tab (from the Status page,
// the first datapack's) and puts the cursor in its box. Esc closes a dialog (Dialog handles it).
// Keys typed into a field, or while a dialog is open, are left alone.

import { useEffect } from "react";
import { navigate } from "./route";

export const SEARCH_INPUT_ID = "search-input";

export function useShortcuts(datapackId: string | undefined) {
  useEffect(() => {
    const on = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null;
      const typing = el?.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el?.tagName ?? "");
      // A dialog has no focus trap: "/" from one of its buttons would change the page behind it.
      const dialogOpen = document.querySelector("[role=dialog]") !== null;
      if (e.key !== "/" || typing || dialogOpen || e.metaKey || e.ctrlKey || e.altKey || !datapackId) return;
      e.preventDefault();
      navigate({ datapackId, tab: "search" });
      // The Search page mounts on navigate; focus its box once it has.
      setTimeout(() => document.getElementById(SEARCH_INPUT_ID)?.focus(), 0);
    };
    window.addEventListener("keydown", on);
    return () => window.removeEventListener("keydown", on);
  }, [datapackId]);
}
