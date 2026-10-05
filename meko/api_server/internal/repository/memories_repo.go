package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lib/pq"

	"meko/api_server/internal/llm"
	"meko/api_server/internal/models"
)

const (
	memoriesTableSQL    = `datapacks.meko_mem0_collection`
	knowledgeBasesTable = `datapacks.knowledge_bases`
	// entitiesTableSQL holds one row per distinct entity mem0 extracted from a
	// memory, with payload.linked_memory_ids naming the memories that mention
	// it. Deleting or promoting a memory has to prune those links, or the
	// entity outlives every memory that justified it.
	entitiesTableSQL = `datapacks.meko_mem0_collection_entities`
)

// Mem0V2CutoffTS gates delete: pre-v2 rows have graph residue this repo doesn't clean.
var Mem0V2CutoffTS = time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)

// ErrPreMem0V2Memory → handler maps to 400 PRE_MEM0_V2_MEMORY.
var ErrPreMem0V2Memory = errors.New("memory pre-dates mem0 v2 (Sept 9, 2026); delete disabled")

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

// pruneEntityLinks drops memoryIDs from every entity's linked_memory_ids in the
// (datapack, user, agent) tenant, deleting entities left with nothing linked.
//
// This mirrors mem0's _remove_memory_from_entity_store (mem0/memory/main.py),
// which is what the MCP delete path runs. The Go delete/promote
// paths write SQL directly rather than going through mem0, so without this they
// leave the entity store in a state the Python path never would. Keep the two
// in step: a memory mentioned by two entities must leave the shared one behind.
//
// Both statements carry the full partition key. The table is hash-partitioned
// 128 ways on (datapack, user, agent); anything less scans all of them.
func pruneEntityLinks(
	ctx context.Context,
	tx *sql.Tx,
	datapackID, userID, agentID string,
	memoryIDs []string,
) error {
	if len(memoryIDs) == 0 {
		return nil
	}

	// Entities whose every link is being removed: nothing justifies the row.
	// A NULL/absent linked_memory_ids yields no rows from the lateral, so the
	// EXISTS is false and the entity is left alone.
	deleteOrphansQ := fmt.Sprintf(
		`DELETE FROM %s
		 WHERE meko_datapack_id = $1::uuid
		   AND meko_user_id     = $2::uuid
		   AND meko_agent_id    = $3
		   AND EXISTS (
		         SELECT 1 FROM jsonb_array_elements_text(payload->'linked_memory_ids') v
		         WHERE v = ANY($4::text[]))
		   AND NOT EXISTS (
		         SELECT 1 FROM jsonb_array_elements_text(payload->'linked_memory_ids') v
		         WHERE NOT (v = ANY($4::text[])))`,
		entitiesTableSQL,
	)
	orphanRes, err := tx.ExecContext(ctx, deleteOrphansQ,
		datapackID, userID, agentID, pq.Array(memoryIDs))
	if err != nil {
		return fmt.Errorf("entity orphan delete: %w", err)
	}
	orphansDeleted, _ := orphanRes.RowsAffected()

	// Survivors keep the row and lose only the removed ids.
	pruneQ := fmt.Sprintf(
		`UPDATE %s
		 SET payload = jsonb_set(payload, '{linked_memory_ids}', COALESCE((
		         SELECT jsonb_agg(v)
		         FROM   jsonb_array_elements_text(payload->'linked_memory_ids') v
		         WHERE  NOT (v = ANY($4::text[]))
		       ), '[]'::jsonb))
		 WHERE meko_datapack_id = $1::uuid
		   AND meko_user_id     = $2::uuid
		   AND meko_agent_id    = $3
		   AND EXISTS (
		         SELECT 1 FROM jsonb_array_elements_text(payload->'linked_memory_ids') v
		         WHERE v = ANY($4::text[]))`,
		entitiesTableSQL,
	)
	pruneRes, err := tx.ExecContext(ctx, pruneQ,
		datapackID, userID, agentID, pq.Array(memoryIDs))
	if err != nil {
		return fmt.Errorf("entity link prune: %w", err)
	}
	linksPruned, _ := pruneRes.RowsAffected()

	slog.Info("pruned entity links",
		"layer", "repository",
		"datapack_id", datapackID,
		"agent_id", agentID,
		"memories", len(memoryIDs),
		"entities_deleted", orphansDeleted,
		"entities_pruned", linksPruned,
	)
	return nil
}

