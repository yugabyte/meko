package repository

import (
	"database/sql"
	"fmt"
	"log/slog"

	"meko/api_server/internal/models"
)

// KnowledgeBaseRepo encapsulates database operations for knowledge bases.
type KnowledgeBaseRepo struct {
	db *sql.DB
}

// NewKnowledgeBaseRepo creates a new KnowledgeBaseRepo.
func NewKnowledgeBaseRepo(db *sql.DB) *KnowledgeBaseRepo {
	return &KnowledgeBaseRepo{db: db}
}

// Add inserts a new knowledge base for the given datapack and links it to all
// existing agents of that datapack. Returns the new knowledge base ID.
func (r *KnowledgeBaseRepo) Add(
	datapackName, url string,
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

	tx, err := r.db.Begin()
	if err != nil {
		return "", fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			slog.Warn("rollback error", "error", rbErr)
		}
	}()

	var kbID string
	err = tx.QueryRow(
		`INSERT INTO meko_system.datapack_knowledge_base_registry
			(datapack_id, knowledge_base_url)
		 VALUES ($1, $2)
		 RETURNING id`,
		datapackID, url,
	).Scan(&kbID)
	if err != nil {
		slog.Error("failed to insert knowledge base",
			"datapack", datapackName, "url", url, "error", err)
		return "", fmt.Errorf("failed to insert knowledge base: %w", err)
	}

	if _, err = tx.Exec(
		`INSERT INTO meko_system.agent_knowledge_base (agent_id, knowledge_base_id)
		 SELECT id, $1
		 FROM meko_system.agent_registry
		 WHERE datapack_id = $2`,
		kbID, datapackID,
	); err != nil {
		slog.Error("failed to link knowledge base to agents",
			"kb_id", kbID, "error", err)
		return "", fmt.Errorf(
			"failed to link knowledge base to agents: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return "", fmt.Errorf("failed to commit: %w", err)
	}

	return kbID, nil
}

// ListByDatapack returns all knowledge bases for a given datapack name.
func (r *KnowledgeBaseRepo) ListByDatapack(
	datapackName string,
) ([]models.KnowledgeBase, error) {
	rows, err := r.db.Query(
		`SELECT kb.id, kb.datapack_id, kb.knowledge_base_url,
		        kb.created_at, kb.updated_at
		 FROM meko_system.datapack_knowledge_base_registry kb
		 JOIN meko_system.datapack_registry d
		   ON kb.datapack_id = d.id
		 WHERE d.datapack_name = $1
		 ORDER BY kb.created_at DESC`,
		datapackName,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list knowledge bases: %w", err)
	}
	defer rows.Close()

	var result []models.KnowledgeBase
	for rows.Next() {
		var kb models.KnowledgeBase
		if err := rows.Scan(
			&kb.ID, &kb.DatapackID, &kb.URL,
			&kb.CreatedAt, &kb.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf(
				"failed to scan knowledge base: %w", err)
		}
		result = append(result, kb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return result, nil
}

// Delete removes a knowledge base by its URL within a datapack. The CASCADE
// on agent_knowledge_base handles unlinking from agents.
func (r *KnowledgeBaseRepo) Delete(
	datapackName, url string,
) error {
	res, err := r.db.Exec(
		`DELETE FROM meko_system.datapack_knowledge_base_registry
		 WHERE knowledge_base_url = $1
		   AND datapack_id = (
		       SELECT id FROM meko_system.datapack_registry
		       WHERE datapack_name = $2
		   )`,
		url, datapackName,
	)
	if err != nil {
		return fmt.Errorf("failed to delete knowledge base: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf(
			"knowledge base %q not found in datapack %q",
			url, datapackName,
		)
	}
	return nil
}
