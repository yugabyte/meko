package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Knowledge file statuses, as Meko Cloud names them (Meko never sets its NOT_STARTED or RETRY).
const (
	DocQueued     = "QUEUED"
	DocProcessing = "PROCESSING"
	DocCompleted  = "COMPLETED"
	DocFailed     = "FAILED"
)

// ErrDocumentProcessing is a file that can't be deleted yet: it's queued or being indexed.
var ErrDocumentProcessing = errors.New("the file is still being indexed")

// Document is a knowledge file, without its bytes (they're in the files folder).
type Document struct {
	Rowid           int64
	ID              string
	DatapackID      string
	Key             string
	Filename        string
	DocumentType    string
	ContentHash     string
	SizeBytes       int64
	Status          string
	Error           string
	ChunksTotal     *int // nil until its text is chunked
	ChunksProcessed int  // its chunks embedded so far
	UploadedBy      string
	UploadedAt      time.Time
	CompletedAt     *time.Time
}

const docCols = "d.rowid, d.id, d.datapack_id, d.document_key, d.filename, d.document_type, d.content_hash, " +
	"d.size_bytes, d.status, d.error, d.chunks_total, " +
	"(SELECT count(*) FROM kb_chunks c WHERE c.document_rowid = d.rowid AND c.embedded = 1), " +
	"d.uploaded_by, d.uploaded_at, d.completed_at"

func scanDocument(row interface{ Scan(...any) error }) (Document, error) {
	var d Document
	var total sql.NullInt64
	var uploaded string
	var completed sql.NullString
	if err := row.Scan(&d.Rowid, &d.ID, &d.DatapackID, &d.Key, &d.Filename, &d.DocumentType, &d.ContentHash,
		&d.SizeBytes, &d.Status, &d.Error, &total, &d.ChunksProcessed, &d.UploadedBy, &uploaded,
		&completed); err != nil {
		return Document{}, err
	}
	if total.Valid {
		n := int(total.Int64)
		d.ChunksTotal = &n
	}
	var err error
	if d.UploadedAt, err = time.Parse(timeLayout, uploaded); err != nil {
		return Document{}, err
	}
	if completed.Valid {
		t, err := time.Parse(timeLayout, completed.String)
		if err != nil {
			return Document{}, err
		}
		d.CompletedAt = &t
	}
	return d, nil
}

// Added says what AddDocument did with a file.
type Added int

// What AddDocument did.
const (
	AddedNew      Added = iota // a new file, queued
	AddedExisting              // the datapack already has these bytes; nothing changed
	AddedRequeued              // the datapack had these bytes but they FAILED: queued again
)

