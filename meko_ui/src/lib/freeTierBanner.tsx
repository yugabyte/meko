// Shared free-tier "you've hit the limit" banner used at the top of
// gated pages (Datapacks overview, Datapack detail, WorkbenchLM chat).
// More prominent than the per-button tooltip; intended as the primary
// upgrade CTA when a user has run out of headroom on a dimension.
//
// Kept in its own .tsx file because `lib/freeTier.ts` is pure data
// (constants + helpers) and importing a TSX component there would force
// the whole module into a .tsx file. Caller is responsible for the
// "should I render at all" check — typically
// `freeTier.hasHit* && isFreeTier(currentUser?.user_type)`.

import { WarningBanner } from "@/components/ui/warning-banner";
import { FREE_TIER_BANNER_MESSAGES, SALES_CONTACT_URL } from "@/lib/freeTier";

export function FreeTierLimitBanner({
  kind,
  className,
}: {
  kind: keyof typeof FREE_TIER_BANNER_MESSAGES;
  className?: string;
}) {
  return (
    <WarningBanner className={className}>
      {FREE_TIER_BANNER_MESSAGES[kind]}{" "}
      <a
        href={SALES_CONTACT_URL}
        target="_blank"
        rel="noreferrer noopener"
        className="underline underline-offset-2 hover:no-underline"
      >
        Contact sales
      </a>
      .
    </WarningBanner>
  );
}
