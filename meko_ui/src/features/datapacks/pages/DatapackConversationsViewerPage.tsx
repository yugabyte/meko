// Full conversation viewer route: `/datapacks/:datapack_id/conversations[/:conversationId]`.
//
// Layout mirrors a chat client — left rail of conversations, right
// thread pane for the selected one, and an on-demand slide-over
// drawer with the per-message trace tree. The page owns the URL
// state so deep links to a specific conversation work and so the
// rail and thread stay in sync without prop-drilling a ref.
//
// Empty state when no conversation is selected (either because the
// datapack has none, or the URL didn't include one and the first
// page came back empty) — the rail is still visible so the user can
// pick one.

import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { MessageSquare } from "lucide-react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { ConversationListRail } from "@/features/datapacks/components/conversations-viewer/ConversationListRail";
import { useDatapackName } from "@/features/datapacks/hooks/useDatapackName";
import { ConversationThreadPane } from "@/features/datapacks/components/conversations-viewer/ConversationThreadPane";
import { TraceDrawer } from "@/features/datapacks/components/conversations-viewer/TraceDrawer";
import {
  CONVERSATION_SOURCE,
  CONVERSATION_SOURCE_QUERY_PARAM,
  ROUTES,
  datapackCollectiveMemorySummaryPath,
  datapackConversationsPath,
  datapackDetailPath,
  datapackMemorySummaryPath,
  type ConversationSource,
} from "@/app/routes/paths";
import type { DatapackConversation } from "@/lib/api/mekoApi";

/** Drives an extra breadcrumb hop when the viewer is reached from one of
 *  the summary pages, so the user can step back to where they came from.
 *  Keyed by {@link ConversationSource} so a typo or rename is caught by
 *  the compiler. */
const SOURCE_BREADCRUMB: Record<
  ConversationSource,
  (datapackId: string) => { label: string; href: string }
> = {
  [CONVERSATION_SOURCE.memorySummary]: (datapackId) => ({
    label: "Memory Summary",
    href: datapackMemorySummaryPath(datapackId),
  }),
  [CONVERSATION_SOURCE.collectiveMemorySummary]: (datapackId) => ({
    label: "Collective Memory Summary",
    href: datapackCollectiveMemorySummaryPath(datapackId),
  }),
};

function isConversationSource(value: string): value is ConversationSource {
  return (Object.values(CONVERSATION_SOURCE) as string[]).includes(value);
}

type Props = {
  datapackId: string;
  /** From URL; undefined when on the bare /conversations route. The
   *  rail will auto-select the newest conversation in that case. */
  conversationId?: string;
};

