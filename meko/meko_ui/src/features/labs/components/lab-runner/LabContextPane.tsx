// RIGHT pane of the lab runner: the live datapack the user is filling —
// knowledge, memories and collective memory — with token savings and the
// X-Ray trace collapsed underneath.

import { Database } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import type { DatapackShareGrant } from "@/lib/datapackShareGrants";
import { documentNameLastSegment } from "@/features/datapacks/components/knowledge-documents/knowledgeDocumentName";

import type { LabContextFeed } from "../../hooks/useLabContextFeed";
import type { LessonTrace } from "../../lib/lessonTrace";
import { LabContextRow, LabContextSection } from "./LabContextSection";
import { TokenSavingsPane } from "./TokenSavingsPane";
import { XRayPane } from "./XRayPane";

/** Last path segment of the S3 key, minus the numeric upload prefix. */
function knowledgeDocDisplayName(documentName: string): string {
  return documentNameLastSegment(documentName).replace(/^\d+_/, "");
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
              count={feed.knowledgeTotal}
              emptyHint="Upload your standards and the chunks land here, searchable."
            >
              {feed.knowledge.map((doc) => (
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
                <LabContextRow key={memory.id} kind="memory" title={memory.data} meta="private" />
              ))}
            </LabContextSection>

            <LabContextSection
              kind="memory"
              label="Collective memory"
              count={feed.collectiveTotal}
              emptyHint="Promote a memory and every agent on this datapack sees it."
            >
              {feed.collective.map((memory) => (
                <LabContextRow key={memory.id} kind="memory" title={memory.data} meta="promoted" />
              ))}
            </LabContextSection>
          </>
        )}
      </div>

      <TokenSavingsPane datapackId={datapackId} />
      <XRayPane trace={lastLessonTrace} />
    </aside>
  );
}
