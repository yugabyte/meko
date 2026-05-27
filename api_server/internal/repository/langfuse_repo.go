package repository

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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
	MessageID         string    `json:"message_id"`
	ObservationID     string    `json:"observation_id"`
	StartedAt         time.Time `json:"started_at"`
	Input             string    `json:"input"`
	Output            string    `json:"output"`
	Reasoning         string    `json:"reasoning,omitempty"`
	LatencyMs         int64     `json:"latency_ms"`
	MemoriesExtracted int64     `json:"memories_extracted"`
	// MemoryIDs lists the mem0 row IDs attributed to this message window.
	// Nil/empty when the message has no associated memories.
	MemoryIDs []string `json:"memory_ids,omitempty"`
	LLMCalls  int64    `json:"llm_calls"`
}

type messageWindow struct {
	MessageID string
	Start     time.Time
	NextStart *time.Time
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
	// MemoryItems is a per-memory expansion for memory spans. Each item maps one
	// extracted memory text to persisted-memory metadata and graph/vector counts.
	MemoryItems []TraceMemoryItem `json:"memory_items,omitempty"`
}

type TraceMemoryItem struct {
	MemoryID  string `json:"memory_id,omitempty"`
	Text      string `json:"text"`
	Vector    int64  `json:"vector"`
	Entities  int64  `json:"entities"`
	Relations int64  `json:"relations"`
	// EntityNames are sampled graph-node names linked to this memory (if present).
	EntityNames []string `json:"entity_names,omitempty"`
	// RelationNames are sampled relation labels/types linked to those nodes (if present).
	RelationNames []string `json:"relation_names,omitempty"`
}

// LangfuseRepo encapsulates database operations for Langfuse mappings
// and direct reads against the Langfuse DB.
//
// Backing tables:
//   - meta DB:     meko_system.langfuse_org_mapping,
//     meko_system.langfuse_project_mapping
//   - langfuse DB: public.api_keys, clickhouse.traces, clickhouse.observations
type LangfuseRepo struct {
	metaDB     *sql.DB
	langfuseDB *sql.DB
}