// AddDocument stores a file's row, queued for indexing, and marks the datapack as having a
// knowledge base. The datapack is unique per content: the same bytes again return the row already
// there, queued again if it FAILED (its old chunks dropped). d's ID, status and times are set here.
func (s *Store) AddDocument(ctx context.Context, d Document) (Document, Added, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return Document{}, 0, err
	}
	defer tx.Rollback()
	if err = datapackExists(ctx, tx, d.DatapackID); err != nil {
		return Document{}, 0, err
	}
	now := time.Now().UTC().Format(timeLayout)
	res, err := tx.ExecContext(ctx, "INSERT INTO kb_documents(id, datapack_id, document_key, filename, document_type, "+
		"content_hash, size_bytes, status, uploaded_by, uploaded_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
		"ON CONFLICT(datapack_id, content_hash) DO NOTHING", uuid.NewString(), d.DatapackID, d.Key, d.Filename,
		d.DocumentType, d.ContentHash, d.SizeBytes, DocQueued, d.UploadedBy, now)
	if err != nil {
		return Document{}, 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Document{}, 0, err
	}
	what := AddedNew
	if n == 0 {
		what = AddedExisting
		res, err = tx.ExecContext(ctx, "UPDATE kb_documents SET status = ?, error = '', chunks_total = NULL, "+
			"completed_at = NULL WHERE datapack_id = ? AND content_hash = ? AND status = ?",
			DocQueued, d.DatapackID, d.ContentHash, DocFailed)
		if err != nil {
			return Document{}, 0, err
		}
		if n, err = res.RowsAffected(); err != nil {
			return Document{}, 0, err
		}
		if n == 1 {
			what = AddedRequeued
			if _, err = tx.ExecContext(ctx, "DELETE FROM kb_chunks WHERE document_rowid = "+
				"(SELECT rowid FROM kb_documents WHERE datapack_id = ? AND content_hash = ?)",
				d.DatapackID, d.ContentHash); err != nil {
				return Document{}, 0, err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO kb_bases(datapack_id, created_at) VALUES (?, ?)",
		d.DatapackID, now); err != nil {
		return Document{}, 0, err
	}
	out, err := scanDocument(tx.QueryRowContext(ctx, "SELECT "+docCols+" FROM kb_documents d "+
		"WHERE d.datapack_id = ? AND d.content_hash = ?", d.DatapackID, d.ContentHash))
	if err != nil {
		return Document{}, 0, err
	}
	return out, what, tx.Commit()
}

// documentWhere is one document of a datapack, or ErrNotFound.
func (s *Store) documentWhere(ctx context.Context, q querier, where string, args ...any) (Document, error) {
	d, err := scanDocument(q.QueryRowContext(ctx, "SELECT "+docCols+" FROM kb_documents d WHERE "+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	return d, err
}

// GetDocument is a datapack's file by its id, or ErrNotFound.
func (s *Store) GetDocument(ctx context.Context, datapack, id string) (Document, error) {
	return s.documentWhere(ctx, s.r, "d.datapack_id = ? AND d.id = ?", datapack, id)
}

// DocumentByKey is a datapack's file by its document key, or ErrNotFound.
func (s *Store) DocumentByKey(ctx context.Context, datapack, key string) (Document, error) {
	return s.documentWhere(ctx, s.r, "d.datapack_id = ? AND d.document_key = ?", datapack, key)
}

// DocumentByHash is a datapack's file by its content, or ErrNotFound.
func (s *Store) DocumentByHash(ctx context.Context, datapack, hash string) (Document, error) {
	return s.documentWhere(ctx, s.r, "d.datapack_id = ? AND d.content_hash = ?", datapack, hash)
}

// HasDocumentFile reports whether a datapack's file with this content has a row, or with hash ""
// whether the datapack exists, for sweeping the files folder.
func (s *Store) HasDocumentFile(ctx context.Context, datapack, hash string) (bool, error) {
	if hash == "" {
		return s.DatapackExists(ctx, datapack)
	}
	var one int
	err := s.r.QueryRowContext(ctx, "SELECT 1 FROM kb_documents WHERE datapack_id = ? AND content_hash = ?",
		datapack, hash).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ClaimDocument marks the oldest QUEUED file PROCESSING and returns it, or nil when none waits.
func (s *Store) ClaimDocument(ctx context.Context) (*Document, error) {
	var rowid int64
	err := s.w.QueryRowContext(ctx, "UPDATE kb_documents SET status = ? WHERE rowid = "+
		"(SELECT rowid FROM kb_documents WHERE status = ? ORDER BY rowid LIMIT 1) RETURNING rowid",
		DocProcessing, DocQueued).Scan(&rowid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, err := s.documentWhere(ctx, s.w, "d.rowid = ?", rowid)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// RequeueProcessing puts every PROCESSING file back in the queue and drops its chunks: a server
// stopped part way through them. serve calls it at start, before the indexer runs.
func (s *Store) RequeueProcessing(ctx context.Context) (int, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM kb_chunks WHERE document_rowid IN "+
		"(SELECT rowid FROM kb_documents WHERE status = ?)", DocProcessing); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, "UPDATE kb_documents SET status = ?, chunks_total = NULL WHERE status = ?",
		DocQueued, DocProcessing)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}

// AddChunks stores a PROCESSING file's chunks from index first on, unembedded. With last set they
// are its final ones: its chunk count is recorded, and it's COMPLETED once they're all embedded. A
// file that isn't PROCESSING any more (deleted, or its datapack) is ErrNotFound.
func (s *Store) AddChunks(ctx context.Context, d Document, first int, texts []string, last bool) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM kb_documents WHERE rowid = ? AND status = ?", d.Rowid,
		DocProcessing).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	for i, t := range texts {
		if _, err = tx.ExecContext(ctx, "INSERT INTO kb_chunks(id, datapack_id, document_rowid, chunk_index, text) "+
			"VALUES (?, ?, ?, ?, ?)", uuid.NewString(), d.DatapackID, d.Rowid, first+i, t); err != nil {
			return err
		}
	}
	if last {
		if _, err = tx.ExecContext(ctx, "UPDATE kb_documents SET chunks_total = ? WHERE rowid = ?",
			first+len(texts), d.Rowid); err != nil {
			return err
		}
		if err = completeDocuments(ctx, tx, []int64{d.Rowid}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// completeDocuments marks COMPLETED those of the files that are PROCESSING, chunked and fully
// embedded.
func completeDocuments(ctx context.Context, tx *sql.Tx, rowids []int64) error {
	now := time.Now().UTC().Format(timeLayout)
	for _, r := range rowids {
		if _, err := tx.ExecContext(ctx, "UPDATE kb_documents SET status = ?, completed_at = ? WHERE rowid = ? "+
			"AND status = ? AND chunks_total IS NOT NULL AND NOT EXISTS "+
			"(SELECT 1 FROM kb_chunks WHERE document_rowid = ? AND embedded = 0)",
			DocCompleted, now, r, DocProcessing, r); err != nil {
			return err
		}
	}
	return nil
}

// FailDocument marks a file FAILED, saying why (never with its text), and drops its chunks.
func (s *Store) FailDocument(ctx context.Context, rowid int64, reason string) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM kb_chunks WHERE document_rowid = ?", rowid); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE kb_documents SET status = ?, error = ?, chunks_total = NULL, "+
		"completed_at = ? WHERE rowid = ?", DocFailed, reason, time.Now().UTC().Format(timeLayout), rowid); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteDocument deletes a datapack's file with its chunks (and their vectors and terms), and says
// how many chunks went, or returns ErrNotFound, or ErrDocumentProcessing while it's queued or
// being indexed. The caller removes its bytes.
func (s *Store) DeleteDocument(ctx context.Context, datapack, id string) (Document, int, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return Document{}, 0, err
	}
	defer tx.Rollback()
	d, err := s.documentWhere(ctx, tx, "d.datapack_id = ? AND d.id = ?", datapack, id)
	if err != nil {
		return Document{}, 0, err
	}
	if d.Status == DocQueued || d.Status == DocProcessing {
		return d, 0, ErrDocumentProcessing
	}
	var n int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM kb_chunks WHERE document_rowid = ?", d.Rowid).
		Scan(&n); err != nil {
		return Document{}, 0, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM kb_documents WHERE rowid = ?", d.Rowid); err != nil {
		return Document{}, 0, err
	}
	return d, n, tx.Commit()
}

// DocumentPage asks for a page of a datapack's files, newest first.
type DocumentPage struct {
	DatapackID string
	Query      string // a case-insensitive part of the filename or key; "" for all
	TypeSuffix string // a case-insensitive end of the filename, such as ".md"; "" for all
	Status     string // one status; "" for all
	Limit      int
	Offset     int
}

// ListDocuments is a page of a datapack's files and how many match in all.
func (s *Store) ListDocuments(ctx context.Context, p DocumentPage) ([]Document, int, error) {
	where := " FROM kb_documents d WHERE d.datapack_id = ? " +
		"AND (? = '' OR instr(lower(d.document_key), lower(?)) > 0) " +
		"AND (? = '' OR lower(d.filename) LIKE '%' || lower(?) ESCAPE '\\') AND (? = '' OR d.status = ?)"
	suffix := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(p.TypeSuffix)
	args := []any{p.DatapackID, p.Query, p.Query, suffix, suffix, p.Status, p.Status}
	var total int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*)"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	// G202: fixed names, and every value is bound.
	q := "SELECT " + docCols + where + " ORDER BY d.uploaded_at DESC, d.rowid DESC LIMIT ? OFFSET ?" //nolint:gosec
	rows, err := s.r.QueryContext(ctx, q, append(args, min(max(p.Limit, 1), maxListLimit), max(p.Offset, 0))...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	return out, total, rows.Err()
}

// MarkKnowledgeBase records that a datapack has a knowledge base.
func (s *Store) MarkKnowledgeBase(ctx context.Context, datapack string) error {
	_, err := s.w.ExecContext(ctx, "INSERT OR IGNORE INTO kb_bases(datapack_id, created_at) VALUES (?, ?)",
		datapack, time.Now().UTC().Format(timeLayout))
	if isForeignKeyViolation(err) {
		return ErrNoDatapack
	}
	return err
}

// KBStats sums up a datapack's knowledge base.
type KBStats struct {
	HasBase        bool  // it has a knowledge base (Cloud's knowledge_base_file_count)
	Files          int   // its files, any status
	IndexedFiles   int   // its files COMPLETED, PROCESSING or QUEUED, as Cloud's summary counts them
	InFlight       int   // its files QUEUED or PROCESSING
	Chunks         int   // every chunk: files' and promoted memories'
	FileChunks     int   // its files' chunks
	Bytes          int64 // its files' sizes
	LastUploadAt   *time.Time
	Heaviest       string // the filename with the most chunks
	HeaviestChunks int
}

// KnowledgeStats sums up a datapack's knowledge base.
func (s *Store) KnowledgeStats(ctx context.Context, datapack string) (KBStats, error) {
	var st KBStats
	var last sql.NullString
	err := s.r.QueryRowContext(ctx, "SELECT "+
		"EXISTS (SELECT 1 FROM kb_bases WHERE datapack_id = ?), "+
		"(SELECT count(*) FROM kb_documents WHERE datapack_id = ?), "+
		"(SELECT count(*) FROM kb_documents WHERE datapack_id = ? AND status IN (?, ?, ?)), "+
		"(SELECT count(*) FROM kb_documents WHERE datapack_id = ? AND status IN (?, ?)), "+
		"(SELECT count(*) FROM kb_chunks WHERE datapack_id = ?), "+
		"(SELECT count(*) FROM kb_chunks WHERE datapack_id = ? AND document_rowid IS NOT NULL), "+
		"(SELECT COALESCE(sum(size_bytes), 0) FROM kb_documents WHERE datapack_id = ?), "+
		"(SELECT max(uploaded_at) FROM kb_documents WHERE datapack_id = ?)",
		datapack, datapack, datapack, DocCompleted, DocProcessing, DocQueued, datapack, DocQueued, DocProcessing,
		datapack, datapack, datapack, datapack).Scan(&st.HasBase, &st.Files, &st.IndexedFiles, &st.InFlight,
		&st.Chunks, &st.FileChunks, &st.Bytes, &last)
	if err != nil {
		return KBStats{}, err
	}
	if last.Valid {
		t, perr := time.Parse(timeLayout, last.String)
		if perr != nil {
			return KBStats{}, perr
		}
		st.LastUploadAt = &t
	}
	err = s.r.QueryRowContext(ctx, "SELECT d.filename, count(*) AS n FROM kb_chunks c "+
		"JOIN kb_documents d ON d.rowid = c.document_rowid WHERE d.datapack_id = ? "+
		"GROUP BY d.rowid ORDER BY n DESC, d.rowid LIMIT 1", datapack).Scan(&st.Heaviest, &st.HeaviestChunks)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return KBStats{}, err
	}
	return st, nil
}

// KnowledgeCounts is a datapack's knowledge-base chunks (files' and promoted memories') and
// whether it has a knowledge base: what describe and context_search's savings need, cheaply.
func (s *Store) KnowledgeCounts(ctx context.Context, datapack string) (chunks int, hasBase bool, err error) {
	err = s.r.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM kb_chunks WHERE datapack_id = ?), "+
		"EXISTS (SELECT 1 FROM kb_bases WHERE datapack_id = ?)", datapack, datapack).Scan(&chunks, &hasBase)
	return chunks, hasBase, err
}

