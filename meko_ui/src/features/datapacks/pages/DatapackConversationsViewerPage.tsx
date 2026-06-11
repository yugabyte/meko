// Full conversation viewer route: `/datapacks/:name/conversations[/:conversationId]`.
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

/** Drives an extra breadcrumb hop when the viewer is reached from one of
 *  the summary pages, so the user can step back to where they came from.
 *  Keyed by {@link ConversationSource} so a typo or rename is caught by
 *  the compiler. */
const SOURCE_BREADCRUMB: Record<
  ConversationSource,
  (datapackName: string) => { label: string; href: string }
> = {
  [CONVERSATION_SOURCE.memorySummary]: (datapackName) => ({
    label: "Memory Summary",
    href: datapackMemorySummaryPath(datapackName),
  }),
  [CONVERSATION_SOURCE.collectiveMemorySummary]: (datapackName) => ({
    label: "Collective Memory Summary",
    href: datapackCollectiveMemorySummaryPath(datapackName),
  }),
};

function isConversationSource(value: string): value is ConversationSource {
  return (Object.values(CONVERSATION_SOURCE) as string[]).includes(value);
}

type Props = {
  datapackName: string;
  datapackId?: string;
  /** From URL; undefined when on the bare /conversations route. The
   *  thread pane shows a "Pick a conversation" empty state until the
   *  user explicitly picks one from the rail. */
  conversationId?: string;
};

export default function DatapackConversationsViewerPage({
  datapackName,
  datapackId,
  conversationId,
}: Props) {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const targetMemoryId = searchParams.get("memoryId") ?? "";
  const sourceCrumb = useMemo(() => {
    const from = searchParams.get(CONVERSATION_SOURCE_QUERY_PARAM) ?? "";
    if (!isConversationSource(from)) return null;
    return SOURCE_BREADCRUMB[from](datapackName);
  }, [searchParams, datapackName]);

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
      navigate(datapackConversationsPath(datapackName, id), { replace: false });
    },
    [navigate, datapackName, conversationId],
  );

  return (
    <div className="flex h-screen flex-1 flex-col overflow-hidden bg-background text-foreground">
      <AppWorkspaceHeader />

      <main className="flex w-full flex-1 flex-col overflow-hidden">
        <div className="border-b border-border px-6 pt-4 pb-3 shrink-0">
          <DatapackBreadcrumbs
            items={[
              { label: "Datapacks", href: ROUTES.datapacks },
              { label: datapackName, href: datapackDetailPath(datapackName) },
              ...(sourceCrumb ? [sourceCrumb] : []),
              { label: "Conversations" },
            ]}
            className="mb-2"
          />
          <div className="flex items-center gap-2.5">
            <MessageSquare
              className="h-5 w-5 shrink-0 text-purple-300"
              aria-hidden
            />
            <h1 className="truncate text-xl font-semibold tracking-tight text-foreground">
              Conversations
            </h1>
            <span className="truncate text-sm text-muted-foreground">
              · {datapackName}
            </span>
          </div>
        </div>

        <div className="grid flex-1 grid-cols-[340px_1fr] min-h-0 overflow-hidden">
          <ConversationListRail
            datapackName={datapackName}
            datapackId={datapackId}
            selectedId={conversationId ?? ""}
            onSelect={handleSelect}
            onSelectedEmailChange={handleSelectedEmailChange}
          />

          <section className="min-h-0 overflow-hidden">
            {conversationId ? (
              <ConversationThreadPane
                datapackName={datapackName}
                datapackId={datapackId}
                conversationId={conversationId}
                userEmail={selectedEmail}
                onOpenTrace={(messageId) => {
                  setTraceFocusSpanId(undefined);
                  setTraceMessageId(messageId);
                }}
                onOpenTraceWithMemoryShimmer={(messageId) => {
                  setTraceFocusSpanId(undefined);
                  setTraceMessageId(messageId);
                  setShimmerMemories(true);
                }}
                onViewTraceSpan={(messageId, spanId) => {
                  setTraceFocusSpanId(spanId);
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
        datapackName={datapackName}
        datapackId={datapackId}
        conversationId={conversationId ?? ""}
        messageId={traceMessageId}
        onClose={() => {
          setTraceMessageId(null);
          setTraceFocusSpanId(undefined);
        }}
        shimmerMemories={shimmerMemories}
        onShimmerMemoriesDone={() => setShimmerMemories(false)}
        highlightMemoryId={targetMemoryId || undefined}
        focusSpanId={traceFocusSpanId}
      />
    </div>
  );
}
