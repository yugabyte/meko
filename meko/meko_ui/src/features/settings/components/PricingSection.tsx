// Pricing section on the Settings page.
//
// Renders Free / Standard / Pro tier cards. Standard is the AWS
// Marketplace $25/mo tier; hidden entirely when
// MEKO_AWS_MARKETPLACE_URL is unset (nothing to link the Upgrade
// button to). The "Current" chip follows the signed-in user's
// user_type from /auth/me. Every row value matches the caps enforced
// by lib/freeTier.ts on the FE and api_server/internal/service/free_tier.go on the BE.

import { useCallback, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { ArrowUpRight, Check, CreditCard, Cloud, Mail } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useArrivalFlash } from "@/hooks/useArrivalFlash";
import { HIGHLIGHT_TIER_PARAM } from "@/lib/freeTierBanner";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import {
  AWS_MARKETPLACE_LISTING_URL,
  KB_STORAGE_LIMIT_LABEL,
  SALES_CONTACT_URL,
  STANDARD_TIER_ENABLED,
  resolveTier,
  type TierName,
} from "@/lib/freeTier";

type TierColumn = "free" | "standard" | "pro";

type Row = {
  label: string;
  values: Record<TierColumn, string | null>;
};

// Row order matches the marketing screenshot: metered caps first, then
// capability rows, then enterprise-only rows at the bottom. Null →
// em-dash placeholder for "not included on this tier."
const TIER_ROWS: ReadonlyArray<Row> = [
  { label: "Datapacks", values: { free: "10", standard: "50", pro: "Unlimited" } },
  { label: "Add conversations", values: { free: "1,000", standard: "10,000", pro: "Custom" } },
  { label: "Memory adds", values: { free: "10,000", standard: "30,000", pro: "Custom" } },
  { label: "Workbench tokens", values: { free: "10M", standard: "50M", pro: "Custom" } },
  { label: KB_STORAGE_LIMIT_LABEL, values: { free: "100 MB", standard: "5 GB", pro: "Custom" } },
  { label: "Max KB file size", values: { free: "5 MB", standard: "50 MB", pro: "Custom" } },
  { label: "Support", values: { free: "Community", standard: "Email", pro: "Priority" } },
  {
    label: "Decision trace",
    values: { free: "Supported", standard: "Supported", pro: "Supported" },
  },
  { label: "On-prem deployment", values: { free: null, standard: null, pro: null } },
  { label: "Custom integrations", values: { free: null, standard: null, pro: "Included" } },
  { label: "SSO", values: { free: null, standard: null, pro: "Included" } },
];

