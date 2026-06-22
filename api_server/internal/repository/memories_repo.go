package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"meko/api_server/internal/llm"
	"meko/api_server/internal/models"
)

const (
	memoriesTableSQL    = `datapacks.meko_mem0_collection`
	knowledgeBasesTable = `datapacks.knowledge_bases`
	// vertexEmbeddingsTable holds one row per (datapack, user, agent,
	// graph_name, node_graphid). When a vertex becomes orphaned (no
	// private memory in the datapack references it), every embedding row
	// for that node_graphid in the datapack is stale and must be removed.
	vertexEmbeddingsTable = `datapacks.meko_mem0_graph_vertex_embeddings`
	// privateGraphName is the Apache AGE / Yugabyte MAGE graph that
	// holds private (un-promoted) memory entities + relationships.
	// Promoted memories live in a separate graph (meko_promoted_graph)
	// that is copied during promote; nothing in this file touches it.
	privateGraphName = "meko_multi_tenant_graph"
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

// DeleteByIDForUser deletes a single private mem0 memory AND the graph
// remnants that were exclusive to it, all in one transaction.
// "Exclusive" means: no other memory in this datapack (private OR
// promoted) references the same node_graphid / edge_graphid via its
// payload's graph_nodes[] / graph_edges[].
//
// Authorisation is enforced by the meko_user_id = $userID predicate on
// the mem0 row — a caller can only delete memories they themselves
// added.
//
// On the happy path the transaction runs:
//  1. Fetch the target memory's (agent_id, payload, created_at) — also
//     serves as existence + ownership gate. We extract node/edge ids
//     from the payload in Go so the exclusivity check below only has
//     to ask "are these specific ids referenced elsewhere?" instead of
//     scanning every memory's array in the datapack.
//  2. Compute exclusive node_graphids (point-lookup against the small
//     target id set, not full-table expansion).
//  3. Same for edge_graphids.
//  4. Cypher DELETE exclusive edges from meko_multi_tenant_graph.
//  5. Cypher DETACH DELETE exclusive vertices from the same graph.
//  6. DELETE matching rows from meko_mem0_graph_vertex_embeddings.
//  7. DELETE the memory row using the FULL primary key
//     (datapack, user, agent, created_at, id) so YB resolves it as a
//     single-row point delete rather than scanning the partition.
//  8. COMMIT. Any error rolls everything back.
//
// Returns (true, nil) on success, (false, nil) when the row isn't ours
// or doesn't exist (handler maps this to 404).
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

	// Single-row DELETE plus a handful of secondary lookups. 15s gives
	// a comfortable margin for the Cypher round trips without holding
	// row locks for too long under contention.
	delCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	tx, err := repo.db.BeginTx(delCtx, nil)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// mag_catalog hosts the cypher() function in Yugabyte's MAGE
	// extension. SET LOCAL scopes the change to this txn.
	if _, err := tx.ExecContext(delCtx,
		`SET LOCAL search_path = mag_catalog, datapacks, public`,
	); err != nil {
		return false, fmt.Errorf("set search_path: %w", err)
	}

	// DB-side statement-timeout backstop. Go's context.WithTimeout
	// above bounds the request; this makes Postgres also bound each
	// statement so a blocked Cypher can't outlive the Go side.
	if _, err := tx.ExecContext(delCtx,
		`SET LOCAL statement_timeout = '15s'`,
	); err != nil {
		return false, fmt.Errorf("set statement_timeout: %w", err)
	}

	// If api_server crashes mid-tx and the deferred Rollback never
	// runs, Postgres terminates this connection's tx after 30s idle
	// so the locks don't outlive the process.
	if _, err := tx.ExecContext(delCtx,
		`SET LOCAL idle_in_transaction_session_timeout = '30s'`,
	); err != nil {
		return false, fmt.Errorf("set idle_in_transaction_session_timeout: %w", err)
	}

	// 1. Fetch the target memory in one shot — payload (to extract
	//    graph ids), agent_id + created_at (to enable the full-PK
	//    DELETE in step 7), and the (id, datapack, user) tuple
	//    doubles as the existence + ownership gate.
	var (
		agentID      string
		payloadBytes []byte
		createdAt    time.Time
	)
	fetchQ := fmt.Sprintf(
		`SELECT meko_agent_id, payload, meko_created_at FROM %s
		 WHERE id = $1::uuid AND meko_datapack_id = $2::uuid AND meko_user_id = $3::uuid`,
		memoriesTableSQL,
	)
	slog.Debug("executing SQL", "func", "DeleteByIDForUser", "query", fetchQ,
		"args", []any{memoryID, datapackID, userID})
	err = tx.QueryRowContext(delCtx, fetchQ, memoryID, datapackID, userID).
		Scan(&agentID, &payloadBytes, &createdAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("memories fetch payload: %w", err)
	}

	// Parse graph_nodes[] / graph_edges[] out of the payload locally.
	// An absent / null payload (or absent arrays) yields empty
	// slices — downstream steps become no-ops, which is the correct
	// behaviour for memories without graph extraction.
	var payload struct {
		GraphNodes []struct {
			NodeGraphID int64 `json:"node_graphid"`
		} `json:"graph_nodes"`
		GraphEdges []struct {
			EdgeGraphID int64 `json:"edge_graphid"`
		} `json:"graph_edges"`
	}
	if len(payloadBytes) > 0 {
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			return false, fmt.Errorf("unmarshal memory payload: %w", err)
		}
	}
	targetNodeIDs := make([]int64, 0, len(payload.GraphNodes))
	for _, n := range payload.GraphNodes {
		if n.NodeGraphID != 0 {
			targetNodeIDs = append(targetNodeIDs, n.NodeGraphID)
		}
	}
	targetEdgeIDs := make([]int64, 0, len(payload.GraphEdges))
	for _, e := range payload.GraphEdges {
		if e.EdgeGraphID != 0 {
			targetEdgeIDs = append(targetEdgeIDs, e.EdgeGraphID)
		}
	}

	excludeIDs := []string{memoryID}

	// 2. Exclusive node_graphids — of the ids THIS memory references,
	//    which ones are NOT referenced by any other memory in the
	//    datapack (private or promoted)?
	exclusiveNodes, err := collectExclusiveGraphIDs(
		delCtx, tx, "graph_nodes", "node_graphid",
		datapackID, excludeIDs, targetNodeIDs,
	)
	if err != nil {
		return false, err
	}

	// 3. Exclusive edge_graphids — same logic for relationships.
	exclusiveEdges, err := collectExclusiveGraphIDs(
		delCtx, tx, "graph_edges", "edge_graphid",
		datapackID, excludeIDs, targetEdgeIDs,
	)
	if err != nil {
		return false, err
	}

	// 4. Drop exclusive edges first. Edges can span vertices we DON'T
	//    own (e.g. a LIKES edge between two vertices that other
	//    memories also reference). Step 5's DETACH DELETE only prunes
	//    edges incident to vertices we're about to remove — explicit
	//    edge cleanup catches the rest.
	if err := cypherDeleteEdges(delCtx, tx, exclusiveEdges); err != nil {
		return false, err
	}

	// 5. DETACH DELETE orphan vertices.
	if err := cypherDeleteVertices(delCtx, tx, exclusiveNodes); err != nil {
		return false, err
	}

	// 6. Embedding rows for the deleted vertices are now stale.
	if err := deleteVertexEmbeddings(
		delCtx, tx, vertexEmbeddingsTable, datapackID, exclusiveNodes,
	); err != nil {
		return false, err
	}

	// 7. Final DELETE using the full primary key
	//    (meko_datapack_id, meko_user_id, meko_agent_id,
	//    meko_created_at, id) so YB does a single-row point delete
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
	if rowsAffected == 0 {
		// Defensive — the fetch above already proved the row exists.
		// If we land here something raced or the row was hand-deleted
		// between steps; treat as no-op so the caller surfaces 404.
		return false, nil
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}

	slog.Info("deleting memory",
		"layer", "repository",
		"memory_id", memoryID,
		"datapack_id", datapackID,
		"user_id", userID,
		"target_nodes", len(targetNodeIDs),
		"target_edges", len(targetEdgeIDs),
		"exclusive_nodes_deleted", len(exclusiveNodes),
		"exclusive_edges_deleted", len(exclusiveEdges),
		"rows_affected", rowsAffected,
	)
	return true, nil
}

