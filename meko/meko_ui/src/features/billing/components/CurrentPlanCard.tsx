import { ArrowUpDown, CreditCard, Sparkles, Zap, HardDrive, BookOpen, Database, Users } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import type { Plan } from "@/features/billing/constants/plans";
import { formatLimitNumeric, formatStorageLimit } from "@/features/billing/lib/format";
import type { BillingSummary } from "@/lib/api/billingApi";

/**
 * Header summary card on the Plans & billing page.
 *
 * Numerator values are placeholders ("—") for now because the
 * usage-aggregation pipeline (memory ops, MCP calls, hot storage GB,
 * knowledge GB) is a separate follow-up. Denominator values come from
 * the active plan so the bars already show the right caps for whichever
 * tier the user is on.
 */
export type CurrentPlanCardProps = {
  plan: Plan;
  /** When true, renders a skeleton-style frame while /auth/me is in flight. */
  loading?: boolean;
  /** True while either Stripe redirect is in flight — disables both buttons. */
  busy?: boolean;
  /** Renews date + card on file from /billing/summary. Null while loading or for Free users. */
  summary?: BillingSummary | null;
  /** Show the secondary line as a loading skeleton while /billing/summary is in flight. */
  summaryLoading?: boolean;
  onUpgrade?: () => void;
  onManagePayment?: () => void;
};

export function CurrentPlanCard({
  plan,
  loading,
  busy,
  summary,
  summaryLoading,
  onUpgrade,
  onManagePayment,
}: CurrentPlanCardProps) {
  const secondaryLine = describeSecondaryLine({
    plan,
    summary,
    summaryLoading,
    loading,
  });
  // "Billing cycle resets in 11 days" — derived from the same summary
  // payload as the secondary line. Falls back to "—" when there's no
  // renewal date (free / enterprise / not-yet-loaded).
  const billingCycleResetIn = describeBillingCycleReset({ summary });
  return (
    <Card className="bg-card/60 p-5 shadow-none">
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-[260px_1fr_auto] lg:items-center">
        {/* Plan summary (left) */}
        <div className="flex items-start gap-3">
          <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-md bg-purple-500/10">
            <Sparkles className="h-5 w-5 text-purple-600 dark:text-purple-300" aria-hidden />
          </div>
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <span className="text-[11px] font-medium uppercase tracking-wider text-muted-foreground">
                Current plan
              </span>
              <Badge
                variant="outline"
                className="h-5 gap-1 border-emerald-500/35 bg-emerald-500/10 px-1.5 text-[10px] font-medium text-emerald-700 dark:text-emerald-300"
              >
                <span className="h-1.5 w-1.5 rounded-full bg-emerald-500" aria-hidden />
                active
              </Badge>
            </div>
            <div className="mt-0.5 flex items-baseline gap-1.5">
              <h2 className="text-lg font-semibold text-foreground">{plan.name}</h2>
              <span className="text-xs text-muted-foreground">
                {plan.priceMonthly === null
                  ? "Custom"
                  : `$${plan.priceMonthly}/mo`}
              </span>
            </div>
            <p className="mt-1 text-xs text-muted-foreground">{secondaryLine}</p>
          </div>
        </div>

        {/* Usage bars (middle) — 2x3 grid */}
        <div className="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2 xl:grid-cols-3">
          <UsageBar
            icon={<Sparkles className="h-3.5 w-3.5 text-blue-500" aria-hidden />}
            label="Memory ops"
            current="—"
            limit={`${formatLimitNumeric(plan.limits.memoryOpsPerMo, { perMonth: true })}`}
            color="bg-blue-500"
          />
          <UsageBar
            icon={<Zap className="h-3.5 w-3.5 text-purple-500" aria-hidden />}
            label="MCP tool calls"
            current="—"
            limit={formatLimitNumeric(plan.limits.mcpCallsPerMo, { perMonth: true })}
            color="bg-purple-500"
          />
          <UsageBar
            icon={<HardDrive className="h-3.5 w-3.5 text-emerald-500" aria-hidden />}
            label="Hot storage"
            current="—"
            limit={formatStorageLimit(plan.limits.hotStorageMB)}
            color="bg-emerald-500"
          />
          <UsageBar
            icon={<BookOpen className="h-3.5 w-3.5 text-emerald-500" aria-hidden />}
            label="Knowledge base"
            current="—"
            limit={formatStorageLimit(plan.limits.knowledgeMB)}
            color="bg-emerald-500"
          />
          <UsageBar
            icon={<Database className="h-3.5 w-3.5 text-muted-foreground" aria-hidden />}
            label="Datapacks"
            current="—"
            limit={formatLimitNumeric(plan.limits.datapacks)}
            color="bg-muted-foreground"
          />
          <UsageBar
            icon={<Users className="h-3.5 w-3.5 text-muted-foreground" aria-hidden />}
            label="Collaborators / pack"
            current="—"
            limit={formatLimitNumeric(plan.limits.collaborators)}
            color="bg-muted-foreground"
          />
        </div>

        {/* Action stack (right) */}
        <div className="flex flex-col gap-2 lg:w-[200px]">
          <Button
            type="button"
            variant="success"
            className="gap-1.5"
            onClick={onUpgrade}
            // Disabled when the upgrade flow isn't available:
            //   - Scale: already at the top self-serve tier.
            //   - Enterprise: handled by sales; no Stripe customer to
            //     route through Checkout / Portal.
            disabled={busy || !onUpgrade || plan.id === "scale" || plan.id === "enterprise"}
            title={
              plan.id === "scale"
                ? "Already on the top self-serve tier — change or cancel below"
                : plan.id === "enterprise"
                  ? "Enterprise plans are managed by sales"
                  : "Switch plans via Stripe (Checkout for first upgrade, Portal afterwards)"
            }
          >
            <ArrowUpDown className="h-3.5 w-3.5" aria-hidden />
            Upgrade
          </Button>
          <Button
            type="button"
            variant="outline"
            className="gap-1.5"
            onClick={onManagePayment}
            disabled={busy || !onManagePayment}
            title="Opens the Stripe billing portal"
          >
            <CreditCard className="h-3.5 w-3.5" aria-hidden />
            Manage payment
          </Button>
        </div>
      </div>

      <div className="mt-4 flex flex-wrap items-center justify-between gap-2 border-t border-border/60 pt-3 text-xs text-muted-foreground">
        <p>
          Billing cycle resets in <span className="font-medium text-foreground">{billingCycleResetIn}</span> · usage above
          carries into next invoice as overage at standard rates
        </p>
        {/* <button
          type="button"
          className="text-muted-foreground/80 hover:text-foreground"
          title="Hard caps land with the billing pipeline"
        >
          Set hard cap ›
        </button> */}
      </div>
    </Card>
  );
}

