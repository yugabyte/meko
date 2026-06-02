/**
 * “Collective learning” ribbon: Conversations → Memories → Learnings → (promote) → Collective Memory + Shared Knowledge.
 */
import { useEffect, useState } from "react";
import type { KeyboardEvent, ReactNode } from "react";
import {
  ArrowLeftRight,
  ArrowUpRight,
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

// Tile gradient tones — one per tab. Theme-aware: the default (light
// mode) is a soft tinted card with the accent colour suggesting which
// pipeline stage it represents; the `dark:` variant restores the
// original brand-art dark gradient where the deep `from-X-950/Y`
// corner glows against navy. Both modes keep the same colored border
// so the section identity (purple = learnings, blue = memories, etc.)
// reads at a glance in either theme.
const toneStyles: Record<DatapackDetailTab, string> = {
  conversations:
    "border-slate-300 bg-gradient-to-br from-slate-100 via-card to-card " +
    "dark:border-slate-500/35 dark:from-slate-900/80 dark:via-meko-navy-900 dark:to-meko-navy-950",
  memories:
    "border-blue-300 bg-gradient-to-br from-blue-50 via-card to-card " +
    "dark:border-blue-500/40 dark:from-blue-950/70 dark:via-meko-navy-900 dark:to-meko-navy-950",
  learnings:
    "border-purple-400 bg-gradient-to-br from-purple-50 via-card to-card " +
    "dark:border-purple-500/50 dark:from-purple-950/65 dark:via-[#141228] dark:to-meko-navy-950 dark:shadow-[0_0_24px_rgba(168,85,247,0.12)]",
  knowledge:
    "border-emerald-300 bg-gradient-to-br from-emerald-50 via-card to-card " +
    "dark:border-emerald-900/50 dark:from-emerald-950/55 dark:via-meko-navy-900 dark:to-meko-navy-950",
  "collective-memory":
    "border-sky-300 bg-gradient-to-br from-sky-50 via-card to-card " +
    "dark:border-sky-500/40 dark:from-sky-950/65 dark:via-meko-navy-900 dark:to-meko-navy-950",
};

const titleTone: Record<DatapackDetailTab, string> = {
  // Slate-700 reads as muted grey against the other accent-saturated
  // titles (blue/purple/emerald-700). Using the semantic foreground
  // token gives it the same visual weight in light mode while still
  // pairing with the slate-200/95 pastel in dark mode.
  conversations: "text-foreground dark:text-slate-200/95",
  memories: "text-blue-700 dark:text-blue-200/95",
  learnings: "text-purple-700 dark:text-purple-200/95",
  knowledge: "text-emerald-700 dark:text-emerald-200/95",
  "collective-memory": "text-sky-700 dark:text-sky-200/95",
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
  // The "open" arrow is a discoverability cue in the top-left corner —
  // signals the whole tile navigates to a summary page. Shown on every
  // interactive tile (top-left position doesn't conflict with the bottom
  // "Review N promotions" button on Learnings or the top-right cornerBadge).
  const showOpenIndicator = interactive;
  const shellClass = cn(
    "group relative flex h-fit min-w-0 shrink-0 flex-col rounded-2xl border-2 px-4 py-3 text-left sm:min-w-[160px]",
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
      ? "border-emerald-400/45 bg-emerald-500/20 text-emerald-700 dark:text-emerald-100"
      : "border-purple-400/40 bg-purple-500/25 text-purple-700 dark:text-purple-100";

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
      {showOpenIndicator ? (
        <ArrowUpRight
          className={cn(
            // Top-left so it never overlaps with the existing top-right
            // cornerBadge (Shared Knowledge "+2 files", Learnings "N ready").
            "pointer-events-none absolute left-2 top-2 h-4 w-4 text-white/85 transition-all duration-150",
            "group-hover:translate-x-0.5 group-hover:-translate-y-0.5 group-hover:text-white",
            "group-focus-visible:text-white",
          )}
          aria-hidden
        />
      ) : null}
      {eyebrow ? (
        <div className={cn("text-center text-[10px] font-semibold uppercase tracking-[0.18em]", titleTone[tone])}>
          {eyebrow}
        </div>
      ) : null}
      <div className={cn("flex w-full min-w-0 flex-col items-center gap-2", eyebrow ? "mt-1" : "")}>
        {titleIcon ? <span className="inline-flex shrink-0">{titleIcon}</span> : null}
        <div className="flex w-full min-w-0 flex-wrap items-baseline justify-center gap-2">
          <span className={cn("text-center text-base font-semibold tracking-tight text-foreground", titleClassName)}>
            {title}
          </span>
          {delta ? <span className="text-xs font-medium text-blue-700 dark:text-blue-300/90">{delta}</span> : null}
        </div>
      </div>
      {loading ? (
        <div className="mt-2 flex min-h-[3rem] items-center justify-center py-1" aria-live="polite">
          <TileSpinner label={loadingLabel ?? `Loading ${title}`} />
        </div>
      ) : (
        <>
          {value != null ? (
            <div className="mt-2 w-full text-center text-3xl font-bold tabular-nums tracking-tight text-foreground sm:text-[2rem] sm:leading-none">
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
      <div className="absolute left-1/2 top-[30px] z-[2] flex -translate-x-1/2 flex-col items-center rounded-md border border-border/70 bg-input/95 px-2 py-1 shadow-md backdrop-blur-sm">
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
  knowledgeFileCount?: number;
  knowledgeFileAndPromotedCount?: number;
  collectiveMemoryCount?: number;
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
  knowledgeFileCount = 0,
  collectiveMemoryCount = 0,
  knowledgeFileAndPromotedCount = 0,
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
      : learningsCount

  const learningsReadyBadge =
    !statusLoading &&
    !learningsCountLoading &&
    learningsDisplay >= 1 &&
    `${learningsDisplay} ready`;

  const knowledgeFileBadge =
    !statusLoading && knowledgeFileCount >= 1 ? (
      <span className="inline-flex items-center gap-0.5">
        <Upload className="size-3 shrink-0 opacity-90" aria-hidden />
        +{knowledgeFileCount} file{knowledgeFileCount === 1 ? "" : "s"}
      </span>
    ) : undefined;

  const reviewLabel = learningsCountLoading
    ? "Loading…"
    : learningsDisplay === 1
      ? "Review 1 promotion →"
      : `Review ${learningsDisplay.toLocaleString()} promotions →`;

  return (
    <TooltipProvider delayDuration={200}>
      <section className="overflow-hidden rounded-2xl border border-border bg-gradient-to-b from-card via-input to-input">
        <div className="border-b border-border/60 bg-card/40 px-5 py-4 sm:px-6">
          <div className="min-w-0 space-y-1">
            <h2 className="text-lg font-semibold tracking-tight text-foreground sm:text-xl">Collective learning</h2>
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
              titleIcon={<MessageSquare className="h-4 w-4 text-foreground" strokeWidth={2} aria-hidden />}
              // No `titleClassName` override — let it inherit the
              // default `text-foreground` so the Conversations title
              // matches the visual weight of the other accent titles
              // (Memories/blue, Learnings/purple, etc.). The previous
              // `text-slate-400` override rendered as faded light-grey
              // in light mode, throwing off the row balance.
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
                // `text-white` (not `text-foreground`) so the CTA stays
                // light-text-on-purple in both modes — the standard
                // colored-button convention. Stepped one stop darker
                // (purple-700 → -800 hover) so it doesn't shout against
                // the soft purple-50 tile in light mode while still
                // reading as the section's accent in dark mode.
                // "Subtle in light, saturated in dark" — light mode shows
                // a soft purple-tint pill with dark purple text so the
                // button doesn't shout against the soft purple-50 tile
                // background; dark mode keeps the original saturated
                // purple CTA on the dark gradient tile. Same pattern as
                // the other "promote" CTAs (`Promote to Ko`,
                // `Promote to Knowledge`).
                // `min-w-0 whitespace-normal h-auto py-2 leading-tight`
                // lets "Review N promotions →" wrap onto two lines inside
                // the tile when the viewport is narrow, instead of
                // overflowing the tile's border (shadcn Button defaults
                // to `whitespace-nowrap` + fixed height, which is what
                // pushed the text out the sides at narrow widths).
                className="w-full min-w-0 whitespace-normal h-auto py-2 leading-tight border-0 bg-purple-100 text-xs font-semibold text-purple-700 shadow-md hover:bg-purple-200 dark:bg-purple-500 dark:text-white dark:hover:bg-purple-600 disabled:opacity-60"
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
              // `border-foreground/20` instead of `border-white/20` so
              // the "me → ko" wrapper outline is visible in both modes
              // — `white/20` collapsed to invisible white-on-white on
              // the light page bg, so the grouping read as a borderless
              // floating pair of tiles.
              className="relative flex min-w-0 flex-1 flex-col gap-3 rounded-2xl border-2 border-foreground/20 bg-input/40 px-4 pb-4 pt-5 sm:px-5 sm:pb-5 sm:pt-6 lg:min-w-[220px]"
              aria-labelledby="collective-learning-me-ko-label"
            >
              <span
                id="collective-learning-me-ko-label"
                className="pointer-events-none absolute left-1/2 top-0 z-10 -translate-x-1/2 -translate-y-1/2 whitespace-nowrap rounded-full border border-border/60 bg-input px-3 py-1 text-[11px] font-semibold tracking-[0.12em] text-muted-foreground shadow-md ring-2 ring-input"
              >
                me → ko
              </span>
              <StageShell
                flexGrow={false}
                tone={DATAPACK_DETAIL_TAB.collectiveMemory}
                eyebrow=""
                title="Collective Memory"
                value={collectiveMemoryCount}
                subtitle=""
                titleIcon={<Sparkles className="h-4 w-4 text-sky-400" aria-hidden />}
                loading={statusLoading}
                loadingLabel="Loading collective memory count"
                afterValue={
                  !statusLoading && collectiveMemoryCount === 0 ? (
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
                value={knowledgeFileAndPromotedCount}
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

        <div className="border-t border-border/55 bg-input/25 px-4 py-3 sm:px-6">
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
