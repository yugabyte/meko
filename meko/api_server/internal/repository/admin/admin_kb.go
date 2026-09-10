package admin

import (
	"fmt"
	"time"

	"github.com/lib/pq"
)

// AdminKBCounts holds the COMPLETED-document and chunk totals used by
// the admin metrics endpoint.
type AdminKBCounts struct {
	Documents int64
	Chunks    int64
}

// completedDocsChunksSQL collapses dist_rag.vector_index_pipeline_details
// to the latest run per document (DISTINCT ON), keeps COMPLETED-only,
// then COUNTs documents and SUMs chunks_processed. The %s is an optional
// extra predicate (e.g. a source_id filter + range); pass "" for the
// global all-time scope.
func completedDocsChunksSQL(extraPredicate string) string {
	return fmt.Sprintf(`WITH latest_per_document AS (
	   SELECT DISTINCT ON (document_id)
	          document_id,
	          chunks_processed,
	          COALESCE(pipeline_status::text, document_status::text) AS status
	   FROM   dist_rag.vector_index_pipeline_details
	   WHERE  schema_name = $1 AND index_name = $2%s
	   ORDER  BY document_id,
	             pipeline_created_at DESC NULLS LAST,
	             pipeline_id DESC
	 ),
	 completed_documents AS (
	   SELECT document_id, chunks_processed
	   FROM   latest_per_document
	   WHERE  status = 'COMPLETED'
	 )
	 SELECT COUNT(*)::bigint,
	        COALESCE(SUM(chunks_processed), 0)::bigint
	 FROM   completed_documents`, extraPredicate)
}

// CountCompletedDocsAndChunksAll returns the global COMPLETED-document
// count and chunk sum across every datapack (overall admin metric).
// start/endExclusive optionally window by pipeline_created_at.
func (r *AdminRepo) CountCompletedDocsAndChunksAll(
	schema, indexName string, start, endExclusive *time.Time,
) (AdminKBCounts, error) {
	pred, rangeArgs := rangePredicate("pipeline_created_at", start, endExclusive, 3)
	args := append([]any{schema, indexName}, rangeArgs...)
	var c AdminKBCounts
	if err := r.dataDB.QueryRow(
		completedDocsChunksSQL(pred), args...,
	).Scan(&c.Documents, &c.Chunks); err != nil {
		return c, fmt.Errorf("kb admin counts (all): %w", err)
	}
	return c, nil
}

// CountCompletedDocsAndChunksBySourceIDs returns the COMPLETED-document
// count and chunk sum restricted to the given rag source ids (per-user
// admin metric). Callers pass an empty slice → zero without a query.
// start/endExclusive optionally window by pipeline_created_at.
func (r *AdminRepo) CountCompletedDocsAndChunksBySourceIDs(
	schema, indexName string, sourceIDs []string, start, endExclusive *time.Time,
) (AdminKBCounts, error) {
	var c AdminKBCounts
	if len(sourceIDs) == 0 {
		return c, nil
	}
	// source_id occupies $3; range params (if any) start at $4.
	pred, rangeArgs := rangePredicate("pipeline_created_at", start, endExclusive, 4)
	args := append([]any{schema, indexName, pq.Array(sourceIDs)}, rangeArgs...)
	if err := r.dataDB.QueryRow(
		completedDocsChunksSQL(" AND source_id = ANY($3::uuid[])"+pred),
		args...,
	).Scan(&c.Documents, &c.Chunks); err != nil {
		return c, fmt.Errorf("kb admin counts (by source ids): %w", err)
	}
	return c, nil
}

// KBDocTypeCount is one file-type bucket for the executive dashboard's
// "indexed documents by file type" chart. DocumentType is the stored MIME type
// (e.g. application/pdf, text/markdown); the UI maps it to a friendly label.
type KBDocTypeCount struct {
	DocumentType string
	Count        int64
}

// indexedDocsByTypeSQL collapses the pipeline to the latest run per document,
// keeps COMPLETED-only, joins dist_rag.documents for the file type, and groups.
// The %s is the optional pipeline_created_at range predicate.
func indexedDocsByTypeSQL(rangePred string) string {
	return fmt.Sprintf(`WITH latest_per_document AS (
	   SELECT DISTINCT ON (document_id)
	          document_id,
	          COALESCE(pipeline_status::text, document_status::text) AS status
	   FROM   dist_rag.vector_index_pipeline_details
	   WHERE  schema_name = $1 AND index_name = $2%s
	   ORDER  BY document_id,
	             pipeline_created_at DESC NULLS LAST,
	             pipeline_id DESC
	 ),
	 completed_documents AS (
	   SELECT document_id FROM latest_per_document WHERE status = 'COMPLETED'
	 )
	 SELECT COALESCE(NULLIF(d.document_type, ''), 'unknown') AS document_type,
	        COUNT(*)::bigint
	 FROM   completed_documents cd
	 LEFT JOIN dist_rag.documents d ON d.document_id = cd.document_id
	 GROUP  BY 1
	 ORDER  BY 2 DESC`, rangePred)
}

// CountIndexedDocsByTypeAll returns the COMPLETED-document count grouped by file
// type across every datapack, optionally windowed by pipeline_created_at. One
// row per distinct document type (no bucketing).
func (r *AdminRepo) CountIndexedDocsByTypeAll(
	schema, indexName string, start, endExclusive *time.Time,
) ([]KBDocTypeCount, error) {
	pred, rangeArgs := rangePredicate("pipeline_created_at", start, endExclusive, 3)
	args := append([]any{schema, indexName}, rangeArgs...)
	rows, err := r.dataDB.Query(indexedDocsByTypeSQL(pred), args...)
	if err != nil {
		return nil, fmt.Errorf("kb docs by type (all): %w", err)
	}
	defer rows.Close()

	out := []KBDocTypeCount{}
	for rows.Next() {
		var c KBDocTypeCount
		if err := rows.Scan(&c.DocumentType, &c.Count); err != nil {
			return nil, fmt.Errorf("scan kb docs by type: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetSourceIDsByOwner returns rag_source_ids across every datapack owned
// by `userID`. Used to scope per-user KB admin metrics to the data plane.
func (r *AdminRepo) GetSourceIDsByOwner(userID string) ([]string, error) {
	rows, err := r.systemDB.Query(
		`SELECT kb.rag_source_id
		 FROM   meko_system.knowledge_bases kb
		 JOIN   meko_system.datapacks      dp ON dp.datapack_id = kb.datapack_id
		 WHERE  dp.user_id = $1`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("kb source ids for owner %s: %w", userID, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan source id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
