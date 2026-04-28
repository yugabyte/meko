package repository

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

// ErrKnowledgeBaseExists is returned by Insert when a row with the same
// (datapack_id, rag_source_id) already exists. Handlers map this to
// 409 Conflict.
var ErrKnowledgeBaseExists = fmt.Errorf("knowledge base already exists")

// ErrKnowledgeBaseNotFound is returned by Get and Delete when no row
// matches the requested (datapack_id, rag_source_id). Handlers map this
// to 404 Not Found.
var ErrKnowledgeBaseNotFound = fmt.Errorf("knowledge base not found")

// KnowledgeBaseRepo encapsulates database operations against
// meko_system.knowledge_bases.
//
// The table has primary key (datapack_id HASH, rag_source_id ASC) so
// rows are conceptually datapack-scoped: ownership is enforced at the
// API boundary via the datapacks.user_id FK and RequireDatapackOwnership
// middleware. The repo deliberately takes a *models.Datapack reference
// at the handler layer rather than re-resolving the datapack by name.
type KnowledgeBaseRepo struct {
	db *sql.DB
}

// NewKnowledgeBaseRepo creates a new KnowledgeBaseRepo.
func NewKnowledgeBaseRepo(db *sql.DB) *KnowledgeBaseRepo {
	return &KnowledgeBaseRepo{db: db}
}

// Insert inserts a new knowledge_base row with a server-generated
// rag_source_id (gen_random_uuid). Returns the new rag_source_id.
// Surfaces ErrKnowledgeBaseExists on PK conflicts (extremely unlikely
// for a freshly-generated uuid but still possible if the caller
// supplies a fixed source_id via the URL path).
//
// TODO(external-kb): only called from KnowledgeBaseService.Add, which
// is reserved for a future external-KB registration flow. The canonical
// create path goes through InsertWithID using the dist_rag source UUID.
func (r *KnowledgeBaseRepo) Insert(
	datapackID, url string,
	details json.RawMessage,
) (string, error) {
	if len(details) == 0 {
		details = json.RawMessage(`{}`)
	}

	var ragSourceID string
	err := r.db.QueryRow(
		`INSERT INTO meko_system.knowledge_bases
			(datapack_id, rag_source_id, knowledge_base_url, knowledge_base_details)
		 VALUES ($1::uuid, gen_random_uuid(), $2, $3::jsonb)
		 RETURNING rag_source_id`,
		datapackID, url, []byte(details),
	).Scan(&ragSourceID)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return "", fmt.Errorf("%w: datapack_id=%s",
				ErrKnowledgeBaseExists, datapackID)
		}
		return "", fmt.Errorf("insert knowledge base: %w", err)
	}
	return ragSourceID, nil
}

// InsertWithID inserts a knowledge_base row using a caller-supplied
// rag_source_id. Used by CreateKB which wants to mirror the dist_rag
// source UUID into meko_system.knowledge_bases for stable lookup.
// Surfaces ErrKnowledgeBaseExists on PK conflicts.
func (r *KnowledgeBaseRepo) InsertWithID(
	datapackID, ragSourceID, url string,
	details json.RawMessage,
) error {
	if len(details) == 0 {
		details = json.RawMessage(`{}`)
	}

	_, err := r.db.Exec(
		`INSERT INTO meko_system.knowledge_bases
			(datapack_id, rag_source_id, knowledge_base_url, knowledge_base_details)
		 VALUES ($1::uuid, $2::uuid, $3, $4::jsonb)`,
		datapackID, ragSourceID, url, []byte(details),
	)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return fmt.Errorf("%w: datapack_id=%s rag_source_id=%s",
				ErrKnowledgeBaseExists, datapackID, ragSourceID)
		}
		return fmt.Errorf("insert knowledge base: %w", err)
	}
	return nil
}

// Upsert inserts or updates a knowledge_base row keyed by
// (datapack_id, rag_source_id). Updates knowledge_base_url,
// knowledge_base_details, and updated_at on conflict.
func (r *KnowledgeBaseRepo) Upsert(
	datapackID, ragSourceID, url string,
	details json.RawMessage,
) error {
	if len(details) == 0 {
		details = json.RawMessage(`{}`)
	}

	_, err := r.db.Exec(
		`INSERT INTO meko_system.knowledge_bases
			(datapack_id, rag_source_id, knowledge_base_url, knowledge_base_details)
		 VALUES ($1::uuid, $2::uuid, $3, $4::jsonb)
		 ON CONFLICT (datapack_id, rag_source_id) DO UPDATE SET
		     knowledge_base_url     = EXCLUDED.knowledge_base_url,
		     knowledge_base_details = EXCLUDED.knowledge_base_details,
		     updated_at             = NOW()`,
		datapackID, ragSourceID, url, []byte(details),
	)
	if err != nil {
		return fmt.Errorf("upsert knowledge base: %w", err)
	}
	return nil
}

