package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"strings"
)

// The readers a datapack's export needs beyond the ones its pages use: the stored vectors, and the
// chunks they belong to. Each reads through s.r, so inside a Snapshot they see its moment.

// ExportChunk is one chunk of a conversation turn or a knowledge file, with its vector (nil until
// the embedder sweep has embedded it).
type ExportChunk struct {
	Index     int             `json:"index"`
	Text      string          `json:"text"`
	Metadata  json.RawMessage `json:"metadata,omitempty"` // a knowledge chunk's
	Embedding []float32       `json:"embedding"`
}

// MemoryEmbedding is a memory's vector, nil when it has none yet.
func (s *Store) MemoryEmbedding(ctx context.Context, datapack, id string) ([]float32, error) {
	var rowid int64
	err := s.r.QueryRowContext(ctx, "SELECT rowid FROM memories WHERE datapack_id = ? AND id = ?", datapack, id).
		Scan(&rowid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.vectorOf(ctx, "memories_vec", rowid)
}

// ArtifactBytes is an artifact's bytes, by its hash, or ErrNotFound. Unlike GetArtifact it only
// reads: it doesn't record the read, so it works inside a Snapshot.
func (s *Store) ArtifactBytes(ctx context.Context, datapack, hash string) ([]byte, error) {
	var b []byte
	err := s.r.QueryRowContext(ctx, "SELECT bytes FROM artifacts WHERE datapack_id = ? AND content_hash = ?",
		datapack, hash).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// MessageChunks is each turn's chunks in a conversation of a datapack, by message id, in order.
func (s *Store) MessageChunks(ctx context.Context, datapack, conversationID string) (map[string][]ExportChunk, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT m.id, mc.rowid, mc.chunk_index, mc.text FROM message_chunks mc "+
		"JOIN messages m ON m.rowid = mc.message_rowid JOIN conversations c ON c.rowid = m.conversation_rowid "+
		"WHERE c.id = ? AND "+datapackMatch+" ORDER BY m.rowid, mc.chunk_index", conversationID, datapack)
	if err != nil {
		return nil, err
	}
	type ref struct {
		message string
		rowid   int64
		chunk   ExportChunk
	}
	var refs []ref
	for rows.Next() {
		var r ref
		if err = rows.Scan(&r.message, &r.rowid, &r.chunk.Index, &r.chunk.Text); err != nil {
			_ = rows.Close()
			return nil, err
		}
		refs = append(refs, r)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := map[string][]ExportChunk{}
	for _, r := range refs {
		if r.chunk.Embedding, err = s.vectorOf(ctx, "message_chunks_vec", r.rowid); err != nil {
			return nil, err
		}
		out[r.message] = append(out[r.message], r.chunk)
	}
	return out, nil
}

// DocumentChunks is a knowledge file's chunks, in order. rowid is the document's (Document.Rowid).
func (s *Store) DocumentChunks(ctx context.Context, rowid int64) ([]ExportChunk, error) {
	return s.kbChunks(ctx, "document_rowid = ?", rowid)
}

// PromotedEmbedding is the vector of a collective memory's knowledge-base chunk, nil when it has
// none yet.
func (s *Store) PromotedEmbedding(ctx context.Context, datapack, id string) ([]float32, error) {
	cs, err := s.kbChunks(ctx, "promoted_rowid = (SELECT rowid FROM promoted_memories WHERE datapack_id = ? AND id = ?)",
		datapack, id)
	if err != nil || len(cs) == 0 {
		return nil, err
	}
	return cs[0].Embedding, nil
}

// kbChunks reads knowledge chunks matching where, one of this file's constant clauses.
func (s *Store) kbChunks(ctx context.Context, where string, args ...any) ([]ExportChunk, error) {
	//nolint:gosec // G202: where is a constant clause
	q := "SELECT rowid, chunk_index, text, metadata FROM kb_chunks WHERE " + where + " ORDER BY chunk_index, rowid"
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var rowids []int64
	var out []ExportChunk
	for rows.Next() {
		var c ExportChunk
		var rowid int64
		var meta string
		if err = rows.Scan(&rowid, &c.Index, &c.Text, &meta); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if meta != "" && meta != "{}" {
			c.Metadata = json.RawMessage(meta)
		}
		rowids = append(rowids, rowid)
		out = append(out, c)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i, r := range rowids {
		if out[i].Embedding, err = s.vectorOf(ctx, "kb_chunks_vec", r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// vectorOf is the vector table's row for an owner's rowid, nil when it has none (not embedded yet,
// or no vector table before the embedding model is known). table is one of vecTables' names.
func (s *Store) vectorOf(ctx context.Context, table string, rowid int64) ([]float32, error) {
	var blob []byte
	q := "SELECT embedding FROM " + table + " WHERE rowid = ?" //nolint:gosec // G202: table is a constant name
	err := s.r.QueryRowContext(ctx, q, rowid).Scan(&blob)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil && isNoSuchTable(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return decodeVector(blob), nil
}

// decodeVector is a vec0 embedding column's little-endian float32s.
func decodeVector(blob []byte) []float32 {
	v := make([]float32, len(blob)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[4*i:]))
	}
	return v
}

// isNoSuchTable says whether err is SQLite's for a table that doesn't exist, as a vector table is
// until the embedding model is known.
func isNoSuchTable(err error) bool { return strings.Contains(err.Error(), "no such table") }
