// RIGHT pane of the lab runner: the live datapack the user is filling —
// knowledge, memories and conversations — with token savings and the X-Ray
// trace collapsed underneath.
//
// One section per context source, in the order Chapter 1 builds them. The
// collective-memory section is parked: promoting a memory is no longer a
// lesson, so it was a permanently empty panel the reader learned to ignore.

import { Database } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import type { DatapackConversation } from "@/lib/api/mekoApi";
import type { DatapackShareGrant } from "@/lib/datapackShareGrants";
import { knowledgeDocDisplayName } from "@/features/datapacks/components/knowledge-documents/knowledgeDocumentName";

import { useNewlyAdded } from "../../hooks/useNewlyAdded";
import type { LabContextFeed } from "../../hooks/useLabContextFeed";
import type { LessonTrace } from "../../lib/lessonTrace";
import { LabContextRow, LabContextSection } from "./LabContextSection";
import { TokenSavingsPane } from "./TokenSavingsPane";
import { XRayPane } from "./XRayPane";

/** The pane is a glance, not a listing. Three is enough to show a
 *  conversation landed without pushing knowledge and memories off screen. */
const CONVERSATION_PREVIEW_LIMIT = 3;

/** What to call a thread. MCP passes a title on conversation_create, but a
 *  session started any other way has none, and the opening prompt says more
 *  about the thread than "Untitled" does. */
function conversationTitle(conversation: DatapackConversation): string {
  return (
    conversation.title?.trim() ||
    conversation.first_message?.trim() ||
    "New conversation"
  );
}

/** Message count, and the memories Meko read out of the thread when there
 *  are any — that second number is the whole point of Chapter 1's last
 *  lesson, so it belongs on the row rather than in a tooltip. */
function conversationMeta(conversation: DatapackConversation): string {
  const messages = `${conversation.messages} message${conversation.messages === 1 ? "" : "s"}`;
  return conversation.memories_added > 0
    ? `${messages} · ${conversation.memories_added} memories extracted`
    : messages;
}

export function LabContextPane({
  feed,
  datapackId,
  datapackName,
  datapackGrant,
  lastLessonTrace,
  showContext,
}: {
  /** Owned by LabRunnerPage: the same fetch decides what this pane shows and
   *  whether a lesson's gate is already satisfied. */
  feed: LabContextFeed;
  datapackId: string | null;
  datapackName: string | null;
  /** Non-null once the datapack is shared — drives the readers badge. */
  datapackGrant: DatapackShareGrant | null;
  lastLessonTrace: LessonTrace | null;
  /** False before the lab has asked for a datapack. A run resumed with one
   *  already chosen would otherwise open on a pane full of knowledge and
   *  memories, on the step whose whole point is that the agent has neither. */
  showContext: boolean;
}) {
  // Only indexed documents reach the pane. A row that appears while the file
  // is still chunking claims the datapack holds something an agent could
  // retrieve, which it does not yet — and it spends the flash on the wrong
  // moment. The middle column already narrates the processing.
  const indexedKnowledge = feed.knowledge.filter(
    (doc) => doc.status?.toUpperCase() === "COMPLETED",
  );
  const newKnowledge = useNewlyAdded(indexedKnowledge.map((doc) => doc.id), feed.hasLoaded);
  const newMemories = useNewlyAdded(
    feed.memories.map((memory) => memory.id ?? ""),
    feed.hasLoaded,
  );
  const conversations = feed.conversations.slice(0, CONVERSATION_PREVIEW_LIMIT);
  const newConversations = useNewlyAdded(
    conversations.map((conversation) => conversation.id),
    feed.hasLoaded,
  );

  return (
    <aside
      aria-label="Live datapack context"
      className="flex min-h-0 flex-1 flex-col overflow-hidden border-l border-border bg-muted/20"
    >
      <div className="flex shrink-0 items-center gap-2 border-b border-border px-4 py-3">
        <Database className="h-3.5 w-3.5 shrink-0 text-primary" aria-hidden />
        <p className="min-w-0 flex-1 truncate text-sm font-medium">
          {showContext ? (datapackName ?? "No datapack yet") : "No datapack yet"}
        </p>
        {showContext && datapackId && (
          <Badge variant="secondary" className="shrink-0 text-[10px]">
            {datapackGrant ? "shared" : "private"}
          </Badge>
        )}
      </div>

      <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
        {!showContext ? (
          <p className="text-xs text-muted-foreground">
            Nothing yet. What you add to a datapack shows here as you build it.
          </p>
        ) : !datapackId ? (
          <p className="text-xs text-muted-foreground">
            Pick a datapack and everything you add appears here, live.
          </p>
        ) : (
          <>
            <LabContextSection
              kind="knowledge"
              label="Knowledge"
              count={indexedKnowledge.length}
              emptyHint="Upload your standards and the chunks land here, searchable."
            >
              {indexedKnowledge.map((doc) => (
                <LabContextRow
                  key={doc.id}
                  kind="knowledge"
                  // document_name is the S3 key (<uuid>/<uuid>_file.md), not a
                  // filename -- same derivation KnowledgeFilesTable uses.
                  title={knowledgeDocDisplayName(doc.document_name)}
                  meta={
                    doc.chunks_processed
                      ? `${doc.chunks_processed} chunks · ${doc.status}`
                      : doc.status
                  }
                  flash={newKnowledge.has(doc.id)}
                />
              ))}
            </LabContextSection>

            <LabContextSection
              kind="memory"
              label="Memories"
              count={feed.memoriesTotal}
              emptyHint="Facts you save, and the ones Meko extracts from what you say."
            >
              {feed.memories.map((memory) => (
                <LabContextRow
                  key={memory.id}
                  kind="memory"
                  title={memory.data}
                  meta="private"
                  flash={newMemories.has(memory.id ?? "")}
                />
              ))}
            </LabContextSection>

            <LabContextSection
              kind="conversation"
              label="Conversations"
              count={feed.conversationsTotal}
              emptyHint="Talk to the agent and the thread lands here, with the memories Meko reads out of it."
            >
              {conversations.map((conversation) => (
                <LabContextRow
                  key={conversation.id}
                  kind="conversation"
                  title={conversationTitle(conversation)}
                  meta={conversationMeta(conversation)}
                  flash={newConversations.has(conversation.id)}
                />
              ))}
            </LabContextSection>
          </>
        )}
      </div>

      {/* Both of these describe a datapack's activity, so they stay out of the
          pane until there is a datapack to have any. Before that they are two
          empty panels the reader has to learn to ignore. */}
      {showContext && datapackId && (
        <>
          <TokenSavingsPane datapackId={datapackId} />
          <XRayPane trace={lastLessonTrace} />
        </>
      )}
    </aside>
  );
}
