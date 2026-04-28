/**
 * “Collective learning · live” ribbon: horizontal Conversations → Memory → Learnings → Knowledge
 * (placeholders until APIs back each stage). Shown above the datapack detail tabs.
 */
import { useEffect, useState } from "react";
import type { KeyboardEvent, ReactNode } from "react";
import { ArrowLeftRight, BookOpen, ChevronRight, Loader2, MessageSquare, Sparkles } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import {
  CurrentUser,
  getCurrentUser,
  listMemories,
  MEMORY_LIST_DEFAULT_USER_TYPE,
} from "@/lib/mekoApi";
import { DATAPACK_DETAIL_TAB, type DatapackDetailTab } from "@/features/datapacks/components/datapack-detail/types";

const toneStyles: Record<DatapackDetailTab, string> = {
  conversations:
    "border-slate-500/35 bg-gradient-to-br from-slate-900/80 via-[#121a28] to-[#0b1220]",
  memory: "border-blue-500/40 bg-gradient-to-br from-blue-950/70 via-[#0f1729] to-[#0b1220]",
  learnings: "border-purple-500/40 bg-gradient-to-br from-purple-950/65 via-[#141228] to-[#0b1220]",
  knowledge: "border-emerald-900/50 bg-gradient-to-br from-emerald-950/55 via-[#0c1814] to-[#0b1220]",
};

const titleTone: Record<DatapackDetailTab, string> = {
  conversations: "text-slate-200/95",
  memory: "text-blue-200/95",
  learnings: "text-purple-200/95",
  knowledge: "text-emerald-200/95",
};

type StageShellProps = {
  tone: DatapackDetailTab;
  eyebrow: string;
  title: string;
  value: number;
  delta?: string;
  subtitle: string;
  cornerBadge?: string;
  /** Lucide icon aligned with datapack tiles (Memory / Learnings / Knowledge). */
  titleIcon?: ReactNode;
  /** Optional title color (default white). */
  titleClassName?: string;
  /** When true, the value/subtitle/children area is replaced by a centered
   *  spinner so the tile communicates "still loading" without leaving a
   *  stale or zero number in the value slot. */
  loading?: boolean;
  /** Accessible label for the loading spinner. */
  loadingLabel?: string;
  children?: React.ReactNode;
  /** Opens the matching detail tab when the stage is activated. */
  onSelect?: () => void;
};

function StageShell({
  tone,
  eyebrow,
  title,
  value,
  delta,
  subtitle,
  cornerBadge,
  titleIcon,
  titleClassName,
  loading = false,
  loadingLabel,
  children,
  onSelect,
}: StageShellProps) {
  const interactive = Boolean(onSelect);
  const shellClass = cn(
    "relative flex min-w-0 flex-1 flex-col rounded-2xl border-2 px-4 py-4 text-left sm:min-w-[160px]",
    toneStyles[tone],
    interactive &&
    "cursor-pointer transition hover:brightness-[1.06] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500/50",
  );

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!onSelect) return;
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onSelect();
    }
  };

  return (
    <div
      role={interactive ? "button" : undefined}
      tabIndex={interactive ? 0 : undefined}
      aria-label={interactive ? `Open ${title} tab` : undefined}
      onClick={interactive ? onSelect : undefined}
      onKeyDown={interactive ? onKeyDown : undefined}
      className={shellClass}
    >
      {cornerBadge ? (
        <span className="absolute right-2 top-2 rounded-md border border-purple-400/30 bg-purple-500/20 px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-purple-100">
          {cornerBadge}
        </span>
      ) : null}
      {eyebrow ? (
        <div className={cn("text-[10px] font-semibold uppercase tracking-[0.18em]", titleTone[tone])}>{eyebrow}</div>
      ) : null}
      <div className={cn("flex min-w-0 items-start gap-2.5", eyebrow ? "mt-1" : "")}>
        {titleIcon ? (
          <span className="mt-0.5 inline-flex shrink-0 self-start">{titleIcon}</span>
        ) : null}
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-baseline gap-2">
            <span className={cn("text-base font-semibold tracking-tight text-white", titleClassName)}>{title}</span>
            {delta ? (
              <span className="text-xs font-medium text-blue-300/90">{delta}</span>
            ) : null}
          </div>
        </div>
      </div>
      {loading ? (
        <div
          className="mt-2 flex flex-1 items-center justify-center"
          aria-live="polite"
        >
          <TileSpinner label={loadingLabel ?? `Loading ${title}`} />
        </div>
      ) : (
        <>
          <div className="mt-2 text-2xl font-bold tabular-nums tracking-tight text-white">
            {value.toLocaleString()}
          </div>
          {subtitle ? (
            <p className="mt-1.5 text-xs leading-snug text-muted-foreground">{subtitle}</p>
          ) : null}
          {children ? <div className="mt-3 min-h-0">{children}</div> : null}
        </>
      )}
    </div>
  );
}

