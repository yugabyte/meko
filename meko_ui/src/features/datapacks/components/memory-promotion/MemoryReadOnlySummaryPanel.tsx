/**
 * Read-only memories: paginated list shown on the memory-summary route.
 * Same card chrome as Learnings summary, without promote / keep actions.
 */

import { useCallback, useState } from "react";
import { useNavigate } from "react-router-dom";
import { cn } from "@/lib/utils";
import { ArrowLeftRight, ChevronLeft, ChevronRight, Loader2, AlertCircle, RefreshCw, Trash2 } from "lucide-react";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { toast } from "@/hooks/use-toast";
import {
  CONVERSATION_SOURCE,
  datapackConversationsPathWithSource,
} from "@/app/routes/paths";
import { ObserveButton } from "@/features/datapacks/components/ObserveButton";
import { useMemoryReadOnlyList } from "./useMemoryReadOnlyList";
import { PAGE_SIZE_OPTIONS } from "./useMemoryPromotion";
import { MemoryRowBody } from "./memoryRowBody";
import { deleteMemory, getApiErrorMessage, type Memory } from "@/lib/mekoApi";

type Props = {
  datapackName: string;
  datapackId: string;
};

export function MemoryReadOnlySummaryPanel({ datapackName, datapackId }: Props) {
  const navigate = useNavigate();
  const {
    loading,
    error,
    memories,
    total,
    totalPages,
    page,
    pageSize,
    accountId,
    accountLoading,
    accountError,
    sessionSummaryReady,
    rangeStart,
    rangeEnd,
    langfuseProjectId,
    loadMemories,
    goToPage,
    changePageSize,
  } = useMemoryReadOnlyList({ datapackId });

  // Memory id currently being deleted. Single-flight: at most one delete
  // in flight at a time so the row spinner is unambiguous and the user
  // can't double-fire.
  const [deletingId, setDeletingId] = useState<string | null>(null);

  // The memory the user clicked Delete on, awaiting confirmation. We
  // store the full object (not just id) so the confirm dialog can show
  // the memory's text — the user sees exactly what they're about to
  // erase. Clearing this closes the dialog. Same pattern Security ->
  // Revoke uses for token deletes.
  const [pendingDeleteMemory, setPendingDeleteMemory] = useState<Memory | null>(null);

  // Delete a memory the current user authored. The server enforces the
  // "only your own" rule via `meko_user_id = caller`, so a row from
  // another member would 404 anyway — but we also hide the button on
  // those rows, which is the primary UX gate. On success we re-run
  // `loadMemories()` so the page total / pagination stay correct.
  const handleDelete = useCallback(
    async (memoryId: string) => {
      if (deletingId) return;
      setDeletingId(memoryId);
      try {
        await deleteMemory(memoryId, datapackId);
        toast({ title: "Memory deleted" });
        setPendingDeleteMemory(null);
        await loadMemories();
      } catch (err) {
        toast({
          title: "Couldn't delete",
          description: getApiErrorMessage(err, "Delete failed."),
        });
      } finally {
        setDeletingId(null);
      }
    },
    [datapackId, deletingId, loadMemories],
  );

  const isValidConvId = (conversationId: string) =>
    !!conversationId && conversationId !== datapackId;

  // Langfuse / conversation viewer both expect IDs without dashes.
  const traceId = (conversationId: string) => conversationId.replace(/-/g, "");

  /** Navigate to the conversation viewer, optionally pre-selecting a
   *  specific message via the memoryId query param so it can be scrolled
   *  to and shimmered. The `from` flag tells the viewer to add an extra
   *  breadcrumb hop (Datapacks > Memory Summary > Conversation) so the
   *  user can step back to where they came from. */
  const goToConversation = (conversationId: string, memoryId: string) => {
    if (!isValidConvId(conversationId)) return;
    navigate(
      datapackConversationsPathWithSource(
        datapackName,
        traceId(conversationId),
        CONVERSATION_SOURCE.memorySummary,
        { memoryId },
        // Carry datapack_id through so the conversation viewer can
        // resolve unambiguously when names collide across share
        // boundaries.
        datapackId,
      ),
    );
  };

  const showPagination = total > 0;

  return (
    <TooltipProvider delayDuration={200}>
      <div className="rounded-xl border border-border bg-[#1C2333] p-6 space-y-4">
        <div className="flex items-center justify-between gap-4">
          <div className="flex min-w-0 items-center gap-4">
            <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-blue-500/15 ring-1 ring-blue-500/10">
              <ArrowLeftRight className="h-5 w-5 text-blue-200" strokeWidth={1.75} aria-hidden />
            </div>
            <div className="min-w-0">
              <h3 className="text-base font-semibold tracking-tight text-white">
                <span>Memories</span>
                {sessionSummaryReady ? (
                  <span>
                    {" "}
                    · {total} {total === 1 ? "memory" : "memories"}
                  </span>
                ) : (
                  <span className="font-normal text-muted-foreground"> · …</span>
                )}
              </h3>
              {accountError ? (
                <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                  Account could not be loaded for this list. Details appear below.
                </p>
              ) : sessionSummaryReady ? (
                <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                  Recent session memories for{" "}
                  <span className="font-medium text-white">{datapackName}</span>.
                </p>
              ) : (
                <p className="mt-1 text-xs leading-relaxed text-muted-foreground">Loading memories…</p>
              )}
            </div>
          </div>
          <Button
            variant="outline"
            className="gap-2 border-border bg-background text-muted-foreground hover:bg-muted hover:text-foreground"
            onClick={() => void loadMemories()}
            disabled={loading || accountLoading || !datapackId}
          >
            {loading || accountLoading ? (
              <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            ) : (
              <RefreshCw className="h-4 w-4" aria-hidden />
            )}
            {loading || accountLoading ? "Loading..." : "Refresh"}
          </Button>
        </div>

        {accountError ? (
          <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-300">
            <AlertCircle className="h-4 w-4 shrink-0" aria-hidden />
            {accountError}
          </div>
        ) : null}

        {error ? (
          <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-300">
            <AlertCircle className="h-4 w-4 shrink-0" aria-hidden />
            {error}
          </div>
        ) : null}

        {showPagination ? (
          <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border bg-[#0E1525] px-4 py-2.5 text-xs text-muted-foreground">
            <div>{total === 0 ? "0 memories" : `Showing ${rangeStart}–${rangeEnd} of ${total}`}</div>
            <div className="flex items-center gap-3">
              <label className="flex items-center gap-2">
                <span>Per page</span>
                <select
                  value={pageSize}
                  onChange={(e) => changePageSize(Number(e.target.value))}
                  disabled={loading}
                  className="rounded-md border border-border bg-background px-2 py-1 text-xs text-foreground focus:outline-none focus:ring-1 focus:ring-ring disabled:opacity-50"
                >
                  {PAGE_SIZE_OPTIONS.map((opt) => (
                    <option key={opt} value={opt}>
                      {opt}
                    </option>
                  ))}
                </select>
              </label>
              <div className="flex items-center gap-1">
                <Button
                  variant="outline"
                  size="sm"
                  className="gap-1 px-2"
                  type="button"
                  onClick={() => goToPage(page - 1)}
                  disabled={loading || page <= 1}
                  aria-label="Previous page"
                >
                  <ChevronLeft className="h-4 w-4" />
                  Prev
                </Button>
                <span className="px-2 text-muted-foreground">
                  Page <span className="text-white">{page}</span> of {totalPages}
                </span>
                <Button
                  variant="outline"
                  size="sm"
                  className="gap-1 px-2"
                  type="button"
                  onClick={() => goToPage(page + 1)}
                  disabled={loading || page >= totalPages}
                  aria-label="Next page"
                >
                  Next
                  <ChevronRight className="h-4 w-4" />
                </Button>
              </div>
            </div>
          </div>
        ) : null}

        {memories.length === 0 && !loading && !error && sessionSummaryReady ? (
          <p className="text-xs text-muted-foreground">No memories found for this datapack yet.</p>
        ) : null}

        {memories.length > 0 ? (
          <>
            <ul className="space-y-2">
              {memories.map((m) => {
                const canLink = m.conversation_id && isValidConvId(m.conversation_id);
                // Only the author may delete. In a shared datapack rows
                // from other members appear in the list but render without
                // the delete affordance; the server also enforces this.
                const isMine = Boolean(m.user_id) && m.user_id === accountId;
                return (
                  <li key={m.id}>
                    <button
                      type="button"
                      onClick={() => canLink && goToConversation(m.conversation_id!, m.id)}
                      className={cn(
                        "w-full rounded-lg border border-border bg-[#0E1525] px-4 py-3 text-left",
                        canLink
                          ? "cursor-pointer hover:bg-[#161d2e] hover:border-border/80 transition-colors"
                          : "cursor-default",
                      )}
                    >
                      <div className="flex items-start justify-between gap-3">
                        <MemoryRowBody memory={m} />
                        <div
                          // Inner action cluster stops click propagation so
                          // pressing Delete / Observe doesn't also navigate
                          // to the conversation viewer via the outer button.
                          className="flex shrink-0 items-center gap-1.5"
                          onClick={(e) => e.stopPropagation()}
                        >
                          {isMine ? (
                            <Tooltip>
                              <TooltipTrigger asChild>
                                <span className="inline-flex">
                                  <Button
                                    type="button"
                                    variant="outline"
                                    size="sm"
                                    className="gap-1.5 border-red-500/40 bg-red-500/10 text-red-300 hover:bg-red-500/20"
                                    // Don't delete on click — open the
                                    // confirm dialog instead. The
                                    // dialog runs the actual delete
                                    // after the user re-affirms with
                                    // the memory text visible.
                                    onClick={() => setPendingDeleteMemory(m)}
                                    disabled={deletingId !== null}
                                    aria-label="Delete memory"
                                  >
                                    {deletingId === m.id ? (
                                      <Loader2 className="h-3.5 w-3.5 animate-spin shrink-0" aria-hidden />
                                    ) : (
                                      <Trash2 className="h-3.5 w-3.5 shrink-0" aria-hidden />
                                    )}
                                    Delete
                                  </Button>
                                </span>
                              </TooltipTrigger>
                              <TooltipContent side="top">Delete this memory</TooltipContent>
                            </Tooltip>
                          ) : null}
                          {canLink ? (
                            <ObserveButton
                              projectId={langfuseProjectId}
                              sessionId={traceId(m.conversation_id!)}
                            />
                          ) : null}
                        </div>
                      </div>
                    </button>
                  </li>
                );
              })}
            </ul>
          </>
        ) : null}
      </div>

      {/* Confirm-delete dialog. Mirrors the Settings → Security
          Revoke flow: the destructive action stays open while the
          request is in flight so the user sees the spinner, and the
          memory text is shown inline so they re-read what they're
          about to erase. Closing the dialog (via Cancel, ESC, or
          the X) clears `pendingDeleteMemory`. */}
      <AlertDialog
        open={pendingDeleteMemory !== null}
        onOpenChange={(next) => {
          if (!next && deletingId === null) setPendingDeleteMemory(null);
        }}
      >
        <AlertDialogContent className="bg-[#1C2333] border-border">
          <AlertDialogHeader>
            <AlertDialogTitle className="text-white">Delete this memory?</AlertDialogTitle>
            <AlertDialogDescription className="text-muted-foreground">
              This permanently removes your memory from this datapack. It can't be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          {pendingDeleteMemory ? (
            <blockquote
              className="rounded-md border border-border bg-[#0E1525] px-3 py-2 text-sm italic text-foreground/90"
              title={pendingDeleteMemory.data}
            >
              &ldquo;{pendingDeleteMemory.data}&rdquo;
            </blockquote>
          ) : null}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deletingId !== null}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                // AlertDialogAction closes the dialog by default; we
                // want it to stay open until the delete resolves so
                // the spinner is visible. handleDelete clears
                // `pendingDeleteMemory` itself on success.
                e.preventDefault();
                if (pendingDeleteMemory) {
                  void handleDelete(pendingDeleteMemory.id);
                }
              }}
              disabled={deletingId !== null}
              className="gap-1.5 bg-red-600 text-white hover:bg-red-700"
            >
              {deletingId !== null ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
              ) : (
                <Trash2 className="h-3.5 w-3.5" aria-hidden />
              )}
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </TooltipProvider>
  );
}
