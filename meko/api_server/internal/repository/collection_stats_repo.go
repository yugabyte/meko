package repository

import (
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/lib/pq"
)

// Collection-wide statistics: standing totals, no date range.
//
// One query per metric for the whole collection, `= ANY($1)` over the
// caller's visible datapack ids, so 500 datapacks cost the same round trips
// as one.
type CollectionStatsRepo struct {
	dataDB   *sql.DB
	systemDB *sql.DB
}

func NewCollectionStatsRepo(dataDB, systemDB *sql.DB) *CollectionStatsRepo {
	return &CollectionStatsRepo{dataDB: dataDB, systemDB: systemDB}
}

// CollectionStats is every figure the panel shows.
//
// Memory counts are absent on purpose: the detail endpoint already returns
// them per datapack from DatapackRollupRepo and the panel sums those. A
// second count here would re-scan meko_mem0_collection on the same page load
// and risk a second definition of promoted.
type CollectionStats struct {
	KBFilesByType  map[string]int64 `json:"kb_files_by_type"`
	SearchesHybrid int64            `json:"searches_hybrid"`
	SearchesKB     int64            `json:"searches_kb"`
	SearchesMemory int64            `json:"searches_memory"`
	Conversations  int64            `json:"conversations"`
	KBFiles        int64            `json:"kb_files"`
	KBChunks       int64            `json:"kb_chunks"`
	Artifacts      int64            `json:"artifacts"`
	ArtifactBytes  int64            `json:"artifact_bytes"`
}

// Stats gathers every figure for one collection. schema and indexName scope
// the knowledge figures to one vector index; a document mapped into two would
// otherwise count twice.
func (r *CollectionStatsRepo) Stats(
	datapackIDs []string, schema, indexName string,
) (CollectionStats, error) {
	out := CollectionStats{KBFilesByType: map[string]int64{}}
	if len(datapackIDs) == 0 {
		return out, nil
	}

	// Each metric degrades on its own, the rule ListActivityLog already
	// applies to its sides: a timeout on the dist_rag view should cost the
	// knowledge tiles, not turn the whole panel into an error banner that
	// also hides the memory tiles, which come from a different query and are
	// fine. Only a total failure is worth reporting.
	steps := []struct {
		name string
		run  func() error
	}{
		{"searches", func() error { return r.searches(datapackIDs, &out) }},
		{"conversations", func() error { return r.conversations(datapackIDs, &out) }},
		{"artifacts", func() error { return r.artifacts(datapackIDs, &out) }},
		{"knowledge", func() error { return r.knowledge(datapackIDs, schema, indexName, &out) }},
	}
	failed := 0
	var lastErr error
	for _, step := range steps {
		if err := step.run(); err != nil {
			slog.Warn("collection stats: metric unavailable, reporting zero",
				"metric", step.name, "error", err)
			failed++
			lastErr = err
		}
	}
	if failed == len(steps) {
		return out, fmt.Errorf("collection stats: every metric failed: %w", lastErr)
	}
	return out, nil
}

// searches reads the rolling per-day counters meko-mcp-server maintains.
func (r *CollectionStatsRepo) searches(ids []string, out *CollectionStats) error {
	rows, err := r.systemDB.Query(
		`SELECT search_type, COALESCE(SUM(query_count), 0)
		 FROM   meko_system.datapack_daily_activity
		 WHERE  datapack_id = ANY($1)
		 GROUP  BY search_type`,
		pq.Array(ids),
	)
	if err != nil {
		return fmt.Errorf("collection stats: searches: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			searchType string
			count      int64
		)
		if err := rows.Scan(&searchType, &count); err != nil {
			return fmt.Errorf("scan search row: %w", err)
		}
		switch searchType {
		case "hybrid":
			out.SearchesHybrid += count
		case "kb":
			out.SearchesKB += count
		case "memory":
			out.SearchesMemory += count
		}
	}
	return rows.Err()
}

// conversations counts distinct conversations. Partitioned by HASH(tenant_id)
// alone, so scoping to the collection's datapacks prunes to their partitions
// rather than touching all 128.
func (r *CollectionStatsRepo) conversations(ids []string, out *CollectionStats) error {
	err := r.dataDB.QueryRow(
		`SELECT COUNT(DISTINCT conversation_id)
		 FROM   datapacks.conversation_history_search
		 WHERE  tenant_id = ANY($1)`,
		pq.Array(ids),
	).Scan(&out.Conversations)
	if err != nil {
		return fmt.Errorf("collection stats: conversations: %w", err)
	}
	return nil
}

func (r *CollectionStatsRepo) artifacts(ids []string, out *CollectionStats) error {
	err := r.dataDB.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0)
		 FROM   datapacks.artifacts
		 WHERE  datapack_id = ANY($1) AND expired_at IS NULL`,
		pq.Array(ids),
	).Scan(&out.Artifacts, &out.ArtifactBytes)
	if err != nil {
		return fmt.Errorf("collection stats: artifacts: %w", err)
	}
	return nil
}

// knowledge counts documents, their chunks and their type breakdown, derived
// like GetKnowledgeBaseSummary so both surfaces report the same files.
//
// Deliberate divergence: that page also counts QUEUED and PROCESSING, so its
// tile moves as soon as an upload starts. This is a standing inventory, and a
// file mid-ingest is not searchable yet, so only COMPLETED counts.
func (r *CollectionStatsRepo) knowledge(
	ids []string, schema, indexName string, out *CollectionStats,
) error {
	sources, err := r.sourceIDs(ids)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return nil
	}

	rows, err := r.dataDB.Query(
		`WITH latest_per_document AS (
		     SELECT DISTINCT ON (document_id)
		            document_id,
		            chunks_processed,
		            COALESCE(NULLIF(document_type, ''), 'other') AS document_type,
		            COALESCE(pipeline_status::text, document_status::text) AS status
		     FROM   dist_rag.vector_index_pipeline_details
		     WHERE  source_id = ANY($1::uuid[])
		       AND  schema_name = $2
		       AND  index_name  = $3
		     ORDER  BY document_id, pipeline_created_at DESC NULLS LAST, pipeline_id DESC
		 ),
		 indexed AS (
		     SELECT document_type, chunks_processed
		     FROM   latest_per_document
		     WHERE  status = 'COMPLETED'
		 )
		 SELECT GROUPING(document_type) AS no_type,
		        document_type,
		        COUNT(*),
		        COALESCE(SUM(chunks_processed), 0)::bigint
		 FROM   indexed
		 GROUP  BY GROUPING SETS ((), (document_type))`,
		pq.Array(sources), schema, indexName,
	)
	if err != nil {
		return fmt.Errorf("collection stats: knowledge: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			noType  int
			docType sql.NullString
			count   int64
			chunks  int64
		)
		if err := rows.Scan(&noType, &docType, &count, &chunks); err != nil {
			return fmt.Errorf("scan knowledge row: %w", err)
		}
		if noType == 1 {
			out.KBFiles = count
			out.KBChunks = chunks
			continue
		}
		if docType.Valid {
			out.KBFilesByType[docType.String] = count
		}
	}
	return rows.Err()
}

// sourceIDs maps the collection's datapacks to their RAG source ids, the join
// key dist_rag is addressed by. One query, in the system DB.
func (r *CollectionStatsRepo) sourceIDs(datapackIDs []string) ([]string, error) {
	rows, err := r.systemDB.Query(
		`SELECT rag_source_id::text
		 FROM   meko_system.knowledge_bases
		 WHERE  datapack_id = ANY($1)`,
		pq.Array(datapackIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("collection stats: kb sources: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan source id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
