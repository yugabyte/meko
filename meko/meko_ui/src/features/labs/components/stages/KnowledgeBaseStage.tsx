// Knowledge Base lesson: upload a coding-standards file — a bundled sample
// or bring-your-own — offering a file-upload UI with a "Add sample" / "Add
// your own" affordance mirroring WorkbenchLMKnowledgePanel's pattern.

import { useRef, useState } from "react";
import {
  ChevronDown,
  ChevronUp,
  FileText,
  Layers,
  Loader2,
  Search,
  Sparkles,
  Upload,
} from "lucide-react";
import FileUpload from "@/features/datapacks/components/FileUpload";
import { knowledgeDocDisplayName } from "@/features/datapacks/components/knowledge-documents/knowledgeDocumentName";
import { Button } from "@/components/ui/button";
import { WarningBanner } from "@/components/ui/warning-banner";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { formatFileSize } from "@/lib/fileSize";

import {
  buildOrgCodingStandardsFile,
  ORG_CODING_STANDARDS_BYTES,
  ORG_CODING_STANDARDS_CONTENT,
  ORG_CODING_STANDARDS_FILENAME,
  ORG_CODING_STANDARDS_FILETYPE,
} from "../../data/adlcReviewAgentLab/orgCodingStandards";
import { BehindTheScenes } from "./BehindTheScenes";
import type { LabContextFeed } from "../../hooks/useLabContextFeed";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { LabStageHeader } from "./LabStageHeader";