// NewLangfuseRepo creates a new LangfuseRepo. metaDB is required;
// langfuseDB is optional and may be nil — methods that need it return
// ErrLangfuseDBNotConfigured.
func NewLangfuseRepo(metaDB, langfuseDB *sql.DB) *LangfuseRepo {
	return &LangfuseRepo{metaDB: metaDB, langfuseDB: langfuseDB}
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
	err := r.metaDB.QueryRow(
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
	err := r.metaDB.QueryRow(
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

// UpsertOrgMapping inserts or updates the org mapping for the user.
// The accountID parameter is the user_id (see GetOrgMapping for context).
func (r *LangfuseRepo) UpsertOrgMapping(accountID, orgID, orgName, publicKey, secretKey string) error {
	_, err := r.metaDB.Exec(
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
	err := r.metaDB.QueryRow(
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
	err := r.metaDB.QueryRow(
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

	rows, err := r.metaDB.Query(q, recipientUserID)
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
	_, err := r.metaDB.Exec(
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
	if _, err := r.metaDB.Exec(
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
func (r *LangfuseRepo) ListConversationsForProject(projectID string, limit, offset int) ([]Conversation, error) {
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

	const q = `
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
    sessions.started_at,
    COALESCE(obs_agg.title,
             LEFT(obs_agg.first_message, 80))               AS title,
    obs_agg.first_message                                   AS first_message,
    COALESCE(obs_agg.messages, 0)                           AS messages,
    COALESCE(obs_agg.last_activity_at, sessions.started_at) AS last_activity_at,
    COALESCE(obs_agg.llm_calls, 0)                          AS llm_calls

FROM (
    -- One row per session: earliest trace wins (agent_id, started_at).
    SELECT DISTINCT ON (traces.session_id)
        traces.session_id              AS conversation_id,
        traces.metadata->>'agent_id'   AS agent_id,
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

ORDER BY last_activity_at DESC NULLS LAST, sessions.started_at DESC
LIMIT  $2 OFFSET $3`

	rows, err := r.langfuseDB.Query(q, projectID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("langfuse db: list conversations: %w", err)
	}
	defer rows.Close()

	out := make([]Conversation, 0, limit)
	for rows.Next() {
		var (
			conv      Conversation
			agentID   sql.NullString
			title     sql.NullString
			firstMsg  sql.NullString
			startedAt sql.NullTime
			lastActAt sql.NullTime
		)
		if err := rows.Scan(
			&conv.ID,
			&agentID,
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
		title     sql.NullString
		firstMsg  sql.NullString
		startedAt sql.NullTime
		lastActAt sql.NullTime
	)
	err := r.langfuseDB.QueryRow(headerQ, projectID, sessionID).Scan(
		&header.ID,
		&agentID,
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
           metadata ->> 'message_id' AS msg_id,
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
    SELECT b.msg_id, o.id, o.parent_observation_id, o.type, o.name, o.start_time, o.end_time, o.output
    FROM   raw_obs o
    JOIN   bookmarks b
      ON   (o.start_time <= b.bm_time AND (b.prev_bm_time IS NULL OR o.start_time > b.prev_bm_time))
        OR (b.is_last AND o.start_time > b.bm_time)
), turn_stats AS (
    SELECT msg_id AS message_id,
           MIN(start_time) AS root_start,
           COUNT(*) FILTER (WHERE type = 'GENERATION') AS llm_calls,
           COALESCE((EXTRACT(EPOCH FROM (MAX(COALESCE(end_time, start_time)) - MIN(start_time))) * 1000)::bigint, 0) AS latency_ms
    FROM   windowed
    GROUP  BY msg_id
), reasoning AS (
    SELECT w.msg_id AS message_id,
           STRING_AGG(w.output, E'\n\n' ORDER BY w.start_time) AS thinking
    FROM   windowed w
    WHERE  w.type = 'GENERATION' AND w.name = 'Reasoning'
    GROUP  BY w.msg_id
)
SELECT b.msg_id AS message_id, b.id AS observation_id, s.root_start, b.start_time,
       LEAD(s.root_start) OVER (ORDER BY s.root_start, b.start_time) AS next_start,
       b.input, b.output,
       COALESCE(r.thinking, '') AS reasoning, s.latency_ms,
       COALESCE(s.llm_calls, 0) AS llm_calls
FROM   raw_obs b
JOIN   turn_stats s ON s.message_id = b.msg_id
LEFT JOIN reasoning r ON r.message_id = b.msg_id
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
	for threadRows.Next() {
		var (
			row         ThreadMessage
			windowStart sql.NullTime
			started     sql.NullTime
			nextStart   sql.NullTime
			input       sql.NullString
			output      sql.NullString
			reason      sql.NullString
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
			&row.LatencyMs,
			&row.LLMCalls,
		); err != nil {
			return nil, fmt.Errorf("langfuse db: scan thread row: %w", err)
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
		for i := range header.Thread {
			mid := header.Thread[i].MessageID
			header.Thread[i].MemoriesExtracted = memRes.counts[mid]
			header.Thread[i].MemoryIDs = memRes.memoryIDs[mid]
		}
	}

	return &header, nil
}

type memoryWindowResult struct {
	counts    map[string]int64
	memoryIDs map[string][]string // messageID → []memoryID (real mem0 UUIDs)
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
			if *winIdx == 0 {
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
	if len(windows) > 0 {
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
func (r *LangfuseRepo) mem0PerMessageWindow(
	datapackID, sessionID string,
	windows []messageWindow,
) (memoryWindowResult, error) {
	empty := memoryWindowResult{
		counts:    map[string]int64{},
		memoryIDs: map[string][]string{},
	}
	if len(windows) == 0 {
		return empty, nil
	}
	datapackID = strings.TrimSpace(datapackID)
	sessionID = strings.ReplaceAll(strings.TrimSpace(sessionID), "-", "")
	if datapackID == "" || sessionID == "" {
		return empty, nil
	}

	const q = `
SELECT id::text AS memory_id, meko_created_at
FROM   datapacks.meko_mem0_collection
WHERE  meko_datapack_id = $1::uuid
  AND  meko_conversation_id IS NOT NULL
  AND  REPLACE(meko_conversation_id::text, '-', '') = $2
  AND  meko_promotion_user_action IS DISTINCT FROM 'keep_as_memory'::datapacks.meko_promotion_user_action
ORDER  BY meko_created_at ASC, id ASC`

	rows, err := r.metaDB.Query(q, datapackID, sessionID)
	if err != nil {
		return empty, fmt.Errorf("meta db: list mem0 rows for message windows: %w", err)
	}
	defer rows.Close()

	res := memoryWindowResult{
		counts:    make(map[string]int64, len(windows)),
		memoryIDs: make(map[string][]string, len(windows)),
	}
	for _, w := range windows {
		res.counts[w.MessageID] = 0
		res.memoryIDs[w.MessageID] = nil
	}

	winIdx := 0
	for rows.Next() {
		var memID string
		var createdAt time.Time
		if err := rows.Scan(&memID, &createdAt); err != nil {
			return empty, fmt.Errorf("meta db: scan mem0 row: %w", err)
		}
		messageID := attributeTimeToMessageWindow(windows, &winIdx, createdAt)
		if messageID == "" {
			continue
		}
		res.counts[messageID]++
		res.memoryIDs[messageID] = append(res.memoryIDs[messageID], memID)
	}
	if err := rows.Err(); err != nil {
		return empty, fmt.Errorf("meta db: iterate mem0 rows: %w", err)
	}
	return res, nil
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
  AND  output::text NOT LIKE '%"results": []%'
  AND  output::text NOT LIKE '%"added_entities": []%'
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
           o.provided_model_name, o.usage_details, o.total_cost, o.output
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
       CASE WHEN (o.type = 'TOOL' AND o.name ILIKE '%memory add%'
                  AND o.output::text NOT LIKE '%"results": []%'
                  AND o.output::text NOT LIKE '%"added_entities": []%')
            THEN COALESCE(o.output::text, '') ELSE '' END AS output
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
	if r == nil || r.metaDB == nil || len(spans) == 0 {
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
		for id, addrs := range refsByID {
			m := metricsByID[id]
			if m == nil {
				continue
			}
			for _, a := range addrs {
				applyTraceMemoryMetrics(&perSpanItems[a.span][a.item], m)
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
          AND REPLACE(es.meko_conversation_id::text, '-', '') = $2
    LEFT JOIN LATERAL (SELECT es.properties::text::jsonb) AS esp_lat(esp) ON TRUE
    LEFT JOIN meko_multi_tenant_graph."Entity" ee
           ON (ee.id::text)::bigint = (e.end_id::text)::bigint
          AND ee.meko_datapack_id = $1::uuid
          AND REPLACE(ee.meko_conversation_id::text, '-', '') = $2
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
	if len(ids) == 0 {
		return out, nil
	}

	const matchedCTE = `WITH matched AS (
    SELECT id,
           payload,
           CASE WHEN embeddings IS NOT NULL THEN 1 ELSE 0 END AS vector
    FROM   datapacks.meko_mem0_collection
    WHERE  meko_datapack_id = $1::uuid
      AND  meko_conversation_id IS NOT NULL
      AND  REPLACE(meko_conversation_id::text, '-', '') = $2
      AND  id::text = ANY($3::text[])
)`

	// Tenant ILIKE patterns occupy $4 and $5.
	q := matchedCTE + fmt.Sprintf(traceMemoryAggregationCTEs, 4, 5) + traceMemoryAggregationSelectByID
	datapackPattern := fmt.Sprintf("%%\"meko_datapack_id\": \"%s\"%%", datapackID)
	conversationPattern := fmt.Sprintf("%%\"meko_conversation_id\": \"%s\"%%", conversationID)

	rows, err := r.metaDB.Query(q, datapackID, conversationID, pq.Array(ids), datapackPattern, conversationPattern)
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
	if len(keys) == 0 {
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
      AND  meko_conversation_id IS NOT NULL
      AND  REPLACE(meko_conversation_id::text, '-', '') = $2
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

	rows, err := r.metaDB.Query(q, datapackID, conversationID, pq.Array(keys), pq.Array(offsets), datapackPattern, conversationPattern)
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
