// Pricing section on the Settings page.
//
// Renders two side-by-side tier cards:
//   - "Free" — detailed limit table reflecting the active free-tier
//     caps enforced by `lib/freeTier.ts` (matched to the BE counters
//     returned by `/auth/me/usage`). Every row's value should align
//     with what the gated surfaces enforce so users see honest copy.
//   - "Pro" — sales-led tier. "Talk to our team" CTA opens the
//     marketing contact page in a new tab.
//
// This is a compact summary, not a full plan-picker — the full
// catalog (Starter / Growth / Scale / Enterprise) lives at /billing
// via `meko_ui/src/features/billing/`. We deliberately don't link
// the two: Settings shows a TL;DR for the user's current tier, the
// billing page handles upgrades.
//
// Tier resolution note: every user is treated as "free" today
// (matches the gating in `freeTier.ts`). When `user_type` from
// `/auth/me` starts driving tier-aware UI, this card should branch
// on it and highlight the active tier — out of scope for this PR.

import { ArrowUpRight, Check } from "lucide-react";
import { Button } from "@/components/ui/button";

import { SALES_CONTACT_URL } from "@/lib/freeTier";

type Row = {
  label: string;
  /** Plain string for set values (e.g. "1,000"); `null` renders as the
   *  em-dash placeholder used for "not included on this tier." */
  freeValue: string | null;
};

// Row order chosen to lead with the metered caps a user actively
// consumes (conversations / retrievals / datapacks / storage), then
// drop to capability rows (support / decision trace), then the
// enterprise-only "—" rows at the bottom. Matches what the user
// expects to scan top-to-bottom.
const FREE_TIER_ROWS: ReadonlyArray<Row> = [
  { label: "Datapacks", freeValue: "3" },
  { label: "Add Conversations", freeValue: "1,000" },
  { label: "Retrieval Requests", freeValue: "10,000" },
  { label: "Workbench Tokens", freeValue: "10M" },
  { label: "KB Storage", freeValue: "100 MB" },
  { label: "Max KB file size", freeValue: "5 MB" },
  { label: "Support", freeValue: "COMMUNITY" },
  { label: "Decision Trace", freeValue: "Supported" },
  { label: "On-Prem Deployment", freeValue: null },
  { label: "Audit Logs", freeValue: null },
  { label: "Custom Integrations", freeValue: null },
  { label: "SSO", freeValue: null },
];

export function PricingSection() {
  return (
    <section className="rounded-xl border border-border bg-card p-5">
      <header className="mb-4">
        <h2 className="text-sm font-semibold text-foreground">Pricing</h2>
        <p className="mt-0.5 text-xs text-muted-foreground">
          You're on the Free tier. Need higher limits, SSO, or on-prem deployment? Talk to our team
          about Pro.
        </p>
      </header>

      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        {/* ── Free tier ─────────────────────────────────────────── */}
        <article className="rounded-lg border border-border bg-background/40 p-4">
          <header className="mb-3 flex items-center justify-between gap-2">
            <div>
              <h3 className="text-base font-semibold text-foreground">Free</h3>
            </div>
            <span
              className="inline-flex items-center gap-1 rounded-full border border-blue-500/30 bg-blue-500/10 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wider text-blue-700 dark:text-blue-300"
              aria-label="Your current plan"
            >
              <Check className="h-3 w-3" aria-hidden />
              Current
            </span>
          </header>

          <dl className="divide-y divide-border/60 text-sm">
            {FREE_TIER_ROWS.map((row) => (
              <div key={row.label} className="flex items-center justify-between gap-3 py-2">
                <dt className="text-muted-foreground">{row.label}</dt>
                <dd
                  className={
                    row.freeValue
                      ? "font-mono text-xs font-medium text-foreground"
                      : "font-mono text-xs text-muted-foreground/60"
                  }
                >
                  {row.freeValue ?? "—"}
                </dd>
              </div>
            ))}
          </dl>
        </article>

        {/* ── Pro tier ──────────────────────────────────────────── */}
        <article className="flex flex-col rounded-lg border border-blue-500/30 bg-blue-500/[0.04] p-4">
          <header className="mb-3">
            <h3 className="text-base font-semibold text-foreground">Pro</h3>
            <p className="text-xs text-muted-foreground">
              Higher limits, SSO, audit logs, custom integrations, and on-prem options.
            </p>
          </header>
          <div className="flex flex-1 items-center justify-center py-6">
            <Button asChild className="gap-2 bg-blue-600 text-white hover:bg-blue-500">
              <a href={SALES_CONTACT_URL} target="_blank" rel="noopener noreferrer">
                Talk to our team
                <ArrowUpRight className="h-4 w-4" aria-hidden />
              </a>
            </Button>
          </div>
        </article>
      </div>
    </section>
  );
}
