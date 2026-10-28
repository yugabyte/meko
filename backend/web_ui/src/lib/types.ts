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

export interface DayActivity {
  date: string;
  hybrid: number;
  kb: number;
  memory: number;
}

// GET /datapacks/:id/metrics, the same payload Meko Cloud's Analytics tab reads.
export interface Metrics {
  agent_count: number;
  human_count: number;
  conversation_count: number;
  conversation_chunk_count: number;
  conversation_bytes: number;
  memory_extraction_count: number;
  kb_file_count: number;
  kb_chunk_count: number;
  kb_bytes: number;
  memory_chunk_count: number;
  memory_bytes: number;
  total_requests: number;
  tokens_saved_total: number;
  tokens_if_raw_total: number;
  savings_pct: number;
  score_high_total: number;
  score_mid_total: number;
  score_low_total: number;
  last_accessed_at: string | null;
  daily_activity: DayActivity[];
}

// One Meko call from the call log, as /usage's recent calls and a turn's decision trace give it.
export interface MekoCall {
  // Absent from an older server's reply.
  id?: number;
  // ids: what a memory search returned (absent from calls recorded before it was).
  steps?: { step: string; ms: number; n: number; ids?: string[] }[];
  at: string;
  tool: string;
  agent_id: string;
  ok: boolean;
  error_code: string;
  ms: number;
  memory_n: number;
  kb_n: number;
  conversation_n: number;
  // Whether the tool only reads, as its definition says; in a turn's trace only.
  read_only?: boolean;
}

// GET /datapacks/:id/usage: Meko local's own view of the call log.
export interface Usage {
  calls: number;
  errors: number;
  tools: { tool: string; calls: number; errors: number; p50_ms: number; p95_ms: number }[];
  steps: { step: string; calls: number; p50_ms: number; p95_ms: number }[];
  agents: { agent_id: string; calls: number; searches: number; last_at: string }[];
  recent: MekoCall[];
  input_tokens: number;
  output_tokens: number;
}

export interface Memory {
  id: string;
  data: string;
  agent_id: string;
  conversation_id: string;
  created_at: string;
  // "extract" for a memory the memory extractor saved, with the turn it came from.
  source?: string;
  message_id?: string;
}

// The memory extractor (GET /settings/memory-extractor): whether turns are extracted, and how.
export interface ExtractorStatus {
  enabled: boolean;
  harness: string;
  model: string;
  interval_seconds: number;
  queued: number;
  last_run_at?: string;
  last_error?: string;
  extracted: number;
}

// A datapack's say in memory extraction (GET /datapacks/:id/memory-extraction).
export interface DatapackExtraction {
  opt_out: boolean;
  profile: string;
  instructions: string;
}

// One change to a memory, as mem0 recorded it (GET /memories/:id/history).
export interface MemoryChange {
  action: "ADD" | "UPDATE" | "DELETE" | string;
  previous_value: string | null;
  new_value: string | null;
  created_at: string;
  updated_at: string | null; // when an update or delete happened
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
  conversation_id?: string;
  created_at: string;
  promoted_to_kb_at: string | null;
}

// The datapack's memories laid out by meaning (GET /datapacks/:id/memory-map): x and y in [-1, 1],
// alike memories close together, links between near neighbors, and where ?q= lands.
export type MemoryState = "learning" | "kept" | "collective";

export interface MapPoint {
  id: string;
  text: string;
  state: MemoryState;
  agent_id: string;
  conversation_id?: string;
  message_id?: string;
  created_at: string;
  x: number;
  y: number;
  similarity?: number; // to the query, when there is one
}

