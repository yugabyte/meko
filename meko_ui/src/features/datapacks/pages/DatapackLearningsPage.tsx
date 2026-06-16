// Full-screen route for reviewing all agent memories and promoting them to the knowledge base.
// Resolves the datapack id then renders MemoryPromotionPanel in `full` (paginated) mode.

import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { BookMarked } from "lucide-react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import MemoryPromotionPanel from "@/features/datapacks/components/memory-promotion/MemoryPromotionPanel";
import { WarningBanner } from "@/components/ui/warning-banner";
import {
  LEARNINGS_SOURCE,
  LEARNINGS_SOURCE_QUERY_PARAM,
  ROUTES,
  datapackDetailPath,
  datapackKnowledgePath,
} from "@/app/routes/paths";
import { getApiErrorMessage, getDatapack, type Datapack } from "@/lib/api/mekoApi";
import { isDatapackGrantEligibleToPromoteMemoryToKnowledge } from "@/lib/datapackShareGrants";

type Props = {
  datapackName: string;
  /** Optional URL-pinned id from `?datapack_id=`. Disambiguates between
   *  same-named datapacks across share boundaries; falls back to
   *  name-only resolution when omitted. */
  datapackId?: string;
};

export default function DatapackLearningsPage({ datapackName, datapackId }: Props) {
  const [datapack, setDatapack] = useState<Datapack | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [statusError, setStatusError] = useState<string | null>(null);
  const [isPromotionAllowed, setIsPromotionAllowed] = useState(false);

  // `?from=...` carries which page the user came from. When set to
  // `shared-knowledge` we grow the breadcrumb trail by one hop so
  // the user can navigate back to the SharedKnowledge view they
  // launched from (e.g. via the "Review N pending" CTA on the
  // Promoted memories tab). Same pattern as the conversation viewer's
  // CONVERSATION_SOURCE query param.
  const [searchParams] = useSearchParams();
  const cameFrom = searchParams.get(LEARNINGS_SOURCE_QUERY_PARAM);
  const breadcrumbItems = useMemo(() => {
    const items: { label: string; href?: string }[] = [
      { label: "Datapacks", href: ROUTES.datapacks },
      { label: datapackName, href: datapackDetailPath(datapackName) },
    ];
    if (cameFrom === LEARNINGS_SOURCE.sharedKnowledge) {
      items.push({
        label: "Shared Knowledge",
        href: datapackKnowledgePath(datapackName),
      });
    }
    items.push({ label: "Learnings Summary" });
    return items;
  }, [datapackName, cameFrom]);

  useEffect(() => {
    let cancelled = false;
    const run = async () => {
      setStatusLoading(true);
      setStatusError(null);
      try {
        const d = await getDatapack(datapackName, { datapackId });
        if (!cancelled) {
          setDatapack(d);
          setIsPromotionAllowed(isDatapackGrantEligibleToPromoteMemoryToKnowledge({ grant: d.grant }));
        }
      } catch (e) {
        if (!cancelled) {
          setDatapack(null);
          setStatusError(getApiErrorMessage(e, "Failed to load datapack."));
        }
      } finally {
        if (!cancelled) setStatusLoading(false);
      }
    };
    void run();
    return () => {
      cancelled = true;
    };
  }, [datapackName, datapackId]);

  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader />

      <main className="flex-1 pr-1 pt-6 pl-1 pb-3 max-w-7xl mx-auto w-full space-y-8">
        <DatapackBreadcrumbs items={breadcrumbItems} />

        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-3">
            <BookMarked className="h-8 w-8 shrink-0 text-purple-400" aria-hidden />
            <h1 className="text-2xl sm:text-3xl font-semibold tracking-tight text-foreground truncate">
              Learnings Summary
            </h1>
          </div>
          <p className="text-sm text-muted-foreground max-w-2xl leading-relaxed">
            Review agent memories for <span className="text-foreground/90">{datapackName}</span> and promote them
            into the knowledge base.
          </p>
        </div>

        {statusError ? <WarningBanner>{statusError}</WarningBanner> : null}

        {datapack?.datapack_id ? (
          <MemoryPromotionPanel datapackName={datapackName} datapackId={datapack.datapack_id} isPromotionAllowed={isPromotionAllowed} />
        ) : (
          <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-16 text-center text-sm text-muted-foreground">
            {statusLoading ? "Loading datapack…" : "Datapack not available."}
          </div>
        )}
      </main>
    </div>
  );
}