export function PricingSection() {
  const { data: user } = useCurrentUser({ enabled: true });
  const activeTier: TierName = resolveTier(user?.user_type);
  // Pre-GA visibility gate. Independent of the Marketplace URL --
  // set MEKO_ENABLE_STANDARD_TIER=true to show the tile in dev even
  // before the real listing URL is wired up.
  const showStandard = STANDARD_TIER_ENABLED;

  const gridCols = showStandard ? "md:grid-cols-3" : "md:grid-cols-2";

  // Deep-link highlight (MEKO-809). TierLimitBanner lands the user here
  // with ?highlightTier=standard; useArrivalFlash scrolls the Standard
  // card into view and pulses it for ~1.6s so the next action is
  // unmissable. The pro variant of the banner opens SALES_CONTACT_URL
  // directly (no Settings hop), so ?highlightTier=pro only reaches
  // this page from a hand-typed URL — supported for completeness but
  // not generated anywhere today.
  //
  // Dropping ?highlightTier=standard when the Standard tile is hidden
  // (STANDARD_TIER_ENABLED off): otherwise the mobile <Tabs defaultValue>
  // below would set an unknown tab value with no trigger/content and
  // render nothing, and the hook would run active:true against a
  // never-mounted node. Free targets are also dropped — Free is never
  // a useful highlight destination.
  const [searchParams, setSearchParams] = useSearchParams();
  const highlightRaw = searchParams.get(HIGHLIGHT_TIER_PARAM);
  const highlightTier: Exclude<TierColumn, "free"> | null =
    highlightRaw === "pro"
      ? "pro"
      : highlightRaw === "standard" && showStandard
        ? "standard"
        : null;
  const clearHighlight = useCallback(() => {
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        next.delete(HIGHLIGHT_TIER_PARAM);
        return next;
      },
      { replace: true },
    );
  }, [setSearchParams]);
  // Two independent arrival-flash hooks (desktop + mobile). The old
  // single-ref version attached the SAME ref object to both the
  // desktop-grid TierCard and the mobile TabsContent TierCard; React
  // commits refs in child order, so ref.current ended up pointing at
  // the mobile node. On >=md viewports that node is `md:hidden`
  // (display:none), so scrollIntoView was a no-op and the Upgrade
  // deep-link never scrolled on desktop. One flash key per layout
  // means each ref targets its OWN visible node for that viewport —
  // the hidden layout's scroll fires against a display:none element
  // and silently no-ops, which is fine.
  const clearedRef = useRef(false);
  const onFlashDone = useCallback(() => {
    if (clearedRef.current) return;
    clearedRef.current = true;
    clearHighlight();
  }, [clearHighlight]);
  const { ref: highlightRefDesktop, flashing: flashingDesktop } = useArrivalFlash<HTMLElement>({
    flashKey: highlightTier ? `desktop:${highlightTier}` : undefined,
    active: highlightTier !== null,
    onDone: onFlashDone,
    block: "center",
    debugLabel: "PricingTierCard[desktop]",
  });
  const { ref: highlightRefMobile, flashing: flashingMobile } = useArrivalFlash<HTMLElement>({
    flashKey: highlightTier ? `mobile:${highlightTier}` : undefined,
    active: highlightTier !== null,
    onDone: onFlashDone,
    block: "center",
    debugLabel: "PricingTierCard[mobile]",
  });

  return (
    <section className="rounded-xl border border-border bg-card p-5">
      <header className="mb-4">
        <h2 className="text-sm font-semibold text-foreground">Pricing</h2>
        <p className="mt-0.5 text-xs text-muted-foreground">
          {activeTier === "free"
            ? "You're on the Free tier. All plan limits are account totals — they don't reset each month. Upgrade for higher limits, more datapacks, and email support."
            : activeTier === "standard"
              ? "You're on Standard. All plan limits are account totals — they don't reset each month. Need SSO, on-prem, or higher limits? Talk to our team about Pro."
              : "Compare Meko plans below. All plan limits are account totals — they don't reset each month."}
        </p>
      </header>

      {/* TooltipProvider wraps the tier grid so the Stripe "Coming soon"
          tooltip inside the Standard tier's purchase-options strip renders.
          Kept at this level (rather than per-button) so a future card can
          also use tooltips without re-adding a provider. */}
      <TooltipProvider delayDuration={150}>
        {/* Desktop (≥ md): tiles side-by-side. */}
        <div className={cn("hidden md:grid md:gap-4", gridCols)}>
          <TierCard column="free" activeTier={activeTier} />
          {showStandard && (
            <TierCard
              column="standard"
              activeTier={activeTier}
              highlightRef={highlightTier === "standard" ? highlightRefDesktop : undefined}
              flashing={highlightTier === "standard" && flashingDesktop}
            />
          )}
          <TierCard
            column="pro"
            activeTier={activeTier}
            highlightRef={highlightTier === "pro" ? highlightRefDesktop : undefined}
            flashing={highlightTier === "pro" && flashingDesktop}
          />
        </div>

        {/* Mobile (< md): tabs so one card renders at full viewport width.
            A highlight target also drives the initial tab so the pulsed
            card is the one on screen — otherwise the flash fires behind
            an inactive tab the user never looks at. */}
        <Tabs
          defaultValue={
            highlightTier ??
            (activeTier === "pro" ? "pro" : activeTier === "standard" ? "standard" : "free")
          }
          className="md:hidden"
        >
          <TabsList className={cn("grid w-full", showStandard ? "grid-cols-3" : "grid-cols-2")}>
            <TabsTrigger value="free">Free</TabsTrigger>
            {showStandard && <TabsTrigger value="standard">Standard</TabsTrigger>}
            <TabsTrigger value="pro">Pro</TabsTrigger>
          </TabsList>
          <TabsContent value="free" className="mt-3">
            <TierCard column="free" activeTier={activeTier} />
          </TabsContent>
          {showStandard && (
            <TabsContent value="standard" className="mt-3">
              <TierCard
                column="standard"
                activeTier={activeTier}
                highlightRef={highlightTier === "standard" ? highlightRefMobile : undefined}
                flashing={highlightTier === "standard" && flashingMobile}
              />
            </TabsContent>
          )}
          <TabsContent value="pro" className="mt-3">
            <TierCard
              column="pro"
              activeTier={activeTier}
              highlightRef={highlightTier === "pro" ? highlightRefMobile : undefined}
              flashing={highlightTier === "pro" && flashingMobile}
            />
          </TabsContent>
        </Tabs>
      </TooltipProvider>
    </section>
  );
}

