package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Profiles a datapack's memory extraction can use.
const (
	ExtractionGeneral = "general"
	ExtractionCoding  = "coding"
)

// ExtractionTurn is a queued turn with what extracting it needs.
type ExtractionTurn struct {
	Rowid             int64
	ConversationRowid int64
	MessageID         string
	ConversationID    string
	DatapackID        string
	AgentID           string
	Input             string
	Output            string
	CreatedAt         time.Time
	Attempts          int
}

// DatapackExtraction is a datapack's say in memory extraction.
type DatapackExtraction struct {
	OptOut       bool   `json:"opt_out"`
	Profile      string `json:"profile"`
	Instructions string `json:"instructions"`
}

// turnCols reads a turn; queuedCols adds its queue row's attempts.
const (
	turnCols = "m.rowid, m.conversation_rowid, m.id, c.id, COALESCE(c.datapack_id, ''), c.agent_id, " +
		"m.input, m.output, m.created_at"
	queuedCols = turnCols + ", q.attempts"
)

// QueuedConversations is the conversations with turns waiting, oldest waiting first, at most limit.
func (s *Store) QueuedConversations(ctx context.Context, limit int) ([]int64, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT m.conversation_rowid FROM extraction_queue q "+
		"JOIN messages m ON m.rowid = q.message_rowid GROUP BY m.conversation_rowid "+
		"ORDER BY min(q.message_rowid) LIMIT ?", max(limit, 1))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var c int64
		if err = rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// QueuedTurns is a conversation's waiting turns, oldest first, at most limit.
func (s *Store) QueuedTurns(ctx context.Context, conversationRowid int64, limit int) ([]ExtractionTurn, error) {
	return extractionTurns(ctx, s.r, queuedCols, "FROM extraction_queue q JOIN messages m ON m.rowid = q.message_rowid "+
		"JOIN conversations c ON c.rowid = m.conversation_rowid WHERE m.conversation_rowid = ? "+
		"ORDER BY q.message_rowid LIMIT ?", conversationRowid, max(limit, 1))
}

// TurnsBefore is up to limit of a conversation's turns just before a turn, oldest first: the
// context a batch is extracted with.
func (s *Store) TurnsBefore(ctx context.Context, conversationRowid, before int64, limit int) ([]ExtractionTurn,
	error) {
	turns, err := extractionTurns(ctx, s.r, turnCols, "FROM messages m "+
		"JOIN conversations c ON c.rowid = m.conversation_rowid "+
		"WHERE m.conversation_rowid = ? AND m.rowid < ? ORDER BY m.rowid DESC LIMIT ?",
		conversationRowid, before, max(limit, 1))
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return turns, err
}

func extractionTurns(ctx context.Context, q reader, cols, from string, args ...any) ([]ExtractionTurn, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+cols+" "+from, args...) //nolint:gosec // G202: constant clauses
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExtractionTurn
	for rows.Next() {
		var t ExtractionTurn
		var created string
		dest := []any{&t.Rowid, &t.ConversationRowid, &t.MessageID, &t.ConversationID, &t.DatapackID, &t.AgentID,
			&t.Input, &t.Output, &created}
		if cols == queuedCols {
			dest = append(dest, &t.Attempts)
		}
		if err = rows.Scan(dest...); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = time.Parse(timeLayout, created)
		out = append(out, t)
	}
	return out, rows.Err()
}

// DoneExtracting takes turns off the queue.
func (s *Store) DoneExtracting(ctx context.Context, rowids []int64) error {
	return s.eachQueued(ctx, "DELETE FROM extraction_queue WHERE message_rowid = ?", rowids)
}

// FailedExtracting records a failed run on turns that stay queued.
func (s *Store) FailedExtracting(ctx context.Context, rowids []int64, reason string) error {
	return s.eachQueued(ctx, "UPDATE extraction_queue SET attempts = attempts + 1, last_error = ? "+
		"WHERE message_rowid = ?", rowids, reason)
}

func (s *Store) eachQueued(ctx context.Context, q string, rowids []int64, args ...any) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rowids {
		if _, err = tx.ExecContext(ctx, q, append(append([]any{}, args...), r)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// QueuedCount is how many turns wait for extraction.
func (s *Store) QueuedCount(ctx context.Context) (int, error) {
	var n int
	err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM extraction_queue").Scan(&n)
	return n, err
}

// GetDatapackExtraction is a datapack's extraction settings, or ErrNoDatapack.
func (s *Store) GetDatapackExtraction(ctx context.Context, datapack string) (DatapackExtraction, error) {
	var d DatapackExtraction
	err := s.r.QueryRowContext(ctx, "SELECT memory_extraction_opt_out, memory_extraction_profile, "+
		"memory_extraction_instructions FROM datapacks WHERE id = ?", datapack).Scan(&d.OptOut, &d.Profile,
		&d.Instructions)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNoDatapack
	}
	return d, err
}

// SetDatapackExtraction replaces a datapack's extraction settings.
func (s *Store) SetDatapackExtraction(ctx context.Context, datapack string, d DatapackExtraction) error {
	res, err := s.w.ExecContext(ctx, "UPDATE datapacks SET memory_extraction_opt_out = ?, "+
		"memory_extraction_profile = ?, memory_extraction_instructions = ? WHERE id = ?", d.OptOut,
		strings.TrimSpace(d.Profile), strings.TrimSpace(d.Instructions), datapack)
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
