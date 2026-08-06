// Pricing section on the Settings page.
//
// Renders Free / Standard / Pro tier cards. Standard is the AWS
// Marketplace $25/mo tier; hidden entirely when
// MEKO_AWS_MARKETPLACE_URL is unset (nothing to link the Upgrade
// button to). The "Current" chip follows the signed-in user's
// user_type from /auth/me. Every row value matches the caps enforced
// by lib/freeTier.ts on the FE and api_server/internal/service/free_tier.go on the BE.

import { useState } from "react";
import { ArrowUpRight, Check } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/utils";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import {
  AWS_MARKETPLACE_LISTING_URL,
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
  { label: "Retrieval requests", values: { free: "10,000", standard: "100,000", pro: "Custom" } },
  { label: "Workbench tokens", values: { free: "10M", standard: "50M", pro: "Custom" } },
  { label: "KB storage", values: { free: "100 MB", standard: "5 GB", pro: "Custom" } },
  { label: "Max KB file size", values: { free: "5 MB", standard: "50 MB", pro: "Custom" } },
  { label: "Support", values: { free: "Community", standard: "Email", pro: "Priority" } },
  { label: "Decision trace", values: { free: "Supported", standard: "Supported", pro: "Supported" } },
  { label: "On-prem deployment", values: { free: null, standard: null, pro: null } },
  { label: "Audit logs", values: { free: null, standard: "30 days", pro: "Included" } },
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

  return (
    <section className="rounded-xl border border-border bg-card p-5">
      <header className="mb-4">
        <h2 className="text-sm font-semibold text-foreground">Pricing</h2>
        <p className="mt-0.5 text-xs text-muted-foreground">
          {activeTier === "free"
            ? "You're on the Free tier. Upgrade for higher limits, more datapacks, and email support."
            : activeTier === "standard"
              ? "You're on Standard. Need SSO, on-prem, or higher limits? Talk to our team about Pro."
              : "Compare Meko plans below."}
        </p>
      </header>

      {/* Desktop (≥ md): tiles side-by-side. */}
      <div className={cn("hidden md:grid md:gap-4", gridCols)}>
        <TierCard column="free" activeTier={activeTier} />
        {showStandard && <TierCard column="standard" activeTier={activeTier} />}
        <TierCard column="pro" activeTier={activeTier} />
      </div>

      {/* Mobile (< md): tabs so one card renders at full viewport width. */}
      <Tabs defaultValue={activeTier === "pro" ? "pro" : activeTier === "standard" ? "standard" : "free"} className="md:hidden">
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
            <TierCard column="standard" activeTier={activeTier} />
          </TabsContent>
        )}
        <TabsContent value="pro" className="mt-3">
          <TierCard column="pro" activeTier={activeTier} />
        </TabsContent>
      </Tabs>
    </section>
  );
}

function TierCard({ column, activeTier }: { column: TierColumn; activeTier: TierName }) {
  const isCurrent = activeTier === column;
  const meta = tierMeta(column);

  return (
    <article
      className={cn(
        "rounded-lg border p-4",
        column === "standard"
          ? "border-brand/40 bg-brand/[0.05]"
          : "border-border bg-background/40",
      )}
    >
      <header className="mb-3 flex min-h-7 items-center justify-between gap-2">
        <div className="flex flex-col">
          <h3 className="text-base font-semibold text-foreground">{meta.title}</h3>
          <p className="text-xs text-muted-foreground">{meta.price}</p>
        </div>
        <TierBadge column={column} isCurrent={isCurrent} />
      </header>

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

// Badge in the top-right of each card. "Current" wins for the active
// tier; Standard shows an Upgrade button that deep-links to AWS
// Marketplace, Pro shows "Talk to our team", Free shows nothing.
// When Standard is the CURRENT plan we also surface a "Manage in AWS"
// link so users have a one-click path to change / cancel the sub.
function TierBadge({ column, isCurrent }: { column: TierColumn; isCurrent: boolean }) {
  if (isCurrent) {
    const chip = (
      <span
        className="inline-flex items-center gap-1 rounded-full border border-brand/40 bg-brand/10 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wider text-brand"
        aria-label="Your current plan"
      >
        <Check className="h-3 w-3" aria-hidden />
        Current
      </span>
    );
    // Standard subscribers manage or cancel via AWS Marketplace, not
    // Meko — surface a link to the buyer console right beside the
    // Current chip.
    if (column === "standard") {
      return (
        <div className="flex flex-col items-end gap-1">
          {chip}
          <a
            href={AWS_BUYER_SUBSCRIPTIONS_URL}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-0.5 text-[11px] font-medium text-brand hover:underline"
          >
            Manage in AWS
            <ArrowUpRight className="h-3 w-3" aria-hidden />
          </a>
        </div>
      );
    }
    return chip;
  }
  if (column === "standard") {
    // If the URL isn't wired yet, disable the button instead of
    // shipping a link that reloads the current page. Tile visibility
    // is a separate flag (STANDARD_TIER_ENABLED) so the tile can
    // render for UI testing before the real listing URL lands.
    if (!AWS_MARKETPLACE_LISTING_URL) {
      return (
        <Button
          type="button"
          size="sm"
          disabled
          className="h-7 gap-1 px-2.5 text-xs font-medium"
          title="AWS Marketplace URL not configured yet"
        >
          Upgrade
        </Button>
      );
    }
    // Show a dialog before deep-linking to AWS so users understand
    // the two-step flow (subscribe on AWS, then click "Set up your
    // account" to activate the entitlement in Meko). Without this
    // primer, buyers routinely subscribe, close the AWS tab, and
    // wonder why their tier hasn't changed.
    return <UpgradeToStandardButton />;
  }
  if (column === "pro") {
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
  return null;
}

// Standard-tile Upgrade CTA. Opens a primer dialog before deep-linking
// to AWS Marketplace so users know what to expect on the other side:
// the AWS subscribe step alone does NOT activate their tier -- they
// have to explicitly click "Set up your account" on AWS's confirmation
// page for AWS to POST the fulfillment token to our /marketplace/register
// handler. Without this primer, buyers subscribe, close the AWS tab,
// and are surprised their tier is still Free.
function UpgradeToStandardButton() {
  const [open, setOpen] = useState(false);

  const handleContinue = () => {
    // Open in a new tab so users can flip back to Meko without losing
    // the settings page they were on. Same-tab would work too but is
    // more disorienting.
    window.open(AWS_MARKETPLACE_LISTING_URL, "_blank", "noopener,noreferrer");
    setOpen(false);
  };

  return (
    <>
      <Button
        type="button"
        size="sm"
        onClick={() => setOpen(true)}
        className="h-7 gap-1 bg-brand px-2.5 text-xs font-medium text-brand-foreground hover:bg-brand/90"
      >
        Upgrade
        <ArrowUpRight className="h-3.5 w-3.5" aria-hidden />
      </Button>

      <Dialog open={open} onOpenChange={setOpen}>
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
              We'll open the Meko listing on AWS Marketplace. Sign in with the AWS account you want
              to bill.
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
            You can close and reopen this dialog if you need to. Your Meko session stays signed in
            in this tab.
          </p>

          <DialogFooter className="gap-2 sm:justify-end">
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button type="button" onClick={handleContinue} className="gap-1">
              Continue to AWS Marketplace
              <ArrowUpRight className="h-3.5 w-3.5" aria-hidden />
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
