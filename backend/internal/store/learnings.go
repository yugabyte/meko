package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// KeepAsMemory is the promotion action of a learning the user chose to keep as a memory.
const KeepAsMemory = "keep_as_memory"

// Memory list scopes, as Meko Cloud's: learnings are the memories waiting for a decision; memories
// are those and the ones kept.
const (
	ScopeMemories  = "memories"
	ScopeLearnings = "learnings"
)

// Promoted is a memory promoted to the datapack's collective memory.
type Promoted struct {
	ID             string
	DatapackID     string
	UserID         string
	AgentID        string
	ConversationID string
	ConnectorID    string
	Source         string
	Role           string
	ActorID        string
	Text           string
	Hash           string
	Payload        map[string]any
	CreatedAt      time.Time
	UpdatedAt      time.Time
	PromotedAt     time.Time
	PromotedBy     string
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }

// KeepMemories marks the user's learnings among ids as kept and says how many it marked.
func (s *Store) KeepMemories(ctx context.Context, datapack, userID string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := []any{KeepAsMemory, datapack, userID}
	for _, id := range ids {
		args = append(args, id)
	}
	// G202: only placeholders are added; every value is bound.
	q := "UPDATE memories SET promotion_action = ? WHERE datapack_id = ? AND user_id = ? " + //nolint:gosec
		"AND promotion_action = '' AND id IN (" + placeholders(len(ids)) + ")"
	res, err := s.w.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// CopyToPromoted copies one of the user's learnings into promoted_memories, keeping every column
// but the lemmatized text, or returns ErrNotFound (no such learning: missing, someone else's, or
// already kept). The memory itself stays until the caller deletes it through the engine. A learning
// already copied by a promotion that didn't get to delete it returns that copy, so promoting it
// again finishes the job.
func (s *Store) CopyToPromoted(ctx context.Context, datapack, userID, id, promotedBy string) (Promoted, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return Promoted{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(timeLayout)
	const learning = "FROM memories WHERE datapack_id = ? AND user_id = ? AND id = ? AND promotion_action = ''"
	res, err := tx.ExecContext(ctx, "INSERT INTO promoted_memories(id, datapack_id, user_id, agent_id, "+
		"conversation_id, connector_id, source, role, actor_id, text, hash, payload, created_at, updated_at, "+
		"promoted_at, promoted_by) SELECT id, datapack_id, user_id, agent_id, conversation_id, connector_id, "+
		"source, role, actor_id, text, hash, payload, created_at, updated_at, ?, ? "+learning+
		" ON CONFLICT(id) DO NOTHING", now, promotedBy, datapack, userID, id)
	if err != nil {
		return Promoted{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Promoted{}, err
	}
	if n == 0 { // no such learning, or one already copied (its copy has the same id)
		var one int
		err = tx.QueryRowContext(ctx, "SELECT 1 "+learning+
			" AND EXISTS (SELECT 1 FROM promoted_memories p WHERE p.id = memories.id AND p.datapack_id = ?)",
			datapack, userID, id, datapack).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return Promoted{}, ErrNotFound
		}
		if err != nil {
			return Promoted{}, err
		}
	}
	p, err := scanPromoted(tx.QueryRowContext(ctx, "SELECT "+promotedCols+" FROM promoted_memories WHERE id = ?", id))
	if err != nil {
		return Promoted{}, err
	}
	return p, tx.Commit()
}

const promotedCols = "id, datapack_id, user_id, agent_id, conversation_id, connector_id, source, role, actor_id, " +
	"text, hash, payload, created_at, updated_at, promoted_at, promoted_by"

func scanPromoted(row interface{ Scan(...any) error }) (Promoted, error) {
	var p Promoted
	var payload, created, updated, promoted string
	if err := row.Scan(&p.ID, &p.DatapackID, &p.UserID, &p.AgentID, &p.ConversationID, &p.ConnectorID, &p.Source,
		&p.Role, &p.ActorID, &p.Text, &p.Hash, &payload, &created, &updated, &promoted, &p.PromotedBy); err != nil {
		return Promoted{}, err
	}
	if err := json.Unmarshal([]byte(payload), &p.Payload); err != nil {
		return Promoted{}, err
	}
	var err error
	if p.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
		return Promoted{}, err
	}
	if p.UpdatedAt, err = time.Parse(timeLayout, updated); err != nil {
		return Promoted{}, err
	}
	if p.PromotedAt, err = time.Parse(timeLayout, promoted); err != nil {
		return Promoted{}, err
	}
	return p, nil
}

// DeletePromoted deletes a promoted memory, or returns ErrNotFound.
func (s *Store) DeletePromoted(ctx context.Context, datapack, id string) error {
	res, err := s.w.ExecContext(ctx, "DELETE FROM promoted_memories WHERE datapack_id = ? AND id = ?", datapack, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err == nil {
			err = ErrNotFound
		}
		return err
	}
	return nil
}

// IsPromoted reports whether id was promoted in the datapack.
func (s *Store) IsPromoted(ctx context.Context, datapack, id string) (bool, error) {
	var one int
	err := s.r.QueryRowContext(ctx, "SELECT 1 FROM promoted_memories WHERE datapack_id = ? AND id = ?",
		datapack, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// PromotedMemories is a page of the datapack's promoted memories, most recently promoted first,
// and how many there are in all.
func (s *Store) PromotedMemories(ctx context.Context, datapack string, limit, offset int) ([]Promoted, int, error) {
	var total int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM promoted_memories WHERE datapack_id = ?",
		datapack).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.r.QueryContext(ctx, "SELECT "+promotedCols+" FROM promoted_memories WHERE datapack_id = ? "+
		"ORDER BY promoted_at DESC, rowid DESC LIMIT ? OFFSET ?", datapack, min(max(limit, 1), maxListLimit),
		max(offset, 0))
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Promoted{}
	for rows.Next() {
		p, err := scanPromoted(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}
