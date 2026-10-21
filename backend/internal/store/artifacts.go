package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Artifact is a file an agent kept, without its bytes unless asked for them.
type Artifact struct {
	ID             string
	DatapackID     string
	ContentHash    string // SHA-256, 64 lowercase hex
	Filename       string
	ContentType    string
	SizeBytes      int
	AgentID        string
	CreatedAt      time.Time
	LastAccessedAt *time.Time
	Bytes          []byte
}

const artifactCols = "id, datapack_id, content_hash, filename, content_type, size_bytes, agent_id, created_at, " +
	"last_accessed_at"

func scanArtifact(row interface{ Scan(...any) error }, extra ...any) (Artifact, error) {
	var a Artifact
	var created string
	var accessed sql.NullString
	dest := append([]any{&a.ID, &a.DatapackID, &a.ContentHash, &a.Filename, &a.ContentType, &a.SizeBytes, &a.AgentID,
		&created, &accessed}, extra...)
	if err := row.Scan(dest...); err != nil {
		return Artifact{}, err
	}
	var err error
	if a.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
		return Artifact{}, err
	}
	if accessed.Valid {
		t, err := time.Parse(timeLayout, accessed.String)
		if err != nil {
			return Artifact{}, err
		}
		a.LastAccessedAt = &t
	}
	return a, nil
}

// PutArtifact keeps a file in a datapack, once per content: the same bytes again return the row
// already there (created false).
func (s *Store) PutArtifact(ctx context.Context, datapack, agentID, filename, contentType string, b []byte) (
	a Artifact, created bool, err error) {
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return Artifact{}, false, err
	}
	defer tx.Rollback()
	if err = datapackExists(ctx, tx, datapack); err != nil {
		return Artifact{}, false, err
	}
	res, err := tx.ExecContext(ctx, "INSERT INTO artifacts(id, datapack_id, content_hash, filename, content_type, "+
		"size_bytes, bytes, agent_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) "+
		"ON CONFLICT(datapack_id, content_hash) DO NOTHING",
		uuid.NewString(), datapack, hash, filename, contentType, len(b), b, agentID, time.Now().UTC().Format(timeLayout))
	if err != nil {
		return Artifact{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Artifact{}, false, err
	}
	a, err = scanArtifact(tx.QueryRowContext(ctx, "SELECT "+artifactCols+" FROM artifacts "+
		"WHERE datapack_id = ? AND content_hash = ?", datapack, hash))
	if err != nil {
		return Artifact{}, false, err
	}
	return a, n == 1, tx.Commit()
}

// GetArtifact is a file of the datapack with its bytes, by its hash, or ErrNotFound. Reading it
// records when it was last read.
func (s *Store) GetArtifact(ctx context.Context, datapack, hash string) (Artifact, error) {
	now := time.Now().UTC().Format(timeLayout)
	var b []byte
	a, err := scanArtifact(s.w.QueryRowContext(ctx, "UPDATE artifacts SET last_accessed_at = ? "+
		"WHERE datapack_id = ? AND content_hash = ? RETURNING "+artifactCols+", bytes", now, datapack, hash), &b)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	a.Bytes = b
	return a, err
}

// ListArtifacts is a page of the datapack's files (without their bytes), newest first, optionally
// only those whose filename contains query (any case), and how many match in all.
func (s *Store) ListArtifacts(ctx context.Context, datapack, query string, limit, offset int) ([]Artifact, int, error) {
	where := " FROM artifacts WHERE datapack_id = ? AND (? = '' OR instr(lower(filename), lower(?)) > 0)"
	args := []any{datapack, query, query}
	var total int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*)"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	// G202: fixed names, and every value is bound.
	q := "SELECT " + artifactCols + where + " ORDER BY created_at DESC, rowid DESC LIMIT ? OFFSET ?" //nolint:gosec
	rows, err := s.r.QueryContext(ctx, q, append(args, min(max(limit, 1), maxListLimit), max(offset, 0))...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}
