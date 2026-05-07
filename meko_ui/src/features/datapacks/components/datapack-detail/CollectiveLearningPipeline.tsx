/**
 * “Collective learning” ribbon: Conversations → Memories → Learnings → (promote) → Collective Memory + Shared Knowledge.
 */
import { useEffect, useState } from "react";
import type { KeyboardEvent, ReactNode } from "react";
import {
  ArrowLeftRight,
  BookOpen,
  ChevronRight,
  Loader2,
  MessageSquare,
  Settings,
  Sparkles,
  Upload,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
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
  memories: "border-blue-500/40 bg-gradient-to-br from-blue-950/70 via-[#0f1729] to-[#0b1220]",
  learnings:
    "border-purple-500/50 bg-gradient-to-br from-purple-950/65 via-[#141228] to-[#0b1220] shadow-[0_0_24px_rgba(168,85,247,0.12)]",
  knowledge: "border-emerald-900/50 bg-gradient-to-br from-emerald-950/55 via-[#0c1814] to-[#0b1220]",
  "collective-memory":
    "border-sky-500/40 bg-gradient-to-br from-sky-950/65 via-[#0c1828] to-[#0b1220]",
};

const titleTone: Record<DatapackDetailTab, string> = {
  conversations: "text-slate-200/95",
  memories: "text-blue-200/95",
  learnings: "text-purple-200/95",
  knowledge: "text-emerald-200/95",
  "collective-memory": "text-sky-200/95",
};

type StageShellProps = {
  tone: DatapackDetailTab;
  eyebrow: string;
  title: string;
  value?: number;
  delta?: string;
  subtitle: string;
  cornerBadge?: ReactNode;
  cornerBadgeTone?: "purple" | "green";
  cornerBadgePulse?: boolean;
  titleIcon?: ReactNode;
  titleClassName?: string;
  loading?: boolean;
  loadingLabel?: string;
  /** Rendered below the large value (e.g. collective memory empty state). */
  afterValue?: ReactNode;
  children?: React.ReactNode;
  onSelect?: () => void;
  flexGrow?: boolean;
};

function StageShell({
  tone,
  eyebrow,
  title,
  value,
  delta,
  subtitle,
  cornerBadge,
  cornerBadgeTone = "purple",
  cornerBadgePulse = false,
  titleIcon,
  titleClassName,
  loading = false,
  loadingLabel,
  afterValue,
  children,
  onSelect,
  flexGrow = true,
}: StageShellProps) {
  const interactive = Boolean(onSelect);
  const shellClass = cn(
    "relative flex h-fit min-w-0 shrink-0 flex-col rounded-2xl border-2 px-4 py-3 text-left sm:min-w-[160px]",
    flexGrow ? "flex-1" : "w-full flex-none",
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

  const badgeClass =
    cornerBadgeTone === "green"
      ? "border-emerald-400/45 bg-emerald-500/20 text-emerald-100"
      : "border-purple-400/40 bg-purple-500/25 text-purple-100";

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
        <span
          className={cn(
            "absolute right-2 top-2 z-[1] inline-flex items-center gap-1 rounded-md border px-2 py-0.5 text-[10px] font-semibold tabular-nums tracking-wide",
            badgeClass,
            cornerBadgePulse &&
            cornerBadgeTone === "purple" &&
            "animate-pulse shadow-[0_0_14px_rgba(168,85,247,0.55)] ring-1 ring-purple-400/30",
            cornerBadgePulse &&
            cornerBadgeTone === "green" &&
            "animate-pulse shadow-[0_0_12px_rgba(52,211,153,0.35)] ring-1 ring-emerald-400/25",
          )}
        >
          {cornerBadge}
        </span>
      ) : null}
      {eyebrow ? (
        <div className={cn("text-center text-[10px] font-semibold uppercase tracking-[0.18em]", titleTone[tone])}>
          {eyebrow}
        </div>
      ) : null}
      <div className={cn("flex w-full min-w-0 flex-col items-center gap-2", eyebrow ? "mt-1" : "")}>
        {titleIcon ? <span className="inline-flex shrink-0">{titleIcon}</span> : null}
        <div className="flex w-full min-w-0 flex-wrap items-baseline justify-center gap-2">
          <span className={cn("text-center text-base font-semibold tracking-tight text-white", titleClassName)}>
            {title}
          </span>
          {delta ? <span className="text-xs font-medium text-blue-300/90">{delta}</span> : null}
        </div>
      </div>
      {loading ? (
        <div className="mt-2 flex min-h-[3rem] items-center justify-center py-1" aria-live="polite">
          <TileSpinner label={loadingLabel ?? `Loading ${title}`} />
        </div>
      ) : (
        <>
          {value != null ? (
            <div className="mt-2 w-full text-center text-3xl font-bold tabular-nums tracking-tight text-white sm:text-[2rem] sm:leading-none">
              {value.toLocaleString()}
            </div>
          ) : null}
          {afterValue ? <div className="mt-2 w-full min-w-0">{afterValue}</div> : null}
          {subtitle ? <p className="mt-1.5 text-xs leading-snug text-muted-foreground">{subtitle}</p> : null}
          {children ? <div className="mt-3 min-h-0">{children}</div> : null}
        </>
      )}
    </div>
  );
}

