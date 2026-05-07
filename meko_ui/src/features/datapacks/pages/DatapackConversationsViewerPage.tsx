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

import { useCallback, useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { MessageSquare } from "lucide-react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { ConversationListRail } from "@/features/datapacks/components/conversations-viewer/ConversationListRail";
import { ConversationThreadPane } from "@/features/datapacks/components/conversations-viewer/ConversationThreadPane";
import { TraceDrawer } from "@/features/datapacks/components/conversations-viewer/TraceDrawer";
import {
  ROUTES,
  datapackConversationsPath,
  datapackDetailPath,
} from "@/app/routes/paths";
import type { DatapackConversation } from "@/lib/mekoApi";

type Props = {
  datapackName: string;
  datapackId?: string;
  /** From URL; undefined when on the bare /conversations route. The
   *  rail will auto-select the newest conversation in that case. */
  conversationId?: string;
};

export default function DatapackConversationsViewerPage({
  datapackName,
  datapackId,
  conversationId,
}: Props) {
  const navigate = useNavigate();

  /** Currently open trace drawer message (null = drawer closed).
   *  Owned at the page level so the drawer survives thread re-fetches
   *  and conversation switches reset it cleanly. */
  const [traceMessageId, setTraceMessageId] = useState<string | null>(null);

  // Closing the drawer when the conversation changes prevents stale
  // trace data from flashing while the new thread loads.
  useEffect(() => {
    setTraceMessageId(null);
  }, [conversationId]);

  const handleSelect = useCallback(
    (id: string) => {
      if (id === conversationId) return;
      navigate(datapackConversationsPath(datapackName, id), { replace: false });
    },
    [navigate, datapackName, conversationId],
  );

  /** Auto-pick newest conversation when the user lands on the bare
   *  /conversations route. Replace (not push) so the back button
   *  doesn't bounce them between empty and selected. */
  const handleFirstPageLoaded = useCallback(
    (conversations: DatapackConversation[]) => {
      if (conversationId) return;
      const first = conversations[0];
      if (!first) return;
      navigate(datapackConversationsPath(datapackName, first.id), {
        replace: true,
      });
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
            onFirstPageLoaded={handleFirstPageLoaded}
          />

          <section className="min-h-0 overflow-hidden">
            {conversationId ? (
              <ConversationThreadPane
                datapackName={datapackName}
                datapackId={datapackId}
                conversationId={conversationId}
                onOpenTrace={(messageId) => setTraceMessageId(messageId)}
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
        onClose={() => setTraceMessageId(null)}
      />
    </div>
  );
}
