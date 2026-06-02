package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

// Fixed datapack-side tables this repo bridges. All live in the shared
// datapacks schema (see migration 000002). The previous per-tenant table-name
// plumbing is gone now that the root connection has cross-tenant access and
// the schema is fixed.
const (
	memoriesSourceTable                = `datapacks.meko_mem0_collection`
	knowledgeBaseTable                 = `datapacks.knowledge_bases`
	promotedCollectionTable            = `datapacks.promoted_collection`
	mem0GraphVertexEmbeddingsTable     = `datapacks.meko_mem0_graph_vertex_embeddings`
	promotedGraphVertexEmbeddingsTable = `datapacks.meko_promoted_graph_vertex_embeddings`
)

// MarkAsKept stamps `meko_promotion_user_action = 'keep_as_memory'` on
// each mem0 row in `memory_ids` for the given datapack. One-way: the
// UI exposes no undo path. Returns the number of rows actually updated
// (a memory_id that doesn't belong to the datapack, or is already kept,
// is simply not counted).
func (r *PromoteMemoriesRepo) MarkAsKept(
	ctx context.Context,
	datapackID string,
	memoryIDs []string,
) (int64, error) {
	if r.db == nil {
		return 0, fmt.Errorf("database connection is required")
	}
	datapackID = strings.TrimSpace(datapackID)
	if datapackID == "" {
		return 0, fmt.Errorf("datapack_id is required")
	}
	if len(memoryIDs) == 0 {
		return 0, nil
	}
	q := fmt.Sprintf(`
		UPDATE %s
		   SET meko_promotion_user_action = 'keep_as_memory'::datapacks.meko_promotion_user_action
		 WHERE meko_datapack_id = $1::uuid
		   AND id = ANY($2::uuid[])
		   AND meko_promotion_user_action IS NULL`, memoriesSourceTable)
	updCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := r.db.ExecContext(updCtx, q, datapackID, pq.Array(memoryIDs))
	if err != nil {
		return 0, fmt.Errorf("mark as kept: %w", err)
	}
	n, _ := res.RowsAffected()
	slog.Info("marked memories as kept",
		"datapack_id", datapackID, "requested", len(memoryIDs), "updated", n)
	return n, nil
}

// Values for datapacks.knowledge_bases.metadata_filters->>'source' — used to
// differentiate where each KB chunk originated. Today every chunk this code
// writes/reads originates from a conversation; additional sources can be
// added here as new ingest paths land. All KB queries default-filter by this
// value so cross-source bleed is impossible.
const KBSourceConversation = "memory"

type PromoteMemoriesRepo struct {
	// db is the dataDB handle (datapacks.* tables live here).
	db *sql.DB
	// systemDB serves out-of-line user lookups for list enrichment (the
	// LEFT JOIN to meko_system.users from a dataDB query is no longer
	// reachable now that the schemas live on separate physical DBs).
	systemDB *sql.DB
}

func NewPromoteMemoriesRepo(db, systemDB *sql.DB) *PromoteMemoriesRepo {
	return &PromoteMemoriesRepo{db: db, systemDB: systemDB}
}

type mem0Row struct {
	id             string
	chunkText      string
	vectorText     string
	agentID        string
	userID         string
	conversationID string
	payload        []byte
	createdAt      time.Time
}

