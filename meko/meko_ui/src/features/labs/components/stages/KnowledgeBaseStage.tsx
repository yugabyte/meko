// Knowledge Base lesson: upload a coding-standards file — a bundled sample
// or bring-your-own — offering a file-upload UI with a "Add sample" / "Add
// your own" affordance mirroring WorkbenchLMKnowledgePanel's pattern.

import { useRef, useState } from "react";
import { CheckCircle2, ChevronDown, ChevronUp, Loader2, Sparkles } from "lucide-react";
import FileUpload from "@/features/datapacks/components/FileUpload";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";

import {
  buildOrgCodingStandardsFile,
  ORG_CODING_STANDARDS_CONTENT,
  ORG_CODING_STANDARDS_FILENAME,
} from "../../data/adlcReviewAgentLab/orgCodingStandards";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { ContextSourceTracker } from "./ContextSourceTracker";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";

export function KnowledgeBaseStage({
  session,
  onGoToCreateDatapack,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
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
      <ContextSourceTracker current="knowledge-base" />
      <div className="flex flex-col gap-3">
        <p className="text-sm text-muted-foreground">
          Give the review agent something to check code against — load our sample
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
        {processingActive ? (
          <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
            Meko is processing your file…
          </p>
        ) : hasProcessedOnce ? (
          <p className="flex items-center gap-1.5 text-xs text-emerald-700 dark:text-emerald-300">
            <CheckCircle2 className="h-3.5 w-3.5" aria-hidden />
            Done — check Shared Knowledge on the right.
          </p>
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
