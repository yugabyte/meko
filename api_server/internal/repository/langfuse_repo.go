package repository

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ErrLangfuseDBNotConfigured is returned by repo methods that need the
// Langfuse DB pool when no connection was configured at startup.
var ErrLangfuseDBNotConfigured = errors.New("langfuse database is not configured")

// LangfuseProjectKeys is one row of meko_system.langfuse_project_mapping.
type LangfuseProjectKeys struct {
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	PublicKey   string `json:"public_key"`
	SecretKey   string `json:"secret_key"`
}

// LangfuseOrgMapping is one row of meko_system.langfuse_org_mapping.
type LangfuseOrgMapping struct {
	OrgID     string
	OrgName   string
	PublicKey string
	SecretKey string
}

// AcceptedShareMembership is the normalized share row needed to backfill
// Langfuse org/project memberships for already-accepted datapack shares.
type AcceptedShareMembership struct {
	OwnerUserID    string
	DatapackID     string
	GrantRole      string
	RecipientEmail string
}

// Conversation is one row from ListConversationsForProject. Each row is
// one Langfuse session = one chat conversation. AgentID is empty when
// the session metadata didn't include it. FirstMessage is empty when
// the session has no messages yet (or the first observation had blank
// input); the UI renders its own placeholder for that case.
//
// Title falls back to the truncated first message when MCP didn't pass
// one to conversation_create. LastActivityAt is MAX(observation.end_time)
// across the trace; for sessions whose only observation is a zero-
// duration bookmark it equals StartedAt.
//
// MemoriesAdded is the count of *persisted* mem0 rows attributed to
// this conversation in datapacks.meko_mem0_collection — not the count
// of Memory Add tool spans in Langfuse. The two diverge in real
// production because mem0's fact extractor returns 0..N rows per call
// (vague text → 0, rich text → multiple) and dedup turns later ADDs
// into UPDATEs. We use the mem0 table because it's the user-facing
// truth (matches what the Memories tab shows); the span count is an
// internal observability metric we don't surface here. See
// LangfuseService for the cross-DB join.
//
// Token / cost fields are intentionally omitted: in current MCP
// instrumentation the Reasoning generation carries no usage and the
// per-tool OpenAI cost spans land in separate traces from the
// conversation, so summing within a trace returns 0 for the vast
// majority of real conversations. See the MCP follow-up notes in
// langfuse_store.add_message.
type Conversation struct {
	ID             string    `json:"id,omitempty"`
	AgentID        string    `json:"agent_id,omitempty"`
	Title          string    `json:"title,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	LastActivityAt time.Time `json:"last_activity_at"`
	FirstMessage   string    `json:"first_message"`
	Messages       int64     `json:"messages"`
	MemoriesAdded  int64     `json:"memories_added"`
	LLMCalls       int64     `json:"llm_calls"`
	// UserID is the meko_system.users.user_id (UUID) MCP wrote onto
	// clickhouse.traces.user_id at conversation_create time. The UI
	// uses it to scope the Workbench LM list to the logged-in user.
	UserID string `json:"user_id,omitempty"`
	// UserEmail is the user_email matching UserID, looked up once
	// per page from meko_system.users by the service layer. Empty
	// when the trace's user_id has no users row (legacy / orphaned
	// traces).
	UserEmail string `json:"user_email_id,omitempty"`
}

// ThreadMessage is one user/assistant exchange inside a conversation,
// returned by GetConversationThread. Reasoning is the chain-of-thought
// from the matched Reasoning generation (empty when the client didn't
// pass one to conversation_add_message).
//
// LatencyMs is the wall-clock duration of the turn — MAX(end_time) -
// MIN(start_time) over every observation that falls between this
// message's parent Conversation Add Message tool span and the next
// message's bookmark. This matches what the trace drawer shows in its
// header and includes downstream work the agent did after writing the
// reply (memory_add tool calls, follow-up reasoning, etc). Using the
// parent Conversation Add Message span alone would under-report —
// that span only wraps the act of persisting the row and is typically
// ~30ms even for a turn that ran for 12s of real work.
//
// MemoriesExtracted is the count of Memory Add tool spans attributed to
// this message window. It reflects extraction attempts observed in the
// trace, not guaranteed persisted rows in mem0.
type ThreadMessage struct {
	MessageID     string    `json:"message_id"`
	ObservationID string    `json:"observation_id"`
	StartedAt     time.Time `json:"started_at"`
	Input         string    `json:"input"`
	Output        string    `json:"output"`
	Reasoning     string    `json:"reasoning,omitempty"`
	// Plan is the structured checklist sent by clients via the `plan`
	// parameter of conversation_add_message. Persisted as
	// `observation.metadata.plan` on the same Reasoning generation that
	// carries Reasoning's prose `output`, and read independently of
	// `Reasoning` so a turn can have one, the other, both, or neither.
	// Nil / empty when the client didn't pass a non-empty plan array
	// (which is the case for every row that pre-dates the structured-
	// plan plumbing; consumers parse the legacy `**Plan:**` block out
	// of `Reasoning` as a fallback).
	Plan []string `json:"plan,omitempty"`
	// Model is the LLM identifier the agent's harness used for this
	// turn — populated when the client passes ``model`` to
	// ``conversation_add_message`` (today: Claude Code's session hook
	// reads it off ``message.model`` in each transcript line).
	// Persisted on the Reasoning generation's native
	// ``provided_model_name`` column; empty when the client didn't
	// supply it. Sampling params (temperature / max_tokens) and TTFT
	// are intentionally not surfaced — no supported host records them
	// in a hook-accessible way today, and fabricated values are
	// worse than missing.
	Model             string `json:"model,omitempty"`
	LatencyMs         int64  `json:"latency_ms"`
	MemoriesExtracted int64  `json:"memories_extracted"`
	// MemoryIDs lists the mem0 row IDs attributed to this message window.
	// Nil/empty when the message has no associated memories.
	MemoryIDs []string `json:"memory_ids,omitempty"`
	// MemoryTexts are the current mem0 memory texts, aligned with MemoryIDs.
	MemoryTexts []string `json:"memory_texts,omitempty"`
	// PromotedMemoryIDs / PromotedMemoryTexts: memories from this turn now
	// promoted to the KB (in promoted_collection). Excluded from the count;
	// surfaced separately as "Promoted to KB" and used for deep-link match.
	PromotedMemoryIDs   []string `json:"promoted_memory_ids,omitempty"`
	PromotedMemoryTexts []string `json:"promoted_memory_texts,omitempty"`
	LLMCalls            int64    `json:"llm_calls"`
	// TotalTokens is the sum of usage_details.total across every
	// GENERATION span in this turn's window — primarily mem0's nested
	// fact-extraction LLM calls for Conversations (the external agent's
	// own assistant call lives outside our process and contributes
	// nothing here). Always present; 0 when no usage was reported.
	TotalTokens int64 `json:"total_tokens"`
	// AnsweredFrom is "conversation_cache" when this turn was served from the
	// shared Q&A cache instead of the LLM (so total_tokens is ~0 by design).
	AnsweredFrom    string           `json:"answered_from,omitempty"`
	MemoryHits      []MemoryHit      `json:"memory_hits,omitempty"`
	KBHits          []KBHit          `json:"kb_hits,omitempty"`
	ProcessingSteps []ProcessingStep `json:"processing_steps,omitempty"`
}

type MemoryHit struct {
	Kind      string   `json:"kind"`
	Text      string   `json:"text"`
	Score     *float64 `json:"score,omitempty"`
	CharCount int      `json:"char_count,omitempty"`
}

type KBHit struct {
	DocumentID string `json:"document_id"`
	// DocumentName is the resolved file basename for document-sourced hits
	// (empty when unresolved or for promoted-memory hits).
	DocumentName string `json:"document_name,omitempty"`
	ChunkText    string `json:"chunk_text"`
	// Source is "memory" when the hit is a promoted memory living in the KB
	// (as opposed to a chunk from an uploaded document).
	Source    string   `json:"source,omitempty"`
	Score     *float64 `json:"score,omitempty"`
	Distance  *float64 `json:"distance,omitempty"`
	CharCount int      `json:"char_count,omitempty"`
}

type ProcessingStep struct {
	Name       string                 `json:"name"`
	DurationMs int64                  `json:"duration_ms"`
	Status     string                 `json:"status"`
	Detail     string                 `json:"detail,omitempty"`
	Query      string                 `json:"query,omitempty"`
	ToolArgs   map[string]interface{} `json:"tool_args,omitempty"`
	// SpanID is the Langfuse observation id of this step. Used by the UI
	// to deep-link the step row to the matching span inside the Langfuse
	// trace viewer. Empty for synthesized rows (e.g. context_assemble).
	SpanID string `json:"span_id,omitempty"`
	// ParentSpanID is this step's parent observation id, when it ran nested
	// inside another step's span (e.g. conversation_search under
	// knowledgebase_search). Lets the UI render it as a child. Empty for roots.
	ParentSpanID string `json:"parent_span_id,omitempty"`
	// SubTimings carries durations of child spans rolled up under this
	// step — e.g. memory_search splits into `vector_ms` (mem0's
	// `Searching (yugabytedb / vector)` span) and `graph_ms` (mem0's
	// `Search Graph DB (...)` span); knowledgebase_search uses
	// `vector_ms` for MCP's `Execute Query (yugabytedb)` wrapper. Nil
	// when the step has no surfaced sub-spans (e.g. memory_add,
	// llm_invoke).
	SubTimings map[string]int64 `json:"sub_timings,omitempty"`
	// ConversationChunks are the prior-conversation excerpts this step
	// retrieved — populated only on the `conversation_search` step. Each
	// links back to its source conversation/user. Empty on other steps.
	ConversationChunks []ConversationChunk `json:"conversation_chunks,omitempty"`
}

type messageWindow struct {
	MessageID string
	Start     time.Time
	NextStart *time.Time
	// CaptureLeading attributes observations that start *before* Start to
	// this window. Only valid when this is the genuine first bookmark of
	// the trace (full-thread query from offset 0, or a single-message
	// trace whose target is turn 1). When the window is the target turn of
	// a single-message trace that is NOT the first turn, leaving this false
	// stops every prior turn's spans (memory/kb searches) from leaking in
	// — the cause of the per-turn kb-chunk inflation (50 → 100 → 150).
	CaptureLeading bool
	// CaptureTrailing attributes observations that start *after* NextStart
	// (or after Start when NextStart is nil) to this window. Only valid
	// when this is the genuine last bookmark of the trace. Prevents later
	// turns' rows from leaking into a single-message trace's target turn.
	CaptureTrailing bool
}

// ConversationDetail is the full envelope returned by
// GetConversationThread: a header (same shape as a list row) plus a
// page of thread messages.
type ConversationDetail struct {
	Conversation
	Thread        []ThreadMessage `json:"thread"`
	ThreadLimit   int             `json:"thread_limit"`
	ThreadOffset  int             `json:"thread_offset"`
	ThreadHasMore bool            `json:"thread_has_more"`
	// CanIndex: WorkbenchLM conversation owned by the viewer that isn't yet in
	// the conversation cache → the UI shows an "Index conversation" button.
	CanIndex bool `json:"can_index"`
}

// TraceSpan is one observation in the per-message span tree returned
// by GetMessageTrace. ParentObservationID is empty when the row is the
// root of the windowed sub-tree. DurationMs / OffsetMs are derived
// from start_time / end_time relative to the message bookmark.
type TraceSpan struct {
	ID                  string `json:"id"`
	ParentObservationID string `json:"parent_observation_id,omitempty"`
	Name                string `json:"name"`
	Type                string `json:"type"`
	DurationMs          int64  `json:"duration_ms"`
	OffsetMs            int64  `json:"offset_ms"`
	Model               string `json:"model,omitempty"`
	Usage               string `json:"usage,omitempty"`
	TotalCost           string `json:"total_cost,omitempty"`
	// Output is the raw output blob for the observation. Only populated
	// for memory-related spans (name ILIKE '%memor%') to keep payloads
	// small. The UI uses it to surface extracted memory text.
	Output string `json:"output,omitempty"`
	// Input is the actual statement executed by a DB span — the SQL for
	// `Execute Query (...)` (vector) spans and the Cypher for
	// `Execute Cypher Query (...)` (graph) spans. Extracted from the
	// langfuse observation `input` (mem0 wraps each cursor call with
	// @observe, which records the function args). Only the statement text
	// is surfaced — bind params (which include huge embedding vectors)
	// are deliberately dropped. Empty for non-DB spans.
	Input string `json:"input,omitempty"`
	// MemoryItems is a per-memory expansion for memory spans. Each item maps one
	// extracted memory text to persisted-memory metadata and graph/vector counts.
	MemoryItems []TraceMemoryItem `json:"memory_items,omitempty"`
}

type TraceMemoryItem struct {
	MemoryID string `json:"memory_id,omitempty"`
	Text     string `json:"text"`
	// Promoted is true when this memory has been promoted to the KB and
	// evicted from mem0 (it now lives in datapacks.promoted_collection).
	// The vector/entity/relation counts below are meaningless for promoted
	// items (the mem0 anchor row is gone), so the UI shows just the text
	// plus a "Promoted to KB" tag instead of the count badges.
	Promoted  bool  `json:"promoted,omitempty"`
	Vector    int64 `json:"vector"`
	Entities  int64 `json:"entities"`
	Relations int64 `json:"relations"`
	// EntityNames are sampled graph-node names linked to this memory (if present).
	EntityNames []string `json:"entity_names,omitempty"`
	// RelationNames are sampled relation labels/types linked to those nodes (if present).
	RelationNames []string `json:"relation_names,omitempty"`
}

// MessageDecisionTrace is the slim per-turn payload the chat UI's
// "Decision Trace" card and AssistantMetaRow chips actually read.
// Drops fields a single-message poll doesn't need (input/output text
// is already in the bubble; observation_id/started_at aren't surfaced
// anywhere). Sourced by projecting one row out of the existing
// ThreadMessage list — no new SQL path.
type MessageDecisionTrace struct {
	MessageID   string `json:"message_id"`
	Model       string `json:"model,omitempty"`
	LatencyMs   int64  `json:"latency_ms"`
	TotalTokens int64  `json:"total_tokens"`
	// AnsweredFrom is "conversation_cache" when this turn was served from the
	// shared cache (LLM skipped); empty otherwise. (The retrieved chunks
	// themselves live on the `conversation_search` processing step — see
	// ProcessingStep.ConversationChunks.)
	AnsweredFrom      string           `json:"answered_from,omitempty"`
	LLMCalls          int64            `json:"llm_calls"`
	MemoriesExtracted int64            `json:"memories_extracted"`
	Reasoning         string           `json:"reasoning,omitempty"`
	Plan              []string         `json:"plan,omitempty"`
	MemoryHits        []MemoryHit      `json:"memory_hits,omitempty"`
	KBHits            []KBHit          `json:"kb_hits,omitempty"`
	ProcessingSteps   []ProcessingStep `json:"processing_steps,omitempty"`
}

// ConversationChunk is one prior-conversation excerpt that fed a cache-hit
// summary. conversation_id + user_id let the UI link back to the source.
type ConversationChunk struct {
	Question       string   `json:"question,omitempty"`
	Answer         string   `json:"answer,omitempty"`
	Distance       *float64 `json:"distance,omitempty"`
	ConversationID string   `json:"conversation_id,omitempty"`
	MessageID      string   `json:"message_id,omitempty"`
	UserID         string   `json:"user_id,omitempty"`
}

// LangfuseRepo encapsulates database operations for Langfuse mappings
// and direct reads against the Langfuse DB.
//
// Backing tables:
//   - system DB:   meko_system.langfuse_org_mapping,
//     meko_system.langfuse_project_mapping, meko_system.users,
//     meko_system.datapacks, meko_system.datapack_invites
//   - data DB:     datapacks.meko_mem0_collection (trace-enrichment lookups)
//   - langfuse DB: public.api_keys, clickhouse.traces, clickhouse.observations
type LangfuseRepo struct {
	systemDB   *sql.DB
	dataDB     *sql.DB
	langfuseDB *sql.DB
}

// NewLangfuseRepo creates a new LangfuseRepo. systemDB is required and
// serves the meko_system.* registries. dataDB is required and serves
// datapacks.meko_mem0_collection for trace-memory enrichment; when the
// api_server runs against a single physical database the two handles
// are the same. langfuseDB is optional and may be nil — methods that
// need it return ErrLangfuseDBNotConfigured.
func NewLangfuseRepo(systemDB, dataDB, langfuseDB *sql.DB) *LangfuseRepo {
	return &LangfuseRepo{systemDB: systemDB, dataDB: dataDB, langfuseDB: langfuseDB}
}

// LangfuseDBConfigured reports whether the Langfuse DB pool is wired.
func (r *LangfuseRepo) LangfuseDBConfigured() bool {
	return r != nil && r.langfuseDB != nil
}

// GetOrgMapping returns the org mapping for the user, or nil if absent.
// The accountID parameter is the user_id (post multi-tenant migration the
// column was renamed; the Go param name is kept for backward compat).
func (r *LangfuseRepo) GetOrgMapping(accountID string) (*LangfuseOrgMapping, error) {
	var m LangfuseOrgMapping
	err := r.systemDB.QueryRow(
		`SELECT langfuse_org_id, langfuse_org_name, langfuse_org_public_key, langfuse_org_secret_key
		 FROM meko_system.langfuse_org_mapping
		 WHERE user_id = $1`,
		accountID,
	).Scan(&m.OrgID, &m.OrgName, &m.PublicKey, &m.SecretKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to fetch langfuse org mapping: %w", err)
	}
	return &m, nil
}

// GetUserEmailByID returns users.user_email for the given user_id.
func (r *LangfuseRepo) GetUserEmailByID(userID string) (string, error) {
	var email string
	err := r.systemDB.QueryRow(
		`SELECT user_email FROM meko_system.users WHERE user_id = $1`,
		userID,
	).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to fetch user email: %w", err)
	}
	return strings.TrimSpace(email), nil
}

// GetUserEmailsByIDs returns user_id → user_email for the supplied
// user_ids in a single round-trip. Invalid UUIDs / unknown ids are
// absent from the result. Filters Go-side and uses `uuid[]` so the
// PK index on meko_system.users(user_id) is used.
func (r *LangfuseRepo) GetUserEmailsByIDs(userIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(userIDs))
	if r == nil || r.systemDB == nil || len(userIDs) == 0 {
		return out, nil
	}

	validUUIDs := make([]string, 0, len(userIDs))
	for _, id := range userIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			continue
		}
		validUUIDs = append(validUUIDs, id)
	}
	if len(validUUIDs) == 0 {
		return out, nil
	}

	rows, err := r.systemDB.Query(
		`SELECT user_id::text, user_email
		   FROM meko_system.users
		  WHERE user_id = ANY($1::uuid[])`,
		pq.Array(validUUIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch user emails: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, email string
		if err := rows.Scan(&id, &email); err != nil {
			return nil, fmt.Errorf("failed to scan user email: %w", err)
		}
		out[id] = strings.TrimSpace(email)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate user emails: %w", err)
	}
	return out, nil
}

// UpsertOrgMapping inserts or updates the org mapping for the user.
// The accountID parameter is the user_id (see GetOrgMapping for context).
func (r *LangfuseRepo) UpsertOrgMapping(accountID, orgID, orgName, publicKey, secretKey string) error {
	_, err := r.systemDB.Exec(
		`INSERT INTO meko_system.langfuse_org_mapping
		 (user_id, langfuse_org_id, langfuse_org_name, langfuse_org_public_key, langfuse_org_secret_key)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (user_id) DO UPDATE SET
		   langfuse_org_id = EXCLUDED.langfuse_org_id,
		   langfuse_org_name = EXCLUDED.langfuse_org_name,
		   langfuse_org_public_key = EXCLUDED.langfuse_org_public_key,
		   langfuse_org_secret_key = EXCLUDED.langfuse_org_secret_key,
		   updated_at = NOW()`,
		accountID, orgID, orgName, publicKey, secretKey,
	)
	if err != nil {
		return fmt.Errorf("failed to persist langfuse org mapping: %w", err)
	}
	return nil
}

// GetProjectMapping returns the project mapping for a datapack, or nil
// if absent.
func (r *LangfuseRepo) GetProjectMapping(datapackID string) (*LangfuseProjectKeys, error) {
	if strings.TrimSpace(datapackID) == "" {
		return nil, fmt.Errorf("datapack_id is required")
	}
	var p LangfuseProjectKeys
	err := r.systemDB.QueryRow(
		`SELECT langfuse_project_id, langfuse_project_name, langfuse_public_key, langfuse_secret_key
		 FROM meko_system.langfuse_project_mapping
		 WHERE datapack_id = $1`,
		datapackID,
	).Scan(&p.ProjectID, &p.ProjectName, &p.PublicKey, &p.SecretKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to fetch langfuse project mapping: %w", err)
	}
	return &p, nil
}

// GetDatapackNameByID returns datapacks.datapack_name for the given datapack_id.
func (r *LangfuseRepo) GetDatapackNameByID(datapackID string) (string, error) {
	var name string
	err := r.systemDB.QueryRow(
		`SELECT datapack_name FROM meko_system.datapacks WHERE datapack_id = $1`,
		datapackID,
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to fetch datapack name: %w", err)
	}
	return strings.TrimSpace(name), nil
}

// ListAcceptedShareMembershipsForRecipient returns accepted datapack shares
// for a recipient user, deduplicated to the latest invite per datapack.
func (r *LangfuseRepo) ListAcceptedShareMembershipsForRecipient(recipientUserID string) ([]AcceptedShareMembership, error) {
	recipientUserID = strings.TrimSpace(recipientUserID)
	if recipientUserID == "" {
		return nil, fmt.Errorf("recipient_user_id is required")
	}

	const q = `
		SELECT DISTINCT ON (di.datapack_id)
		       di.owner_user_id,
		       di.datapack_id,
		       di.grant_role,
		       COALESCE(u.user_email, '')
		  FROM meko_system.datapack_invites di
		  LEFT JOIN meko_system.users u
		         ON u.user_id = di.recipient_user_id
		 WHERE di.recipient_user_id = $1
		   AND di.status::text = 'accepted'
		 ORDER BY di.datapack_id, di.updated_at DESC, di.created_at DESC`

	rows, err := r.systemDB.Query(q, recipientUserID)
	if err != nil {
		return nil, fmt.Errorf("failed to list accepted share memberships: %w", err)
	}
	defer rows.Close()

	out := make([]AcceptedShareMembership, 0, 8)
	for rows.Next() {
		var row AcceptedShareMembership
		if err := rows.Scan(&row.OwnerUserID, &row.DatapackID, &row.GrantRole, &row.RecipientEmail); err != nil {
			return nil, fmt.Errorf("failed to scan accepted share membership: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate accepted share memberships: %w", err)
	}
	return out, nil
}

// UpsertProjectMapping inserts or updates the project mapping.
// Post multi-tenant migration, langfuse_project_mapping is keyed
// solely on datapack_id and the user is derived via the join to
// meko_system.datapacks when needed.
func (r *LangfuseRepo) UpsertProjectMapping(datapackID, projectID, projectName, publicKey, secretKey string) error {
	if strings.TrimSpace(datapackID) == "" {
		return fmt.Errorf("failed to persist langfuse project mapping: datapack_id is required")
	}
	_, err := r.systemDB.Exec(
		`INSERT INTO meko_system.langfuse_project_mapping
		 (datapack_id, langfuse_project_id, langfuse_project_name, langfuse_public_key, langfuse_secret_key)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (datapack_id) DO UPDATE SET
		   langfuse_project_id = EXCLUDED.langfuse_project_id,
		   langfuse_project_name = EXCLUDED.langfuse_project_name,
		   langfuse_public_key = EXCLUDED.langfuse_public_key,
		   langfuse_secret_key = EXCLUDED.langfuse_secret_key,
		   updated_at = NOW()`,
		datapackID, projectID, projectName, publicKey, secretKey,
	)
	if err != nil {
		return fmt.Errorf("failed to persist langfuse project mapping: %w", err)
	}
	return nil
}

// DeleteProjectMapping removes the langfuse_project_mapping row for a datapack.
// It is idempotent: deleting a non-existing mapping is treated as success.
func (r *LangfuseRepo) DeleteProjectMapping(datapackID string) error {
	if strings.TrimSpace(datapackID) == "" {
		return fmt.Errorf("failed to delete langfuse project mapping: datapack_id is required")
	}
	if _, err := r.systemDB.Exec(
		`DELETE FROM meko_system.langfuse_project_mapping
		 WHERE datapack_id = $1`,
		datapackID,
	); err != nil {
		return fmt.Errorf("failed to delete langfuse project mapping: %w", err)
	}
	return nil
}

// FindLangfuseAPIKeyID returns public.api_keys.id for (project_id,
// public_key) on the Langfuse DB; found=false (no error) when no row
// matches — caller decides whether that's a drift error.
func (r *LangfuseRepo) FindLangfuseAPIKeyID(projectID, publicKey string) (string, bool, error) {
	if !r.LangfuseDBConfigured() {
		return "", false, ErrLangfuseDBNotConfigured
	}
	var id string
	err := r.langfuseDB.QueryRow(
		`SELECT id
		   FROM public.api_keys
		  WHERE project_id = $1
		    AND public_key = $2
		  LIMIT 1`,
		projectID, publicKey,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("langfuse db: verify api_keys: %w", err)
	}
	return id, true, nil
}

// CountDistinctSessionsForProject counts conversations in the
// project that have at least one message (observation tagged
// with metadata.message_id by add_message). Bare traces with no
// messages are excluded so the count matches the listing endpoint.
func (r *LangfuseRepo) CountDistinctSessionsForProject(projectID string) (int64, error) {
	if !r.LangfuseDBConfigured() {
		return 0, ErrLangfuseDBNotConfigured
	}
	const query = `
		SELECT COUNT(DISTINCT t.session_id)
		FROM   clickhouse.traces t
		WHERE  t.project_id  = $1
		  AND  t.session_id IS NOT NULL
		  AND  t.is_deleted  = false
		  AND  EXISTS (
		      -- a message = observation whose metadata has message_id set
		      SELECT 1
		      FROM   clickhouse.observations o
		      WHERE  o.project_id = t.project_id
		        AND  o.trace_id   = t.id
		        AND  (o.metadata ->> 'message_id') IS NOT NULL
		  )
	`
	var total int64
	err := r.langfuseDB.QueryRow(query, projectID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("langfuse db: count distinct session_id: %w", err)
	}
	return total, nil
}

// SumTokenUsageByUserAndAgent sums `usage_details.total` across GENERATION
// observations whose parent trace matches (user, agent).
//
// TODO(scaling): full-scans `traces` + `observations` — neither
// `idx_traces_user_id` nor `idx_observations_trace_id` is enabled in
// the langfuse schema.
func (r *LangfuseRepo) SumTokenUsageByUserAndAgent(userID, agentID string) (int64, error) {
	if !r.LangfuseDBConfigured() {
		return 0, ErrLangfuseDBNotConfigured
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(agentID) == "" {
		return 0, fmt.Errorf("user_id and agent_id are required")
	}
	const query = `
		SELECT COALESCE(SUM(
		           CASE WHEN o.type = 'GENERATION' AND o.usage_details ? 'total'
		                THEN (o.usage_details ->> 'total')::bigint
		                ELSE 0 END
		       ), 0)
		FROM   clickhouse.traces       t
		JOIN   clickhouse.observations o
		  ON   o.project_id = t.project_id
		  AND  o.trace_id   = t.id
		WHERE  t.user_id              = $1
		  AND  (t.metadata->>'agent_id') = $2
		  AND  t.is_deleted = false
	`
	var total int64
	if err := r.langfuseDB.QueryRow(query, userID, agentID).Scan(&total); err != nil {
		return 0, fmt.Errorf("langfuse db: sum token usage for user %s agent %s: %w", userID, agentID, err)
	}
	return total, nil
}

// ListConversationsForProject returns one page of Langfuse sessions
// (= chat conversations) for the project, newest first. Pagination is
// offset-based.
//
// Each output row: one Langfuse session, with its first user prompt
// and total message count attached.
//
//	$1 = langfuse project_id
//	$2 = page size  (e.g. 20)
//	$3 = offset     (e.g. 0)
func (r *LangfuseRepo) ListConversationsForProject(projectID string, limit, offset int, q string) ([]Conversation, error) {
	if !r.LangfuseDBConfigured() {
		return nil, ErrLangfuseDBNotConfigured
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	const query = `
-- One page of conversations enriched with title, last activity, and
-- per-trace counters. UI uses this for an infinite-scroll list.
--
-- All per-trace columns are derived from a single scan of
-- clickhouse.observations using conditional aggregation
-- (COUNT/MAX/ARRAY_AGG ... FILTER WHERE), instead of separately
-- joining four subqueries that each re-scanned the table. The HAVING
-- clause preserves the original INNER-JOIN semantics that drop
-- conversations with zero chat messages.
SELECT
    sessions.conversation_id,
    sessions.agent_id,
    sessions.user_id,
    sessions.started_at,
    COALESCE(obs_agg.title,
             LEFT(obs_agg.first_message, 80))               AS title,
    obs_agg.first_message                                   AS first_message,
    COALESCE(obs_agg.messages, 0)                           AS messages,
    COALESCE(obs_agg.last_activity_at, sessions.started_at) AS last_activity_at,
    COALESCE(obs_agg.llm_calls, 0)                          AS llm_calls

FROM (
    -- One row per session: earliest trace wins (agent_id, user_id,
    -- started_at). user_id is the meko user UUID written on the
    -- trace at conversation_create time; the service layer pairs it
    -- with the matching user_email from meko_system.users before
    -- returning to API callers.
    SELECT DISTINCT ON (traces.session_id)
        traces.session_id              AS conversation_id,
        traces.metadata->>'agent_id'   AS agent_id,
        traces.user_id                 AS user_id,
        traces."timestamp"             AS started_at
    FROM   clickhouse.traces AS traces
    WHERE  traces.project_id  = $1
      AND  traces.session_id IS NOT NULL
      AND  traces.is_deleted  = false
    ORDER  BY traces.session_id, traces."timestamp" ASC
) AS sessions

JOIN (
    -- Single scan of observations, grouped by trace_id. Each output
    -- column comes from a different FILTER clause:
    --   * messages          — count of Conversation Message Added bookmarks.
    --   * first_message     — earliest add_message input (used as preview /
    --                         title fallback). We aggregate inputs ordered by
    --                         start_time and take [1], which is the first
    --                         element because Postgres arrays are 1-indexed.
    --                         This avoids a separate DISTINCT ON subquery.
    --   * title             — earliest title set on the Conversation Created
    --                         marker, if any.
    --   * last_activity_at  — latest end_time across the whole trace.
    --   * llm_calls         — model-call estimate: use GENERATION spans
    --                         when present, otherwise fall back to the
    --                         count of persisted assistant turns.
    -- The HAVING ... > 0 clause keeps the previous INNER JOIN semantics:
    -- conversations with 0 chat messages are dropped from the list.
    SELECT
        o.trace_id AS conversation_id,
        -- Number of Conversation Message Added markers (one per persisted turn).
        COUNT(*) FILTER (
            WHERE o.name = 'Conversation Message Added'
              AND (o.metadata ->> 'message_id') IS NOT NULL
        ) AS messages,

        -- Earliest user prompt text, used as list preview/fallback title.
        (ARRAY_AGG(o.input ORDER BY o.start_time ASC) FILTER (
            WHERE o.name = 'Conversation Message Added'
              AND (o.metadata ->> 'message_id') IS NOT NULL
        ))[1] AS first_message,

        -- Earliest explicit conversation title, if one was set.
        (ARRAY_AGG(NULLIF(o.metadata ->> 'title', '') ORDER BY o.start_time ASC) FILTER (
            WHERE o.name = 'Conversation Created'
        ))[1] AS title,

        -- Latest/last observation end timestamp across the whole trace (= last activity).
        MAX(o.end_time) AS last_activity_at,

        -- Estimate model calls robustly for mixed instrumentation:
        -- prefer explicit GENERATION spans; if missing, use persisted
        -- assistant-turn bookmarks as fallback.
        GREATEST(
            COUNT(*) FILTER (WHERE o.type = 'GENERATION'),
            COUNT(*) FILTER (
                WHERE o.name = 'Conversation Message Added'
                  AND (o.metadata ->> 'message_id') IS NOT NULL
            )
        ) AS llm_calls
    FROM   clickhouse.observations AS o
    WHERE  o.project_id = $1
      AND  o.is_deleted = false
    GROUP  BY o.trace_id
    HAVING COUNT(*) FILTER (
            WHERE o.name = 'Conversation Message Added'
              AND (o.metadata ->> 'message_id') IS NOT NULL
        ) > 0
) AS obs_agg ON obs_agg.conversation_id = sessions.conversation_id

-- Optional substring filter on first_message. $2 = '' disables it so
-- the same query handles "no search" and "search" without dynamic SQL.
WHERE ($2 = '' OR obs_agg.first_message ILIKE '%' || $2 || '%')
ORDER BY last_activity_at DESC NULLS LAST, sessions.started_at DESC
LIMIT  $3 OFFSET $4`

	rows, err := r.langfuseDB.Query(query, projectID, strings.TrimSpace(q), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("langfuse db: list conversations: %w", err)
	}
	defer rows.Close()

	out := make([]Conversation, 0, limit)
	for rows.Next() {
		var (
			conv      Conversation
			agentID   sql.NullString
			userID    sql.NullString
			title     sql.NullString
			firstMsg  sql.NullString
			startedAt sql.NullTime
			lastActAt sql.NullTime
		)
		if err := rows.Scan(
			&conv.ID,
			&agentID,
			&userID,
			&startedAt,
			&title,
			&firstMsg,
			&conv.Messages,
			&lastActAt,
			&conv.LLMCalls,
		); err != nil {
			return nil, fmt.Errorf("langfuse db: scan conversation row: %w", err)
		}
		if agentID.Valid {
			conv.AgentID = agentID.String
		}
		if userID.Valid {
			conv.UserID = strings.TrimSpace(userID.String)
		}
		if title.Valid {
			conv.Title = title.String
		}
		if firstMsg.Valid {
			conv.FirstMessage = firstMsg.String
		}
		if startedAt.Valid {
			conv.StartedAt = startedAt.Time
		}
		if lastActAt.Valid {
			conv.LastActivityAt = lastActAt.Time
		} else {
			conv.LastActivityAt = conv.StartedAt
		}
		out = append(out, conv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("langfuse db: iterate conversation rows: %w", err)
	}
	return out, nil
}

// ConversationHistoryIndexed reports whether any conversation_history_search
// row already exists for this conversation (datapack = tenant). Indexed lookup
// via the (tenant_id, conversation_id) index — no scan.
func (r *LangfuseRepo) ConversationHistoryIndexed(datapackID, conversationID string) (bool, error) {
	if r == nil || r.dataDB == nil {
		return false, fmt.Errorf("data db not configured")
	}
	dpUUID := canonicalUUID(datapackID)
	convUUID := canonicalUUID(conversationID)
	if dpUUID == "" || convUUID == "" {
		return false, nil
	}
	const q = `SELECT 1 FROM datapacks.conversation_history_search
	           WHERE tenant_id = $1::uuid AND conversation_id = $2::uuid LIMIT 1`
	var one int
	switch err := r.dataDB.QueryRow(q, dpUUID, convUUID).Scan(&one); err {
	case nil:
		return true, nil
	case sql.ErrNoRows:
		return false, nil
	default:
		return false, fmt.Errorf("meta db: conversation indexed check: %w", err)
	}
}

// GetConversationThread returns the header aggregates for one
// conversation plus a page of its messages, paired with reasoning and
// per-turn counters. messagesLimit/messagesOffset paginate through
// the thread (UI uses messagesOffset+messagesLimit for "load older
// turns" without re-fetching the header).
//
//	$1 = langfuse project_id
//	$2 = session_id (= trace_id, hyphens stripped by MCP)
//	$3 = messages page size
//	$4 = messages offset
//
// Returns (nil, nil) when the project has no rows for the session_id;
// callers translate that into a 404. The header always reflects the
// full conversation; only the thread slice is paginated.
func (r *LangfuseRepo) GetConversationThread(
	projectID, sessionID, datapackID string,
	messagesLimit, messagesOffset int,
) (*ConversationDetail, error) {
	if !r.LangfuseDBConfigured() {
		return nil, ErrLangfuseDBNotConfigured
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	datapackID = strings.TrimSpace(datapackID)
	if datapackID == "" {
		return nil, fmt.Errorf("datapack_id is required")
	}
	if messagesLimit <= 0 {
		messagesLimit = 100
	}
	if messagesOffset < 0 {
		messagesOffset = 0
	}

	// Same conditional-aggregation shape as ListConversationsForProject:
	// every per-trace column comes from a single scan of
	// clickhouse.observations filtered to this trace, instead of four
	// separate LEFT JOINs each re-scanning the table. Header always
	// reflects the full conversation; only the thread slice is paginated.
	const headerQ = `
SELECT
    sessions.conversation_id,
    sessions.agent_id,
    sessions.user_id,
    sessions.started_at,
    COALESCE(obs_agg.title, LEFT(obs_agg.first_message, 80))  AS title,
    obs_agg.first_message,
    COALESCE(obs_agg.messages, 0)                             AS messages,
    COALESCE(obs_agg.last_activity_at, sessions.started_at)   AS last_activity_at,
    COALESCE(obs_agg.llm_calls, 0)                            AS llm_calls
FROM (
    SELECT DISTINCT ON (traces.session_id)
        traces.session_id              AS conversation_id,
        traces.metadata->>'agent_id'   AS agent_id,
        traces.user_id                 AS user_id,
        traces."timestamp"             AS started_at
    FROM clickhouse.traces AS traces
    WHERE traces.project_id = $1
      AND traces.session_id = $2
      AND traces.is_deleted = false
    ORDER BY traces.session_id, traces."timestamp" ASC
) AS sessions
LEFT JOIN (
    SELECT
        o.trace_id AS conversation_id,
        COUNT(*) FILTER (
            WHERE o.name = 'Conversation Message Added'
              AND (o.metadata ->> 'message_id') IS NOT NULL
        ) AS messages,
        (ARRAY_AGG(o.input ORDER BY o.start_time ASC) FILTER (
            WHERE o.name = 'Conversation Message Added'
              AND (o.metadata ->> 'message_id') IS NOT NULL
        ))[1] AS first_message,
        (ARRAY_AGG(NULLIF(o.metadata ->> 'title', '') ORDER BY o.start_time ASC) FILTER (
            WHERE o.name = 'Conversation Created'
        ))[1] AS title,
        MAX(o.end_time) AS last_activity_at,
        GREATEST(
            COUNT(*) FILTER (WHERE o.type = 'GENERATION'),
            COUNT(*) FILTER (
                WHERE o.name = 'Conversation Message Added'
                  AND (o.metadata ->> 'message_id') IS NOT NULL
            )
        ) AS llm_calls
    FROM clickhouse.observations AS o
    WHERE o.project_id = $1
      AND o.trace_id   = $2
      AND o.is_deleted = false
    GROUP BY o.trace_id
) AS obs_agg ON obs_agg.conversation_id = sessions.conversation_id
LIMIT 1`

	var (
		header    ConversationDetail
		agentID   sql.NullString
		userID    sql.NullString
		title     sql.NullString
		firstMsg  sql.NullString
		startedAt sql.NullTime
		lastActAt sql.NullTime
	)
	err := r.langfuseDB.QueryRow(headerQ, projectID, sessionID).Scan(
		&header.ID,
		&agentID,
		&userID,
		&startedAt,
		&title,
		&firstMsg,
		&header.Messages,
		&lastActAt,
		&header.LLMCalls,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("langfuse db: load conversation header: %w", err)
	}
	if agentID.Valid {
		header.AgentID = agentID.String
	}
	if userID.Valid {
		header.UserID = userID.String
	}
	if title.Valid {
		header.Title = title.String
	}
	if firstMsg.Valid {
		header.FirstMessage = firstMsg.String
	}
	if startedAt.Valid {
		header.StartedAt = startedAt.Time
	}
	if lastActAt.Valid {
		header.LastActivityAt = lastActAt.Time
	} else {
		header.LastActivityAt = header.StartedAt
	}

	// Spans are attributed to the *next* Conversation Message Added bookmark
	// (work happens before the bookmark, which is written at turn end). We
	// compute each bookmark's owning interval (prev_bm_time, bm_time] once
	// in a window pass over bookmarks (M rows), then join observations to
	// their interval in a single O(N) range scan rather than running two
	// correlated subqueries per observation.
	const threadQ = `
WITH raw_obs AS (
    SELECT id, parent_observation_id, type, name, start_time, end_time, input::text AS input, output::text AS output,
           usage_details,
           metadata ->> 'message_id' AS msg_id,
           -- Structured plan (string[]) from conversation_add_message.
           -- Extracted as text here so the outer CTE can scan it
           -- without re-parsing the full metadata blob. NULL when the
           -- observation has no plan key (most non-Reasoning rows).
           CASE WHEN metadata ? 'plan' THEN metadata ->> 'plan' END AS plan_json,
           -- LLM identifier (carried on the Reasoning generation when
           -- the client passes the model parameter). Langfuse-native column.
           provided_model_name,
           -- Whole user_metadata blob (cache-hit provenance + chunks) as text;
           -- parsed in Go, robust to how Langfuse nests it.
           metadata ->> 'user_metadata' AS user_metadata_json,
           (metadata ->> 'message_id' IS NOT NULL AND name = 'Conversation Message Added') AS is_bookmark
    FROM   clickhouse.observations
    WHERE  project_id = $1 AND trace_id = $2 AND is_deleted = false
), bookmarks AS (
    SELECT msg_id, start_time AS bm_time,
           LAG(start_time)  OVER (ORDER BY start_time)      AS prev_bm_time,
           (ROW_NUMBER() OVER (ORDER BY start_time DESC) = 1) AS is_last
    FROM   raw_obs
    WHERE  is_bookmark
), windowed AS (
    SELECT b.msg_id, o.id, o.parent_observation_id, o.type, o.name, o.start_time, o.end_time, o.output,
           o.usage_details, o.provided_model_name
    FROM   raw_obs o
    JOIN   bookmarks b
      ON   (o.start_time <= b.bm_time AND (b.prev_bm_time IS NULL OR o.start_time > b.prev_bm_time))
        OR (b.is_last AND o.start_time > b.bm_time)
), turn_stats AS (
    SELECT msg_id AS message_id,
           MIN(start_time) AS root_start,
           COUNT(*) FILTER (WHERE type = 'GENERATION') AS llm_calls,
           -- Sum of usage_details.total across every GENERATION span in
           -- the window. For Conversations this primarily captures
           -- mem0's nested fact-extraction LLM calls (the Reasoning span
           -- emitted by MCP does not carry token usage today); empty
           -- agents will report 0.
           COALESCE(
               SUM(
                   CASE WHEN type = 'GENERATION'
                            AND usage_details ? 'total'
                        THEN (usage_details ->> 'total')::bigint
                        ELSE 0 END
               ),
               0
           )                                                                                  AS total_tokens,
           COALESCE((EXTRACT(EPOCH FROM (MAX(COALESCE(end_time, start_time)) - MIN(start_time))) * 1000)::bigint, 0) AS latency_ms
    FROM   windowed
    GROUP  BY msg_id
), reasoning AS (
    -- Reasoning is emitted by MCP as a direct child of the
    -- "Conversation Message Added" bookmark, but its start_time is
    -- typically 1-2 ms AFTER its parent's start_time. The preceding-
    -- window join used for retrieval spans (above) therefore drops
    -- each turn's Reasoning into the NEXT turn's window. Attribute by
    -- parent_observation_id so reasoning always lands on the turn that
    -- produced it.
    --
    -- plan_json is the structured checklist persisted alongside the
    -- prose. When a turn has multiple Reasoning observations (rare,
    -- but the schema allows it) we keep the earliest non-null plan in
    -- start_time order -- picking later ones would silently overwrite
    -- the plan the user actually saw render.
    SELECT b.msg_id AS message_id,
           STRING_AGG(r.output, E'\n\n' ORDER BY r.start_time) AS thinking,
           (array_agg(r.plan_json ORDER BY r.start_time)
                FILTER (WHERE r.plan_json IS NOT NULL))[1]   AS plan_json
    FROM   raw_obs r
    JOIN   raw_obs b
      ON   b.id = r.parent_observation_id
     AND   b.is_bookmark
    WHERE  r.type = 'GENERATION' AND r.name = 'Reasoning'
    GROUP  BY b.msg_id
), model_pick AS (
    -- Model from any real LLM GENERATION in the turn's window —
    -- mem0's instrumented OpenAI / Bedrock / etc. or any future
    -- agent-side LLM. Excludes the synthetic Reasoning row (no real
    -- call) and CostUsage Probe (sentinel). Provider-agnostic so a
    -- mem0 LLM_PROVIDER switch doesn't blank out the model column.
    SELECT msg_id,
           (array_agg(provided_model_name ORDER BY start_time)
              FILTER (WHERE type = 'GENERATION'
                AND name NOT IN ('Reasoning', 'CostUsage Probe')
                AND provided_model_name IS NOT NULL
                AND provided_model_name <> ''))[1] AS model
    FROM   windowed
    GROUP  BY msg_id
)
SELECT b.msg_id AS message_id, b.id AS observation_id, s.root_start, b.start_time,
       LEAD(s.root_start) OVER (ORDER BY s.root_start, b.start_time) AS next_start,
       b.input, b.output,
       COALESCE(r.thinking, '') AS reasoning,
       r.plan_json AS plan_json,
       mp.model AS model,
       s.latency_ms,
       COALESCE(s.llm_calls, 0) AS llm_calls,
       COALESCE(s.total_tokens, 0) AS total_tokens,
       b.user_metadata_json AS user_metadata_json
