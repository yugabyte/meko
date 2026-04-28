package repository

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Fixed datapack-side tables this repo bridges. All live in the shared
// datapacks schema (see migration 000002). The previous per-tenant table-name
// plumbing is gone now that the root connection has cross-tenant access and
// the schema is fixed.
const (
	memoriesSourceTable     = `datapacks.meko_mem0_collection`
	knowledgeBaseTable      = `datapacks.knowledge_bases`
	promotedCollectionTable = `datapacks.promoted_collection`
)

// Values for datapacks.knowledge_bases.metadata_filters->>'source' — used to
// differentiate where each KB chunk originated. Today every chunk this code
// writes/reads originates from a conversation; additional sources can be
// added here as new ingest paths land. All KB queries default-filter by this
// value so cross-source bleed is impossible.
const KBSourceConversation = "conversation"

type PromoteMemoriesRepo struct {
	db *sql.DB
}

func NewPromoteMemoriesRepo(db *sql.DB) *PromoteMemoriesRepo {
	return &PromoteMemoriesRepo{db: db}
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
//	document_id      = mem0.id                                 (= memory_id)
//	metadata_filters = {"source": "conversation", "agent_id": <meko_agent_id>}
//
// All KB reads/writes here default-filter by
// metadata_filters->>'source' = 'conversation' so rows from any other
// ingest paths added later are invisible to this flow.
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
	// the KB upsert (chunk_text, embeddings) and the promoted_collection
	// upsert (full payload, conversation_id, user_id, created_at) need.
	// Scoping is by datapack_id + the caller's explicit memory_ids list;
	// meko_user_id is intentionally not filtered, and per-agent scoping
	// happens on the KB side via metadata_filters->>'agent_id'.
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
		  AND m.meko_promotion_user_action IS DISTINCT FROM 'keep_as_memory'::datapacks.meko_promotion_user_action`,
		memoriesSourceTable)
	fetchArgs := []any{datapackID, pq.Array(memoryIDs)}

	slog.Debug("executing SQL", "func", "Promote", "query", fetchQ, "args", fetchArgs)
	rows, err := tx.QueryContext(txCtx, fetchQ, fetchArgs...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fetch mem0 rows: %w", err)
	}

	type mem0Row struct {
		id             string
		chunkText      string
		vectorText     string
		agentID        string
		userID         string
		conversationID sql.NullString
		payload        []byte
		createdAt      time.Time
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
	// document_id holds the source memory_id (we no longer keep it in
	// metadata_filters). When the caller supplies an agent_id, scope the
	// lookup to that agent via metadata_filters->>'agent_id' so existence is
	// per-agent. Always default-filter by source = 'conversation'.
	fetchedIDs := make([]string, len(fetched))
	for i, row := range fetched {
		fetchedIDs[i] = row.id
	}

	scopedAgentID := strings.TrimSpace(agentID)
	hasAgentScope := scopedAgentID != ""

	existConds := []string{
		"tenant_id = $1::uuid",
		"document_id = ANY($2::uuid[])",
		"metadata_filters->>'source' = '" + KBSourceConversation + "'",
	}
	existArgs := []any{datapackID, pq.Array(fetchedIDs)}
	if hasAgentScope {
		existConds = append(existConds, "metadata_filters->>'agent_id' = $3")
		existArgs = append(existArgs, scopedAgentID)
	}
	existQ := fmt.Sprintf(`
		SELECT document_id::text
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
	//    document_id, additionally scoped by metadata_filters->>'agent_id'
	//    when provided). All writes/reads default-scope by source = 'conversation'.
	insertQ := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, chunk_text, embeddings, document_id, metadata_filters)
		VALUES ($1::uuid, $2, $3::vector, $4::uuid, $5::jsonb)`, knowledgeBaseTable)

	updateConds := []string{
		"tenant_id = $4::uuid",
		"document_id = $5::uuid",
		"metadata_filters->>'source' = '" + KBSourceConversation + "'",
	}
	if hasAgentScope {
		updateConds = append(updateConds, "metadata_filters->>'agent_id' = $6")
	}
	updateQ := fmt.Sprintf(`
		UPDATE %s
		SET chunk_text       = $1,
		    embeddings       = $2::vector,
		    metadata_filters = $3::jsonb
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
		metadataJSON := fmt.Sprintf(`{"source":%q,"agent_id":%q}`, KBSourceConversation, row.agentID)
		payloadJSON := row.payload
		if len(payloadJSON) == 0 {
			payloadJSON = []byte("null")
		}
		// nullable conversation_id — nil interface{} sends as SQL NULL.
		var convID any
		if row.conversationID.Valid {
			convID = row.conversationID.String
		}

		if existingSet[row.id] {
			updateArgs := []any{row.chunkText, row.vectorText, metadataJSON, datapackID, row.id}
			if hasAgentScope {
				updateArgs = append(updateArgs, scopedAgentID)
			}
			slog.Debug("executing SQL", "func", "Promote", "query", updateQ, "args", updateArgs)
			if _, err := tx.ExecContext(txCtx, updateQ, updateArgs...); err != nil {
				return nil, nil, nil, fmt.Errorf("update kb row (memory_id=%s): %w", row.id, err)
			}
			updatedIDs = append(updatedIDs, row.id)
		} else {
			slog.Debug("executing SQL", "func", "Promote", "query", insertQ,
				"args", []any{datapackID, row.chunkText, row.vectorText, row.id, metadataJSON})
			if _, err := tx.ExecContext(txCtx, insertQ,
				datapackID, row.chunkText, row.vectorText, row.id, metadataJSON,
			); err != nil {
				return nil, nil, nil, fmt.Errorf("insert kb row (memory_id=%s): %w", row.id, err)
			}
			insertedIDs = append(insertedIDs, row.id)
		}

		// Mirror the row into promoted_collection.
		if promotedSet[row.id] {
			promotedArgs := []any{
				row.userID, row.agentID, convID, payloadJSON, row.createdAt,
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
				convID, payloadJSON, row.createdAt,
			}
			slog.Debug("executing SQL", "func", "Promote", "query", promotedInsertQ, "args", promotedArgs)
			if _, err := tx.ExecContext(txCtx, promotedInsertQ, promotedArgs...); err != nil {
				return nil, nil, nil, fmt.Errorf("insert promoted row (memory_id=%s): %w", row.id, err)
			}
		}
	}

	// 6. Delete the just-promoted rows from mem0. Ownership of the memory
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
	)

	return insertedIDs, updatedIDs, notFoundIDs, nil
}
