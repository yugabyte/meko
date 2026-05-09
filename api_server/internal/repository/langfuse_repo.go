package repository

import (
	"database/sql"
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

	const threadQ = `
WITH msg AS (
    SELECT id           AS observation_id,
           parent_observation_id,
           start_time, end_time,
           input::text  AS input,
           output::text AS output,
           metadata ->> 'message_id' AS message_id
    FROM   clickhouse.observations
    WHERE  project_id = $1 AND trace_id = $2 AND is_deleted = false
      AND  name = 'Conversation Message Added'
      AND  (metadata ->> 'message_id') IS NOT NULL
), parents AS (
    -- The Conversation Add Message tool span is the *write-the-row*
    -- wrapper around the bookmark — it's only ~30ms long and does NOT
    -- enclose the agent's reasoning, memory_add calls, etc. We grab
    -- its start_time so the per-turn window begins where the agent
    -- actually started working on the turn (not at the zero-duration
    -- bookmark).
    SELECT id, start_time AS p_start
    FROM   clickhouse.observations
    WHERE  project_id = $1 AND trace_id = $2 AND is_deleted = false
      AND  name = 'Conversation Add Message(Meko MCP)'
), conv_start AS (
    -- Earliest observation in the trace (Conversation Create span).
    -- Used to extend the first message's window backward so out-of-order
    -- tool calls (e.g. memory_add before conversation_add_message) are
    -- attributed to the first turn rather than dropped.
    SELECT MIN(start_time) AS trace_start
    FROM   clickhouse.observations
    WHERE  project_id = $1 AND trace_id = $2 AND is_deleted = false
), raw_windows AS (
    SELECT m.observation_id, m.message_id, m.start_time, m.end_time,
           m.input, m.output,
           COALESCE(p.p_start, m.start_time) AS msg_start
    FROM   msg m LEFT JOIN parents p ON p.id = m.parent_observation_id
), windows AS (
    -- root_start is the canonical start for this message window.
    -- For the first message only, extend root_start back to conv_start
    -- so spans written before conversation_add_message (out-of-order
    -- memory_add, etc.) are included in latency and the trace drawer.
    -- next_start always uses the unextended msg_start so the extension
    -- does not bleed into subsequent message windows.
    SELECT observation_id, message_id, start_time, end_time,
           input, output,
           CASE
               WHEN ROW_NUMBER() OVER (ORDER BY msg_start, start_time) = 1
               THEN LEAST(msg_start, (SELECT trace_start FROM conv_start))
               ELSE msg_start
           END AS root_start,
           LEAD(msg_start) OVER (ORDER BY msg_start, start_time) AS next_start
    FROM   raw_windows
), reasoning_raw AS (
    SELECT NULLIF(o.metadata ->> 'message_id', '') AS metadata_message_id,
           o.parent_observation_id,
           o.start_time,
           o.output::text                           AS thinking
    FROM   clickhouse.observations o
    WHERE  o.project_id = $1 AND o.trace_id = $2 AND o.is_deleted = false
      AND  o.type       = 'GENERATION'
      AND  o.name       = 'Reasoning'
), reasoning AS (
    -- Robust join priority:
    --   1) explicit metadata.message_id (best)
    --   2) fallback parent_observation_id == Conversation Message Added.id
    SELECT COALESCE(rr.metadata_message_id, m.message_id) AS message_id,
           STRING_AGG(rr.thinking, E'\n\n' ORDER BY rr.start_time) AS thinking
    FROM   reasoning_raw rr
    LEFT   JOIN msg m ON m.observation_id = rr.parent_observation_id
    WHERE  COALESCE(rr.metadata_message_id, m.message_id) IS NOT NULL
    GROUP  BY COALESCE(rr.metadata_message_id, m.message_id)
), windowed AS (
    -- Tool / LLM / memory observations falling between this message's
    -- root_start and the next message's root_start
    -- start. Attributes inline tool work to the turn that spawned it.
    -- start_time / end_time are projected so per_msg can compute
    -- wall-clock latency over the same span set the trace drawer
    -- renders (max(end) - min(start)).
    SELECT w.message_id, o.name, o.type, o.start_time, o.end_time
    FROM   windows w
    JOIN   clickhouse.observations o
           ON o.project_id = $1 AND o.trace_id = $2 AND o.is_deleted = false
          AND o.start_time >= w.root_start
          AND (w.next_start IS NULL OR o.start_time < w.next_start)
), per_msg AS (
    SELECT message_id,
           COUNT(*) FILTER (WHERE type = 'GENERATION') AS llm_calls,
           -- Wall-clock time of the turn = MAX(end) - MIN(start) over
           -- the windowed observation set (same number the trace drawer
           -- shows in its header). Using COALESCE(end,start) matches
           -- Langfuse's own latency semantics for open spans. Using
           -- MAX/MIN handles overlap and
           -- parallel work correctly; SUM(durations) would double-count.
           COALESCE(
             (EXTRACT(EPOCH FROM (MAX(COALESCE(end_time, start_time)) - MIN(start_time))) * 1000)::bigint,
             0::bigint
           ) AS latency_ms
    FROM   windowed GROUP BY message_id
)
SELECT w.message_id, w.observation_id,
       w.root_start                                            AS window_start,
       w.start_time, w.next_start, w.input, w.output,
       COALESCE(r.thinking, '')                                  AS reasoning,
       COALESCE(p.latency_ms, 0)     AS latency_ms,
       COALESCE(
         NULLIF(p.llm_calls, 0),
         CASE WHEN NULLIF(BTRIM(w.output), '') IS NOT NULL THEN 1 ELSE 0 END
       ) AS llm_calls
FROM   windows w
LEFT   JOIN reasoning r USING (message_id)
LEFT   JOIN per_msg   p USING (message_id)
ORDER  BY w.start_time ASC
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
	// to the specific message that extracted a given memory.
	if len(header.Thread) > 0 {
		memRes, err := r.memoriesPerMessageWindow(datapackID, sessionID, windows)
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
	memoryIDs map[string][]string // messageID → []memoryID
}

func (r *LangfuseRepo) memoriesPerMessageWindow(
	datapackID, sessionID string,
	windows []messageWindow,
) (memoryWindowResult, error) {
	empty := memoryWindowResult{
		counts:    map[string]int64{},
		memoryIDs: map[string][]string{},
	}
	if r == nil || r.metaDB == nil {
		return empty, fmt.Errorf("meta database is not configured")
	}
	if len(windows) == 0 {
		return empty, nil
	}

	const q = `
SELECT id::text, meko_created_at
FROM   datapacks.meko_mem0_collection
WHERE  meko_datapack_id = $1::uuid
  AND  meko_promotion_user_action IS DISTINCT FROM 'keep_as_memory'::datapacks.meko_promotion_user_action
  AND  meko_conversation_id IS NOT NULL
  AND  REPLACE(meko_conversation_id::text, '-', '') = $2
ORDER  BY meko_created_at ASC`

	rows, err := r.metaDB.Query(q, datapackID, strings.ReplaceAll(sessionID, "-", ""))
	if err != nil {
		return empty, fmt.Errorf("meta db: load mem0 rows for message windows: %w", err)
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

	assign := func(messageID, memID string) {
		res.counts[messageID]++
		res.memoryIDs[messageID] = append(res.memoryIDs[messageID], memID)
	}

	// Two-pointer attribution over ascending timestamps.
	winIdx := 0
	for rows.Next() {
		var memID string
		var createdAt sql.NullTime
		if err := rows.Scan(&memID, &createdAt); err != nil {
			return empty, fmt.Errorf("meta db: scan mem0 row: %w", err)
		}
		if !createdAt.Valid {
			continue
		}
		ct := createdAt.Time
		// Some memories are persisted slightly before the first
		// Conversation Message Added bookmark is written. Attribute those
		// to the first message so per-message totals still reconcile with
		// the conversation-level mem0 total.
		if ct.Before(windows[0].Start) {
			assign(windows[0].MessageID, memID)
			continue
		}
		for winIdx < len(windows) {
			w := windows[winIdx]
			if ct.Before(w.Start) {
				break
			}
			if w.NextStart != nil && !ct.Before(*w.NextStart) {
				winIdx++
				continue
			}
			assign(w.MessageID, memID)
			break
		}
	}
	if err := rows.Err(); err != nil {
		return empty, fmt.Errorf("meta db: iterate mem0 rows for message windows: %w", err)
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
	datapackID string,
	conversationIDs []string,
) (map[string]int64, error) {
	if r == nil || r.metaDB == nil {
		return nil, fmt.Errorf("meta database is not configured")
	}
	datapackID = strings.TrimSpace(datapackID)
	if datapackID == "" {
		return nil, fmt.Errorf("datapack_id is required")
	}
	if len(conversationIDs) == 0 {
		return map[string]int64{}, nil
	}

	// mem0 stores meko_conversation_id as a real UUID (with hyphens);
	// langfuse stores trace_id / session_id as the unhyphenated 32-char
	// hex form. Compare on the stripped projection so callers don't
	// have to format UUIDs in Go.
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

	const q = `
SELECT REPLACE(meko_conversation_id::text, '-', '') AS conversation_id,
       COUNT(*) AS memories
FROM   datapacks.meko_mem0_collection
WHERE  meko_datapack_id = $1::uuid
  AND  meko_promotion_user_action IS DISTINCT FROM 'keep_as_memory'::datapacks.meko_promotion_user_action
  AND  meko_conversation_id IS NOT NULL
  AND  REPLACE(meko_conversation_id::text, '-', '') = ANY($2::text[])
GROUP  BY meko_conversation_id`

	rows, err := r.metaDB.Query(q, datapackID, pq.Array(stripped))
	if err != nil {
		return nil, fmt.Errorf("meta db: count mem0 per conversation: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int64, len(stripped))
	for rows.Next() {
		var (
			id  string
			cnt int64
		)
		if err := rows.Scan(&id, &cnt); err != nil {
			return nil, fmt.Errorf("meta db: scan mem0 per conversation: %w", err)
		}
		out[id] = cnt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("meta db: iterate mem0 per conversation: %w", err)
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

	const q = `
WITH msg AS (
    SELECT id, parent_observation_id, start_time, end_time
    FROM   clickhouse.observations
    WHERE  project_id = $1 AND trace_id = $2 AND is_deleted = false
      AND  name       = 'Conversation Message Added'
      AND  (metadata ->> 'message_id') = $3
    LIMIT 1
), conv_start AS (
    -- Earliest observation in the trace — the Conversation Create(Meko MCP)
    -- tool span or the Conversation Created bookmark. Used as a floor for
    -- the first message's window so that any tool calls the agent made
    -- between conversation_create and conversation_add_message (e.g. an
    -- out-of-order memory_add) are still visible in the trace drawer.
    SELECT MIN(start_time) AS trace_start
    FROM   clickhouse.observations
    WHERE  project_id = $1 AND trace_id = $2 AND is_deleted = false
), root AS (
    -- Anchor the window at the parent Add Message tool span when one
    -- exists (its start time is slightly earlier than the bookmark);
    -- this way the tree includes the wrapper itself.
    --
    -- For the first message (no earlier Conversation Message Added exists),
    -- fall back to conv_start so out-of-order pre-message tool calls
    -- (memory_add, etc.) are included in the window rather than dropped.
    SELECT
        CASE
            WHEN NOT EXISTS (
                SELECT 1 FROM clickhouse.observations
                WHERE  project_id = $1 AND trace_id = $2 AND is_deleted = false
                  AND  name = 'Conversation Message Added'
                  AND  (metadata ->> 'message_id') <> $3
                  AND  start_time < COALESCE(p.start_time, m.start_time)
            )
            THEN LEAST(COALESCE(p.start_time, m.start_time),
                       (SELECT trace_start FROM conv_start))
            ELSE COALESCE(p.start_time, m.start_time)
        END AS root_start
    FROM msg m
    LEFT JOIN clickhouse.observations p
           ON p.id = m.parent_observation_id
          AND p.project_id = $1 AND p.trace_id = $2 AND p.is_deleted = false
), nxt AS (
    -- Next message window starts at its own root_start (parent tool start
    -- when present, else bookmark start). This prevents the next
    -- Conversation Add Message wrapper from leaking into this window.
    SELECT COALESCE(p2.start_time, m2.start_time) AS next_start
    FROM   clickhouse.observations m2
    LEFT JOIN clickhouse.observations p2
           ON p2.id = m2.parent_observation_id
          AND p2.project_id = $1 AND p2.trace_id = $2 AND p2.is_deleted = false
    WHERE  m2.project_id = $1 AND m2.trace_id = $2 AND m2.is_deleted = false
      AND  m2.name = 'Conversation Message Added'
      AND  (m2.metadata ->> 'message_id') <> $3
      AND  COALESCE(p2.start_time, m2.start_time) > (SELECT root_start FROM root)
    ORDER  BY COALESCE(p2.start_time, m2.start_time), m2.start_time ASC
    LIMIT 1
)
SELECT o.id,
       COALESCE(o.parent_observation_id, ''),
       o.name,
       o.type::text                                                                  AS type,
       (EXTRACT(EPOCH FROM (COALESCE(o.end_time, o.start_time) - o.start_time))*1000)::bigint AS dur_ms,
       (EXTRACT(EPOCH FROM (o.start_time - (SELECT root_start FROM root)))*1000)::bigint AS offset_ms,
       COALESCE(o.provided_model_name, '')                                           AS model,
       CASE WHEN o.usage_details::text = '{}' THEN '' ELSE o.usage_details::text END AS usage,
       COALESCE(o.total_cost::text, '')                                              AS total_cost,
       -- Include output only for memory-related spans to keep payload small.
       CASE WHEN o.name ILIKE '%memory%' OR o.name ILIKE '%memories%'
            THEN COALESCE(LEFT(o.output::text, 4000), '')
            ELSE ''
       END                                                                           AS output
FROM   clickhouse.observations o
WHERE  o.project_id = $1 AND o.trace_id = $2 AND o.is_deleted = false
  AND  o.start_time >= (SELECT root_start FROM root)
  AND  (NOT EXISTS (SELECT 1 FROM nxt) OR o.start_time < (SELECT next_start FROM nxt))
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