/** Small inline spinner sized to drop into a tile's body. */
function TileSpinner({ label = "Loading" }: { label?: string }) {
  return (
    <span
      role="status"
      aria-label={label}
      className="inline-flex items-center text-muted-foreground/80"
    >
      <Loader2 className="h-6 w-6 animate-spin" aria-hidden />
    </span>
  );
}

function FlowArrow({ label }: { label: string }) {
  return (
    <div
      className="flex shrink-0 flex-col items-center justify-center gap-1 self-center px-1 py-4 lg:flex-row lg:px-2 lg:py-0"
      aria-hidden
    >
      <ChevronRight className="hidden h-5 w-5 text-muted-foreground/70 lg:block" />
      <div className="flex h-8 w-px flex-col bg-gradient-to-b from-border via-muted-foreground/35 to-border lg:hidden" />
      <span className="whitespace-nowrap rounded-full border border-border/80 bg-muted/25 px-2.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
        {label}
      </span>
    </div>
  );
}

type Props = {
  currentUser: CurrentUser | null;
  conversationsCount?: number;
  memoryCount?: number;
  /** Fallback when `datapackId` is not set or memories request fails. */
  learningsCount?: number;
  knowledgeCount?: number;
  /** Knowledge index size; wire from API when available. */
  knowledgeChunks?: number;
  /**
   * Datapack UUID (`datapack_registry.id`). When set, learnings proposal count is loaded from
   * GET /memories (`total`) using the signed-in user as `user_id`, same as the promotion panel.
   */
  datapackId?: string | null;
  /** Switch the datapack detail tab when a stage is clicked. */
  onSelectSection?: (tab: DatapackDetailTab) => void;
  /** True while the datapack bundle (memory_count / knowledge_count / etc.)
   *  is still being fetched. Each tile shows a centered spinner instead
   *  of a stale or zero number while this is true. */
  statusLoading?: boolean;
};