function TierCard({
  column,
  activeTier,
  highlightRef,
  flashing,
}: {
  column: TierColumn;
  activeTier: TierName;
  /** Attached by PricingSection to one card per layout (desktop grid
   *  and mobile tabs are both mounted; refs are per-layout). On the
   *  hidden layout the scroll is a no-op against a display:none node,
   *  so only the visible card actually scrolls into view and flashes.
   *  Target comes from ?highlightTier=… on the banner's deep-link. */
  highlightRef?: React.Ref<HTMLElement>;
  flashing?: boolean;
}) {
  const isCurrent = activeTier === column;
  const meta = tierMeta(column);

  return (
    <article
      ref={highlightRef}
      className={cn(
        "flex flex-col rounded-lg border p-4 transition-shadow",
        column === "standard"
          ? "border-brand/40 bg-brand/[0.05]"
          : "border-border bg-background/40",
        // Deep-link flash: brand-colour ring + a subtle pulse while
        // useArrivalFlash holds `flashing` true (1.6s). Falls back to
        // nothing after the one-shot timer clears, so the card settles
        // into its normal styling.
        flashing && "ring-4 ring-brand/70 shadow-lg animate-pulse",
      )}
    >
      <header className="mb-3 flex min-h-7 flex-col">
        <h3 className="text-base font-semibold text-foreground">{meta.title}</h3>
        <p className="text-xs text-muted-foreground">{meta.price}</p>
      </header>

      {/*
        CTA banner slot — a fixed-height strip that ALWAYS renders on
        every card so the tier rows below start at the same Y across
        Free / Standard / Pro. Before this, Standard's purchase strip
        pushed its own row list down and the same-named rows
        ("Datapacks", "Add conversations", ...) stopped aligning
        horizontally across columns. `min-h-[4.5rem]` (72px) is chosen
        to fit the tallest content (Standard's 3-way purchase chip row
        with icon-over-label) without cutting any variant off; simpler
        contents (a single Current chip, a Talk-to-team link) centre
        vertically in the same slot.
      */}
      <div className="mb-4 flex min-h-[4.5rem] items-center">
        <TierCtaBanner column={column} activeTier={activeTier} isCurrent={isCurrent} />
      </div>

      <dl
        className={cn(
          "text-sm",
          column === "standard" ? "divide-y divide-brand/15" : "divide-y divide-border/60",
        )}
      >
        {TIER_ROWS.map((row) => {
          const val = row.values[column];
          return (
            <div key={row.label} className="flex items-center justify-between gap-3 py-2">
              <dt className="text-muted-foreground">{row.label}</dt>
              <dd
                className={
                  val
                    ? "font-mono text-xs font-medium text-foreground"
                    : "font-mono text-xs text-muted-foreground/60"
                }
              >
                {val ?? "—"}
              </dd>
            </div>
          );
        })}
      </dl>
    </article>
  );
}

function tierMeta(column: TierColumn): { title: string; price: string } {
  switch (column) {
    case "free":
      return { title: "Free", price: "$0" };
    case "standard":
      return { title: "Standard", price: "$25 / month" };
    case "pro":
      return { title: "Pro", price: "Custom pricing" };
  }
}

// AWS buyer console — where users manage / cancel their Marketplace
// subscription. There's no cancellation API on our side; only AWS can
// end a Contract subscription, so we deep-link to the console instead.
const AWS_BUYER_SUBSCRIPTIONS_URL =
  "https://console.aws.amazon.com/marketplace/home?#/subscriptions";

