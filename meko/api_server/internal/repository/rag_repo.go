package repository

import (
	"context"
	"database/sql"
	"errors"
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

func (r *RagRepo) LookupIndexID(ctx context.Context) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx,
		`SELECT id FROM dist_rag.vector_indexes
		 WHERE schema_name = $1 AND index_name = $2`,
		r.schemaName, r.indexName,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("lookup index %q: %w", r.FullName(), err)
	}
	return id, nil
}

func (r *RagRepo) LookupSourceByURI(ctx context.Context, sourceURI string) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx,
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

func (r *RagRepo) SourceExists(ctx context.Context, sourceID string) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx,
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
func (r *RagRepo) CreateSource(ctx context.Context, sourceURI, tenantID string) (string, error) {
	var sourceID string
	err := r.db.QueryRowContext(ctx,
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

func (r *RagRepo) AddSourceToIndex(ctx context.Context, indexID, sourceID, chunkParams string) error {
	_, err := r.db.ExecContext(ctx,
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

// WithSourceRowLock runs criticalSection -- on the same transaction as the
// lock, not a separate connection -- while holding a FOR NO KEY UPDATE lock on
// the dist_rag source's own row. dist_rag.documents has no unique constraint on
// (source_id, document_uri) -- it's extension-owned schema this repo doesn't
// migrate -- so there's no ON CONFLICT target to upsert against and a plain
// check-then-write would race two concurrent callers for the same pair (both
// could see "not exists" and both insert). The source row, guaranteed to
// already exist, stands in as the shared serialization point instead.
//
// FOR NO KEY UPDATE, not FOR UPDATE: it conflicts with another caller's
// FOR NO KEY UPDATE, which is all the serialization needs, but not with the
// FOR KEY SHARE that foreign-key checks take on the parent row.
// dist_rag.documents and dist_rag.vector_index_source_mappings both reference
// dist_rag.sources(id), so a FOR UPDATE here would also block dist_rag's own
// inserts for this source for as long as the lock is held.
//
// criticalSection runs on this txn so the check and the write commit (or roll
// back) together with the lock. That is also why the whole transaction can
// follow ctx, unlike WithDatapackRowLock's: if ctx ends, the write rolls back
// with the lock instead of outliving it.
func (r *RagRepo) WithSourceRowLock(
	ctx context.Context, sourceID string, criticalSection func(txn *sql.Tx) error,
) error {
	txn, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin source lock %s: %w", sourceID, err)
	}
	defer func() { _ = txn.Rollback() }()

	if err := lockRow(ctx, txn,
		`SELECT id FROM dist_rag.sources WHERE id = $1::UUID FOR NO KEY UPDATE`,
		sourceID,
	); err != nil {
		return fmt.Errorf("lock source %s: %w", sourceID, err)
	}

	if err := criticalSection(txn); err != nil {
		return err
	}

	if err := txn.Commit(); err != nil {
		return fmt.Errorf("commit source lock %s: %w", sourceID, err)
	}
	return nil
}

// InsertDocument upserts a row into dist_rag.documents, serialized against
// concurrent callers for the same (source_id, document_uri) via
// WithSourceRowLock (see its doc comment for why a lock stands in for a
// missing unique constraint here).
//
// `documentDetails` is a JSONB blob carried under the conventional
// `meko` sub-key — see the dist_rag schema for the column definition.
// Pass a JSON of shape `{"meko": {"trace_id": ..., "user_id": ...,
// "size": ...}}` to populate. Use nil/empty to fall back to the
// dist_rag default. The api_server-side `meko.user_id` is the
// authenticated uploader (not the datapack owner), so per-file
// uploader provenance survives shared-datapack collaboration; the
// downstream listing endpoint resolves it to an email at read time.
func (r *RagRepo) InsertDocument(
	ctx context.Context, sourceID, documentName, documentURI, checksum, documentType string,
	documentDetails []byte,
) error {
	if len(documentDetails) == 0 {
		documentDetails = []byte(`{}`)
	}
	return r.WithSourceRowLock(ctx, sourceID, func(txn *sql.Tx) error {
		// One statement instead of a separate exists-check: the UPDATE runs
		// first and reports via RETURNING whether it touched a row; the
		// INSERT only fires when it didn't. Without a unique constraint that
		// is safe against callers that take WithSourceRowLock, which run one
		// at a time. dist_rag's own CREATE_SOURCE worker also inserts
		// documents and never takes the lock; callers avoid racing it by not
		// writing while the source is QUEUED or IN_PROGRESS (CreateKB skips
		// its sync then, and waitBuildAndPopulateDetails waits for COMPLETED).
		// On txn, not r.db, so the write commits or rolls back with the lock.
		if _, err := txn.ExecContext(ctx,
			`WITH updated AS (
			     UPDATE dist_rag.documents
			        SET document_type    = $3,
			            document_details = $6::jsonb
			      WHERE source_id = $1::UUID AND document_uri = $2
			     RETURNING source_id
			 )
			 INSERT INTO dist_rag.documents
			        (source_id, document_name, document_uri, document_checksum, document_type, document_details)
			 SELECT $1::UUID, $4, $2, $5, $3, $6::jsonb
			 WHERE NOT EXISTS (SELECT 1 FROM updated)`,
			sourceID, documentURI, documentType, documentName, checksum, documentDetails,
		); err != nil {
			return fmt.Errorf("upsert document %s: %w", documentName, err)
		}
		return nil
	})
}

// QueueSourceDocuments enqueues preprocessing for one source's documents that
// are not already processing or done. dist_rag.build_index does the same for
// every source mapped to the index, which on an upload re-queues every pending
// document in the deployment.
//
// Documents that already have a QUEUED task are queued again, so uploading K
// files into one source before the workers drain it still adds about K²/2
// tasks for that source. The duplicate is bounded to the source, not removed.
func (r *RagRepo) QueueSourceDocuments(sourceID string) error {
	indexID, err := r.LookupIndexID(context.Background())
	if err != nil {
		return fmt.Errorf("queue source %s: %w", sourceID, err)
	}
	if _, err := r.db.Exec(
		`SELECT dist_rag._queue_source_documents(r_source_id := $1::UUID, r_index_id := $2::UUID)`,
		sourceID, indexID,
	); err != nil {
		return fmt.Errorf("_queue_source_documents(%s, source=%s): %w", r.FullName(), sourceID, err)
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
		r_ai_provider  := 'AWS_BEDROCK',
		r_embedding_model_params := '{"model": "amazon.titan-embed-text-v1", "dimensions": 1536}',
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

	// document_status alone lags pipeline_status: the row-level check
	// below (dist_rag.documents.status, surfaced by ListDistRagDocuments
	// via the same COALESCE) reflects completion sooner than
	// document_status does. (admin_kb.go's analytics counts read
	// pipeline_details directly and intentionally count only documents
	// that have a pipeline run — see kbSpine.)
	rows, err := r.db.Query(
		`SELECT DISTINCT ON (document_uri)
		        schema_name, index_name, source_uri, document_name,
		        COALESCE(pipeline_status::text, document_status::text) AS document_status,
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

// GetDocumentStatus returns the pipeline detail for a single document
// (identified by document_name) within the given sources, or nil if there is no
// matching document. It is the single-row form of GetPipelineStatus: the
// document_name filter is pushed into the query so the DB returns just that row
// instead of the whole datapack's pipeline.
func (r *RagRepo) GetDocumentStatus(sourceURIs []string, documentName string) (*PipelineDetail, error) {
	if len(sourceURIs) == 0 {
		return nil, nil
	}

	row := r.db.QueryRow(
		`SELECT DISTINCT ON (document_uri)
		        schema_name, index_name, source_uri, document_name, document_status,
		        chunks_processed
		 FROM dist_rag.vector_index_pipeline_details
		 WHERE schema_name   = $1
		   AND index_name    = $2
		   AND source_uri    = ANY($3::TEXT[])
		   AND document_name = $4
		 ORDER BY document_uri,
		          pipeline_created_at DESC NULLS LAST,
		          pipeline_id DESC
		 LIMIT 1`,
		r.schemaName, r.indexName, pq.Array(sourceURIs), documentName,
	)

	var (
		d        PipelineDetail
		schema   string
		rawIndex string
	)
	if err := row.Scan(
		&schema, &rawIndex, &d.SourceURI, &d.DocumentName,
		&d.DocumentStatus, &d.ChunksProcessed,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan document status: %w", err)
	}
	d.IndexName = schema + "." + rawIndex
	return &d, nil
}
