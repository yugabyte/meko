// Top bar: workspace breadcrumb on the left, the "ask an agent" search
// in the centre, and the action cluster (theme toggle + notifications +
// settings) on the right. Styling is sourced from `layoutStyles`.

import { Bell, LogOut, Settings, Sparkles } from "lucide-react";
import { useNavigate } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { ThemeToggle } from "@/components/ThemeToggle";
import { useAuth } from "@/auth/AuthContext";
import { layoutStyles as s } from "./layoutStyles";

export function Header() {
  const navigate = useNavigate();
  const { user, signOut } = useAuth();

  const handleSignOut = () => {
    signOut();
    navigate("/signin", { replace: true });
  };

  return (
    <header className={s.header}>
      {/* Centre search */}
      <div className={s.searchWrap}>
        <Sparkles className={s.searchIcon} />
        <Input
          type="text"
          placeholder="Ask an agent or describe what to build…"
          className={s.searchInput}
        />
        <kbd className={s.searchKbd}>⌘K</kbd>
      </div>

      {/* Actions */}
      <div className={s.headerActions}>
        <ThemeToggle />
        <Button variant="ghost" size="icon" aria-label="Notifications">
          <Bell className="h-5 w-5" />
        </Button>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" aria-label="Settings">
              <Settings className="h-5 w-5" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-56">
            <DropdownMenuLabel>{user?.email ?? "Account"}</DropdownMenuLabel>
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={handleSignOut}>
              <LogOut className="mr-2 h-4 w-4" />
              Sign out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </header>
  );
}
