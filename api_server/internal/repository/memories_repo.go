package repository

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"meko/api_server/internal/llm"
	"meko/api_server/internal/models"
)

const (
	memoriesTableSQL    = `datapacks.meko_mem0_collection`
	knowledgeBasesTable = `datapacks.knowledge_bases`
)

type MemoriesRepo struct {
	db                   *sql.DB
	openAIAPIKey         string
	openAILLMModel       string
	openAIEmbeddingModel string
}

func NewMemoriesRepo(db *sql.DB, openAIAPIKey, openAILLMModel, openAIEmbeddingModel string) *MemoriesRepo {
	return &MemoriesRepo{
		db:                   db,
		openAIAPIKey:         openAIAPIKey,
		openAILLMModel:       openAILLMModel,
		openAIEmbeddingModel: openAIEmbeddingModel,
	}
}

func (repo *MemoriesRepo) GetAllChunkTexts(ctx context.Context, datapackID string) (string, error) {
	if repo.db == nil {
		return "", fmt.Errorf("database connection is required")
	}
	datapackID = strings.TrimSpace(datapackID)
	if datapackID == "" {
		return "", fmt.Errorf("datapack_id is required")
	}

	q := fmt.Sprintf(`
		SELECT chunk_text
		FROM %s
		WHERE tenant_id = $1::uuid
		ORDER BY id`, knowledgeBasesTable)

	slog.Debug("executing SQL", "func", "GetAllChunkTexts", "query", q, "args", []any{datapackID})
	rows, err := repo.db.QueryContext(ctx, q, datapackID)
	if err != nil {
		return "", fmt.Errorf("query chunk texts: %w", err)
	}
	defer rows.Close()

	var chunks []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return "", fmt.Errorf("scan chunk text: %w", err)
		}
		chunks = append(chunks, text)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate chunk texts: %w", err)
	}

	return strings.Join(chunks, "\n"), nil
}

func Float64SliceToVector(v []float64) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func (repo *MemoriesRepo) resolveQueryVector(ctx context.Context, filter models.MemoryListFilter) ([]float64, error) {
	chunkTexts, err := repo.GetAllChunkTexts(ctx, filter.DatapackID)
	if err != nil {
		return nil, fmt.Errorf("get chunk texts: %w", err)
	}
	if strings.TrimSpace(chunkTexts) == "" {
		return nil, nil
	}
	llmClient, err := llm.NewClient(repo.openAIAPIKey, repo.openAILLMModel)
	if err != nil {
		return nil, fmt.Errorf("create llm client: %w", err)
	}
	summary, err := llmClient.Complete(ctx, fmt.Sprintf("Generate a summary of the following text: %s", chunkTexts))
	if err != nil {
		return nil, fmt.Errorf("generate summary: %w", err)
	}
	vector, err := llmClient.GenerateEmbedding(ctx, summary, repo.openAIEmbeddingModel)
	if err != nil {
		return nil, fmt.Errorf("generate embedding: %w", err)
	}
	return vector, nil
}

// CountByDatapack returns the total number of rows in the mem0 collection
// for a given datapack_id. No additional filtering is applied; this is a
// straight COUNT(*) over datapacks.meko_mem0_collection scoped by
// meko_datapack_id.
func (repo *MemoriesRepo) CountByDatapack(ctx context.Context, datapackID string) (int, error) {
	if repo.db == nil {
		return 0, fmt.Errorf("database connection is required")
	}
	datapackID = strings.TrimSpace(datapackID)
	if datapackID == "" {
		return 0, fmt.Errorf("datapack_id is required")
	}

	query := fmt.Sprintf(
		`SELECT COUNT(*) FROM %s WHERE meko_datapack_id = $1::uuid`,
		memoriesTableSQL,
	)

	countCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	slog.Debug("executing SQL", "func", "CountByDatapack", "query", query, "args", []any{datapackID})
	var count int
	if err := repo.db.QueryRowContext(countCtx, query, datapackID).Scan(&count); err != nil {
		return 0, fmt.Errorf("memories count by datapack query: %w", err)
	}

	slog.Info("counting memories",
		"layer", "repository",
		"datapack_id", datapackID,
		"count", count,
	)
	return count, nil
}