FROM   raw_obs b
JOIN   turn_stats s ON s.message_id = b.msg_id
LEFT JOIN reasoning r ON r.message_id = b.msg_id
LEFT JOIN model_pick mp ON mp.msg_id     = b.msg_id
WHERE  b.is_bookmark
ORDER  BY b.start_time ASC
LIMIT  $3 OFFSET $4`

	threadRows, err := r.langfuseDB.Query(threadQ, projectID, sessionID, messagesLimit, messagesOffset)
	if err != nil {
		return nil, fmt.Errorf("langfuse db: load thread: %w", err)
	}
	defer threadRows.Close()

	header.Thread = make([]ThreadMessage, 0, messagesLimit)
	windows := make([]messageWindow, 0, messagesLimit)
	// Conversation-search chunks per message, attached to each message's
	// conversation_search step after processing steps are populated below.
	chunksByMsg := make(map[string][]ConversationChunk)
	for threadRows.Next() {
		var (
			row          ThreadMessage
			windowStart  sql.NullTime
			started      sql.NullTime
			nextStart    sql.NullTime
			input        sql.NullString
			output       sql.NullString
			reason       sql.NullString
			planJSON     sql.NullString
			model        sql.NullString
			userMetaJSON sql.NullString
		)
		if err := threadRows.Scan(
			&row.MessageID,
			&row.ObservationID,
			&windowStart,
			&started,
			&nextStart,
			&input,
			&output,
			&reason,
			&planJSON,
			&model,
			&row.LatencyMs,
			&row.LLMCalls,
			&row.TotalTokens,
			&userMetaJSON,
		); err != nil {
			return nil, fmt.Errorf("langfuse db: scan thread row: %w", err)
		}
		// Parse cache-hit metadata the gateway wrote into user_metadata.
		if userMetaJSON.Valid && strings.TrimSpace(userMetaJSON.String) != "" {
			var um struct {
				AnsweredFrom       string              `json:"answered_from"`
				ConversationChunks []ConversationChunk `json:"conversation_chunks"`
			}
			if err := json.Unmarshal([]byte(userMetaJSON.String), &um); err != nil {
				slog.Debug("failed to parse conversation user_metadata", "message_id", row.MessageID, "err", err)
			} else {
				row.AnsweredFrom = um.AnsweredFrom
				if len(um.ConversationChunks) > 0 {
					chunksByMsg[row.MessageID] = um.ConversationChunks
				}
			}
		}
		if started.Valid {
			row.StartedAt = started.Time
		}
		if input.Valid {
			row.Input = input.String
		}
		if output.Valid {
			row.Output = output.String
		}
		if reason.Valid {
			row.Reasoning = reason.String
		}
		// `plan_json` arrives as the raw `metadata->>'plan'` text — a
		// JSON-encoded array of strings written by conversation_add_message
		// (`plan=["step 1", "step 2", ...]`). parsePlanSteps swallows
		// malformed payloads: the prose is independently useful and a
		// bad plan blob should never sink a whole turn's render.
		if planJSON.Valid {
			row.Plan = parsePlanSteps(planJSON.String)
		}
		if model.Valid {
			row.Model = model.String
		}
		header.Thread = append(header.Thread, row)
		if windowStart.Valid {
			var nextPtr *time.Time
			if nextStart.Valid {
				t := nextStart.Time
				nextPtr = &t
			}
			windows = append(windows, messageWindow{
				MessageID: row.MessageID,
				Start:     windowStart.Time,
				NextStart: nextPtr,
			})
		}
	}
	if err := threadRows.Err(); err != nil {
		return nil, fmt.Errorf("langfuse db: iterate thread rows: %w", err)
	}

	header.ThreadLimit = messagesLimit
	header.ThreadOffset = messagesOffset
	header.ThreadHasMore = len(header.Thread) == messagesLimit

	// Enable leading/trailing span capture only when this page actually
	// bounds the trace: the first window owns pre-first-bookmark spans only
	// when we start from offset 0, and the last window owns post-last-bookmark
	// spans only when this is the final page. Off-page windows must NOT sweep
	// up neighbouring turns' spans.
	if len(windows) > 0 {
		windows[0].CaptureLeading = messagesOffset == 0
		windows[len(windows)-1].CaptureTrailing = !header.ThreadHasMore
	}

	// Populate per-message persisted-memory counts and IDs from mem0 so
	// the thread chips reflect actual extracted rows (aligning with the
	// conversation-level MemoriesAdded metric), and the UI can deep-link
	// to the specific message that extracted a given memory by matching
	// the mem0 row's `id` against the per-message `MemoryIDs` slice.
	if len(header.Thread) > 0 {
		memRes, err := r.mem0PerMessageWindow(datapackID, sessionID, windows)
		if err != nil {
			return nil, err
		}
		agentRes, err := r.agentReasoningPerMessageWindow(projectID, sessionID, windows)
		if err != nil {
			return nil, err
		}
		for i := range header.Thread {
			messageID := header.Thread[i].MessageID
			header.Thread[i].MemoriesExtracted = memRes.counts[messageID]
			header.Thread[i].MemoryIDs = memRes.memoryIDs[messageID]
			header.Thread[i].MemoryTexts = memRes.memoryTexts[messageID]
			header.Thread[i].PromotedMemoryIDs = memRes.promotedMemoryIDs[messageID]
			header.Thread[i].PromotedMemoryTexts = memRes.promotedMemoryTexts[messageID]
			header.Thread[i].MemoryHits = agentRes.memoryHits[messageID]
			header.Thread[i].KBHits = agentRes.kbHits[messageID]
			header.Thread[i].ProcessingSteps = ensureLLMInvokeStep(agentRes.processingSteps[messageID], header.Thread[i].LLMCalls)
			// Attach conversation-search chunks to this message's
			// conversation_search step (or synthesize it if the span didn't land).
			if chunks, ok := chunksByMsg[messageID]; ok && len(chunks) > 0 {
				attached := false
				for j := range header.Thread[i].ProcessingSteps {
					if header.Thread[i].ProcessingSteps[j].Name == "conversation_search" {
						header.Thread[i].ProcessingSteps[j].ConversationChunks = chunks
						attached = true
						break
					}
				}
				if !attached {
					header.Thread[i].ProcessingSteps = append(header.Thread[i].ProcessingSteps, ProcessingStep{
						Name:               "conversation_search",
						Status:             "ok",
						Detail:             fmt.Sprintf("%d chunk(s)", len(chunks)),
						ParentSpanID:       kbSearchSpanID(header.Thread[i].ProcessingSteps),
						ConversationChunks: chunks,
					})
				}
			}
		}
	}

	return &header, nil
}

type memoryWindowResult struct {
	counts      map[string]int64
	memoryIDs   map[string][]string // messageID → []memoryID (real mem0 UUIDs)
	memoryTexts map[string][]string // messageID → []memory text (payload->>'data')
	// promoted* mirror the above for rows that have been promoted to the KB
	// (now living in datapacks.promoted_collection, evicted from mem0). Kept
	// separate so the live count stays "current memories only" while the
	// UI can still surface + deep-link promoted memories on the originating
	// turn, tagged as "Promoted to KB".
	promotedMemoryIDs   map[string][]string
	promotedMemoryTexts map[string][]string
}

// windowScanBounds returns the inclusive lower / exclusive upper time bounds
// for fetching only the observations the given windows can actually consume,
// so a per-message trace doesn't scan the whole conversation. The bounds key
// off the capture flags rather than the raw window edges:
//   - lower is nil when the first window captures leading (pre-first-bookmark)
//     spans — i.e. it's the genuine first turn of the trace — otherwise it's
//     the first window's Start.
//   - upper is nil when the last window captures trailing (post-last-bookmark)
//     spans, otherwise it's the last window's NextStart.
//
// A nil bound disables that side of the filter (`$n IS NULL OR ...`), so the
// full-thread query (which captures at both ends) reads the whole trace
// exactly as before.
func windowScanBounds(windows []messageWindow) (lower, upper *time.Time) {
	n := len(windows)
	if n == 0 {
		return nil, nil
	}
	if !windows[0].CaptureLeading {
		lower = &windows[0].Start
	}
	if !windows[n-1].CaptureTrailing {
		upper = windows[n-1].NextStart
	}
	return lower, upper
}

// attributeTimeToMessageWindow walks the ordered bookmark windows in
// lockstep with chronologically-sorted events and returns the message
// turn whose window contains `at`. `winIdx` is advanced in place so
// successive calls amortise to O(N+M) instead of O(N*M).
//
// Mirrors the windowing logic from the Langfuse-span attribution path:
// events firing before the first bookmark land on the first turn, and
// events firing after the last bookmark land on the last turn.
func attributeTimeToMessageWindow(windows []messageWindow, winIdx *int, at time.Time) string {
	for *winIdx < len(windows) {
		w := windows[*winIdx]
		if at.Before(w.Start) {
			if *winIdx == 0 && w.CaptureLeading {
				return w.MessageID
			}
			return ""
		}
		if w.NextStart != nil && !at.Before(*w.NextStart) {
			*winIdx++
			continue
		}
		return w.MessageID
	}
	if len(windows) > 0 && windows[len(windows)-1].CaptureTrailing {
		return windows[len(windows)-1].MessageID
	}
	return ""
}

// mem0PerMessageWindow attributes persisted mem0 rows to the message
// turn whose bookmark window they fall inside, using
// `meko_created_at` for chronological ordering. The returned
// `memoryIDs` are real mem0 UUIDs, so the UI can deep-link from the
// Memories tab via `?memoryId=<id>` and reverse-lookup the matching
// turn via `thread.message.memory_ids`.
//
// Counts only mem0 rows surfaced in the Memories tab — rows whose
// `meko_promotion_user_action` is not `keep_as_memory` (matches the
// filter in MemoriesRepo.ListByFilter, so the deep-link from a row
// the user can actually see always resolves).
// canonicalUUID normalizes a UUID that may arrive with or without dashes
// (Langfuse session ids are the conversation UUID with dashes stripped)
// into the canonical lowercase 8-4-4-4-12 dashed form. Returns "" when the
// input isn't a valid UUID, so callers can treat it as "no match" rather
// than feeding an invalid value into a ::uuid cast. uuid.Parse accepts both
// dashed and 32-char hex forms.
func canonicalUUID(s string) string {
	parsed, err := uuid.Parse(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return parsed.String()
}

func (r *LangfuseRepo) mem0PerMessageWindow(
	datapackID, sessionID string,
	windows []messageWindow,
) (memoryWindowResult, error) {
	empty := memoryWindowResult{
		counts:              map[string]int64{},
		memoryIDs:           map[string][]string{},
		memoryTexts:         map[string][]string{},
		promotedMemoryIDs:   map[string][]string{},
		promotedMemoryTexts: map[string][]string{},
	}
	if len(windows) == 0 {
		return empty, nil
	}
	// Compare conversation ids as native uuid = uuid. The previous
	// `REPLACE(meko_conversation_id::text,'-','') = $2` was non-sargable:
	// it cast + REPLACE'd the column for every scanned row and could never
	// use an index. Reconstructing the dashed UUID once here lets the
	// planner do a plain 16-byte uuid compare (and use an index on
	// meko_conversation_id if one is added).
	convUUID := canonicalUUID(sessionID)
	// Validate/canonicalize datapackID before binding it as a uuid: a
	// malformed value would otherwise fail the query at the DB layer.
	dpUUID := canonicalUUID(datapackID)
	if dpUUID == "" || convUUID == "" {
		return empty, nil
	}

	// Current mem0 memories + promoted-to-KB memories in one query via
	// UNION ALL. Each table is scanned exactly once and the DB concatenates
	// the two result sets (no dedup — that's the "ALL"), then a single
	// ORDER BY interleaves them so the lockstep window attribution below
	// walks every memory oldest-first. `promoted` tags which side a row
	// came from.
	//
	// e.g. mem0 yields [A@10:00 (promoted=false)] and promoted_collection
	// yields [B@10:05 (promoted=true)]; UNION ALL stacks them and ORDER BY
	// returns [A@10:00 false, B@10:05 true] — one pass over each table.
	// Same time-bounding as agentReasoningPerMessageWindow: a single-message
	// window only needs mem0 rows created within [Start, NextStart), so we
	// avoid scanning the whole conversation's memories per lazy trace fetch.
	// NULL bounds (full-thread query) keep the original "scan everything"
	// behaviour. Attribution below still drops anything outside the window,
	// so this only trims fetch cost.
	lower, upper := windowScanBounds(windows)

	const q = `
