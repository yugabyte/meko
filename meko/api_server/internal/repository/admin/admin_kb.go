package admin

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// kbStmtTimeout bounds the KB analytics queries. dist_rag.pipeline_details has no
// index on created_at yet (only PK + document_id), so a wide window still scans
// the table; the timeout turns a runaway into a fast, clean failure the caller
// swallows (empty panel) instead of a request that hangs until the gateway 504s.
// Drop/raise once dist_rag adds (document_id, created_at DESC).
const kbStmtTimeout = "30s"

// AdminKBCounts holds the COMPLETED-document and chunk totals used by
// the admin metrics endpoint.
type AdminKBCounts struct {
	Documents int64
	Chunks    int64
}

// completedDocsChunksSQL collapses the pipeline to the latest run per document
// (DISTINCT ON), keeps COMPLETED-only, then COUNTs documents and SUMs
// chunks_processed. sourceFilter is "" for the global scope, or the documents
// join + source_id filter for the per-user scope (see kbSpine).
func completedDocsChunksSQL(sourceFilter, rangePred string) string {
	return `WITH latest_per_document AS (` + kbSpine(sourceFilter, rangePred) + `
	 ),
	 completed_documents AS (
	   SELECT document_id, chunks_processed
	   FROM   latest_per_document
	   WHERE  status = 'COMPLETED'
	 )
	 SELECT COUNT(*)::bigint,
	        COALESCE(SUM(chunks_processed), 0)::bigint
	 FROM   completed_documents`
}

// kbSpine is the shared "latest COMPLETED pipeline run per document" scan, read
// from the base dist_rag tables (pipeline_details, joined to documents only when
// a source filter is needed) rather than the vector_index_pipeline_details VIEW.
//
// The view is a 5-table join carrying a mandatory ORDER BY over its whole result,
// so every query against it sorted the entire dataset before our WHERE/DISTINCT
// applied — the cause of the exec knowledge panel's timeouts (MEKO-728 follow-up).
// Meko runs a single global index (datapacks/knowledge_bases) to which every KB
// source maps, so scanning pipeline_details directly (optionally filtered to a
// user's sources via documents.source_id) counts the same rows the view's
// index-scoped join did — without the join and the global sort. If Meko ever runs
// more than one dist_rag index this must re-scope by index.
//
// Semantics note: the old view LEFT JOINed pipeline_details, so a document with
// NO pipeline run still appeared and its documents.status decided COMPLETED (the
// COALESCE(pipeline_status, document_status) fallback). This spine reads
// pipeline_details directly, so "indexed" means "has ≥1 pipeline run" — a run is
// what produces chunks and the only per-document timestamp we can window on, so a
// run-less document has 0 chunks and no date anyway. Windowed calls are identical
// to the old query (a NULL pipeline_created_at was already excluded by the range);
// only the all-time overall count omits run-less COMPLETED documents, which is
// intended.
//
// sourceFilter is "" (global) or " AND d0.source_id = ANY($1::uuid[])" and, when
// set, requires the documents join below; rangePred is the pipeline_created_at
// window (mapped to pd.created_at), or "".
func kbSpine(sourceFilter, rangePred string) string {
	join := ""
	if sourceFilter != "" {
		join = "\n	   JOIN   dist_rag.documents d0 ON d0.document_id = pd.document_id"
	}
	return fmt.Sprintf(`
	   SELECT DISTINCT ON (pd.document_id)
	          pd.document_id,
	          pd.chunks_processed,
	          pd.status::text AS status
	   FROM   dist_rag.pipeline_details pd%s
	   WHERE  TRUE%s%s
	   ORDER  BY pd.document_id,
	             pd.created_at DESC NULLS LAST,
	             pd.pipeline_id DESC`, join, sourceFilter, rangePred)
}

// runCounts executes a 2-column (documents, chunks) KB query under a short
// read-only statement_timeout.
func (r *AdminRepo) runCounts(query string, args ...any) (AdminKBCounts, error) {
	var c AdminKBCounts
	err := r.withKBTimeout(func(tx *sql.Tx) error {
		return tx.QueryRow(query, args...).Scan(&c.Documents, &c.Chunks)
	})
	return c, err
}

