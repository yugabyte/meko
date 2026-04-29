/**
 * Sticky top bar shared across workspace pages: optional leading title and,
 * when `showUserAvatar` is true, invite notifications plus “Signed in as …” from {@link getCurrentUser}.
 */
import { useEffect, useState, type ReactNode } from "react";
import { cn } from "@/lib/utils";
import { getCurrentUser, type CurrentUser } from "@/lib/mekoApi";
import { IncomingShareNotificationsBell } from "@/features/layout/IncomingShareNotificationsBell";

export type AppWorkspaceHeaderProps = {
  /** Left column (e.g. Observe title + subtitle). */
  leading?: ReactNode;
  /** When true, loads `/auth/me`, shows share-invite bell, and “Signed in as” plus the user email on the right. */
  showUserAvatar?: boolean;
  padding?: "page" | "compact";
  sticky?: boolean;
  className?: string;
};

export function AppWorkspaceHeader({
  leading,
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

  const hasLeft = Boolean(leading);
  const hasRight = showUserAvatar;

  return (
    <header
      className={cn(
        "flex items-center border-b border-border bg-background/95 py-4 backdrop-blur supports-[backdrop-filter]:bg-background/60",
        padding === "page" ? "px-8" : "px-6",
        hasLeft && hasRight ? "justify-between" : hasLeft ? "justify-start" : "justify-end",
        sticky && "sticky top-0 z-10",
        className,
      )}
    >
      {leading ? <div className="min-w-0 flex-1">{leading}</div> : null}
      {showUserAvatar ? (
        <div className="flex shrink-0 items-center gap-2 sm:gap-3">
          {userLoading ? (
            <div className="h-11 w-11 shrink-0 rounded-md bg-muted/40 animate-pulse" aria-hidden />
          ) : currentUser?.account_id ? (
            <IncomingShareNotificationsBell recipientUserId={currentUser.account_id} />
          ) : null}
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
        </div>
      ) : null}
    </header>
  );
}
