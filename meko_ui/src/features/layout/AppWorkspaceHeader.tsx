/**
 * Sticky top bar shared across workspace pages: optional leading title and,
 * when `showUserAvatar` is true, invite notifications plus “Signed in as …”
 * from the shared {@link useCurrentUser} query.
 */

import { type ReactNode } from "react";
import { cn } from "@/lib/utils";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { IncomingShareNotificationsBell } from "@/features/layout/IncomingShareNotificationsBell";
import { ThemeToggleButton } from "@/features/layout/ThemeToggleButton";

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
  // Shared, session-cached identity (one `/me` fetch app-wide). Gated by
  // `showUserAvatar` so routes that hide the avatar don't fetch.
  const { data: currentUser = null, isLoading: userLoading } = useCurrentUser({
    enabled: showUserAvatar,
  });

  const hasLeft = Boolean(leading);
  const hasRight = showUserAvatar;

  return (
    <header
      className={cn(
        "flex items-center border-b border-border bg-background/95 py-4 backdrop-blur supports-[backdrop-filter]:bg-background/60",
        padding === "page" ? "px-8" : "px-6",
        hasLeft && hasRight ? "justify-between" : hasLeft ? "justify-start" : "justify-end",
        // `z-20` (not `z-10`) so the sticky header always paints above
        // page content. Several pages — notably the Datapacks overview's
        // "What is a Datapack?" panel — use `relative z-10` to lift their
        // own inner content above decorative blur blobs in the same
        // card. With both at the same z, DOM order wins and content
        // bleeds through the translucent header. A header layer above
        // every page-content layer is the safer global default.
        sticky && "sticky top-0 z-20",
        className,
      )}
    >
      {leading ? <div className="min-w-0 flex-1">{leading}</div> : null}
      {showUserAvatar ? (
        <div className="flex shrink-0 items-center gap-2 sm:gap-3">
          {/* Theme toggle sits left of the bell — always available
              regardless of auth/user state, so even the loading skeleton
              frame still shows it. */}
          <ThemeToggleButton />
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
          {/* Settings moved to the left sidebar — see Sidebar.tsx. */}
        </div>
      ) : null}
    </header>
  );
}
