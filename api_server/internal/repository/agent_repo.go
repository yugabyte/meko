package repository

import (
	"database/sql"
	"fmt"
	"log/slog"

	"meko/api_server/internal/models"
)

// AgentRepo encapsulates database operations for agents.
type AgentRepo struct {
	db *sql.DB
}

// NewAgentRepo creates a new AgentRepo.
func NewAgentRepo(db *sql.DB) *AgentRepo {
	return &AgentRepo{db: db}
}

// Create inserts a new agent for the given datapack and links it to all of
// the datapack's knowledge bases. Returns the new agent ID.
func (r *AgentRepo) Create(
	datapackName, agentName, metadata string,
) (string, error) {
	var datapackID string
	err := r.db.QueryRow(
		`SELECT id FROM meko_system.datapack_registry
		 WHERE datapack_name = $1`, datapackName,
	).Scan(&datapackID)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("datapack %q not found", datapackName)
	}
	if err != nil {
		return "", fmt.Errorf("failed to look up datapack: %w", err)
	}

	if metadata == "" {
		metadata = "{}"
	}

	var agentID string
	err = r.db.QueryRow(
		`INSERT INTO meko_system.agent_registry
			(datapack_id, agent_name, agent_metadata)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		datapackID, agentName, metadata,
	).Scan(&agentID)
	if err != nil {
		slog.Error("failed to create agent",
			"datapack", datapackName, "agent", agentName, "error", err)
		return "", fmt.Errorf("failed to create agent: %w", err)
	}

	if _, err = r.db.Exec(
		`INSERT INTO meko_system.agent_knowledge_base
			(agent_id, knowledge_base_id)
		 SELECT $1, id
		 FROM meko_system.datapack_knowledge_base_registry
		 WHERE datapack_id = $2`,
		agentID, datapackID,
	); err != nil {
		slog.Error("failed to link agent to knowledge bases",
			"agent_id", agentID, "error", err)
		return "", fmt.Errorf(
			"failed to link agent to knowledge bases: %w", err)
	}

	return agentID, nil
}

// ListByDatapack returns all agents for a given datapack name.
func (r *AgentRepo) ListByDatapack(
	datapackName string,
) ([]models.Agent, error) {
	rows, err := r.db.Query(
		`SELECT a.id, a.datapack_id, a.agent_name,
		        COALESCE(a.agent_metadata::text, '{}'),
		        a.created_at, a.updated_at
		 FROM meko_system.agent_registry a
		 JOIN meko_system.datapack_registry d
		   ON a.datapack_id = d.id
		 WHERE d.datapack_name = $1
		 ORDER BY a.created_at DESC`,
		datapackName,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list agents: %w", err)
	}
	defer rows.Close()

	var result []models.Agent
	for rows.Next() {
		var a models.Agent
		if err := rows.Scan(
			&a.ID, &a.DatapackID, &a.Name,
			&a.Metadata, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan agent: %w", err)
		}
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return result, nil
}

// Delete removes an agent by datapack name and agent name.
func (r *AgentRepo) Delete(datapackName, agentName string) error {
	res, err := r.db.Exec(
		`DELETE FROM meko_system.agent_registry
		 WHERE agent_name = $1
		   AND datapack_id = (
		       SELECT id FROM meko_system.datapack_registry
		       WHERE datapack_name = $2
		   )`,
		agentName, datapackName,
	)
	if err != nil {
		return fmt.Errorf("failed to delete agent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf(
			"agent %q not found in datapack %q",
			agentName, datapackName,
		)
	}
	return nil
}
