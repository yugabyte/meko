/**
 * Configure wizard step: Knowledge (staged uploads), Conversations and Skills as coming-soon tiles.
 * Parent owns refs and list state so navigation between wizard steps does not reset selections unexpectedly.
 */
import { AlertCircle, Book, MessageSquare, Wrench } from "lucide-react";
import type { RefObject } from "react";
import { ConfigureSyncTile } from "./ConfigureSyncTile";
import type { ListedSource, SyncSectionId } from "./types";
import {
  KNOWLEDGE_UPLOAD_MAX_BATCH,
  KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB,
} from "@/lib/knowledgeFileUploadRules";

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
  onUploadKnowledgeFiles: () => void;
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
  const knowledgePendingUpload = knowledgeItems.some(
    (it) =>
      it.kind === "file" &&
      it.file &&
      (it.uploadStatus === "pending" || it.uploadStatus === "error" || !it.uploadStatus),
  );

  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Add knowledge files for retrieval in this datapack (same types and size limits as the datapack file uploader:
        PDF, TXT, MD, JSON, MP4 — up to {KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB}MB each, up to {KNOWLEDGE_UPLOAD_MAX_BATCH}{" "}
        files per batch). Conversation exports and Markdown skills
        will be available here in a future release.
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
          description="Files we can index and chunk for retrieval."
          badge={`${knowledgeItems.length} items`}
          icon={Book}
          tone="emerald"
          items={knowledgeItems}
          fileInputRef={knowledgeInputRef}
          accept={knowledgeFileAccept}
          onPickFiles={(fl) => onPickFiles("knowledge", fl)}
          onAddUrl={() => onAddUrl("knowledge")}
          onRemove={(id) => onRemove("knowledge", id)}
          showKnowledgeUpload={knowledgePendingUpload}
          knowledgeUploading={knowledgeUploading}
          onUploadKnowledgeFiles={onUploadKnowledgeFiles}
          onRetryKnowledgeUpload={onRetryKnowledgeUpload}
        />
        <ConfigureSyncTile
          section="conversations"
          title="Conversations"
          description="Chat exports or logs you want available to the datapack."
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
          description="Markdown runbooks and conventions (.md only)."
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
