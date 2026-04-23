/**
 * Sticky top bar shared across workspace pages: optional CLI, optional leading title,
 * and a “Signed in as …” line from {@link getCurrentUser} when `showUserAvatar` is true.
 */
import { useEffect, useState, type ReactNode } from "react";
import { Terminal } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { getCurrentUser, type CurrentUser } from "@/lib/mekoApi";

export type AppWorkspaceHeaderProps = {
  /** Left column (e.g. Observe title + subtitle). */
  leading?: ReactNode;
  /** Extra controls in the right cluster, before CLI / signed-in line. */
  trailing?: ReactNode;
  showCliButton?: boolean;
  /** When true, loads `/auth/me` and shows “Signed in as” plus the user email on the right. */
  showUserAvatar?: boolean;
  padding?: "page" | "compact";
  sticky?: boolean;
  className?: string;
};

export function AppWorkspaceHeader({
  leading,
  trailing,
  showCliButton = true,
  showUserAvatar = true,
  padding = "page",
  sticky = true,
  className,
}: AppWorkspaceHeaderProps) {
  const [currentUser, setCurrentUser] = useState<CurrentUser | null>(null);
  const [userLoading, setUserLoading] = useState(showUserAvatar);

  useEffect(() => {
    if (!showUserAvatar) {
      setUserLoading(false);
      return;
    }
    const ac = new AbortController();
    setUserLoading(true);
    void getCurrentUser({ signal: ac.signal })
      .then((u) => {
        setCurrentUser(u);
      })
      .catch(() => {
        setCurrentUser(null);
      })
      .finally(() => {
        setUserLoading(false);
      });
    return () => ac.abort();
  }, [showUserAvatar]);

  const hasRight = trailing != null || showCliButton || showUserAvatar;

  return (
    <header
      className={cn(
        "flex items-center border-b border-border bg-background/95 py-4 backdrop-blur supports-[backdrop-filter]:bg-background/60",
        padding === "page" ? "px-8" : "px-6",
        leading && hasRight ? "justify-between" : leading ? "justify-start" : "justify-end",
        sticky && "sticky top-0 z-10",
        className,
      )}
    >
      {leading ? <div className="min-w-0">{leading}</div> : null}
      {hasRight ? (
        <div className="flex items-center gap-4 shrink-0">
          {trailing}
          {showCliButton ? (
            <Button
              type="button"
              variant="outline"
              className="gap-2 text-muted-foreground border-border bg-background hover:bg-muted hover:text-foreground"
            >
              <Terminal className="w-4 h-4" aria-hidden />
              CLI
            </Button>
          ) : null}
          {showUserAvatar ? (
            <p className="flex min-w-0 max-w-xs flex-wrap items-baseline justify-end gap-x-1 text-right text-sm leading-tight sm:max-w-sm">
              {userLoading ? (
                <span className="text-muted-foreground">Loading…</span>
              ) : currentUser?.email ? (
                <>
                  <span className="shrink-0 text-muted-foreground">Signed in as</span>
                  <span
                    className="min-w-0 truncate font-medium text-foreground"
                    title={currentUser.email}
                  >
                    {currentUser.email}
                  </span>
                </>
              ) : (
                <span className="text-muted-foreground">Signed in</span>
              )}
            </p>
          ) : null}
        </div>
      ) : null}
    </header>
  );
}
