/**
 * Configure wizard step: Knowledge (staged uploads), Conversations and Skills as coming-soon tiles.
 * Parent owns refs and list state so navigation between wizard steps does not reset selections unexpectedly.
 */
import { AlertCircle, Book, MessageSquare, Wrench } from "lucide-react";
import type { RefObject } from "react";
import { ConfigureSyncTile } from "./ConfigureSyncTile";
import type { ListedSource, SyncSectionId } from "./types";
import {
  KNOWLEDGE_UPLOAD_EXTENSIONS_HINT,
  KNOWLEDGE_UPLOAD_MAX_BATCH,
} from "@/lib/knowledgeFileUploadRules";
import { useKnowledgeUploadMaxFileSizeMB } from "@/hooks/useKnowledgeUploadMaxFileSizeMB";
type Props = {
  knowledgeItems: ListedSource[];
  conversationItems: ListedSource[];
  skillItems: ListedSource[];
  knowledgeInputRef: RefObject<HTMLInputElement | null>;
  conversationInputRef: RefObject<HTMLInputElement | null>;
  skillsInputRef: RefObject<HTMLInputElement | null>;
  onPickFiles: (section: SyncSectionId, files: FileList | null) => void;
  onAddUrl: (section: SyncSectionId) => void;
  onRemove: (section: SyncSectionId, id: string) => void;
  knowledgeUploading: boolean;
  onUploadKnowledgeFiles: (includedIds: Set<string>) => void;
  onRetryKnowledgeUpload: (id: string) => void;
  knowledgeFileAccept: string;
  syncFileWarning: string | null;
};

export function ConfigureWizardSyncStep({
  knowledgeItems,
  conversationItems,
  skillItems,
  knowledgeInputRef,
  conversationInputRef,
  skillsInputRef,
  onPickFiles,
  onAddUrl,
  onRemove,
  knowledgeUploading,
  onUploadKnowledgeFiles,
  onRetryKnowledgeUpload,
  knowledgeFileAccept,
  syncFileWarning,
}: Props) {
  // Tier-aware per-file cap (5 MB free / 50 MB standard / ∞ pro).
  const maxFileSizeMB = useKnowledgeUploadMaxFileSizeMB();
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Stage knowledge files for this datapack (PDF, TXT, MD, JSON, CSV, HTML
        {Number.isFinite(maxFileSizeMB) ? ` — up to ${maxFileSizeMB}MB each,` : " —"} up to{" "}
        {KNOWLEDGE_UPLOAD_MAX_BATCH} per batch). Conversations and skills use the same layout below and
        will be available in a future release.
      </p>
      {syncFileWarning ? (
        <div className="flex items-center gap-2 rounded-lg border border-yellow-500/30 bg-yellow-500/10 px-4 py-2 text-xs text-yellow-200">
          <AlertCircle className="h-4 w-4 shrink-0" aria-hidden />
          {syncFileWarning}
        </div>
      ) : null}
      <div className="grid gap-4 lg:grid-cols-3 lg:items-stretch">
        <ConfigureSyncTile
          section="knowledge"
          title="Knowledge"
          description=""
          badge=""
          icon={Book}
          tone="emerald"
          items={knowledgeItems}
          fileInputRef={knowledgeInputRef}
          accept={knowledgeFileAccept}
          knowledgeExtensionsHint={KNOWLEDGE_UPLOAD_EXTENSIONS_HINT}
          onPickFiles={(fl) => onPickFiles("knowledge", fl)}
          onAddUrl={() => onAddUrl("knowledge")}
          onRemove={(id) => onRemove("knowledge", id)}
          knowledgeUploading={knowledgeUploading}
          onUploadKnowledgeFiles={onUploadKnowledgeFiles}
          onRetryKnowledgeUpload={onRetryKnowledgeUpload}
        />
        <ConfigureSyncTile
          section="conversations"
          title="Conversations"
          description="Past chats with the agent. Older ones will auto-tier to S3."
          badge=""
          icon={MessageSquare}
          tone="blue"
          items={conversationItems}
          fileInputRef={conversationInputRef}
          accept={undefined}
          onPickFiles={(fl) => onPickFiles("conversations", fl)}
          onAddUrl={() => onAddUrl("conversations")}
          onRemove={(id) => onRemove("conversations", id)}
        />
        <ConfigureSyncTile
          section="skills"
          title="Skills"
          description="Standards and runbooks that define how the agent works."
          badge=""
          icon={Wrench}
          tone="purple"
          items={skillItems}
          fileInputRef={skillsInputRef}
          accept=".md,.markdown,text/markdown"
          onPickFiles={(fl) => onPickFiles("skills", fl)}
          onAddUrl={() => onAddUrl("skills")}
          onRemove={(id) => onRemove("skills", id)}
        />
      </div>
    </div>
  );
}