// collectExclusiveGraphIDs returns the subset of `targetIDs` that are
// NOT referenced by any memory in the same datapack OTHER than the ones
// in `excludeMemoryIDs` — checked across BOTH meko_mem0_collection
// (other private rows) and promoted_collection.
//
// Cross-table awareness matters: when a memory is promoted, its mem0
// row is deleted but its graph entities stay in meko_multi_tenant_graph,
// and the promoted_collection row still references those node_graphids.
// Mem0 reuses those same ids when a new private memory mentions the
// same entity, so a node can be referenced by both a private memory
// and a promoted memory at once. Without the UNION, deleting that
// private memory would compute the node as "exclusive" and orphan
// the promoted memory's reference.
//
// The caller already extracted `targetIDs` from the relevant payloads
// in Go, so this query only has to ask "which of THESE specific ids
// are shared?" instead of expanding every memory's payload arrays.
// With an empty `targetIDs` slice the function returns empty without
// touching the DB — the right no-op for memories that lack
// graph_nodes / graph_edges.
//
// `excludeMemoryIDs` is the set of mem0 rows being deleted (private-
// delete passes one id; promote passes the whole batch). They're
// excluded from the "shared" lookup so a memory doesn't appear to
// share ids with itself.
//
// Used for both node and edge exclusivity (same query shape, just
// different JSONB field + id key).
func collectExclusiveGraphIDs(
	ctx context.Context,
	tx *sql.Tx,
	arrayField, idKey string,
	datapackID string,
	excludeMemoryIDs []string,
	targetIDs []int64,
) ([]int64, error) {
	if len(targetIDs) == 0 {
		return nil, nil
	}
	query := fmt.Sprintf(`
WITH shared AS (
  -- of the target ids, which are referenced by another private memory?
  SELECT DISTINCT (elem->>'%[1]s')::bigint AS gid
  FROM %[2]s,
       LATERAL jsonb_array_elements(COALESCE(payload->'%[4]s', '[]'::jsonb)) elem
  WHERE id != ALL($1::uuid[])
    AND meko_datapack_id = $2::uuid
    AND (elem->>'%[1]s')::bigint = ANY($3::bigint[])
  UNION
  -- ... or by any promoted memory in this datapack
  SELECT DISTINCT (elem->>'%[1]s')::bigint AS gid
  FROM %[3]s,
       LATERAL jsonb_array_elements(COALESCE(payload->'%[4]s', '[]'::jsonb)) elem
  WHERE meko_datapack_id = $2::uuid
    AND (elem->>'%[1]s')::bigint = ANY($3::bigint[])
)
-- target ids that are NOT shared = exclusive to the batch
SELECT t.gid
FROM unnest($3::bigint[]) AS t(gid)
LEFT JOIN shared s USING (gid)
WHERE s.gid IS NULL`,
		idKey, memoriesTableSQL, promotedCollectionTable, arrayField)

	slog.Debug("executing SQL", "func", "collectExclusiveGraphIDs",
		"array_field", arrayField, "id_key", idKey,
		"exclude_count", len(excludeMemoryIDs),
		"target_count", len(targetIDs))
	rows, err := tx.QueryContext(ctx, query,
		pq.Array(excludeMemoryIDs), datapackID, pq.Array(targetIDs))
	if err != nil {
		return nil, fmt.Errorf("collect exclusive %s: %w", arrayField, err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			slog.Warn("collectExclusiveGraphIDs close", "error", closeErr)
		}
	}()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan exclusive %s: %w", arrayField, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate exclusive %s: %w", arrayField, err)
	}
	return ids, nil
}

