/**
 * Sticky top bar shared across workspace pages: optional leading title and,
 * when `showUserAvatar` is true, invite notifications plus “Signed in as …”
 * from the shared {@link useCurrentUser} query.
 */

import { type ReactNode } from "react";
import { cn } from "@/lib/utils";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { isPlaceholderEmail } from "@/lib/api/mekoApi";
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

  // A federated (LinkedIn) signup with no IdP email carries a synthesized
  // placeholder address until they verify a real one. Surfacing that
  // `…@no-reply.…meko.invalid` string in the navbar looks broken, so we hide it
  // and show the user's name (or a neutral label) until verification swaps in
  // the real email (the modal invalidates /me on success, refreshing this).
  const email = currentUser?.email ?? "";
  const showPlaceholder = isPlaceholderEmail(email);
  // Drop the "-" name placeholders the inbound-federation Lambda injects.
  const realName = [currentUser?.first_name, currentUser?.last_name]
    .map((p) => (p ?? "").trim())
    .filter((p) => p !== "" && p !== "-")
    .join(" ");

  return (
    <header
      className={cn(
        // Mobile (< md): stack vertically — leading content (page title /
        // datapack picker / description) gets a full-width row, the
        // user-info cluster (theme / bell / email + tier badge) goes on
        // its own row below. Otherwise the two compete for ~390px and
        // the leading column collapses to one-word-per-line wrap.
        // Mobile also gets `pl-14` so it clears the fixed hamburger
        // button at top-left (`left-2 + w-10 = 48px`).
        // gap-y-2 on mobile so the stacked leading row + user-info row
        // get visual breathing room; reset to 0 at md+ where the
        // single horizontal row doesn't need vertical spacing.
        "flex flex-col items-stretch gap-y-2 border-b border-border bg-background/95 py-3 sm:py-4 backdrop-blur supports-[backdrop-filter]:bg-background/60 md:gap-y-0",
        // Desktop (>= md): back to a single horizontal row.
        "md:flex-row md:items-center",
        // Tighter horizontal padding below sm so the chrome doesn't eat
        // half the phone's viewport. Mobile pl-14 clears the hamburger;
        // md+ reverts to the original spacious gutters.
        padding === "page"
          ? "pl-14 pr-4 sm:pr-6 md:px-8 md:pl-8"
          : "pl-14 pr-3 sm:pr-4 md:px-6 md:pl-6",
        // Stacked layout doesn't need `justify-between`; row layout does.
        hasLeft && hasRight
          ? "md:justify-between"
          : hasLeft
            ? "md:justify-start"
            : "md:justify-end",
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
            ) : showPlaceholder ? (
              // Never show the synthesized placeholder address. Prefer the
              // user's name; otherwise a neutral "finish setup" nudge.
              realName ? (
                <>
                  <span className="shrink-0 text-muted-foreground">Signed in as</span>
                  <span className="min-w-0 truncate font-medium text-foreground" title={realName}>
                    {realName}
                  </span>
                </>
              ) : (
                <span className="text-muted-foreground">Finish setting up your account</span>
              )
            ) : email ? (
              <>
                <span className="shrink-0 text-muted-foreground">Signed in as</span>
                <span className="min-w-0 truncate font-medium text-foreground" title={email}>
                  {email}
                </span>
                <TierBadge userType={currentUser?.user_type} />
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

/**
 * Tier chip shown next to the signed-in email. Renders for free / pro /
 * enterprise — anything unknown / empty stays hidden so unauthenticated
 * or pre-/auth/me states don't flash a wrong tag. Source of truth:
 * `currentUser.user_type` from `/auth/me`.
 */
function TierBadge({ userType }: { userType?: string | null }) {
  const userTier = (userType ?? "").trim().toLowerCase();
  if (userTier !== "free" && userTier !== "pro" && userTier !== "enterprise") return null;
  // Each tier gets a distinct hue so the eye separates them at a glance.
  //   free       — muted/neutral; "you can upgrade" without nagging.
  //   pro        — blue (matches the existing CTA / Pricing accents).
  //   enterprise — purple so it reads as one tier above Pro.
  const tone =
    userTier === "enterprise"
      ? "border-purple-500/40 bg-purple-500/10 text-purple-700 dark:text-purple-300"
      : userTier === "pro"
        ? "border-blue-500/40 bg-blue-500/10 text-blue-700 dark:text-blue-300"
        : "border-border bg-muted text-muted-foreground";
  const label = userTier.toUpperCase();
  return (
    <span
      className={cn(
        "ml-1 inline-flex shrink-0 items-center rounded-full border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wider",
        tone,
      )}
      aria-label={`${label} tier`}
    >
      {label}
    </span>
  );
}