// DeleteByIDForUser: ownership-gated (meko_user_id = caller) full-PK DELETE.
// Returns ErrPreMem0V2Memory for pre-cutoff rows; (false, nil) for 404.
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

	// Transactional: the memory row and its entity links must go together, or
	// a half-applied delete leaves entities pointing at a memory that is gone.
	delCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tx, err := repo.db.BeginTx(delCtx, nil)
	if err != nil {
		return false, fmt.Errorf("memories delete begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Fetch the target row's agent_id + created_at — needed for the
	//    full-PK DELETE below, and the (id, datapack, user) tuple
	//    doubles as the existence + ownership gate.
	var (
		agentID   string
		createdAt time.Time
	)
	fetchQ := fmt.Sprintf(
		`SELECT meko_agent_id, meko_created_at FROM %s
		 WHERE id = $1::uuid AND meko_datapack_id = $2::uuid AND meko_user_id = $3::uuid`,
		memoriesTableSQL,
	)
	slog.Debug("executing SQL", "func", "DeleteByIDForUser", "query", fetchQ,
		"args", []any{memoryID, datapackID, userID})
	err = tx.QueryRowContext(delCtx, fetchQ, memoryID, datapackID, userID).
		Scan(&agentID, &createdAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("memories fetch row: %w", err)
	}

	// 2. Date gate — refuse mem0-v1 rows so we never orphan their
	//    graph vertices/edges.
	if createdAt.Before(Mem0V2CutoffTS) {
		return false, ErrPreMem0V2Memory
	}

	// 3. Full-PK DELETE so YB resolves it as a single-row point delete
	//    instead of scanning the partition.
	deleteQ := fmt.Sprintf(
		`DELETE FROM %s
		 WHERE id = $1::uuid
		   AND meko_datapack_id = $2::uuid
		   AND meko_user_id = $3::uuid
		   AND meko_agent_id = $4
		   AND meko_created_at = $5`,
		memoriesTableSQL,
	)
	slog.Debug("executing SQL", "func", "DeleteByIDForUser", "query", deleteQ,
		"args", []any{memoryID, datapackID, userID, agentID, createdAt})
	result, err := tx.ExecContext(delCtx, deleteQ,
		memoryID, datapackID, userID, agentID, createdAt)
	if err != nil {
		return false, fmt.Errorf("memories delete query: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("memories delete rows affected: %w", err)
	}

	// 4. Reap the entities this memory was the last reference for, and prune
	//    it from the ones still referenced by other memories.
	if rowsAffected > 0 {
		if err := pruneEntityLinks(delCtx, tx, datapackID, userID, agentID,
			[]string{memoryID}); err != nil {
			return false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("memories delete commit: %w", err)
	}

	slog.Info("deleting memory",
		"layer", "repository",
		"memory_id", memoryID,
		"datapack_id", datapackID,
		"user_id", userID,
		"rows_affected", rowsAffected,
	)
	return rowsAffected > 0, nil
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

	// Per-user scoping. `meko_user_id` is a first-class column on
	// `datapacks.meko_mem0_collection` (same predicate used on the
	// single-row lookups at GetByID / DeleteByID); guarding on the empty
	// string keeps admin / cross-user callers working when they omit
	// the field. The FE (WorkbenchLM observation panel) always passes
	// it, so the typical request is now strictly user-scoped.
	if uid := strings.TrimSpace(filter.UserID); uid != "" {
		whereSQL += fmt.Sprintf(" AND m.meko_user_id = $%d::uuid", n)
		args = append(args, uid)
		n++
	}

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