// CTA banner rendered inside every tier card's fixed-height CTA slot.
// Content varies by (column, activeTier); the slot itself has a
// min-height (see TierCard) so tier rows below stay aligned across
// Free / Standard / Pro. The banner content always fills the slot's
// width so the visual weight matches across columns.
//
// Routing:
//   isCurrent + free       → "Current plan" chip
//   isCurrent + standard   → "Current" chip + "Manage subscription" link
//                            (AWS Marketplace is the only cancel path)
//   isCurrent + pro        → "Current plan" chip
//   free viewer + standard → StandardPurchaseOptions (3 chips)
//   free viewer + pro      → "Talk to our team" button
//   any other combination  → subtle "Included in your plan" or empty spacer
//                            so the row still occupies its slot
function TierCtaBanner({
  column,
  activeTier,
  isCurrent,
}: {
  column: TierColumn;
  activeTier: TierName;
  isCurrent: boolean;
}) {
  if (isCurrent) {
    const chip = (
      <span
        className="inline-flex items-center gap-1 rounded-full border border-brand/40 bg-brand/10 px-2.5 py-1 text-[10px] font-medium uppercase tracking-wider text-brand"
        aria-label="Your current plan"
      >
        <Check className="h-3 w-3" aria-hidden />
        Current plan
      </span>
    );
    if (column === "standard") {
      // Standard subscribers see the Current chip PLUS a link to the
      // AWS Marketplace buyer console — cancel/change of a Contract
      // subscription can only happen there, so surface it inline.
      return (
        <div className="flex w-full flex-col items-start gap-1.5">
          {chip}
          <a
            href={AWS_BUYER_SUBSCRIPTIONS_URL}
            target="_blank"
            rel="noopener noreferrer"
            title="Opens the AWS Marketplace buyer console"
            className="inline-flex items-center gap-0.5 text-[11px] font-medium text-brand hover:underline"
          >
            Manage subscription
            <ArrowUpRight className="h-3 w-3" aria-hidden />
          </a>
        </div>
      );
    }
    return <div className="flex w-full">{chip}</div>;
  }

  if (column === "standard" && activeTier === "free") {
    return <StandardPurchaseOptions />;
  }

  if (column === "pro") {
    // Pro is "custom pricing / talk to us" for everyone who isn't
    // already on Pro — Standard subscribers upgrading, Free viewers
    // scoping the ceiling. Compact link-style CTA fills the slot.
    return (
      <Button
        asChild
        variant="link"
        size="sm"
        className="h-auto gap-1 p-0 text-xs font-medium text-brand hover:text-brand/80"
      >
        <a href={SALES_CONTACT_URL} target="_blank" rel="noopener noreferrer">
          Talk to our team
          <ArrowUpRight className="h-3.5 w-3.5" aria-hidden />
        </a>
      </Button>
    );
  }

  // Free column when the viewer isn't on Free (Standard/Pro users
  // looking down at the base tier). Empty — the slot's min-height still
  // reserves space so rows below stay aligned.
  return <div aria-hidden />;
}

// Purchase-options strip — three mutually-exclusive payment paths for
// upgrading to Standard, rendered inside the Standard card's CTA banner
// slot for viewers on Free. Slim horizontal 3-up so the strip is the
// same vertical size as the "Current" chip on Free's card and the
// "Talk to our team" link on Pro's card, keeping the tier rows below
// aligned across all three columns.
//
// The wrapping container has no visible border/heading anymore — the
// slot's minimum height and the tier card's own brand tint already
// signal "this is the CTA area", and dropping the extra chrome saves
// vertical space so the chips fit alongside icons + labels without
// getting taller than the sibling banners.
function StandardPurchaseOptions() {
  const [awsDialogOpen, setAwsDialogOpen] = useState(false);
  const awsDisabled = !AWS_MARKETPLACE_LISTING_URL;

  return (
    <>
      <div className="grid w-full grid-cols-3 gap-1.5">
        {/* AWS Marketplace — primary route today. Opens the same primer
            dialog as before so the two-step "subscribe → Set up your
            account" explainer isn't duplicated inline. */}
        <PurchaseChip
          icon={<Cloud className="h-3.5 w-3.5" aria-hidden />}
          label="AWS Marketplace"
          onClick={() => setAwsDialogOpen(true)}
          disabled={awsDisabled}
          disabledTooltip={awsDisabled ? "AWS Marketplace URL not configured yet" : undefined}
        />

        {/* Stripe — coming soon. Kept in the strip (rather than hidden)
            so the roadmap stays visible to buyers evaluating tiers
            today. Tooltip explains the state; click is disabled. */}
        <PurchaseChip
          icon={<CreditCard className="h-3.5 w-3.5" aria-hidden />}
          label="Stripe"
          disabled
          disabledTooltip="Coming soon"
        />

        {/* Contact Us — deep-links to the same sales form Pro's "Talk
            to our team" uses. Buyers with procurement rules that
            require an order form / MSA end up here. */}
        <PurchaseChip
          icon={<Mail className="h-3.5 w-3.5" aria-hidden />}
          label="Contact Us"
          href={SALES_CONTACT_URL}
        />
      </div>

      <UpgradeToStandardDialog open={awsDialogOpen} onOpenChange={setAwsDialogOpen} />
    </>
  );
}

