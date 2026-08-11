// UsagePage — dedicated account usage view: the live consumed-vs-cap
// tracker for every metered dimension of the caller's tier. Promoted out
// of Settings to its own left-nav item so usage is one click away.

import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { UsageTrackerSection } from "@/features/settings/components/UsageTrackerSection";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { FREE_TIER_QUOTA_ENABLED, isGatedTier } from "@/lib/freeTier";

export default function UsagePage() {
  const { data: user } = useCurrentUser();
  // UsageTrackerSection self-hides for un-gated tiers (pro / enterprise)
  // and quota-off deployments — show a note instead of a blank page.
  const hasCaps = FREE_TIER_QUOTA_ENABLED && isGatedTier(user?.user_type);

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden bg-background text-foreground">
      <AppWorkspaceHeader
        leading={<h1 className="text-base font-medium text-foreground">Usage</h1>}
      />

      <div className="flex-1 overflow-y-auto px-4 py-5 sm:px-6 md:px-8 md:py-8">
        <div className="mx-auto w-full max-w-5xl space-y-6">
          {hasCaps ? (
            <UsageTrackerSection />
          ) : (
            <p className="text-sm text-muted-foreground">
              Your plan has no usage caps — there&apos;s nothing to track here.
            </p>
          )}
        </div>
      </div>
    </div>
  );
}