// Promote moves a set of mem0 memories into the tenant's knowledge_bases +
// promoted_collection tables, then evicts the source rows from mem0. All
// writes and the eviction happen inside a single transaction so the operation
// is atomic.
//
// Mapping (datapacks.meko_mem0_collection -> datapacks.knowledge_bases):
//
//	tenant_id        = datapackID                              (partition key)
//	chunk_text       = payload->>'data'
//	embeddings       = embeddings (vector, copied as-is)
//	document_id      = mem0.meko_conversation_id               (KB document_id is NOT NULL)
//	metadata_filters = {"source": "memory", "agent_id": <meko_agent_id>, "memory_id": <mem0.id>}
//
// The KB row representing a promoted memory is identified by
// (tenant_id, metadata_filters->>'memory_id'); document_id carries the
// originating conversation id so chunks from the same conversation can
// be grouped on the KB side.
//
// All KB reads/writes here default-filter by
// metadata_filters->>'source' = 'memory' so rows from any other
// ingest paths added later are invisible to this flow.
//
// Memories without a meko_conversation_id cannot be promoted (KB
// document_id is NOT NULL) and are reported back in notFoundIDs alongside
// any IDs the caller asked about that did not exist in mem0.
//
// Memories explicitly opted out of promotion (meko_promotion_user_action =
// 'keep_as_memory') are skipped and reported back as not found.
func (r *PromoteMemoriesRepo) Promote(
	ctx context.Context,
	userID string,
	agentID string,
	datapackID string,
	memoryIDs []string,
) (insertedIDs, updatedIDs, notFoundIDs []string, err error) {
	if r.db == nil {
		return nil, nil, nil, fmt.Errorf("database connection is required")
	}
	if strings.TrimSpace(datapackID) == "" {
		return nil, nil, nil, fmt.Errorf("datapack_id is required")
	}
	if len(memoryIDs) == 0 {
		return nil, nil, nil, nil
	}

	txCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	tx, err := r.db.BeginTx(txCtx, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Fetch eligible mem0 rows for this datapack. Pulls everything both
	// the KB upsert (chunk_text, embeddings, conversation_id) and the
	// promoted_collection upsert (full payload, conversation_id, user_id,
	// created_at) need. Scoping is by datapack_id + the caller's explicit
	// memory_ids list; meko_user_id is intentionally not filtered, and
	// per-agent scoping happens on the KB side via
	// metadata_filters->>'agent_id'. meko_conversation_id is required
	// because KB.document_id is NOT NULL — memories without one are
	// dropped here and surfaced as notFoundIDs.
	fetchQ := fmt.Sprintf(`
		SELECT m.id::text,
		       COALESCE(m.payload->>'data', '') AS chunk_text,
		       m.embeddings::text               AS embeddings,
		       m.meko_agent_id                  AS agent_id,
		       m.meko_user_id::text             AS user_id,
		       m.meko_conversation_id::text     AS conversation_id,
		       m.payload                        AS payload,
		       m.meko_created_at                AS created_at
		FROM %s m
		WHERE m.meko_datapack_id = $1::uuid
		  AND m.id = ANY($2::uuid[])
		  AND m.embeddings IS NOT NULL
		  AND m.meko_conversation_id IS NOT NULL
		  AND m.meko_promotion_user_action IS DISTINCT FROM 'keep_as_memory'::datapacks.meko_promotion_user_action`,
		memoriesSourceTable)
	fetchArgs := []any{datapackID, pq.Array(memoryIDs)}

	slog.Debug("executing SQL", "func", "Promote", "query", fetchQ, "args", fetchArgs)
	rows, err := tx.QueryContext(txCtx, fetchQ, fetchArgs...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fetch mem0 rows: %w", err)
	}

	var fetched []mem0Row
	for rows.Next() {
		var row mem0Row
		if err := rows.Scan(
			&row.id, &row.chunkText, &row.vectorText, &row.agentID,
			&row.userID, &row.conversationID, &row.payload, &row.createdAt,
		); err != nil {
			_ = rows.Close()
			return nil, nil, nil, fmt.Errorf("scan mem0 row: %w", err)
		}
		fetched = append(fetched, row)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, nil, fmt.Errorf("close mem0 rows: %w", err)
	}

	fetchedSet := make(map[string]bool, len(fetched))
	for _, row := range fetched {
		fetchedSet[row.id] = true
	}
	for _, id := range memoryIDs {
		if !fetchedSet[id] {
			notFoundIDs = append(notFoundIDs, id)
		}
	}

	if len(fetched) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, nil, nil, err
		}
		return nil, nil, notFoundIDs, nil
	}

	// 2. Find which of these mem0 IDs already exist in the KB for this tenant.
	// memory_id lives in metadata_filters->>'memory_id' (document_id now
	// holds the originating conversation id). When the caller supplies an
	// agent_id, scope the lookup to that agent via
	// metadata_filters->>'agent_id' so existence is per-agent. Always
	// default-filter by source = KBSourceConversation.
	fetchedIDs := make([]string, len(fetched))
	for i, row := range fetched {
		fetchedIDs[i] = row.id
	}

	scopedAgentID := strings.TrimSpace(agentID)
	hasAgentScope := scopedAgentID != ""

	existConds := []string{
		"tenant_id = $1::uuid",
		"metadata_filters->>'memory_id' = ANY($2::text[])",
		"metadata_filters->>'source' = '" + KBSourceConversation + "'",
	}
	existArgs := []any{datapackID, pq.Array(fetchedIDs)}
	if hasAgentScope {
		existConds = append(existConds, "metadata_filters->>'agent_id' = $3")
		existArgs = append(existArgs, scopedAgentID)
	}
	existQ := fmt.Sprintf(`
		SELECT metadata_filters->>'memory_id'
		FROM %s
		WHERE %s`, knowledgeBaseTable, strings.Join(existConds, " AND "))

	slog.Debug("executing SQL", "func", "Promote", "query", existQ, "args", existArgs)
	existRows, err := tx.QueryContext(txCtx, existQ, existArgs...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("check existing: %w", err)
	}

	existingSet := make(map[string]bool, len(fetched))
	for existRows.Next() {
		var memID string
		if err := existRows.Scan(&memID); err != nil {
			_ = existRows.Close()
			return nil, nil, nil, fmt.Errorf("scan existing: %w", err)
		}
		existingSet[memID] = true
	}
	if err := existRows.Close(); err != nil {
		return nil, nil, nil, fmt.Errorf("close existing rows: %w", err)
	}

	// 3. Find which mem0 IDs already exist in promoted_collection for this
	//    datapack (and agent, when supplied). Mirrors the KB existence-check
	//    so the promoted-collection upsert can independently choose
	//    INSERT vs UPDATE without assuming the two tables are in lockstep.
	promotedExistConds := []string{
		"meko_datapack_id = $1::uuid",
		"id = ANY($2::uuid[])",
	}
	promotedExistArgs := []any{datapackID, pq.Array(fetchedIDs)}
	if hasAgentScope {
		promotedExistConds = append(promotedExistConds, "meko_agent_id = $3")
		promotedExistArgs = append(promotedExistArgs, scopedAgentID)
	}
	promotedExistQ := fmt.Sprintf(`
		SELECT id::text
		FROM %s
		WHERE %s`, promotedCollectionTable, strings.Join(promotedExistConds, " AND "))

	slog.Debug("executing SQL", "func", "Promote", "query", promotedExistQ, "args", promotedExistArgs)
	promotedExistRows, err := tx.QueryContext(txCtx, promotedExistQ, promotedExistArgs...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("check promoted existing: %w", err)
	}
	promotedSet := make(map[string]bool, len(fetched))
	for promotedExistRows.Next() {
		var memID string
		if err := promotedExistRows.Scan(&memID); err != nil {
			_ = promotedExistRows.Close()
			return nil, nil, nil, fmt.Errorf("scan promoted existing: %w", err)
		}
		promotedSet[memID] = true
	}
	if err := promotedExistRows.Close(); err != nil {
		return nil, nil, nil, fmt.Errorf("close promoted existing rows: %w", err)
	}

	// 4. KB upsert: INSERT new rows, UPDATE existing ones (matched by
	//    metadata_filters->>'memory_id', additionally scoped by
	//    metadata_filters->>'agent_id' when provided).
	insertQ := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, chunk_text, embeddings, document_id, metadata_filters)
		VALUES ($1::uuid, $2, $3::vector, $4::uuid, $5::jsonb)`, knowledgeBaseTable)

	updateConds := []string{
		"tenant_id = $4::uuid",
		"metadata_filters->>'memory_id' = $5",
		"metadata_filters->>'source' = '" + KBSourceConversation + "'",
	}
	if hasAgentScope {
		updateConds = append(updateConds, "metadata_filters->>'agent_id' = $7")
	}
	updateQ := fmt.Sprintf(`
		UPDATE %s
		SET chunk_text       = $1,
		    embeddings       = $2::vector,
		    metadata_filters = $3::jsonb,
		    document_id      = $6::uuid
		WHERE %s`, knowledgeBaseTable, strings.Join(updateConds, " AND "))

	// 5. promoted_collection upsert: mirrors the KB upsert flow. New rows
	//    record the original promotion timestamp via NOW(); re-syncs refresh
	//    meko_promoted_to_kb_at and the cached payload/created_at.
	promotedInsertQ := fmt.Sprintf(`
		INSERT INTO %s (
			meko_datapack_id, meko_user_id, meko_agent_id, id,
			meko_conversation_id, payload, meko_created_at, meko_promoted_to_kb_at
		) VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5::uuid, $6::jsonb, $7, NOW())`,
		promotedCollectionTable)

	promotedUpdateConds := []string{
		"meko_datapack_id = $6::uuid",
		"id = $7::uuid",
	}
	if hasAgentScope {
		promotedUpdateConds = append(promotedUpdateConds, "meko_agent_id = $8")
	}
	promotedUpdateQ := fmt.Sprintf(`
		UPDATE %s
		SET meko_user_id           = $1::uuid,
		    meko_agent_id          = $2,
		    meko_conversation_id   = $3::uuid,
		    payload                = $4::jsonb,
		    meko_created_at        = $5,
		    meko_promoted_to_kb_at = NOW()
		WHERE %s`, promotedCollectionTable, strings.Join(promotedUpdateConds, " AND "))

	for _, row := range fetched {
		metadataJSON := fmt.Sprintf(
			`{"source":%q,"agent_id":%q,"memory_id":%q}`,
			KBSourceConversation, row.agentID, row.id,
		)
		payloadJSON := row.payload
		if len(payloadJSON) == 0 {
			payloadJSON = []byte("null")
		}

		if existingSet[row.id] {
			updateArgs := []any{
				row.chunkText, row.vectorText, metadataJSON,
				datapackID, row.id, row.conversationID,
			}
			if hasAgentScope {
				updateArgs = append(updateArgs, scopedAgentID)
			}
			slog.Debug("executing SQL", "func", "Promote", "query", updateQ, "args", updateArgs)
			if _, err := tx.ExecContext(txCtx, updateQ, updateArgs...); err != nil {
				return nil, nil, nil, fmt.Errorf("update kb row (memory_id=%s): %w", row.id, err)
			}
			updatedIDs = append(updatedIDs, row.id)
		} else {
			insertArgs := []any{datapackID, row.chunkText, row.vectorText, row.conversationID, metadataJSON}
			slog.Debug("executing SQL", "func", "Promote", "query", insertQ, "args", insertArgs)
			if _, err := tx.ExecContext(txCtx, insertQ, insertArgs...); err != nil {
				return nil, nil, nil, fmt.Errorf("insert kb row (memory_id=%s): %w", row.id, err)
			}
			insertedIDs = append(insertedIDs, row.id)
		}

		// Mirror the row into promoted_collection.
		if promotedSet[row.id] {
			promotedArgs := []any{
				row.userID, row.agentID, row.conversationID, payloadJSON, row.createdAt,
				datapackID, row.id,
			}
			if hasAgentScope {
				promotedArgs = append(promotedArgs, scopedAgentID)
			}
			slog.Debug("executing SQL", "func", "Promote", "query", promotedUpdateQ, "args", promotedArgs)
			if _, err := tx.ExecContext(txCtx, promotedUpdateQ, promotedArgs...); err != nil {
				return nil, nil, nil, fmt.Errorf("update promoted row (memory_id=%s): %w", row.id, err)
			}
		} else {
			promotedArgs := []any{
				datapackID, row.userID, row.agentID, row.id,
				row.conversationID, payloadJSON, row.createdAt,
			}
			slog.Debug("executing SQL", "func", "Promote", "query", promotedInsertQ, "args", promotedArgs)
			if _, err := tx.ExecContext(txCtx, promotedInsertQ, promotedArgs...); err != nil {
				return nil, nil, nil, fmt.Errorf("insert promoted row (memory_id=%s): %w", row.id, err)
			}
		}
	}

	// 6. Promote graph vertex embeddings referenced by the promoted memories.
	promotedGraphRows, err := r.promoteGraphVertices(txCtx, tx, datapackID, fetched)
	if err != nil {
		return nil, nil, nil, err
	}

	// 7. Delete the just-promoted rows from mem0. Ownership of the memory
	deleteQ := fmt.Sprintf(`DELETE FROM %s WHERE id = ANY($1::uuid[]) AND meko_datapack_id = $2::uuid`, memoriesSourceTable)
	slog.Debug("executing SQL", "func", "Promote", "query", deleteQ, "args", []any{fetchedIDs, datapackID})
	deleteRes, err := tx.ExecContext(txCtx, deleteQ, pq.Array(fetchedIDs), datapackID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("delete mem0 rows: %w", err)
	}
	deletedFromMem0, _ := deleteRes.RowsAffected()

	if err := tx.Commit(); err != nil {
		return nil, nil, nil, fmt.Errorf("commit: %w", err)
	}

	slog.Info("memories promoted",
		"datapack_id", datapackID,
		"user_id", userID,
		"agent_id", agentID,
		"requested", len(memoryIDs),
		"inserted", len(insertedIDs),
		"updated", len(updatedIDs),
		"not_found", len(notFoundIDs),
		"deleted_from_mem0", deletedFromMem0,
		"promoted_graph_vertices", promotedGraphRows,
	)

	return insertedIDs, updatedIDs, notFoundIDs, nil
}

// graphNodeRef is a lookup key into meko_mem0_graph_vertex_embeddings,
// pulled from a mem0 collection row + one entry of its payload.graph_nodes[].
type graphNodeRef struct {
	UserID      string
	AgentID     string
	GraphName   string
	NodeGraphID int64
}

// graphVertexRow is a single row read from meko_mem0_graph_vertex_embeddings
// and ready to be written into meko_promoted_graph_vertex_embeddings.
type graphVertexRow struct {
	DatapackID     string
	NodeGraphID    int64
	UserID         string
	AgentID        string
	GraphName      string
	NodeName       sql.NullString
	EmbeddingsText string
	ConversationID sql.NullString
	CreatedAt      time.Time
}

// promoteGraphVertices runs the three-step graph promotion:
//  1. extract graph_node refs from mem0 payloads,
//  2. fetch the matching row from meko_mem0_graph_vertex_embeddings,
//  3. insert it into meko_promoted_graph_vertex_embeddings.
func (r *PromoteMemoriesRepo) promoteGraphVertices(
	ctx context.Context,
	tx *sql.Tx,
	datapackID string,
	fetched []mem0Row,
) (int64, error) {
	refs := collectGraphNodeRefs(fetched)
	if len(refs) == 0 {
		return 0, nil
	}

	var inserted int64
	for _, ref := range refs {
		vertex, err := fetchGraphVertex(ctx, tx, datapackID, ref)
		if err != nil {
			return inserted, err
		}
		if vertex == nil {
			continue
		}
		if err := insertPromotedGraphVertex(ctx, tx, *vertex); err != nil {
			return inserted, err
		}
		inserted++
	}
	return inserted, nil
}

// Step 1: pull (user_id, agent_id, graph_name, node_graphid) tuples out of
// each mem0 row's payload.graph_nodes[].
func collectGraphNodeRefs(fetched []mem0Row) []graphNodeRef {
	var refs []graphNodeRef
	seen := make(map[string]bool)
	for _, row := range fetched {
		if len(row.payload) == 0 {
			continue
		}
		var p struct {
			GraphNodes []struct {
				GraphName   string `json:"graph_name"`
				NodeGraphID int64  `json:"node_graphid"`
			} `json:"graph_nodes"`
		}
		if err := json.Unmarshal(row.payload, &p); err != nil {
			slog.Warn("parse graph_nodes", "memory_id", row.id, "err", err)
			continue
		}
		for _, n := range p.GraphNodes {
			if n.GraphName == "" || n.NodeGraphID == 0 {
				continue
			}
			key := fmt.Sprintf("%s|%s|%s|%d", row.userID, row.agentID, n.GraphName, n.NodeGraphID)
			if seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, graphNodeRef{
				UserID:      row.userID,
				AgentID:     row.agentID,
				GraphName:   n.GraphName,
				NodeGraphID: n.NodeGraphID,
			})
		}
	}
	return refs
}

// Step 2: fetch the matching graph vertex row.
func fetchGraphVertex(
	ctx context.Context,
	tx *sql.Tx,
	datapackID string,
	ref graphNodeRef,
) (*graphVertexRow, error) {
	q := fmt.Sprintf(`
		SELECT meko_datapack_id::text,
		       node_graphid,
		       meko_user_id::text,
		       meko_agent_id,
		       graph_name,
		       node_name,
		       embeddings::text,
		       meko_conversation_id::text,
		       created_at
		FROM %s
		WHERE meko_datapack_id = $1::uuid
		  AND meko_user_id     = $2::uuid
		  AND meko_agent_id    = $3
		  AND graph_name       = $4
		  AND node_graphid     = $5`, mem0GraphVertexEmbeddingsTable)

	var v graphVertexRow
	err := tx.QueryRowContext(ctx, q,
		datapackID, ref.UserID, ref.AgentID, ref.GraphName, ref.NodeGraphID,
	).Scan(
		&v.DatapackID, &v.NodeGraphID, &v.UserID, &v.AgentID,
		&v.GraphName, &v.NodeName, &v.EmbeddingsText, &v.ConversationID, &v.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fetch graph vertex (%s/%d): %w", ref.GraphName, ref.NodeGraphID, err)
	}
	return &v, nil
}

// Step 3: insert into meko_promoted_graph_vertex_embeddings.
func insertPromotedGraphVertex(
	ctx context.Context,
	tx *sql.Tx,
	v graphVertexRow,
) error {
	q := fmt.Sprintf(`
		INSERT INTO %s (
			meko_datapack_id, node_graphid, meko_user_id, meko_agent_id,
			graph_name, node_name, embeddings, meko_conversation_id,
			created_at, meko_prompted_at
		) VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6, $7::vector, $8::uuid, $9, NOW())
		ON CONFLICT (meko_datapack_id, node_graphid) DO UPDATE SET
			meko_user_id         = EXCLUDED.meko_user_id,
			meko_agent_id        = EXCLUDED.meko_agent_id,
			graph_name           = EXCLUDED.graph_name,
			node_name            = EXCLUDED.node_name,
			embeddings           = EXCLUDED.embeddings,
			meko_conversation_id = EXCLUDED.meko_conversation_id,
			created_at           = EXCLUDED.created_at,
			meko_prompted_at     = NOW()`, promotedGraphVertexEmbeddingsTable)

	var nodeName any
	if v.NodeName.Valid {
		nodeName = v.NodeName.String
	}
	var conversationID any
	if v.ConversationID.Valid {
		conversationID = v.ConversationID.String
	}

	_, err := tx.ExecContext(ctx, q,
		v.DatapackID, v.NodeGraphID, v.UserID, v.AgentID,
		v.GraphName, nodeName, v.EmbeddingsText, conversationID, v.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert promoted graph vertex (node_graphid=%d): %w", v.NodeGraphID, err)
	}
	return nil
}

// ListPromoted returns a page of rows from datapacks.promoted_collection for a datapack.
//
// Per-row enrichment:
//   - promoted_by_name / promoted_by_email come from an out-of-line
//     systemDB lookup via `LookupUserDisplays`. We can no longer LEFT
//     JOIN meko_system.users from a dataDB query (the two schemas live
//     on different physical DBs), so the list query selects
//     `meko_user_id` raw and we batch-resolve names/emails after the
//     scan. Both fields stay empty when the user row is missing
//     (deleted account) or the memory has no associated user — matches
//     the previous LEFT JOIN's "missing user → no enrichment" semantics.
func (r *PromoteMemoriesRepo) ListPromoted(
	ctx context.Context,
	datapackID string,
	page, pageSize int,
) ([]models.PromotedMemory, int, error) {
	if r.db == nil {
		return nil, 0, fmt.Errorf("database connection is required")
	}
	datapackID = strings.TrimSpace(datapackID)
	if datapackID == "" {
		return nil, 0, fmt.Errorf("datapack_id is required")
	}

	args := []any{datapackID}
	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE meko_datapack_id = $1::uuid`, promotedCollectionTable)
	countCtx, cancelCount := context.WithTimeout(ctx, 30*time.Second)
	defer cancelCount()
	slog.Debug("executing SQL", "func", "ListPromoted", "query", countQuery, "args", args)
	var total int
	if err := r.db.QueryRowContext(countCtx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("promoted memories count query: %w", err)
	}

	listQuery := fmt.Sprintf(`
		SELECT pc.id::text,
		       pc.meko_conversation_id::text,
		       pc.meko_agent_id,
		       COALESCE(pc.payload->>'data', '') AS data,
		       pc.meko_promoted_to_kb_at,
		       pc.meko_user_id::text AS promoted_by_user_id
		  FROM %s pc
		 WHERE pc.meko_datapack_id = $1::uuid
		 ORDER BY pc.meko_promoted_to_kb_at DESC
		 LIMIT $2 OFFSET $3`,
		promotedCollectionTable)

	offset := (page - 1) * pageSize
	listArgs := []any{datapackID, pageSize, offset}

	queryCtx, cancelQuery := context.WithTimeout(ctx, 30*time.Second)
	defer cancelQuery()
	slog.Debug("executing SQL", "func", "ListPromoted", "query", listQuery, "args", listArgs)
	rows, err := r.db.QueryContext(queryCtx, listQuery, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("promoted memories query: %w", err)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			slog.Warn("promoted memories list close sql rows", "error", errClose)
		}
	}()

	var out []models.PromotedMemory
	// Parallel to `out`: the promoter's user_id per row, so we can
	// attach name + email after a single batched systemDB lookup.
	promoterIDs := []string{}
	for rows.Next() {
		var (
			row              models.PromotedMemory
			convoID          sql.NullString
			agentID          sql.NullString
			promotedAt       sql.NullTime
			promotedByUserID sql.NullString
		)
		if err := rows.Scan(
			&row.ID, &convoID, &agentID, &row.Data, &promotedAt,
			&promotedByUserID,
		); err != nil {
			return nil, 0, fmt.Errorf("promoted memories scan: %w", err)
		}
		if convoID.Valid && strings.TrimSpace(convoID.String) != "" {
			s := strings.TrimSpace(convoID.String)
			row.ConversationID = &s
		}
		if agentID.Valid {
			row.AgentID = agentID.String
		}
		if promotedAt.Valid {
			t := promotedAt.Time
			row.PromotedToKBAt = &t
		}
		out = append(out, row)
		if promotedByUserID.Valid {
			promoterIDs = append(promoterIDs, strings.TrimSpace(promotedByUserID.String))
		} else {
			promoterIDs = append(promoterIDs, "")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("promoted memories rows: %w", err)
	}

	// Out-of-line systemDB enrichment for promoter name + email.
	// Non-fatal: a lookup failure just leaves both fields nil, matching
	// the previous LEFT JOIN's "missing user → no enrichment" semantics.
	displays, dispErr := LookupUserDisplays(ctx, r.systemDB, promoterIDs)
	if dispErr != nil {
		slog.Warn("promoted memories: user display lookup failed",
			"datapack_id", datapackID, "error", dispErr)
	} else if displays != nil {
		for i := range out {
			uid := promoterIDs[i]
			if uid == "" {
				continue
			}
			if d, ok := displays[uid]; ok {
				if d.Name != "" {
					n := d.Name
					out[i].PromotedByName = &n
				}
				if d.Email != "" {
					e := d.Email
					out[i].PromotedByEmail = &e
				}
			}
		}
	}

	return out, total, nil
}