// cypherDeleteVertices removes the given node ids from the private
// graph via DETACH DELETE. DETACH (vs plain DELETE) prunes incident
// edges in the same statement so the graph never holds a dangling edge,
// even in the edge case where graph_edges[] was missing or stale.
//
// IDs are inlined into the Cypher string — safe because they came from
// a bigint column we just queried, not from user input. Cypher in
// Apache AGE / MAGE does not accept Postgres parameters inside the
// $cypher$ block.
func cypherDeleteVertices(
	ctx context.Context, tx *sql.Tx, ids []int64,
) error {
	if len(ids) == 0 {
		return nil
	}
	query := fmt.Sprintf(`
		SELECT * FROM cypher('%s', $cypher$
		  MATCH (n:Entity) WHERE id(n) IN [%s] DETACH DELETE n
		$cypher$) AS (result agtype)`,
		privateGraphName, joinBigints(ids),
	)
	slog.Debug("executing Cypher", "func", "cypherDeleteVertices",
		"count", len(ids), "query", query)
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("cypher delete vertices: %w", err)
	}
	return nil
}

// cypherDeleteEdges removes the given relationship ids from the private
// graph. Plain DELETE (not DETACH) — edges have no further attachments
// to clean up. Same inlining rationale as cypherDeleteVertices.
//
// MATCH is label-agnostic (`-[e]->`, no `:CONNECTED_TO`) so cleanup
// stays correct if mem0 introduces additional edge labels in the
// future. Match still scopes by id, so it never touches edges other
// than the ones we explicitly listed.
func cypherDeleteEdges(
	ctx context.Context, tx *sql.Tx, ids []int64,
) error {
	if len(ids) == 0 {
		return nil
	}
	query := fmt.Sprintf(`
		SELECT * FROM cypher('%s', $cypher$
		  MATCH ()-[e]->() WHERE id(e) IN [%s] DELETE e
		$cypher$) AS (result agtype)`,
		privateGraphName, joinBigints(ids),
	)
	slog.Debug("executing Cypher", "func", "cypherDeleteEdges",
		"count", len(ids), "query", query)
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("cypher delete edges: %w", err)
	}
	return nil
}

