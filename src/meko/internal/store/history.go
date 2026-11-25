package store

import (
	"context"
)

// HistoryEntry is one change to a memory, as mem0's HistoryManager records it.
type HistoryEntry struct {
	MemoryID      string  `json:"memory_id"`
	PreviousValue *string `json:"previous_value"`
	NewValue      *string `json:"new_value"`
	Action        string  `json:"action"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     *string `json:"updated_at"`
	IsDeleted     bool    `json:"is_deleted"`
}

// AddHistory records changes to the datapack's memories.
func (s *Store) AddHistory(ctx context.Context, datapack string, entries []HistoryEntry) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := datapackExists(ctx, tx, datapack); err != nil {
		return err
	}
	for _, e := range entries {
		if _, err := tx.ExecContext(ctx, "INSERT INTO memory_history(datapack_id, memory_id, previous_value, "+
			"new_value, action, is_deleted, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			datapack, e.MemoryID, e.PreviousValue, e.NewValue, e.Action, e.IsDeleted, e.CreatedAt, e.UpdatedAt,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// History is a memory's changes, oldest first.
func (s *Store) History(ctx context.Context, datapack, memoryID string) ([]HistoryEntry, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT memory_id, previous_value, new_value, action, is_deleted, "+
		"created_at, updated_at FROM memory_history WHERE datapack_id = ? AND memory_id = ? ORDER BY id",
		datapack, memoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryEntry{}
	for rows.Next() {
		var e HistoryEntry
		if err := rows.Scan(&e.MemoryID, &e.PreviousValue, &e.NewValue, &e.Action, &e.IsDeleted, &e.CreatedAt,
			&e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
