// UsageTrackerSection — live "consumed / max" view for every metered
// dimension of the user's current tier. Reads the same data the
// gating logic uses (useAccountUsage + FREE_/STANDARD_TIER_LIMITS)
// so the numbers here match exactly what the BE/MCP enforce.
//
// Hidden entirely when the caller is on a tier without hard caps
// (pro / enterprise / unknown) — those users have nothing to track
// against.

import {
  AlertCircle,
  Brain,
  Database,
  FileText,
  MessageSquare,
  PlusCircle,
  Search,
  Zap,
} from "lucide-react";

import { Progress } from "@/components/ui/progress";
import { cn } from "@/lib/utils";
import { formatFileSize } from "@/lib/fileSize";
import { useAccountUsage } from "@/hooks/useAccountUsage";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import {
  FREE_TIER_LIMITS,
  FREE_TIER_QUOTA_ENABLED,
  KB_STORAGE_LIMIT_LABEL,
  STANDARD_TIER_LIMITS,
  resolveTier,
  type TierLimits,
  type TierName,
} from "@/lib/freeTier";
import { isByocEnabled } from "@/lib/byoc";
import type { AccountUsage } from "@/lib/api/mekoApi";

// Threshold for the "you're getting close" amber tint. 80% is early
// enough to notice, late enough not to nag on normal usage.
const NEAR_LIMIT_FRACTION = 0.8;

// One row per metered dimension. `format` turns a raw counter into
// a display string (identity for counts, formatFileSize for bytes),
// so consumed + max render in consistent units.
type UsageRowSpec = {
  key: string;
  label: string;
  icon: React.ReactNode;
  consumed: (u: AccountUsage) => number;
  max: (l: TierLimits) => number;
  format: (n: number) => string;
};

