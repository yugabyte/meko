// Reply shapes of the Meko routes and tools the UI calls (Meko Cloud's, as backend/docs/api.md
// lists them), only the fields the UI reads.

export interface Datapack {
  datapack_id: string;
  datapack_name: string;
  description: string;
  created_at: string;
  updated_at: string;
  memory_count?: number;
  learnings_count?: number;
  collective_memory_count?: number;
  knowledge_chunk_count?: number;
  knowledge_base_file_count?: number;
  conversation_count?: number;
  conversation_search_opt_out?: boolean;
}

export interface Metrics {
  agent_count: number;
  conversation_count: number;
  kb_file_count: number;
  kb_chunk_count: number;
  kb_bytes: number;
  memory_chunk_count: number;
  memory_bytes: number;
  total_requests: number;
  tokens_saved_total: number;
  savings_pct: number;
  last_accessed_at: string | null;
  daily_activity: { date: string; hybrid: number; kb: number; memory: number }[];
}

export interface Memory {
  id: string;
  data: string;
  agent_id: string;
  conversation_id: string;
  created_at: string;
}

export interface MemoryPage {
  memories: Memory[];
  total: number;
  page: number;
  page_size: number;
  total_pages: number;
}

export interface PromotedMemory {
  id: string;
  data: string;
  agent_id: string;
  created_at: string;
  promoted_to_kb_at: string | null;
}

export interface Conversation {
  id: string;
  agent_id: string;
  title: string;
  started_at: string;
  last_activity_at: string;
  first_message: string;
  messages: number;
  memories_added: number;
}

export interface Turn {
  message_id: string;
  started_at: string;
  input: string;
  output: string;
  memory_texts: string[];
}

export interface ConversationDetail extends Conversation {
  thread: Turn[];
  thread_has_more: boolean;
}

export type DocumentStatus = "QUEUED" | "PROCESSING" | "COMPLETED" | "FAILED" | string;

export interface KnowledgeDocument {
  id: string;
  filename: string;
  document_uri: string;
  document_type: string;
  size_bytes: number;
  chunks_processed: number;
  status: DocumentStatus;
  uploaded_at: string;
  completed_at: string | null;
  error?: string;
}

export interface DocumentList {
  items: KnowledgeDocument[];
  total: number;
  has_more: boolean;
}

export interface UploadOffer {
  url: string;
  fields?: Record<string, string>;
  key: string;
  already_uploaded: boolean;
  existing_filename?: string;
}

export interface ProvisionStatus {
  mcp_server_url: string;
  mcp_api_key: string;
}

export interface MemoryHit {
  id: string;
  memory: string;
  score?: number;
  agent_id: string;
  created_at: string;
}

export interface KnowledgeHit {
  id: string;
  chunk_text: string;
  document_id: string;
  distance: number;
  match_type: string;
  rerank_score?: number;
  metadata_filters?: { source?: string };
}

export interface ConversationHit {
  conversation_id: string;
  message_id: string;
  content: string;
  distance: number;
}

// A source is null when it wasn't searched, as while the memory engine is starting.
export interface ContextSearch {
  results: {
    memory: MemoryHit[] | null;
    knowledge_base: KnowledgeHit[] | null;
    conversation: { hits: ConversationHit[]; thread: unknown[] } | null;
  };
  token_savings?: { tokens_saved: number; total_tokens: number };
}

export interface Health {
  status: string;
  version: string;
}
