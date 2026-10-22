package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Settings keys for the embedding model, recorded at the memory engine's first hello.
const (
	keyEmbedderProvider = "embedder_provider"
	keyEmbedderModel    = "embedder_model"
	keyEmbeddingDims    = "embedding_dims"
)

// Embedder names the embedding model the memory engine runs. Its dimension is the vector tables' width.
type Embedder struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Dims     int    `json:"dims"`
}

// ErrNoVectors means the vector tables don't exist yet: the memory engine hasn't reported its model.
var ErrNoVectors = errors.New("the memory engine hasn't reported its embedding model yet")

// ModelChangedError refuses a memory engine whose model isn't the one the vectors were made with.
type ModelChangedError struct{ Have, Got Embedder }

func (e *ModelChangedError) Error() string {
	return fmt.Sprintf("the stored vectors were made with %s %s (%d dims), but the memory engine runs %s %s (%d dims). "+
		"Changing the embedding model needs re-embedding, which Meko can't do yet",
		e.Have.Provider, e.Have.Model, e.Have.Dims, e.Got.Provider, e.Got.Model, e.Got.Dims)
}

// vecTable is a vec0 table holding the vectors of one table's rows, keyed by its rowid. The
// metadata columns are the ones a KNN query can filter on inside the scan.
type vecTable struct {
	name, owner, metadata string
}

// vecTables are made together at the first hello. A later milestone adding one here gets it made
// at the next start (CREATE ... IF NOT EXISTS).
var vecTables = []vecTable{
	{"memories_vec", "memories", "user_id TEXT, agent_id TEXT, conversation_id TEXT"},
	{"entities_vec", "entities", "user_id TEXT, agent_id TEXT, conversation_id TEXT"},
	{"message_chunks_vec", "message_chunks", "conversation_id TEXT"},
	{"kb_chunks_vec", "kb_chunks", "source TEXT"}, // milestone 4: made at the next start on older databases
}

// EnsureVectors records the memory engine's embedding model the first time and makes the vector tables
// at its width. Later it checks the memory engine still runs the same model: a different one gets a
// *ModelChangedError, since the stored vectors can't be compared with its.
func (s *Store) EnsureVectors(ctx context.Context, e Embedder) error {
	if e.Dims <= 0 || e.Model == "" {
		return fmt.Errorf("embedding model %q with %d dims isn't valid", e.Model, e.Dims)
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	have, found, err := embedderIn(ctx, tx)
	if err != nil {
		return err
	}
	if found && have != e {
		return &ModelChangedError{Have: have, Got: e}
	}
	if !found {
		for k, v := range map[string]string{
			keyEmbedderProvider: e.Provider, keyEmbedderModel: e.Model, keyEmbeddingDims: strconv.Itoa(e.Dims),
		} {
			if _, err := tx.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES (?, ?)", k, v); err != nil {
				return err
			}
		}
	}
	for _, t := range vecTables {
		stmts := []string{
			fmt.Sprintf("CREATE VIRTUAL TABLE IF NOT EXISTS %s USING vec0(datapack_id TEXT partition key, %s, "+
				"embedding float[%d] distance_metric=cosine)", t.name, t.metadata, e.Dims),
			// Every delete of the owner's rows (a datapack's cascade included) takes its vectors along:
			// vec0 tables can't have foreign keys.
			fmt.Sprintf("CREATE TRIGGER IF NOT EXISTS %s_ad AFTER DELETE ON %s BEGIN "+
				"DELETE FROM %s WHERE rowid = old.rowid; END", t.name, t.owner, t.name),
		}
		for _, q := range stmts {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.dims.Store(int64(e.Dims))
	return nil
}

// Embedder is the recorded embedding model; ok is false before the memory engine's first hello.
func (s *Store) Embedder(ctx context.Context) (e Embedder, ok bool, err error) {
	return embedderIn(ctx, s.r)
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func embedderIn(ctx context.Context, q querier) (Embedder, bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT key, value FROM settings WHERE key IN (?, ?, ?)",
		keyEmbedderProvider, keyEmbedderModel, keyEmbeddingDims)
	if err != nil {
		return Embedder{}, false, err
	}
	defer rows.Close()
	var e Embedder
	n := 0
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			return Embedder{}, false, err
		}
		n++
		switch k {
		case keyEmbedderProvider:
			e.Provider = v
		case keyEmbedderModel:
			e.Model = v
		case keyEmbeddingDims:
			if e.Dims, err = strconv.Atoi(v); err != nil {
				return Embedder{}, false, fmt.Errorf("setting %s is %q, not a number", k, v)
			}
		}
	}
	return e, n == 3, rows.Err()
}

// vectorDims is the vector tables' width, or ErrNoVectors before they exist. It never changes once
// set, so it's cached.
func (s *Store) vectorDims(ctx context.Context) (int, error) {
	if d := s.dims.Load(); d > 0 {
		return int(d), nil
	}
	e, ok, err := s.Embedder(ctx)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrNoVectors
	}
	var n int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name = ?", vecTables[0].name).
		Scan(&n); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrNoVectors
	}
	s.dims.Store(int64(e.Dims))
	return e.Dims, nil
}

// vectorBlob encodes v as sqlite-vec's float32 blob, checking its width.
func vectorBlob(v []float32, dims int) ([]byte, error) {
	if len(v) != dims {
		return nil, fmt.Errorf("%w: it has %d dimensions, but the embedding model has %d", ErrBadVector, len(v), dims)
	}
	b := make([]byte, 4*len(v))
	for i, f := range v {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return nil, fmt.Errorf("%w: it holds NaN or infinity", ErrBadVector)
		}
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b, nil
}