// KBChunk is a knowledge-base chunk waiting for its vector.
type KBChunk struct {
	Rowid int64
	Text  string
}

// PendingKBChunks is up to limit chunks that have no vector yet, oldest first.
func (s *Store) PendingKBChunks(ctx context.Context, limit int) ([]KBChunk, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT rowid, text FROM kb_chunks WHERE embedded = 0 ORDER BY rowid LIMIT ?",
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KBChunk{}
	for rows.Next() {
		var c KBChunk
		if err := rows.Scan(&c.Rowid, &c.Text); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutKBChunkVectors stores chunks' vectors and their terms, making them searchable, and completes
// the files whose last chunk this was. A chunk deleted meanwhile is skipped. A promoted memory's
// chunk records when it reached the knowledge base.
func (s *Store) PutKBChunkVectors(ctx context.Context, rowids []int64, vectors [][]float32) error {
	dims, err := s.vectorDims(ctx)
	if err != nil {
		return err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(timeLayout)
	docs := map[int64]bool{}
	for i, id := range rowids {
		var blob []byte
		if blob, err = vectorBlob(vectors[i], dims); err != nil {
			return err
		}
		var dp, text string
		var doc, promoted sql.NullInt64
		err = tx.QueryRowContext(ctx, "SELECT datapack_id, text, document_rowid, promoted_rowid FROM kb_chunks "+
			"WHERE rowid = ? AND embedded = 0", id).Scan(&dp, &text, &doc, &promoted)
		if errors.Is(err, sql.ErrNoRows) { // deleted meanwhile
			continue
		}
		if err != nil {
			return err
		}
		if err = searchable(ctx, tx, id, dp, text, doc.Valid, blob); err != nil {
			return err
		}
		if doc.Valid {
			docs[doc.Int64] = true
		} else if _, err = tx.ExecContext(ctx, "UPDATE promoted_memories SET promoted_to_kb_at = ? "+
			"WHERE rowid = ? AND promoted_to_kb_at IS NULL", now, promoted.Int64); err != nil {
			return err
		}
	}
	list := make([]int64, 0, len(docs))
	for d := range docs {
		list = append(list, d)
	}
	if err = completeDocuments(ctx, tx, list); err != nil {
		return err
	}
	return tx.Commit()
}

// searchable stores a chunk's vector and its terms and marks it embedded.
func searchable(ctx context.Context, tx *sql.Tx, rowid int64, datapack, text string, file bool, blob []byte) error {
	source := "memory"
	if file {
		source = "file"
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO kb_chunks_vec(rowid, datapack_id, source, embedding) "+
		"VALUES (?, ?, ?, ?)", rowid, datapack, source, blob); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO kb_chunks_fts(rowid, datapack_id, text) VALUES (?, ?, ?)",
		rowid, datapack, text); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE kb_chunks SET embedded = 1 WHERE rowid = ?", rowid)
	return err
}

// addPromotedChunk gives a just-promoted memory its knowledge-base chunk, copying the memory's
// vector when it has one (the engine deletes the memory, vector and all, once the promotion
// commits). Without one, the chunk waits for the embedder sweep.
func (s *Store) addPromotedChunk(ctx context.Context, tx *sql.Tx, p Promoted, promotedRowid int64) error {
	meta, err := json.Marshal(map[string]string{"source": "memory", "agent_id": p.AgentID, "memory_id": p.ID})
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "INSERT INTO kb_chunks(id, datapack_id, promoted_rowid, text, metadata) "+
		"VALUES (?, ?, ?, ?, ?)", uuid.NewString(), p.DatapackID, promotedRowid, p.Text, string(meta))
	if err != nil {
		return err
	}
	chunk, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err = s.vectorDims(ctx); errors.Is(err, ErrNoVectors) {
		return nil
	} else if err != nil {
		return err
	}
	var blob []byte
	err = tx.QueryRowContext(ctx, "SELECT v.embedding FROM memories m JOIN memories_vec v ON v.rowid = m.rowid "+
		"WHERE m.datapack_id = ? AND m.id = ?", p.DatapackID, p.ID).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = searchable(ctx, tx, chunk, p.DatapackID, p.Text, false, blob); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE promoted_memories SET promoted_to_kb_at = promoted_at WHERE rowid = ?",
		promotedRowid)
	return err
}