function UsageBar({
  icon,
  label,
  current,
  limit,
  color,
}: {
  icon: React.ReactNode;
  label: string;
  current: string;
  limit: string;
  color: string;
}) {
  return (
    <div className="min-w-0">
      <div className="flex items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-1.5">
          {icon}
          <span className="truncate text-xs text-muted-foreground">{label}</span>
        </div>
        <div className="shrink-0 text-xs tabular-nums">
          <span className="font-medium text-foreground">{current}</span>
          <span className="text-muted-foreground"> / {limit}</span>
        </div>
      </div>
      <div className="mt-1.5 h-1.5 w-full overflow-hidden rounded-full bg-foreground/5">
        {/* Empty fill — numerator is "—" until usage aggregation lands. The
            colored bar still hints at the metric color for visual continuity
            with the populated state. */}
        <div className={`h-full w-0 rounded-full ${color}`} />
      </div>
    </div>
  );
}

/**
 * Renders the value for "Billing cycle resets in <X>" on the bottom
 * row of the current-plan card. Just the number of days until the
 * next renewal — phrased as "today" / "tomorrow" / "11 days" so the
 * sentence reads naturally without a separate "in" preposition for
 * each case.
 *
 * Returns "—" for users without a renewal date (free tier,
 * enterprise off-platform, /billing/summary still in flight) so the
 * sentence still parses.
 */
function describeBillingCycleReset({
  summary,
}: {
  summary?: BillingSummary | null;
}): string {
  if (!summary?.current_period_end) return "—";
  const end = new Date(summary.current_period_end);
  if (Number.isNaN(end.getTime())) return "—";
  const msLeft = end.getTime() - Date.now();
  const days = Math.ceil(msLeft / (24 * 60 * 60 * 1000));
  if (days <= 0) return "today";
  if (days === 1) return "tomorrow";
  return `${days} days`;
}

/**
 * Builds the "Renews May 28, 2026 · Card on file Visa 4242" line under
 * the plan name. Branches:
 *   - /auth/me still loading           → "Loading account…"
 *   - Free plan (no subscription)      → "Free plan · upgrade for invoices"
 *   - Paid + summary loading           → "Loading subscription…"
 *   - Paid + summary loaded            → renews + card pieces, joined by "·"
 *   - Cancel pending                   → "Cancels <date>" instead of "Renews"
 */
function describeSecondaryLine({
  plan,
  summary,
  summaryLoading,
  loading,
}: {
  plan: Plan;
  summary?: BillingSummary | null;
  summaryLoading?: boolean;
  loading?: boolean;
}): string {
  if (loading) return "Loading account…";
  if (plan.id === "free") return "Free plan · upgrade for invoices";
  if (plan.id === "enterprise") return "Billed off-platform · talk to sales";
  if (summaryLoading) return "Loading subscription…";

  const parts: string[] = [];

  if (summary?.current_period_end) {
    const d = new Date(summary.current_period_end);
    if (!Number.isNaN(d.getTime())) {
      const formatted = d.toLocaleDateString(undefined, {
        month: "short",
        day: "numeric",
        year: "numeric",
      });
      parts.push(`${summary.cancel_at_period_end ? "Cancels" : "Renews"} ${formatted}`);
    }
  } else {
    parts.push("Renews —");
  }

  if (summary?.card_brand && summary?.card_last4) {
    // Stripe returns lowercase brand strings ("visa", "mastercard"); the
    // UI capitalizes the first letter for visual parity with the Stripe
    // dashboard rendering.
    const brand =
      summary.card_brand.charAt(0).toUpperCase() + summary.card_brand.slice(1);
    parts.push(`Card on file ${brand} ${summary.card_last4}`);
  } else {
    parts.push("Card on file —");
  }

  return parts.join(" · ");
}

