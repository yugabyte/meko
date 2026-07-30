import { Check, Sparkles, Rocket, TrendingUp, Gauge, Building2 } from "lucide-react";
import { cn } from "@/lib/utils";
import { Card } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  PLANS,
  MOST_POPULAR_PLAN_ID,
  PLAN_RANK,
  SALES_CONTACT_EMAIL,
  type Plan,
  type PlanId,
} from "@/features/billing/constants/plans";
import { formatDays, formatLimitNumeric, formatStorageLimit } from "@/features/billing/lib/format";

/**
 * Plan-picker row (screenshot 2). One `Card` per tier, `Button` for the
 * CTA, `Badge` for the "Current plan" / "Most popular" callout. No
 * monthly/annual toggle for the first pass — annual pricing arrives with
 * the billing pipeline.
 */
export type PlanPickerProps = {
  currentPlanId: PlanId;
  /** True while a Stripe redirect is in flight; disables every Upgrade button. */
  busy?: boolean;
  onUpgrade?: (planId: PlanId) => void;
};

export function PlanPicker({ currentPlanId, busy, onUpgrade }: PlanPickerProps) {
  return (
    <section className="space-y-4">
      <header>
        <h2 className="text-lg font-semibold text-foreground">Choose a plan</h2>
        <p className="text-xs text-muted-foreground">
          Each tier is a hard cap on the metrics shown above. Overage billed at
          standard per-unit rates.
        </p>
      </header>

      <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-5">
        {PLANS.map((plan) => (
          <PlanCard
            key={plan.id}
            plan={plan}
            currentPlanId={currentPlanId}
            isCurrent={plan.id === currentPlanId}
            isMostPopular={plan.id === MOST_POPULAR_PLAN_ID}
            busy={busy}
            onUpgrade={onUpgrade ? () => onUpgrade(plan.id) : undefined}
          />
        ))}
      </div>
    </section>
  );
}

function PlanCard({
  plan,
  currentPlanId,
  isCurrent,
  isMostPopular,
  busy,
  onUpgrade,
}: {
  plan: Plan;
  currentPlanId: PlanId;
  isCurrent: boolean;
  isMostPopular: boolean;
  busy?: boolean;
  onUpgrade?: () => void;
}) {
  return (
    <Card
      className={cn(
        "relative flex flex-col gap-4 p-5 shadow-none transition-colors",
        // Highlight border for current / most-popular. Current wins if both
        // — keeps the user oriented to their tier first.
        isCurrent
          ? "border-purple-500/50 bg-purple-500/5"
          : isMostPopular
            ? "border-emerald-500/40 bg-emerald-500/5"
            : "bg-card/60",
      )}
    >
      {(isCurrent || isMostPopular) && (
        // Floating pill sits fully above the card border (negative offset
        // > badge height) so it reads as a tab without crossing the line.
        // Solid fill (not /15 translucent) so the card border behind it
        // is hidden — translucent fills let the border show through and
        // looked like an overlap bug.
        <div className="pointer-events-none absolute -top-3 left-1/2 -translate-x-1/2">
          <Badge
            className={cn(
              "gap-1 border-0 px-2.5 py-0.5 text-[10px] font-semibold shadow-sm",
              isCurrent
                ? "bg-purple-500 text-white dark:bg-purple-400 dark:text-purple-950"
                : "bg-emerald-500 text-white dark:bg-emerald-400 dark:text-emerald-950",
            )}
          >
            {isCurrent ? <Check className="h-3 w-3" aria-hidden /> : <span aria-hidden>★</span>}
            {isCurrent ? "Current plan" : "Most popular"}
          </Badge>
        </div>
      )}

      <header className="flex items-start gap-2">
        <PlanIcon planId={plan.id} />
        <div className="min-w-0">
          <h3 className="text-base font-semibold text-foreground">{plan.name}</h3>
          <p className="text-xs text-muted-foreground">{plan.tagline}</p>
        </div>
      </header>

      <PriceBlock plan={plan} />

      <PlanCTA
        plan={plan}
        currentPlanId={currentPlanId}
        isCurrent={isCurrent}
        busy={busy}
        onUpgrade={onUpgrade}
      />

      <PlanLimitsList plan={plan} />

      <ul className="space-y-1.5 border-t border-border/50 pt-3 text-xs text-foreground">
        {plan.features.map((f) => (
          <li key={f} className="flex items-start gap-1.5">
            <Check className="mt-0.5 h-3 w-3 shrink-0 text-emerald-500" aria-hidden />
            <span>{f}</span>
          </li>
        ))}
      </ul>
    </Card>
  );
}

function PriceBlock({ plan }: { plan: Plan }) {
  if (plan.priceMonthly === null) {
    return (
      <div>
        <div className="text-2xl font-semibold text-foreground">Custom</div>
        <div className="text-xs text-muted-foreground">{plan.priceSubtitle}</div>
      </div>
    );
  }
  return (
    <div>
      <div className="flex items-baseline gap-1">
        <span className="text-base text-muted-foreground">$</span>
        <span className="text-3xl font-semibold tabular-nums text-foreground">
          {plan.priceMonthly}
        </span>
        <span className="text-xs text-muted-foreground">/mo</span>
      </div>
      <div className="text-xs text-muted-foreground">{plan.priceSubtitle}</div>
    </div>
  );
}

