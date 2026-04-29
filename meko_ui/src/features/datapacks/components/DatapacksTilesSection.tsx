import { useMemo, useState } from "react";
import { Database } from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/utils";
import type { CurrentUser, Datapack } from "@/lib/mekoApi";
import { formatDatapackCreatedAtUtc, sortDatapacksByCreatedAtDesc } from "@/lib/datapackDateFormat";
import { shareGrantLabel } from "@/features/datapacks/components/datapack-share/shareInviteRoles";
import { DATAPACK_SHARE_GRANT_OWNER } from "@/lib/datapackShareGrants";

function formatDatapackOwnerLabel(dp: Datapack, currentUser: CurrentUser | null): string {
  if (
    currentUser &&
    dp.user_id != null &&
    dp.user_id !== "" &&
    dp.user_id === currentUser.account_id
  ) {
    const fullName = `${currentUser.first_name ?? ""} ${currentUser.last_name ?? ""}`.trim();
    if (fullName) return fullName;
    if (currentUser.email) return currentUser.email;
    return "You";
  }
  return "—";
}

/** Owned datapacks: no share grant on the list row (`grant` absent or null). */
function datapackIsOwned(dp: Datapack): boolean {
  return dp.grant === DATAPACK_SHARE_GRANT_OWNER;
}

function filterByName(datapacks: Datapack[], q: string): Datapack[] {
  const trimmed = q.trim().toLowerCase();
  if (!trimmed) return datapacks;
  return datapacks.filter((dp) => dp.datapack_name.toLowerCase().includes(trimmed));
}

const DESCRIPTION_PREVIEW_LEN = 120;

function datapackDescriptionPreview(dp: Datapack): { preview: string; title?: string } {
  const raw = (dp.description ?? "").trim();
  if (!raw) {
    return { preview: "No description yet." };
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
};

/** Placeholders until per-datapack metrics are wired. */
const SHARED = "shared";
const OWNED = "owned";

type TileVariant = "owned" | "shared";

type ScopeTab = "all" | "owned" | "shared";

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

function DatapackTile({
  dp,
  variant,
  currentUser,
  onOpenDatapack,
}: {
  dp: Datapack;
  variant: TileVariant;
  currentUser: CurrentUser | null;
  onOpenDatapack: (datapack: Datapack) => void;
}) {
  const grantLabel = variant === SHARED && dp.grant ? shareGrantLabel(dp.grant) : null;
  const { preview, title } = datapackDescriptionPreview(dp);

  return (
    <Card
      role="button"
      tabIndex={0}
      className={cn(
        "border-border bg-[#1C2333] shadow-sm cursor-pointer transition-colors",
        "hover:border-blue-500/40 hover:bg-[#1C2333]/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500",
        variant === SHARED && "hover:border-violet-500/35",
      )}
      onClick={() => onOpenDatapack(dp)}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpenDatapack(dp);
        }
      }}
    >
      <CardContent className="flex flex-col gap-3 p-4 sm:p-5">
        <div className="flex items-start gap-3 shrink-0">
          <div
            className={cn(
              "flex h-10 w-10 shrink-0 items-center justify-center rounded-lg border",
              variant === OWNED
                ? "bg-blue-500/15 border-blue-500/25"
                : "bg-violet-500/15 border-violet-500/25",
            )}
          >
            <Database className={cn("h-5 w-5", variant === OWNED ? "text-blue-400" : "text-violet-300")} />
          </div>
          <div className="min-w-0 flex-1">
            <h3 className="font-semibold text-white truncate" title={dp.datapack_name}>
              {dp.datapack_name}
            </h3>
            {grantLabel ? (
              <p className="text-xs text-violet-200/90 mt-1 leading-snug">Your access: {grantLabel}</p>
            ) : null}
            <p className="text-xs text-muted-foreground mt-1 leading-snug">
              Created:{" "}
              <span className="text-muted-foreground/90">{formatDatapackCreatedAtUtc(dp.created_at)}</span>
            </p>
            <p className="text-xs text-muted-foreground mt-1 leading-snug">
              Owner:&nbsp;&nbsp;&nbsp;
              <span className="text-muted-foreground/90 break-words">{dp.datapack_owner_name ?? formatDatapackOwnerLabel(dp, currentUser)}</span>
            </p>
          </div>
        </div>

        <div className="border-t border-white/10 pt-3">
          <p
            className="text-sm leading-relaxed text-muted-foreground"
            title={title}
          >
            {`Description: ${preview}`}
          </p>
        </div>
      </CardContent>
    </Card>
  );
}

export default function DatapacksTilesSection({
  datapacks,
  isLoading,
  currentUser,
  onOpenDatapack,
}: Props) {
  const [searchQuery, setSearchQuery] = useState("");
  const [scopeTab, setScopeTab] = useState<ScopeTab>("all");

  const { ownedDatapacks, sharedDatapacks } = useMemo(() => {
    const owned: Datapack[] = [];
    const shared: Datapack[] = [];
    for (const dp of datapacks) {
      if (datapackIsOwned(dp)) owned.push(dp);
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

  return (
    <div className="space-y-6">
      <h2 className="text-lg font-semibold text-foreground">Your datapacks</h2>

      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <Tabs value={scopeTab} onValueChange={(v) => setScopeTab(v as ScopeTab)} className="w-full sm:w-auto">
          <TabsList className="flex h-auto w-full flex-wrap justify-start gap-1 rounded-lg bg-muted p-1 sm:inline-flex sm:w-auto">
            <TabsTrigger value="all" className="px-3 py-1.5 text-sm">
              All
            </TabsTrigger>
            <TabsTrigger value="owned" className="px-3 py-1.5 text-sm">
              Owned
            </TabsTrigger>
            <TabsTrigger value="shared" className="px-3 py-1.5 text-sm">
              Shared
            </TabsTrigger>
          </TabsList>
        </Tabs>
        <Input
          type="search"
          placeholder="Search by name…"
          value={searchQuery}
          onChange={(e) => setSearchQuery(e.target.value)}
          className="h-9 w-full max-w-[200px] shrink-0 bg-[#0E1525] border-border text-foreground placeholder:text-muted-foreground sm:max-w-[220px]"
          aria-label="Search datapacks by name"
        />
      </div>

      {isLoading ? (
        <p className="text-sm text-muted-foreground">Loading datapacks…</p>
      ) : datapacks.length === 0 ? (
        <p className="text-sm text-muted-foreground">No datapacks yet. Add a datapack to get started.</p>
      ) : listEmpty ? (
        <p className="text-sm text-muted-foreground">{emptyCopy(scopeTab, searchActive)}</p>
      ) : (
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
          {visibleDatapacks.map((dp) => (
            <DatapackTile
              key={dp.datapack_id}
              dp={dp}
              variant={datapackIsOwned(dp) ? OWNED : SHARED}
              currentUser={currentUser}
              onOpenDatapack={onOpenDatapack}
            />
          ))}
        </div>
      )}
    </div>
  );
}
