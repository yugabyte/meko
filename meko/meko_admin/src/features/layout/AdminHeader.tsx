// Slim top header bar spanning the content column: right-aligned theme-toggle
// and sign-out icon buttons (the app-level actions moved here from the sidebar).
import { Moon, Sun, LogOut } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { useThemeToggle } from "@/lib/theme";
import { useAuth } from "@/lib/authContext";

export function AdminHeader() {
  const { theme, toggle } = useThemeToggle();
  const { logout } = useAuth();
  const themeLabel = theme === "dark" ? "Switch to light mode" : "Switch to dark mode";

  return (
    <header className="flex h-14 shrink-0 items-center justify-end gap-1 border-b border-border bg-background px-4">
      <TooltipProvider delayDuration={200}>
        <Tooltip>
          <TooltipTrigger asChild>
            <Button variant="ghost" size="icon" onClick={toggle} aria-label={themeLabel}>
              {theme === "dark" ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
            </Button>
          </TooltipTrigger>
          <TooltipContent>{themeLabel}</TooltipContent>
        </Tooltip>
        <Tooltip>
          <TooltipTrigger asChild>
            <Button variant="ghost" size="icon" onClick={logout} aria-label="Sign out">
              <LogOut className="h-4 w-4" />
            </Button>
          </TooltipTrigger>
          <TooltipContent>Sign out</TooltipContent>
        </Tooltip>
      </TooltipProvider>
    </header>
  );
}
