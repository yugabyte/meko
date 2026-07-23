// TokenBurnTile — the TOKEN BURN tile of the Meko efficiency dashboard.
// Headline = model-agnostic context-reduction % (tokenBurnPercent); bars = $
// saved by running GPT-OSS-20B with reduced context vs each premier model.
// One presentational component drives both the account-wide (Datapacks
// Overview) and per-datapack (datapack detail) placements — only the data
// source differs, so scope is passed in.

import { Zap } from "lucide-react";
import {
  EfficiencyTile,
  type EfficiencyTileBar,
  type EfficiencyTileBreakdown,
} from "@/components/efficiency-tile";
import { getApiErrorMessage, type TokenSavingsSummary } from "@/lib/api/mekoApi";
import {
  dollarsSavedByModel,
  formatTokens,
  OUR_INFERENCE_MODEL,
  sourceBreakdown,
  tokenBurnPercent,
} from "@/lib/tokenSavings";
import { useAccountTokenSavings, useDatapackTokenSavings } from "@/features/datapacks/hooks/useTokenSavings";

const usdFormatter = new Intl.NumberFormat(undefined, {
  style: "currency",
  currency: "USD",
  maximumFractionDigits: 2,
});

// Bars are sized relative to the largest saving so the most expensive premier
// model (Opus) fills the track and the cheaper ones scale down from it.
function toBars(summary: TokenSavingsSummary): EfficiencyTileBar[] {
  const saved = dollarsSavedByModel(summary);
  const max = saved.reduce((m, s) => Math.max(m, s.dollars), 0);
  return saved.map((s) => ({
    label: `vs ${s.label}`,
    value: max > 0 ? (s.dollars / max) * 100 : 0,
    valueLabel: usdFormatter.format(s.dollars),
  }));
}

// Turns `tokens_saved_by_source` into a compact "How we saved" list so the
// tile answers "why is the headline what it is?" — the API already carries
// the split (conversation / memory / knowledge_base+memory), we just need
// to render it. Returns undefined when the API had no source data so
// `EfficiencyTile` skips the section entirely instead of drawing an empty
// header.
function toBreakdown(summary: TokenSavingsSummary): EfficiencyTileBreakdown | undefined {
  const rows = sourceBreakdown(summary);
  if (rows.length === 0) return undefined;
  return {
    title: "How we saved",
    entries: rows.map((r) => ({
      key: r.key,
      label: r.label,
      primary: `${formatTokens(r.tokens)} tokens`,
      secondary: `${Math.round(r.fraction * 100)}%`,
    })),
  };
}

function TokenBurnTileView({
  summary,
  loading,
  error,
}: {
  summary: TokenSavingsSummary | undefined;
  loading: boolean;
  error: string | null;
}) {
  const hasData = Boolean(summary && summary.total_requests > 0);
  return (
    <EfficiencyTile
      icon={<Zap className="h-4 w-4" />}
      title="Token Burn"
      badge="live"
      accent="amber"
      loading={loading}
      error={error}
      isEmpty={!loading && !error && !hasData}
      emptyLabel="No contextual searches recorded yet."
      headline={summary ? `${tokenBurnPercent(summary)}% saved` : "—"}
      subtitle="vs all-frontier baseline"
      bars={summary ? toBars(summary) : []}
      breakdown={summary ? toBreakdown(summary) : undefined}
      footer={`${OUR_INFERENCE_MODEL.label} vs premier models · illustrative rates`}
    />
  );
}

// Account-wide variant (GET /me/token-savings) — Datapacks Overview.
export function AccountTokenBurnTile() {
  const { data, isLoading, error } = useAccountTokenSavings();
  return (
    <TokenBurnTileView
      summary={data}
      loading={isLoading}
      error={error ? getApiErrorMessage(error, "Couldn't load token savings.") : null}
    />
  );
}

// Per-datapack variant (GET /datapacks/:id/token-savings) — datapack detail.
export function DatapackTokenBurnTile({ datapackId }: { datapackId: string | null | undefined }) {
  const { data, isLoading, error } = useDatapackTokenSavings(datapackId);
  return (
    <TokenBurnTileView
      summary={data}
      // Treat "no id yet" (parent still loading) as loading so we show the
      // skeleton, not a flash of the empty state, before the query enables.
      loading={isLoading || !datapackId}
      error={error ? getApiErrorMessage(error, "Couldn't load token savings.") : null}
    />
  );
}