// KBHit is a knowledge-base chunk a search found.
type KBHit struct {
	Rowid      int64
	ID         string
	Text       string
	DocumentID string // the file's id, or a promoted memory's conversation id, as Cloud gives it
	Metadata   map[string]any
	Distance   float64  // cosine distance from the query
	Rank       *float64 // a keyword hit's BM25 score (positive, higher is better); nil for a vector hit
}

const kbHitCols = "c.rowid, c.id, c.text, COALESCE(d.id, p.conversation_id, ''), c.metadata"

const kbHitJoins = " LEFT JOIN kb_documents d ON d.rowid = c.document_rowid " +
	"LEFT JOIN promoted_memories p ON p.rowid = c.promoted_rowid"

func scanKBHit(rows *sql.Rows, extra ...any) (KBHit, error) {
	var h KBHit
	var meta string
	if err := rows.Scan(append([]any{&h.Rowid, &h.ID, &h.Text, &h.DocumentID, &meta}, extra...)...); err != nil {
		return KBHit{}, err
	}
	if err := json.Unmarshal([]byte(meta), &h.Metadata); err != nil {
		return KBHit{}, err
	}
	return h, nil
}

// SearchKBVectors is the datapack's k chunks nearest to query, nearest first.
func (s *Store) SearchKBVectors(ctx context.Context, datapack string, query []float32, k int) ([]KBHit, error) {
	release, err := s.searchSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	dims, err := s.vectorDims(ctx)
	if err != nil {
		return nil, err
	}
	blob, err := vectorBlob(query, dims)
	if err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx, "WITH knn AS (SELECT rowid, distance FROM kb_chunks_vec "+
		"WHERE embedding MATCH ? AND k = ? AND datapack_id = ?) SELECT "+kbHitCols+", knn.distance FROM knn "+
		"JOIN kb_chunks c ON c.rowid = knn.rowid"+kbHitJoins+" ORDER BY knn.distance",
		blob, min(max(k, 1), maxKNN), datapack)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KBHit{}
	for rows.Next() {
		var dist float64
		h, err := scanKBHit(rows, &dist)
		if err != nil {
			return nil, err
		}
		h.Distance = dist
		out = append(out, h)
	}
	return out, rows.Err()
}