export function CollectiveLearningPipeline({
  currentUser,
  conversationsCount = 0,
  memoryCount = 0,
  learningsCount = 0,
  knowledgeCount = 0,
  knowledgeChunks = 0,
  datapackId = null,
  onSelectSection,
  statusLoading = false,
}: Props) {
  /** `undefined` = not loaded yet or reset; includes `0` when the API reports no memories. */
  const [totalPromotionProposals, setTotalPromotionProposals] = useState<number | undefined>(undefined);
  const [learningsCountLoading, setLearningsCountLoading] = useState(false);

  useEffect(() => {
    if (!datapackId?.trim()) {
      setTotalPromotionProposals(undefined);
      setLearningsCountLoading(false);
      return;
    }
    const ac = new AbortController();
    setLearningsCountLoading(true);
    setTotalPromotionProposals(undefined);
    const run = async () => {
      try {
        const user = currentUser ?? await getCurrentUser({ signal: ac.signal });
        const resp = await listMemories(
          {
            datapack_id: datapackId,
            user_id: user.account_id,
            user_type: MEMORY_LIST_DEFAULT_USER_TYPE,
            page: 1,
            page_size: 1,
          },
          { signal: ac.signal },
        );
        if (ac.signal.aborted) return;
        setTotalPromotionProposals(resp.total ?? 0);
      } catch {
        if (ac.signal.aborted) return;
        setTotalPromotionProposals(undefined);
      } finally {
        if (!ac.signal.aborted) setLearningsCountLoading(false);
      }
    };
    void run();
    return () => ac.abort();
  }, [datapackId]);

  const hasDatapack = Boolean(datapackId?.trim());
  const learningsDisplay = !hasDatapack
    ? learningsCount
    : learningsCountLoading
      ? 0
      : totalPromotionProposals !== undefined
        ? totalPromotionProposals
        : learningsCount;

  return (
    <section className="overflow-hidden rounded-2xl border border-border bg-gradient-to-b from-[#1c2438] via-[#171f2e] to-[#141a28]">
      <div className="border-b border-border/60 bg-[#1C2333]/40 px-5 py-4 sm:px-6">
        <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
          <div className="min-w-0 space-y-1">
            <div className="flex flex-wrap items-center gap-2">
              <h2 className="text-lg font-semibold tracking-tight text-white sm:text-xl">
                Collective learning
              </h2>
            </div>
            <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">
              How memory from individual agents flows into shared knowledge everyone can read.
            </p>
          </div>
        </div>
      </div>

      <div className="px-4 py-6 sm:px-6 sm:py-8">
        <div className="flex flex-col items-stretch gap-2 lg:flex-row lg:items-stretch lg:gap-1">
          <StageShell
            tone={DATAPACK_DETAIL_TAB.conversations}
            eyebrow=""
            title="Conversations"
            value={conversationsCount}
            delta=""
            subtitle=""
            titleIcon={
              <MessageSquare className="h-4 w-4 text-white" strokeWidth={2} aria-hidden />
            }
            titleClassName="text-slate-400"
            loading={statusLoading}
            loadingLabel="Loading conversations count"
            onSelect={onSelectSection ? () => onSelectSection(DATAPACK_DETAIL_TAB.conversations) : undefined}
          />

          <FlowArrow label="converts" />

          <StageShell
            tone={DATAPACK_DETAIL_TAB.memory}
            eyebrow=""
            title="ME (Memories)"
            value={memoryCount}
            delta=""
            subtitle=""
            titleIcon={<Sparkles className="h-4 w-4 text-sky-400" aria-hidden />}
            loading={statusLoading}
            loadingLabel="Loading memories count"
            onSelect={onSelectSection ? () => onSelectSection(DATAPACK_DETAIL_TAB.memory) : undefined}
          />

          <FlowArrow label="extract" />

          <StageShell
            tone={DATAPACK_DETAIL_TAB.learnings}
            eyebrow=""
            title="ME->KO (Learnings)"
            value={learningsDisplay ?? learningsCount}
            subtitle=""
            titleIcon={<ArrowLeftRight className="h-4 w-4 text-violet-400" aria-hidden />}
            loading={statusLoading || learningsCountLoading}
            loadingLabel="Loading learnings count"
            onSelect={onSelectSection ? () => onSelectSection(DATAPACK_DETAIL_TAB.learnings) : undefined}
          >
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="w-full border-purple-500/40 bg-purple-500/10 text-xs font-medium text-purple-100 hover:bg-purple-500/20"
              disabled={learningsCountLoading || !onSelectSection}
              onClick={(e) => {
                e.stopPropagation();
                onSelectSection?.(DATAPACK_DETAIL_TAB.learnings);
              }}
            >
              {learningsCountLoading ? (
                <span className="inline-flex items-center justify-center gap-2">
                  <Loader2 className="h-3.5 w-3.5 animate-spin shrink-0" aria-hidden />
                  Loading…
                </span>
              ) : (
                <>Review {learningsDisplay?.toLocaleString()} promotions →</>
              )}
            </Button>
          </StageShell>

          <FlowArrow label="promote" />

          <StageShell
            tone={DATAPACK_DETAIL_TAB.knowledge}
            eyebrow=""
            title="KO (Knowledge)"
            value={knowledgeCount}
            delta=""
            subtitle=""
            titleIcon={<BookOpen className="h-4 w-4 text-emerald-400" aria-hidden />}
            loading={statusLoading}
            loadingLabel="Loading knowledge count"
            onSelect={onSelectSection ? () => onSelectSection(DATAPACK_DETAIL_TAB.knowledge) : undefined}
          >
            <p className="text-[11px] leading-snug text-muted-foreground/95">
              <span className="tabular-nums text-foreground/85">
                {knowledgeChunks.toLocaleString()} chunk{knowledgeChunks === 1 ? "" : "s"}
              </span>
            </p>
          </StageShell>
        </div>
      </div>

    </section>
  );
}
