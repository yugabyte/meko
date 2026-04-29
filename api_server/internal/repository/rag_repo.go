package repository

import (
	"database/sql"
	"fmt"

	"github.com/lib/pq"
)

// RagRepo encapsulates dist_rag SQL queries on a per-datapack database.
type RagRepo struct {
	db        *sql.DB
	indexName string
}

func NewRagRepo(db *sql.DB, indexName string) *RagRepo {
	return &RagRepo{db: db, indexName: indexName}
}

func (r *RagRepo) LookupIndexID() (string, error) {
	var id string
	err := r.db.QueryRow(
		`SELECT id FROM dist_rag.vector_indexes WHERE index_name = $1`,
		r.indexName,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("lookup index %q: %w", r.indexName, err)
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

func (r *RagRepo) InsertDocument(sourceID, documentName, documentURI, checksum string) error {
	_, err := r.db.Exec(
		`INSERT INTO dist_rag.documents
			(source_id, document_name, document_uri, document_checksum)
		 VALUES ($1::UUID, $2, $3, $4)
		 ON CONFLICT DO NOTHING`,
		sourceID, documentName, documentURI, checksum,
	)
	if err != nil {
		return fmt.Errorf("insert document %s: %w", documentName, err)
	}
	return nil
}

func (r *RagRepo) BuildIndex() error {
	_, err := r.db.Exec(
		`SELECT dist_rag.build_index(r_index_name := $1)`,
		r.indexName,
	)
	if err != nil {
		return fmt.Errorf("build_index(%s): %w", r.indexName, err)
	}
	return nil
}

// IndexExists returns true if the vector index already exists in dist_rag.
func (r *RagRepo) IndexExists() (bool, error) {
	var exists bool
	err := r.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM dist_rag.vector_indexes WHERE index_name = $1)`,
		r.indexName,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check index exists %q: %w", r.indexName, err)
	}
	return exists, nil
}

// InitVectorIndex creates the vector index via dist_rag.init_vector_index.
func (r *RagRepo) InitVectorIndex() (string, error) {
	var indexID string
	err := r.db.QueryRow(`SELECT dist_rag.init_vector_index(
		r_index_name   := $1,
		r_sources      := ARRAY[]::UUID[],
		r_embedding_model_params := '{"model": "text-embedding-3-large", "dimensions": 1536}',
		r_chunk_params := '{"chunk_size": 1024, "chunk_overlap": 256}'
	)`, r.indexName).Scan(&indexID)
	if err != nil {
		return "", fmt.Errorf("init_vector_index(%s): %w", r.indexName, err)
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
	ps := &PipelineStatus{IndexName: r.indexName}

	if len(sourceURIs) == 0 {
		return ps, nil
	}

	rows, err := r.db.Query(
		`SELECT DISTINCT ON (document_id)
		        index_name, source_uri, document_name, document_status,
		        chunks_processed
		 FROM dist_rag.vector_index_pipeline_details
		 WHERE index_name = $1
		   AND source_uri = ANY($2::TEXT[])
		 ORDER BY document_id, pipeline_created_at DESC NULLS LAST`,
		r.indexName, pq.Array(sourceURIs),
	)
	if err != nil {
		return nil, fmt.Errorf("query pipeline_details: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var d PipelineDetail
		if err := rows.Scan(
			&d.IndexName, &d.SourceURI, &d.DocumentName,
			&d.DocumentStatus, &d.ChunksProcessed,
		); err != nil {
			return nil, fmt.Errorf("scan pipeline_details: %w", err)
		}
		ps.Details = append(ps.Details, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return ps, nil
}