SELECT id::text AS memory_id,
       COALESCE(payload->>'data', '') AS memory_text,
       meko_created_at,
       false AS promoted
FROM   datapacks.meko_mem0_collection
WHERE  meko_datapack_id = $1::uuid
  AND  meko_conversation_id = $2::uuid
  AND  meko_promotion_user_action IS DISTINCT FROM 'keep_as_memory'::datapacks.meko_promotion_user_action
  AND  ($3::timestamptz IS NULL OR meko_created_at >= $3)
  AND  ($4::timestamptz IS NULL OR meko_created_at <  $4)
UNION ALL
SELECT id::text AS memory_id,
       COALESCE(payload->>'data', '') AS memory_text,
       meko_created_at,
       true AS promoted
FROM   datapacks.promoted_collection
WHERE  meko_datapack_id = $1::uuid
  AND  meko_conversation_id = $2::uuid
  AND  ($3::timestamptz IS NULL OR meko_created_at >= $3)
  AND  ($4::timestamptz IS NULL OR meko_created_at <  $4)
ORDER  BY meko_created_at ASC, memory_id ASC`

	rows, err := r.dataDB.Query(q, dpUUID, convUUID, lower, upper)
	if err != nil {
		return empty, fmt.Errorf("meta db: list mem0 rows for message windows: %w", err)
	}
	defer rows.Close()

	res := memoryWindowResult{
		counts:              make(map[string]int64, len(windows)),
		memoryIDs:           make(map[string][]string, len(windows)),
		memoryTexts:         make(map[string][]string, len(windows)),
		promotedMemoryIDs:   make(map[string][]string, len(windows)),
		promotedMemoryTexts: make(map[string][]string, len(windows)),
	}
	for _, w := range windows {
		res.counts[w.MessageID] = 0
		res.memoryIDs[w.MessageID] = nil
		res.memoryTexts[w.MessageID] = nil
		res.promotedMemoryIDs[w.MessageID] = nil
		res.promotedMemoryTexts[w.MessageID] = nil
	}

	winIdx := 0
	for rows.Next() {
		var memID, memText string
		var createdAt time.Time
		var promoted bool
		if err := rows.Scan(&memID, &memText, &createdAt, &promoted); err != nil {
			return empty, fmt.Errorf("meta db: scan mem0 row: %w", err)
		}
		messageID := attributeTimeToMessageWindow(windows, &winIdx, createdAt)
		if messageID == "" {
			continue
		}
		if promoted {
			res.promotedMemoryIDs[messageID] = append(res.promotedMemoryIDs[messageID], memID)
			if memText != "" {
				res.promotedMemoryTexts[messageID] = append(res.promotedMemoryTexts[messageID], memText)
			}
			continue
		}
		res.counts[messageID]++
		res.memoryIDs[messageID] = append(res.memoryIDs[messageID], memID)
		if memText != "" {
			res.memoryTexts[messageID] = append(res.memoryTexts[messageID], memText)
		}
	}
	if err := rows.Err(); err != nil {
		return empty, fmt.Errorf("meta db: iterate mem0 rows: %w", err)
	}
	return res, nil
}

type agentReasoningWindowResult struct {
	memoryHits      map[string][]MemoryHit
	kbHits          map[string][]KBHit
	processingSteps map[string][]ProcessingStep
}

type agentReasoningObservation struct {
	ID                  string
	ParentObservationID string
	Name                string
	Type                string
	StartTime           time.Time
	DurationMs          int64
	Input               string
	Output              string
}

// childSubTimingClass maps the name of a child observation to the parent
// step it belongs under and the key under which its duration should be
// rolled up in ProcessingStep.SubTimings. Returns ("", "") when the row
// is not a recognised sub-span.
//
// The three names come from:
//   - "Searching (yugabytedb / vector)"  — mem0/vector_stores/pgvector.py
//   - "Search Graph DB (mage / graph)" / "Search Graph DB (yugabytedb / graph)"
//     — mem0/memory/mage_memory.py and mem0/memory/apache_age_memory.py
//   - "Execute Query (yugabytedb)" — meko-mcp-server/src/tools.py
//     (used by knowledgebase_search; mem0's own "Execute Query
//     (yugabytedb / vector)" deliberately falls through here because
//     it nests inside the "Searching" span we already account for).
func childSubTimingClass(name string) (parentStep, subKey string) {
	n := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.Contains(n, "searching") && strings.Contains(n, "vector"):
		return "memory_search", "vector_ms"
	case strings.Contains(n, "search graph db"):
		return "memory_search", "graph_ms"
	case n == "execute query (yugabytedb)":
		return "knowledgebase_search", "vector_ms"
	}
	return "", ""
}

func (r *LangfuseRepo) agentReasoningPerMessageWindow(
	projectID, sessionID string,
	windows []messageWindow,
) (agentReasoningWindowResult, error) {
	emptyResult := agentReasoningWindowResult{
		memoryHits:      map[string][]MemoryHit{},
		kbHits:          map[string][]KBHit{},
		processingSteps: map[string][]ProcessingStep{},
	}
	if !r.LangfuseDBConfigured() {
		return emptyResult, ErrLangfuseDBNotConfigured
	}
	if len(windows) == 0 {
		return emptyResult, nil
	}

	// Time-bound the scan to exactly the turns this call needs. Derived from
	// the window capture flags: the full-thread query (offset 0, last page)
	// has leading+trailing capture on at its ends, so both bounds resolve to
	// NULL and we fetch the whole trace as before. A single-message trace for
	// a middle turn has both flags off, so we only read [Start, NextStart) —
	// turning the per-bubble lazy fetch from O(all spans in trace) into
	// O(spans in this turn) and avoiding the O(M^2) blow-up when every turn's
	// trace is opened in WorkbenchLM. Spans outside the bound would be dropped
	// by the attribution loop anyway, so this is purely a fetch-cost win.
	lower, upper := windowScanBounds(windows)

	// TODO(MEKO): Replace name ILIKE '%...%' matching below with a structured
	// step marker (metadata/tag/category) emitted at trace-write time.
	// Currently we use the name field for this, but this is not ideal for large tables and can degrade query performance.
	// Gemini comment link: https://github.com/yugabyte/meko/pull/233#discussion_r3264939913
	// The current fuzzy filters are robust across legacy naming variants but
	// are not ideal for large tables and can degrade query performance.
	const q = `
