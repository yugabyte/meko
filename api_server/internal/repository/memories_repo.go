package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"meko/api_server/internal/db"
	"meko/api_server/internal/llm"
	"meko/api_server/internal/models"
)

const DefaultMemoriesCollection = "meko_tenants_collection"

func quoteMemoriesCollection(memoriesCollection string) (string, error) {
	name := strings.TrimSpace(memoriesCollection)
	if name == "" {
		name = DefaultMemoriesCollection
	}
	return fmt.Sprintf(`"public"."%s"`, name), nil
}

type MemoriesRepo struct {
	db                     *sql.DB
	tenantKBCollectionName string
	openAIAPIKey           string
	openAILLMModel         string
	openAIEmbeddingModel   string
}

func NewMemoriesRepo(db *sql.DB, tenantKBTable, openAIAPIKey, openAILLMModel, openAIEmbeddingModel string) *MemoriesRepo {
	tenantKBCollectionName, _ := quoteMemoriesCollection(tenantKBTable)
	if tenantKBCollectionName == "" {
		tenantKBCollectionName = fmt.Sprintf(`"public"."%s"`, DefaultTenantKBTable)
	}
	return &MemoriesRepo{
		db:                     db,
		tenantKBCollectionName: tenantKBCollectionName,
		openAIAPIKey:           openAIAPIKey,
		openAILLMModel:         openAILLMModel,
		openAIEmbeddingModel:   openAIEmbeddingModel,
	}
}

