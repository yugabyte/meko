package repository

import (
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/lib/pq"
)

// RagRepo encapsulates dist_rag SQL queries on a per-datapack database.
//
// dist_rag stores fully-qualified vector indexes as two columns
// (schema_name, index_name) and the SQL functions take them as separate
// arguments, so the repo carries both pieces and never reasons about the
// dotted form except when constructing display strings via FullName.
type RagRepo struct {
	db         *sql.DB
	schemaName string
	indexName  string
}

func NewRagRepo(db *sql.DB, schemaName, indexName string) *RagRepo {
	return &RagRepo{db: db, schemaName: schemaName, indexName: indexName}
}

// FullName returns the dotted "schema.index" form used in error messages
// and log lines. It is never sent to dist_rag SQL functions — those take
// schema_name and index_name as separate parameters.
func (r *RagRepo) FullName() string { return r.schemaName + "." + r.indexName }

func (r *RagRepo) LookupIndexID() (string, error) {
	var id string
	err := r.db.QueryRow(
		`SELECT id FROM dist_rag.vector_indexes
		 WHERE schema_name = $1 AND index_name = $2`,
		r.schemaName, r.indexName,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("lookup index %q: %w", r.FullName(), err)
	}
	return id, nil
}

func (r *RagRepo) LookupSourceByURI(sourceURI string) (string, error) {
	var id string
	err := r.db.QueryRow(
		`SELECT id FROM dist_rag.sources WHERE source_uri = $1`,
		sourceURI,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup source by URI %q: %w", sourceURI, err)
	}
	return id, nil
}

func (r *RagRepo) SourceExists(sourceID string) (bool, error) {
	var exists bool
	err := r.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM dist_rag.sources WHERE id = $1::UUID)`,
		sourceID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check source exists %s: %w", sourceID, err)
	}
	return exists, nil
}

// CreateSource registers a new dist_rag source bound to a tenant.
// tenantID is required: dist_rag.create_source defaults it to the
// all-zeros UUID, which would break /knowledge-bases/{counts,documents}
// (those endpoints filter by tenant_id = datapack_id).
func (r *RagRepo) CreateSource(sourceURI, tenantID string) (string, error) {
	var sourceID string
	err := r.db.QueryRow(
		`SELECT dist_rag.create_source(
			r_source_uri := $1,
			r_tenant_id  := $2::UUID
		)`,
		sourceURI, tenantID,
	).Scan(&sourceID)
	if err != nil {
		return "", fmt.Errorf("create_source(uri=%s, tenant=%s): %w", sourceURI, tenantID, err)
	}
	return sourceID, nil
}

func (r *RagRepo) AddSourceToIndex(indexID, sourceID, chunkParams string) error {
	_, err := r.db.Exec(
		`SELECT dist_rag.add_source_to_index(
			r_index_id := $1::UUID,
			r_source_id := $2::UUID,
			r_chunk_params := $3::JSONB
		)`,
		indexID, sourceID, chunkParams,
	)
	if err != nil {
		return fmt.Errorf("add_source_to_index(index=%s, source=%s): %w", indexID, sourceID, err)
	}
	return nil
}

func (r *RagRepo) GetExistingDocURIs(sourceID string) (map[string]bool, error) {
	rows, err := r.db.Query(
		`SELECT document_uri FROM dist_rag.documents WHERE source_id = $1::UUID`,
		sourceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list existing docs for source %s: %w", sourceID, err)
	}
	defer rows.Close()

	uris := make(map[string]bool)
	for rows.Next() {
		var uri string
		if err := rows.Scan(&uri); err != nil {
			return nil, fmt.Errorf("scan document_uri: %w", err)
		}
		uris[uri] = true
	}
	return uris, rows.Err()
}

func (r *RagRepo) InsertDocument(sourceID, documentName, documentURI, checksum, documentType string) error {
	_, err := r.db.Exec(
		`INSERT INTO dist_rag.documents
			(source_id, document_name, document_uri, document_checksum, document_type)
		 VALUES ($1::UUID, $2, $3, $4, $5)
		 ON CONFLICT (source_id, document_uri)
		   DO UPDATE SET document_type = EXCLUDED.document_type
		   WHERE dist_rag.documents.document_type IS NULL`,
		sourceID, documentName, documentURI, checksum, documentType,
	)
	if err == nil {
		return nil
	}
	slog.Warn("RagRepo.InsertDocument: upsert failed, entering fallback",
		"source_id", sourceID,
		"document_uri", documentURI,
		"error", err,
	)

	// Fallback path: the table has no unique constraint on (source_id, document_uri).
	// Check existence and insert/update manually.
	var exists bool
	if scanErr := r.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM dist_rag.documents WHERE source_id = $1::UUID AND document_uri = $2)`,
		sourceID, documentURI,
	).Scan(&exists); scanErr != nil {
		return fmt.Errorf("check document existence %s: %w", documentName, scanErr)
	}

	if exists {
		return nil
	}

	// Document is genuinely new — plain insert without conflict target.
	_, err = r.db.Exec(
		`INSERT INTO dist_rag.documents
			(source_id, document_name, document_uri, document_checksum, document_type)
		 VALUES ($1::UUID, $2, $3, $4, $5)`,
		sourceID, documentName, documentURI, checksum, documentType,
	)
	if err != nil {
		return fmt.Errorf("insert document %s: %w", documentName, err)
	}
	return nil
}