function PlanCTA({
  plan,
  currentPlanId,
  isCurrent,
  busy,
  onUpgrade,
}: {
  plan: Plan;
  currentPlanId: PlanId;
  isCurrent: boolean;
  busy?: boolean;
  onUpgrade?: () => void;
}) {
  // Single source for the "Contact sales" CTA — used on the Enterprise
  // card (anyone) and on every self-serve card when the user is already
  // on Enterprise (no Stripe customer to route through Portal).
  const contactSalesButton = (
    <Button asChild type="button" variant="outline" className="w-full">
      <a href={`mailto:${SALES_CONTACT_EMAIL}`}>Contact sales</a>
    </Button>
  );

  if (isCurrent) {
    return (
      <Button type="button" variant="secondary" disabled className="w-full">
        Current plan
      </Button>
    );
  }
  if (plan.id === "enterprise") {
    return contactSalesButton;
  }

  // Enterprise users don't have a Stripe customer record -- their
  // subscription is managed by sales. Clicking "Upgrade" on a
  // self-serve card would open the portal and fail with
  // NO_STRIPE_CUSTOMER, so we surface Contact-sales instead.
  if (currentPlanId === "enterprise") {
    return contactSalesButton;
  }

  // currentPlanId is narrowed to a non-enterprise tier here (early
  // return above handles "enterprise"). isPaidUser distinguishes Free
  // from any of the three paid self-serve tiers.
  const isPaidUser = currentPlanId !== "free";
  const isDowngrade = PLAN_RANK[plan.id] < PLAN_RANK[currentPlanId];

  // Free card has two completely different meanings depending on who's
  // looking at it. Free user: "Get started" (no-op, this IS your plan).
  // Paid user: "Downgrade" -> routes through the portal so Stripe can
  // cancel the active sub with proration.
  if (plan.id === "free") {
    if (!isPaidUser) {
      return (
        <Button
          type="button"
          variant="outline"
          className="w-full"
          disabled
          title="Free plan is the default"
        >
          Get started
        </Button>
      );
    }
    return (
      <Button
        type="button"
        variant="outline"
        className="w-full"
        onClick={onUpgrade}
        disabled={busy || !onUpgrade}
        title="Cancels your subscription at end of period via Stripe portal"
      >
        Switch
      </Button>
    );
  }

  // Paid-to-paid card. Label flips Upgrade <-> Downgrade based on
  // rank. Both route through the portal for the actual swap (Stripe
  // handles proration); only first-time-paid clicks go to Checkout.
  return (
    <Button
      type="button"
      variant="success"
      className="w-full"
      onClick={onUpgrade}
      disabled={busy || !onUpgrade}
      title={
        !isPaidUser
          ? "Start a Stripe Checkout to upgrade"
          : "Opens the Stripe billing portal to switch plans with proration"
      }
    >
      {isDowngrade ? "Switch" : "Upgrade"}
    </Button>
  );
}

function PlanLimitsList({ plan }: { plan: Plan }) {
  // Order mirrors the screenshot row-by-row.
  const rows: Array<{ label: string; value: string; emphasize?: boolean }> = [
    { label: "Datapacks", value: formatLimitNumeric(plan.limits.datapacks) },
    { label: "Agents / pack", value: formatLimitNumeric(plan.limits.agentsPerPack) },
    { label: "Collaborators", value: formatLimitNumeric(plan.limits.collaborators) },
    { label: "Hot storage", value: formatStorageLimit(plan.limits.hotStorageMB) },
    { label: "Knowledge", value: formatStorageLimit(plan.limits.knowledgeMB) },
    {
      label: "Memory ops / mo",
      value: formatLimitNumeric(plan.limits.memoryOpsPerMo),
      emphasize: true,
    },
    {
      label: "MCP calls / mo",
      value: formatLimitNumeric(plan.limits.mcpCallsPerMo),
      emphasize: true,
    },
    { label: "Conv. history", value: formatDays(plan.limits.convHistoryDays) },
    {
      label: "Decision traces",
      // Free: "24 hours"; Scale: "90 days + export"; Enterprise: "10+ years".
      // The middle/last two are tier-specific copy that lives here rather
      // than in the data so the constants file stays a clean enum.
      value: decisionTracesLabel(plan),
    },
  ];

  return (
    <dl className="space-y-1 text-xs">
      {rows.map((r) => (
        <div key={r.label} className="flex items-center justify-between gap-2">
          <dt className="text-muted-foreground">{r.label}</dt>
          <dd
            className={cn(
              "tabular-nums",
              r.emphasize ? "text-blue-700 dark:text-blue-300" : "text-foreground",
            )}
          >
            {r.value}
          </dd>
        </div>
      ))}
    </dl>
  );
}

function decisionTracesLabel(plan: Plan): string {
  if (plan.id === "scale") return "90 days + export";
  if (plan.id === "enterprise") return "10+ years";
  return formatDays(plan.limits.decisionTracesDays, { hoursWhenOne: true });
}

function PlanIcon({ planId }: { planId: PlanId }) {
  const Icon =
    planId === "free"
      ? Sparkles
      : planId === "starter"
        ? Rocket
        : planId === "growth"
          ? TrendingUp
          : planId === "scale"
            ? Gauge
            : Building2;
  const color =
    planId === "free"
      ? "text-blue-500"
      : planId === "starter"
        ? "text-sky-500"
        : planId === "growth"
          ? "text-purple-500"
          : planId === "scale"
            ? "text-emerald-500"
            : "text-amber-500";
  return (
    <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-foreground/5">
      <Icon className={cn("h-4 w-4", color)} aria-hidden />
    </div>
  );
}