// One button in the purchase-options grid. Absorbs three shapes:
//   href      — external link (Contact Us)
//   onClick   — opens a dialog (AWS Marketplace)
//   disabled  — surfaces `disabledTooltip` and swallows clicks (Stripe)
// so the visual chip stays identical across all three states.
function PurchaseChip({
  icon,
  label,
  href,
  onClick,
  disabled,
  disabledTooltip,
}: {
  icon: React.ReactNode;
  label: string;
  href?: string;
  onClick?: () => void;
  disabled?: boolean;
  disabledTooltip?: string;
}) {
  const base =
    "flex h-full flex-col items-center justify-center gap-1 rounded-md border px-2 py-2 text-center text-[11px] font-medium leading-tight transition-colors";
  const enabledCls =
    "border-brand/40 bg-background text-foreground hover:border-brand hover:bg-brand/10";
  const disabledCls =
    "cursor-not-allowed border-border/60 bg-background/40 text-muted-foreground opacity-70";

  // Icon + label + external arrow (only for enabled).
  const body = (
    <>
      <span className={cn("text-brand", disabled && "text-muted-foreground")}>{icon}</span>
      <span className="line-clamp-2">
        {label}
        {!disabled && href ? (
          <ArrowUpRight className="ml-0.5 inline h-2.5 w-2.5" aria-hidden />
        ) : null}
      </span>
    </>
  );

  if (disabled) {
    // Wrap the disabled chip in a Tooltip so hover explains why it's
    // greyed out. Radix tooltip needs a focusable trigger; a plain div
    // works because TooltipTrigger sets tabIndex on its child.
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <div className={cn(base, disabledCls)} aria-disabled tabIndex={0}>
            {body}
          </div>
        </TooltipTrigger>
        {disabledTooltip ? (
          <TooltipContent side="bottom" className="max-w-[180px] text-center">
            {disabledTooltip}
          </TooltipContent>
        ) : null}
      </Tooltip>
    );
  }

  if (href) {
    return (
      <a href={href} target="_blank" rel="noopener noreferrer" className={cn(base, enabledCls)}>
        {body}
      </a>
    );
  }

  return (
    <button type="button" onClick={onClick} className={cn(base, enabledCls)}>
      {body}
    </button>
  );
}

// AWS Marketplace primer dialog. Same copy as the original single-button
// primer — factored out so `StandardPurchaseOptions` can drive it from
// the chip click without duplicating the two-step explainer, and so a
// future test can mount it in isolation.
function UpgradeToStandardDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
}) {
  const handleContinue = () => {
    window.open(AWS_MARKETPLACE_LISTING_URL, "_blank", "noopener,noreferrer");
    onOpenChange(false);
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Upgrade to Standard on AWS Marketplace</DialogTitle>
          <DialogDescription>
            Meko Standard is billed through AWS Marketplace. You'll finish signup on AWS in a new
            tab, then come back here.
          </DialogDescription>
        </DialogHeader>

        <ol className="ml-5 list-decimal space-y-2 text-sm text-foreground">
          <li>
            We'll open the Meko listing on AWS Marketplace. Sign in with the AWS account you want to
            bill.
          </li>
          <li>Accept the contract to subscribe.</li>
          <li>
            <span className="font-semibold text-foreground">Important:</span> after AWS shows the
            purchase confirmation, click{" "}
            <span className="font-semibold">&ldquo;Set up your account&rdquo;</span> to activate
            your Standard tier in Meko. Without this step your plan stays on Free.
          </li>
        </ol>

        <p className="text-xs text-muted-foreground">
          You can close and reopen this dialog if you need to. Your Meko session stays signed in in
          this tab.
        </p>

        <DialogFooter className="gap-2 sm:justify-end">
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="button" onClick={handleContinue} className="gap-1">
            Continue to AWS Marketplace
            <ArrowUpRight className="h-3.5 w-3.5" aria-hidden />
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