// deleteVertexEmbeddings removes embedding rows for the given
// node_graphids in the given datapack. Scoped by datapack only — the
// embeddings table's PK includes user_id and agent_id, so the same
// node_graphid can have multiple rows; once the vertex itself is gone
// every embedding row pointing at it is an orphan regardless of who
// added it.
func deleteVertexEmbeddings(
	ctx context.Context, tx *sql.Tx, embeddingsTable, datapackID string, nodeIDs []int64,
) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	query := fmt.Sprintf(
		`DELETE FROM %s
		 WHERE meko_datapack_id = $1::uuid AND node_graphid = ANY($2::bigint[])`,
		embeddingsTable,
	)
	slog.Debug("executing SQL", "func", "deleteVertexEmbeddings",
		"embeddings_table", embeddingsTable, "datapack_id", datapackID, "count", len(nodeIDs))
	if _, err := tx.ExecContext(ctx, query, datapackID, pq.Array(nodeIDs)); err != nil {
		return fmt.Errorf("delete graph vertex embeddings: %w", err)
	}
	return nil
}

// joinBigints renders a slice of int64 as a comma-separated list for
// inlining into a Cypher IN [...] clause. Empty slice returns "" — the
// caller must guard against len(ids) == 0 before building the query.
func joinBigints(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
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
