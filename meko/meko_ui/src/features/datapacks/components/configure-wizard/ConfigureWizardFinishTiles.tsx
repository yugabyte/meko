/**
 * Finish-step summary: Knowledge upload status + same Conversations / Skills tiles as the sync step (coming soon).
 */
import { useRef } from "react";
import { Book, Check, MessageSquare, Wrench } from "lucide-react";
import { ConfigureSyncTile } from "@/features/datapacks/components/configure-wizard/ConfigureSyncTile";
import {
  wizardTileRootClass,
  wizardTileToneBadge,
  wizardTileToneIcon,
} from "@/features/datapacks/components/configure-wizard/configureWizardTileStyles";
import { KNOWLEDGE_UPLOAD_STATUS } from "@/lib/knowledgeUploadStatus";
import { cn } from "@/lib/utils";
import type { ListedSource } from "@/features/datapacks/components/configure-wizard/types";

function FinishKnowledgeSummaryTile({ uploadedDoneCount }: { uploadedDoneCount: number }) {
  const tone = "emerald" as const;
  return (
    <div className={wizardTileRootClass(tone)}>
      <div className="flex items-start justify-between gap-2 border-b border-border/60 pb-3">
        <div className="flex min-w-0 items-center gap-2">
          <Book className={cn("h-5 w-5 shrink-0", wizardTileToneIcon[tone])} aria-hidden />
          <h3 className="truncate text-sm font-semibold text-foreground">Knowledge</h3>
        </div>
        <span
          className={cn(
            "shrink-0 rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide",
            wizardTileToneBadge[tone],
          )}
        >
          {uploadedDoneCount > 0 ? `${uploadedDoneCount} uploaded` : "0 uploaded"}
        </span>
      </div>
      <p className="mt-3 text-xs leading-relaxed text-muted-foreground">
        Files we&apos;ll index and chunk for retrieval.
      </p>
      <div className="mt-4 flex min-h-[160px] flex-1 flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-border/60 bg-muted/5 px-3 py-6">
        {uploadedDoneCount > 0 ? (
          <>
            <Check className="h-10 w-10 shrink-0 text-emerald-400" strokeWidth={2.5} aria-hidden />
            <p className="text-center text-sm font-medium text-foreground">
              {uploadedDoneCount} knowledge file{uploadedDoneCount === 1 ? "" : "s"} uploaded
            </p>
          </>
        ) : (
          <p className="text-center text-sm leading-relaxed text-muted-foreground">
            Please add knowledge files in the previous step.
          </p>
        )}
      </div>
    </div>
  );
}

type Props = {
  knowledgeItems: ListedSource[];
};

export function ConfigureWizardFinishTiles({ knowledgeItems }: Props) {
  const uploadedDoneCount = knowledgeItems.filter(
    (it) => it.kind === "file" && it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.DONE,
  ).length;
  const conversationInputRef = useRef<HTMLInputElement>(null);
  const skillsInputRef = useRef<HTMLInputElement>(null);

  return (
    <div className="grid gap-4 lg:grid-cols-3 lg:items-stretch">
      <FinishKnowledgeSummaryTile uploadedDoneCount={uploadedDoneCount} />
      <ConfigureSyncTile
        section="conversations"
        title="Conversations"
        description="Past chats with the agent. Older ones will auto-tier to S3."
        badge=""
        icon={MessageSquare}
        tone="blue"
        items={[]}
        fileInputRef={conversationInputRef}
        accept={undefined}
        onPickFiles={() => {}}
        onAddUrl={() => {}}
        onRemove={() => {}}
      />
      <ConfigureSyncTile
        section="skills"
        title="Skills"
        description="Standards and runbooks that define how the agent works."
        badge=""
        icon={Wrench}
        tone="purple"
        items={[]}
        fileInputRef={skillsInputRef}
        accept=".md,.markdown,text/markdown"
        onPickFiles={() => {}}
        onAddUrl={() => {}}
        onRemove={() => {}}
      />
    </div>
  );
}
