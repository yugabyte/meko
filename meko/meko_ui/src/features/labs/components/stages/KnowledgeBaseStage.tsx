// Knowledge Base lesson: upload a coding-standards file — a bundled sample
// or bring-your-own — offering a file-upload UI with a "Add sample" / "Add
// your own" affordance mirroring WorkbenchLMKnowledgePanel's pattern.

import { useRef, useState } from "react";
import { ChevronDown, ChevronUp, Loader2, Sparkles } from "lucide-react";
import FileUpload from "@/features/datapacks/components/FileUpload";
import { WarningBanner } from "@/components/ui/warning-banner";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";

import {
  buildOrgCodingStandardsFile,
  ORG_CODING_STANDARDS_CONTENT,
  ORG_CODING_STANDARDS_FILENAME,
} from "../../data/adlcReviewAgentLab/orgCodingStandards";
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
  // Open by default -- the point of this card is showing users what the
  // sample actually contains, not making them click to find out.
  const [previewOpen, setPreviewOpen] = useState(true);
  const [hasQueuedDocs, setHasQueuedDocs] = useState(false);
  const [processingActive, setProcessingActive] = useState(false);
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
        title="Load your coding standards"
        lede="The rules your team already wrote down. Meko chunks the document and embeds it once, so an agent can ask for one rule instead of reading all of them."
        tags={["Knowledge"]}
      />
      <div className="flex flex-col gap-3">
        <p className="text-sm text-muted-foreground">
          Give the review agent something to check code against. Load our sample
          coding standards, or upload your own.
        </p>
        {!hasQueuedDocs && (
          <div className="space-y-2 rounded-lg border border-border bg-card p-3">
            <div className="flex items-center gap-2">
              <span className="min-w-0 flex-1 truncate text-sm font-medium text-foreground">
                {ORG_CODING_STANDARDS_FILENAME}
              </span>
            </div>
            <p className="text-[11px] leading-relaxed text-muted-foreground">
              A short excerpt covering SQL, error handling, input validation, and
              security conventions.
            </p>
            <Collapsible open={previewOpen} onOpenChange={setPreviewOpen}>
              <div className="flex flex-wrap items-center gap-2">
                <button
                  type="button"
                  onClick={() => setSampleSeed([buildOrgCodingStandardsFile()])}
                  className="inline-flex items-center gap-1.5 rounded-md border border-emerald-500/30 bg-emerald-500/10 px-2.5 py-1.5 text-xs font-medium text-emerald-700 dark:text-emerald-200 transition-colors hover:bg-emerald-500/20"
                >
                  <Sparkles className="h-3.5 w-3.5" aria-hidden />
                  Add the sample
                </button>
                <CollapsibleTrigger asChild>
                  <button
                    type="button"
                    className="inline-flex items-center gap-1.5 rounded-md border border-border bg-background/60 px-2.5 py-1.5 text-xs font-medium text-foreground/80 transition-colors hover:border-border/80 hover:text-foreground"
                  >
                    {previewOpen ? (
                      <ChevronUp className="h-3.5 w-3.5" aria-hidden />
                    ) : (
                      <ChevronDown className="h-3.5 w-3.5" aria-hidden />
                    )}
                    {previewOpen ? "Hide preview" : "Preview"}
                  </button>
                </CollapsibleTrigger>
              </div>
              <CollapsibleContent>
                <pre className="mt-2 max-h-48 overflow-y-auto whitespace-pre-wrap rounded-md border border-border bg-background/60 p-2.5 text-[11px] leading-relaxed text-foreground/90">
                  {ORG_CODING_STANDARDS_CONTENT}
                </pre>
              </CollapsibleContent>
            </Collapsible>
          </div>
        )}
        {indexed ? (
          <WarningBanner tone="success">
            {feed.knowledgeTotal} document{feed.knowledgeTotal === 1 ? "" : "s"} indexed. It's in
            Knowledge on the right, and searchable now.
          </WarningBanner>
        ) : processingActive ? (
          <div className="flex items-center gap-2.5 rounded-md border border-border bg-card px-4 py-3">
            <Loader2 className="h-4 w-4 shrink-0 animate-spin text-primary" aria-hidden />
            <p className="text-sm font-medium text-foreground">
              Meko is chunking and embedding your file…
            </p>
          </div>
        ) : null}
        <FileUpload
          datapackName={session.datapackName ?? ""}
          datapackId={session.datapackId}
          initialFiles={sampleSeed}
          onQueuedDocsChange={(docs) => setHasQueuedDocs(docs.length > 0)}
          onProcessingActiveChange={handleProcessingActiveChange}
          onKnowledgeBaseCreated={session.bumpRefreshTick}
        />
      </div>
    </div>
  );
}