// GetAllChunkTexts fetches every chunk_text from knowledge_base and concatenates them
// into a single string.
// If agentID is non-empty, an additional filter on metadata_filters->>'agent_id' is applied.
// @note: user_id & datapack_id are needed when the multi-tenant is enabled.
func (repo *MemoriesRepo) GetAllChunkTexts(ctx context.Context, dbConn *sql.DB, agentID string) (string, error) {
	if dbConn == nil {
		return "", fmt.Errorf("database connection is required")
	}

	var conds []string
	var args []any
	n := 1

	if s := strings.TrimSpace(agentID); s != "" {
		conds = append(conds, fmt.Sprintf("metadata_filters->>'agent_id' = $%d", n))
		args = append(args, s)
		n++
	}

	var whereClause string
	if len(conds) > 0 {
		whereClause = "WHERE " + strings.Join(conds, " AND ")
	}

	q := fmt.Sprintf(`
		SELECT chunk_text
		FROM %s
		%s
		ORDER BY id`, repo.tenantKBCollectionName, whereClause)

	slog.Debug("executing SQL", "func", "GetAllChunkTexts", "query", q, "args", args)
	rows, err := dbConn.QueryContext(ctx, q, args...)
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

// to get the mem0 collections use the mcp_user credentials from the system registry.
// note: this is a temporary fix and will be replaced later when multiple tenants are supported.
func getMem0DBConn(ctx context.Context, systemDB *sql.DB, datapackID string) (*sql.DB, error) {
	const getMem0ConnQuery = `
		SELECT
			regexp_replace(
				d.datapack_ysql_connection_string,
				'^postgresql://[^@]+@',
				'postgresql://' || a.mcp_user_name || ':' || a.mcp_user_password || '@'
			) AS updated_connection_string
		FROM
			meko_system.account_registry a
		JOIN
			meko_system.datapack_registry d
			ON d.account_id = a.id
		WHERE
			d.id = $1`

	slog.Debug("executing SQL", "func", "getMem0DBConn", "query", getMem0ConnQuery, "args", []any{datapackID})
	var connStr string
	if err := systemDB.QueryRowContext(ctx, getMem0ConnQuery, datapackID).Scan(&connStr); err != nil {
		return nil, fmt.Errorf("fetch mem0 conn string: %w", err)
	}
	u, err := url.Parse(connStr)
	if err != nil {
		return nil, fmt.Errorf("parse mem0 conn str: %w", err)
	}
	q := u.Query()
	if q.Get("sslmode") == "" {
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
	}

	conn, err := db.ConnectWithRetry(u.String(), 3, time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to mem0 db: %w", err)
	}
	return conn, nil
}

// getMemoriesCollections returns the quoted table names to query.
// If agentID is non-empty, returns a single table "{agentID}_mem0_collection".
// Otherwise, discovers all tables in the public schema ending with "_mem0_collection".
func getMemoriesCollections(ctx context.Context, yugaDB *sql.DB, agentID string) ([]string, error) {
	if s := strings.TrimSpace(agentID); s != "" {
		return []string{fmt.Sprintf(`"public"."%s_mem0_collection"`, s)}, nil
	}
	const listCollectionsQuery = `SELECT tablename FROM pg_tables
		 WHERE schemaname = 'public' AND tablename LIKE '%\_mem0\_collection' ESCAPE '\'`

	slog.Debug("executing SQL", "func", "getMemoriesCollections", "query", listCollectionsQuery)
	rows, err := yugaDB.QueryContext(ctx, listCollectionsQuery)
	if err != nil {
		return nil, fmt.Errorf("list mem0 collections: %w", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan collection name: %w", err)
		}
		tables = append(tables, fmt.Sprintf(`"public"."%s"`, name))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mem0 collections: %w", err)
	}
	return tables, nil
}

// resolveQueryVector fetches all chunk texts for the filter, summarizes them via LLM,
// and returns the embedding vector. Used only for paid users.
func (repo *MemoriesRepo) resolveQueryVector(ctx context.Context, yugaDB *sql.DB, filter models.MemoryListFilter) ([]float64, error) {
	chunkTexts, err := repo.GetAllChunkTexts(ctx, yugaDB, filter.AgentID)
	if err != nil {
		return nil, fmt.Errorf("get chunk texts: %w", err)
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

// buildPaidUserQuery builds a SELECT ordered by vector similarity (cosine distance).
func buildPaidUserQuery(collections []string, whereSQL, vecParam string) string {
	if len(collections) == 1 {
		return fmt.Sprintf(`
			SELECT id::text, payload
			FROM %s
			WHERE %s
			ORDER BY vector <=> %s::vector`, collections[0], whereSQL, vecParam)
	}
	subSelects := make([]string, len(collections))
	for i, tbl := range collections {
		subSelects[i] = fmt.Sprintf(`SELECT id::text AS id, payload, vector FROM %s WHERE %s`, tbl, whereSQL)
	}
	return fmt.Sprintf(`
		SELECT id, payload
		FROM (%s) combined
		ORDER BY vector <=> %s::vector`,
		strings.Join(subSelects, "\nUNION ALL\n"), vecParam)
}

// buildFreeUserQuery builds a plain SELECT with no vector operations, ordered by id.
func buildFreeUserQuery(collections []string, whereSQL string) string {
	if len(collections) == 1 {
		return fmt.Sprintf(`
			SELECT id::text, payload
			FROM %s
			WHERE %s
			ORDER BY id`, collections[0], whereSQL)
	}
	subSelects := make([]string, len(collections))
	for i, tbl := range collections {
		subSelects[i] = fmt.Sprintf(`SELECT id::text AS id, payload FROM %s WHERE %s`, tbl, whereSQL)
	}
	return fmt.Sprintf(`
		SELECT id, payload
		FROM (%s) combined
		ORDER BY id`,
		strings.Join(subSelects, "\nUNION ALL\n"))
}

// buildCountQuery counts total rows matching the WHERE conditions across all collections.
func buildCountQuery(collections []string, whereSQL string) string {
	if len(collections) == 1 {
		return fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s`, collections[0], whereSQL)
	}
	subSelects := make([]string, len(collections))
	for i, tbl := range collections {
		subSelects[i] = fmt.Sprintf(`SELECT id FROM %s WHERE %s`, tbl, whereSQL)
	}
	return fmt.Sprintf(`SELECT COUNT(*) FROM (%s) combined`,
		strings.Join(subSelects, "\nUNION ALL\n"))
}

func (repo *MemoriesRepo) ListByFilter(
	ctx context.Context,
	filter models.MemoryListFilter,
) ([]models.Memory, int, error) {

	if repo.db == nil {
		return nil, 0, fmt.Errorf("database connection is required")
	}
	var conds []string
	var args []any
	n := 1
	if s := strings.TrimSpace(filter.AgentID); s != "" {
		conds = append(conds, fmt.Sprintf(`payload->>'agent_id' = $%d`, n))
		args = append(args, s)
		n++
	}
	if strings.TrimSpace(filter.DatapackID) == "" {
		return nil, 0, fmt.Errorf("datapack_id is required")
	}

	whereSQL := "TRUE"
	if len(conds) > 0 {
		whereSQL = strings.Join(conds, " AND ")
	}

	mem0DB, err := getMem0DBConn(ctx, repo.db, filter.DatapackID)
	if err != nil {
		return nil, 0, fmt.Errorf("open mem0 connection: %w", err)
	}
	defer mem0DB.Close()

	collections, err := getMemoriesCollections(ctx, mem0DB, filter.AgentID)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve mem0 collections: %w", err)
	}
	if len(collections) == 0 {
		slog.Info("no mem0 collections found", "user_id", filter.UserID, "agent_id", filter.AgentID)
		return []models.Memory{}, 0, nil
	}

	// Run count query before adding vector/pagination args.
	countQuery := buildCountQuery(collections, whereSQL)
	queryCtx, cancelCount := context.WithTimeout(ctx, 30*time.Second)
	defer cancelCount()
	slog.Debug("executing SQL", "func", "ListByFilter", "query", countQuery, "args", args)
	var total int
	if err := mem0DB.QueryRowContext(queryCtx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("memories count query: %w", err)
	}

	var query string
	if filter.FreeUser {
		query = buildFreeUserQuery(collections, whereSQL)
	} else {
		vector, err := repo.resolveQueryVector(ctx, mem0DB, filter)
		if err != nil {
			return nil, 0, err
		}
		vecParam := fmt.Sprintf("$%d", n)
		args = append(args, Float64SliceToVector(vector))
		n++
		query = buildPaidUserQuery(collections, whereSQL, vecParam)
	}

	// Append LIMIT / OFFSET for pagination.
	offset := (filter.Page - 1) * filter.PageSize
	query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", n, n+1)
	args = append(args, filter.PageSize, offset)

	queryCtx, cancelQuery := context.WithTimeout(ctx, 30*time.Second)
	defer cancelQuery()

	slog.Debug("executing SQL", "func", "ListByFilter", "query", query, "args", args)
	sqlRows, err := mem0DB.QueryContext(queryCtx, query, args...)
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
		var payload []byte
		if err := sqlRows.Scan(&row.ID, &payload); err != nil {
			return nil, 0, fmt.Errorf("memories scan: %w", err)
		}
		if len(payload) == 0 {
			row.Payload = json.RawMessage("null")
		} else {
			row.Payload = append(json.RawMessage(nil), payload...)
		}
		matched = append(matched, row)
	}
	if err := sqlRows.Err(); err != nil {
		return nil, 0, fmt.Errorf("memories rows: %w", err)
	}

	slog.Info("listing memories",
		"layer", "repository",
		"user_id", filter.UserID,
		"agent_id", filter.AgentID,
		"datapack_id", filter.DatapackID,
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