// Get returns the knowledge_base row for (datapack_id, rag_source_id),
// or ErrKnowledgeBaseNotFound when no such row exists.
func (r *KnowledgeBaseRepo) Get(
	datapackID, ragSourceID string,
) (*models.KnowledgeBase, error) {
	var kb models.KnowledgeBase
	var details sql.NullString
	err := r.db.QueryRow(
		`SELECT datapack_id, rag_source_id, knowledge_base_url,
		        COALESCE(knowledge_base_details::text, '{}'),
		        created_at, updated_at
		 FROM meko_system.knowledge_bases
		 WHERE datapack_id = $1::uuid AND rag_source_id = $2::uuid`,
		datapackID, ragSourceID,
	).Scan(
		&kb.DatapackID, &kb.RagSourceID, &kb.URL,
		&details, &kb.CreatedAt, &kb.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: datapack_id=%s rag_source_id=%s",
			ErrKnowledgeBaseNotFound, datapackID, ragSourceID)
	}
	if err != nil {
		return nil, fmt.Errorf("get knowledge base: %w", err)
	}
	if details.Valid {
		kb.Details = json.RawMessage(details.String)
	}
	return &kb, nil
}

// ListByDatapack returns every knowledge_base row for the given
// datapack_id, ordered by created_at descending.
func (r *KnowledgeBaseRepo) ListByDatapack(
	datapackID string,
) ([]models.KnowledgeBase, error) {
	rows, err := r.db.Query(
		`SELECT datapack_id, rag_source_id, knowledge_base_url,
		        COALESCE(knowledge_base_details::text, '{}'),
		        created_at, updated_at
		 FROM meko_system.knowledge_bases
		 WHERE datapack_id = $1::uuid
		 ORDER BY created_at DESC`,
		datapackID,
	)
	if err != nil {
		return nil, fmt.Errorf("list knowledge bases: %w", err)
	}
	defer rows.Close()

	var result []models.KnowledgeBase
	for rows.Next() {
		var kb models.KnowledgeBase
		var details sql.NullString
		if err := rows.Scan(
			&kb.DatapackID, &kb.RagSourceID, &kb.URL,
			&details, &kb.CreatedAt, &kb.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan knowledge base: %w", err)
		}
		if details.Valid {
			kb.Details = json.RawMessage(details.String)
		}
		result = append(result, kb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration: %w", err)
	}
	return result, nil
}

// LookupByURL returns the knowledge_base row for the given
// (datapack_id, knowledge_base_url) pair if present. Returns
// (nil, nil) when no row matches; this lets callers distinguish
// "not found" from a genuine error without using the sentinel.
func (r *KnowledgeBaseRepo) LookupByURL(
	datapackID, url string,
) (*models.KnowledgeBase, error) {
	var kb models.KnowledgeBase
	var details sql.NullString
	err := r.db.QueryRow(
		`SELECT datapack_id, rag_source_id, knowledge_base_url,
		        COALESCE(knowledge_base_details::text, '{}'),
		        created_at, updated_at
		 FROM meko_system.knowledge_bases
		 WHERE datapack_id = $1::uuid AND knowledge_base_url = $2
		 LIMIT 1`,
		datapackID, url,
	).Scan(
		&kb.DatapackID, &kb.RagSourceID, &kb.URL,
		&details, &kb.CreatedAt, &kb.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup knowledge base by URL: %w", err)
	}
	if details.Valid {
		kb.Details = json.RawMessage(details.String)
	}
	return &kb, nil
}

// Delete removes the knowledge_base row identified by
// (datapack_id, rag_source_id). Returns ErrKnowledgeBaseNotFound when
// no such row exists.
func (r *KnowledgeBaseRepo) Delete(
	datapackID, ragSourceID string,
) error {
	res, err := r.db.Exec(
		`DELETE FROM meko_system.knowledge_bases
		 WHERE datapack_id = $1::uuid AND rag_source_id = $2::uuid`,
		datapackID, ragSourceID,
	)
	if err != nil {
		return fmt.Errorf("delete knowledge base: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: datapack_id=%s rag_source_id=%s",
			ErrKnowledgeBaseNotFound, datapackID, ragSourceID)
	}
	return nil
}

// DeleteByDatapack removes every knowledge_base row for the given
// datapack_id. Used to clear stale RAG cache when a datapack's source
// no longer exists in dist_rag.
func (r *KnowledgeBaseRepo) DeleteByDatapack(datapackID string) error {
	_, err := r.db.Exec(
		`DELETE FROM meko_system.knowledge_bases
		 WHERE datapack_id = $1::uuid`,
		datapackID,
	)
	if err != nil {
		return fmt.Errorf("delete knowledge bases for datapack %s: %w",
			datapackID, err)
	}
	return nil
}

// GetSourceIDs returns all rag_source_id values for a datapack, ordered
// by created_at descending.
func (r *KnowledgeBaseRepo) GetSourceIDs(
	datapackID string,
) ([]string, error) {
	rows, err := r.db.Query(
		`SELECT rag_source_id
		 FROM meko_system.knowledge_bases
		 WHERE datapack_id = $1::uuid
		 ORDER BY created_at DESC`,
		datapackID,
	)
	if err != nil {
		return nil, fmt.Errorf("get source IDs: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan source ID: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetSourceURIs returns the knowledge_base_url values for a datapack,
// ordered by created_at descending.
func (r *KnowledgeBaseRepo) GetSourceURIs(
	datapackID string,
) ([]string, error) {
	rows, err := r.db.Query(
		`SELECT knowledge_base_url
		 FROM meko_system.knowledge_bases
		 WHERE datapack_id = $1::uuid
		 ORDER BY created_at DESC`,
		datapackID,
	)
	if err != nil {
		return nil, fmt.Errorf("get source URIs: %w", err)
	}
	defer rows.Close()

	var uris []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, fmt.Errorf("scan source URI: %w", err)
		}
		uris = append(uris, u)
	}
	return uris, rows.Err()
}

// KnowledgeBaseCounts is the per-datapack chunk / doc / promoted-memory
// breakdown.
type KnowledgeBaseCounts struct {
	TotalDocs        int64 `json:"total_docs"`
	TotalChunks      int64 `json:"total_chunks"`
	PromotedMemories int64 `json:"promoted_memories"`
}

// KnowledgeBaseDocument is one row of the uploaded-files listing.
type KnowledgeBaseDocument struct {
	DocumentName string `json:"document_name"`
	Status       string `json:"status"`
}

// Counts returns the chunk / doc / promoted-memory breakdown for a
// datapack.
func (r *KnowledgeBaseRepo) Counts(
	datapackID string,
) (KnowledgeBaseCounts, error) {
	var c KnowledgeBaseCounts

	if err := r.db.QueryRow(
		`select count(distinct(document_id)) as doc_count
		 from datapacks.knowledge_bases
		 where tenant_id = $1::uuid
		   and metadata_filters->>'source' = 'doc'`,
		datapackID,
	).Scan(&c.TotalDocs); err != nil {
		return c, fmt.Errorf("count total_docs: %w", err)
	}

	if err := r.db.QueryRow(
		`select count(*) as chunk_count
		 from datapacks.knowledge_bases
		 where tenant_id = $1::uuid`,
		datapackID,
	).Scan(&c.TotalChunks); err != nil {
		return c, fmt.Errorf("count total_chunks: %w", err)
	}

	if err := r.db.QueryRow(
		`select count(*)
		 from datapacks.promoted_collection
		 where meko_datapack_id = $1::uuid`,
		datapackID,
	).Scan(&c.PromotedMemories); err != nil {
		return c, fmt.Errorf("count promoted_memories: %w", err)
	}

	return c, nil
}

// ListDistRagDocuments returns (document_name, status) for every
// dist_rag.documents row whose source belongs to the given datapack,
// joined through dist_rag.sources.tenant_id.
func (r *KnowledgeBaseRepo) ListDistRagDocuments(
	datapackID string,
) ([]KnowledgeBaseDocument, error) {
	rows, err := r.db.Query(
		`SELECT d.document_name, d.status::text
		 FROM dist_rag.documents d
		 JOIN dist_rag.sources  s ON s.id = d.source_id
		 WHERE s.tenant_id = $1::uuid`,
		datapackID,
	)
	if err != nil {
		return nil, fmt.Errorf("list dist_rag documents: %w", err)
	}
	defer rows.Close()

	docs := []KnowledgeBaseDocument{}
	for rows.Next() {
		var d KnowledgeBaseDocument
		var name sql.NullString
		if err := rows.Scan(&name, &d.Status); err != nil {
			return nil, fmt.Errorf("scan document row: %w", err)
		}
		if name.Valid {
			d.DocumentName = name.String
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate document rows: %w", err)
	}
	return docs, nil
}
