package repository

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

// documentBasename returns the last path segment of a document_name
// — e.g. "<user>/<datapack>/foo.md" → "foo.md". `dist_rag.documents`
// stores the full uploader path; UI-facing fields (header summary,
// activity feed) want just the file. The Files-tab row list keeps the
// full path on `document_name` for callers that need to reconstruct
// the S3 key.
func documentBasename(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ErrKnowledgeBaseExists is returned by Insert when a row with the same
// (datapack_id, rag_source_id) already exists. Handlers map this to
// 409 Conflict.
var ErrKnowledgeBaseExists = fmt.Errorf("knowledge base already exists")

// ErrKnowledgeBaseNotFound is returned by Get and Delete when no row
// matches the requested (datapack_id, rag_source_id). Handlers map this
// to 404 Not Found.
var ErrKnowledgeBaseNotFound = fmt.Errorf("knowledge base not found")

// ErrDocumentNotFound is returned by GetDocumentUploader / DeleteDocument
// when no dist_rag.documents row matches (datapack_id, document_id).
// Handlers map this to 404 Not Found.
var ErrDocumentNotFound = fmt.Errorf("document not found")

// KnowledgeBaseRepo encapsulates database operations against the
// meko_system.knowledge_bases registry as well as per-datapack chunk
// counts in datapacks.* and document listings in dist_rag.*.
//
// The meko_system.knowledge_bases table has primary key (datapack_id
// HASH, rag_source_id ASC) so rows are conceptually datapack-scoped:
// ownership is enforced at the API boundary via the datapacks.user_id
// FK and RequireDatapackOwnership middleware. The repo deliberately
// takes a *models.Datapack reference at the handler layer rather than
// re-resolving the datapack by name.
//
// systemDB serves meko_system.knowledge_bases. dataDB serves
// datapacks.* and dist_rag.*. When the api_server is run against a
// single physical database, the two handles are the same.
type KnowledgeBaseRepo struct {
	systemDB *sql.DB
	dataDB   *sql.DB
}

// NewKnowledgeBaseRepo creates a new KnowledgeBaseRepo. systemDB is
// used for meko_system.knowledge_bases CRUD; dataDB is used for the
// datapacks.* / dist_rag.* read-only counters and document listings.
func NewKnowledgeBaseRepo(systemDB, dataDB *sql.DB) *KnowledgeBaseRepo {
	return &KnowledgeBaseRepo{systemDB: systemDB, dataDB: dataDB}
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
	err := r.systemDB.QueryRow(
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

	_, err := r.systemDB.Exec(
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

	_, err := r.systemDB.Exec(
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
	err := r.systemDB.QueryRow(
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
	rows, err := r.systemDB.Query(
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
	err := r.systemDB.QueryRow(
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
	res, err := r.systemDB.Exec(
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
	_, err := r.systemDB.Exec(
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

// DocumentRef is the small per-file projection returned by
// GetDocumentRef — just enough for the DELETE flow to enforce
// permission (UploadedByUserID) and clean up S3 (DocumentURI).
//
// Both pointer fields can be nil for legacy rows: documents inserted
// before the dual-write don't carry a `meko.user_id`, and (rarely)
// dist_rag.documents.document_uri can be NULL. The permission rule
// treats a nil UploadedByUserID as "no file-uploader claim" — only
// datapack owner/maintainer can delete.
type DocumentRef struct {
	UploadedByUserID *string
	DocumentURI      *string
}

// GetDocumentRef returns the (uploader_user_id, document_uri) for a
// single file inside the given datapack. Used by the DELETE flow to
// (a) check whether the caller is the file's uploader, and (b) build
// the S3 key for the object delete step.
//
// Returns ErrDocumentNotFound when no row matches (datapack_id,
// document_id). The (datapack_id, document_id) pair is validated via
// the same Option-2 path the listing uses: join dist_rag.documents to
// the datapack's rag_source_ids through meko_system.knowledge_bases.
func (r *KnowledgeBaseRepo) GetDocumentRef(
	datapackID, documentID string,
) (DocumentRef, error) {
	// Two-step: meko_system and dist_rag now live on separate physical
	// databases, so we can't filter `dist_rag.documents` with an inline
	// subquery against `meko_system.knowledge_bases` — dataDB has at best
	// an empty shadow of that table, so the subquery would match nothing
	// and we'd 404 even on a row that exists.
	sourceIDs, err := r.GetSourceIDs(datapackID)
	if err != nil {
		return DocumentRef{}, fmt.Errorf("get document ref: source ids: %w", err)
	}
	if len(sourceIDs) == 0 {
		return DocumentRef{}, fmt.Errorf("%w: datapack_id=%s document_id=%s",
			ErrDocumentNotFound, datapackID, documentID)
	}
	var (
		uploaderUserID sql.NullString
		documentURI    sql.NullString
	)
	err = r.dataDB.QueryRow(
		`SELECT d.document_details->'meko'->>'user_id',
		        d.document_uri
		 FROM   dist_rag.documents d
		 WHERE  d.document_id = $1::uuid
		   AND  d.source_id = ANY($2::uuid[])`,
		documentID, pq.Array(sourceIDs),
	).Scan(&uploaderUserID, &documentURI)
	if errors.Is(err, sql.ErrNoRows) {
		return DocumentRef{}, fmt.Errorf("%w: datapack_id=%s document_id=%s",
			ErrDocumentNotFound, datapackID, documentID)
	}
	if err != nil {
		return DocumentRef{}, fmt.Errorf("get document ref: %w", err)
	}
	out := DocumentRef{}
	if uploaderUserID.Valid && strings.TrimSpace(uploaderUserID.String) != "" {
		s := uploaderUserID.String
		out.UploadedByUserID = &s
	}
	if documentURI.Valid && strings.TrimSpace(documentURI.String) != "" {
		s := documentURI.String
		out.DocumentURI = &s
	}
	return out, nil
}

// DeleteDocument removes one file's chunks from the chunk store
// (datapacks.knowledge_bases) AND the document + pipeline rows from
// dist_rag. The S3 object is deleted by the handler, not here.
//
// Order matters:
//  1. datapacks.knowledge_bases chunks  -- partition-pruned by
//     tenant_id, then range-scanned by document_id via the
//     `knowledge_bases_tenant_document_idx` secondary index added in
//     migration 000005. Without that index, this step would scan the
//     full hash partition; with it, the cost is O(chunks_for_file).
//  2. dist_rag.pipeline_details         -- explicit delete first so we
//     don't depend on the dist_rag schema having ON DELETE CASCADE
//     from documents. If CASCADE is configured, this is a harmless
//     no-op.
//  3. dist_rag.documents                -- final dist_rag-side delete.
//
// The three statements run in a single transaction so a partial
// failure can't leave dangling chunks pointing at a deleted document.
// Returns the chunk count actually removed so the handler can log /
// surface it. ErrDocumentNotFound when no dist_rag.documents row
// matches (datapack_id, document_id).
func (r *KnowledgeBaseRepo) DeleteDocument(
	datapackID, documentID string,
) (int64, error) {
	// Step 1 (systemDB): collect this datapack's source_ids. Used to
	// scope the step-3 DELETE against `dist_rag.documents` (so a caller
	// can't delete a row from another datapack just by guessing its
	// document_id). We can't keep the previous inline subquery against
	// `meko_system.knowledge_bases` because dataDB doesn't host that
	// table — the subquery would match zero rows and we'd 404 on
	// legitimate deletes.
	sourceIDs, err := r.GetSourceIDs(datapackID)
	if err != nil {
		return 0, fmt.Errorf("delete document: source ids: %w", err)
	}
	if len(sourceIDs) == 0 {
		return 0, fmt.Errorf("%w: datapack_id=%s document_id=%s",
			ErrDocumentNotFound, datapackID, documentID)
	}

	// All 3 DELETE statements live in one dataDB transaction —
	// datapacks.knowledge_bases (chunks), dist_rag.pipeline_details
	// (pipeline runs), dist_rag.documents (the row itself).
	tx, err := r.dataDB.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Step 1: chunks + embeddings.
	res, err := tx.Exec(
		`DELETE FROM datapacks.knowledge_bases
		 WHERE  tenant_id   = $1::uuid
		   AND  document_id = $2::uuid`,
		datapackID, documentID,
	)
	if err != nil {
		return 0, fmt.Errorf("delete chunks: %w", err)
	}
	chunksDeleted, _ := res.RowsAffected()

	// Step 2: pipeline_details — explicit, even if CASCADE is set.
	if _, err := tx.Exec(
		`DELETE FROM dist_rag.pipeline_details
		 WHERE  document_id = $1::uuid`,
		documentID,
	); err != nil {
		return 0, fmt.Errorf("delete pipeline_details: %w", err)
	}

	// Step 3: dist_rag.documents — scoped to the source_ids gathered
	// from systemDB at the top of the function, so a caller can't delete
	// a doc from another datapack by guessing its UUID.
	res, err = tx.Exec(
		`DELETE FROM dist_rag.documents
		 WHERE  document_id = $1::uuid
		   AND  source_id = ANY($2::uuid[])`,
		documentID, pq.Array(sourceIDs),
	)
	if err != nil {
		return 0, fmt.Errorf("delete dist_rag document: %w", err)
	}
	docsDeleted, _ := res.RowsAffected()
	if docsDeleted == 0 {
		// Document didn't belong to this datapack, or already gone.
		return 0, fmt.Errorf("%w: datapack_id=%s document_id=%s",
			ErrDocumentNotFound, datapackID, documentID)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit delete tx: %w", err)
	}
	return chunksDeleted, nil
}

// GetSourceIDs returns all rag_source_id values for a datapack, ordered
// by created_at descending.
func (r *KnowledgeBaseRepo) GetSourceIDs(
	datapackID string,
) ([]string, error) {
	rows, err := r.systemDB.Query(
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

// lookupUserEmails batch-resolves user_id → user_email against
// `meko_system.users` (systemDB). Replaces the cross-schema
// `LEFT JOIN meko_system.users` that several dataDB queries previously
// used inline; those queries can no longer reach the meko_system schema
// in their own SELECT now that the two schemas live on separate physical
// databases.
//
// Behavior:
//   - Empty / whitespace-only IDs are skipped (matches the previous
//     `NULLIF(..., '')::uuid` guard).
//   - Input is de-duplicated before the query (callers typically pass a
//     parallel slice with repeats).
//   - Missing rows are silently absent from the returned map; this
//     mirrors `LEFT JOIN` semantics: no row → no email → caller leaves
//     the email field nil.
//   - Returns nil map (not an error) when there are no IDs to look up.
func (r *KnowledgeBaseRepo) lookupUserEmails(userIDs []string) (map[string]string, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(userIDs))
	unique := make([]string, 0, len(userIDs))
	for _, id := range userIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return nil, nil
	}
	rows, err := r.systemDB.Query(
		`SELECT user_id::text, user_email
		 FROM   meko_system.users
		 WHERE  user_id = ANY($1::uuid[])`,
		pq.Array(unique),
	)
	if err != nil {
		return nil, fmt.Errorf("lookup user emails: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string, len(unique))
	for rows.Next() {
		var (
			uid   string
			email sql.NullString
		)
		if err := rows.Scan(&uid, &email); err != nil {
			return nil, fmt.Errorf("scan user email: %w", err)
		}
		if email.Valid && strings.TrimSpace(email.String) != "" {
			out[uid] = email.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user email rows: %w", err)
	}
	return out, nil
}

// GetSourceURIs returns the knowledge_base_url values for a datapack,
// ordered by created_at descending.
func (r *KnowledgeBaseRepo) GetSourceURIs(
	datapackID string,
) ([]string, error) {
	rows, err := r.systemDB.Query(
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

// KnowledgeBaseSummary feeds the header tile + auto-generated description
// + files-summary line on the SharedKnowledge page.
//
// Aggregates come from the same `latest_pipeline` view-of-the-view that
// the documents list uses, so a chunk count seen in the row list always
// rolls up to the same total here. PromotedMemoriesCount is filled in by
// the service from the existing promoted_collection count, not this
// struct's SQL.
type KnowledgeBaseSummary struct {
	TotalFiles           int64   `json:"total_files"`
	TotalChunks          int64   `json:"total_chunks"`
	LastFileUploadedAt   *string `json:"last_file_uploaded_at,omitempty"`
	HeaviestSourceName   *string `json:"heaviest_source_name,omitempty"`
	HeaviestSourceChunks *int64  `json:"heaviest_source_chunks,omitempty"`
	EmbeddingModel       *string `json:"embedding_model,omitempty"`
	EmbeddingDimensions  *int64  `json:"embedding_dimensions,omitempty"`
	// AIProvider is `dist_rag.vector_indexes.ai_provider` for the one
	// global Meko vector index (e.g. "OPENAI"). Lives on /summary as
	// the single source of truth for index-level constants — other
	// endpoints (e.g. /promoted-memories) intentionally do NOT carry
	// duplicates so we have one place to keep this in sync.
	AIProvider *string `json:"ai_provider,omitempty"`
	// ChunkSize comes from `dist_rag.vector_indexes.chunk_params->>'chunk_size'`.
	// Used as the suffix on the UI's chunking "strategy" label
	// (e.g. "semantic·1024"). Nullable when the index has no
	// chunk_params payload — UI then renders just "semantic".
	ChunkSize *int64 `json:"chunk_size,omitempty"`
}

// KnowledgeBaseDocument is one row of the uploaded-files listing.
//
// Fields beyond document_name/status are added for the SharedKnowledge
// summary page. `ChunksProcessed` joins from
// dist_rag.vector_index_pipeline_details (NULL when the doc hasn't
// finished embedding); `CompletedAt` is the latest pipeline run's
// finished timestamp — surfaced as "Uploaded" in the UI.
//
// Per-file provenance (`UploadedByEmail`, `SizeBytes`,
// `UploadedByUserID`) comes from `dist_rag.documents.document_details`
// JSONB under the `meko` sub-key; the SQL extracts it and LEFT JOINs
// `meko_system.users` to resolve the user_id into a display email.
// All three are pointer-nullable: documents that pre-date the dual-write
// have NULL details, in which case the UI shows "—".
//
// Embedding model + dimensions are deliberately NOT on this struct —
// they're properties of the vector index (one global index in Meko
// today, so the value is the same across every row). UI surfaces
// them from the `/summary` response and passes as a prop to per-row
// detail views.
type KnowledgeBaseDocument struct {
	ID               string  `json:"id"`
	DocumentName     string  `json:"document_name"`
	DocumentURI      string  `json:"document_uri"`
	DocumentType     *string `json:"document_type,omitempty"`
	DocumentChecksum *string `json:"document_checksum,omitempty"`
	ChunksProcessed  *int64  `json:"chunks_processed,omitempty"`
	CompletedAt      *string `json:"completed_at,omitempty"`
	Status           string  `json:"status"`
	// Per-file provenance from document_details->meko->{user_id,size}.
	UploadedByUserID *string `json:"uploaded_by_user_id,omitempty"`
	UploadedByEmail  *string `json:"uploaded_by_email,omitempty"`
	SizeBytes        *int64  `json:"size_bytes,omitempty"`
}

// Counts returns the chunk / doc / promoted-memory breakdown for a
// datapack.
func (r *KnowledgeBaseRepo) Counts(
	datapackID string,
) (KnowledgeBaseCounts, error) {
	var c KnowledgeBaseCounts

	if err := r.dataDB.QueryRow(
		`select count(distinct(document_id)) as doc_count
		 from datapacks.knowledge_bases
		 where tenant_id = $1::uuid
		   and metadata_filters->>'source' = 'doc'`,
		datapackID,
	).Scan(&c.TotalDocs); err != nil {
		return c, fmt.Errorf("count total_docs: %w", err)
	}

	if err := r.dataDB.QueryRow(
		`select count(*) as chunk_count
		 from datapacks.knowledge_bases
		 where tenant_id = $1::uuid`,
		datapackID,
	).Scan(&c.TotalChunks); err != nil {
		return c, fmt.Errorf("count total_chunks: %w", err)
	}

	if err := r.dataDB.QueryRow(
		`select count(*)
		 from datapacks.promoted_collection
		 where meko_datapack_id = $1::uuid`,
		datapackID,
	).Scan(&c.PromotedMemories); err != nil {
		return c, fmt.Errorf("count promoted_memories: %w", err)
	}

	return c, nil
}

// DocumentListOpts controls pagination and filtering for ListDistRagDocuments.
//
// Limit/Offset are required (service layer enforces min/max bounds).
// Filter fields are all optional — zero value means "no filter":
//   - SearchQuery: case-insensitive substring match on document_name.
//   - TypeFilter:  file-extension suffix match, e.g. ".md". Matched
//     against the full document_name path so ".md" correctly filters
//     "user/pack/file.md" without a JOIN.
//   - IndexedOnly: when true, restricts to documents in COMPLETED status.
type DocumentListOpts struct {
	Limit       int
	Offset      int
	SearchQuery string // ILIKE '%<val>%' on document_name; empty = no filter
	TypeFilter  string // LIKE '%<val>'  on document_name; empty = no filter
	IndexedOnly bool   // true → status = 'COMPLETED' only
}

// ListDistRagDocuments returns one row per uploaded file for the given
// datapack, enriched with chunk count, MD5, type, and the latest
// pipeline's completed_at (rendered as "Uploaded" in the UI).
//
// Reads from the `dist_rag.vector_index_pipeline_details` view, which
// already INNER-JOINs vector_indexes → mappings → sources → documents
// and LEFT-JOINs pipeline_details.
//
// === Query entry point ===
// Routes via meko_system.knowledge_bases (PK `(datapack_id HASH,
// rag_source_id ASC)` → `WHERE datapack_id = $1`
//
// Why not filter on `dist_rag.sources.tenant_id` directly? Because
// today there's no index on it — that path would seq-scan
// `dist_rag.sources` before any of the other indexes can help. The
// PK-driven path here is fully index-driven at every hop.
//
// Two CTEs:
//
//  1. `datapack_sources` — fast meko-side PK lookup for this datapack's
//     rag_source_ids. Typically 1 row per datapack.
//  2. `latest_per_document` — collapses per-pipeline-run rows in the
//     view to the most recent run per document. Without this dedup, a
//     doc that's been re-indexed N times shows up N times.
//
// COUNT(*) OVER() in the final SELECT gives the total matching-row
// count at query time so the caller can derive has_more without a
// separate COUNT query.
//
// `schema` and `indexName` are the service-layer constants
// ("datapacks" / "knowledge_bases" today).
func (r *KnowledgeBaseRepo) ListDistRagDocuments(
	datapackID, schema, indexName string,
	opts DocumentListOpts,
) ([]KnowledgeBaseDocument, int64, error) {
	// Step 1 (systemDB): the datapack's rag_source_id values. Run as a
	// separate round-trip because meko_system and dist_rag now live on
	// different physical databases — the previous inline CTE that read
	// `meko_system.knowledge_bases` from a dataDB query is no longer
	// possible.
	sourceIDs, err := r.GetSourceIDs(datapackID)
	if err != nil {
		return nil, 0, fmt.Errorf("list dist_rag documents: source ids: %w", err)
	}
	if len(sourceIDs) == 0 {
		return nil, 0, nil
	}

	// Step 2 (dataDB): documents page.
	//   - `document_details->meko` carries the uploader user_id and size.
	//   - user_email enrichment is intentionally NOT joined here (the
	//     `LEFT JOIN meko_system.users` previously inline is now an
	//     out-of-line systemDB lookup — see step 3).
	//   - NULLIF on the user_id JSON path matches the previous query's
	//     "" → NULL guard (avoids passing a non-uuid empty string up to
	//     Go-side enrichment).
	//
	// Filter predicates use the "($N = '' OR ...)" short-circuit so the
	// same parameterized query handles all filter combinations without
	// dynamic SQL. IndexedOnly uses a bool param instead.
	rows, err := r.dataDB.Query(
		`WITH latest_per_document AS (
		   SELECT DISTINCT ON (document_id)
		          document_id,
		          document_name,
		          document_uri,
		          document_checksum,
		          chunks_processed,
		          COALESCE(pipeline_completed_at, pipeline_last_started_at, pipeline_created_at) AS pipeline_completed_at,
		          COALESCE(pipeline_status::text, document_status::text) AS status
		   FROM   dist_rag.vector_index_pipeline_details
		   WHERE  source_id = ANY($1::uuid[])
		     AND  schema_name = $2
		     AND  index_name  = $3
		   ORDER  BY document_id,
		             pipeline_created_at DESC NULLS LAST,
		             pipeline_id DESC
		 )
		 SELECT lpd.document_id::text,
		        lpd.document_name,
		        lpd.document_uri,
		        d.document_type,
		        lpd.document_checksum,
		        lpd.pipeline_completed_at,
		        lpd.chunks_processed,
		        lpd.status,
		        NULLIF(d.document_details->'meko'->>'user_id', '') AS uploaded_by_user_id,
		        (d.document_details->'meko'->>'size')::bigint      AS size_bytes,
		        COUNT(*) OVER()                                    AS total_count
		 FROM   latest_per_document lpd
		 LEFT JOIN dist_rag.documents d
		   ON   d.document_id = lpd.document_id
		 WHERE  ($4 = '' OR lpd.document_name ILIKE '%' || $4 || '%')
		   AND  ($5 = '' OR lower(lpd.document_name) LIKE '%' || lower($5))
		   AND  (NOT $6  OR lpd.status = 'COMPLETED')
		 ORDER  BY lpd.pipeline_completed_at DESC NULLS LAST, lpd.document_name
		 LIMIT  $7 OFFSET $8`,
		pq.Array(sourceIDs), schema, indexName,
		opts.SearchQuery, opts.TypeFilter, opts.IndexedOnly,
		opts.Limit, opts.Offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list dist_rag documents: %w", err)
	}
	defer rows.Close()

	var total int64
	docs := []KnowledgeBaseDocument{}
	// Parallel to `docs`: uploader user_id per row, so we can attach
	// emails after the batch systemDB lookup. Empty string for rows
	// without an uploader_id (those just stay un-enriched).
	uploaderIDs := []string{}
	for rows.Next() {
		var (
			d              KnowledgeBaseDocument
			name           sql.NullString
			uri            sql.NullString
			docType        sql.NullString
			checksum       sql.NullString
			completedAt    sql.NullTime
			chunks         sql.NullInt64
			uploaderUserID sql.NullString
			sizeBytes      sql.NullInt64
			totalCount     sql.NullInt64
		)
		if err := rows.Scan(
			&d.ID, &name, &uri, &docType, &checksum, &completedAt, &chunks, &d.Status,
			&uploaderUserID, &sizeBytes, &totalCount,
		); err != nil {
			return nil, 0, fmt.Errorf("scan document row: %w", err)
		}
		if name.Valid {
			d.DocumentName = name.String
		}
		if uri.Valid {
			d.DocumentURI = uri.String
		}
		if docType.Valid {
			s := docType.String
			d.DocumentType = &s
		}
		if checksum.Valid {
			s := checksum.String
			d.DocumentChecksum = &s
		}
		if completedAt.Valid {
			s := completedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
			d.CompletedAt = &s
		}
		if chunks.Valid {
			n := chunks.Int64
			d.ChunksProcessed = &n
		}
		if uploaderUserID.Valid && strings.TrimSpace(uploaderUserID.String) != "" {
			s := uploaderUserID.String
			d.UploadedByUserID = &s
			uploaderIDs = append(uploaderIDs, s)
		} else {
			uploaderIDs = append(uploaderIDs, "")
		}
		if sizeBytes.Valid {
			n := sizeBytes.Int64
			d.SizeBytes = &n
		}
		if totalCount.Valid {
			total = totalCount.Int64
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate document rows: %w", err)
	}

	// Step 3 (systemDB): batch-resolve uploader emails. Best-effort —
	// matches the previous LEFT JOIN's "missing user → no email"
	// semantics; the rest of the row still ships.
	emails, err := r.lookupUserEmails(uploaderIDs)
	if err != nil {
		slog.Warn("list dist_rag documents: user email lookup failed",
			"datapack_id", datapackID, "error", err)
		emails = nil
	}
	for i := range docs {
		uid := uploaderIDs[i]
		if uid == "" {
			continue
		}
		if e, ok := emails[uid]; ok {
			s := e
			docs[i].UploadedByEmail = &s
		}
	}
	return docs, total, nil
}

// ActivityLogItem is one row in the merged file-uploads + memory-promotions
// feed shown at the bottom of the SharedKnowledge page. The `Type`
// discriminator tells the UI which side of the union populated this
// row; the other side's fields stay nil.
//
// Wire shape is intentionally flat (not a discriminated union of two
// embedded structs) so JSON consumers don't need polymorphic decoding.
type ActivityLogItem struct {
	Type      string `json:"type"` // "file_upload" | "memory_promotion"
	Timestamp string `json:"timestamp"`

	// file_upload fields (nil on memory_promotion rows)
	DocumentName    *string `json:"document_name,omitempty"`
	DocumentType    *string `json:"document_type,omitempty"`
	ChunksProcessed *int64  `json:"chunks_processed,omitempty"`
	SizeBytes       *int64  `json:"size_bytes,omitempty"`
	UploadedByEmail *string `json:"uploaded_by_email,omitempty"`

	// memory_promotion fields (nil on file_upload rows)
	MemoryID       *string `json:"memory_id,omitempty"`
	Content        *string `json:"content,omitempty"`
	AgentID        *string `json:"agent_id,omitempty"`
	ConversationID *string `json:"conversation_id,omitempty"`
}

// ActivityLogCursor is the opaque pagination state for ListActivityLog.
//
// Two per-side keys are kept (one for file_uploads, one for promotions)
// because the feed is the in-memory merge of two independent queries:
// after returning a page that contained 8 files + 2 promotions, the
// next page must skip past the 8th file on the file side AND the 2nd
// promotion on the promotion side. A single shared (ts, id) cursor
// would not work — file_uploads and promotions can have rows with
// identical timestamps that should still both appear.
//
// Each side's (ts, id) is the SQL row-comparison key
// `(timestamp_col, primary_key_uuid)` used in `WHERE (ts, id) < (...)`
// against the DESC-sorted result set. Empty strings mean "this side has
// no cursor yet" (first page) or "this side is exhausted" (no more
// rows after the previous fetch).
//
// Marshalled to base64(JSON) on the wire to keep it opaque to the
// client — the client just round-trips whatever the server gave it.
type ActivityLogCursor struct {
	FileTs  string `json:"f_ts,omitempty"`
	FileID  string `json:"f_id,omitempty"`
	PromoTs string `json:"p_ts,omitempty"`
	PromoID string `json:"p_id,omitempty"`
}

// EncodeActivityLogCursor returns the opaque wire form of a cursor, or
// the empty string for the zero cursor (so the JSON envelope can render
// `next_cursor: ""` cleanly when the feed is exhausted).
func EncodeActivityLogCursor(c ActivityLogCursor) string {
	if c == (ActivityLogCursor{}) {
		return ""
	}
	b, err := json.Marshal(c)
	if err != nil {
		// Shouldn't fail for this fixed shape; falling back to empty
		// degrades to "start from the beginning" rather than crashing.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeActivityLogCursor parses the opaque wire form. Empty / invalid
// strings yield the zero cursor (interpreted as "first page") rather
// than an error: a malformed cursor in a URL shouldn't 500 the user.
func DecodeActivityLogCursor(s string) ActivityLogCursor {
	s = strings.TrimSpace(s)
	if s == "" {
		return ActivityLogCursor{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ActivityLogCursor{}
	}
	var c ActivityLogCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return ActivityLogCursor{}
	}
	return c
}

// activityRow wraps an ActivityLogItem with its sort + cursor key.
// `id` is the per-side UUID (document_id for files, memory id for
// promotions) used as the tiebreaker in the `(ts, id)` cursor.
type activityRow struct {
	item ActivityLogItem
	ts   string
	id   string
}

// ListActivityLog returns a merged, time-ordered feed of file uploads
// + promoted memories for the SharedKnowledge "Activity log" panel.
//
// Keyset pagination: caller passes the opaque `cursor` returned by the
// previous call (empty for the first page). Each side filters with the
// SQL row-comparison `(ts, id) < (:cursor_ts, :cursor_id)` against the
// DESC-sorted stream, fetching `limit + 1` rows. The two sides are
// merged in Go, the first `limit` are returned, and `nextCursor`
// captures the last-kept (ts, id) per side. Cost is constant per page
// regardless of how deep the user scrolls.
//
// Resilience: the two sides are fetched independently. If ONE side
// errors (e.g. dist_rag is unavailable but promoted memories load
// fine) the other side is still served and we log a warning. Only
// when BOTH fail do we bubble an error to the caller.
//
// Both sides are per-datapack from the planner's entry:
//   - file_uploads: meko_system.knowledge_bases PK lookup → dist_rag
//     view via `source_id IN (...)`. Same Option-2 pattern as
//     ListDistRagDocuments.
//   - promotions:   PK-prefix lookup on
//     datapacks.promoted_collection(meko_datapack_id HASH, …).
//
// Neither side seq-scans.
//
// `schema` and `indexName` are the service-layer constants
// ("datapacks" / "knowledge_bases").
func (r *KnowledgeBaseRepo) ListActivityLog(
	datapackID, schema, indexName string,
	limit int, cursor ActivityLogCursor,
) (items []ActivityLogItem, nextCursor ActivityLogCursor, err error) {
	if limit <= 0 {
		limit = 10
	}

	// Each side fetches `limit + 1`: enough to fill the page from
	// either side alone, plus one extra to detect "has more" without
	// a separate COUNT.
	perSideCap := limit + 1

	files, fileErr := r.listActivityFileUploads(
		datapackID, schema, indexName,
		cursor.FileTs, cursor.FileID, perSideCap,
	)
	promos, promoErr := r.listActivityPromotions(
		datapackID, cursor.PromoTs, cursor.PromoID, perSideCap,
	)

	// If BOTH sides failed, the feed has nothing to show — propagate.
	if fileErr != nil && promoErr != nil {
		return nil, ActivityLogCursor{}, fmt.Errorf(
			"activity log: both sources failed (files: %v, promotions: %v)",
			fileErr, promoErr,
		)
	}
	// Partial failure: log + degrade. The good side serves alone.
	if fileErr != nil {
		slog.Warn("activity log: file uploads side failed; serving promotions only",
			"datapack_id", datapackID, "error", fileErr)
		files = nil
	}
	if promoErr != nil {
		slog.Warn("activity log: promotions side failed; serving file uploads only",
			"datapack_id", datapackID, "error", promoErr)
		promos = nil
	}

	// Merge + sort by ts DESC, tiebreak by id DESC. Stable sort so
	// rows with identical (ts, id) keep their per-side ordering.
	merged := make([]activityRow, 0, len(files)+len(promos))
	merged = append(merged, files...)
	merged = append(merged, promos...)
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].ts != merged[j].ts {
			return merged[i].ts > merged[j].ts
		}
		return merged[i].id > merged[j].id
	})

	// Take the first `limit` rows. Track the last kept row from each
	// side so we can build a per-side next cursor. Rows we fetched but
	// did NOT keep are simply dropped — they'll be re-fetched on the
	// next page using the kept-side cursor as the filter.
	//
	// NOTE: `take` is a local clamp; we deliberately leave the input
	// `limit` un-mutated so the has_more decision below can compare
	// against the ORIGINAL page size, not the clamped one.
	take := limit
	if take > len(merged) {
		take = len(merged)
	}
	out := make([]ActivityLogItem, 0, take)
	var lastFile, lastPromo *activityRow
	for i := range merged[:take] {
		row := &merged[i]
		out = append(out, row.item)
		switch row.item.Type {
		case "file_upload":
			lastFile = row
		case "memory_promotion":
			lastPromo = row
		}
	}

	// Next cursor: per-side last-kept (ts, id). If a side contributed
	// nothing to this page, carry forward the incoming cursor for that
	// side so we don't reset its position to "start".
	nextCursor = cursor
	if lastFile != nil {
		nextCursor.FileTs = lastFile.ts
		nextCursor.FileID = lastFile.id
	}
	if lastPromo != nil {
		nextCursor.PromoTs = lastPromo.ts
		nextCursor.PromoID = lastPromo.id
	}

	// Feed-exhaustion signal: the zero cursor encodes to "" at the
	// wire boundary, which the service layer converts to has_more=false.
	//
	// We use a "filled the page?" heuristic instead of trusting the
	// per-side over-fetch sentinel, because that sentinel is fragile:
	// the keyset cursor uses tuple comparison `(ts, id) < (ts, id)`
	// on timestamptz + uuid, and PostgreSQL truncates Go's nanosecond
	// timestamps to microseconds, which can drop a row from the
	// next-page query unexpectedly (e.g. same-second uploads).
	//
	// Rule: if we filled the requested page exactly, optimistically
	// assume more might exist — the next call will discover the empty
	// tail and terminate (one extra round-trip at the very end of the
	// feed; cheap and unconditionally correct). Only when we returned
	// FEWER rows than `limit` do we know for sure the feed is
	// exhausted.
	if len(out) < limit {
		nextCursor = ActivityLogCursor{}
	}

	return out, nextCursor, nil
}

// listActivityFileUploads pulls the file-upload side of the activity
// log: one event per document, most-recent pipeline completion as the
// timestamp. Bounded by `cap` rows (caller passes `limit + 1` so the
// merge layer can detect "has more" without a COUNT).
//
// Keyset cursor: when `cursorTs` / `cursorID` are non-empty, the SQL
// filters to rows strictly after that key in the DESC sort using a
// Postgres tuple comparison — `(sort_ts, document_id) < ($ts, $id)`.
// Empty cursor strings mean "no cursor yet" → no extra WHERE clause.
//
// CRITICAL: the cursor uses `sort_ts` (COALESCEd with epoch), NOT the
// nullable `display_ts`. PG tuple comparison treats NULLs as falsy, so
// if we keyset on the display timestamp, rows with no pipeline timestamp
// at all are silently skipped from every cursor query and become
// unreachable past page 1. Switching to a never-NULL sort key fixes that
// while keeping the displayed timestamp truthfully NULL for the UI.
//
// Two-step fetch because the meko_system and dist_rag schemas now live
// on separate physical databases:
//
//  1. systemDB: gather this datapack's rag_source_id values (`GetSourceIDs`).
//  2. dataDB:  pipeline rows filtered by `source_id = ANY($1::uuid[])`.
//             Drops the previous `LEFT JOIN meko_system.users` —
//             uploader_user_id is selected raw from `document_details`.
//  3. systemDB: batch-resolve user_id → user_email (`lookupUserEmails`).
//  4. Go:      merge the email back into each row.
//
// (When the two physical DBs are colocated, this is one extra trivial
// round-trip; when they're truly split, this is the only correct shape.)
func (r *KnowledgeBaseRepo) listActivityFileUploads(
	datapackID, schema, indexName string,
	cursorTs, cursorID string,
	cap int,
) ([]activityRow, error) {
	// Step 1: source IDs for this datapack (systemDB).
	sourceIDs, err := r.GetSourceIDs(datapackID)
	if err != nil {
		return nil, fmt.Errorf("activity log file uploads: source ids: %w", err)
	}
	if len(sourceIDs) == 0 {
		return nil, nil
	}

	// Step 2: pipeline rows (dataDB). Two parameter layouts so we don't
	// pay for unnecessary type casts on the empty-cursor path; the body
	// is identical, only the keyset WHERE differs.
	const baseQ = `WITH latest_per_document AS (
		   SELECT DISTINCT ON (document_id)
		          document_id, document_name, document_type, document_details,
		          chunks_processed,
		          COALESCE(pipeline_completed_at, pipeline_last_started_at, pipeline_created_at)               AS display_ts,
		          COALESCE(pipeline_completed_at, pipeline_last_started_at, pipeline_created_at, 'epoch'::timestamptz) AS sort_ts
		   FROM   dist_rag.vector_index_pipeline_details
		   WHERE  source_id = ANY($1::uuid[])
		     AND  schema_name = $2
		     AND  index_name  = $3
		   ORDER  BY document_id,
		             sort_ts DESC,
		             pipeline_id DESC
		 )
		 SELECT l.display_ts,
		        l.sort_ts,
		        l.document_id::text,
		        l.document_name,
		        l.document_type,
		        l.chunks_processed,
		        (l.document_details->'meko'->>'size')::bigint AS size_bytes,
		        NULLIF(l.document_details->'meko'->>'user_id', '') AS uploaded_by_user_id
		 FROM   latest_per_document l `

	var rows *sql.Rows
	if cursorTs == "" || cursorID == "" {
		rows, err = r.dataDB.Query(
			baseQ+
				` ORDER BY sort_ts DESC, document_id DESC
				  LIMIT $4`,
			pq.Array(sourceIDs), schema, indexName, cap,
		)
	} else {
		rows, err = r.dataDB.Query(
			baseQ+
				` WHERE (sort_ts, document_id) < ($4::timestamptz, $5::uuid)
				  ORDER BY sort_ts DESC, document_id DESC
				  LIMIT $6`,
			pq.Array(sourceIDs), schema, indexName, cursorTs, cursorID, cap,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("activity log file uploads: %w", err)
	}
	defer rows.Close()

	out := []activityRow{}
	// Parallel to `out`: uploader user_id per row, so we can attach the
	// email after the batch lookup. Empty string for rows without an
	// uploader_id (their email entry just stays unset).
	uploaderIDs := []string{}
	for rows.Next() {
		var (
			displayTs        sql.NullTime
			sortTs           time.Time // never NULL — backed by COALESCE(..., 'epoch')
			docID            sql.NullString
			docName          sql.NullString
			docType          sql.NullString
			chunks           sql.NullInt64
			sizeBytes        sql.NullInt64
			uploaderUserID   sql.NullString
		)
		if err := rows.Scan(&displayTs, &sortTs, &docID, &docName, &docType, &chunks, &sizeBytes, &uploaderUserID); err != nil {
			return nil, fmt.Errorf("scan file upload row: %w", err)
		}
		it := ActivityLogItem{Type: "file_upload"}
		// Display timestamp stays nullable — UI renders "—" when this is
		// empty (matches the existing convention for rows whose pipeline
		// never produced a timestamp).
		if displayTs.Valid {
			it.Timestamp = displayTs.Time.UTC().Format(time.RFC3339Nano)
		}
		if docName.Valid {
			s := docName.String
			it.DocumentName = &s
		}
		if docType.Valid {
			s := docType.String
			it.DocumentType = &s
		}
		if chunks.Valid {
			it.ChunksProcessed = &chunks.Int64
		}
		if sizeBytes.Valid {
			it.SizeBytes = &sizeBytes.Int64
		}
		out = append(out, activityRow{
			item: it,
			// Cursor / merge sort use sort_ts (always non-NULL), NOT
			// the display value — see the function-level comment.
			ts: sortTs.UTC().Format(time.RFC3339Nano),
			id: docID.String,
		})
		if uploaderUserID.Valid {
			uploaderIDs = append(uploaderIDs, strings.TrimSpace(uploaderUserID.String))
		} else {
			uploaderIDs = append(uploaderIDs, "")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate file upload rows: %w", err)
	}

	// Step 3: batch-resolve uploader emails (systemDB). Best-effort:
	// matches the previous LEFT JOIN's "missing user → no email" semantics.
	emails, err := r.lookupUserEmails(uploaderIDs)
	if err != nil {
		slog.Warn("activity log file uploads: user email lookup failed",
			"datapack_id", datapackID, "error", err)
		emails = nil
	}
	// Step 4: merge emails onto rows.
	for i := range out {
		uid := uploaderIDs[i]
		if uid == "" {
			continue
		}
		if e, ok := emails[uid]; ok {
			s := e
			out[i].item.UploadedByEmail = &s
		}
	}
	return out, nil
}

// listActivityPromotions pulls the memory-promotion side of the
// activity log. Uses the datapacks.promoted_collection PK-prefix
// lookup (meko_datapack_id HASH first). Bounded by `cap` like the
// other side. Keyset cursor mirrors the file-uploads side: tuple
// comparison on (meko_promoted_to_kb_at, id).
func (r *KnowledgeBaseRepo) listActivityPromotions(
	datapackID string,
	cursorTs, cursorID string,
	cap int,
) ([]activityRow, error) {
	const baseQ = `SELECT meko_promoted_to_kb_at,
		        id::text,
		        COALESCE(payload->>'data', '') AS content,
		        meko_agent_id,
		        meko_conversation_id::text
		 FROM   datapacks.promoted_collection
		 WHERE  meko_datapack_id = $1::uuid
		   AND  meko_promoted_to_kb_at IS NOT NULL `

	var (
		rows *sql.Rows
		err  error
	)
	if cursorTs == "" || cursorID == "" {
		rows, err = r.dataDB.Query(
			baseQ+
				` ORDER BY meko_promoted_to_kb_at DESC NULLS LAST, id DESC
				  LIMIT $2`,
			datapackID, cap,
		)
	} else {
		rows, err = r.dataDB.Query(
			baseQ+
				`   AND  (meko_promoted_to_kb_at, id) < ($2::timestamptz, $3::uuid)
				 ORDER BY meko_promoted_to_kb_at DESC NULLS LAST, id DESC
				 LIMIT $4`,
			datapackID, cursorTs, cursorID, cap,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("activity log promotions: %w", err)
	}
	defer rows.Close()

	out := []activityRow{}
	for rows.Next() {
		var (
			ts      sql.NullTime
			memID   sql.NullString
			content sql.NullString
			agentID sql.NullString
			convID  sql.NullString
		)
		if err := rows.Scan(&ts, &memID, &content, &agentID, &convID); err != nil {
			return nil, fmt.Errorf("scan promotion row: %w", err)
		}
		it := ActivityLogItem{Type: "memory_promotion"}
		if ts.Valid {
			it.Timestamp = ts.Time.UTC().Format(time.RFC3339Nano)
		}
		if memID.Valid {
			s := memID.String
			it.MemoryID = &s
		}
		if content.Valid {
			s := content.String
			it.Content = &s
		}
		if agentID.Valid {
			s := agentID.String
			it.AgentID = &s
		}
		if convID.Valid {
			s := convID.String
			it.ConversationID = &s
		}
		out = append(out, activityRow{
			item: it,
			ts:   it.Timestamp,
			id:   memID.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate promotion rows: %w", err)
	}
	return out, nil
}

// GetKnowledgeBaseSummary returns header-tile aggregates for the
// SharedKnowledge page in TWO round trips:
//
//  1. doc/chunk/upload aggregates: same `latest_pipeline` CTE the
//     documents list uses, then COUNT/SUM/MAX across it. The heaviest
//     source is pulled in the same query via a subselect so we don't
//     need a second pass over the rows.
//  2. embedding model metadata: vector_indexes JSONB lookup.
//
// PromotedMemoriesCount is intentionally NOT included here — it's
// already exposed by GET /promoted-memories which the UI calls anyway
// for the Promoted memories tab. Duplicating it would mean an extra
// SQL roundtrip we'd never use.
//
// All embedding-model fields are pointer-nullable: the vector index may
// not exist yet for fresh datapacks (KB never created).
func (r *KnowledgeBaseRepo) GetKnowledgeBaseSummary(
	datapackID, schema, indexName string,
) (KnowledgeBaseSummary, error) {
	var (
		s             KnowledgeBaseSummary
		lastUploaded  sql.NullTime
		heaviestName  sql.NullString
		heaviestChunk sql.NullInt64
	)

	// Aggregate query: collapses the view's per-pipeline-run rows to the
	// latest run per document, then narrows to COMPLETED-only before
	// counting / summing / picking the heaviest source. This matches
	// product semantics — "files" in the header tile and "heaviest
	// source" both mean successfully-ingested files, not in-flight ones.
	//
	// Two-step CTE intentional:
	//   `latest_per_document` (1) picks the most recent pipeline run
	//     per document (DISTINCT ON); without this step a doc with N
	//     prior failed re-indexes would expose a failed status even
	//     though its latest run succeeded.
	//   `completed_documents` (2) then filters that "latest snapshot"
	//     to COMPLETED-only. So a file that previously succeeded but
	//     just re-failed is correctly excluded.
	//
	// Step 1 (systemDB): resolve the datapack's rag_source_id values.
	// Done as a separate round-trip because meko_system and dist_rag
	// now live on different physical databases — a single query can't
	// span both.
	sourceIDs, err := r.GetSourceIDs(datapackID)
	if err != nil {
		return s, fmt.Errorf("kb summary aggregates: source ids: %w", err)
	}
	// No KB rows for this datapack → all aggregates stay zero/nil; still
	// run the embedding-model lookup below since that's a global index
	// config not gated on per-datapack sources.
	if len(sourceIDs) > 0 {
		if err := r.dataDB.QueryRow(
			`WITH latest_per_document AS (
		   SELECT DISTINCT ON (document_id)
		          document_id,
		          document_name,
		          pipeline_completed_at,
		          chunks_processed,
		          COALESCE(pipeline_status::text, document_status::text) AS status
		   FROM   dist_rag.vector_index_pipeline_details
		   WHERE  source_id = ANY($1::uuid[])
		     AND  schema_name = $2
		     AND  index_name  = $3
		   ORDER  BY document_id,
		             pipeline_created_at DESC NULLS LAST,
		             pipeline_id DESC
		 ),
		 completed_documents AS (
		   SELECT document_id, document_name, pipeline_completed_at, chunks_processed
		   FROM   latest_per_document
		   WHERE  status = 'COMPLETED'
		 )
		 SELECT
		   COUNT(*)                                   AS total_files,
		   COALESCE(SUM(chunks_processed), 0)::bigint AS total_chunks,
		   MAX(pipeline_completed_at)                 AS last_uploaded,
		   (SELECT document_name FROM completed_documents
		      WHERE chunks_processed IS NOT NULL
		      ORDER BY chunks_processed DESC NULLS LAST
		      LIMIT 1)                                AS heaviest_name,
		   (SELECT chunks_processed FROM completed_documents
		      WHERE chunks_processed IS NOT NULL
		      ORDER BY chunks_processed DESC NULLS LAST
		      LIMIT 1)                                AS heaviest_chunks
		 FROM completed_documents`,
			pq.Array(sourceIDs), schema, indexName,
		).Scan(&s.TotalFiles, &s.TotalChunks, &lastUploaded, &heaviestName, &heaviestChunk); err != nil {
			return s, fmt.Errorf("kb summary aggregates: %w", err)
		}
	}
	if lastUploaded.Valid {
		t := lastUploaded.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		s.LastFileUploadedAt = &t
	}
	if heaviestName.Valid {
		// document_name is stored with the full uploader path (e.g.
		// "<user_id>/<datapack_id>/PAT_validation.md"); the API
		// surface should be the basename only — UI shouldn't have to
		// re-strip the prefix. The doc list endpoint keeps the full
		// path because callers may want to reconstruct the S3 key;
		// the header summary is purely cosmetic.
		n := documentBasename(heaviestName.String)
		s.HeaviestSourceName = &n
	}
	if heaviestChunk.Valid {
		c := heaviestChunk.Int64
		s.HeaviestSourceChunks = &c
	}

	// Embedding model + chunking metadata. Optional: when the user
	// hasn't created the KB yet there's no row in vector_indexes — we
	// just leave the fields nil and the UI degrades to "—" / "semantic".
	//
	// Both JSONB columns live on the same `vector_indexes` row, so a
	// single SELECT pulls all three fields. `chunk_size` is shown as
	// the suffix on the UI's "semantic·N" strategy label.
	var (
		model     sql.NullString
		dims      sql.NullInt64
		chunkSize sql.NullInt64
		provider  sql.NullString
	)
	// Both tables live in `dist_rag.*` → dataDB.
	if err := r.dataDB.QueryRow(
		`SELECT vi.embedding_model_params->>'model',
		        (vi.embedding_model_params->>'dimensions')::bigint,
		        (vism.chunk_params->>'chunk_size')::bigint,
		        vi.ai_provider::text
		 FROM   dist_rag.vector_indexes vi
		 LEFT JOIN dist_rag.vector_index_source_mappings vism ON vism.index_id = vi.id
		 WHERE  vi.schema_name = $1 AND vi.index_name = $2
		 LIMIT 1`,
		schema, indexName,
	).Scan(&model, &dims, &chunkSize, &provider); err != nil && err != sql.ErrNoRows {
		return s, fmt.Errorf("kb summary embedding model: %w", err)
	}
	if model.Valid {
		m := model.String
		s.EmbeddingModel = &m
	}
	if dims.Valid {
		d := dims.Int64
		s.EmbeddingDimensions = &d
	}
	if chunkSize.Valid {
		n := chunkSize.Int64
		s.ChunkSize = &n
	}
	if provider.Valid {
		p := provider.String
		s.AIProvider = &p
	}

	return s, nil
}