export interface MemoryMap {
  points: MapPoint[];
  links: { a: number; b: number; similarity: number }[];
  query?: [number, number];
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
  // What the agent said about the turn: why it answered so, and the steps it took.
  reasoning?: string;
  plan?: string[];
  // The Meko calls it made since the turn before; absent from an older server.
  calls?: MekoCall[];
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

export interface Artifact {
  artifact_id: string;
  content_hash: string;
  filename: string;
  content_type: string;
  size_bytes: number;
  created_at: string;
  last_accessed_at?: string;
  content_base64?: string;
}

export interface ArtifactList {
  items: Artifact[];
  total: number;
  has_more: boolean;
}

// GET .../knowledge-bases/activity-log: files uploaded and memories promoted, newest first.
export interface KnowledgeActivity {
  type: "file_upload" | "memory_promotion" | string;
  timestamp: string;
  document_name?: string; // a file's key, ending in its name
  chunks_processed?: number;
  size_bytes?: number;
  memory_id?: string;
  content?: string; // a promoted memory's text
  agent_id?: string;
}

export interface ProvisionStatus {
  mcp_server_url: string;
  mcp_api_key: string;
}

export interface MemoryHit {
  id: string;
  memory: string;
  score?: number;
  // Cosine similarity, the number Cloud bands and floors memories by.
  raw_similarity_score?: number;
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

// GET /api/v1/server: the server and its memory engine.
export interface ServerStatus {
  version: string;
  pid: number;
  port: number;
  data_dir: string;
  started_at: string;
  uptime_seconds: number;
  auth: string;
  memory_engine: {
    state: string;
    reason?: string;
    embedder?: { provider: string; model: string; dims: number };
    reranker?: { provider: string; model: string; state: string; error?: string };
  };
  children: { name: string; state: string; restarts: number; last_error?: string; since: string }[];
}

// GET /api/v1/update (and the replies of POST /update/check and /update/apply): Meko's own updates.
export interface UpdateStatus {
  running: string;
  // A release install, which updates itself; a build from source doesn't.
  release: boolean;
  // Why a release install's updates can't run, when they can't.
  off?: string;
  // The login service runs this server, and starts a new version again if it fails.
  service: boolean;
  channel?: string;
  // Installs updates on its own (under the service, with [update] auto).
  auto: boolean;
  checked_at?: string;
  latest?: string;
  latest_url?: string;
  available: boolean;
  // Why the last check failed.
  error?: string;
  // An update on probation: it's done once the new version has run 5 minutes.
  pending?: { from: string; to: string; attempts: number };
  rolled_back: string[];
  job?: UpdateJob;
}

// The last install started from this page since the server started. to is "" until a release is
// found, and stays "" when there was nothing newer.
export interface UpdateJob {
  state: "running" | "done" | "failed";
  step?: "checking" | "installing" | "restarting";
  to?: string;
  error?: string;
  started_at: string;
  finished_at?: string;
}

export interface Health {
  status: string;
  version: string;
}

// GET and PATCH /settings/usage-stats: whether usage stats go to YugabyteDB.
export interface UsageStats {
  enabled: boolean;
  // Turned off by config.toml or the environment: the web UI can't turn them on.
  forced_off: boolean;
  // What turned them off: "web UI", "config.toml", "MEKO_CALL_HOME" or "YB_DISABLE_CALLHOME".
  by?: string;
  // On, but a build from source has nowhere to send them.
  sends: boolean;
  // Where they go while sends.
  url?: string;
}

// GET /auth/status, which needs no credential.
export interface AuthStatus {
  // How [web] access in config.toml opens the web listener: "local" (this computer, no login) or
  // "remote" (any address, HTTPS, a password login).
  access: "local" | "remote";
  // This request came in as a local one, which needs no login.
  local_access: boolean;
  signed_in: boolean;
  // Remote access needs a login made from a setup link (meko setup_link) first.
  setup_required: boolean;
  // setup_required while a login exists: the setup link replaces it.
  reset: boolean;
  // The login's username, or null when there's none.
  username: string | null;
  totp_enabled: boolean;
}

// POST /auth/totp/enroll: the authenticator's secret, as text and as the otpauth:// URL its QR
// code holds.
export interface TotpEnroll {
  secret: string;
  otpauth_url: string;
}

// GET /extensions: every extension Meko knows, whether it's installed (registered with Meko), its
// web UI at path ("" while it isn't served) and whether that answers (available: running), and its
// last action from the Extensions page. An older server lists only the served ones, with no
// installed, web_ui or description.
export interface Extension {
  name: string;
  title: string;
  description?: string;
  installed?: boolean;
  web_ui?: boolean;
  path: string;
  available: boolean;
  // The release Meko installed and can update; none for a build from source.
  version?: string;
  updatable?: boolean;
  // The newest release the last check found, and whether it's newer than version (and wasn't rolled back).
  latest?: string;
  latest_url?: string;
  update_available?: boolean;
  checked_at?: string;
  check_error?: string;
  // An update on probation: Meko counts its starts and rolls it back after three failed ones.
  pending?: { from: string; to: string; attempts: number; at: string };
  rolled_back?: string[];
  // Update automatically: Meko installs the updates its checks find (on unless turned off).
  auto_update?: boolean;
  // Why an automatic update waits (it's in use, or its last update is still on probation).
  waiting?: string;
  job?: ExtensionJob;
}

export type ExtensionAction = "install" | "update" | "start" | "stop";

// An action running, or the last one run, on an extension; output is its last 16 KB. auto marks an
// update Meko started on its own.
export interface ExtensionJob {
  action: ExtensionAction;
  state: "running" | "done" | "failed";
  started_at: string;
  finished_at?: string;
  error?: string;
  output: string;
  auto?: boolean;
}

// GET|PUT|DELETE /settings/meko-cloud: the Meko Cloud link the extensions use (never its key).
export interface MekoCloud {
  // Set once, linked or unlinked.
  set: boolean;
  // A key is saved, and the extensions use this link.
  linked: boolean;
  // The MCP server as set; "" is Meko Cloud's.
  url: string;
  web_url: string;
  api_url: string;
  effective_url: string;
  // Null when it can't be worked out (another server with no web app URL set).
  effective_web_url: string | null;
  effective_api_url: string | null;
  // The key's last four characters, "" for a short key, null with none.
  key_hint: string | null;
  default_url: string;
  default_web_url: string;
}

// GET /datapacks?location=cloud: the linked Meko Cloud's datapacks, newest first.
export interface CloudDatapack {
  datapack_id: string;
  datapack_name: string;
  description: string;
  // owner for the account's own; any other (maintainer, contributor, viewer) was shared with it.
  grant: string;
  created_at: string;
  updated_at: string;
  // Opens it in the link's web app; null when that can't be known.
  web_url: string | null;
  location: "cloud";
}

// The sidebar's Cloud rows: none while unlinked.
export interface CloudDatapacks {
  linked: boolean;
  datapacks: CloudDatapack[];
}

// PUT /settings/meko-cloud: a field left out is unchanged; an empty api_key keeps the saved one.
export interface MekoCloudUpdate {
  url?: string;
  web_url?: string;
  api_url?: string;
  api_key?: string;
}
