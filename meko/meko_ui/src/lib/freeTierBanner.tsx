// Shared "you've hit the tier limit" banner used at the top of gated
// pages. Picks Free→Standard vs Standard→Pro copy based on the current
// user_type from /auth/me. CTA links to /settings where PricingSection
// hosts both the Standard "Upgrade" button and the Pro "Talk to our team"
// link — no more mailto in the banner itself.
//
// Kept in its own .tsx file because `lib/freeTier.ts` is pure data.
// Caller decides whether to render at all (typically
// `tierStatus.hasHit* && isGatedTier(currentUser?.user_type)`).

import { Link } from "react-router-dom";
import { WarningBanner } from "@/components/ui/warning-banner";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { ROUTES } from "@/app/routes/paths";
import { TIER_BANNER_MESSAGES, nextTierFor, type TierBannerKind } from "@/lib/freeTier";

export function TierLimitBanner({
  kind,
  className,
}: {
  kind: TierBannerKind;
  className?: string;
}) {
  const { data: user } = useCurrentUser({ enabled: true });
  const target = nextTierFor(user?.user_type);
  const message = TIER_BANNER_MESSAGES[target][kind];
  const linkLabel = target === "standard" ? "Upgrade" : "Talk to our team";
  return (
    <WarningBanner className={className}>
      {message}{" "}
      <Link
        to={ROUTES.settings}
        className="underline underline-offset-2 hover:no-underline"
      >
        {linkLabel}
      </Link>
      .
    </WarningBanner>
  );
}

/** Back-compat alias — existing callers importing `FreeTierLimitBanner`
 *  keep working while we migrate. Prefer `TierLimitBanner` in new code. */
export const FreeTierLimitBanner = TierLimitBanner;
