package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
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

// Conversation is one row from ListConversationsForProject. Each row is
// one Langfuse session = one chat conversation. AgentID is empty when
// the session metadata didn't include it. FirstMessage is empty when
// the session has no messages yet (or the first observation had blank
// input); the UI renders its own placeholder for that case.
type Conversation struct {
	ID           string    `json:"id"`
	AgentID      string    `json:"agent_id,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	FirstMessage string    `json:"first_message"`
	Messages     int64     `json:"messages"`
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

// GetOrgMapping returns the org mapping for the account, or nil if absent.
func (r *LangfuseRepo) GetOrgMapping(accountID string) (*LangfuseOrgMapping, error) {
	var m LangfuseOrgMapping
	err := r.metaDB.QueryRow(
		`SELECT langfuse_org_id, langfuse_org_name, langfuse_org_public_key, langfuse_org_secret_key
		 FROM meko_system.langfuse_org_mapping
		 WHERE account_id = $1`,
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

// UpsertOrgMapping inserts or updates the org mapping for the account.
func (r *LangfuseRepo) UpsertOrgMapping(accountID, orgID, orgName, publicKey, secretKey string) error {
	_, err := r.metaDB.Exec(
		`INSERT INTO meko_system.langfuse_org_mapping
		 (account_id, langfuse_org_id, langfuse_org_name, langfuse_org_public_key, langfuse_org_secret_key)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (account_id) DO UPDATE SET
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

// UpsertProjectMapping inserts or updates the project mapping.
func (r *LangfuseRepo) UpsertProjectMapping(accountID, datapackID, projectID, projectName, publicKey, secretKey string) error {
	if strings.TrimSpace(datapackID) == "" {
		return fmt.Errorf("failed to persist langfuse project mapping: datapack_id is required")
	}
	_, err := r.metaDB.Exec(
		`INSERT INTO meko_system.langfuse_project_mapping
		 (account_id, datapack_id, langfuse_project_id, langfuse_project_name, langfuse_public_key, langfuse_secret_key)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (datapack_id) DO UPDATE SET
		   account_id = EXCLUDED.account_id,
		   datapack_id = EXCLUDED.datapack_id,
		   langfuse_project_id = EXCLUDED.langfuse_project_id,
		   langfuse_project_name = EXCLUDED.langfuse_project_name,
		   langfuse_public_key = EXCLUDED.langfuse_public_key,
		   langfuse_secret_key = EXCLUDED.langfuse_secret_key,
		   updated_at = NOW()`,
		accountID, datapackID, projectID, projectName, publicKey, secretKey,
	)
	if err != nil {
		return fmt.Errorf("failed to persist langfuse project mapping: %w", err)
	}
	return nil
}

// GetProjectIDAndPublicKey returns (project_id, public_key) for the
// given (account, datapack). Used by flows that only need those two
// columns. Returns found=false (no error) when no mapping row exists.
func (r *LangfuseRepo) GetProjectIDAndPublicKey(accountID, datapackID string) (string, string, bool, error) {
	var projectID string
	var publicKey sql.NullString
	err := r.metaDB.QueryRow(
		`SELECT langfuse_project_id, langfuse_public_key
		   FROM meko_system.langfuse_project_mapping
		  WHERE account_id  = $1::uuid
		    AND datapack_id = $2::uuid`,
		accountID, datapackID,
	).Scan(&projectID, &publicKey)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("meko meta db: lookup langfuse_project_mapping: %w", err)
	}
	return strings.TrimSpace(projectID), strings.TrimSpace(publicKey.String), true, nil
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

// CountDistinctSessionsForProject returns COUNT(DISTINCT session_id)
// over non-deleted clickhouse.traces rows for the given project.
// A session_id is the canonical conversation identifier in Langfuse,
// so the result equals the number of distinct conversations.
func (r *LangfuseRepo) CountDistinctSessionsForProject(projectID string) (int64, error) {
	if !r.LangfuseDBConfigured() {
		return 0, ErrLangfuseDBNotConfigured
	}
	var total int64
	err := r.langfuseDB.QueryRow(
		`SELECT COUNT(DISTINCT session_id)
		   FROM clickhouse.traces
		  WHERE project_id  = $1
		    AND session_id IS NOT NULL
		    AND is_deleted  = false`,
		projectID,
	).Scan(&total)
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
SELECT
    sessions.conversation_id,
    sessions.agent_id,
    sessions.started_at,

    -- May be NULL (no messages yet) or empty (observation had blank
    -- input). Both are handled by the UI as a styled placeholder, so
    -- we intentionally do NOT coalesce on the server.
    first_messages.first_message                  AS first_message,

    -- Sessions absent from the message-count subquery have no
    -- messages; treat them as zero rather than NULL.
    COALESCE(message_counts.messages, 0)          AS messages

FROM (
    -- (1) One row per session_id. clickhouse.traces can have multiple
    -- rows for the same trace_id (Langfuse upserts traces as events
    -- arrive); DISTINCT ON keeps the earliest-timestamped row per
    -- session, giving a clean canonical list.
    --   agent_id   : trace metadata of the earliest trace (Meko MCP
    --                writes it on conversation_create).
    --   started_at : timestamp of the earliest trace = when the
    --                conversation opened.
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

LEFT JOIN (
    -- (2) Message count per session. A "message" = one observation
    -- written by langfuse_store.add_message(); that function is the
    -- only place a 'message_id' key is set on observation metadata,
    -- so the (metadata ? 'message_id') predicate is implementation-
    -- stable and survives renames of the observation name column.
    --
    -- trace_id == session_id in the Meko MCP server (see
    -- langfuse_store.create_conversation), which is why we can join
    -- directly on sessions.conversation_id below.
    SELECT
        observations.trace_id  AS conversation_id,
        COUNT(*)               AS messages
    FROM   clickhouse.observations AS observations
    WHERE  observations.project_id = $1
      AND  observations.metadata ? 'message_id'
    GROUP  BY observations.trace_id
) AS message_counts ON message_counts.conversation_id = sessions.conversation_id

LEFT JOIN (
    -- (3) First message per session. Pick the earliest message
    -- observation and use its input (the user's prompt) as the
    -- preview text.
    SELECT DISTINCT ON (observations.trace_id)
        observations.trace_id  AS conversation_id,
        observations.input     AS first_message
    FROM   clickhouse.observations AS observations
    WHERE  observations.project_id = $1
      AND  observations.metadata ? 'message_id'
    ORDER  BY observations.trace_id, observations.start_time ASC
) AS first_messages ON first_messages.conversation_id = sessions.conversation_id

ORDER BY sessions.started_at DESC
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
			firstMsg  sql.NullString
			startedAt sql.NullTime
		)
		if err := rows.Scan(&conv.ID, &agentID, &startedAt, &firstMsg, &conv.Messages); err != nil {
			return nil, fmt.Errorf("langfuse db: scan conversation row: %w", err)
		}
		if agentID.Valid {
			conv.AgentID = agentID.String
		}
		if firstMsg.Valid {
			conv.FirstMessage = firstMsg.String
		}
		if startedAt.Valid {
			conv.StartedAt = startedAt.Time
		}
		out = append(out, conv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("langfuse db: iterate conversation rows: %w", err)
	}
	return out, nil
}