export default function DatapackConversationsViewerPage({
  datapackId,
  conversationId,
}: Props) {
  const navigate = useNavigate();
  const datapackName = useDatapackName(datapackId);
  const [searchParams] = useSearchParams();
  const targetMemoryId = searchParams.get("memoryId") ?? "";
  const sourceCrumb = useMemo(() => {
    const from = searchParams.get(CONVERSATION_SOURCE_QUERY_PARAM) ?? "";
    if (!isConversationSource(from)) return null;
    return SOURCE_BREADCRUMB[from](datapackId);
  }, [searchParams, datapackId]);

  /** Currently open trace drawer message (null = drawer closed).
   *  Owned at the page level so the drawer survives thread re-fetches
   *  and conversation switches reset it cleanly. */
  const [traceMessageId, setTraceMessageId] = useState<string | null>(null);
  /** Span to focus in the drawer (from "View Trace" on a Decision Trace
   *  step). Cleared when the drawer is opened via other entry points. */
  const [traceFocusSpanId, setTraceFocusSpanId] = useState<string | undefined>(undefined);
  /** When true, the TraceDrawer shimmers its memories panel to draw
   *  the user's eye after opening via the memory hover card. */
  const [shimmerMemories, setShimmerMemories] = useState(false);
  /** mem0 row id whose card the drawer should flash amber. Set ONLY by the
   *  `?memoryId=` deep-link reveal — manual opens (badge click, "View Trace")
   *  clear it so a manual click only ever runs the white sweep, never the
   *  yellow card flash, regardless of what's in the URL. */
  const [cardFlashMemoryId, setCardFlashMemoryId] = useState<string | null>(null);
  /** Author email of the selected conversation, sourced from the rail's
   *  list rows (the thread detail endpoint omits it). Forwarded to the
   *  header so it can render the author chip. */
  const [selectedEmail, setSelectedEmail] = useState<string | undefined>(undefined);
  const handleSelectedEmailChange = useCallback(
    (email: string | undefined) => setSelectedEmail(email),
    [],
  );

  // Closing the drawer when the conversation changes prevents stale
  // trace data from shimmering while the new thread loads.
  useEffect(() => {
    setTraceMessageId(null);
    setTraceFocusSpanId(undefined);
  }, [conversationId]);

  const handleSelect = useCallback(
    (id: string) => {
      if (id === conversationId) return;
      navigate(datapackConversationsPath(datapackId, id), { replace: false });
    },
    [navigate, datapackId, conversationId],
  );

  /** Auto-pick newest conversation when the user lands on the bare
   *  /conversations route. Replace (not push) so the back button
   *  doesn't bounce them between empty and selected. */
  const handleFirstPageLoaded = useCallback(
    (conversations: DatapackConversation[]) => {
      if (conversationId) return;
      const first = conversations[0];
      if (!first) return;
      navigate(datapackConversationsPath(datapackId, first.id), {
        replace: true,
      });
    },
    [navigate, datapackId, conversationId],
  );

  return (
    <div className="flex h-screen flex-1 flex-col overflow-hidden bg-background text-foreground">
      <AppWorkspaceHeader />

      <main className="flex w-full flex-1 flex-col overflow-hidden">
        <div className="border-b border-border px-6 pt-4 pb-3 shrink-0">
          <DatapackBreadcrumbs
            items={[
              { label: "Datapacks", href: ROUTES.datapacks },
              { label: datapackName, href: datapackDetailPath(datapackId) },
              ...(sourceCrumb ? [sourceCrumb] : []),
              { label: "Conversations" },
            ]}
            className="mb-2"
          />
          <div className="flex items-center gap-2.5">
            <MessageSquare className="h-5 w-5 shrink-0 text-purple-300" aria-hidden />
            <h1 className="truncate text-xl font-semibold tracking-tight text-foreground">
              Conversations
            </h1>
            <span className="truncate text-sm text-muted-foreground">· {datapackName}</span>
          </div>
        </div>

        <div className="grid flex-1 grid-cols-[340px_1fr] min-h-0 overflow-hidden">
          <ConversationListRail
            datapackId={datapackId}
            selectedId={conversationId ?? ""}
            onSelect={handleSelect}
            onFirstPageLoaded={handleFirstPageLoaded}
            onSelectedEmailChange={handleSelectedEmailChange}
          />

          <section className="min-h-0 overflow-hidden">
            {conversationId ? (
              <ConversationThreadPane
                datapackId={datapackId}
                datapackName={datapackName}
                conversationId={conversationId}
                userEmail={selectedEmail}
                onOpenTrace={(messageId) => {
                  setTraceFocusSpanId(undefined);
                  setCardFlashMemoryId(null);
                  setTraceMessageId(messageId);
                }}
                onOpenTraceWithMemoryShimmer={(messageId) => {
                  setTraceFocusSpanId(undefined);
                  setCardFlashMemoryId(null);
                  setTraceMessageId(messageId);
                  setShimmerMemories(true);
                }}
                onOpenTraceFromDeepLink={(messageId) => {
                  setTraceFocusSpanId(undefined);
                  setShimmerMemories(false);
                  setCardFlashMemoryId(targetMemoryId || null);
                  setTraceMessageId(messageId);
                }}
                onViewTraceSpan={(messageId, spanId) => {
                  setTraceFocusSpanId(spanId);
                  setCardFlashMemoryId(null);
                  setTraceMessageId(messageId);
                }}
              />
            ) : (
              <div className="flex h-full flex-col items-center justify-center gap-2 text-sm text-muted-foreground">
                <MessageSquare className="h-7 w-7 opacity-40" aria-hidden />
                <p>Pick a conversation from the rail to inspect.</p>
              </div>
            )}
          </section>
        </div>
      </main>

      <TraceDrawer
        open={!!traceMessageId}
        datapackId={datapackId}
        conversationId={conversationId ?? ""}
        messageId={traceMessageId}
        onClose={() => {
          setTraceMessageId(null);
          setTraceFocusSpanId(undefined);
        }}
        shimmerMemories={shimmerMemories}
        onShimmerMemoriesDone={() => setShimmerMemories(false)}
        highlightMemoryId={cardFlashMemoryId || undefined}
        focusSpanId={traceFocusSpanId}
      />
    </div>
  );
}