SELECT id,
       COALESCE(parent_observation_id, '') AS parent_observation_id,
       name,
       type::text,
       start_time,
       (EXTRACT(EPOCH FROM (COALESCE(end_time, start_time) - start_time))*1000)::bigint AS dur_ms,
       COALESCE(input::text, '') AS input,
       COALESCE(output::text, '') AS output
FROM   clickhouse.observations
WHERE  project_id = $1 AND trace_id = $2 AND is_deleted = false
  AND  ($3::timestamptz IS NULL OR start_time >= $3)
  AND  ($4::timestamptz IS NULL OR start_time <  $4)
  AND  (
        (type = 'TOOL' AND (
             name ILIKE '%memory%search%'
          OR name ILIKE '%knowledge%search%'
          OR name ILIKE '%knowledgebase%search%'
          OR name ILIKE '%memory%add%'
          OR name ILIKE '%conversation%add%message%'
        ))
        OR name ILIKE 'Conversation Message Added'
        -- Child spans rolled up into ProcessingStep.SubTimings. Kept in
        -- the same query (instead of a second round-trip) so we can
        -- attribute them by start_time within the same iteration.
        OR (name ILIKE '%searching%vector%')
        OR (name ILIKE '%search graph db%')
        OR name ILIKE 'Execute Query (yugabytedb)'
       )
