package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
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

// ErrMemoriesNotReady is returned when one or more memories in a
// promote batch lack a populated payload.graph_nodes[]. mem0 sets
// graph_nodes at the end of memory.add — an empty array means mem0
// is still extracting entities. Promoting now would either hang
// behind mem0's open transaction or produce a half-baked promoted
// memory with no graph context. Handlers should surface this as a
// retry-friendly 503.
var ErrMemoriesNotReady = errors.New("memory not ready: mem0 has not finished extracting graph entities")

// memoryReadyBackstop bounds how long the pre-flight check waits for
// mem0's async entity extraction to populate payload.graph_nodes. Past
// this age, we assume the memory will never get graph context (mem0
// crashed mid-extraction, the row predates the graph schema, etc.) and
// promote anyway — downstream graph steps become no-ops, but the KB +
// promoted_collection rows still land. Without this, a stuck memory
// would trap the UI in an infinite retry loop.
const memoryReadyBackstop = 5 * time.Minute

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

// PromoteNew is the revamped 12-step promote flow (graph copy, id
// rewriting, exclusive-id cleanup, full-PK batched eviction). Hidden
// behind this name until it has end-to-end testing — the production
// handler still calls Promote (below), which is the older simpler
// flow restored from main.
//
// PromoteNew moves a set of mem0 memories into the tenant's knowledge_bases +
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
func (r *PromoteMemoriesRepo) PromoteNew(
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

	// Scoped search_path so cypher() resolves without leaking to the pool.
	if _, err := tx.ExecContext(txCtx,
		`SET LOCAL search_path = mag_catalog, datapacks, public`,
	); err != nil {
		return nil, nil, nil, fmt.Errorf("set search_path: %w", err)
	}

	// Hard DB-side cap on any single statement in this tx. Go's
	// context.WithTimeout above bounds the overall request, but
	// lib/pq doesn't always propagate cancellation to the server —
	// queries can keep running on the DB after Go has given up. This
	// SET LOCAL makes Postgres enforce the limit itself, so a hung
	// Cypher (e.g. blocked on a mem0 lock) aborts cleanly instead of
	// wedging the connection.
	if _, err := tx.ExecContext(txCtx,
		`SET LOCAL statement_timeout = '60s'`,
	); err != nil {
		return nil, nil, nil, fmt.Errorf("set statement_timeout: %w", err)
	}

	// Backstop in case api_server crashes mid-tx and the deferred
	// Rollback above never runs: Postgres terminates the connection
	// if it sits idle inside a transaction for >30s. Without this, a
	// crashed process would leave the tx (and its locks) open until
	// TCP keepalive notices — minutes by default.
	if _, err := tx.ExecContext(txCtx,
		`SET LOCAL idle_in_transaction_session_timeout = '30s'`,
	); err != nil {
		return nil, nil, nil, fmt.Errorf("set idle_in_transaction_session_timeout: %w", err)
	}

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

	// Pre-flight readiness check: distinguish "mem0 hasn't written
	// graph_nodes yet" (key absent — still processing) from "mem0
	// finished but found 0 entities" (key present, empty array — fine
	// to promote, downstream graph steps just become no-ops).
	//
	// We probe with a POINTER so json.Unmarshal sets the field to nil
	// when the key is absent vs an empty (but non-nil) slice when the
	// key is present with []. A second guard, memoryReadyBackstop,
	// stops a memory that never gets graph_nodes (mem0 crashed mid-
	// extraction, schema changes, etc.) from trapping the UI in an
	// infinite retry loop — once the row is older than the backstop
	// we proceed regardless.
	now := time.Now()
	for _, row := range fetched {
		if len(row.payload) == 0 {
			if now.Sub(row.createdAt) >= memoryReadyBackstop {
				continue
			}
			return nil, nil, nil, fmt.Errorf("%w (memory_id=%s)", ErrMemoriesNotReady, row.id)
		}
		var probe struct {
			GraphNodes *[]json.RawMessage `json:"graph_nodes"`
		}
		if err := json.Unmarshal(row.payload, &probe); err != nil {
			return nil, nil, nil, fmt.Errorf("%w (memory_id=%s, payload parse failed: %v)",
				ErrMemoriesNotReady, row.id, err)
		}
		if probe.GraphNodes == nil && now.Sub(row.createdAt) < memoryReadyBackstop {
			return nil, nil, nil, fmt.Errorf("%w (memory_id=%s)", ErrMemoriesNotReady, row.id)
		}
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

	// 4. KB upsert: bulk INSERT new rows (built inline below); per-row
	//    UPDATE for existing ones (matched by
	//    metadata_filters->>'memory_id', additionally scoped by
	//    metadata_filters->>'agent_id' when provided).
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

	// 5. promoted_collection upsert: bulk INSERT for new rows (inline
	//    below); per-row UPDATE for existing ones.
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

	// Bucket fetched rows by KB/promoted-collection existence so we can
	// run a single bulk INSERT per table for the (common) new-promote
	// path. The existing-row path keeps its per-row UPDATE loop —
	// re-promotes are rare and the UPDATE has nuanced agent-scoping
	// behaviour that's awkward to fold into ON CONFLICT.
	type pmRow struct {
		row          mem0Row
		payloadJSON  []byte
		metadataJSON string
	}
	var kbToInsert, kbToUpdate []pmRow
	var promotedToInsert, promotedToUpdate []pmRow
	for _, row := range fetched {
		md := fmt.Sprintf(
			`{"source":%q,"agent_id":%q,"memory_id":%q}`,
			KBSourceConversation, row.agentID, row.id,
		)
		pl := row.payload
		if len(pl) == 0 {
			pl = []byte("null")
		}
		pr := pmRow{row: row, payloadJSON: pl, metadataJSON: md}

		if existingSet[row.id] {
			kbToUpdate = append(kbToUpdate, pr)
			updatedIDs = append(updatedIDs, row.id)
		} else {
			kbToInsert = append(kbToInsert, pr)
			insertedIDs = append(insertedIDs, row.id)
		}
		if promotedSet[row.id] {
			promotedToUpdate = append(promotedToUpdate, pr)
		} else {
			promotedToInsert = append(promotedToInsert, pr)
		}
	}

	// Bulk INSERT for new KB rows — one multi-VALUES statement
	// regardless of batch size.
	if len(kbToInsert) > 0 {
		args := make([]any, 0, len(kbToInsert)*5)
		valueRows := make([]string, 0, len(kbToInsert))
		for i, pr := range kbToInsert {
			b := i*5 + 1
			valueRows = append(valueRows,
				fmt.Sprintf("($%d::uuid, $%d, $%d::vector, $%d::uuid, $%d::jsonb)",
					b, b+1, b+2, b+3, b+4))
			args = append(args,
				datapackID, pr.row.chunkText, pr.row.vectorText,
				pr.row.conversationID, pr.metadataJSON)
		}
		bulkQ := fmt.Sprintf(
			`INSERT INTO %s (tenant_id, chunk_text, embeddings, document_id, metadata_filters) VALUES %s`,
			knowledgeBaseTable, strings.Join(valueRows, ", "))
		slog.Debug("executing SQL", "func", "Promote", "op", "bulk_insert_kb",
			"count", len(kbToInsert))
		if _, err := tx.ExecContext(txCtx, bulkQ, args...); err != nil {
			return nil, nil, nil, fmt.Errorf("bulk insert kb rows (n=%d): %w", len(kbToInsert), err)
		}
	}

	// Per-row UPDATE for existing KB rows. Rare; preserves the
	// agent-scoping behaviour of the existing query.
	for _, pr := range kbToUpdate {
		updateArgs := []any{
			pr.row.chunkText, pr.row.vectorText, pr.metadataJSON,
			datapackID, pr.row.id, pr.row.conversationID,
		}
		if hasAgentScope {
			updateArgs = append(updateArgs, scopedAgentID)
		}
		slog.Debug("executing SQL", "func", "Promote", "query", updateQ, "args", updateArgs)
		if _, err := tx.ExecContext(txCtx, updateQ, updateArgs...); err != nil {
			return nil, nil, nil, fmt.Errorf("update kb row (memory_id=%s): %w", pr.row.id, err)
		}
	}

	// Bulk INSERT for new promoted_collection rows.
	if len(promotedToInsert) > 0 {
		args := make([]any, 0, len(promotedToInsert)*7)
		valueRows := make([]string, 0, len(promotedToInsert))
		for i, pr := range promotedToInsert {
			b := i*7 + 1
			valueRows = append(valueRows,
				fmt.Sprintf("($%d::uuid, $%d::uuid, $%d, $%d::uuid, $%d::uuid, $%d::jsonb, $%d, NOW())",
					b, b+1, b+2, b+3, b+4, b+5, b+6))
			args = append(args,
				datapackID, pr.row.userID, pr.row.agentID, pr.row.id,
				pr.row.conversationID, pr.payloadJSON, pr.row.createdAt)
		}
		bulkQ := fmt.Sprintf(`
			INSERT INTO %s (
				meko_datapack_id, meko_user_id, meko_agent_id, id,
				meko_conversation_id, payload, meko_created_at, meko_promoted_to_kb_at
			) VALUES %s`,
			promotedCollectionTable, strings.Join(valueRows, ", "))
		slog.Debug("executing SQL", "func", "Promote", "op", "bulk_insert_promoted",
			"count", len(promotedToInsert))
		if _, err := tx.ExecContext(txCtx, bulkQ, args...); err != nil {
			return nil, nil, nil, fmt.Errorf("bulk insert promoted rows (n=%d): %w", len(promotedToInsert), err)
		}
	}

	// Per-row UPDATE for existing promoted_collection rows.
	for _, pr := range promotedToUpdate {
		promotedArgs := []any{
			pr.row.userID, pr.row.agentID, pr.row.conversationID,
			pr.payloadJSON, pr.row.createdAt,
			datapackID, pr.row.id,
		}
		if hasAgentScope {
			promotedArgs = append(promotedArgs, scopedAgentID)
		}
		slog.Debug("executing SQL", "func", "Promote", "query", promotedUpdateQ, "args", promotedArgs)
		if _, err := tx.ExecContext(txCtx, promotedUpdateQ, promotedArgs...); err != nil {
			return nil, nil, nil, fmt.Errorf("update promoted row (memory_id=%s): %w", pr.row.id, err)
		}
	}

	// 6. Promote graph vertex embeddings referenced by the promoted memories.
	promotedGraphRows, err := r.promoteGraphVerticesNew(txCtx, tx, datapackID, fetched)
	if err != nil {
		return nil, nil, nil, err
	}

	// 7. Copy graph vertices from meko_multi_tenant_graph to meko_promoted_graph
	//    and build a (oldID -> newID) map. Cypher's CREATE always allocates a
	//    fresh id() so we have to capture and rewrite the rest of the data.
	nodeIDMap, err := copyVerticesToPromotedGraph(txCtx, tx, fetched)
	if err != nil {
		return nil, nil, nil, err
	}

	// 8. Copy graph edges, mapping endpoints through nodeIDMap.
	edgeIDMap, err := copyEdgesToPromotedGraph(txCtx, tx, fetched, nodeIDMap)
	if err != nil {
		return nil, nil, nil, err
	}

	// 9. Patch promoted_collection.payload (graph_nodes/graph_edges arrays)
	//    so promoted memories now reference the NEW ids in meko_promoted_graph.
	if err := patchPromotedPayloadIDs(txCtx, tx, datapackID, fetchedIDs, nodeIDMap, edgeIDMap); err != nil {
		return nil, nil, nil, err
	}

	// 10. Patch the promoted_graph_vertex_embeddings rows the previous step
	//     copied: they're currently keyed on the OLD ids and need to switch.
	if err := patchPromotedVertexEmbeddingIDs(txCtx, tx, datapackID, nodeIDMap); err != nil {
		return nil, nil, nil, err
	}

	// 11. Compute exclusive ids (private side) for this batch and clean them
	//     up. Reuses the same helpers as private-memory delete. Must run
	//     AFTER step 9 — promoted_collection now holds the NEW ids, so it
	//     won't accidentally appear to "share" the old ids with itself.
	//
	//     `targetIDs` come straight from the maps we built in steps 7 & 8:
	//     keys are the OLD ids (still in meko_multi_tenant_graph), which is
	//     exactly the set the shared lookup has to check. Passing them
	//     explicitly bounds the anti-join to just these ids instead of
	//     expanding every memory's payload in SQL.
	targetNodeIDs := make([]int64, 0, len(nodeIDMap))
	for oldID := range nodeIDMap {
		targetNodeIDs = append(targetNodeIDs, oldID)
	}
	targetEdgeIDs := make([]int64, 0, len(edgeIDMap))
	for oldID := range edgeIDMap {
		targetEdgeIDs = append(targetEdgeIDs, oldID)
	}
	exclusiveNodes, err := collectExclusiveGraphIDs(
		txCtx, tx, "graph_nodes", "node_graphid",
		datapackID, fetchedIDs, targetNodeIDs,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	exclusiveEdges, err := collectExclusiveGraphIDs(
		txCtx, tx, "graph_edges", "edge_graphid",
		datapackID, fetchedIDs, targetEdgeIDs,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cypherDeleteEdges(txCtx, tx, exclusiveEdges); err != nil {
		return nil, nil, nil, err
	}
	if err := cypherDeleteVertices(txCtx, tx, exclusiveNodes); err != nil {
		return nil, nil, nil, err
	}
	if err := deleteVertexEmbeddings(
		txCtx, tx, mem0GraphVertexEmbeddingsTable, datapackID, exclusiveNodes,
	); err != nil {
		return nil, nil, nil, err
	}

	// 12. Delete the just-promoted rows from mem0. We feed the full
	//     primary key (meko_datapack_id, meko_user_id, meko_agent_id,
	//     meko_created_at, id) for every row via unnest() instead of
	//     ANY($ids), so YB resolves each row to a point delete within
	//     the (hash-partitioned) datapack instead of scanning the
	//     partition for matching ids. All four per-row values were
	//     already fetched in step 1, so there's no extra read cost.
	deleteUserIDs := make([]string, len(fetched))
	deleteAgentIDs := make([]string, len(fetched))
	deleteCreatedAts := make([]time.Time, len(fetched))
	deleteIDs := make([]string, len(fetched))
	for i, row := range fetched {
		deleteUserIDs[i] = row.userID
		deleteAgentIDs[i] = row.agentID
		deleteCreatedAts[i] = row.createdAt
		deleteIDs[i] = row.id
	}
	deleteQ := fmt.Sprintf(`
		DELETE FROM %s m
		USING unnest($1::uuid[], $2::text[], $3::timestamptz[], $4::uuid[])
		   AS t(user_id, agent_id, created_at, id)
		WHERE m.meko_datapack_id = $5::uuid
		  AND m.meko_user_id    = t.user_id
		  AND m.meko_agent_id   = t.agent_id
		  AND m.meko_created_at = t.created_at
		  AND m.id              = t.id`, memoriesSourceTable)
	slog.Debug("executing SQL", "func", "Promote", "query", deleteQ,
		"row_count", len(fetched), "datapack_id", datapackID)
	deleteRes, err := tx.ExecContext(txCtx, deleteQ,
		pq.Array(deleteUserIDs), pq.Array(deleteAgentIDs),
		pq.Array(deleteCreatedAts), pq.Array(deleteIDs), datapackID)
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
		"copied_vertices_to_promoted_graph", len(nodeIDMap),
		"copied_edges_to_promoted_graph", len(edgeIDMap),
		"private_vertices_cleaned", len(exclusiveNodes),
		"private_edges_cleaned", len(exclusiveEdges),
	)

	return insertedIDs, updatedIDs, notFoundIDs, nil
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

// promoteGraphVerticesNew is the batched counterpart of
// promoteGraphVertices. Hidden behind the *New suffix until the
// revamped Promote flow (PromoteNew) ships — only PromoteNew calls it
// today. It runs the graph-embedding promotion in two batched
// round-trips (down from 2N when the older version loops per-row):
//
//  1. Collect the graph_node refs from mem0 payloads.
//  2. One bulk SELECT fetches every matching row from
//     meko_mem0_graph_vertex_embeddings.
//  3. One bulk INSERT … ON CONFLICT writes them all into
//     meko_promoted_graph_vertex_embeddings.
//
// The single-row helpers `fetchGraphVertex` and `insertPromotedGraphVertex`
// are kept in the file because the older promoteGraphVertices (called
// by main's Promote) still uses them.
func (r *PromoteMemoriesRepo) promoteGraphVerticesNew(
	ctx context.Context,
	tx *sql.Tx,
	datapackID string,
	fetched []mem0Row,
) (int64, error) {
	refs := collectGraphNodeRefs(fetched)
	if len(refs) == 0 {
		return 0, nil
	}

	// 2. Bulk SELECT — fetch all matching rows in one round-trip
	//    using parallel arrays joined via unnest().
	userIDs := make([]string, len(refs))
	agentIDs := make([]string, len(refs))
	graphNames := make([]string, len(refs))
	nodeIDs := make([]int64, len(refs))
	for i, ref := range refs {
		userIDs[i] = ref.UserID
		agentIDs[i] = ref.AgentID
		graphNames[i] = ref.GraphName
		nodeIDs[i] = ref.NodeGraphID
	}

	selectQ := fmt.Sprintf(`
		SELECT v.meko_datapack_id::text,
		       v.node_graphid,
		       v.meko_user_id::text,
		       v.meko_agent_id,
		       v.graph_name,
		       v.node_name,
		       v.embeddings::text,
		       v.meko_conversation_id::text,
		       v.created_at
		FROM %s v
		JOIN unnest($2::uuid[], $3::text[], $4::text[], $5::bigint[])
		     AS r(user_id, agent_id, graph_name, node_graphid)
		  ON v.meko_user_id = r.user_id
		 AND v.meko_agent_id = r.agent_id
		 AND v.graph_name    = r.graph_name
		 AND v.node_graphid  = r.node_graphid
		WHERE v.meko_datapack_id = $1::uuid`,
		mem0GraphVertexEmbeddingsTable)

	slog.Debug("executing SQL", "func", "promoteGraphVertices", "op", "bulk_select",
		"refs", len(refs))
	rows, err := tx.QueryContext(ctx, selectQ,
		datapackID,
		pq.Array(userIDs), pq.Array(agentIDs),
		pq.Array(graphNames), pq.Array(nodeIDs),
	)
	if err != nil {
		return 0, fmt.Errorf("bulk select graph vertices: %w", err)
	}
	var vertices []graphVertexRow
	for rows.Next() {
		var v graphVertexRow
		if err := rows.Scan(
			&v.DatapackID, &v.NodeGraphID, &v.UserID, &v.AgentID,
			&v.GraphName, &v.NodeName, &v.EmbeddingsText, &v.ConversationID, &v.CreatedAt,
		); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan graph vertex: %w", err)
		}
		vertices = append(vertices, v)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close graph vertex rows: %w", err)
	}
	if len(vertices) == 0 {
		return 0, nil
	}

	// 3. Bulk INSERT — single multi-VALUES with ON CONFLICT.
	args := make([]any, 0, len(vertices)*9)
	valueRows := make([]string, 0, len(vertices))
	for i, v := range vertices {
		b := i*9 + 1
		valueRows = append(valueRows,
			fmt.Sprintf("($%d::uuid, $%d, $%d::uuid, $%d, $%d, $%d, $%d::vector, $%d::uuid, $%d, NOW())",
				b, b+1, b+2, b+3, b+4, b+5, b+6, b+7, b+8))
		var nodeName any
		if v.NodeName.Valid {
			nodeName = v.NodeName.String
		}
		var conversationID any
		if v.ConversationID.Valid {
			conversationID = v.ConversationID.String
		}
		args = append(args,
			v.DatapackID, v.NodeGraphID, v.UserID, v.AgentID,
			v.GraphName, nodeName, v.EmbeddingsText, conversationID, v.CreatedAt,
		)
	}
	insertQ := fmt.Sprintf(`
		INSERT INTO %s (
			meko_datapack_id, node_graphid, meko_user_id, meko_agent_id,
			graph_name, node_name, embeddings, meko_conversation_id,
			created_at, meko_prompted_at
		) VALUES %s
		ON CONFLICT (meko_datapack_id, node_graphid) DO UPDATE SET
			meko_user_id         = EXCLUDED.meko_user_id,
			meko_agent_id        = EXCLUDED.meko_agent_id,
			graph_name           = EXCLUDED.graph_name,
			node_name            = EXCLUDED.node_name,
			embeddings           = EXCLUDED.embeddings,
			meko_conversation_id = EXCLUDED.meko_conversation_id,
			created_at           = EXCLUDED.created_at,
			meko_prompted_at     = NOW()`,
		promotedGraphVertexEmbeddingsTable, strings.Join(valueRows, ", "))

	slog.Debug("executing SQL", "func", "promoteGraphVertices", "op", "bulk_insert",
		"count", len(vertices))
	if _, err := tx.ExecContext(ctx, insertQ, args...); err != nil {
		return 0, fmt.Errorf("bulk insert promoted graph vertices (n=%d): %w", len(vertices), err)
	}
	return int64(len(vertices)), nil
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

// copyVerticesToPromotedGraph copies the graph vertices referenced by the
// promoted memories (deduped) from meko_multi_tenant_graph to
// meko_promoted_graph and returns an oldID -> newID map. We read each
// source vertex's properties via SQL, then CREATE in the destination
// graph and capture the freshly-allocated id().
func copyVerticesToPromotedGraph(
	ctx context.Context,
	tx *sql.Tx,
	fetched []mem0Row,
) (map[int64]int64, error) {
	srcIDs := uniqueNodeIDs(fetched)
	if len(srcIDs) == 0 {
		return map[int64]int64{}, nil
	}

	// Read properties for every source vertex in one round-trip.
	readQ := `SELECT (id::text)::bigint AS gid, properties::text
	          FROM meko_multi_tenant_graph."Entity"
	          WHERE (id::text)::bigint = ANY($1::bigint[])`
	rows, err := tx.QueryContext(ctx, readQ, pq.Array(srcIDs))
	if err != nil {
		return nil, fmt.Errorf("read source vertices: %w", err)
	}
	defer rows.Close()
	type srcVertex struct {
		id    int64
		props string
	}
	var sources []srcVertex
	for rows.Next() {
		var v srcVertex
		if err := rows.Scan(&v.id, &v.props); err != nil {
			return nil, fmt.Errorf("scan source vertex: %w", err)
		}
		sources = append(sources, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate source vertices: %w", err)
	}

	// Application-side dedup: MAGE's MERGE is intrinsically slow even on
	// an empty graph (per-statement parse/plan + table walk with no
	// usable property index). We replace MERGE with:
	//   1. One SELECT against meko_promoted_graph."Entity" to find any
	//      already-existing entities matching this batch's
	//      (conversation_id, name) pairs for this datapack.
	//   2. Plain CREATE (which is fast) for the remaining names that
	//      have no existing node.
	//
	// Dedup key: (meko_datapack_id, meko_conversation_id, name). User /
	// agent are constant per Promote call. conversation_id varies per
	// memory and is part of the key so anchors from different
	// conversations stay distinct. Same effective behaviour as the old
	// MERGE pattern, without paying MERGE's cost.
	idMap := make(map[int64]int64, len(sources))
	if len(sources) == 0 {
		return idMap, nil
	}

	// Parse all source properties once, build dedup keys.
	type sourceInfo struct {
		id       int64
		props    map[string]any
		convID   string
		name     string
		dedupKey string // conv_id + "\x00" + name
	}
	parsed := make([]sourceInfo, 0, len(sources))
	uniqueKeys := make(map[string]struct{ convID, name string }, len(sources))
	var datapackID string
	for _, s := range sources {
		var propsMap map[string]any
		if err := json.Unmarshal([]byte(s.props), &propsMap); err != nil {
			return nil, fmt.Errorf("parse source vertex props (old_id=%d): %w", s.id, err)
		}
		name, _ := propsMap["name"].(string)
		convID, _ := propsMap["meko_conversation_id"].(string)
		if datapackID == "" {
			if dp, ok := propsMap["meko_datapack_id"].(string); ok {
				datapackID = dp
			}
		}
		key := convID + "\x00" + name
		parsed = append(parsed, sourceInfo{
			id: s.id, props: propsMap, convID: convID, name: name, dedupKey: key,
		})
		if _, seen := uniqueKeys[key]; !seen {
			uniqueKeys[key] = struct{ convID, name string }{convID, name}
		}
	}

	// 1. SELECT existing entities for these (conv_id, name) pairs in
	//    the current datapack. Empty result on a fresh datapack → zero
	//    cost beyond the query overhead.
	existing := make(map[string]int64, len(uniqueKeys))
	if datapackID != "" && len(uniqueKeys) > 0 {
		convArr := make([]string, 0, len(uniqueKeys))
		nameArr := make([]string, 0, len(uniqueKeys))
		for _, kv := range uniqueKeys {
			convArr = append(convArr, kv.convID)
			nameArr = append(nameArr, kv.name)
		}
		selectQ := `
			SELECT (e.id::text)::bigint AS gid,
			       e.meko_conversation_id::text AS conv,
			       (e.properties::text::jsonb)->>'name' AS name
			FROM meko_promoted_graph."Entity" e
			JOIN unnest($1::text[], $2::text[]) AS k(conv_id, name)
			  ON e.meko_conversation_id::text = k.conv_id
			 AND (e.properties::text::jsonb)->>'name' = k.name
			WHERE e.meko_datapack_id = $3::uuid`
		slog.Debug("executing SQL", "func", "copyVerticesToPromotedGraph",
			"op", "dedup_select", "unique_keys", len(uniqueKeys))
		exRows, err := tx.QueryContext(ctx, selectQ,
			pq.Array(convArr), pq.Array(nameArr), datapackID)
		if err != nil {
			return nil, fmt.Errorf("select existing promoted entities: %w", err)
		}
		err = func() error {
			defer exRows.Close()
			for exRows.Next() {
				var gid int64
				var conv, name string
				if err := exRows.Scan(&gid, &conv, &name); err != nil {
					return fmt.Errorf("scan existing promoted entity: %w", err)
				}
				existing[conv+"\x00"+name] = gid
			}
			if err := exRows.Err(); err != nil {
				return fmt.Errorf("iterate existing promoted entities: %w", err)
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}
	}

	// 2. Wire existing ids into the map; collect remaining indices for
	//    CREATE. Multiple source vertices can share the same dedup
	//    key (e.g. session-anchor referenced by multiple memories) —
	//    all of them point at the one existing/created id.
	toCreate := make([]int, 0, len(parsed))
	seenKey := make(map[string]int, len(parsed)) // dedup_key -> first toCreate index for it
	for i, p := range parsed {
		if exID, ok := existing[p.dedupKey]; ok {
			idMap[p.id] = exID
			continue
		}
		if _, alreadyQueued := seenKey[p.dedupKey]; alreadyQueued {
			// Same key already queued for CREATE in this batch — defer;
			// fill in the id after CREATE returns.
			continue
		}
		seenKey[p.dedupKey] = i
		toCreate = append(toCreate, i)
	}

	// 3. Batch CREATE for the missing ones. Uses plain Cypher CREATE
	//    (no MERGE) — N inline CREATE patterns in one statement,
	//    returning the new ids as a list.
	if len(toCreate) > 0 {
		patterns := make([]string, 0, len(toCreate))
		returns := make([]string, 0, len(toCreate))
		for newIdx, sourceIdx := range toCreate {
			patterns = append(patterns,
				fmt.Sprintf("(n%d:Entity %s)", newIdx, cypherMapLiteral(parsed[sourceIdx].props)))
			returns = append(returns, fmt.Sprintf("id(n%d)", newIdx))
		}
		tag := cypherDollarTag()
		createQ := fmt.Sprintf(`SELECT ids::text FROM cypher('meko_promoted_graph', %[1]s
		                         CREATE %[2]s
		                         RETURN [%[3]s] AS ids
		                       %[1]s) AS (ids agtype)`,
			tag, strings.Join(patterns, ", "), strings.Join(returns, ", "))
		slog.Debug("executing SQL", "func", "copyVerticesToPromotedGraph",
			"op", "batch_create", "count", len(toCreate))
		var idsText string
		if err := tx.QueryRowContext(ctx, createQ).Scan(&idsText); err != nil {
			return nil, fmt.Errorf("cypher batch create vertices: %w", err)
		}
		newIDs, err := parseAgtypeIDList(idsText)
		if err != nil {
			return nil, fmt.Errorf("parse created vertex ids %q: %w", idsText, err)
		}
		if len(newIDs) != len(toCreate) {
			return nil, fmt.Errorf("batch create vertices: expected %d ids, got %d (%q)", len(toCreate), len(newIDs), idsText)
		}
		for newIdx, sourceIdx := range toCreate {
			idMap[parsed[sourceIdx].id] = newIDs[newIdx]
		}
	}

	// 4. Fill in idMap entries for the within-batch duplicates we
	//    deferred above. They all share the same dedup key as the
	//    representative we already mapped.
	for _, p := range parsed {
		if _, mapped := idMap[p.id]; mapped {
			continue
		}
		// Find the representative source for this dedup key.
		if exID, ok := existing[p.dedupKey]; ok {
			idMap[p.id] = exID
			continue
		}
		if representativeIdx, ok := seenKey[p.dedupKey]; ok {
			idMap[p.id] = idMap[parsed[representativeIdx].id]
			continue
		}
		return nil, fmt.Errorf("internal: no id resolved for source %d (key=%q)", p.id, p.dedupKey)
	}

	slog.Debug("copyVerticesToPromotedGraph",
		"sources", len(parsed),
		"existing_reused", len(parsed)-len(toCreate),
		"created", len(toCreate))
	return idMap, nil
}

// mergeKeyProps are the property keys that go INTO the MERGE pattern
// — `name` is what we'd ideally use alone, but MAGE enforces that
// every Entity insert carries the tenant columns (meko_user_id /
// meko_agent_id / meko_conversation_id) so the new-node case fails
// the NOT NULL check on the underlying table. Including them in the
// match pattern keeps MERGE's CREATE branch happy.
//
// Side effect: dedup is now per (name, datapack, user, agent,
// conversation) rather than (name, datapack). In practice this still
// dedups across multiple promotes within the same chat conversation
// (the common case). The same entity name surfacing in two different
// conversations becomes two nodes — arguably correct since they may
// refer to different real-world entities.
var mergeKeyProps = []string{
	"name",
	"meko_datapack_id",
	"meko_user_id",
	"meko_agent_id",
	"meko_conversation_id",
}

// cypherMergeStatement renders one
//
//	MERGE (n<i>:Entity {<all tenant + name properties>})
//	SET n<i>.<other_key> = <value>, …
//
// statement. If `name` is missing we fall back to MERGE on the full
// property map (CREATE-like behaviour) so we don't crash on
// unexpected payloads.
//
// We use plain SET (not ON CREATE SET) because MAGE rejects the
// ON CREATE clause. Trade-off: on a re-promote, non-key properties
// get overwritten with the current memory's values. Identity (the
// merge-key tuple) stays the same.
func cypherMergeStatement(idx int, props map[string]any) string {
	name, hasName := props["name"].(string)
	if !hasName || name == "" {
		return fmt.Sprintf("MERGE (n%d:Entity %s)", idx, cypherMapLiteral(props))
	}

	mergeKeySet := make(map[string]bool, len(mergeKeyProps))
	keyPairs := make([]string, 0, len(mergeKeyProps))
	for _, k := range mergeKeyProps {
		v, ok := props[k]
		if !ok {
			continue
		}
		mergeKeySet[k] = true
		keyPairs = append(keyPairs, fmt.Sprintf("%s: %s", cypherIdentifier(k), cypherValueLiteral(v)))
	}
	mergeKey := "{" + strings.Join(keyPairs, ", ") + "}"

	// Per-property SET clauses for non-key properties.
	keys := make([]string, 0, len(props))
	for k := range props {
		if mergeKeySet[k] {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return fmt.Sprintf("MERGE (n%d:Entity %s)", idx, mergeKey)
	}
	sort.Strings(keys)
	setClauses := make([]string, 0, len(keys))
	for _, k := range keys {
		setClauses = append(setClauses,
			fmt.Sprintf("n%d.%s = %s", idx, cypherIdentifier(k), cypherValueLiteral(props[k])))
	}
	return fmt.Sprintf("MERGE (n%d:Entity %s) SET %s",
		idx, mergeKey, strings.Join(setClauses, ", "))
}

// copyEdgesToPromotedGraph copies graph edges referenced by the promoted
// memories into meko_promoted_graph, translating endpoint ids through
// nodeIDMap. Returns an oldEdgeID -> newEdgeID map.
func copyEdgesToPromotedGraph(
	ctx context.Context,
	tx *sql.Tx,
	fetched []mem0Row,
	nodeIDMap map[int64]int64,
) (map[int64]int64, error) {
	srcEdgeIDs := uniqueEdgeIDs(fetched)
	if len(srcEdgeIDs) == 0 {
		return map[int64]int64{}, nil
	}

	readQ := `SELECT (id::text)::bigint AS eid,
	                 (start_id::text)::bigint AS sid,
	                 (end_id::text)::bigint AS tid,
	                 properties::text
	          FROM meko_multi_tenant_graph."CONNECTED_TO"
	          WHERE (id::text)::bigint = ANY($1::bigint[])`
	rows, err := tx.QueryContext(ctx, readQ, pq.Array(srcEdgeIDs))
	if err != nil {
		return nil, fmt.Errorf("read source edges: %w", err)
	}
	defer rows.Close()
	type srcEdge struct {
		id, srcNode, dstNode int64
		props                string
	}
	var sources []srcEdge
	for rows.Next() {
		var e srcEdge
		if err := rows.Scan(&e.id, &e.srcNode, &e.dstNode, &e.props); err != nil {
			return nil, fmt.Errorf("scan source edge: %w", err)
		}
		sources = append(sources, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate source edges: %w", err)
	}

	// Filter edges whose endpoints are both in the copy batch, then
	// build one UNWIND list and run a single Cypher batch CREATE.
	type batchEdge struct {
		oldID   int64
		srcNode int64
		dstNode int64
		props   string
	}
	var batched []batchEdge
	for _, e := range sources {
		newSrc, okS := nodeIDMap[e.srcNode]
		newDst, okD := nodeIDMap[e.dstNode]
		if !okS || !okD {
			// Endpoint wasn't in our copy batch — skip.
			continue
		}
		batched = append(batched, batchEdge{
			oldID: e.id, srcNode: newSrc, dstNode: newDst, props: e.props,
		})
	}
	edgeMap := make(map[int64]int64, len(batched))
	if len(batched) == 0 {
		return edgeMap, nil
	}

	// Same inline-CREATE approach as vertices, but each edge needs a
	// MATCH for its two endpoints by id. We MATCH them all at once
	// (one comma-separated MATCH with all variables) and CREATE all
	// the relationships in one statement. Returns ids as a list.
	matchVars := make([]string, 0, 2*len(batched))
	whereClauses := make([]string, 0, 2*len(batched))
	createPatterns := make([]string, 0, len(batched))
	returns := make([]string, 0, len(batched))
	for i, b := range batched {
		var propsMap map[string]any
		if err := json.Unmarshal([]byte(b.props), &propsMap); err != nil {
			return nil, fmt.Errorf("parse source edge props (old_id=%d): %w", b.oldID, err)
		}
		matchVars = append(matchVars, fmt.Sprintf("(a%d:Entity)", i), fmt.Sprintf("(b%d:Entity)", i))
		whereClauses = append(whereClauses,
			fmt.Sprintf("id(a%d) = %d", i, b.srcNode),
			fmt.Sprintf("id(b%d) = %d", i, b.dstNode),
		)
		createPatterns = append(createPatterns,
			fmt.Sprintf("(a%d)-[r%d:CONNECTED_TO %s]->(b%d)", i, i, cypherMapLiteral(propsMap), i))
		returns = append(returns, fmt.Sprintf("id(r%d)", i))
	}
	tag := cypherDollarTag()
	createQ := fmt.Sprintf(`SELECT ids::text FROM cypher('meko_promoted_graph', %[1]s
	                         MATCH %[2]s
	                         WHERE %[3]s
	                         CREATE %[4]s
	                         RETURN [%[5]s] AS ids
	                       %[1]s) AS (ids agtype)`,
		tag,
		strings.Join(matchVars, ", "),
		strings.Join(whereClauses, " AND "),
		strings.Join(createPatterns, ", "),
		strings.Join(returns, ", "),
	)

	var idsText string
	if err := tx.QueryRowContext(ctx, createQ).Scan(&idsText); err != nil {
		return nil, fmt.Errorf("cypher batch create edges: %w", err)
	}
	newIDs, err := parseAgtypeIDList(idsText)
	if err != nil {
		return nil, fmt.Errorf("parse new edge ids %q: %w", idsText, err)
	}
	if len(newIDs) != len(batched) {
		return nil, fmt.Errorf("batch create edges: expected %d ids, got %d (%q)", len(batched), len(newIDs), idsText)
	}
	for i, b := range batched {
		edgeMap[b.oldID] = newIDs[i]
	}
	slog.Debug("copyEdgesToPromotedGraph", "count", len(edgeMap))
	return edgeMap, nil
}

// patchPromotedPayloadIDs rewrites graph_nodes[].node_graphid and
// graph_edges[].edge_graphid (plus start/end_node_id) in
// promoted_collection.payload to point at the freshly-created ids in
// meko_promoted_graph. Reads each row, mutates in Go, writes back —
// JSONB array-element-rewrite in pure SQL is awkward.
func patchPromotedPayloadIDs(
	ctx context.Context,
	tx *sql.Tx,
	datapackID string,
	memoryIDs []string,
	nodeIDMap, edgeIDMap map[int64]int64,
) error {
	if len(memoryIDs) == 0 {
		return nil
	}
	readQ := fmt.Sprintf(`SELECT id::text, payload::text FROM %s
	                      WHERE id = ANY($1::uuid[]) AND meko_datapack_id = $2::uuid`,
		promotedCollectionTable)
	rows, err := tx.QueryContext(ctx, readQ, pq.Array(memoryIDs), datapackID)
	if err != nil {
		return fmt.Errorf("read promoted payloads: %w", err)
	}
	defer rows.Close()
	type pmRow struct {
		id      string
		payload string
	}
	var pms []pmRow
	for rows.Next() {
		var p pmRow
		if err := rows.Scan(&p.id, &p.payload); err != nil {
			return fmt.Errorf("scan promoted payload: %w", err)
		}
		pms = append(pms, p)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate promoted payloads: %w", err)
	}

	// Patch each row's payload JSON in Go, then UPDATE all rows in
	// ONE round-trip via unnest on parallel (id, payload) arrays.
	// Previously this was a per-row UPDATE in a loop — fine for one
	// memory, 5x the distributed-write cost for a 5-memory batch.
	ids := make([]string, 0, len(pms))
	payloads := make([]string, 0, len(pms))
	for _, p := range pms {
		var doc map[string]any
		if err := json.Unmarshal([]byte(p.payload), &doc); err != nil {
			return fmt.Errorf("parse promoted payload (id=%s): %w", p.id, err)
		}
		rewriteIDArray(doc, "graph_nodes", "node_graphid", nodeIDMap)
		rewriteEdgeArray(doc, "graph_edges", nodeIDMap, edgeIDMap)
		patched, err := json.Marshal(doc)
		if err != nil {
			return fmt.Errorf("marshal patched payload (id=%s): %w", p.id, err)
		}
		ids = append(ids, p.id)
		payloads = append(payloads, string(patched))
	}
	if len(ids) == 0 {
		return nil
	}
	updateQ := fmt.Sprintf(`
		UPDATE %s v
		SET payload = m.new_payload::jsonb
		FROM unnest($1::uuid[], $2::text[]) AS m(memory_id, new_payload)
		WHERE v.id = m.memory_id
		  AND v.meko_datapack_id = $3::uuid`,
		promotedCollectionTable)
	slog.Debug("executing SQL", "func", "patchPromotedPayloadIDs",
		"op", "bulk_update", "count", len(ids))
	if _, err := tx.ExecContext(ctx, updateQ, pq.Array(ids), pq.Array(payloads), datapackID); err != nil {
		return fmt.Errorf("batch update promoted payloads (n=%d): %w", len(ids), err)
	}
	return nil
}

// patchPromotedVertexEmbeddingIDs rewrites node_graphid on the rows
// promoteGraphVertices just inserted into the promoted embeddings
// table.
//
// MERGE complicates this: when MERGE finds an existing vertex (the
// entity was already promoted in a prior batch), the new_id we got
// back ALREADY has an embedding row from that earlier promote. A
// straight UPDATE old_id → new_id would then violate the
// (datapack_id, node_graphid) primary key. So we do two statements:
//
//  1. DELETE any temp row whose new_id already has an embedding —
//     the merged vertex's embedding lives there; this temp row is
//     redundant.
//  2. UPDATE the remaining temp rows from old_id → new_id (the
//     MERGE-created-new case where new_id has no row yet).
//
// Both statements run inside the caller's transaction.
func patchPromotedVertexEmbeddingIDs(
	ctx context.Context,
	tx *sql.Tx,
	datapackID string,
	nodeIDMap map[int64]int64,
) error {
	if len(nodeIDMap) == 0 {
		return nil
	}
	oldIDs := make([]int64, 0, len(nodeIDMap))
	newIDs := make([]int64, 0, len(nodeIDMap))
	for oldID, newID := range nodeIDMap {
		oldIDs = append(oldIDs, oldID)
		newIDs = append(newIDs, newID)
	}

	// 1. Drop temp rows whose new_id already exists in this datapack —
	//    they're duplicates of an earlier promote's embedding.
	deleteQ := fmt.Sprintf(`
		DELETE FROM %[1]s v
		WHERE v.meko_datapack_id = $3::uuid
		  AND v.node_graphid IN (
		    SELECT m.old_id
		    FROM unnest($1::bigint[], $2::bigint[]) AS m(old_id, new_id)
		    WHERE m.old_id <> m.new_id
		      AND EXISTS (
		        SELECT 1 FROM %[1]s v2
		        WHERE v2.meko_datapack_id = $3::uuid AND v2.node_graphid = m.new_id
		      )
		  )`,
		promotedGraphVertexEmbeddingsTable)
	if _, err := tx.ExecContext(ctx, deleteQ, pq.Array(oldIDs), pq.Array(newIDs), datapackID); err != nil {
		return fmt.Errorf("delete duplicate temp embeddings: %w", err)
	}

	// 2. UPDATE remaining temp rows from old_id → new_id.
	updateQ := fmt.Sprintf(`
		UPDATE %s v
		SET node_graphid = m.new_id
		FROM unnest($1::bigint[], $2::bigint[]) AS m(old_id, new_id)
		WHERE v.node_graphid = m.old_id
		  AND v.meko_datapack_id = $3::uuid`,
		promotedGraphVertexEmbeddingsTable)
	if _, err := tx.ExecContext(ctx, updateQ, pq.Array(oldIDs), pq.Array(newIDs), datapackID); err != nil {
		return fmt.Errorf("batch update promoted embeddings: %w", err)
	}
	return nil
}

// uniqueNodeIDs deduplicates node_graphid values across the batch's
// payload.graph_nodes[] arrays.
func uniqueNodeIDs(fetched []mem0Row) []int64 {
	seen := make(map[int64]bool)
	var out []int64
	for _, row := range fetched {
		if len(row.payload) == 0 {
			continue
		}
		var p struct {
			GraphNodes []struct {
				NodeGraphID int64 `json:"node_graphid"`
			} `json:"graph_nodes"`
		}
		if err := json.Unmarshal(row.payload, &p); err != nil {
			continue
		}
		for _, n := range p.GraphNodes {
			if n.NodeGraphID == 0 || seen[n.NodeGraphID] {
				continue
			}
			seen[n.NodeGraphID] = true
			out = append(out, n.NodeGraphID)
		}
	}
	return out
}

// uniqueEdgeIDs deduplicates edge_graphid values across the batch's
// payload.graph_edges[] arrays.
func uniqueEdgeIDs(fetched []mem0Row) []int64 {
	seen := make(map[int64]bool)
	var out []int64
	for _, row := range fetched {
		if len(row.payload) == 0 {
			continue
		}
		var p struct {
			GraphEdges []struct {
				EdgeGraphID int64 `json:"edge_graphid"`
			} `json:"graph_edges"`
		}
		if err := json.Unmarshal(row.payload, &p); err != nil {
			continue
		}
		for _, e := range p.GraphEdges {
			if e.EdgeGraphID == 0 || seen[e.EdgeGraphID] {
				continue
			}
			seen[e.EdgeGraphID] = true
			out = append(out, e.EdgeGraphID)
		}
	}
	return out
}

// rewriteIDArray walks doc[field] (an array of objects with key idKey)
// and replaces each id via the map. Missing/unmapped ids pass through
// unchanged so a partial map doesn't blow up valid rows.
func rewriteIDArray(doc map[string]any, field, idKey string, idMap map[int64]int64) {
	arr, ok := doc[field].([]any)
	if !ok {
		return
	}
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if oldID, ok := numberAsInt64(obj[idKey]); ok {
			if newID, mapped := idMap[oldID]; mapped {
				obj[idKey] = newID
			}
		}
	}
}

// rewriteEdgeArray walks doc.graph_edges[] and rewrites edge_graphid
// (via edgeMap) plus start_node_id / end_node_id (via nodeMap).
func rewriteEdgeArray(doc map[string]any, field string, nodeMap, edgeMap map[int64]int64) {
	arr, ok := doc[field].([]any)
	if !ok {
		return
	}
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if oldID, ok := numberAsInt64(obj["edge_graphid"]); ok {
			if newID, mapped := edgeMap[oldID]; mapped {
				obj["edge_graphid"] = newID
			}
		}
		if oldID, ok := numberAsInt64(obj["start_node_id"]); ok {
			if newID, mapped := nodeMap[oldID]; mapped {
				obj["start_node_id"] = newID
			}
		}
		if oldID, ok := numberAsInt64(obj["end_node_id"]); ok {
			if newID, mapped := nodeMap[oldID]; mapped {
				obj["end_node_id"] = newID
			}
		}
	}
}

// numberAsInt64 extracts an int64 from a JSON number (encoding/json
// decodes JSON numbers as float64 by default).
func numberAsInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// parseAgtypeIDList parses the text representation of an agtype list
// of integer ids into []int64. AGE/MAGE typically emits JSON-compatible
// `[123, 456]`, but versions vary (some include `::numeric` suffixes
// or other agtype-specific markers). We try strict JSON first and
// fall back to a regex digit-extract that ignores any extra noise.
func parseAgtypeIDList(s string) ([]int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return nil, nil
	}
	// Fast path: clean JSON list.
	var ids []int64
	if err := json.Unmarshal([]byte(s), &ids); err == nil {
		return ids, nil
	}
	// Strip agtype type-cast suffixes (`::vertex`, `::numeric(10,2)`,
	// etc.) and retry JSON. This catches the common MAGE output
	// `[1::numeric, 2::numeric]` while keeping the digits inside
	// parameterised type modifiers (e.g. the `10,2` in `numeric(10,2)`)
	// from being mistaken for ids.
	cleaned := agtypeCastSuffix.ReplaceAllString(s, "")
	if err := json.Unmarshal([]byte(cleaned), &ids); err == nil {
		return ids, nil
	}
	// Last-resort fallback: pull out every contiguous run of digits
	// after suffix-stripping. Reached only when the input is malformed
	// even after type stripping.
	out := make([]int64, 0, 8)
	cur := ""
	flush := func() {
		if cur == "" {
			return
		}
		if n, err := strconv.ParseInt(cur, 10, 64); err == nil {
			out = append(out, n)
		}
		cur = ""
	}
	for _, r := range cleaned {
		if r >= '0' && r <= '9' {
			cur += string(r)
			continue
		}
		flush()
	}
	flush()
	if len(out) == 0 {
		return nil, fmt.Errorf("no numeric ids found in agtype list %q", s)
	}
	return out, nil
}

// agtypeCastSuffix matches the `::typename` cast suffix MAGE emits in
// agtype lists, including optional parameterised type modifiers like
// `(10,2)`. Used by parseAgtypeIDList to normalise the input before
// JSON-decoding.
var agtypeCastSuffix = regexp.MustCompile(`::\w+(?:\([^)]*\))?`)

// cypherMapLiteral renders a Go map as a Cypher map literal, e.g.
// `{name: 'Raj', meko_datapack_id: '...'}`. Used to inline source
// vertex/edge properties into a CREATE statement so we don't have to
// fight AGE/MAGE's agtype parameter binding.
//
// Keys are emitted unquoted (Cypher identifier syntax). Values are
// emitted via cypherValueLiteral. Keys are sorted so the output is
// deterministic across runs (helps with diff-able logs).
func cypherMapLiteral(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %s", cypherIdentifier(k), cypherValueLiteral(m[k])))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// cypherDollarTag returns a fresh Postgres dollar-quote tag of the form
// `$cypher_<8-hex-random>$`. The Cypher body MUST be wrapped with the
// SAME tag at both ends. A static tag like `$cypher$` would be vulnerable
// to dollar-quote breakout if any inlined property value contains that
// literal string — randomising the tag per query makes such a collision
// astronomically unlikely (1 in 2^32).
func cypherDollarTag() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a time-based suffix; still astronomically unlikely
		// to collide with attacker-controlled property text. crypto/rand
		// failing on a healthy host is itself a bigger problem to log.
		slog.Warn("cypherDollarTag: crypto/rand failed, using time fallback", "error", err)
		return fmt.Sprintf("$cypher_%x$", time.Now().UnixNano())
	}
	return "$cypher_" + hex.EncodeToString(b[:]) + "$"
}

// cypherValueLiteral renders a single value in Cypher syntax. Strings
// are single-quoted with embedded single-quotes / backslashes escaped.
// Numbers / bools / null map straight across; nested maps and arrays
// recurse.
func cypherValueLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case string:
		esc := strings.ReplaceAll(x, `\`, `\\`)
		esc = strings.ReplaceAll(esc, `'`, `\'`)
		return "'" + esc + "'"
	case float64:
		// JSON numbers decode as float64 by default. If it's a whole
		// number, emit without a decimal point so Cypher reads it as
		// integer (avoids type drift on ids stored in properties).
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		return x.String()
	case map[string]any:
		return cypherMapLiteral(x)
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			parts = append(parts, cypherValueLiteral(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		// Fallback: JSON-encode and treat as a string. Should rarely fire
		// for real entity properties.
		b, _ := json.Marshal(x)
		esc := strings.ReplaceAll(string(b), `'`, `\'`)
		return "'" + esc + "'"
	}
}

// cypherIdentifier wraps a key in backticks if it isn't a plain
// identifier (letters, digits, underscores; not starting with a digit).
// Most meko property keys are plain — backticks are belt-and-suspenders
// for keys we don't control.
func cypherIdentifier(k string) string {
	if k == "" {
		return "``"
	}
	for i, r := range k {
		isAlpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
		isDigit := r >= '0' && r <= '9'
		if i == 0 && !isAlpha {
			return "`" + strings.ReplaceAll(k, "`", "``") + "`"
		}
		if !isAlpha && !isDigit {
			return "`" + strings.ReplaceAll(k, "`", "``") + "`"
		}
	}
	return k
}
