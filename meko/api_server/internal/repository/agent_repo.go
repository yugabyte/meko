package repository

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	"meko/api_server/internal/models"
)

// AgentRepo encapsulates database operations for agents.
//
// Agents are user-scoped: meko_system.agents has primary key
// (user_id HASH, agent_id ASC). Datapack association lives on
// meko_system.agent_access (user_id, agent_id, datapack_id).
type AgentRepo struct {
	db *sql.DB
}

// NewAgentRepo creates a new AgentRepo.
func NewAgentRepo(db *sql.DB) *AgentRepo {
	return &AgentRepo{db: db}
}

// ErrAgentExists is returned by CreateAgent when an agent with the
// given (user_id, agent_id) already exists. Handlers map this to a
// 409 Conflict response.
var ErrAgentExists = fmt.Errorf("agent already exists")

// ErrAgentLinkNotFound is returned by UnlinkAgentFromDatapack and
// DeleteAgent when the targeted row(s) do not exist. Handlers map this
// to a 404 Not Found response.
var ErrAgentLinkNotFound = fmt.Errorf("agent link not found")

// ErrAgentNotFoundForDelete is returned by DeleteAgent when the
// targeted (user_id, agent_id) row does not exist. Handlers map this
// to a 404 Not Found response.
var ErrAgentNotFoundForDelete = fmt.Errorf("agent not found")

// CreateAgent creates a user-scoped agent. It is strictly create-only:
// when (user_id, agent_id) already exists it returns ErrAgentExists
// rather than overwriting the row. agent_details defaults to {} when
// the caller passes nil/empty bytes.
//
// Use this for the public POST /agents endpoint. The default agent
// inserted at signup uses its own statement in AccountRepo.Create with
// ON CONFLICT DO NOTHING semantics, since it must be idempotent for
// signup retries.
func (r *AgentRepo) CreateAgent(
	userID, agentID string,
	details json.RawMessage,
) error {
	if len(details) == 0 {
		details = json.RawMessage(`{}`)
	}

	res, err := r.db.Exec(
		`INSERT INTO meko_system.agents (user_id, agent_id, agent_details)
		 VALUES ($1, $2, $3::jsonb)
		 ON CONFLICT (user_id, agent_id) DO NOTHING`,
		userID, agentID, []byte(details),
	)
	if err != nil {
		slog.Error("failed to create agent",
			"user_id", userID, "agent_id", agentID, "error", err)
		return fmt.Errorf("failed to create agent: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: user=%s agent_id=%s",
			ErrAgentExists, userID, agentID)
	}
	return nil
}

// GetAgent returns a single user-scoped agent. Returns (nil, nil) if no
// such agent exists.
func (r *AgentRepo) GetAgent(userID, agentID string) (*models.Agent, error) {
	var a models.Agent
	var details sql.NullString
	err := r.db.QueryRow(
		`SELECT user_id, agent_id, COALESCE(agent_details::text, '{}'),
		        created_at, updated_at
		 FROM meko_system.agents
		 WHERE user_id = $1 AND agent_id = $2`,
		userID, agentID,
	).Scan(&a.UserID, &a.AgentID, &details, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get agent: %w", err)
	}
	if details.Valid {
		a.Details = json.RawMessage(details.String)
	}
	return &a, nil
}

// LinkAgentToDatapack adds a row in meko_system.agent_access
// connecting an existing user-scoped agent to a datapack the same user
// owns. Returns (false, nil) if no row was inserted because the agent
// does not exist (strict link-only semantics).
func (r *AgentRepo) LinkAgentToDatapack(
	userID, agentID, datapackID string,
) (bool, error) {
	res, err := r.db.Exec(
		`INSERT INTO meko_system.agent_access
			(user_id, agent_id, datapack_id)
		 SELECT $1::uuid, $2::varchar, $3::uuid
		 WHERE EXISTS (
		     SELECT 1 FROM meko_system.agents
		     WHERE user_id = $1::uuid AND agent_id = $2::varchar
		 )
		 ON CONFLICT DO NOTHING`,
		userID, agentID, datapackID,
	)
	if err != nil {
		slog.Error("failed to link agent to datapack",
			"user_id", userID, "agent_id", agentID,
			"datapack_id", datapackID, "error", err)
		return false, fmt.Errorf("failed to link agent to datapack: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to check rows affected: %w", err)
	}

	if n == 0 {
		exists, err := r.linkExists(userID, agentID, datapackID)
		if err != nil {
			return false, err
		}
		return exists, nil
	}
	return true, nil
}