function TileSpinner({ label = "Loading" }: { label?: string }) {
  return (
    <span role="status" aria-label={label} className="inline-flex items-center text-muted-foreground/80">
      <Loader2 className="h-6 w-6 animate-spin" aria-hidden />
    </span>
  );
}

function FlowArrow({ label, direction = "horizontal" }: { label: string; direction?: "horizontal" | "vertical" }) {
  const vertical = direction === "vertical";
  return (
    <div
      className={cn(
        "flex shrink-0 flex-col items-center justify-center gap-1 self-center px-1",
        vertical ? "py-2" : "py-4 lg:flex-row lg:px-2 lg:py-0",
      )}
      aria-hidden
    >
      <ChevronRight
        className={cn("h-5 w-5 text-muted-foreground/70", vertical ? "hidden" : "hidden lg:block")}
      />
      <div
        className={cn(
          "flex h-8 w-px flex-col bg-gradient-to-b from-border via-muted-foreground/35 to-border",
          vertical ? "" : "lg:hidden",
        )}
      />
      <span className="whitespace-nowrap rounded-full border border-border/80 bg-muted/25 px-2.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
        {label}
      </span>
    </div>
  );
}

/** Desktop-only: compact “PROMOTE / to both” connector (fixed size — not stretched with the pipeline row). */
function PromoteFlowBracket() {
  const line = "bg-muted-foreground/45";
  return (
    <div className="relative mx-1 hidden h-[92px] w-[4.25rem] shrink-0 self-center lg:block" aria-hidden>
      {/* Learnings → spine */}
      <div className={`absolute left-0 top-[40px] z-[1] h-px w-[18px] ${line}`} />
      {/* Single short spine (runs behind the pill) */}
      <div className={`absolute left-[18px] top-1 z-0 h-[62px] w-px ${line}`} />
      {/* Upper tee toward Collective Memory */}
      <div className={`absolute left-2 top-4 h-px w-2 ${line}`} />
      {/* PROMOTE (covers middle of spine) */}
      <div className="absolute left-1/2 top-[30px] z-[2] flex -translate-x-1/2 flex-col items-center rounded-md border border-border/70 bg-[#141b2c]/95 px-2 py-1 shadow-md backdrop-blur-sm">
        <span className="text-[10px] font-bold uppercase tracking-wide text-muted-foreground">Promote</span>
      </div>
      {/* Lower tee toward Shared Knowledge */}
      <div className={`absolute left-2 top-[60px] h-px w-2 ${line}`} />
      <p className="absolute left-1/2 top-[72px] -translate-x-1/2 whitespace-nowrap text-[9px] font-medium text-muted-foreground/75">
        to both
      </p>
    </div>
  );
}