func (repo *MemoriesRepo) ListByFilter(
	ctx context.Context,
	filter models.MemoryListFilter,
) ([]models.Memory, int, error) {
	if repo.db == nil {
		return nil, 0, fmt.Errorf("database connection is required")
	}
	datapackID := strings.TrimSpace(filter.DatapackID)
	if datapackID == "" {
		return nil, 0, fmt.Errorf("datapack_id is required")
	}

	whereSQL := "meko_datapack_id = $1::uuid" +
		" AND meko_promotion_user_action IS DISTINCT FROM 'keep_as_memory'::datapacks.meko_promotion_user_action"
	args := []any{datapackID}
	n := 2

	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s`, memoriesTableSQL, whereSQL)
	countCtx, cancelCount := context.WithTimeout(ctx, 30*time.Second)
	defer cancelCount()
	slog.Debug("executing SQL", "func", "ListByFilter", "query", countQuery, "args", args)
	var total int
	if err := repo.db.QueryRowContext(countCtx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("memories count query: %w", err)
	}

	const selectCols = `
			id::text,
			COALESCE(payload->>'data', '') AS data,
			meko_agent_id`

	var vector []float64
	if filter.IsPaidUserType() {
		v, err := repo.resolveQueryVector(ctx, filter)
		if err != nil {
			return nil, 0, err
		}
		vector = v
		if len(vector) == 0 {
			slog.Info("memories list: no KB chunks available, falling back to plain ordering",
				"datapack_id", filter.DatapackID, "user_type", filter.UserType)
		}
	}

	var query string
	if len(vector) > 0 {
		vecParam := fmt.Sprintf("$%d", n)
		args = append(args, Float64SliceToVector(vector))
		n++
		query = fmt.Sprintf(`
			SELECT %s
			FROM %s
			WHERE %s
			ORDER BY embeddings <=> %s::vector`, selectCols, memoriesTableSQL, whereSQL, vecParam)
	} else {
		query = fmt.Sprintf(`
			SELECT %s
			FROM %s
			WHERE %s
			ORDER BY meko_created_at DESC, id DESC`, selectCols, memoriesTableSQL, whereSQL)
	}

	offset := (filter.Page - 1) * filter.PageSize
	query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", n, n+1)
	args = append(args, filter.PageSize, offset)

	queryCtx, cancelQuery := context.WithTimeout(ctx, 30*time.Second)
	defer cancelQuery()
	slog.Debug("executing SQL", "func", "ListByFilter", "query", query, "args", args)
	sqlRows, err := repo.db.QueryContext(queryCtx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("memories query: %w", err)
	}
	defer func() {
		if closeErr := sqlRows.Close(); closeErr != nil {
			slog.Warn("memories list close sql rows", "error", closeErr)
		}
	}()

	var matched []models.Memory
	for sqlRows.Next() {
		var row models.Memory
		if err := sqlRows.Scan(&row.ID, &row.Data, &row.AgentID); err != nil {
			return nil, 0, fmt.Errorf("memories scan: %w", err)
		}
		matched = append(matched, row)
	}
	if err := sqlRows.Err(); err != nil {
		return nil, 0, fmt.Errorf("memories rows: %w", err)
	}

	slog.Info("listing memories",
		"layer", "repository",
		"datapack_id", filter.DatapackID,
		"user_id", filter.UserID,
		"user_type", filter.UserType,
		"page", filter.Page,
		"page_size", filter.PageSize,
		"total", total,
		"row_count", len(matched),
	)

	if matched == nil {
		matched = []models.Memory{}
	}
	return matched, total, nil
}
