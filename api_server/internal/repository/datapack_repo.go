// Package repository provides database access functions for the API server.
package repository

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"meko/api_server/internal/models"
)

// DatapackRepo encapsulates database operations for datapacks.
type DatapackRepo struct {
	db *sql.DB
}

// NewDatapackRepo creates a new DatapackRepo.
func NewDatapackRepo(db *sql.DB) *DatapackRepo {
	return &DatapackRepo{db: db}
}

// Create registers a new datapack with its account, knowledge base, default
// agent, and agent-knowledge-base link. Returns the datapack ID and default
// agent ID.
func (r *DatapackRepo) Create(
	name, embedModel, inferModel, kbase string,
) (string, string, error) {
	var accountID string
	err := r.db.QueryRow(
		`INSERT INTO meko_system.account_registry (account_name, account_email)
		 VALUES ($1, $2)
		 ON CONFLICT (account_name) DO UPDATE SET updated_at = NOW()
		 RETURNING id`,
		"default", "default@meko.local",
	).Scan(&accountID)
	if err != nil {
		slog.Error("failed to upsert account", "error", err)
		return "", "", fmt.Errorf("failed to upsert account: %w", err)
	}

	tx, err := r.db.Begin()
	if err != nil {
		return "", "", fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			slog.Warn("rollback error", "error", rbErr)
		}
	}()

	var datapackID string
	err = tx.QueryRow(
		`INSERT INTO meko_system.datapack_registry
			(account_id, datapack_name, llm_model, embedding_model)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		accountID, name, inferModel, embedModel,
	).Scan(&datapackID)
	if err != nil {
		slog.Error("failed to insert datapack",
			"datapack", name, "error", err)
		return "", "", fmt.Errorf(
			"failed to insert datapack: %w", err)
	}

	if _, err = tx.Exec(
		`INSERT INTO meko_system.datapack_knowledge_base_registry
			(datapack_id, knowledge_base_url)
		 VALUES ($1, $2)`,
		datapackID, kbase,
	); err != nil {
		slog.Error("failed to insert knowledge base",
			"datapack_id", datapackID, "error", err)
		return "", "", fmt.Errorf(
			"failed to insert knowledge base: %w", err)
	}

	if err = tx.Commit(); err != nil {
		slog.Error("failed to commit", "error", err)
		return "", "", fmt.Errorf(
			"failed to commit datapack transaction: %w", err)
	}

	agentMeta := "{}"
	if inferModel != "" || embedModel != "" {
		agentMeta = fmt.Sprintf(
			`{"llm_model":%q,"embedding_model":%q}`,
			inferModel, embedModel,
		)
	}
	defaultAgentName := "meko_user_agent"

	var agentID string
	err = r.db.QueryRow(
		`INSERT INTO meko_system.agent_registry
			(datapack_id, agent_name, agent_metadata)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		datapackID, defaultAgentName, agentMeta,
	).Scan(&agentID)
	if err != nil {
		slog.Error("failed to create default agent",
			"datapack_id", datapackID, "error", err)
		return "", "", fmt.Errorf(
			"failed to create default agent: %w", err)
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
		return "", "", fmt.Errorf(
			"failed to link agent to knowledge bases: %w", err)
	}

	return datapackID, agentID, nil
}

// List returns all datapacks.
func (r *DatapackRepo) List() ([]models.Datapack, error) {
	rows, err := r.db.Query(
		`SELECT id, account_id, datapack_name,
		        COALESCE(llm_model, ''),
		        COALESCE(embedding_model, ''),
		        created_at, updated_at
		 FROM meko_system.datapack_registry
		 ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list datapacks: %w", err)
	}
	defer rows.Close()

	var result []models.Datapack
	for rows.Next() {
		var d models.Datapack
		if err := rows.Scan(
			&d.ID, &d.AccountID, &d.Name,
			&d.LLMModel, &d.EmbeddingModel,
			&d.CreatedAt, &d.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan datapack: %w", err)
		}
		result = append(result, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}

	return result, nil
}

// GetByName returns a single datapack by name.
func (r *DatapackRepo) GetByName(name string) (*models.Datapack, error) {
	var d models.Datapack
	err := r.db.QueryRow(
		`SELECT id, account_id, datapack_name,
		        COALESCE(llm_model, ''),
		        COALESCE(embedding_model, ''),
		        created_at, updated_at
		 FROM meko_system.datapack_registry
		 WHERE datapack_name = $1`,
		name,
	).Scan(
		&d.ID, &d.AccountID, &d.Name,
		&d.LLMModel, &d.EmbeddingModel,
		&d.CreatedAt, &d.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get datapack: %w", err)
	}
	return &d, nil
}

// Update modifies a datapack's mutable fields. Only non-empty values are
// applied.
func (r *DatapackRepo) Update(
	name, llmModel, embedModel string,
) error {
	var sets []string
	var args []any
	idx := 1

	if llmModel != "" {
		sets = append(sets, fmt.Sprintf("llm_model = $%d", idx))
		args = append(args, llmModel)
		idx++
	}
	if embedModel != "" {
		sets = append(sets, fmt.Sprintf("embedding_model = $%d", idx))
		args = append(args, embedModel)
		idx++
	}
	if len(sets) == 0 {
		return nil
	}

	sets = append(sets, "updated_at = NOW()")
	//nolint:gosec // column names are from controlled input, not user data
	query := fmt.Sprintf(
		"UPDATE meko_system.datapack_registry SET %s WHERE datapack_name = $%d",
		strings.Join(sets, ", "), idx,
	)
	args = append(args, name)

	res, err := r.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("failed to update datapack: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("datapack %q not found", name)
	}
	return nil
}

// Delete removes a datapack by name. CASCADE handles child rows.
func (r *DatapackRepo) Delete(name string) error {
	res, err := r.db.Exec(
		`DELETE FROM meko_system.datapack_registry
		 WHERE datapack_name = $1`, name,
	)
	if err != nil {
		return fmt.Errorf("failed to delete datapack: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("datapack %q not found", name)
	}
	return nil
}

// DatapackStatus holds status information for a datapack.
type DatapackStatus struct {
	CreatedAt      time.Time `json:"created_at"`
	Name           string    `json:"name"`
	ID             string    `json:"id"`
	LLMModel       string    `json:"llm_model"`
	EmbeddingModel string    `json:"embedding_model"`
	AgentCount     int       `json:"agent_count"`
	KBCount        int       `json:"knowledge_base_count"`
}

// GetStatus returns a summary status for a datapack.
func (r *DatapackRepo) GetStatus(name string) (*DatapackStatus, error) {
	var s DatapackStatus
	err := r.db.QueryRow(
		`SELECT d.id, d.datapack_name,
		        COALESCE(d.llm_model, ''),
		        COALESCE(d.embedding_model, ''),
		        d.created_at,
		        (SELECT COUNT(*) FROM meko_system.agent_registry
		         WHERE datapack_id = d.id),
		        (SELECT COUNT(*)
		         FROM meko_system.datapack_knowledge_base_registry
		         WHERE datapack_id = d.id)
		 FROM meko_system.datapack_registry d
		 WHERE d.datapack_name = $1`, name,
	).Scan(
		&s.ID, &s.Name, &s.LLMModel, &s.EmbeddingModel,
		&s.CreatedAt, &s.AgentCount, &s.KBCount,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get datapack status: %w", err)
	}
	return &s, nil
}