type Props = {
  currentUser: CurrentUser | null;
  conversationsCount?: number;
  memoryCount?: number;
  learningsCount?: number;
  knowledgeCount?: number;
  knowledgeChunks?: number;
  datapackId?: string | null;
  onSelectSection?: (tab: DatapackDetailTab) => void;
  onUploadKnowledgeFiles?: () => void;
  statusLoading?: boolean;
};

export function CollectiveLearningPipeline({
  currentUser,
  conversationsCount = 0,
  memoryCount = 0,
  learningsCount = 0,
  knowledgeCount = 0,
  knowledgeChunks: _knowledgeChunks = 0,
  datapackId = null,
  onSelectSection,
  onUploadKnowledgeFiles,
  statusLoading = false,
}: Props) {
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
        const user = currentUser ?? (await getCurrentUser({ signal: ac.signal }));
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
  }, [datapackId, currentUser]);

  const hasDatapack = Boolean(datapackId?.trim());
  const learningsDisplay = !hasDatapack
    ? learningsCount
    : learningsCountLoading
      ? 0
      : totalPromotionProposals !== undefined
        ? totalPromotionProposals
        : learningsCount;

  const learningsReadyBadge =
    !statusLoading &&
    !learningsCountLoading &&
    learningsDisplay >= 1 &&
    `${learningsDisplay} ready`;

  const knowledgeFileBadge =
    !statusLoading && knowledgeCount >= 1 ? (
      <span className="inline-flex items-center gap-0.5">
        <Upload className="size-3 shrink-0 opacity-90" aria-hidden />
        +{knowledgeCount} file{knowledgeCount === 1 ? "" : "s"}
      </span>
    ) : undefined;

  const reviewLabel = learningsCountLoading
    ? "Loading…"
    : learningsDisplay === 1
      ? "Review 1 promotion →"
      : `Review ${learningsDisplay.toLocaleString()} promotions →`;

  return (
    <TooltipProvider delayDuration={200}>
      <section className="overflow-hidden rounded-2xl border border-border bg-gradient-to-b from-[#1c2438] via-[#171f2e] to-[#141a28]">
        <div className="border-b border-border/60 bg-[#1C2333]/40 px-5 py-4 sm:px-6">
          <div className="min-w-0 space-y-1">
            <h2 className="text-lg font-semibold tracking-tight text-white sm:text-xl">Collective learning</h2>
            <p className="text-sm leading-normal text-muted-foreground text-pretty">
              How memory from individual agents flows into shared knowledge everyone can read. Each promotion
              populates <span className="font-semibold text-sky-400/95">both</span> destinations; Shared Knowledge can
              also receive files directly.
            </p>
          </div>
        </div>

        <div className="px-4 py-6 sm:px-6 sm:py-8">
          <div className="flex flex-col items-stretch gap-2 lg:flex-row lg:items-center lg:gap-0">
            <StageShell
              tone={DATAPACK_DETAIL_TAB.conversations}
              eyebrow=""
              title="Conversations"
              value={conversationsCount}
              delta=""
              subtitle=""
              titleIcon={<MessageSquare className="h-4 w-4 text-white" strokeWidth={2} aria-hidden />}
              titleClassName="text-slate-400"
              loading={statusLoading}
              loadingLabel="Loading conversations count"
              onSelect={onSelectSection ? () => onSelectSection(DATAPACK_DETAIL_TAB.conversations) : undefined}
            />

            <FlowArrow label="extract" />

            <StageShell
              tone={DATAPACK_DETAIL_TAB.memories}
              eyebrow=""
              title="Memories"
              value={memoryCount}
              delta=""
              subtitle=""
              titleIcon={<Sparkles className="h-4 w-4 text-sky-400" aria-hidden />}
              loading={statusLoading}
              loadingLabel="Loading memories count"
              onSelect={onSelectSection ? () => onSelectSection(DATAPACK_DETAIL_TAB.memories) : undefined}
            />

            <FlowArrow label="extract" />

            <StageShell
              tone={DATAPACK_DETAIL_TAB.learnings}
              eyebrow=""
              title="Learnings"
              delta=""
              subtitle=""
              cornerBadge={learningsReadyBadge || undefined}
              cornerBadgePulse={Boolean(learningsReadyBadge)}
              titleIcon={<ArrowLeftRight className="h-4 w-4 text-violet-400" aria-hidden />}
              loading={statusLoading || learningsCountLoading}
              loadingLabel="Loading learnings count"
              onSelect={onSelectSection ? () => onSelectSection(DATAPACK_DETAIL_TAB.learnings) : undefined}
            >
              <Button
                type="button"
                size="sm"
                className="w-full border-0 bg-purple-600 text-xs font-semibold text-white shadow-md hover:bg-purple-500 disabled:opacity-60"
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
                  reviewLabel
                )}
              </Button>
            </StageShell>

            <div className="hidden lg:contents">
              <PromoteFlowBracket />
            </div>
            <div className="lg:hidden">
              <FlowArrow label="promote" />
            </div>

            <div
              className="relative flex min-w-0 flex-1 flex-col gap-3 rounded-2xl border-2 border-white/20 bg-[#0c1018]/40 px-4 pb-4 pt-5 sm:px-5 sm:pb-5 sm:pt-6 lg:min-w-[220px]"
              aria-labelledby="collective-learning-me-ko-label"
            >
              <span
                id="collective-learning-me-ko-label"
                className="pointer-events-none absolute left-1/2 top-0 z-10 -translate-x-1/2 -translate-y-1/2 whitespace-nowrap rounded-full border border-border/60 bg-[#0d121c] px-3 py-1 text-[11px] font-semibold tracking-[0.12em] text-muted-foreground shadow-md ring-2 ring-[#141a28]"
              >
                me → ko
              </span>
              <StageShell
                flexGrow={false}
                tone={DATAPACK_DETAIL_TAB.collectiveMemory}
                eyebrow=""
                title="Collective Memory"
                value={learningsCount}
                subtitle=""
                titleIcon={<Sparkles className="h-4 w-4 text-sky-400" aria-hidden />}
                loading={statusLoading}
                loadingLabel="Loading collective memory count"
                afterValue={
                  !statusLoading && learningsCount === 0 ? (
                    <p className="text-center text-[10px] font-semibold uppercase  text-muted-foreground">
                      Awaiting first promotion
                    </p>
                  ) : null
                }
                onSelect={onSelectSection ? () => onSelectSection(DATAPACK_DETAIL_TAB.collectiveMemory) : undefined}
              />

              <StageShell
                flexGrow={false}
                tone={DATAPACK_DETAIL_TAB.knowledge}
                eyebrow=""
                title="Shared Knowledge"
                value={knowledgeCount}
                delta=""
                subtitle=""
                cornerBadge={knowledgeFileBadge}
                cornerBadgeTone="green"
                titleIcon={<BookOpen className="h-4 w-4 text-emerald-400" aria-hidden />}
                loading={statusLoading}
                loadingLabel="Loading knowledge count"
                onSelect={onSelectSection ? () => onSelectSection(DATAPACK_DETAIL_TAB.knowledge) : undefined}
              />
            </div>
          </div>
        </div>

        <div className="border-t border-border/55 bg-[#161d2e]/25 px-4 py-3 sm:px-6">
          <div className="flex flex-wrap items-center justify-end gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="gap-2 border-border/80 bg-background/40 text-foreground hover:bg-muted/30"
              onClick={() => onUploadKnowledgeFiles?.()}
            >
              <Upload className="h-4 w-4 shrink-0 opacity-90" aria-hidden />
              Upload knowledge files
            </Button>
            <Tooltip>
              <TooltipTrigger asChild>
                <span className="inline-flex">
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    disabled
                    className="gap-2 border-border/80 bg-background/30 opacity-70"
                    aria-label="Tune thresholds (coming soon)"
                  >
                    <Settings className="h-4 w-4 shrink-0 opacity-90" aria-hidden />
                    Tune thresholds
                  </Button>
                </span>
              </TooltipTrigger>
              <TooltipContent side="top" className="max-w-[14rem]">
                Coming soon
              </TooltipContent>
            </Tooltip>
          </div>
        </div>
      </section>
    </TooltipProvider>
  );
}