// SearchKBKeywords is the datapack's k chunks best matching any of query's words, best first by
// BM25 (as rank), each with its cosine distance from queryVector. Stop words are left out, and so
// are words in half or more of all chunks, which BM25 weighs at nothing (as memory search drops
// them).
func (s *Store) SearchKBKeywords(ctx context.Context, datapack, query string, queryVector []float32, k int) (
	[]KBHit, error) {
	release, err := s.searchSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	dims, err := s.vectorDims(ctx)
	if err != nil {
		return nil, err
	}
	blob, err := vectorBlob(queryVector, dims)
	if err != nil {
		return nil, err
	}
	terms, err := s.weighableKBTerms(ctx, kbTerms(query))
	if err != nil || len(terms) == 0 {
		return []KBHit{}, err
	}
	match := kbFTSQuery(datapack, terms)
	// The datapack column's weight is 0: matching it says nothing about relevance.
	rows, err := s.r.QueryContext(ctx, "WITH hits AS (SELECT rowid, -bm25(kb_chunks_fts, 0.0, 1.0) AS rank "+
		"FROM kb_chunks_fts WHERE kb_chunks_fts MATCH ? ORDER BY rank DESC LIMIT ?) "+
		"SELECT "+kbHitCols+", hits.rank, "+
		"vec_distance_cosine((SELECT v.embedding FROM kb_chunks_vec v WHERE v.rowid = hits.rowid), ?) FROM hits "+
		"JOIN kb_chunks c ON c.rowid = hits.rowid"+kbHitJoins+
		" ORDER BY hits.rank DESC", match, min(max(k, 1), maxListLimit), blob)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KBHit{}
	for rows.Next() {
		var rank, dist float64
		h, err := scanKBHit(rows, &rank, &dist)
		if err != nil {
			return nil, err
		}
		h.Rank, h.Distance = &rank, dist
		out = append(out, h)
	}
	return out, rows.Err()
}