ORDER  BY start_time ASC`

	rows, err := r.langfuseDB.Query(q, projectID, sessionID, lower, upper)
	if err != nil {
		return emptyResult, fmt.Errorf("langfuse db: load agent reasoning observations: %w", err)
	}
	defer rows.Close()

	res := agentReasoningWindowResult{
		memoryHits:      make(map[string][]MemoryHit, len(windows)),
		kbHits:          make(map[string][]KBHit, len(windows)),
		processingSteps: make(map[string][]ProcessingStep, len(windows)),
	}
	for _, w := range windows {
		res.memoryHits[w.MessageID] = nil
		res.kbHits[w.MessageID] = nil
		res.processingSteps[w.MessageID] = nil
	}

	// parentRef tracks the slice index + time window of a parent step
	// emitted into res.processingSteps[messageID]. When a child sub-span
	// arrives we walk this slice to find the parent whose [start,end)
	// covers the child's start_time and whose Name matches the expected
	// parent for the child's sub-timing class. Index-based (not pointer-
	// based) so it survives slice growth from append.
	type parentRef struct {
		name  string
		idx   int
		start time.Time
		end   time.Time
	}
	parentsByMessage := make(map[string][]parentRef, len(windows))

	assignChild := func(messageID string, obs agentReasoningObservation) {
		parentName, subKey := childSubTimingClass(obs.Name)
		if parentName == "" {
			return
		}
		parents := parentsByMessage[messageID]
		// Walk in reverse so we attribute to the most recent matching
		// parent — multiple memory.search calls in one turn each get
		// their own roll-up.
		for i := len(parents) - 1; i >= 0; i-- {
			p := parents[i]
			if p.name != parentName {
				continue
			}
			if obs.StartTime.Before(p.start) {
				continue
			}
			if !p.end.IsZero() && !obs.StartTime.Before(p.end) {
				continue
			}
			step := &res.processingSteps[messageID][p.idx]
			if step.SubTimings == nil {
				step.SubTimings = map[string]int64{}
			}
			step.SubTimings[subKey] += obs.DurationMs
			return
		}
	}

	assignParent := func(messageID string, obs agentReasoningObservation) {
		step := processingStepFromObservation(obs)
		if step == nil {
			return
		}
		switch step.Name {
		case "memory_search":
			hits := extractAgentMemoryHits(parseJSONValue(obs.Output))
			res.memoryHits[messageID] = append(res.memoryHits[messageID], hits...)
			step.Detail = hitDetail(len(hits))
		case "knowledgebase_search":
			hits := extractAgentKBHits(parseJSONValue(obs.Output))
			res.kbHits[messageID] = append(res.kbHits[messageID], hits...)
			step.Detail = hitDetail(len(hits))
		}

		// Conversation persistence can show up as both the MCP TOOL span and the
		// zero-duration bookmark span. Keep one row in the compact reasoning
		// timeline, preferring the longer TOOL duration when available. Per-
		// message step counts are tiny (≤ ~6), so a linear scan is cheaper than
		// maintaining a side index.
		if isDedupedStep(step.Name) {
			existing := res.processingSteps[messageID]
			for i := range existing {
				if existing[i].Name == step.Name {
					if step.DurationMs > existing[i].DurationMs {
						preserved := existing[i].SubTimings
						existing[i] = *step
						if step.SubTimings == nil {
							existing[i].SubTimings = preserved
						}
					}
					return
				}
			}
		}
		res.processingSteps[messageID] = append(res.processingSteps[messageID], *step)
		parentsByMessage[messageID] = append(parentsByMessage[messageID], parentRef{
			name:  step.Name,
			idx:   len(res.processingSteps[messageID]) - 1,
			start: obs.StartTime,
			end:   obs.StartTime.Add(time.Duration(obs.DurationMs) * time.Millisecond),
		})
	}

	assign := func(messageID string, obs agentReasoningObservation) {
		if parentName, _ := childSubTimingClass(obs.Name); parentName != "" {
			assignChild(messageID, obs)
			return
		}
		assignParent(messageID, obs)
	}

	winIdx := 0
	for rows.Next() {
		var obs agentReasoningObservation
		if err := rows.Scan(&obs.ID, &obs.ParentObservationID, &obs.Name, &obs.Type, &obs.StartTime, &obs.DurationMs, &obs.Input, &obs.Output); err != nil {
			return emptyResult, fmt.Errorf("langfuse db: scan agent reasoning observation: %w", err)
		}
		for winIdx < len(windows) {
			w := windows[winIdx]
			if obs.StartTime.Before(w.Start) {
				if winIdx == 0 && w.CaptureLeading {
					assign(w.MessageID, obs)
				}
				break
			}
			if w.NextStart != nil && !obs.StartTime.Before(*w.NextStart) {
				winIdx++
				continue
			}
			assign(w.MessageID, obs)
			break
		}
	}
	if err := rows.Err(); err != nil {
		return emptyResult, fmt.Errorf("langfuse db: iterate agent reasoning observations: %w", err)
	}
	r.resolveKBDocumentNames(res.kbHits)
	return res, nil
}

func (r *LangfuseRepo) resolveKBDocumentNames(kbHits map[string][]KBHit) {
	if r == nil || r.dataDB == nil || len(kbHits) == 0 {
		return
	}

	// Collect the distinct document ids of document-sourced hits. A single
	// document yields many chunks across many messages, so dedup here to keep
	// the IN-list (and the lookup query) small.
	uniqueIDs := make(map[string]struct{})
	for _, hits := range kbHits {
		for _, h := range hits {
			if h.Source == "memory" || h.DocumentID == "" {
				continue
			}
			uniqueIDs[h.DocumentID] = struct{}{}
		}
	}
	if len(uniqueIDs) == 0 {
		return
	}
	ids := make([]string, 0, len(uniqueIDs))
	for id := range uniqueIDs {
		ids = append(ids, id)
	}

	names := documentBasenamesByIDs(r.dataDB, ids)
	if len(names) == 0 {
		return
	}

	for msg := range kbHits {
		for i := range kbHits[msg] {
			if kbHits[msg][i].Source == "memory" {
				continue
			}
			if name, ok := names[kbHits[msg][i].DocumentID]; ok {
				kbHits[msg][i].DocumentName = name
			}
		}
	}
}

const (
	agentReasoningMaxItems       = 50
	agentReasoningMemoryMinScore = 0.3
	agentReasoningKBMaxDistance  = 1.2
)

// kbSearchSpanID returns the knowledgebase_search step's span id, so a
// synthesized conversation_search step can nest under it in the UI.
func kbSearchSpanID(steps []ProcessingStep) string {
	for i := range steps {
		if steps[i].Name == "knowledgebase_search" {
			return steps[i].SpanID
		}
	}
	return ""
}

func processingStepFromObservation(obs agentReasoningObservation) *ProcessingStep {
	name := strings.ToLower(strings.TrimSpace(obs.Name))
	stepName := ""
	switch {
	case strings.Contains(name, "memory") && strings.Contains(name, "search"):
		stepName = "memory_search"
	case strings.Contains(name, "knowledge") && strings.Contains(name, "search"):
		stepName = "knowledgebase_search"
	case strings.Contains(name, "conversation") && strings.Contains(name, "search"):
		stepName = "conversation_search"
	case strings.Contains(name, "conversation add message") || name == "conversation message added":
		stepName = "conversation_add_message"
	case strings.Contains(name, "memory add"):
		stepName = "memory_add"
	case strings.EqualFold(obs.Type, "GENERATION"):
		stepName = "llm_invoke"
	default:
		return nil
	}

	args, query := extractToolArgsAndQuery(parseJSONValue(obs.Input))
	return &ProcessingStep{
		Name:         stepName,
		DurationMs:   obs.DurationMs,
		Status:       "ok",
		Query:        query,
		ToolArgs:     args,
		SpanID:       obs.ID,
		ParentSpanID: obs.ParentObservationID,
	}
}

// ensureLLMInvokeStep guarantees the timeline has one llm_invoke row whenever
// we know at least one LLM call happened (llmCalls > 0). The agent-reasoning
// loader focuses on TOOL/bookmark observations and may not always surface a
// concrete GENERATION span in this step list, so we synthesize one fallback
// row from llmCalls for UI completeness.
func ensureLLMInvokeStep(steps []ProcessingStep, llmCalls int64) []ProcessingStep {
	if llmCalls <= 0 {
		return steps
	}
	for _, step := range steps {
		if step.Name == "llm_invoke" {
			return steps
		}
	}
	detail := ""
	if llmCalls > 1 {
		detail = fmt.Sprintf("%d calls", llmCalls)
	}
	llmStep := ProcessingStep{Name: "llm_invoke", Status: "ok", Detail: detail}
	out := make([]ProcessingStep, 0, len(steps)+1)
	inserted := false
	for _, step := range steps {
		if !inserted && (step.Name == "conversation_add_message" || step.Name == "memory_add") {
			out = append(out, llmStep)
			inserted = true
		}
		out = append(out, step)
	}
	if !inserted {
		out = append(out, llmStep)
	}
	return out
}

func hitDetail(n int) string {
	if n == 1 {
		return "1 hit"
	}
	return fmt.Sprintf("%d hits", n)
}

// isDedupedStep reports whether a step name should be collapsed when the same
// pipeline stage shows up more than once for one message turn. Currently only
// conversation_add_message is affected: it can surface as both the MCP TOOL
// span and the zero-duration bookmark span, and we want a single row in the
// timeline. Extend with more names if other steps grow the same pattern.
func isDedupedStep(name string) bool {
	return name == "conversation_add_message"
}

// Recursively parse a JSON string into a generic interface.
func parseJSONValue(raw string) interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		if s, ok := v.(string); ok {
			// Langfuse can store JSON payloads as JSON-encoded strings.
			// If the inner value isn't itself JSON, fall back to the
			// decoded plain string so callers still see the content.
			if next := parseJSONValue(s); next != nil {
				return next
			}
			return s
		}
		return v
	}
	return nil
}

func extractToolArgsAndQuery(v interface{}) (map[string]interface{}, string) {
	args := objectFromValue(v)
	if len(args) == 0 {
		return nil, ""
	}
	query, _ := args["query"].(string)
	delete(args, "query")
	if len(args) == 0 {
		args = nil
	}
	return args, query
}

func objectFromValue(v interface{}) map[string]interface{} {
	obj, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	for _, key := range []string{"args", "input", "arguments", "kwargs"} {
		if nested, ok := obj[key].(map[string]interface{}); ok {
			return cloneStringMap(nested)
		}
	}
	return cloneStringMap(obj)
}

func cloneStringMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func extractAgentMemoryHits(v interface{}) []MemoryHit {
	var vectorItems, relations, promoted []interface{}
	switch payload := v.(type) {
	case map[string]interface{}:
		for _, key := range []string{"results", "memories", "data"} {
			if items, ok := payload[key].([]interface{}); ok {
				vectorItems = items
				break
			}
		}
		if items, ok := payload["relations"].([]interface{}); ok {
			relations = items
		}
		if items, ok := payload["promoted_relations"].([]interface{}); ok {
			promoted = items
		}
	case []interface{}:
		vectorItems = payload
	}

	memHits := make([]MemoryHit, 0, len(vectorItems))
	for _, item := range firstN(vectorItems, agentReasoningMaxItems) {
		switch x := item.(type) {
		case map[string]interface{}:
			score, hasScore := numberField(x, "score")
			if hasScore && score < agentReasoningMemoryMinScore {
				continue
			}
			text := firstStringField(x, "memory", "data", "text", "content")
			if text == "" {
				continue
			}
			hit := MemoryHit{Kind: "mem", Text: text, CharCount: utf8.RuneCountInString(text)}
			if hasScore {
				hit.Score = &score
			}
			memHits = append(memHits, hit)
		case string:
			if x != "" {
				memHits = append(memHits, MemoryHit{Kind: "mem", Text: x, CharCount: utf8.RuneCountInString(x)})
			}
		}
	}
	sort.SliceStable(memHits, func(i, j int) bool {
		return scoreOrZero(memHits[i].Score) > scoreOrZero(memHits[j].Score)
	})

	out := make([]MemoryHit, 0, len(memHits)+len(relations)+len(promoted))
	out = append(out, memHits...)
	for _, item := range firstN(relations, agentReasoningMaxItems) {
		if triple := formatRelationTriple(item); triple != "" {
			out = append(out, MemoryHit{Kind: "graph", Text: triple, CharCount: utf8.RuneCountInString(triple)})
		}
	}
	for _, item := range firstN(promoted, agentReasoningMaxItems) {
		if triple := formatRelationTriple(item); triple != "" {
			out = append(out, MemoryHit{Kind: "promoted", Text: triple, CharCount: utf8.RuneCountInString(triple)})
		}
	}
	return out
}

func extractAgentKBHits(v interface{}) []KBHit {
	var items []interface{}
	switch payload := v.(type) {
	case map[string]interface{}:
		for _, key := range []string{"results", "matches", "documents", "data"} {
			if candidate, ok := payload[key].([]interface{}); ok {
				items = candidate
				break
			}
		}
	case []interface{}:
		items = payload
	}

	hits := make([]KBHit, 0, len(items))
	for _, item := range firstN(items, agentReasoningMaxItems) {
		switch x := item.(type) {
		case map[string]interface{}:
			distance, hasDistance := numberField(x, "distance")
			if hasDistance && distance > agentReasoningKBMaxDistance {
				continue
			}
			text := firstStringField(x, "chunk_text", "text", "content", "snippet", "chunk")
			if text == "" {
				continue
			}
			docRef := firstStringField(x, "document_name", "source", "source_uri", "document_id")
			if docRef != "" {
				docRef = path.Base(docRef)
			}
			hit := KBHit{DocumentID: docRef, ChunkText: text, CharCount: utf8.RuneCountInString(text)}
			// metadata_filters distinguishes promoted-memory hits (source ==
			// "memory") from document chunks (no source). The UI labels them
			// differently.
			if mf, ok := x["metadata_filters"].(map[string]interface{}); ok {
				if s, ok := mf["source"].(string); ok {
					hit.Source = s
				}
			}
			if hasDistance {
				d := distance
				score := 1 - distance
				if score < 0 {
					score = 0
				}
				if score > 1 {
					score = 1
				}
				hit.Distance = &d
				hit.Score = &score
			}
			hits = append(hits, hit)
		case string:
			if x != "" {
				hits = append(hits, KBHit{ChunkText: x, CharCount: utf8.RuneCountInString(x)})
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		return distanceOrInf(hits[i].Distance) < distanceOrInf(hits[j].Distance)
	})
	return hits
}

func firstN(items []interface{}, n int) []interface{} {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

// parsePlanSteps decodes the `metadata->>'plan'` payload written by
// conversation_add_message. Expects a JSON-encoded `[]string`; returns
// nil for empty / malformed / non-string-array inputs so a single bad
// row never sinks a turn's render. Trimmed; empty steps are dropped to
// match the MCP server's pre-persist filtering.
func parsePlanSteps(raw string) []string {
	if raw == "" {
		return nil
	}
	var steps []string
	if err := json.Unmarshal([]byte(raw), &steps); err != nil {
		return nil
	}
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func numberField(obj map[string]interface{}, key string) (float64, bool) {
	switch v := obj[key].(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func firstStringField(obj map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if s, ok := obj[key].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func formatRelationTriple(item interface{}) string {
	obj, ok := item.(map[string]interface{})
	if !ok {
		return ""
	}
	source := firstStringField(obj, "source", "subject")
	target := firstStringField(obj, "destination", "target", "object")
	relation := firstStringField(obj, "relationship", "relation", "predicate")
	if source == "" || target == "" || relation == "" {
		return ""
	}
	return source + " " + relation + " " + target
}

func scoreOrZero(score *float64) float64 {
	if score == nil {
		return 0
	}
	return *score
}

func distanceOrInf(distance *float64) float64 {
	if distance == nil {
		return 1e9
	}
	return *distance
}

// MemoriesPerConversation returns persisted-mem0 row counts for the
// given Langfuse session_ids (= conversation_ids), scoped to the
// datapack. Used by LangfuseService to overlay an authoritative
// "memories" count onto results coming back from
// ListConversationsForProject / GetConversationThread, so the UI shows
// the same number the Memories tab does instead of the inflated
// Memory Add tool-invocation count from clickhouse.observations.
//
// Counts only mem0 rows surfaced in the Memories tab — i.e. rows whose
// meko_promotion_user_action is not 'keep_as_memory' (matches the
// filter in MemoriesRepo.ListByFilter).
//
// Result is keyed by the unhyphenated session_id form used everywhere
// else in this repo (so callers can index directly without normalizing).
// Sessions with no persisted mem0 rows are simply absent from the map;
// callers should default to 0. Empty input returns an empty map and no
// error. Lives on LangfuseRepo (not MemoriesRepo) because the cross-DB
// fan-out is a langfuse-listing concern; MemoriesRepo continues to own
// the canonical Memories tab read path.
func (r *LangfuseRepo) MemoriesPerConversation(
	projectID string,
	conversationIDs []string,
) (map[string]int64, error) {
	if !r.LangfuseDBConfigured() {
		return nil, ErrLangfuseDBNotConfigured
	}
	if len(conversationIDs) == 0 {
		return map[string]int64{}, nil
	}

	stripped := make([]string, 0, len(conversationIDs))
	for _, id := range conversationIDs {
		id = strings.ReplaceAll(strings.TrimSpace(id), "-", "")
		if id == "" {
			continue
		}
		stripped = append(stripped, id)
	}
	if len(stripped) == 0 {
		return map[string]int64{}, nil
	}

	// Sum extracted-memory counts per trace. Each 'Memory Add' tool call
	// in mem0 can extract multiple memories at once (returned as the
	// `results` array in the output payload), so we count those array
	// entries instead of doing COUNT(*) on tool calls — that mismatch is
	// what made the conversation badge disagree with the trace drawer.
	//
	// Same guarded JSON cast as memoriesPerMessageWindow: only attempt
	// the jsonb conversion when output looks like a `{...}` object that
	// mentions `"results"`; everything else falls through to count = 1.
	const q = `
