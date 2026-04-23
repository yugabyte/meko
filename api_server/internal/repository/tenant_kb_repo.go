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

const DefaultTenantKBTable = "knowledge_base"

type TenantKBRepo struct {
	db          *sql.DB
	tenantKBSQL string
}

func NewTenantKBRepo(db *sql.DB, tenantKBTable string) *TenantKBRepo {
	kbSQL, _ := quoteMemoriesCollection(tenantKBTable)
	if kbSQL == "" {
		kbSQL = fmt.Sprintf(`"public"."%s"`, DefaultTenantKBTable)
	}
	return &TenantKBRepo{db: db, tenantKBSQL: kbSQL}
}

// SyncMemories reads rows from the mem0 collection by IDs + user_id + datapack_id,
// then upserts them into knowledge_base.
//
// Mapping:
//
//	knowledge_base.id          = gen_random_uuid()  (new) or existing (update)
//	knowledge_base.chunk_text  = mem0.payload->>'data'
//	knowledge_base.embeddings  = mem0.vector
//	knowledge_base.document_id = 00000000-0000-0000-0000-000000000000
//	knowledge_base.metadata_filters = {"agent_id": ..., "memory_id": mem0.id}
//
// memory_id inside metadata_filters tracks the mem0 source row so we
// can detect existing vs new on subsequent calls.
// @note: user_id & datapack_id are needed when the multi-tenant is enabled.
func (r *TenantKBRepo) SyncMemories(
	ctx context.Context,
	userID string,
	agentID string,
	datapackID string,
	memoryIDs []string,
) (insertedIDs, updatedIDs, notFoundIDs []string, err error) {
	if r.db == nil {
		return nil, nil, nil, fmt.Errorf("database connection is required")
	}
	mem0DB, err := getMem0DBConn(ctx, r.db, datapackID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open mem0 connection: %w", err)
	}
	defer mem0DB.Close()

	txCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	tx, err := mem0DB.BeginTx(txCtx, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Resolve which mem0 collection(s) to fetch from.
	collections, err := getMemoriesCollections(ctx, mem0DB, agentID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve mem0 collections: %w", err)
	}
	if len(collections) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, nil, nil, err
		}
		return nil, nil, memoryIDs, nil
	}

	// Build fetch query: UNION ALL across all matching collections.
	var fetchArgs []any
	fetchArgs = append(fetchArgs, pq.Array(memoryIDs))
	userCond := ""

	subSelects := make([]string, len(collections))
	for i, tbl := range collections {
		subSelects[i] = fmt.Sprintf(`
	SELECT m.id::text,
		COALESCE(m.payload->>'data', ''),
		m.vector::text,
		COALESCE(m.payload->>'agent_id', '')
	FROM %s m
	WHERE m.id = ANY($1::uuid[])%s
	AND m.vector IS NOT NULL`, tbl, userCond)
	}
	fetchQ := strings.Join(subSelects, "\nUNION ALL\n")

	slog.Debug("executing SQL", "func", "SyncMemories", "query", fetchQ, "args", fetchArgs)
	rows, err := mem0DB.QueryContext(txCtx, fetchQ, fetchArgs...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fetch mem0 rows: %w", err)
	}

	type mem0Row struct {
		id         string
		chunkText  string
		vectorText string
		agentID    string
	}

	var fetched []mem0Row
	for rows.Next() {
		var r mem0Row
		if err := rows.Scan(&r.id, &r.chunkText, &r.vectorText, &r.agentID); err != nil {
			_ = rows.Close()
			return nil, nil, nil, fmt.Errorf("scan mem0 row: %w", err)
		}
		fetched = append(fetched, r)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, nil, fmt.Errorf("close mem0 rows: %w", err)
	}

	fetchedSet := make(map[string]bool, len(fetched))
	for _, r := range fetched {
		fetchedSet[r.id] = true
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

	// 2. Check which mem0 IDs already exist in knowledge_base
	fetchedIDs := make([]string, len(fetched))
	for i, r := range fetched {
		fetchedIDs[i] = r.id
	}

	existQ := fmt.Sprintf(`
		SELECT metadata_filters->>'memory_id'
		FROM %s
		WHERE metadata_filters->>'memory_id' = ANY($1::text[])`, r.tenantKBSQL)

	slog.Debug("executing SQL", "func", "SyncMemories", "query", existQ, "args", fetchedIDs)
	existRows, err := tx.QueryContext(txCtx, existQ, pq.Array(fetchedIDs))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("check existing: %w", err)
	}

	existingSet := make(map[string]bool)
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

	// 3. Insert new rows, update existing rows.
	insertQ := fmt.Sprintf(`
		INSERT INTO %s (chunk_text, embeddings, document_id, metadata_filters)
		VALUES ($1, $2::vector, '00000000-0000-0000-0000-000000000000'::uuid, $3::jsonb)`, r.tenantKBSQL)

	updateQ := fmt.Sprintf(`
		UPDATE %s
		SET chunk_text = $1,
			embeddings = $2::vector,
			metadata_filters = $3::jsonb
		WHERE metadata_filters->>'memory_id' = $4`, r.tenantKBSQL)

	for _, row := range fetched {
		metadataJSON := fmt.Sprintf(
			`{"memory_id":%q,"agent_id":%q}`,
			row.id, row.agentID,
		)

		if existingSet[row.id] {
			slog.Debug("executing SQL", "func", "SyncMemories", "query", updateQ, "args", []any{row.chunkText, row.vectorText, metadataJSON, row.id})
			if _, err := tx.ExecContext(txCtx, updateQ,
				row.chunkText, row.vectorText, metadataJSON, row.id,
			); err != nil {
				return nil, nil, nil, fmt.Errorf("update kb row (memory_id=%s): %w", row.id, err)
			}
			updatedIDs = append(updatedIDs, row.id)
		} else {
			slog.Debug("executing SQL", "func", "SyncMemories", "query", insertQ, "args", []any{row.chunkText, row.vectorText, metadataJSON})
			if _, err := tx.ExecContext(txCtx, insertQ,
				row.chunkText, row.vectorText, metadataJSON,
			); err != nil {
				return nil, nil, nil, fmt.Errorf("insert kb row (memory_id=%s): %w", row.id, err)
			}
			insertedIDs = append(insertedIDs, row.id)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, nil, fmt.Errorf("commit: %w", err)
	}

	slog.Info("tenant kb sync complete",
		"user_id", userID,
		"datapack_id", datapackID,
		"requested", len(memoryIDs),
		"inserted", len(insertedIDs),
		"updated", len(updatedIDs),
		"not_found", len(notFoundIDs),
	)

	return insertedIDs, updatedIDs, notFoundIDs, nil
}
