import { useMemo, useState } from "react";
import { ArrowRight, Database, Plus, Search, Share2, Terminal } from "lucide-react";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/utils";
import type { CurrentUser, Datapack } from "@/lib/mekoApi";
import {
  formatDatapackCreatedDateShorthand,
  sortDatapacksByCreatedAtDesc,
} from "@/lib/datapackDateFormat";
import {
  formatDatapackShareGrantCamelCase,
  isDatapackGrantEligibleToShare,
  isDatapackListGrantOwner,
  isDatapackShareGrant,
} from "@/lib/datapackShareGrants";

function filterByName(datapacks: Datapack[], q: string): Datapack[] {
  const trimmed = q.trim().toLowerCase();
  if (!trimmed) return datapacks;
  return datapacks.filter((dp) => dp.datapack_name.toLowerCase().includes(trimmed));
}

/** Short subtitle under the datapack name (ellipsis when longer). */
const DESCRIPTION_PREVIEW_LEN = 72;

function datapackDescriptionPreview(dp: Datapack): { preview: string; title?: string } {
  const raw = (dp.description ?? "").trim();
  if (!raw) {
    return { preview: "No description" };
  }
  if (raw.length <= DESCRIPTION_PREVIEW_LEN) {
    return { preview: raw };
  }
  return {
    preview: `${raw.slice(0, DESCRIPTION_PREVIEW_LEN)}...`,
    title: raw,
  };
}

type Props = {
  datapacks: Datapack[];
  isLoading: boolean;
  currentUser: CurrentUser | null;
  onOpenDatapack: (datapack: Datapack) => void;
  onConnectDatapack: (datapack: Datapack) => void;
  onShareDatapack: (datapack: Datapack) => void;
  onNewDatapack: () => void;
};

/** Placeholders until per-datapack metrics are wired. */
const SHARED = "shared";
const OWNED = "owned";

type TileVariant = "owned" | "shared";

type ScopeTab = "all" | "owned" | "shared";

/** Overrides default TabsTrigger active ring/shadow so the pill sits evenly inside the list padding. */
const scopeTabsTriggerClassName = cn(
  "inline-flex h-7 shrink-0 items-center justify-center rounded-md px-3 text-sm font-medium transition-colors",
  "text-muted-foreground hover:text-foreground",
  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500 focus-visible:ring-offset-2",
  "data-[state=active]:bg-white/10 data-[state=active]:text-foreground",
  "data-[state=active]:shadow-none data-[state=active]:ring-0 data-[state=active]:ring-offset-0",
);

function emptyCopy(scope: ScopeTab, searchActive: boolean): string {
  if (searchActive) {
    if (scope === "owned") return "No owned datapacks match your search.";
    if (scope === "shared") return "No shared datapacks match your search.";
    return "No datapacks match your search.";
  }
  if (scope === "owned") return "You don't have any datapacks you own yet.";
  if (scope === "shared") return "You don't have any shared datapacks yet.";
  return "No datapacks to show.";
}

function datapackAccessFooterLabel(dp: Datapack): string {
  const g = dp.grant;
  if (g != null && isDatapackShareGrant(g)) return formatDatapackShareGrantCamelCase(g);
  return "—";
}

/** Owner line for tiles: "me" when the signed-in account owns the row; else API owner name. */
function datapackSharedByLabel(dp: Datapack): string {
  const email = (dp.datapack_owner_email_id ?? "").trim();
  if (email) return email;
  const name = (dp.datapack_owner_name ?? "").trim();
  if (name) return name;
  return "—";
}

function datapackTileOwnerLabel(dp: Datapack, currentUser: CurrentUser | null): string {
  if (
    currentUser &&
    dp.user_id != null &&
    dp.user_id !== "" &&
    dp.user_id === currentUser.account_id
  ) {
    return "Me";
  }
  const name = (dp.datapack_owner_name ?? "").trim();
  if (name) return name;
  return "—";
}