SELECT trace_id,
       SUM(
         GREATEST(
           COALESCE(
             CASE WHEN btrim(output::text) LIKE '{%}'
                   AND output::text LIKE '%"results"%'
                  THEN jsonb_array_length((output::text)::jsonb -> 'results')
                  ELSE NULL
             END,
             1
           ),
           1
         )
       )::bigint AS memories
FROM   clickhouse.observations
WHERE  project_id = $1
  AND  trace_id = ANY($2::text[])
  AND  is_deleted = false
  AND  type = 'TOOL'
  AND  name ILIKE '%memory add%'
  AND  output::text IS NOT NULL
  AND  output::text <> ''
  -- Count any add whose results carry memory events (ADD or UPDATE).
  -- We intentionally do NOT also require graph relations
  -- (added_entities): an update to an existing memory has no new
  -- relations but is still a meaningful memory operation to surface.
  AND  output::text NOT LIKE '%"results": []%'
GROUP  BY trace_id`

	rows, err := r.langfuseDB.Query(q, projectID, pq.Array(stripped))
	if err != nil {
		return nil, fmt.Errorf("langfuse db: count meaningful memories per conversation: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int64, len(stripped))
	for rows.Next() {
		var (
			id  string
			cnt int64
		)
		if err := rows.Scan(&id, &cnt); err != nil {
			return nil, fmt.Errorf("langfuse db: scan memory count: %w", err)
		}
		out[id] = cnt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("langfuse db: iterate memory counts: %w", err)
	}
	return out, nil
}

// GetMessageTrace returns the windowed observation tree for one
// message — the message bookmark span plus everything between its
// start_time and the next message's start_time. Used by the trace
// drawer in the conversation viewer.
//
//	$1 = langfuse project_id
//	$2 = session_id (= trace_id)
//	$3 = message_id (from observation.metadata.message_id)
//
// Returns (nil, nil) when no message with that id exists in the
// trace; callers translate to 404.
func (r *LangfuseRepo) GetMessageTrace(
	projectID, sessionID, messageID string,
) ([]TraceSpan, error) {
	if !r.LangfuseDBConfigured() {
		return nil, ErrLangfuseDBNotConfigured
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if strings.TrimSpace(messageID) == "" {
		return nil, fmt.Errorf("message_id is required")
	}

	// Spans are attributed to the *next* Conversation Message Added bookmark
	// (the bookmark is written at the end of a turn, so all the agent's work
	// happens before it). For target message $3, the owning window is
	// (prev_bookmark_time, target_bookmark_time]; if $3 is the last bookmark,
	// any tail observations also belong to it.
	const q = `
WITH bookmarks AS (
    SELECT (metadata ->> 'message_id') AS msg_id,
           start_time AS bm_time,
           LAG(start_time) OVER (ORDER BY start_time) AS prev_bm_time,
           (ROW_NUMBER() OVER (ORDER BY start_time DESC) = 1) AS is_last
    FROM   clickhouse.observations
    WHERE  project_id = $1 AND trace_id = $2 AND is_deleted = false
      AND  name = 'Conversation Message Added'
      AND  (metadata ->> 'message_id') IS NOT NULL
), bounds AS (
    SELECT bm_time, prev_bm_time, is_last FROM bookmarks WHERE msg_id = $3
), windowed AS (
    SELECT o.id, o.parent_observation_id, o.name, o.type, o.start_time, o.end_time,
           o.provided_model_name, o.usage_details, o.total_cost, o.output, o.input
    FROM   clickhouse.observations o, bounds b
    WHERE  o.project_id = $1 AND o.trace_id = $2 AND o.is_deleted = false
      AND  ((o.start_time <= b.bm_time AND (b.prev_bm_time IS NULL OR o.start_time > b.prev_bm_time))
           OR (b.is_last AND o.start_time > b.bm_time))
), root AS (
    SELECT MIN(start_time) AS root_start FROM windowed
)
SELECT o.id, COALESCE(o.parent_observation_id, ''), o.name, o.type::text AS type,
       (EXTRACT(EPOCH FROM (COALESCE(o.end_time, o.start_time) - o.start_time))*1000)::bigint AS dur_ms,
       (EXTRACT(EPOCH FROM (o.start_time - r.root_start))*1000)::bigint AS offset_ms,
       COALESCE(o.provided_model_name, '') AS model,
       CASE WHEN o.usage_details::text = '{}' THEN '' ELSE o.usage_details::text END AS usage,
       COALESCE(o.total_cost::text, '') AS total_cost,
       -- Attach the memory output whenever results carry events (ADD or
       -- UPDATE). We do NOT also require added_entities (new graph
       -- relations): an update to an existing memory has none but should
       -- still show its memory card in the trace.
       CASE WHEN (o.type = 'TOOL' AND o.name ILIKE '%memory add%'
                  AND o.output::text NOT LIKE '%"results": []%')
            THEN COALESCE(o.output::text, '') ELSE '' END AS output,
       -- Statement text for DB-execution spans. mem0 / meko-mcp-server
       -- trace each cursor call via @observe, which records the function
       -- args as input: {"args": [<cursor>, "<SQL>", [params]], ...} for
       -- vector and {"args": ["<Cypher>", {params}], ...} for graph. Pull
       -- the first string element (the statement); never the params
       -- (embedding vectors are megabytes). Guard on a leading '{' so a
       -- non-JSON input never aborts the whole query via a failed ::jsonb
       -- cast.
       --
       -- DB-execution spans are declared via @observe(name="Execute ...")
       -- across mem0 / meko-mcp-server. Rather than pin an exact list that
       -- silently drops the statement whenever a new variant is added, we
       -- wildcard-match any "Execute ..." span name and let the input guard
       -- (args object with a string element) decide whether a statement is
       -- actually present. Known names at time of writing:
       --   meko-mem0/mem0/vector_stores/pgvector.py
       --     - "Execute Query (yugabytedb / vector)"             (:97)
       --     - "Execute Query Many (yugabytedb / vector)"        (:106)
       --     - "Execute Query Values (yugabytedb / vector)"      (:112)
       --   meko-mem0/mem0/memory/mage_memory.py
       --     - "Execute Query (mage / graph)"                    (:224)
       --     - "Execute Cypher Query (mage / graph)"             (:328)
       --     - "Execute Cypher With Tenant Filter (mage / graph)"(:417)
       --   meko-mem0/mem0/memory/apache_age_memory.py
       --     - "Execute Query (yugabytedb / graph)"              (:224)
       --     - "Execute Cypher Query (yugabytedb / graph)"       (:328)
       --     - "Execute Cypher With Tenant Filter (yugabytedb / graph)" (:417)
       --   meko-mcp-server/src/tools.py
       --     - "Execute Query (yugabytedb)"                      (:207)
       CASE WHEN (o.name ILIKE 'Execute Query%' OR o.name ILIKE 'Execute Cypher%')
                  AND o.input IS NOT NULL AND left(o.input::text, 1) = '{'
            THEN COALESCE((
                   SELECT value #>> '{}'
                   FROM   jsonb_array_elements((o.input::text)::jsonb -> 'args')
                            WITH ORDINALITY AS t(value, ord)
                   WHERE  jsonb_typeof(value) = 'string'
                   ORDER  BY ord
                   LIMIT  1
                 ), '')
            ELSE '' END AS input
FROM   windowed o, root r
ORDER  BY o.start_time ASC`

	rows, err := r.langfuseDB.Query(q, projectID, sessionID, messageID)
	if err != nil {
		return nil, fmt.Errorf("langfuse db: load message trace: %w", err)
	}
	defer rows.Close()

	out := make([]TraceSpan, 0, 64)
	for rows.Next() {
		var (
			span     TraceSpan
			durMs    sql.NullInt64
			offsetMs sql.NullInt64
		)
		if err := rows.Scan(
			&span.ID,
			&span.ParentObservationID,
			&span.Name,
			&span.Type,
			&durMs,
			&offsetMs,
			&span.Model,
			&span.Usage,
			&span.TotalCost,
			&span.Output,
			&span.Input,
		); err != nil {
			return nil, fmt.Errorf("langfuse db: scan trace span: %w", err)
		}
		if durMs.Valid {
			span.DurationMs = durMs.Int64
		}
		if offsetMs.Valid {
			span.OffsetMs = offsetMs.Int64
		}
		out = append(out, span)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("langfuse db: iterate trace rows: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// GetMessageDecisionTrace returns the slim per-turn payload for one
// message — the targeted-SQL counterpart of GetConversationThread that
// only computes one window's stats instead of N. Reuses the same
// (prev_bm, target_bm] windowing semantics as the thread query, then
// reuses the existing memoriesPerMessageWindow /
// agentReasoningPerMessageWindow helpers with a single-element slice
// to enrich memory IDs / memory hits / kb hits / processing steps.
//
// Returns (nil, nil) when the message_id isn't a bookmark in this
// trace; callers translate to 404 so live-chat clients can poll with
// backoff.
func (r *LangfuseRepo) GetMessageDecisionTrace(
	projectID, datapackID, sessionID, messageID string,
) (*MessageDecisionTrace, error) {
	if !r.LangfuseDBConfigured() {
		return nil, ErrLangfuseDBNotConfigured
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	if strings.TrimSpace(datapackID) == "" {
		return nil, fmt.Errorf("datapack_id is required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if strings.TrimSpace(messageID) == "" {
		return nil, fmt.Errorf("message_id is required")
	}

	// Same CTE chain as GetConversationThread, but `bookmarks` is
	// filtered to the target msg_id so windowed/turn_stats/reasoning/
	// model_pick all aggregate over a single window. next_root yields
	// the next window's earliest observation start_time so the
	// existing memoriesPerMessageWindow /
	// agentReasoningPerMessageWindow helpers can correctly drop spans
	// that belong to the next message when called with a single-
	// element windows slice.
	const q = `
