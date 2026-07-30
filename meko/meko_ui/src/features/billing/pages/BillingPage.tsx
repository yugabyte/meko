import { CreditCard, FileText, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { useCurrentPlan } from "@/features/billing/hooks/useCurrentPlan";
import { useBillingActions } from "@/features/billing/hooks/useBillingActions";
import { useBillingSummary } from "@/features/billing/hooks/useBillingSummary";
import { useBillingNavFlag } from "@/features/billing/hooks/useBillingNavFlag";
import NotFoundPage from "@/features/system/pages/NotFoundPage";
import { CurrentPlanCard } from "@/features/billing/components/CurrentPlanCard";
import { PlanPicker } from "@/features/billing/components/PlanPicker";
import { PlanComparisonTable } from "@/features/billing/components/PlanComparisonTable";
import { BillingInfoCards } from "@/features/billing/components/BillingInfoCards";
import type { PlanId } from "@/features/billing/constants/plans";

/**
 * Picks the "next reasonable paid tier" the header's single Upgrade
 * button should target. We avoid Enterprise (that's a sales-touch
 * flow) and pick the next paid step up from whatever the user is on.
 * Free → Starter, Starter → Growth, Growth → Scale, Scale → Scale
 * (no-op; the button is also hidden in that case).
 */
function nextPaidPlanFrom(planId: PlanId): "starter" | "growth" | "scale" {
  switch (planId) {
    case "free":
      return "starter";
    case "starter":
      return "growth";
    case "growth":
    case "scale":
      return "scale";
    case "enterprise":
      return "scale";
  }
}

/**
 * Plans & billing — workspace plan, current usage, plan picker, feature
 * comparison, and compliance/auth/discount info cards.
 *
 * Usage numerators ("—") and renewal/card metadata are placeholders; the
 * usage-aggregation pipeline and Stripe Customer Portal link are tracked
 * separately. The Invoices / Usage history buttons in the header are
 * disabled today because Stripe Customer Portal (Option A) will host
 * both — they only need a `window.open(portalUrl)` wire-up later.
 */
export default function BillingPage() {
  // Pre-GA feature gate: the route exists in the router (so deep links
  // for invited testers work), but the page only renders for callers
  // whose ?showPaymentOptions=true unlock has been latched. Everyone
  // else gets the standard 404 -- same shell as any other dead URL.
  const showBilling = useBillingNavFlag();

  const { plan, planId, loading } = useCurrentPlan();
  const { summary, loading: summaryLoading } = useBillingSummary();
  const { busy, openCheckout, openPortal } = useBillingActions();

  if (!showBilling) {
    return <NotFoundPage />;
  }

  /**
   * Single decision point for every "change plan" button on the page.
   *   - Free user picking a paid tier   → Stripe Checkout
   *   - Paid user picking any other tier (up or down) → Stripe Portal
   *
   * The Portal handles plan swaps with proration on the *same*
   * subscription, which is why we never call Checkout for an
   * already-subscribed user (the BE returns 409 in that case
   * anyway -- this just makes the UI smarter so the user never
   * sees the error toast).
   */
  const routeUpgradeClick = (currentPlanId: PlanId, targetPlanId: PlanId) => {
    if (currentPlanId === "free") {
      // Free users start a fresh subscription. Skip Checkout for
      // sentinel targets so we never POST a no-op (no price ID).
      if (targetPlanId === "free" || targetPlanId === "enterprise") return;
      openCheckout(targetPlanId);
      return;
    }
    // Paid user: route through Stripe Portal regardless of the
    // target tier. The portal lets them pick the new plan there
    // (or cancel down to Free) with Stripe-handled proration.
    openPortal();
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden bg-background text-foreground">
      <AppWorkspaceHeader
        leading={
          <div className="flex items-start gap-3">
            <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-md bg-emerald-500/10">
              <CreditCard className="h-4 w-4 text-emerald-600 dark:text-emerald-300" aria-hidden />
            </div>
            <div className="min-w-0">
              <h1 className="text-base font-medium text-foreground">Plans &amp; billing</h1>
              <p className="text-xs text-muted-foreground">
                Your workspace plan, current usage, and invoices. Upgrades take effect
                immediately and are prorated.
              </p>
            </div>
          </div>
        }
      />

      <div className="flex-1 overflow-y-auto px-8 py-6">
        <div className="mx-auto w-full max-w-6xl space-y-6">
          {/* Header actions row — Invoices opens the Stripe Customer
              Portal (which lists invoices on its home page).
              "Usage history" is intentionally absent for now: Stripe
              Portal has no usage view, and Meko's per-metric usage
              dashboard ships separately with the aggregation
              pipeline. Bring it back once we have a /usage page to
              link to. */}
          <div className="flex flex-wrap items-center justify-end gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="gap-1.5"
              onClick={openPortal}
              disabled={busy}
              title="Opens the Stripe billing portal"
            >
              {busy ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
              ) : (
                <FileText className="h-3.5 w-3.5" aria-hidden />
              )}
              Invoices
            </Button>
          </div>

          <CurrentPlanCard
            plan={plan}
            loading={loading}
            busy={busy}
            summary={summary}
            summaryLoading={summaryLoading}
            onUpgrade={() => routeUpgradeClick(planId, nextPaidPlanFrom(planId))}
            onManagePayment={openPortal}
          />

          <PlanPicker
            currentPlanId={planId}
            busy={busy}
            onUpgrade={(targetPlanId) => routeUpgradeClick(planId, targetPlanId)}
          />

          <PlanComparisonTable currentPlanId={planId} />

          <BillingInfoCards />
        </div>
      </div>
    </div>
  );
}
