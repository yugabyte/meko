// Light/dark mode toggle button. Reads + flips the theme via the shared
// `useThemeToggle` hook (which toggles the `.dark` class on <html> and
// persists the choice). Icon swaps to reflect the action it performs.

import { Moon, Sun } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useThemeToggle } from "@/lib/theme";

export function ThemeToggle() {
  const { theme, toggle } = useThemeToggle();
  return (
    <Button
      variant="ghost"
      size="icon"
      onClick={toggle}
      aria-label={theme === "dark" ? "Switch to light mode" : "Switch to dark mode"}
    >
      {theme === "dark" ? <Sun className="h-5 w-5" /> : <Moon className="h-5 w-5" />}
    </Button>
  );
}