// BuildIndex enqueues a build for this repo's vector index. dist_rag.build_index
// requires exactly one of r_index_id / r_index_name; we resolve to the id first
// so callers don't depend on dist_rag still uniquely identifying by index_name
// alone (today it does, but it's a per-extension-version contract).
func (r *RagRepo) BuildIndex() error {
	indexID, err := r.LookupIndexID()
	if err != nil {
		return fmt.Errorf("build_index(%s): %w", r.FullName(), err)
	}
	if _, err := r.db.Exec(
		`SELECT dist_rag.build_index(r_index_id := $1::UUID)`,
		indexID,
	); err != nil {
		return fmt.Errorf("build_index(%s, id=%s): %w", r.FullName(), indexID, err)
	}
	return nil
}

// IndexExists returns true if the vector index already exists in dist_rag.
//
// dist_rag.vector_indexes has a UNIQUE index on index_name alone, but we
// filter on (schema_name, index_name) anyway so we don't accidentally match
// an unrelated index that happens to share the same unqualified name in a
// different schema (defensive against future relaxations of the constraint).
func (r *RagRepo) IndexExists() (bool, error) {
	var exists bool
	err := r.db.QueryRow(
		`SELECT EXISTS(
		   SELECT 1 FROM dist_rag.vector_indexes
		   WHERE schema_name = $1 AND index_name = $2)`,
		r.schemaName, r.indexName,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check index exists %q: %w", r.FullName(), err)
	}
	return exists, nil
}

// InitVectorIndex creates the vector index via dist_rag.init_vector_index.
// Passes schema_name and index_name as separate parameters per the
// dist_rag function signature (introduced when the table grew a
// schema_name column).
func (r *RagRepo) InitVectorIndex() (string, error) {
	var indexID string
	err := r.db.QueryRow(`SELECT dist_rag.init_vector_index(
		r_index_name   := $1,
		r_sources      := ARRAY[]::UUID[],
		r_embedding_model_params := '{"model": "text-embedding-3-large", "dimensions": 1536}',
		r_chunk_params := '{"chunk_size": 1024, "chunk_overlap": 256}',
		r_schema_name  := $2
	)`, r.indexName, r.schemaName).Scan(&indexID)
	if err != nil {
		return "", fmt.Errorf("init_vector_index(%s): %w", r.FullName(), err)
	}
	return indexID, nil
}

func (r *RagRepo) GetSourceStatus(sourceID string) (string, error) {
	var status string
	err := r.db.QueryRow(
		`SELECT status FROM dist_rag.sources WHERE id = $1::UUID`, sourceID,
	).Scan(&status)
	if err != nil {
		return "", fmt.Errorf("get source status %s: %w", sourceID, err)
	}
	return status, nil
}

type SourceStatus struct {
	SourceID  string `json:"source_id"`
	SourceURI string `json:"source_uri"`
	Status    string `json:"status"`
}

func (r *RagRepo) GetSourceStatuses(sourceIDs []string) ([]SourceStatus, error) {
	if len(sourceIDs) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(
		`SELECT id, source_uri, status FROM dist_rag.sources
		 WHERE id = ANY($1::UUID[])`,
		pq.Array(sourceIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("query source statuses: %w", err)
	}
	defer rows.Close()

	var result []SourceStatus
	for rows.Next() {
		var ss SourceStatus
		if err := rows.Scan(&ss.SourceID, &ss.SourceURI, &ss.Status); err != nil {
			return nil, fmt.Errorf("scan source status: %w", err)
		}
		result = append(result, ss)
	}
	return result, rows.Err()
}

type PipelineDetail struct {
	IndexName       string `json:"index_name"`
	SourceURI       string `json:"source_uri"`
	DocumentName    string `json:"document_name"`
	DocumentStatus  string `json:"document_status"`
	ChunksProcessed *int   `json:"chunks_processed"`
}

type PipelineStatus struct {
	IndexName string           `json:"index_name"`
	Sources   []SourceStatus   `json:"source_statuses"`
	Details   []PipelineDetail `json:"pipeline_details"`
}

func (r *RagRepo) GetPipelineStatus(sourceURIs []string) (*PipelineStatus, error) {
	ps := &PipelineStatus{IndexName: r.FullName()}

	if len(sourceURIs) == 0 {
		slog.Info("RagRepo.GetPipelineStatus: no source URIs, returning empty details",
			"index_name", r.FullName(),
		)
		return ps, nil
	}
	slog.Info("RagRepo.GetPipelineStatus: querying pipeline details",
		"index_name", r.FullName(),
		"schema_name", r.schemaName,
		"raw_index_name", r.indexName,
		"source_uris_count", len(sourceURIs),
		"source_uris", sourceURIs,
		"status_source", "documents+latest_pipeline+active_work_queue",
	)

	rows, err := r.db.Query(
		`SELECT DISTINCT ON (document_uri)
		        schema_name, index_name, source_uri, document_name, document_status,
		        chunks_processed
		 FROM dist_rag.vector_index_pipeline_details
		 WHERE schema_name = $1
		   AND index_name  = $2
		   AND source_uri  = ANY($3::TEXT[])
		 ORDER BY document_uri,
		          pipeline_created_at DESC NULLS LAST,
		          pipeline_id DESC`,
		r.schemaName, r.indexName, pq.Array(sourceURIs),
	)
	if err != nil {
		return nil, fmt.Errorf("query pipeline_details: %w", err)
	}
	defer rows.Close()

	rowCount := 0
	for rows.Next() {
		var (
			d        PipelineDetail
			schema   string
			rawIndex string
		)
		if err := rows.Scan(
			&schema, &rawIndex, &d.SourceURI, &d.DocumentName,
			&d.DocumentStatus, &d.ChunksProcessed,
		); err != nil {
			return nil, fmt.Errorf("scan pipeline_details: %w", err)
		}
		d.IndexName = schema + "." + rawIndex
		ps.Details = append(ps.Details, d)
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slog.Info("RagRepo.GetPipelineStatus: query complete",
		"index_name", r.FullName(),
		"detail_rows", rowCount,
	)

	return ps, nil
}