WITH raw_obs AS (
    SELECT id, parent_observation_id, type, name, start_time, end_time,
           output::text AS output,
           usage_details,
           metadata ->> 'message_id' AS msg_id,
           CASE WHEN metadata ? 'plan' THEN metadata ->> 'plan' END AS plan_json,
           provided_model_name,
           -- Whole user_metadata blob as text; parsed in Go (robust to how
           -- Langfuse nests it — avoids depending on 2-level JSON access).
           metadata ->> 'user_metadata' AS user_metadata_json,
           (metadata ->> 'message_id' IS NOT NULL AND name = 'Conversation Message Added') AS is_bookmark
    FROM   clickhouse.observations
    WHERE  project_id = $1 AND trace_id = $2 AND is_deleted = false
), bookmark_bounds AS (
    SELECT msg_id, id AS bookmark_id, start_time AS bm_time,
           user_metadata_json,
           LAG(start_time)  OVER (ORDER BY start_time)      AS prev_bm_time,
           (ROW_NUMBER() OVER (ORDER BY start_time DESC) = 1) AS is_last
    FROM   raw_obs
    WHERE  is_bookmark
), target AS (
    SELECT * FROM bookmark_bounds WHERE msg_id = $3
), windowed AS (
    SELECT o.type, o.name, o.start_time, o.end_time,
           o.usage_details, o.provided_model_name
    FROM   raw_obs o, target t
    WHERE  (o.start_time <= t.bm_time AND (t.prev_bm_time IS NULL OR o.start_time > t.prev_bm_time))
        OR (t.is_last AND o.start_time > t.bm_time)
), turn_stats AS (
    SELECT MIN(start_time) AS root_start,
           COALESCE((EXTRACT(EPOCH FROM (MAX(COALESCE(end_time, start_time)) - MIN(start_time))) * 1000)::bigint, 0) AS latency_ms,
           COALESCE(SUM(
               CASE WHEN type = 'GENERATION' AND usage_details ? 'total'
                    THEN (usage_details ->> 'total')::bigint
                    ELSE 0 END
           ), 0) AS total_tokens,
           COUNT(*) FILTER (WHERE type = 'GENERATION') AS llm_calls
    FROM   windowed
), reasoning AS (
    -- Same parent_observation_id attribution rule as the thread query
    -- (Reasoning starts a few ms after its parent bookmark, so a
    -- start_time-only window misclassifies it).
    SELECT STRING_AGG(r.output, E'\n\n' ORDER BY r.start_time) AS thinking,
           (array_agg(r.plan_json ORDER BY r.start_time)
                FILTER (WHERE r.plan_json IS NOT NULL))[1] AS plan_json
    FROM   raw_obs r, target t
    WHERE  r.parent_observation_id = t.bookmark_id
      AND  r.type = 'GENERATION' AND r.name = 'Reasoning'
), model_pick AS (
    -- Provider-agnostic — any real LLM GENERATION in the window.
    SELECT (array_agg(provided_model_name ORDER BY start_time)
              FILTER (WHERE type = 'GENERATION'
                AND name NOT IN ('Reasoning', 'CostUsage Probe')
                AND provided_model_name IS NOT NULL
                AND provided_model_name <> ''))[1] AS model
    FROM   windowed
), next_root AS (
    -- Earliest observation start_time of the next window — used as
    -- NextStart in the messageWindow handed to the helper queries.
    -- NULL when the target is the last bookmark in the trace.
    SELECT MIN(o.start_time) AS next_root_start
    FROM   raw_obs o, target t
    WHERE  NOT t.is_last AND o.start_time > t.bm_time
)
SELECT t.msg_id,
       t.bookmark_id,
       t.bm_time,
       s.root_start,
       n.next_root_start,
       (t.prev_bm_time IS NULL) AS is_first,
       t.is_last AS is_last,
       COALESCE(r.thinking, '') AS reasoning,
       r.plan_json AS plan_json,
       mp.model AS model,
       COALESCE(s.latency_ms, 0)   AS latency_ms,
       COALESCE(s.llm_calls, 0)    AS llm_calls,
       COALESCE(s.total_tokens, 0) AS total_tokens,
       t.user_metadata_json AS user_metadata_json
FROM   target t
LEFT JOIN turn_stats s  ON true
LEFT JOIN reasoning  r  ON true
LEFT JOIN model_pick mp ON true
LEFT JOIN next_root  n  ON true`

	var (
		msgID         string
		bookmarkID    string
		bmTime        time.Time
		rootStart     sql.NullTime
		nextRootStart sql.NullTime
		isFirst       bool
		isLast        bool
		reasoningStr  string
		planJSON      sql.NullString
		model         sql.NullString
		latencyMs     int64
		llmCalls      int64
		totalTokens   int64
		userMetaJSON  sql.NullString
	)
	err := r.langfuseDB.QueryRow(q, projectID, sessionID, messageID).Scan(
		&msgID, &bookmarkID, &bmTime, &rootStart, &nextRootStart,
		&isFirst, &isLast,
		&reasoningStr, &planJSON, &model,
		&latencyMs, &llmCalls, &totalTokens,
		&userMetaJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("langfuse db: load message decision trace: %w", err)
	}

	slog.Debug("decision-trace user_metadata",
		"message_id", msgID,
		"user_metadata_present", userMetaJSON.Valid,
		"user_metadata", func() string {
			if userMetaJSON.Valid {
				return userMetaJSON.String
			}
			return "<null>"
		}())

	// Hand a single-element windows slice to the existing helpers —
	// they iterate spans by start_time and assign to the matching
	// window, so a one-window call only attributes the target turn's
	// rows. Helpers still scan the whole trace's name-matched
	// observations, but the attribution loop is O(spans) regardless of
	// thread length.
	winStart := bmTime
	if rootStart.Valid {
		winStart = rootStart.Time
	}
	var nextPtr *time.Time
	if nextRootStart.Valid {
		t := nextRootStart.Time
		nextPtr = &t
	}
	windows := []messageWindow{{
		MessageID: msgID,
		Start:     winStart,
		NextStart: nextPtr,
		// Only sweep up out-of-window spans when the target turn genuinely
		// bounds the trace. Without these guards, a single-message trace for
		// turn N pulls in every earlier turn's kb/memory search spans (the
		// 50 → 100 → 150 chunk inflation seen in WorkbenchLM).
		CaptureLeading:  isFirst,
		CaptureTrailing: isLast,
	}}

	memRes, err := r.mem0PerMessageWindow(datapackID, sessionID, windows)
	if err != nil {
		return nil, err
	}
	agentRes, err := r.agentReasoningPerMessageWindow(projectID, sessionID, windows)
	if err != nil {
		return nil, err
	}

	out := &MessageDecisionTrace{
		MessageID:         msgID,
		LatencyMs:         latencyMs,
		TotalTokens:       totalTokens,
		LLMCalls:          llmCalls,
		MemoriesExtracted: memRes.counts[msgID],
		Reasoning:         reasoningStr,
		MemoryHits:        agentRes.memoryHits[msgID],
		KBHits:            agentRes.kbHits[msgID],
		ProcessingSteps:   ensureLLMInvokeStep(agentRes.processingSteps[msgID], llmCalls),
	}
	if model.Valid {
		out.Model = model.String
	}
	// Parse the cache-hit metadata the gateway wrote into the bookmark's
	// user_metadata: answered_from and the retrieved chunks.
	if userMetaJSON.Valid && strings.TrimSpace(userMetaJSON.String) != "" {
		var um struct {
			AnsweredFrom         string              `json:"answered_from"`
			ConversationChunks   []ConversationChunk `json:"conversation_chunks"`
			SummaryModel         string              `json:"summary_model"`
			SourceConversationID string              `json:"source_conversation_id"`
		}
		if err := json.Unmarshal([]byte(userMetaJSON.String), &um); err != nil {
			slog.Debug("failed to parse conversation user_metadata", "message_id", msgID, "err", err)
		} else {
			out.AnsweredFrom = um.AnsweredFrom
			// Attach chunks to the conversation_search step (their semantic
			// owner) so the UI can expand that step to show + link them.
			if len(um.ConversationChunks) > 0 {
				attached := false
				for i := range out.ProcessingSteps {
					if out.ProcessingSteps[i].Name == "conversation_search" {
						out.ProcessingSteps[i].ConversationChunks = um.ConversationChunks
						attached = true
						break
					}
				}
				// MCP's conversation_search span emit is best-effort; if it
				// didn't land as a step, synthesize one so chunks still surface.
				if !attached {
					out.ProcessingSteps = append(out.ProcessingSteps, ProcessingStep{
						Name:               "conversation_search",
						Status:             "ok",
						Detail:             fmt.Sprintf("%d chunk(s)", len(um.ConversationChunks)),
						ParentSpanID:       kbSearchSpanID(out.ProcessingSteps),
						ConversationChunks: um.ConversationChunks,
					})
				}
			}
			// Synthesize the conversation_summarize step (Execute phase) so a
			// loaded conversation shows which small model summarized which
			// conversation — the gateway's live step isn't a Langfuse span.
			if um.SummaryModel != "" {
				out.ProcessingSteps = append(out.ProcessingSteps, ProcessingStep{
					Name:   "conversation_summarize",
					Status: "ok",
					// tool_args (not Detail) so the UI renders the bespoke
					// "Used <model> for summarizing conversation <link>" line.
					ToolArgs: map[string]interface{}{
						"summary_model":   um.SummaryModel,
						"conversation_id": um.SourceConversationID,
					},
				})
			}
		}
	}
	if planJSON.Valid {
		out.Plan = parsePlanSteps(planJSON.String)
	}
	return out, nil
}

type traceMemoryMetrics struct {
	MemoryID      string
	Vector        int64
	Entities      int64
	Relations     int64
	EntityNames   []string
	RelationNames []string
}

type traceMemoryParsedItem struct {
	MemoryID string
	Text     string
}

// EnrichTraceMemoryItems expands memory span output into per-memory items and
// annotates each item with vector / entity / relation counts pulled from
// `datapacks.meko_mem0_collection` and `meko_multi_tenant_graph`.
//
// Performance: this used to issue one DB round-trip per memory item, which
// landed as N+1 on traces with multiple memory_add calls (each running the
// same heavy graph join). We now do at most two batched queries regardless
// of memory count — one keyed by mem0 row id (the modern structured-output
// path) and one keyed by normalized memory text (legacy fallback for spans
// that don't surface an id). Most traces only need the by-id query; the
// by-text path is skipped entirely when every item carries an id.
func (r *LangfuseRepo) EnrichTraceMemoryItems(
	datapackID, conversationID string,
	spans []TraceSpan,
) ([]TraceSpan, error) {
	if r == nil || r.dataDB == nil || len(spans) == 0 {
		return spans, nil
	}
	convID := strings.ReplaceAll(strings.TrimSpace(conversationID), "-", "")
	if strings.TrimSpace(datapackID) == "" || convID == "" {
		return spans, nil
	}

	out := make([]TraceSpan, len(spans))
	copy(out, spans)

	// itemAddr points into perSpanItems so the resolved metrics can be
	// stitched back to the right (span, item) slot in a single pass at the
	// end. Each parsed memory either gets a mem0 row id (refsByID) or a
	// normalized-text + occurrence-offset bucket (textRefs).
	type itemAddr struct{ span, item int }
	type textRef struct {
		key    string
		offset int
		addr   itemAddr
	}

	perSpanItems := make([][]TraceMemoryItem, len(out))
	refsByID := make(map[string][]itemAddr)
	textCursor := make(map[string]int, 16)
	var textRefs []textRef

	// Pass 1 — parse every span, bucket each item by its lookup key.
	for i := range out {
		parsedItems := parseTraceMemoryItems(out[i].Output)
		if len(parsedItems) == 0 {
			continue
		}
		items := make([]TraceMemoryItem, len(parsedItems))
		for j, parsed := range parsedItems {
			items[j] = TraceMemoryItem{Text: parsed.Text}
			addr := itemAddr{span: i, item: j}
			if parsed.MemoryID != "" {
				refsByID[parsed.MemoryID] = append(refsByID[parsed.MemoryID], addr)
				continue
			}
			key := normalizeTraceMemoryText(parsed.Text)
			if key == "" {
				continue
			}
			offset := textCursor[key]
			textCursor[key] = offset + 1
			textRefs = append(textRefs, textRef{key: key, offset: offset, addr: addr})
		}
		perSpanItems[i] = items
	}

	// Pass 2a — one batched query for every mem0 row id we saw.
	if len(refsByID) > 0 {
		ids := make([]string, 0, len(refsByID))
		for id := range refsByID {
			ids = append(ids, id)
		}
		metricsByID, err := r.batchLookupTraceMemoryMetricsByIDs(datapackID, convID, ids)
		if err != nil {
			return spans, err
		}
		// IDs absent from mem0 may have been promoted to the KB — flag those
		// so the UI renders them as "Promoted to KB" instead of showing
		// misleading 0/0/0 counts (the mem0 anchor row no longer exists).
		missingIDs := make([]string, 0)
		for id := range refsByID {
			if metricsByID[id] == nil {
				missingIDs = append(missingIDs, id)
			}
		}
		promotedSet, err := r.batchLookupPromotedMemoryIDs(datapackID, convID, missingIDs)
		if err != nil {
			return spans, err
		}
		for id, addrs := range refsByID {
			if m := metricsByID[id]; m != nil {
				for _, a := range addrs {
					applyTraceMemoryMetrics(&perSpanItems[a.span][a.item], m)
				}
				continue
			}
			if promotedSet[id] {
				for _, a := range addrs {
					perSpanItems[a.span][a.item].Promoted = true
					perSpanItems[a.span][a.item].MemoryID = id
				}
			}
		}
	}

	// Pass 2b — one batched query for every (text, occurrence) we saw.
	if len(textRefs) > 0 {
		keys := make([]string, len(textRefs))
		offsets := make([]int, len(textRefs))
		for i, t := range textRefs {
			keys[i] = t.key
			offsets[i] = t.offset
		}
		metricsByText, err := r.batchLookupTraceMemoryMetricsByTexts(datapackID, convID, keys, offsets)
		if err != nil {
			return spans, err
		}
		for _, t := range textRefs {
			m := metricsByText[traceMemoryTextRefKey{Key: t.key, Offset: t.offset}]
			if m == nil {
				continue
			}
			applyTraceMemoryMetrics(&perSpanItems[t.addr.span][t.addr.item], m)
		}
	}

	// Pass 3 — attach the (now-resolved) items back onto their spans.
	for i := range out {
		if perSpanItems[i] != nil {
			out[i].MemoryItems = perSpanItems[i]
		}
	}
	return out, nil
}

// batchLookupPromotedMemoryIDs returns the subset of `ids` that exist in
// datapacks.promoted_collection for this datapack + conversation (i.e. were
// promoted to the KB and evicted from mem0). Used to tag trace memory items
// as "Promoted to KB" so the side panel renders text without misleading
// 0/0/0 counts. Empty input → empty result, no query.
func (r *LangfuseRepo) batchLookupPromotedMemoryIDs(
	datapackID, conversationID string,
	ids []string,
) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	convUUID := canonicalUUID(conversationID)
	dpUUID := canonicalUUID(datapackID)
	if len(ids) == 0 || convUUID == "" || dpUUID == "" {
		return out, nil
	}
	// Compare against the native uuid `id` (sargable, uses the PK) instead of
	// casting it to text. That requires a uuid[] bind, so drop any malformed
	// ids up front — otherwise one bad value would error the whole query.
	validIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		if canonical := canonicalUUID(id); canonical != "" {
			validIDs = append(validIDs, canonical)
		}
	}
	if len(validIDs) == 0 {
		return out, nil
	}
	const q = `
