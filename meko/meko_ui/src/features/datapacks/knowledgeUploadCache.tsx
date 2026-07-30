import { createContext, useContext, useRef, type ReactNode } from "react";
import type { PipelineDetail, SourceStatus } from "@/lib/api/mekoApi";
import type { KnowledgeUploadStatus } from "@/lib/knowledgeUploadStatus";

/** A single staged/uploading file tracked by the KB upload UI. */
export type FileEntry = {
  /** Stable id, independent of array index/order. */
  id: string;
  file: File;
  status: KnowledgeUploadStatus;
  progress: number;
  error?: string;
  /** Set when skipped due to duplicate content. */
  note?: string;
};

/** Last-known KB status snapshot used to hydrate the status card instantly. */
export type StatusCacheEntry = {
  sourceStatuses: SourceStatus[];
  pipelineDetails: PipelineDetail[];
  kbCreated: boolean;
};

/**
 * Radix unmounts Dialog.Content on close and the datapack pages toggle via
 * `display:none` rather than unmounting, so FileUpload needs its file/status
 * state to survive close/reopen. Previously this lived in module-level Maps,
 * which meant a logged-out user's files (up to 5MB each) leaked into memory
 * and into the next user's session on the same SPA tab.
 *
 * Holding the Maps in a provider mounted inside the authenticated tree ties
 * their lifetime to the session: the provider unmounts on logout, so the Maps
 * (and every retained File blob) are garbage-collected instead of persisting.
 */
type KnowledgeUploadCache = {
  fileUploadStateCache: Map<string, FileEntry[]>;
  statusCache: Map<string, StatusCacheEntry>;
};

const KnowledgeUploadCacheContext = createContext<KnowledgeUploadCache | null>(null);

export function KnowledgeUploadCacheProvider({ children }: { children: ReactNode }) {
  // Refs so the Map identities are stable for the provider's lifetime; a fresh
  // pair is created each mount (i.e. per authenticated session).
  const fileUploadStateCache = useRef<Map<string, FileEntry[]>>(new Map());
  const statusCache = useRef<Map<string, StatusCacheEntry>>(new Map());
  const value = useRef<KnowledgeUploadCache>({
    fileUploadStateCache: fileUploadStateCache.current,
    statusCache: statusCache.current,
  });
  return (
    <KnowledgeUploadCacheContext.Provider value={value.current}>
      {children}
    </KnowledgeUploadCacheContext.Provider>
  );
}

export function useKnowledgeUploadCache(): KnowledgeUploadCache {
  const ctx = useContext(KnowledgeUploadCacheContext);
  if (!ctx) {
    throw new Error("useKnowledgeUploadCache must be used within KnowledgeUploadCacheProvider");
  }
  return ctx;
}