func (r *AgentRepo) linkExists(userID, agentID, datapackID string) (bool, error) {
	var one int
	err := r.db.QueryRow(
		`SELECT 1 FROM meko_system.agent_access
		 WHERE user_id = $1 AND agent_id = $2 AND datapack_id = $3`,
		userID, agentID, datapackID,
	).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to check agent_access link: %w", err)
	}
	return true, nil
}

// UnlinkAgentFromDatapack removes the (user_id, agent_id, datapack_id)
// row from meko_system.agent_access. The agent itself is left intact.
func (r *AgentRepo) UnlinkAgentFromDatapack(
	userID, agentID, datapackID string,
) error {
	res, err := r.db.Exec(
		`DELETE FROM meko_system.agent_access
		 WHERE user_id = $1 AND agent_id = $2 AND datapack_id = $3`,
		userID, agentID, datapackID,
	)
	if err != nil {
		return fmt.Errorf("failed to unlink agent from datapack: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: agent %q not linked to datapack %q",
			ErrAgentLinkNotFound, agentID, datapackID)
	}
	return nil
}

// ListAgentsForDatapack returns the agents linked to a datapack for a
// given user.
func (r *AgentRepo) ListAgentsForDatapack(
	userID, datapackID string,
) ([]models.Agent, error) {
	rows, err := r.db.Query(
		`SELECT a.user_id, a.agent_id,
		        COALESCE(a.agent_details::text, '{}'),
		        a.created_at, a.updated_at
		 FROM meko_system.agents a
		 JOIN meko_system.agent_access ad
		   ON ad.user_id = a.user_id AND ad.agent_id = a.agent_id
		 WHERE ad.datapack_id = $1 AND a.user_id = $2
		 ORDER BY a.created_at DESC`,
		datapackID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list agents for datapack: %w", err)
	}
	defer rows.Close()
	return scanAgents(rows)
}

// ListAgentsForUser returns every agent registered to the user.
func (r *AgentRepo) ListAgentsForUser(userID string) ([]models.Agent, error) {
	rows, err := r.db.Query(
		`SELECT user_id, agent_id,
		        COALESCE(agent_details::text, '{}'),
		        created_at, updated_at
		 FROM meko_system.agents
		 WHERE user_id = $1
		 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list agents for user: %w", err)
	}
	defer rows.Close()
	return scanAgents(rows)
}

// DeleteAgent removes a user-scoped agent. Also removes any
// agent_access rows for the same (user_id, agent_id) so callers do
// not leak link rows when no FK cascade is in place.
func (r *AgentRepo) DeleteAgent(userID, agentID string) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			slog.Warn("rollback error",
				"user_id", userID, "agent_id", agentID, "error", rbErr)
		}
	}()

	if _, err := tx.Exec(
		`DELETE FROM meko_system.agent_access
		 WHERE user_id = $1 AND agent_id = $2`,
		userID, agentID,
	); err != nil {
		return fmt.Errorf("failed to delete agent_access rows: %w", err)
	}

	res, err := tx.Exec(
		`DELETE FROM meko_system.agents
		 WHERE user_id = $1 AND agent_id = $2`,
		userID, agentID,
	)
	if err != nil {
		return fmt.Errorf("failed to delete agent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: agent %q for user %q",
			ErrAgentNotFoundForDelete, agentID, userID)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit agent deletion: %w", err)
	}
	return nil
}

func scanAgents(rows *sql.Rows) ([]models.Agent, error) {
	var result []models.Agent
	for rows.Next() {
		var a models.Agent
		var details sql.NullString
		if err := rows.Scan(
			&a.UserID, &a.AgentID, &details,
			&a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan agent: %w", err)
		}
		if details.Valid {
			a.Details = json.RawMessage(details.String)
		}
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return result, nil
}