SELECT id::text
FROM   datapacks.promoted_collection
WHERE  meko_datapack_id = $1::uuid
  AND  meko_conversation_id = $2::uuid
  AND  id = ANY($3::uuid[])`
	rows, err := r.dataDB.Query(q, dpUUID, convUUID, pq.Array(validIDs))
	if err != nil {
		return nil, fmt.Errorf("meta db: batch lookup promoted memory ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("meta db: scan promoted memory id: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("meta db: iterate promoted memory ids: %w", err)
	}
	return out, nil
}

func applyTraceMemoryMetrics(item *TraceMemoryItem, m *traceMemoryMetrics) {
	item.MemoryID = m.MemoryID
	item.Vector = m.Vector
	item.Entities = m.Entities
	item.Relations = m.Relations
	item.EntityNames = m.EntityNames
	item.RelationNames = m.RelationNames
}

// traceMemoryTextRefKey is the composite key used to look up text-resolved
// metrics in the per-text batched result map. We use a struct rather than
// a separator-encoded string so there's no escaping concern (a normalized
// memory text containing the separator would silently collide) and no
// per-lookup allocation — Go map keys can be any comparable type.
type traceMemoryTextRefKey struct {
	Key    string
	Offset int
}

// traceMemoryAggregationCTEs is the shared aggregation tail used by every
// batched memory-metrics query. Callers prepend their own `matched` CTE that
// exposes `(id, payload, vector, ...)` and append a SELECT that pulls per-
// memory counts and sample names off of it. The two `%d` placeholders are
// the parameter positions of the tenant `ILIKE` patterns (datapack_id and
// conversation_id projected into the agtype properties JSON), so each caller
// can keep its own parameter numbering. Everything is grouped on
// `nodes.memory_id` / `edges.memory_id` so a single query handles many
// memories at once.
const traceMemoryAggregationCTEs = `,
nodes AS (
    SELECT DISTINCT
           m.id AS memory_id,
           (n->>'node_graphid')::bigint AS node_graphid,
           NULLIF(
             btrim(
               COALESCE(
                 n->>'name',
                 n->>'entity',
                 n->>'node_name',
                 n->>'label',
                 n->>'text'
               )
             ),
             ''
           ) AS node_name
    FROM   matched m
    CROSS JOIN LATERAL jsonb_array_elements(COALESCE(m.payload->'graph_nodes', '[]'::jsonb)) AS n
    -- graph_nodes is JSON written by mem0/agent code, so the id field
    -- could be missing, empty, or non-numeric. Keep digits-only rows so
    -- the bigint cast above can't fail the whole trace query.
    WHERE  (n->>'node_graphid') ~ '^\d+$'
),
edges AS (
    SELECT DISTINCT
           ns.memory_id,
           e.id AS edge_id,
           COALESCE(
             NULLIF(
               COALESCE(
                 ep ->> 'relationship',
                 ep ->> 'relation',
                 ep ->> 'type',
                 ep ->> 'edge_type',
                 ep ->> 'label'
               ),
               ''
             ),
             NULLIF(
               CONCAT(
                 COALESCE(
                   ns.node_name,
                   NULLIF(esp ->> 'name', ''),
                   'node:' || (e.start_id::text)::bigint::text
                 ),
                 ' -> ',
                 COALESCE(
                   ne.node_name,
                   NULLIF(eep ->> 'name', ''),
                   'node:' || (e.end_id::text)::bigint::text
                 )
               ),
               ''
             )
           ) AS relation_name
    FROM   meko_multi_tenant_graph."CONNECTED_TO" e
    CROSS JOIN LATERAL (SELECT e.properties::text::jsonb) AS ep_lat(ep)
    JOIN   nodes ns ON ns.node_graphid = (e.start_id::text)::bigint
    JOIN   nodes ne ON ne.node_graphid = (e.end_id::text)::bigint
                   AND ne.memory_id    = ns.memory_id
    LEFT JOIN meko_multi_tenant_graph."Entity" es
           ON (es.id::text)::bigint = (e.start_id::text)::bigint
          AND es.meko_datapack_id = $1::uuid
          AND es.meko_conversation_id = $2::uuid
    LEFT JOIN LATERAL (SELECT es.properties::text::jsonb) AS esp_lat(esp) ON TRUE
    LEFT JOIN meko_multi_tenant_graph."Entity" ee
           ON (ee.id::text)::bigint = (e.end_id::text)::bigint
          AND ee.meko_datapack_id = $1::uuid
          AND ee.meko_conversation_id = $2::uuid
    LEFT JOIN LATERAL (SELECT ee.properties::text::jsonb) AS eep_lat(eep) ON TRUE
    WHERE  e.properties::text ILIKE $%d
      AND  e.properties::text ILIKE $%d
)`

// traceMemoryAggregationSelectByID is the SELECT half of the by-id batched
// query. One row per memory in `matched`. Sample name arrays are capped at
// six entries via an inner subquery so a single noisy memory can't blow up
// the response payload.
const traceMemoryAggregationSelectByID = `
SELECT m.id::text,
       m.vector,
       COALESCE((SELECT COUNT(DISTINCT n.node_graphid) FROM nodes n WHERE n.memory_id = m.id), 0) AS entities,
       COALESCE((SELECT COUNT(DISTINCT e.edge_id)      FROM edges e WHERE e.memory_id = m.id), 0) AS relations,
       COALESCE(
         (SELECT array_agg(t.node_name ORDER BY t.node_name)
          FROM   (
                   SELECT DISTINCT n.node_name
                   FROM   nodes n
                   WHERE  n.memory_id = m.id AND n.node_name IS NOT NULL
                   ORDER  BY n.node_name
                   LIMIT  6
                 ) t),
         ARRAY[]::text[]
       ) AS entity_names,
       COALESCE(
         (SELECT array_agg(t.relation_name ORDER BY t.relation_name)
          FROM   (
                   SELECT DISTINCT e.relation_name
                   FROM   edges e
                   WHERE  e.memory_id = m.id AND e.relation_name IS NOT NULL
                   ORDER  BY e.relation_name
                   LIMIT  6
                 ) t),
         ARRAY[]::text[]
       ) AS relation_names
FROM   matched m`

// traceMemoryAggregationSelectByText extends the by-id SELECT with the
// (text_key, text_off) tuple the caller used to find this memory so the Go
// side can stitch results back into the right slot.
const traceMemoryAggregationSelectByText = traceMemoryAggregationSelectByID + `,
       m.text_key,
       m.text_off`

// batchLookupTraceMemoryMetricsByIDs resolves vector / entity / relation
// counts and sample node / relation names for every supplied mem0 row id in
// a single round-trip. Missing ids are simply absent from the returned map.
func (r *LangfuseRepo) batchLookupTraceMemoryMetricsByIDs(
	datapackID, conversationID string,
	ids []string,
) (map[string]*traceMemoryMetrics, error) {
	out := make(map[string]*traceMemoryMetrics, len(ids))
	// $2 is matched as a plain uuid so the planner can use an index instead
	// of stripping dashes off every row; the JSON ILIKE patterns below still
	// use the dashless form the graph properties are written with.
	convUUID := canonicalUUID(conversationID)
	if len(ids) == 0 || convUUID == "" {
		return out, nil
	}

	const matchedCTE = `WITH matched AS (
    SELECT id,
           payload,
           CASE WHEN embeddings IS NOT NULL THEN 1 ELSE 0 END AS vector
    FROM   datapacks.meko_mem0_collection
    WHERE  meko_datapack_id = $1::uuid
      AND  meko_conversation_id = $2::uuid
      AND  id::text = ANY($3::text[])
)`

	// Tenant ILIKE patterns occupy $4 and $5.
	q := matchedCTE + fmt.Sprintf(traceMemoryAggregationCTEs, 4, 5) + traceMemoryAggregationSelectByID
	datapackPattern := fmt.Sprintf("%%\"meko_datapack_id\": \"%s\"%%", datapackID)
	conversationPattern := fmt.Sprintf("%%\"meko_conversation_id\": \"%s\"%%", conversationID)

	rows, err := r.dataDB.Query(q, datapackID, convUUID, pq.Array(ids), datapackPattern, conversationPattern)
	if err != nil {
		return nil, fmt.Errorf("meta db: batch lookup trace memory metrics by id: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m traceMemoryMetrics
		var entityNames, relationNames pq.StringArray
		if err := rows.Scan(&m.MemoryID, &m.Vector, &m.Entities, &m.Relations, &entityNames, &relationNames); err != nil {
			return nil, fmt.Errorf("meta db: scan trace memory metrics by id: %w", err)
		}
		m.EntityNames = []string(entityNames)
		m.RelationNames = []string(relationNames)
		out[m.MemoryID] = &m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("meta db: iterate trace memory metrics by id: %w", err)
	}
	return out, nil
}

// batchLookupTraceMemoryMetricsByTexts is the legacy-fallback batched variant
// that resolves metrics for memory items whose span output didn't carry a
// mem0 row id. Keys are normalized memory texts and offsets are 0-based
// "Nth occurrence of the same normalized text within this conversation
// (oldest first)" — `EnrichTraceMemoryItems` walks spans in trace-time
// ascending order and we want trace-time-1st → oldest DB row, trace-time-Nth
// → Nth-oldest, so the partition ranks rows ASC by `meko_created_at`.
// Folded into one ROW_NUMBER pass so the cost is independent of how many
// items we have to resolve. Unlike the old per-item helper this only takes
// the exact-equality branch; the substring LIKE fallbacks were heuristic
// and could resolve to the wrong memory, so we drop them in favour of just
// returning no metrics when there's no exact normalized match.
func (r *LangfuseRepo) batchLookupTraceMemoryMetricsByTexts(
	datapackID, conversationID string,
	keys []string,
	offsets []int,
) (map[traceMemoryTextRefKey]*traceMemoryMetrics, error) {
	out := make(map[traceMemoryTextRefKey]*traceMemoryMetrics, len(keys))
	// See batchLookupTraceMemoryMetricsByIDs: $2 is a sargable uuid compare;
	// the JSON ILIKE patterns keep the dashless form.
	convUUID := canonicalUUID(conversationID)
	dpUUID := canonicalUUID(datapackID)
	if len(keys) == 0 || convUUID == "" || dpUUID == "" {
		return out, nil
	}
	if len(keys) != len(offsets) {
		return nil, fmt.Errorf("meta db: batch lookup trace memory metrics by text: keys/offsets length mismatch (%d vs %d)", len(keys), len(offsets))
	}

	// `wanted` is the (text, occurrence_offset) request set, joined back
	// against `ranked` so each input row resolves to at most one mem0 row.
	// `ranked.rn` is 0-based and increases with age (oldest = 0): the
	// caller assigns offsets in trace-time ascending order, and mem0 rows
	// are persisted in the same order their spans fire, so rn=0 → oldest
	// match → trace-time-first occurrence is the correct alignment.
	const matchedCTE = `WITH wanted AS (
    SELECT key, off
    FROM   UNNEST($3::text[], $4::int[]) AS t(key, off)
),
candidates AS (
    SELECT id,
           payload,
           embeddings,
           meko_created_at,
           btrim(regexp_replace(lower(COALESCE(payload->>'data', payload->>'memory', payload->>'text', payload->>'content', '')), '[^a-z0-9]+', ' ', 'g')) AS normalized
    FROM   datapacks.meko_mem0_collection
    WHERE  meko_datapack_id = $1::uuid
      AND  meko_conversation_id = $2::uuid
),
ranked AS (
    SELECT id, payload, embeddings, normalized,
           (ROW_NUMBER() OVER (PARTITION BY normalized ORDER BY meko_created_at ASC) - 1) AS rn
    FROM   candidates
    WHERE  normalized IN (SELECT key FROM wanted)
),
matched AS (
    SELECT r.id,
           r.payload,
           CASE WHEN r.embeddings IS NOT NULL THEN 1 ELSE 0 END AS vector,
           w.key AS text_key,
           w.off AS text_off
    FROM   ranked r
    JOIN   wanted w ON w.key = r.normalized AND w.off = r.rn
)`

	// Tenant ILIKE patterns occupy $5 and $6.
	q := matchedCTE + fmt.Sprintf(traceMemoryAggregationCTEs, 5, 6) + traceMemoryAggregationSelectByText
	datapackPattern := fmt.Sprintf("%%\"meko_datapack_id\": \"%s\"%%", datapackID)
	conversationPattern := fmt.Sprintf("%%\"meko_conversation_id\": \"%s\"%%", conversationID)

	rows, err := r.dataDB.Query(q, dpUUID, convUUID, pq.Array(keys), pq.Array(offsets), datapackPattern, conversationPattern)
	if err != nil {
		return nil, fmt.Errorf("meta db: batch lookup trace memory metrics by text: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m traceMemoryMetrics
		var entityNames, relationNames pq.StringArray
		var textKey string
		var textOff int
		if err := rows.Scan(&m.MemoryID, &m.Vector, &m.Entities, &m.Relations, &entityNames, &relationNames, &textKey, &textOff); err != nil {
			return nil, fmt.Errorf("meta db: scan trace memory metrics by text: %w", err)
		}
		m.EntityNames = []string(entityNames)
		m.RelationNames = []string(relationNames)
		out[traceMemoryTextRefKey{Key: textKey, Offset: textOff}] = &m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("meta db: iterate trace memory metrics by text: %w", err)
	}
	return out, nil
}

func parseTraceMemoryItems(raw string) []traceMemoryParsedItem {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" || trimmed == `""` || trimmed == "{}" {
		return nil
	}

	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return []traceMemoryParsedItem{{Text: trimmed}}
	}

	if s, ok := parsed.(string); ok {
		inner := strings.TrimSpace(s)
		if inner == "" {
			return nil
		}
		var nested any
		if err := json.Unmarshal([]byte(inner), &nested); err != nil {
			return []traceMemoryParsedItem{{Text: inner}}
		}
		parsed = nested
	}

	obj, ok := parsed.(map[string]any)
	if !ok {
		return []traceMemoryParsedItem{{Text: fmt.Sprint(parsed)}}
	}

	if results, ok := obj["results"].([]any); ok {
		if len(results) == 0 {
			return nil
		}
		items := make([]traceMemoryParsedItem, 0, len(results))
		for _, r := range results {
			ro, ok := r.(map[string]any)
			if !ok {
				continue
			}
			mem := strings.TrimSpace(fmt.Sprint(ro["memory"]))
			if mem == "" || mem == "<nil>" {
				continue
			}
			id := strings.TrimSpace(fmt.Sprint(ro["id"]))
			if id == "<nil>" {
				id = ""
			}
			items = append(items, traceMemoryParsedItem{MemoryID: id, Text: mem})
		}
		if len(items) > 0 {
			return items
		}
		return nil
	}

	for _, key := range []string{"memory", "data", "text", "content", "message"} {
		if v, ok := obj[key].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return []traceMemoryParsedItem{{Text: v}}
			}
		}
	}
	return nil
}

func normalizeTraceMemoryText(text string) string {
	text = strings.ToLower(text)
	replacer := strings.NewReplacer(
		".", " ",
		",", " ",
		";", " ",
		":", " ",
		"!", " ",
		"?", " ",
		"(", " ",
		")", " ",
		"[", " ",
		"]", " ",
		"{", " ",
		"}", " ",
		"\"", " ",
		"'", " ",
	)
	text = replacer.Replace(text)
	return strings.Join(strings.Fields(text), " ")
}