function DatapackTile({
  dp,
  variant,
  currentUser,
  onOpenDatapack,
  onConnectDatapack,
  onShareDatapack,
}: {
  dp: Datapack;
  variant: TileVariant;
  currentUser: CurrentUser | null;
  onOpenDatapack: (datapack: Datapack) => void;
  onConnectDatapack: (datapack: Datapack) => void;
  onShareDatapack: (datapack: Datapack) => void;
}) {
  const { preview, title } = datapackDescriptionPreview(dp);
  const createdShorthand = formatDatapackCreatedDateShorthand(dp.created_at);
  const accessLabel = datapackAccessFooterLabel(dp);
  const ownerLabel = datapackTileOwnerLabel(dp, currentUser);
  const canShare = isDatapackGrantEligibleToShare(dp);
  const sharedByLabel = datapackSharedByLabel(dp);

  return (
    <Card
      className={cn(
        "group flex h-full min-h-0 flex-col overflow-hidden border-border bg-card p-0 shadow-sm transition-colors",
        "hover:border-blue-500/40 hover:bg-card/90",
        variant === SHARED && "hover:border-violet-500/35",
      )}
    >
      <div
        role="button"
        tabIndex={0}
        className={cn(
          "flex min-h-0 flex-1 cursor-pointer flex-col px-4 pb-4 pt-4 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500 focus-visible:ring-inset sm:px-5 sm:pb-4 sm:pt-5",
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

          <div className="border-t border-foreground/20" />

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
            className="flex flex-1 items-center justify-center gap-1.5 px-2 text-sm font-semibold text-foreground transition-colors hover:bg-foreground/5 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-blue-500"
            onClick={() => onOpenDatapack(dp)}
          >
            Open
            <ArrowRight className="h-4 w-4 shrink-0 opacity-90" aria-hidden />
          </button>
          <div className="w-px shrink-0 self-stretch bg-foreground/10" aria-hidden />
          <button
            type="button"
            className="flex flex-1 items-center justify-center gap-1.5 px-2 text-sm font-medium text-foreground/95 transition-colors hover:bg-foreground/5 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-blue-500"
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
              "flex flex-1 items-center justify-center gap-1.5 px-2 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-blue-500",
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

function NewDatapackPlaceholderCard({ onClick }: { onClick: () => void }) {
  return (
    <Card
      role="button"
      tabIndex={0}
      className={cn(
        "flex min-h-[12rem] flex-col items-center justify-center gap-3 border border-dashed border-border/70 bg-transparent p-6 shadow-none transition-colors",
        "cursor-pointer hover:border-muted-foreground/45 hover:bg-white/[0.03]",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500 focus-visible:ring-offset-2 focus-visible:ring-offset-card",
      )}
      onClick={onClick}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onClick();
        }
      }}
    >
      <div
        className="flex h-10 w-10 items-center justify-center rounded-lg border border-muted-foreground/45"
        aria-hidden
      >
        <Plus className="h-5 w-5 text-muted-foreground" />
      </div>
      <div className="space-y-1 text-center">
        <p className="text-base font-semibold text-foreground">New datapack</p>
        <p className="max-w-[14rem] text-xs leading-relaxed text-muted-foreground">
          Connect an existing project or start from scratch
        </p>
      </div>
    </Card>
  );
}

export default function DatapacksTilesSection({
  datapacks,
  isLoading,
  currentUser,
  onOpenDatapack,
  onConnectDatapack,
  onShareDatapack,
  onNewDatapack,
}: Props) {
  const [searchQuery, setSearchQuery] = useState("");
  const [scopeTab, setScopeTab] = useState<ScopeTab>("all");

  const { ownedDatapacks, sharedDatapacks } = useMemo(() => {
    const owned: Datapack[] = [];
    const shared: Datapack[] = [];
    for (const dp of datapacks) {
      if (isDatapackListGrantOwner(dp)) owned.push(dp);
      else shared.push(dp);
    }
    return { ownedDatapacks: owned, sharedDatapacks: shared };
  }, [datapacks]);

  const filteredOwned = useMemo(() => filterByName(ownedDatapacks, searchQuery), [ownedDatapacks, searchQuery]);
  const filteredShared = useMemo(() => filterByName(sharedDatapacks, searchQuery), [sharedDatapacks, searchQuery]);
  const filteredAll = useMemo(() => {
    const filtered = filterByName(datapacks, searchQuery);
    return sortDatapacksByCreatedAtDesc(filtered);
  }, [datapacks, searchQuery]);

  const visibleDatapacks = useMemo(() => {
    if (scopeTab === "owned") return filteredOwned;
    if (scopeTab === "shared") return filteredShared;
    return filteredAll;
  }, [scopeTab, filteredOwned, filteredShared, filteredAll]);

  const searchActive = searchQuery.trim() !== "";
  const listEmpty = !isLoading && datapacks.length > 0 && visibleDatapacks.length === 0;

  const headerCount = visibleDatapacks.length;

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
        <div className="flex min-w-0 flex-wrap items-baseline gap-2">
          <h2 className="text-lg font-semibold text-foreground">Your datapacks</h2>
          <span
            className="text-sm tabular-nums text-muted-foreground"
            aria-live="polite"
            aria-label={`${headerCount} datapacks in current view`}
          >
            {headerCount}
          </span>
        </div>

        <div className="flex flex-col gap-3 sm:flex-row sm:flex-wrap sm:items-center sm:justify-end lg:shrink-0">
          <Tabs value={scopeTab} onValueChange={(v) => setScopeTab(v as ScopeTab)} className="w-full sm:w-auto">
            <TabsList className="flex min-h-9 w-full flex-wrap items-center justify-start gap-1 rounded-lg border border-border bg-input/90 p-1 sm:inline-flex sm:w-auto sm:flex-nowrap">
              <TabsTrigger value="all" className={scopeTabsTriggerClassName}>
                All
              </TabsTrigger>
              <TabsTrigger value="owned" className={scopeTabsTriggerClassName}>
                Mine
              </TabsTrigger>
              <TabsTrigger value="shared" className={scopeTabsTriggerClassName}>
                Shared with me
              </TabsTrigger>
            </TabsList>
          </Tabs>
          <div className="relative w-full sm:w-auto sm:min-w-[200px] sm:max-w-[240px]">
            <Search
              className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
              aria-hidden
            />
            <Input
              type="search"
              placeholder="Search by name..."
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              className="h-9 w-full border-border bg-input pl-9 text-foreground placeholder:text-muted-foreground"
              aria-label="Search datapacks by name"
            />
          </div>
        </div>
      </div>

      {isLoading ? (
        <p className="text-sm text-muted-foreground">Loading datapacks…</p>
      ) : datapacks.length === 0 ? (
        <p className="text-sm text-muted-foreground">No datapacks yet. Add a datapack to get started.</p>
      ) : listEmpty ? (
        <p className="text-sm text-muted-foreground">{emptyCopy(scopeTab, searchActive)}</p>
      ) : (
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 [&>*]:min-h-0">
          {visibleDatapacks.map((dp) => (
            <DatapackTile
              key={dp.datapack_id}
              dp={dp}
              variant={isDatapackListGrantOwner(dp) ? OWNED : SHARED}
              currentUser={currentUser}
              onOpenDatapack={onOpenDatapack}
              onConnectDatapack={onConnectDatapack}
              onShareDatapack={onShareDatapack}
            />
          ))}
          <NewDatapackPlaceholderCard onClick={onNewDatapack} />
        </div>
      )}
    </div>
  );
}
