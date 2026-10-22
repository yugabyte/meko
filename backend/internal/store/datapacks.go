package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/mattn/go-sqlite3"
)

// ErrDuplicateName is a datapack name another datapack already has.
var ErrDuplicateName = errors.New("a datapack with that name already exists")

const datapackCols = "id, name, description, created_at, updated_at, conversation_search_opt_out"

func scanDatapack(row interface{ Scan(...any) error }) (Datapack, error) {
	var d Datapack
	var created, updated string
	if err := row.Scan(&d.ID, &d.Name, &d.Description, &created, &updated, &d.ConversationSearchOptOut); err != nil {
		return Datapack{}, err
	}
	var err error
	if d.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
		return Datapack{}, err
	}
	if d.UpdatedAt, err = time.Parse(timeLayout, updated); err != nil {
		return Datapack{}, err
	}
	return d, nil
}

// GetDatapack is one datapack, or ErrNoDatapack.
func (s *Store) GetDatapack(ctx context.Context, id string) (Datapack, error) {
	d, err := scanDatapack(s.r.QueryRowContext(ctx, "SELECT "+datapackCols+" FROM datapacks WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Datapack{}, ErrNoDatapack
	}
	return d, err
}

// DefaultDatapackID is the default datapack's id, or "" once the user has deleted it (it isn't
// made again).
func (s *Store) DefaultDatapackID(ctx context.Context) (string, error) {
	var id string
	err := s.r.QueryRowContext(ctx, "SELECT d.id FROM settings s JOIN datapacks d ON d.id = s.value WHERE s.key = ?",
		keyDefaultDatapack).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// CreateDatapack adds a datapack with a new UUID, or returns ErrDuplicateName.
func (s *Store) CreateDatapack(ctx context.Context, name, description string) (Datapack, error) {
	id := uuid.NewString()
	now := time.Now().UTC().Format(timeLayout)
	_, err := s.w.ExecContext(ctx,
		"INSERT INTO datapacks(id, name, description, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		id, name, description, now, now)
	if isUniqueViolation(err) {
		return Datapack{}, ErrDuplicateName
	}
	if err != nil {
		return Datapack{}, err
	}
	return s.getDatapackW(ctx, id)
}

// UpdateDatapack sets a datapack's name or description (nil leaves it), or returns ErrNoDatapack
// or ErrDuplicateName.
func (s *Store) UpdateDatapack(ctx context.Context, id string, name, description *string) (Datapack, error) {
	now := time.Now().UTC().Format(timeLayout)
	res, err := s.w.ExecContext(ctx, "UPDATE datapacks SET name = COALESCE(?, name), "+
		"description = COALESCE(?, description), updated_at = ? WHERE id = ?", name, description, now, id)
	if isUniqueViolation(err) {
		return Datapack{}, ErrDuplicateName
	}
	if err != nil {
		return Datapack{}, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err == nil {
			err = ErrNoDatapack
		}
		return Datapack{}, err
	}
	return s.getDatapackW(ctx, id)
}

// DeleteDatapack deletes a datapack and everything in it (memories, entities, history,
// conversations, and their vectors), or returns ErrNoDatapack.
func (s *Store) DeleteDatapack(ctx context.Context, id string) error {
	res, err := s.w.ExecContext(ctx, "DELETE FROM datapacks WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err == nil {
			err = ErrNoDatapack
		}
		return err
	}
	return nil
}

// getDatapackW reads a datapack on the writer, so it sees a write just made.
func (s *Store) getDatapackW(ctx context.Context, id string) (Datapack, error) {
	return scanDatapack(s.w.QueryRowContext(ctx, "SELECT "+datapackCols+" FROM datapacks WHERE id = ?", id))
}

// DatapackCounts are what a datapack holds, for describe.
type DatapackCounts struct {
	Memories      int // the user's memories: learnings and kept
	Learnings     int // the user's memories waiting for a decision
	Promoted      int // the datapack's collective memories
	Conversations int
}

// CountDatapack counts what a datapack holds of the user's.
func (s *Store) CountDatapack(ctx context.Context, id, userID string) (DatapackCounts, error) {
	var c DatapackCounts
	err := s.r.QueryRowContext(ctx, "SELECT "+
		"(SELECT count(*) FROM memories WHERE datapack_id = ? AND user_id = ?), "+
		"(SELECT count(*) FROM memories WHERE datapack_id = ? AND user_id = ? AND promotion_action = ''), "+
		"(SELECT count(*) FROM promoted_memories WHERE datapack_id = ?), "+
		"(SELECT count(*) FROM conversations WHERE datapack_id = ?)",
		id, userID, id, userID, id, id).Scan(&c.Memories, &c.Learnings, &c.Promoted, &c.Conversations)
	return c, err
}

func isUniqueViolation(err error) bool {
	var se sqlite3.Error
	return errors.As(err, &se) && se.ExtendedCode == sqlite3.ErrConstraintUnique
}
