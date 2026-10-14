// Shared "you've hit the tier limit" banner used wherever a tier cap
// blocks an action (MEKO-809). Renders the metric line, a primary
// Upgrade / Talk-to-our-team CTA, and a secondary Contact-us link.
//
// Two destinations for the primary CTA:
//
//   - Free → Standard: button lands on /settings?highlightTier=standard;
//     PricingSection scrolls to the Standard card and pulses it. Falls
//     back to the pro variant below when the Standard tile is hidden
//     (STANDARD_TIER_ENABLED off) — otherwise we'd route Scott to a
//     Settings page that doesn't show Standard anywhere.
//   - Standard → Pro (or Free when Standard is gated off): Pro has no
//     self-serve upgrade, so the button is "Talk to our team" and
//     opens SALES_CONTACT_URL directly in a new tab. Keeps the same
//     one-click promise the Standard variant has (no double-hop
//     through Settings to another "Talk to our team" link).
//
// Kept in its own .tsx file because `lib/freeTier.ts` is pure data.

import { Link } from "react-router-dom";
import { ArrowUpRight, ExternalLink } from "lucide-react";
import { Button } from "@/components/ui/button";
import { WarningBanner } from "@/components/ui/warning-banner";
import { useAccountUsage } from "@/hooks/useAccountUsage";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { ROUTES } from "@/app/routes/paths";
import { isMekoApiRequestError } from "@/lib/api/mekoApi";
import {
  AMP_COMPUTE_BANNER_KIND,
  SALES_CONTACT_URL,
  STANDARD_TIER_ENABLED,
  limitsFor,
  nextTierFor,
  resolveTier,
  type TierBannerKind,
} from "@/lib/freeTier";

/** Deep-link param PricingSection reads to scroll + flash a tier card. */
export const HIGHLIGHT_TIER_PARAM = "highlightTier";

export function TierLimitBanner({
  kind,
  className,
  metricLine,
}: {
  kind: TierBannerKind;
  className?: string;
  /** Override the usage-derived metric line. AMP compute's vCPU usage comes from
   *  the account-wide AMP core budget (a different endpoint from `usage`), so that surface passes it. */
  metricLine?: string;
}) {
  const { data: user } = useCurrentUser({ enabled: true });
  const { data: usage } = useAccountUsage();
  const target = resolveBannerTarget(user?.user_type);
  // Source tier drives the "You've reached the X tier limit…" lead;
  // target drives the CTA suffix. Picking them independently fixes the
  // Standard-gated-off case: a Free user who falls back to the Pro CTA
  // still sees "Free tier limit…", not "Standard tier limit…" from the
  // old TIER_BANNER_MESSAGES.pro row (which was written assuming the
  // viewer is a Standard user).
  const message = formatBannerMessage(sourceTierLabel(user?.user_type), target, kind);
  const metric = metricLine ?? (usage ? formatMetricLine(kind, usage, user?.user_type) : null);

  return (
    <WarningBanner className={className}>
      <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between sm:gap-4">
        <div className="min-w-0 flex-1 space-y-0.5">
          <p>{message}</p>
          {metric ? (
            <p className="text-xs text-amber-700/85 dark:text-amber-200/80 tabular-nums">
              {metric}
            </p>
          ) : null}
        </div>
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          {target === "standard" ? (
            <Button
              asChild
              size="sm"
              className="gap-1.5 bg-primary text-primary-foreground hover:bg-primary/90"
            >
              <Link to={`${ROUTES.settings}?${HIGHLIGHT_TIER_PARAM}=standard`}>
                Upgrade to Standard
                <ArrowUpRight className="h-3.5 w-3.5" aria-hidden />
              </Link>
            </Button>
          ) : (
            // Pro has no self-serve upgrade — the Pro card itself just
            // shows a "Talk to our team" link. Open the HubSpot form
            // directly instead of bouncing through Settings. Keeps the
            // promise the banner's primary action takes you one click
            // closer to resolution.
            <Button
              asChild
              size="sm"
              className="gap-1.5 bg-primary text-primary-foreground hover:bg-primary/90"
            >
              <a href={SALES_CONTACT_URL} target="_blank" rel="noopener noreferrer">
                Talk to our team
                <ExternalLink className="h-3.5 w-3.5" aria-hidden />
              </a>
            </Button>
          )}
        </div>
      </div>
    </WarningBanner>
  );
}

/** Which "next tier" the banner's CTA should point at. When Standard is
 *  pre-GA (STANDARD_TIER_ENABLED off), a Free user would otherwise be
 *  routed to a Settings page with no Standard tile to click — fall
 *  back to the Pro variant so there's at least a working path (the
 *  HubSpot form). Standard → always Pro. */
function resolveBannerTarget(userType?: string | null): "standard" | "pro" {
  const next = nextTierFor(userType);
  if (next === "standard" && !STANDARD_TIER_ENABLED) return "pro";
  return next;
}

/** The user's CURRENT tier name for the "You've reached the <X> tier limit…"
 *  lead. Free/Standard/Pro are labelled from the resolved tier — AMP compute
 *  caps gate Pro too (Pro → Enterprise), so unlike the other kinds this banner
 *  does render for a Pro user. Enterprise and an unresolved/unknown user_type
 *  fall back to "Standard", unchanged from the pre-AMP behaviour and matching
 *  the Pro-target CTA resolveBannerTarget already gives them. */
function sourceTierLabel(userType?: string | null): "Free" | "Standard" | "Pro" {
  const t = resolveTier(userType);
  if (t === "free") return "Free";
  if (t === "pro") return "Pro";
  // standard, enterprise, unknown, still-loading — same label as before AMP.
  return "Standard";
}