// stopWords are the English words a keyword query leaves out, standing in for Cloud's choice of
// nouns and verbs (spaCy, which Meko doesn't have), and the verbs Cloud drops too.
var stopWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a about above after again against all am an and any are aren't as at be
because been before being below between both but by can can't cannot could couldn't did didn't do does
doesn't doing don't down during each few for from further had hadn't has hasn't have haven't having he
her here hers herself him himself his how i if in into is isn't it it's its itself let's me more most
mustn't my myself no nor not of off on once only or other ought our ours ourselves out over own same
shan't she should shouldn't so some such than that that's the their theirs them themselves then there
there's these they this those through to too under until up very was wasn't we were weren't what what's
when when's where where's which while who who's whom why why's with won't would wouldn't you your yours
yourself yourselves get got make made go goes went come came also just like will shall may might must
does done`) {
		m[w] = true
	}
	return m
}()

// kbTerms is a query's distinct words, lowercased, without stop words, at most maxTerms, cut where
// FTS5's tokenizer cuts (anything but letters and digits).
func kbTerms(query string) []string {
	seen := map[string]bool{}
	var terms []string
	for _, w := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	}) {
		w = strings.Trim(w, "'")
		if w == "" || stopWords[w] || seen[w] || len(terms) >= maxTerms {
			continue
		}
		seen[w] = true
		terms = append(terms, w)
	}
	return terms
}

// minWeighableRows is how many chunks the knowledge base must hold before keyword search drops
// common terms. Below it, matching every chunk costs nothing, and in a small knowledge base a
// file's every chunk can share a word the query needs (one chunk holds all its words).
const minWeighableRows = 1000

// weighableKBTerms drops the terms in half or more of all embedded chunks: FTS5's bm25() weighs
// them at nothing, and matching them would make nearly every chunk a candidate. A term's rows are
// counted with MATCH, so it's stemmed as the index is.
func (s *Store) weighableKBTerms(ctx context.Context, terms []string) ([]string, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	var rows int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM kb_chunks WHERE embedded = 1").Scan(&rows); err != nil {
		return nil, err
	}
	if rows < minWeighableRows {
		return terms, nil
	}
	keep := terms[:0:0]
	for _, t := range terms {
		var docs int
		if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM kb_chunks_fts WHERE kb_chunks_fts MATCH ?",
			"text:"+ftsQuote(t)).Scan(&docs); err != nil {
			return nil, err
		}
		if docs*2 < rows {
			keep = append(keep, t)
		}
	}
	return keep, nil
}

func ftsQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// kbFTSQuery is an FTS5 query for any of the terms within the datapack, each term quoted.
func kbFTSQuery(datapack string, terms []string) string {
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = ftsQuote(t)
	}
	return "datapack_id:" + ftsQuote(datapack) + " AND text:(" + strings.Join(quoted, " OR ") + ")"
}

// Activity is one event of a datapack's knowledge base: a file uploaded or a memory promoted.
type Activity struct {
	Type      string // "file_upload" or "memory_promotion"
	Timestamp time.Time
	ID        string // the file's or the memory's id
	// A file upload's.
	DocumentKey     string
	DocumentType    string
	ChunksProcessed int
	SizeBytes       int64
	// A promotion's.
	Content        string
	AgentID        string
	ConversationID string
}

// ActivityCursor is where a page of activity starts: after the event at this time with this id.
type ActivityCursor struct {
	Timestamp string `json:"ts"`
	ID        string `json:"id"`
}

// KBActivity is a page of the datapack's file uploads and promotions, newest first, starting after
// the cursor (nil for the newest), and the cursor of its last event.
func (s *Store) KBActivity(ctx context.Context, datapack string, after *ActivityCursor, limit int) (
	[]Activity, *ActivityCursor, error) {
	ts, id := "~", "" // "~" sorts after every time
	if after != nil {
		ts, id = after.Timestamp, after.ID
	}
	rows, err := s.r.QueryContext(ctx, "SELECT * FROM ("+
		"SELECT 'file_upload' AS type, d.uploaded_at AS ts, d.id AS id, d.document_key, d.document_type, "+
		"(SELECT count(*) FROM kb_chunks c WHERE c.document_rowid = d.rowid AND c.embedded = 1), d.size_bytes, "+
		"'', '', '' FROM kb_documents d WHERE d.datapack_id = ? "+
		"UNION ALL SELECT 'memory_promotion', p.promoted_at, p.id, '', '', 0, 0, p.text, p.agent_id, "+
		"p.conversation_id FROM promoted_memories p WHERE p.datapack_id = ?) "+
		"WHERE ts < ? OR (ts = ? AND id < ?) ORDER BY ts DESC, id DESC LIMIT ?",
		datapack, datapack, ts, ts, id, min(max(limit, 1), maxListLimit))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []Activity{}
	var last *ActivityCursor
	for rows.Next() {
		var a Activity
		var at string
		if err = rows.Scan(&a.Type, &at, &a.ID, &a.DocumentKey, &a.DocumentType, &a.ChunksProcessed, &a.SizeBytes,
			&a.Content, &a.AgentID, &a.ConversationID); err != nil {
			return nil, nil, err
		}
		if a.Timestamp, err = time.Parse(timeLayout, at); err != nil {
			return nil, nil, err
		}
		out = append(out, a)
		last = &ActivityCursor{Timestamp: at, ID: a.ID}
	}
	return out, last, rows.Err()
}

func isForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
