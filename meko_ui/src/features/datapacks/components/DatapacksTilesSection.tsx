import { useMemo, useState } from "react";
import { ArrowLeftRight, BookOpen, BrainCircuit, Database } from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import type { CurrentUser, Datapack } from "@/lib/mekoApi";
import { formatDatapackCreatedAtUtc } from "@/lib/datapackDateFormat";

function formatDatapackOwnerLabel(dp: Datapack, currentUser: CurrentUser | null): string {
  if (dp.agent_id != null && String(dp.agent_id).trim() !== "") {
    return "Agent";
  }
  if (
    currentUser &&
    dp.account_id != null &&
    dp.account_id !== "" &&
    dp.account_id === currentUser.account_id
  ) {
    const fullName = `${currentUser.first_name ?? ""} ${currentUser.last_name ?? ""}`.trim();
    if (fullName) return fullName;
    if (currentUser.email) return currentUser.email;
    return "You";
  }
  return "—";
}

type Props = {
  datapacks: Datapack[];
  isLoading: boolean;
  currentUser: CurrentUser | null;
  /** `listKnowledgeBases(name).length` per datapack name (filled by parent). */
  knowledgeBaseCountByName: Record<string, number>;
  knowledgeBaseCountsLoading?: boolean;
  onOpenDatapack: (datapack: Datapack) => void;
};

/** Placeholders until per-datapack metrics are wired. */
const MEMORY_PLACEHOLDER_COUNT = 0;
const LEARNINGS_PLACEHOLDER_COUNT = 0;

export default function DatapacksTilesSection({
  datapacks,
  isLoading,
  currentUser,
  knowledgeBaseCountByName,
  knowledgeBaseCountsLoading = false,
  onOpenDatapack,
}: Props) {
  const [searchQuery, setSearchQuery] = useState("");

  const filteredDatapacks = useMemo(() => {
    const q = searchQuery.trim().toLowerCase();
    if (!q) return datapacks;
    return datapacks.filter((dp) => dp.name.toLowerCase().includes(q));
  }, [datapacks, searchQuery]);

  return (
    <div className="space-y-4">
      <div className="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3">
        <h2 className="text-lg font-semibold text-foreground">Your datapacks</h2>
        <Input
          type="search"
          placeholder="Search by name…"
          value={searchQuery}
          onChange={(e) => setSearchQuery(e.target.value)}
          className="max-w-md bg-[#0E1525] border-border text-foreground placeholder:text-muted-foreground"
          aria-label="Search datapacks by name"
        />
      </div>

      {isLoading ? (
        <p className="text-sm text-muted-foreground">Loading datapacks…</p>
      ) : filteredDatapacks.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          {datapacks.length === 0
            ? "No datapacks yet. Add a datapack to get started."
            : "No datapacks match your search."}
        </p>
      ) : (
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
          {filteredDatapacks.map((dp) => (
            <Card
              key={dp.id}
              role="button"
              tabIndex={0}
              className={cn(
                "border-border bg-[#1C2333] shadow-sm cursor-pointer transition-colors",
                "hover:border-blue-500/40 hover:bg-[#1C2333]/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500",
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
                  <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-blue-500/15 border border-blue-500/25">
                    <Database className="h-5 w-5 text-blue-400" />
                  </div>
                  <div className="min-w-0 flex-1">
                    <h3 className="font-semibold text-white truncate" title={dp.name}>
                      {dp.name}
                    </h3>
                    <p className="text-xs text-muted-foreground mt-1 leading-snug">
                      Created:{" "}
                      <span className="text-muted-foreground/90">
                        {formatDatapackCreatedAtUtc(dp.created_at)}
                      </span>
                    </p>
                    <p className="text-xs text-muted-foreground mt-1 leading-snug">
                      Owner:&nbsp;&nbsp;&nbsp;
                      <span className="text-muted-foreground/90 break-words">
                        {formatDatapackOwnerLabel(dp, currentUser)}
                      </span>
                    </p>
                  </div>
                </div>

                <div className="border-t border-white/10 pt-3">
                  <div className="grid grid-cols-3 gap-1.5 min-w-0 sm:gap-2">
                    <div className="min-w-0 rounded-md bg-[#0E1525]/80 border border-border/60 px-1.5 py-2 sm:px-2 sm:py-2.5">
                      <div className="flex flex-nowrap items-center gap-1 text-[10px] font-medium leading-tight text-muted-foreground whitespace-nowrap sm:text-[11px]">
                        <BrainCircuit className="h-3 w-3 shrink-0 text-violet-400" aria-hidden />
                        <span title="Memory (ME)">ME</span>
                      </div>
                      <p className="mt-1 text-lg font-semibold tabular-nums tracking-tight text-white sm:text-xl">
                        {MEMORY_PLACEHOLDER_COUNT}
                      </p>
                    </div>
                    <div className="min-w-0 rounded-md bg-[#0E1525]/80 border border-border/60 px-1.5 py-2 sm:px-2 sm:py-2.5">
                      <div className="flex flex-nowrap items-center gap-1 text-[10px] font-medium leading-tight text-muted-foreground whitespace-nowrap sm:text-[11px]">
                        <BookOpen className="h-3 w-3 shrink-0 text-emerald-400" aria-hidden />
                        <span title="Knowledge (KO)">KO</span>
                      </div>
                      <p className="mt-1 min-h-[1.75rem] flex items-center text-lg font-semibold tabular-nums tracking-tight text-white sm:min-h-[2rem] sm:text-xl">
                        {knowledgeBaseCountsLoading ? (
                          <span className="text-base text-muted-foreground tabular-nums animate-pulse sm:text-lg">
                            …
                          </span>
                        ) : (
                          knowledgeBaseCountByName[dp.name] ?? 0
                        )}
                      </p>
                    </div>
                    <div className="min-w-0 rounded-md bg-[#0E1525]/80 border border-border/60 px-1.5 py-2 sm:px-2 sm:py-2.5">
                      <div className="flex flex-nowrap items-center gap-1 text-[10px] font-medium leading-tight text-muted-foreground whitespace-nowrap sm:text-[11px]">
                        <ArrowLeftRight className="h-3 w-3 shrink-0 text-amber-400" aria-hidden />
                        <span title="Learnings (ME → KO)" className="truncate">
                          ME → KO
                        </span>
                      </div>
                      <p className="mt-1 text-lg font-semibold tabular-nums tracking-tight text-white sm:text-xl">
                        {LEARNINGS_PLACEHOLDER_COUNT}
                      </p>
                    </div>

                  </div>
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      )}
    </div>
  );
}
