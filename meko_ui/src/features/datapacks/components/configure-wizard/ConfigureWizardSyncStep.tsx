/**
 * Configure wizard step: three sync tiles (Knowledge, Conversations, Skills) with shared handlers from the page.
 * Parent owns refs and list state so navigation between wizard steps does not reset selections unexpectedly.
 */
import { Book, MessageSquare, Wrench } from "lucide-react";
import type { RefObject } from "react";
import { ConfigureSyncTile } from "./ConfigureSyncTile";
import type { ListedSource, SyncSectionId } from "./types";

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
}: Props) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Add knowledge files, conversation exports, and Markdown skills. Only{" "}
        <span className="font-medium text-foreground">.md</span> files are accepted in Skills.
      </p>
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
          accept={undefined}
          onPickFiles={(fl) => onPickFiles("knowledge", fl)}
          onAddUrl={() => onAddUrl("knowledge")}
          onRemove={(id) => onRemove("knowledge", id)}
        />
        <ConfigureSyncTile
          section="conversations"
          title="Conversations"
          description="Chat exports or logs you want available to the datapack."
          badge={`${conversationItems.length} items`}
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
          badge={`${skillItems.length} items`}
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