// withKBTimeout runs fn inside a read-only tx that has SET LOCAL statement_timeout,
// so a slow KB query fails fast rather than hanging. fn must consume its rows
// before returning (the tx ends here).
func (r *AdminRepo) withKBTimeout(fn func(*sql.Tx) error) error {
	tx, err := r.dataDB.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("kb query: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if _, err := tx.Exec("SET LOCAL statement_timeout = '" + kbStmtTimeout + "'"); err != nil {
		return fmt.Errorf("kb query: set timeout: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// CountCompletedDocsAndChunksAll returns the global COMPLETED-document
// count and chunk sum across every datapack (overall admin metric).
// start/endExclusive optionally window by pipeline_created_at.
func (r *AdminRepo) CountCompletedDocsAndChunksAll(
	start, endExclusive *time.Time,
) (AdminKBCounts, error) {
	pred, rangeArgs := rangePredicate("pd.created_at", start, endExclusive, 1)
	c, err := r.runCounts(completedDocsChunksSQL("", pred), rangeArgs...)
	if err != nil {
		return c, fmt.Errorf("kb admin counts (all): %w", err)
	}
	return c, nil
}

// CountCompletedDocsAndChunksBySourceIDs returns the COMPLETED-document
// count and chunk sum restricted to the given rag source ids (per-user
// admin metric). Callers pass an empty slice → zero without a query.
// start/endExclusive optionally window by pipeline_created_at.
func (r *AdminRepo) CountCompletedDocsAndChunksBySourceIDs(
	sourceIDs []string, start, endExclusive *time.Time,
) (AdminKBCounts, error) {
	if len(sourceIDs) == 0 {
		return AdminKBCounts{}, nil
	}
	// source_ids occupy $1; range params (if any) start at $2.
	pred, rangeArgs := rangePredicate("pd.created_at", start, endExclusive, 2)
	args := append([]any{pq.Array(sourceIDs)}, rangeArgs...)
	c, err := r.runCounts(completedDocsChunksSQL(" AND d0.source_id = ANY($1::uuid[])", pred), args...)
	if err != nil {
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

// AdminKBBreakdown is the knowledge panel's one-scan result: COMPLETED-document
// and chunk totals plus the per-file-type document counts. Documents/Chunks are
// the row sums, so the panel gets its tiles and its by-type chart from a single
// pipeline scan instead of two.
type AdminKBBreakdown struct {
	Documents int64
	Chunks    int64
	ByType    []KBDocTypeCount
}

// indexedDocsBreakdownSQL is completedDocsChunksSQL plus the document-type
// grouping: it joins dist_rag.documents for the file type and returns one row per
// type with its document count and chunk sum. Summing the rows gives the same
// doc/chunk totals, so the knowledge panel's tiles and its by-type chart come
// from a single scan (see kbSpine for why this reads base tables, not the view).
func indexedDocsBreakdownSQL(sourceFilter, rangePred string) string {
	return `WITH latest_per_document AS (` + kbSpine(sourceFilter, rangePred) + `
	 ),
	 completed_documents AS (
	   SELECT document_id, chunks_processed
	   FROM   latest_per_document
	   WHERE  status = 'COMPLETED'
	 )
	 SELECT COALESCE(NULLIF(d.document_type, ''), 'unknown') AS document_type,
	        COUNT(*)::bigint,
	        COALESCE(SUM(cd.chunks_processed), 0)::bigint
	 FROM   completed_documents cd
	 LEFT JOIN dist_rag.documents d ON d.document_id = cd.document_id
	 GROUP  BY 1
	 ORDER  BY 2 DESC`
}

// scanKBBreakdown reads the per-type rows into the doc/chunk totals and the
// by-type slice (one row per distinct type, no bucketing).
func scanKBBreakdown(rows *sql.Rows) (AdminKBBreakdown, error) {
	out := AdminKBBreakdown{ByType: []KBDocTypeCount{}}
	for rows.Next() {
		var t KBDocTypeCount
		var chunks int64
		if err := rows.Scan(&t.DocumentType, &t.Count, &chunks); err != nil {
			return out, fmt.Errorf("scan kb breakdown: %w", err)
		}
		out.Documents += t.Count
		out.Chunks += chunks
		out.ByType = append(out.ByType, t)
	}
	return out, rows.Err()
}

// runBreakdown executes a 3-column breakdown query under the KB statement_timeout
// and scans it. Rows are read before the tx ends.
func (r *AdminRepo) runBreakdown(query string, args ...any) (AdminKBBreakdown, error) {
	var out AdminKBBreakdown
	err := r.withKBTimeout(func(tx *sql.Tx) error {
		rows, qErr := tx.Query(query, args...)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		var sErr error
		out, sErr = scanKBBreakdown(rows)
		return sErr
	})
	return out, err
}

// CountIndexedDocsBreakdownAll returns the global COMPLETED-document + chunk
// totals and the by-file-type document counts across every datapack, optionally
// windowed by pipeline_created_at. One scan of pipeline_details (see kbSpine).
func (r *AdminRepo) CountIndexedDocsBreakdownAll(
	start, endExclusive *time.Time,
) (AdminKBBreakdown, error) {
	pred, rangeArgs := rangePredicate("pd.created_at", start, endExclusive, 1)
	out, err := r.runBreakdown(indexedDocsBreakdownSQL("", pred), rangeArgs...)
	if err != nil {
		return AdminKBBreakdown{}, fmt.Errorf("kb breakdown (all): %w", err)
	}
	return out, nil
}

// CountIndexedDocsBreakdownBySourceIDs is CountIndexedDocsBreakdownAll scoped to
// a user's rag source ids (per-user knowledge metric). An empty slice returns
// the zero value without a query.
func (r *AdminRepo) CountIndexedDocsBreakdownBySourceIDs(
	sourceIDs []string, start, endExclusive *time.Time,
) (AdminKBBreakdown, error) {
	if len(sourceIDs) == 0 {
		return AdminKBBreakdown{ByType: []KBDocTypeCount{}}, nil
	}
	// source_ids occupy $1; range params (if any) start at $2.
	pred, rangeArgs := rangePredicate("pd.created_at", start, endExclusive, 2)
	args := append([]any{pq.Array(sourceIDs)}, rangeArgs...)
	out, err := r.runBreakdown(indexedDocsBreakdownSQL(" AND d0.source_id = ANY($1::uuid[])", pred), args...)
	if err != nil {
		return AdminKBBreakdown{}, fmt.Errorf("kb breakdown (by source ids): %w", err)
	}
	return out, nil
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

// documentsByStatusSQL counts kbSpine's latest run per document by status.
func documentsByStatusSQL() string {
	return `SELECT status, COUNT(*)::bigint FROM (` + kbSpine("", "") + `
	 ) latest GROUP BY status`
}

// CountDocumentsByStatus returns every knowledge-base document's count by its
// latest pipeline run's status — the rag workers' backlog and failures, for the
// api-server's metrics. kbSpine's scan with no filter, so it stays on the base
// tables and under the same statement timeout; a document with no run yet is not
// counted (see kbSpine's semantics note).
func (r *AdminRepo) CountDocumentsByStatus() (map[string]int64, error) {
	counts := map[string]int64{}
	query := documentsByStatusSQL()
	err := r.withKBTimeout(func(tx *sql.Tx) error {
		rows, err := tx.Query(query)
		if err != nil {
			return fmt.Errorf("count documents by status: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var status sql.NullString
			var n int64
			if err := rows.Scan(&status, &n); err != nil {
				return fmt.Errorf("count documents by status: scan: %w", err)
			}
			key := status.String
			if !status.Valid {
				key = "unknown"
			}
			counts[key] += n
		}
		return rows.Err()
	})
	return counts, err
}
