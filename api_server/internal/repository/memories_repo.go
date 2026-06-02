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
	// db is the dataDB handle (datapacks.meko_mem0_collection lives here).
	db *sql.DB
	// systemDB is the meko_system schema handle, used by ListByFilter
	// for out-of-line user lookups now that the LEFT JOIN to
	// meko_system.users would cross physical databases.
	systemDB             *sql.DB
	openAIAPIKey         string
	openAILLMModel       string
	openAIEmbeddingModel string
	// skipVectorSort, when true, bypasses the OpenAI summary + embedding step
	// in ListByFilter and orders results by meko_created_at DESC, id DESC.
	skipVectorSort bool
}

func NewMemoriesRepo(db, systemDB *sql.DB, openAIAPIKey, openAILLMModel, openAIEmbeddingModel string, skipVectorSort bool) *MemoriesRepo {
	return &MemoriesRepo{
		db:                   db,
		systemDB:             systemDB,
		openAIAPIKey:         openAIAPIKey,
		openAILLMModel:       openAILLMModel,
		openAIEmbeddingModel: openAIEmbeddingModel,
		skipVectorSort:       skipVectorSort,
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
		slog.Warn("memories list: could not create LLM client, falling back to plain ordering", "error", err)
		return nil, nil
	}
	summary, err := llmClient.Complete(ctx, fmt.Sprintf("Generate a summary of the following text: %s", chunkTexts))
	if err != nil {
		slog.Warn("memories list: LLM summarization failed, falling back to plain ordering", "error", err)
		return nil, nil
	}
	vector, err := llmClient.GenerateEmbedding(ctx, summary, repo.openAIEmbeddingModel)
	if err != nil {
		slog.Warn("memories list: embedding generation failed, falling back to plain ordering", "error", err)
		return nil, nil
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

// CountLearningsByDatapack counts only mem0 rows still awaiting a
// user decision — i.e. `meko_promotion_user_action IS NULL`. Sibling of
// CountByDatapack which counts the full inventory (NULL or
// 'keep_as_memory'). Drives the `learnings_count` field on the datapack
// describe response, which the Learnings UI uses as its source of
// truth.
func (repo *MemoriesRepo) CountLearningsByDatapack(ctx context.Context, datapackID string) (int, error) {
	if repo.db == nil {
		return 0, fmt.Errorf("database connection is required")
	}
	datapackID = strings.TrimSpace(datapackID)
	if datapackID == "" {
		return 0, fmt.Errorf("datapack_id is required")
	}

	query := fmt.Sprintf(
		`SELECT COUNT(*) FROM %s WHERE meko_datapack_id = $1::uuid AND meko_promotion_user_action IS NULL`,
		memoriesTableSQL,
	)

	countCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	slog.Debug("executing SQL", "func", "CountLearningsByDatapack", "query", query, "args", []any{datapackID})
	var count int
	if err := repo.db.QueryRowContext(countCtx, query, datapackID).Scan(&count); err != nil {
		return 0, fmt.Errorf("memories learnings-count query: %w", err)
	}
	return count, nil
}

// DeleteByIDForUser deletes a single mem0 memory, scoped to the given
// (memoryID, datapackID, userID) triple. The userID predicate is the
// authorization gate: it ensures a caller can only delete memories they
// added themselves, even when other members of a shared datapack see
// the row in the listing.
//
// Returns (true, nil) when the row was deleted, (false, nil) when no row
// matched (either the memory does not exist, lives in a different
// datapack, or belongs to another user — collapsed to a single 404 on
// the handler side so we don't leak existence to unauthorised callers).
func (repo *MemoriesRepo) DeleteByIDForUser(
	ctx context.Context,
	memoryID, datapackID, userID string,
) (bool, error) {
	if repo.db == nil {
		return false, fmt.Errorf("database connection is required")
	}
	memoryID = strings.TrimSpace(memoryID)
	datapackID = strings.TrimSpace(datapackID)
	userID = strings.TrimSpace(userID)
	if memoryID == "" {
		return false, fmt.Errorf("memory_id is required")
	}
	if datapackID == "" {
		return false, fmt.Errorf("datapack_id is required")
	}
	if userID == "" {
		return false, fmt.Errorf("user_id is required")
	}

	query := fmt.Sprintf(
		`DELETE FROM %s WHERE id = $1::uuid AND meko_datapack_id = $2::uuid AND meko_user_id = $3::uuid`,
		memoriesTableSQL,
	)

	// Single-row indexed DELETE — 10s is plenty even with light contention.
	// Other queries in this file use 30s because they do vector ordering /
	// chunk fetching; that bound doesn't apply here.
	delCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	slog.Debug("executing SQL", "func", "DeleteByIDForUser", "query", query,
		"args", []any{memoryID, datapackID, userID})
	result, err := repo.db.ExecContext(delCtx, query, memoryID, datapackID, userID)
	if err != nil {
		return false, fmt.Errorf("memories delete query: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("memories delete rows affected: %w", err)
	}

	slog.Info("deleting memory",
		"layer", "repository",
		"memory_id", memoryID,
		"datapack_id", datapackID,
		"user_id", userID,
		"rows_affected", rows,
	)
	return rows > 0, nil
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

	// Scope decides which `meko_promotion_user_action` values are kept.
	// "learnings" = rows still awaiting a decision (NULL).
	// "memories"  (default) = NULL OR explicitly kept.
	scopeFilter := "m.meko_promotion_user_action IS NULL" +
		" OR m.meko_promotion_user_action = 'keep_as_memory'::datapacks.meko_promotion_user_action"
	if strings.EqualFold(filter.Scope, "learnings") {
		scopeFilter = "m.meko_promotion_user_action IS NULL"
	}
	whereSQL := fmt.Sprintf("m.meko_datapack_id = $1::uuid AND (%s)", scopeFilter)
	args := []any{datapackID}
	n := 2

	if q := strings.TrimSpace(filter.Q); q != "" {
		whereSQL += fmt.Sprintf(" AND (m.payload->>'data') ILIKE '%%' || $%d || '%%'", n)
		args = append(args, q)
		n++
	}

	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM %s m WHERE %s`, memoriesTableSQL, whereSQL)
	countCtx, cancelCount := context.WithTimeout(ctx, 30*time.Second)
	defer cancelCount()
	slog.Debug("executing SQL", "func", "ListByFilter", "query", countQuery, "args", args)
	var total int
	if err := repo.db.QueryRowContext(countCtx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("memories count query: %w", err)
	}

	if total == 0 {
		slog.Info("memories list: no memories found", "datapack_id", filter.DatapackID, "user_type", filter.UserType)
		return []models.Memory{}, 0, nil
	}

	// meko_system.users now lives on systemDB; we can't join to it from
	// a dataDB query (the JOIN would silently return NULLs for every
	// row, blanking user_name + user_email). The list query stays on
	// dataDB and selects `meko_user_id` raw; the user-display enrichment
	// happens out-of-line below via `LookupUserDisplays` on systemDB.
	const selectCols = `
			m.id::text,
			COALESCE(m.payload->>'data', '') AS data,
			m.meko_agent_id,
			COALESCE(m.meko_conversation_id::text, '') AS conversation_id,
			m.meko_user_id::text,
			m.meko_created_at`
	const fromSQL = memoriesTableSQL + ` m`

	var vector []float64
	if filter.IsPaidUserType() {
		if repo.skipVectorSort {
			slog.Debug("memories list: vector sort disabled by config",
				"datapack_id", filter.DatapackID, "user_type", filter.UserType)
		} else {
			v, err := repo.resolveQueryVector(ctx, filter)
			if err != nil {
				return nil, 0, err
			}
			vector = v
			if len(vector) == 0 {
				slog.Debug("memories list: no KB chunks available, falling back to plain ordering",
					"datapack_id", filter.DatapackID, "user_type", filter.UserType)
			}
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
			ORDER BY m.embeddings <=> %s::vector`, selectCols, fromSQL, whereSQL, vecParam)
	} else {
		query = fmt.Sprintf(`
			SELECT %s
			FROM %s
			WHERE %s
			ORDER BY m.meko_created_at DESC, m.id DESC`, selectCols, fromSQL, whereSQL)
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
		var (
			row       models.Memory
			createdAt sql.NullTime
		)
		if err := sqlRows.Scan(
			&row.ID,
			&row.Data,
			&row.AgentID,
			&row.ConversationID,
			&row.UserID,
			&createdAt,
		); err != nil {
			return nil, 0, fmt.Errorf("memories scan: %w", err)
		}
		if createdAt.Valid {
			t := createdAt.Time.UTC()
			row.CreatedAt = &t
		}
		matched = append(matched, row)
	}

	// Out-of-line systemDB enrichment: pull user_name + user_email for
	// every distinct meko_user_id we just scanned, then attach. Mirrors
	// the previous LEFT JOIN's semantics — missing user → empty fields →
	// `omitempty` strips them from the JSON.
	userIDs := make([]string, 0, len(matched))
	for _, m := range matched {
		userIDs = append(userIDs, m.UserID)
	}
	displays, dispErr := LookupUserDisplays(ctx, repo.systemDB, userIDs)
	if dispErr != nil {
		// Non-fatal: log + ship rows without name/email rather than
		// failing the whole list.
		slog.Warn("memories list: user display lookup failed",
			"datapack_id", filter.DatapackID, "error", dispErr)
	} else if displays != nil {
		for i := range matched {
			if d, ok := displays[matched[i].UserID]; ok {
				matched[i].UserName = d.Name
				matched[i].UserEmail = d.Email
			}
		}
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
