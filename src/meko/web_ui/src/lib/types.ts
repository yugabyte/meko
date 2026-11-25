// Reply shapes of the Meko routes and tools the UI calls (Meko Cloud's, as src/meko/docs/api.md
// lists them), only the fields the UI reads.

export interface Datapack {
  datapack_id: string;
  datapack_name: string;
  description: string;
  created_at: string;
  updated_at: string;
  // GET /datapacks only. owner for the user's own; any other (maintainer, contributor, viewer) was
  // shared with them in Meko Cloud.
  grant?: string;
  // GET /datapacks only: where the row opens. This UI's path (/datapacks/<id>) for a datapack Meko
  // serves here, Meko Cloud's web app for one of its own, null when that web app isn't known.
  // Absent from a server older than the UI, whose rows are all its own.
  web_url?: string | null;
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

// One source's share of context size: count is knowledge-base chunks, memories or conversation turns.
export interface SizePart {
  count: number;
  tokens: number;
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
  artifact_count?: number; // absent from an older server
  // Context size in tokens, as the server counts it (memory.SizeOf).
  context_size: { tokens: number; knowledge: SizePart; memories: SizePart; conversations: SizePart };
  total_requests: number;
  // context_search calls that sent something: what context efficiency averages over (absent from an
  // older server, which averaged over total_requests).
  savings_requests?: number;
  tokens_sent_total?: number; // what those searches sent (absent from an older server)
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
  // On the Calls page's calls only: its ids, "kode" for Kode's own calls, and whether what it sent
  // and got back is kept.
  datapack_id?: string;
  conversation_id?: string;
  client?: string;
  has_contents?: boolean;
}

// GET /calls/:id: a call, with what it sent (JSON) and got back while Settings keeps call contents.
export interface CallDetail extends MekoCall {
  args?: string;
  reply?: string;
  // Either was cut to fit.
  cut?: boolean;
}

// GET /calls: a page of the call log, newest first; more says older ones match too.
export interface CallPage {
  calls: MekoCall[];
  more: boolean;
}

// The Calls page's filters, as its address's query has them.
export interface CallFilter {
  agent?: string;
  tool?: string;
  client?: string;
  failed?: boolean;
}

// GET /settings/call-contents: whether calls' contents are kept, for how long, and what's kept now.
// GET /settings/cloud-sync: whether datapacks can be synced to Meko Cloud; off until it's turned on.
export interface CloudSyncSetting {
  enabled: boolean;
}

export interface CallContents {
  keep: boolean;
  retention_days: number;
  calls: number;
  bytes: number;
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

// What a search map's point is: a memory, a knowledge-base chunk or a conversation turn.
export type MapSource = "memory" | "knowledge_base" | "conversation";

// A memory map's point carries its state; a search map's its source (and no agent or date).
export interface MapPoint {
  id: string;
  text: string;
  state?: MemoryState;
  source?: MapSource;
  agent_id?: string;
  conversation_id?: string;
  message_id?: string;
  created_at?: string;
  x: number;
  y: number;
  z?: number; // the 3D view's depth
  similarity?: number; // to the query, when there is one
}

export interface MemoryMap {
  points: MapPoint[];
  links: { a: number; b: number; similarity: number }[];
  query?: [number, number];
  query_z?: number;
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
  // A project's events': the datapack (none on a folder made) and the folder.
  datapack_id?: string;
  datapack_name?: string;
  folder_id?: string;
  folder_name?: string;
}

// Projects, as Meko Cloud's: a project files datapacks in folders, one level deep.
export interface Project {
  project_id: string;
  project_name: string;
  theme_key: string;
  description: string;
  folder_names: string[];
  folder_count: number;
  datapack_count: number;
  created_at: string;
  updated_at: string;
}

export interface ProjectFolder {
  folder_id: string;
  project_id: string;
  folder_name: string;
  folder_description: string;
  datapack_count: number;
  created_at: string;
}

export interface FiledDatapack {
  datapack_id: string;
  datapack_name: string;
  folder_id: string;
  updated_at: string;
  placed_at: string;
  memory_count: number;
  awaiting_review_count: number;
  promoted_count: number;
  knowledge_count: number;
}

export interface ProjectDetail {
  project: Project;
  folders: ProjectFolder[];
  datapacks: FiledDatapack[];
}

export interface ProjectTheme {
  key: string;
  label: string;
  description: string;
  folders: string[];
}

export interface Placement {
  datapack_id: string;
  project_id: string;
  project_name: string;
  folder_id: string;
  folder_name: string;
  path: string;
}

export interface ProjectStats {
  kb_files_by_type: Record<string, number>;
  kb_files: number;
  kb_chunks: number;
  searches_hybrid: number;
  searches_kb: number;
  searches_memory: number;
  conversations: number;
  artifacts: number;
  artifact_bytes: number;
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

// GET /settings/answers: whether Search writes an answer from its results, and through what.
export interface AnswerStatus {
  enabled: boolean;
  harness: string;
  model: string;
}

// One search result an answer is written from.
export interface Passage {
  source: MapSource;
  id: string;
  text: string;
}

// An answer, with the passages it cites by number, from 1.
export interface SearchAnswer {
  answer: string;
  cited: number[];
  model: string;
}

// A source is null when it wasn't searched, as while the context engine is starting.
export interface ContextSearch {
  results: {
    memory: MemoryHit[] | null;
    knowledge_base: KnowledgeHit[] | null;
    conversation: { hits: ConversationHit[]; thread: unknown[] } | null;
  };
  token_savings?: { tokens_saved: number; total_tokens: number };
}

// GET /api/v1/server: the server and its context engine.
export interface ServerStatus {
  version: string;
  pid: number;
  port: number;
  data_dir: string;
  started_at: string;
  uptime_seconds: number;
  auth: string;
  // Missing from a server of another version than this page's (an older one called it
  // memory_engine), as while a tab outlives an update or a rollback.
  context_engine?: {
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

// GET /auth/status, which needs no credential. There's no login: Meko still sends setup_required,
// reset, username and totp_enabled, always empty, for older pages.
export interface AuthStatus {
  // How [web] access in config.toml opens the web listener: "local" (this computer only) or
  // "remote" (every address, over HTTPS). Neither has a login.
  access: "local" | "remote";
  // This request came in as a local one.
  local_access: boolean;
  // Whether Meko lets this request in.
  signed_in: boolean;
}

// GET /extensions: every extension Meko knows, whether it's installed (registered with Meko), its
// web UI at path ("" while it isn't served) and whether that answers (available: running), and its
// last action from the Extensions page. An older server lists only the served ones, with no
// installed, web_ui or description.
export interface Extension {
  name: string;
  // On a row under an old name: the extension's name now. Such rows are left out.
  renamed_to?: string;
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
  // PUT and DELETE only: the agents a link change moved to the new target, left where they point,
  // or failed to move. Absent from an older server.
  agents?: AgentMoves;
}

export interface AgentMoves {
  moved: { name: string; title: string }[];
  left: { name: string; title: string; url: string }[];
  failed: { name: string; title: string; error: string }[];
}

// An installed agent and whether its meko MCP server reaches the target: connected; stale (one of
// Meko's own, but not exactly the target: a token or a move that's missing); elsewhere (another
// server); none.
export type AgentState = "connected" | "stale" | "elsewhere" | "none";

export interface Agent {
  name: string;
  title: string;
  command: string;
  state: AgentState;
  // Its meko server's URL, null with none.
  url: string | null;
  notes: string[];
}

// GET /agents: where agents connect (this Meko, or the linked Meko Cloud) and the installed agents.
export interface Agents {
  target: { kind: "local" | "cloud"; url: string };
  agents: Agent[];
}

// POST /agents/:name/connect|disconnect: the agent's row after, and what the user should know.
export interface AgentChange {
  agent: Agent;
  notes: string[];
}

// PUT /settings/meko-cloud: a field left out is unchanged; an empty api_key keeps the saved one.
export interface MekoCloudUpdate {
  url?: string;
  web_url?: string;
  api_url?: string;
  api_key?: string;
}

// POST /settings/meko-cloud/sign-in: where the browser goes to sign in to Meko Cloud.
export interface MekoCloudSignIn {
  authorize_url: string;
}

// Context Gaps: the datapack's searches Meko answered poorly, grouped by question.
// empty: the search found nothing. weak: it found only memories, each scoring below 0.75.
export type GapQuality = "empty" | "weak";
export type GapState = "open" | "dismissed" | "taught";

// One question: the query as last asked, how its latest ask went, and what was done about it. A null
// conversation_id on taught or dismissal means a person did it; a set one, an agent in that
// conversation.
export interface Gap {
  key: string;
  query: string;
  quality: GapQuality;
  tool: string;
  results: number | null;
  top_score: number | null;
  top_raw: number | null;
  asks: number;
  agents: string[];
  conversations: string[];
  asked_by_conversation: boolean;
  first_at: string;
  last_at: string;
  state: GapState;
  marked_at: string | null;
  taught: { text: string; memory_id: string | null; at: string; conversation_id: string | null } | null;
  dismissal: { conversation_id: string | null; memory_id: string | null; memory_text: string | null } | null;
}

// GET /datapacks/:id/questions: a page of questions, latest asked first, with the counts by state, the
// open ones by quality, and the searches recorded since when.
export interface GapPage {
  questions: Gap[];
  more: boolean;
  counts: Record<GapState, number>;
  open: Record<GapQuality, number>;
  searches: number;
  since: string | null;
}

// Moving one of this Meko's datapacks to the linked Meko Cloud: each of the plan's limits it was
// checked against (limit -1 is none), and the sync as it stands.
export interface SyncCheck {
  check: string; // datapacks, memory_adds, kb_storage, kb_file_size or searchable_turns
  used: number;
  adding: number;
  limit: number;
  ok: boolean;
  message?: string;
}

export interface SyncPrecheck {
  ok: boolean;
  plan: string;
  checks: SyncCheck[];
}

export interface SyncRun {
  datapack_id: string;
  datapack_name: string;
  state: "syncing" | "done" | "failed";
  step: "" | "exporting" | "uploading" | "importing";
  error?: string;
  precheck?: SyncPrecheck;
  web_url?: string;
  started_at?: string;
  ended_at?: string;
  /** The sync's timeline in order; the last one is open while it runs, and a failed run failed in it. */
  phases?: SyncPhase[] | null;
  /** The export's bytes sent so far, once uploading starts. */
  upload?: { done: number; total: number };
}

export interface SyncPhase {
  name: string;
  started_at: string;
  ended_at?: string;
}