export function KnowledgeBaseStage({
  session,
  feed,
  onGoToCreateDatapack,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  /** The datapack's real documents. FileUpload's own "processing" flag can
   *  stay true after the work is finished (and is false on a remount that
   *  missed the transition), so the indexed document itself is the authority
   *  on whether this is still running. */
  feed: LabContextFeed;
  onGoToCreateDatapack: () => void;
}) {
  const [sampleSeed, setSampleSeed] = useState<File[] | undefined>(undefined);
  // Closed by default: the card leads with "Add this sample", and the excerpt
  // is there for whoever wants to check what they are adding.
  const [previewOpen, setPreviewOpen] = useState(false);
  const [hasQueuedDocs, setHasQueuedDocs] = useState(false);
  const [processingActive, setProcessingActive] = useState(false);
  // Whether there is anything left to add. The first two are this session's
  // own doing; the third is what makes the answer survive a remount, since
  // coming back to a finished lesson found local state reset and offered to
  // add a document the datapack already held.
  const sampleAdded =
    Boolean(sampleSeed) ||
    hasQueuedDocs ||
    feed.knowledge.some(
      (doc) => knowledgeDocDisplayName(doc.document_name ?? "") === ORG_CODING_STANDARDS_FILENAME,
    );
  const [hasProcessedOnce, setHasProcessedOnce] = useState(false);
  // Tracks the previous `processingActive` value across renders so the
  // true->false transition can be detected without a stale closure over
  // state captured at callback-creation time.
  const wasProcessingRef = useRef(false);
  // When processing started -- FileUpload doesn't expose per-call (presign
  // / S3 PUT / confirm) timing, so this is one honest aggregate duration
  // for X-Ray rather than a fake per-call breakdown.
  const processingStartedAtRef = useRef<number | null>(null);

  // The document list is polled, so this flips to true on its own once the
  // backend reports COMPLETED -- which is what the old spinner waited for and
  // never saw.
  const indexed = feed.knowledge.some((doc) => doc.status?.toUpperCase() === "COMPLETED");

  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }

  const handleProcessingActiveChange = (active: boolean) => {
    if (active && !wasProcessingRef.current) {
      processingStartedAtRef.current = Date.now();
    } else if (!active && wasProcessingRef.current) {
      setHasProcessedOnce(true);
      const startedAt = processingStartedAtRef.current;
      session.recordLessonTrace({
        lessonTitle: "Build a knowledge base",
        calls: [
          {
            name: "Upload & index file(s)",
            durationMs: startedAt !== null ? Date.now() - startedAt : 0,
            status: "ok",
          },
        ],
      });
    }
    wasProcessingRef.current = active;
    setProcessingActive(active);
  };

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-4">
      <LabStageHeader
        eyebrow="Chapter 1 · Step 3"
        title="Add coding standards as Knowledge"
        lede="Put the rules your team already wrote down into the datapack. Meko chunks and embeds the document once, so every later review retrieves the one rule it needs instead of the whole file."
        tags={["Knowledge"]}
      />
      <div className="flex flex-col gap-4">
        {/* Stays on screen after the upload. It is this lesson's scope tile:
            what is being indexed, and the rules inside it. Unmounting it the
            moment a file was queued took the document away exactly when the
            page started talking about chunking and embedding it. */}
        <div className="overflow-hidden rounded-lg border border-border bg-card">
          <div className="flex items-center gap-2 border-b border-border px-3 py-2">
            <FileText className="h-3.5 w-3.5 shrink-0 text-context-knowledge" aria-hidden />
            <span className="min-w-0 flex-1 truncate text-sm font-medium text-foreground">
              {ORG_CODING_STANDARDS_FILENAME}
            </span>
            <span className="shrink-0 font-mono text-[11px] text-muted-foreground">
              {formatFileSize(ORG_CODING_STANDARDS_BYTES)} · {ORG_CODING_STANDARDS_FILETYPE}
            </span>
          </div>
          <div className="flex flex-col gap-3 p-3">
            <p className="text-[11px] leading-relaxed text-muted-foreground">
              A short excerpt covering SQL, error handling, input validation, and security
              conventions. Swap in your own later.
            </p>
            <Collapsible open={previewOpen} onOpenChange={setPreviewOpen}>
              <div className="flex flex-wrap items-center gap-2">
                <Button
                  size="sm"
                  done={sampleAdded}
                  doneLabel="Added"
                  onClick={() => setSampleSeed([buildOrgCodingStandardsFile()])}
                >
                  <Sparkles aria-hidden />
                  Add this sample
                </Button>
                <CollapsibleTrigger asChild>
                  <Button variant="outline" size="sm">
                    {previewOpen ? (
                      <ChevronUp className="h-3.5 w-3.5" aria-hidden />
                    ) : (
                      <ChevronDown className="h-3.5 w-3.5" aria-hidden />
                    )}
                    {previewOpen ? "Hide" : "Preview"}
                  </Button>
                </CollapsibleTrigger>
              </div>
              <CollapsibleContent>
                <pre className="mt-2 max-h-32 overflow-y-auto whitespace-pre-wrap rounded-md border border-border bg-background/60 p-2.5 text-[11px] leading-relaxed text-foreground/90">
                  {ORG_CODING_STANDARDS_CONTENT}
                </pre>
              </CollapsibleContent>
            </Collapsible>
          </div>
        </div>
        {/* Always mounted and always visible, just small: FileUpload owns the
            presign/PUT/confirm that "Add this sample" triggers, so hiding it
            behind a toggle meant mounting it hidden. `compact` drops its
            header and collapses the drop zone to one line instead. */}
        <FileUpload
          compact
          datapackName={session.datapackName ?? ""}
          datapackId={session.datapackId}
          initialFiles={sampleSeed}
          onQueuedDocsChange={(docs) => setHasQueuedDocs(docs.length > 0)}
          onProcessingActiveChange={handleProcessingActiveChange}
          onKnowledgeBaseCreated={session.bumpRefreshTick}
        />

        {(processingActive || indexed || hasProcessedOnce) && (
          <BehindTheScenes
            className="pt-2"
            concept="knowledge"
            steps={[
              { icon: Upload, label: "Uploaded", detail: "stored once, not per agent" },
              { icon: Layers, label: "Chunked", detail: "split on rule boundaries" },
              { icon: Sparkles, label: "Embedded", detail: "one pass, for everyone" },
              { icon: Search, label: "Retrievable", detail: "ask for a rule, not the file" },
            ]}
          />
        )}

        {indexed ? (
          <WarningBanner tone="success">
            {feed.knowledgeTotal} document{feed.knowledgeTotal === 1 ? "" : "s"} indexed and
            searchable. It just appeared under Knowledge on the right, and no agent had to read the
            whole file to get there.
          </WarningBanner>
        ) : processingActive ? (
          <div className="flex items-center gap-2.5 rounded-md border border-border bg-card px-4 py-3">
            <Loader2 className="h-4 w-4 shrink-0 animate-spin text-primary" aria-hidden />
            <p className="text-sm font-medium text-foreground">
              Meko is chunking and embedding your file…
            </p>
          </div>
        ) : null}
      </div>
    </div>
  );
}
