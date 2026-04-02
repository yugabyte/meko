import { Link, useLocation } from "wouter";
import {
  Database,
  Play,
  Activity,
  Settings,
  CreditCard,
  Box,
  ChevronLeft,
  ChevronRight,
  LogOut,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { useState } from "react";
import { Button } from "./ui/button";
import { logout } from "@/lib/mekoApi";

export default function Sidebar() {
  const [location] = useLocation();
  const [isCollapsed, setIsCollapsed] = useState(false);
  const [loggingOut, setLoggingOut] = useState(false);
  const [logoutError, setLogoutError] = useState<string | null>(null);

  const navItems = [
    // { name: "Meko Agent", href: "/agent", icon: Play },
    { name: "Datapacks", href: "/datapacks", icon: Database },
    { name: "Observe", href: "/observe-hub", icon: Activity },
  ];

  const bottomItems = [
    { name: "Settings", href: "/settings", icon: Settings },
    { name: "Usage and Billing", href: "/billing", icon: CreditCard },
  ];

  const handleLogout = async () => {
    setLogoutError(null);
    setLoggingOut(true);
    try {
      await logout();
      window.location.replace("/login");
    } catch (err) {
      setLogoutError(err instanceof Error ? err.message : "logout failed");
      setLoggingOut(false);
    }
  };

  return (
    <div className={cn("flex-shrink-0 border-r border-border bg-[#0E1525] h-screen flex flex-col transition-all duration-300 relative", isCollapsed ? "w-[68px]" : "w-[240px]")}>
      
      <Button 
        variant="ghost" 
        size="icon" 
        className="absolute -right-4 top-6 z-50 h-8 w-8 rounded-full border border-border bg-[#0E1525] shadow-md hover:bg-muted text-muted-foreground hover:text-white"
        onClick={() => setIsCollapsed(!isCollapsed)}
      >
        {isCollapsed ? <ChevronRight className="h-4 w-4" /> : <ChevronLeft className="h-4 w-4" />}
      </Button>

      <div className={cn("p-4 flex items-center", isCollapsed ? "justify-center px-0" : "gap-3")}>
        <div className="w-8 h-8 rounded-md flex shrink-0 items-center justify-center bg-white text-black">
          <Box className="w-5 h-5" />
        </div>
        {!isCollapsed && <span className="font-semibold text-xl tracking-tight text-white animate-in fade-in">meko</span>}
      </div>

      <div className="flex-1 overflow-y-auto py-4 flex flex-col gap-1 px-3">
        {navItems.map((item) => {
          const isActive =
            location === item.href ||
            location.startsWith(`${item.href}/`);
          const itemClassName = cn(
            "flex items-center rounded-md text-sm font-medium transition-colors",
            isCollapsed ? "justify-center w-10 h-10 mx-auto" : "gap-3 px-3 py-2",
            isActive
              ? "bg-white/10 text-white"
              : "text-gray-400 hover:bg-white/5 hover:text-white",
          );

          return (
            <Link
              key={item.name}
              href={item.href}
              className={itemClassName}
              title={isCollapsed ? item.name : undefined}
            >
              <item.icon className={cn("shrink-0", isCollapsed ? "w-5 h-5" : "w-4 h-4")} />
              {!isCollapsed && <span className="animate-in fade-in truncate">{item.name}</span>}
            </Link>
          );
        })}
      </div>

      <div className="p-3 flex flex-col gap-1">
        {!isCollapsed && <div className="px-3 py-2 text-xs font-semibold text-gray-500 uppercase tracking-wider animate-in fade-in">Account</div>}
        {bottomItems.map((item) => (
          <Link key={item.name} href={item.href} className={cn(
              "flex items-center rounded-md text-sm font-medium text-gray-400 hover:bg-white/5 hover:text-white transition-colors",
              isCollapsed ? "justify-center w-10 h-10 mx-auto" : "gap-3 px-3 py-2"
            )}
            title={isCollapsed ? item.name : undefined}
          >
              <item.icon className={cn("shrink-0", isCollapsed ? "w-5 h-5" : "w-4 h-4")} />
              {!isCollapsed && <span className="animate-in fade-in truncate">{item.name}</span>}
          </Link>
        ))}
        <button
          type="button"
          className={cn(
            "mt-1 flex items-center rounded-md text-sm font-medium text-gray-400 hover:bg-white/5 hover:text-white transition-colors",
            isCollapsed ? "justify-center w-10 h-10 mx-auto" : "gap-3 px-3 py-2",
          )}
          onClick={handleLogout}
          disabled={loggingOut}
          title={isCollapsed ? "Sign out" : undefined}
        >
          <LogOut className={cn("shrink-0", isCollapsed ? "w-5 h-5" : "w-4 h-4")} />
          {!isCollapsed && (
            <span className="animate-in fade-in truncate">
              {loggingOut ? "Signing out..." : "Sign out"}
            </span>
          )}
        </button>
        {!isCollapsed && logoutError ? (
          <p className="px-3 pt-1 text-xs text-red-400">{logoutError}</p>
        ) : null}
      </div>
    </div>
  );
}
