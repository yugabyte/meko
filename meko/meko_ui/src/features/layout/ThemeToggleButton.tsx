// Light / dark mode toggle. Sits in `AppWorkspaceHeader` next to the
// notification bell. Sun icon when dark (click to go light), moon icon
// when light (click to go dark) — matching the "icon shows what you'll
// switch TO" convention most users expect (GitHub, Discord, etc.).
//
// All state + persistence lives in `@/lib/theme`; this component is a
// thin button wrapper.

import { Moon, Sun } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useThemeToggle } from "@/lib/theme";

export function ThemeToggleButton() {
  const { theme, toggle } = useThemeToggle();
  const nextLabel = theme === "dark" ? "Switch to light mode" : "Switch to dark mode";
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      onClick={toggle}
      className="h-9 w-9 shrink-0 text-muted-foreground hover:text-foreground"
      aria-label={nextLabel}
      title={nextLabel}
    >
      {/*
        The icon shows the destination, not the current state — a sun
        means "click to go to light mode" (i.e. you're currently dark).
        That's the prevailing convention; flipping it confuses users
        who expect the icon to be the action, not a status indicator.
      */}
      {theme === "dark" ? (
        <Sun className="h-5 w-5" strokeWidth={2} aria-hidden />
      ) : (
        <Moon className="h-5 w-5" strokeWidth={2} aria-hidden />
      )}
    </Button>
  );
}
