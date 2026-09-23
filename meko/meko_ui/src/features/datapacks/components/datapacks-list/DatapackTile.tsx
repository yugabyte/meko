// One datapack card in the Datapacks grid: name, description, context/savings
// /quality metrics, where it sits in a collection, and the hover action strip.
// Extracted from DatapacksTilesSection, which had grown past the size a single
// file should carry.

import { ArrowRight, Database, Share2, Terminal } from "lucide-react";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import type { CurrentUser, Datapack } from "@/lib/api/mekoApi";
import { formatDatapackCreatedDateShorthand } from "@/lib/datapackDateFormat";
import { isDatapackGrantEligibleToShare } from "@/lib/datapackShareGrants";
import type { DatapackPath } from "@/lib/api/collectionsApi";
import { DatapackPathLabel } from "@/features/collections/components/DatapackPathLabel";
import {
  SHARED,
  datapackAccessFooterLabel,
  datapackDescriptionPreview,
  datapackSharedByLabel,
  datapackTileOwnerLabel,
  type TileVariant,
} from "@/features/datapacks/components/datapacks-list/datapackTileLabels";

export function DatapackTile({
  dp,
  variant,
  currentUser,
  onOpenDatapack,
  onConnectDatapack,
  onShareDatapack,
  path,
  showPath = false,
  onFileDatapack,
}: {
  dp: Datapack;
  variant: TileVariant;
  currentUser: CurrentUser | null;
  onOpenDatapack: (datapack: Datapack) => void;
  onConnectDatapack: (datapack: Datapack) => void;
  onShareDatapack: (datapack: Datapack) => void;
  /** Where this datapack sits in the caller's tree; undefined means unfiled. */
  path?: DatapackPath;
  /** Collections feature gate. Off, the tile renders exactly as it did before
   *  collections existed — no path line, no filing action. */
  showPath?: boolean;
  onFileDatapack?: (datapack: Datapack) => void;
}) {
  const { preview, title } = datapackDescriptionPreview(dp);
  const createdShorthand = formatDatapackCreatedDateShorthand(dp.created_at);
  const accessLabel = datapackAccessFooterLabel(dp);
  const ownerLabel = datapackTileOwnerLabel(dp, currentUser);
  const canShare = isDatapackGrantEligibleToShare(dp);
  const sharedByLabel = datapackSharedByLabel(dp);

  // M-01: list endpoint does not enrich knowledge_count/memory_count (MEKO-653),
  // so m01Tokens is 0 until the backend fix lands. Badge is suppressed when 0.
  const m01Tokens = ((dp.knowledge_count ?? 0) * 1000) + ((dp.memory_count ?? 0) * 200);
  const m01Label = m01Tokens >= 1_000_000
    ? `${(m01Tokens / 1_000_000).toFixed(1)}M ctx`
    : m01Tokens >= 1_000
      ? `${Math.round(m01Tokens / 1_000)}K ctx`
      : `${m01Tokens} ctx`;

  return (
    <Card
      className={cn(
        "group flex h-full min-h-0 flex-col overflow-hidden border-border bg-card p-0 shadow-sm transition-colors",
        "hover:border-brand/40 hover:bg-card/90",
        variant === SHARED && "hover:border-violet-500/35",
      )}
    >
      <div
        role="button"
        tabIndex={0}
        className={cn(
          "flex min-h-0 flex-1 cursor-pointer flex-col px-4 pb-4 pt-4 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand focus-visible:ring-inset sm:px-5 sm:pb-4 sm:pt-5",
        )}
        onClick={() => onOpenDatapack(dp)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onOpenDatapack(dp);
          }
        }}
      >
        <div className="flex flex-col gap-3">
          <div className="flex items-start gap-3">
            <div
              className={cn(
                // `border-white/10` collapsed to white-on-white in light
                // mode — using `border-foreground/10` themes the hairline
                // (subtle dark line on light card, subtle light line on
                // dark card). Same fix for the icon below: white-on-white
                // was invisible; `text-foreground/80` carries through to
                // a near-black icon in light mode and near-white in dark.
                "flex h-10 w-10 shrink-0 items-center justify-center rounded-lg border border-foreground/10 bg-card",
              )}
            >
              <Database className="h-5 w-5 text-foreground/80" />
            </div>
            <div className="min-w-0 flex-1 space-y-1">
              <h3
                className="truncate font-mono text-base font-semibold tracking-tight text-foreground"
                title={dp.datapack_name}
              >
                {dp.datapack_name}
              </h3>
              <p
                className="truncate text-xs leading-snug text-muted-foreground"
                title={title}
              >
                {preview}
              </p>
              {showPath ? (
                <DatapackPathLabel
                  path={path}
                  onFile={onFileDatapack ? () => onFileDatapack(dp) : undefined}
                />
              ) : null}
            </div>
          </div>

          {variant === SHARED ? (
            // "Subtle in light, saturated in dark" — light mode gets a
            // soft violet-tinted pill with darker text (matches the
            // soft-pill family used elsewhere); dark mode keeps the
            // original brand-art `violet-950/35` deep tint that reads
            // well against the dark card background.
            <div className="flex items-center justify-between gap-2 rounded-lg border border-violet-500/30 bg-violet-100 dark:bg-violet-950/35 px-3 py-2">
              <div className="flex min-w-0 items-center gap-2 text-xs text-violet-700 dark:text-violet-100/90">
                <Share2 className="h-3.5 w-3.5 shrink-0 text-violet-700 dark:text-violet-300/90" aria-hidden />
                <span className="min-w-0 leading-snug">
                  <span className="text-violet-700 dark:text-violet-200/80">Shared by </span>
                  <span
                    className="font-medium text-fuchsia-700 dark:text-fuchsia-300/95 [overflow-wrap:anywhere]"
                    title={sharedByLabel !== "—" ? sharedByLabel : undefined}
                  >
                    {sharedByLabel}
                  </span>
                </span>
              </div>
            </div>
          ) : null}

          {m01Tokens > 0 && (
            <>
              <div className="border-t border-foreground/20" />
              <div className="flex flex-wrap gap-1.5">
                <span className="rounded-md bg-muted px-2 py-0.5 text-[10px] font-medium tabular-nums text-muted-foreground">
                  {m01Label}
                </span>
              </div>
            </>
          )}

          <div className="flex items-start justify-between gap-3 text-xs text-muted-foreground">
            <div className="min-w-0 space-y-1">
              <div className="min-w-0">
                <span className="font-medium text-foreground/70">Access </span>
                <span className="text-muted-foreground/95">{accessLabel}</span>
              </div>
              <div className="min-w-0 leading-snug">
                <span className="font-medium text-foreground/70">Owner </span>
                <span
                  className="break-words text-muted-foreground/95"
                  title={ownerLabel !== "—" ? ownerLabel : undefined}
                >
                  {ownerLabel}
                </span>
              </div>
            </div>
            <div className="shrink-0 text-right">
              <span className="font-medium text-foreground/70">Created </span>
              <span className="tabular-nums text-muted-foreground/95">{createdShorthand}</span>
            </div>
          </div>
        </div>
      </div>

      {/* Fixed-height strip; shrink-0 + flex-1 main above keeps row-equal-height grids from crowding metadata.
          Every `bg-white/X` / `text-white/X` / `border-white/X` swapped to `foreground/X` so the strip auto-themes — in dark mode the foreground IS near-white so it reads identical to the original; in light mode the foreground is near-black, giving a legible "darker bar on hover" without the white-on-white invisibility. */}
      <div
        className={cn(
          "relative z-10 flex h-11 w-full shrink-0 border-t border-transparent bg-card",
          "transition-[border-color,background-color] duration-150",
          "group-hover:border-foreground/10 group-hover:bg-input/98",
        )}
      >
        <div
          className={cn(
            "flex h-full w-full items-stretch justify-center",
            "opacity-0 transition-opacity duration-150 group-hover:opacity-100",
            "pointer-events-none group-hover:pointer-events-auto",
          )}
        >
          <button
            type="button"
            className="flex flex-1 items-center justify-center gap-1.5 px-2 text-sm font-semibold text-foreground transition-colors hover:bg-foreground/5 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-brand"
            onClick={() => onOpenDatapack(dp)}
          >
            Open
            <ArrowRight className="h-4 w-4 shrink-0 opacity-90" aria-hidden />
          </button>
          <div className="w-px shrink-0 self-stretch bg-foreground/10" aria-hidden />
          <button
            type="button"
            className="flex flex-1 items-center justify-center gap-1.5 px-2 text-sm font-medium text-foreground/95 transition-colors hover:bg-foreground/5 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-brand"
            onClick={() => onConnectDatapack(dp)}
          >
            <Terminal className="h-4 w-4 shrink-0 opacity-90" aria-hidden />
            Connect
          </button>
          <div className="w-px shrink-0 self-stretch bg-foreground/10" aria-hidden />
          <button
            type="button"
            disabled={!canShare}
            title={canShare ? "Share this datapack" : "Owner or Maintainer access required to share"}
            className={cn(
              "flex flex-1 items-center justify-center gap-1.5 px-2 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-brand",
              canShare
                ? "text-foreground/95 hover:bg-foreground/5"
                : "cursor-not-allowed text-foreground/35 hover:bg-transparent",
            )}
            onClick={() => {
              if (!canShare) return;
              onShareDatapack(dp);
            }}
          >
            <Share2 className="h-4 w-4 shrink-0 opacity-90" aria-hidden />
            Share
          </button>
        </div>
      </div>
    </Card>
  );
}