function fmtCount(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(n >= 10_000 ? 0 : 1)}K`;
  return n.toLocaleString();
}

// Label copy matches the Pricing card's row labels so the two Settings
// cards read as a matched pair — "Datapacks" here, "Datapacks" there.
const ROWS: ReadonlyArray<UsageRowSpec> = [
  {
    key: "datapacks",
    label: "Datapacks",
    icon: <Database className="h-3.5 w-3.5" aria-hidden />,
    // Mirror the gate exactly: max(lifetime creations, current count).
    // Creations gate the quota so delete-and-recreate can't reset it, but
    // legacy users whose lifetime counter was never backfilled fall back to
    // the live count — displaying creations alone would show usage below the
    // amount already blocking creation. See freeTier.ts line for the same max.
    consumed: (u) => Math.max(u.datapack_creations ?? 0, u.datapack_count),
    max: (l) => l.datapacks,
    format: fmtCount,
  },
  {
    key: "conversations",
    label: "Add conversations",
    icon: <MessageSquare className="h-3.5 w-3.5" aria-hidden />,
    consumed: (u) => u.num_conversations,
    max: (l) => l.conversations,
    format: fmtCount,
  },
  {
    key: "workbench_tokens",
    label: "Workbench tokens",
    icon: <Zap className="h-3.5 w-3.5" aria-hidden />,
    consumed: (u) => u.token_burn,
    max: (l) => l.tokenBurn,
    format: fmtCount,
  },
  {
    key: "kb_storage",
    label: KB_STORAGE_LIMIT_LABEL,
    icon: <FileText className="h-3.5 w-3.5" aria-hidden />,
    consumed: (u) => u.kb_storage_file_size,
    max: (l) => l.kbStorageBytes,
    format: formatFileSize,
  },
  // MCP-side retrieval / write counters. Enforced by meko-mcp-server
  // (api_server never gates these) but surfaced here so users see one
  // consolidated "all my caps" view under Settings > Usage.
  {
    key: "memory_searches",
    label: "Memory searches",
    icon: <Brain className="h-3.5 w-3.5" aria-hidden />,
    consumed: (u) => u.num_memory_searches ?? 0,
    max: (l) => l.memorySearches,
    format: fmtCount,
  },
  {
    key: "kb_searches",
    label: "KB searches",
    icon: <Search className="h-3.5 w-3.5" aria-hidden />,
    consumed: (u) => u.num_kb_searches ?? 0,
    max: (l) => l.kbSearches,
    format: fmtCount,
  },
  {
    key: "memory_adds",
    label: "Memory adds",
    icon: <PlusCircle className="h-3.5 w-3.5" aria-hidden />,
    consumed: (u) => u.num_memory_adds ?? 0,
    max: (l) => l.memoryAdds,
    format: fmtCount,
  },
];

function limitsForTier(tier: TierName): TierLimits | null {
  if (tier === "free") return FREE_TIER_LIMITS;
  if (tier === "standard") return STANDARD_TIER_LIMITS;
  return null;
}

function tierDisplayName(tier: TierName): string {
  if (tier === "free") return "Free";
  if (tier === "standard") return "Standard";
  if (tier === "pro") return "Pro";
  if (tier === "enterprise") return "Enterprise";
  return "your";
}

export function UsageTrackerSection() {
  const { data: user } = useCurrentUser();
  const tier = resolveTier(user?.user_type);
  const limits = limitsForTier(tier);
  // Only fetch usage when the quota feature is on AND the user is on a
  // gated tier — otherwise the endpoint returns zeros / the numbers
  // aren't actionable, and we'd render an empty card.
  //
  // `refetchOnMount: "always"` overrides the hook's 60s staleTime so
  // users see fresh consumption every time they open Settings, without
  // affecting the other gate-check callers (WorkbenchLM chat, KB
  // upload, Add datapack) that read from cache to stay snappy.
  const enabled = FREE_TIER_QUOTA_ENABLED && limits != null;
  const { data: usage, isLoading, error } = useAccountUsage({
    enabled,
    refetchOnMount: "always",
  });

  if (!enabled || !limits) return null;

  return (
    <section className="rounded-xl border border-border bg-card p-5">
      <header className="mb-4">
        <div className="flex items-center justify-between gap-3">
          <h2 className="text-sm font-semibold text-foreground">Usage</h2>
          {/* No tier chip on a BYOC data plane: naming a tier points at a plan
              the customer never bought. In practice this section rarely renders
              there at all -- BYOC accounts are created 'pro', for which
              limitsForTier returns null and the whole card is skipped. It still
              shows for an account that predates the flag, which is the case
              this gate is for. */}
          {isByocEnabled() ? null : (
            <span className="rounded-full border border-brand/40 bg-brand/10 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wider text-brand">
              {tierDisplayName(tier)} tier
            </span>
          )}
        </div>
        <p className="mt-0.5 text-xs text-muted-foreground">
          Live consumption against your current-tier caps.
        </p>
      </header>

      {isLoading && !usage ? (
        <p className="py-6 text-center text-sm text-muted-foreground">Loading usage…</p>
      ) : error ? (
        <p className="flex items-center gap-2 py-6 text-sm text-red-600 dark:text-red-400">
          <AlertCircle className="h-4 w-4 shrink-0" aria-hidden />
          Couldn't load usage counters.
        </p>
      ) : usage ? (
        <ul className="space-y-4">
          {ROWS.map((row) => (
            <UsageRow key={row.key} row={row} usage={usage} limits={limits} />
          ))}
        </ul>
      ) : null}
    </section>
  );
}

function UsageRow({
  row,
  usage,
  limits,
}: {
  row: UsageRowSpec;
  usage: AccountUsage;
  limits: TierLimits;
}) {
  const consumed = Math.max(0, row.consumed(usage));
  const max = Math.max(0, row.max(limits));
  // max=0 is a valid, fully-blocking cap (the env parsers on all three
  // services accept 0 and enforcement then rejects every request), so
  // render it as over-limit — not as an untouched 0%.
  const fraction = max > 0 ? consumed / max : 1;
  const barPercent = Math.min(100, Math.round(fraction * 100));
  const overLimit = fraction >= 1;
  const nearLimit = !overLimit && fraction >= NEAR_LIMIT_FRACTION;

  // Colour ladder: brand orange under 80%, amber 80–99%, red at/over
  // 100%. Text label + icon inherit the same tint on near/over so the
  // row reads as a single semantic unit rather than a bar disconnected
  // from its label.
  const tone = overLimit
    ? "text-red-600 dark:text-red-400"
    : nearLimit
      ? "text-amber-600 dark:text-amber-400"
      : "text-muted-foreground";
  const barFill = overLimit
    ? "bg-red-500"
    : nearLimit
      ? "bg-amber-500"
      : "bg-brand";

  return (
    <li>
      <div className="flex items-center justify-between gap-3 text-xs">
        <span className={cn("inline-flex items-center gap-1.5 font-medium", tone)}>
          {row.icon}
          {row.label}
        </span>
        <span className={cn("tabular-nums", tone)}>
          <span className="text-foreground">{row.format(consumed)}</span>
          <span className="text-muted-foreground"> / {row.format(max)}</span>
        </span>
      </div>
      <Progress
        value={barPercent}
        className="mt-1.5 h-1.5 bg-muted"
        indicatorClassName={barFill}
        aria-label={`${row.label} usage`}
      />
    </li>
  );
}