const SUBJECT_COPY: Record<TierBannerKind, string> = {
  datapack: "datapack creation",
  kbStorage: "knowledge-base storage",
  conversations: "conversations",
  tokenBurn: "WorkbenchLM token usage",
  [AMP_COMPUTE_BANNER_KIND]: "AMP compute (vCPU)",
};

/** Compose banner copy from (source tier × target × kind). Replaces the
 *  TIER_BANNER_MESSAGES lookup, which was keyed only on the target tier
 *  and hard-coded "Upgrade to Standard" / "Upgrade to Pro" tails. That
 *  broke on the Standard-gated-off path: a Free user falling back to
 *  the Pro CTA was shown the Pro-target string, which starts with
 *  "You've reached the Standard tier limit…". Keeping copy local to
 *  this component also fixes the CTA/text mismatch for Standard users
 *  (old copy said "Upgrade to Pro"; the button has always been "Talk
 *  to our team"). */
function formatBannerMessage(
  sourceLabel: "Free" | "Standard" | "Pro",
  target: "standard" | "pro",
  kind: TierBannerKind,
): string {
  const action =
    target === "standard"
      ? "Upgrade to Standard for higher limits"
      : "Talk to our team about higher limits";
  return `You've reached the ${sourceLabel} tier limit on ${SUBJECT_COPY[kind]}. ${action}.`;
}

/** Detect a 402 tier-limit error from the api_server and map it to the
 *  banner kind to render. Returns null for anything else (plain errors
 *  render as before). Codes come from
 *  api_server/internal/service/free_tier.go (lines 301-309): the only
 *  codes api_server ever emits are {FREE,STANDARD}_TIER_DATAPACK_LIMIT,
 *  {FREE,STANDARD}_TIER_KB_STORAGE_LIMIT, and {FREE,STANDARD,PRO}_TIER_CORE_LIMIT.
 *  Pattern-matched by suffix so both tier variants land on the same
 *  banner kind. Conversation / token-burn caps are enforced inside the
 *  inference_gateway and MCP tiers, not api_server — so no 402 for
 *  those surfaces reaches this detector today (the "conversations" /
 *  "tokenBurn" banner kinds are still used by the WorkbenchLM header's
 *  own direct `hasHit*` check, not by this error path). */
export function tierBannerKindForError(err: unknown): TierBannerKind | null {
  if (!isMekoApiRequestError(err)) return null;
  if (err.httpStatus !== 402) return null;
  const code = err.code ?? "";
  if (code.endsWith("DATAPACK_LIMIT")) return "datapack";
  if (code.endsWith("KB_STORAGE_LIMIT")) return "kbStorage";
  // FREE/STANDARD/PRO_TIER_CORE_LIMIT — the per-tier AMP vCPU cap.
  if (code.endsWith("CORE_LIMIT")) return AMP_COMPUTE_BANNER_KIND;
  return null;
}

/** Compact "X / Y <unit>" formatter for the usage-backed banner kinds. Reads
 *  the active caps from `limitsFor(userType)` so Free vs Standard numbers both
 *  render correctly without a second code path, and returns null when a cap is
 *  Infinity (Pro + above on those kinds). The ampCompute kind is NOT usage-backed
 *  — its vCPU line comes from the core budget via the metricLine prop — so it
 *  returns null here regardless of tier. */
function formatMetricLine(
  kind: TierBannerKind,
  usage: NonNullable<ReturnType<typeof useAccountUsage>["data"]>,
  userType: string | null | undefined,
): string | null {
  const limits = limitsFor(userType);
  switch (kind) {
    case "datapack": {
      const used = Math.max(usage.datapack_creations ?? 0, usage.datapack_count);
      if (!isFinite(limits.datapacks)) return null;
      return `${formatCount(used)} / ${formatCount(limits.datapacks)} datapacks used`;
    }
    case "kbStorage": {
      if (!isFinite(limits.kbStorageBytes)) return null;
      return `${formatBytes(usage.kb_storage_file_size)} / ${formatBytes(limits.kbStorageBytes)} knowledge storage used`;
    }
    case "conversations": {
      if (!isFinite(limits.conversations)) return null;
      return `${formatCount(usage.num_conversations)} / ${formatCount(limits.conversations)} conversations used`;
    }
    case "tokenBurn": {
      if (!isFinite(limits.tokenBurn)) return null;
      return `${COMPACT.format(usage.token_burn)} / ${COMPACT.format(limits.tokenBurn)} WorkbenchLM tokens used`;
    }
    case AMP_COMPUTE_BANNER_KIND:
      // Derived from the account-wide AMP core budget and passed via the metricLine
      // prop, not from account usage — nothing to compute here.
      return null;
  }
}

const COMPACT = new Intl.NumberFormat("en", { notation: "compact", maximumFractionDigits: 1 });

function formatCount(n: number): string {
  return n.toLocaleString("en-US");
}

/** Matches the precision shown in PricingSection's tier-card rows.
 *  Three bands:
 *   - >= 1 GB:  "5 GB" when integer, else one decimal ("1.4 GB").
 *   - 100-1023 MB: whole MB ("100 MB", "250 MB").
 *   - < 100 MB:  one decimal ("95.0 MB", "4.3 MB") so a partial-cap
 *     metric line reads with meaningful precision. */
function formatBytes(bytes: number): string {
  const mb = bytes / (1024 * 1024);
  if (mb >= 1024) {
    const gb = mb / 1024;
    return Number.isInteger(gb) ? `${gb} GB` : `${gb.toFixed(1)} GB`;
  }
  if (mb >= 100) return `${Math.round(mb)} MB`;
  return `${mb.toFixed(1)} MB`;
}

/** Back-compat alias — existing callers importing `FreeTierLimitBanner`
 *  keep working while we migrate. Prefer `TierLimitBanner` in new code. */
export const FreeTierLimitBanner = TierLimitBanner;
