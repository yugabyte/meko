package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Ask is one search Meko answered poorly: a row of question_asks.
type Ask struct {
	DatapackID     string
	Key            string // the query lower-cased with its whitespace collapsed
	At             time.Time
	Query          string
	Quality        string // empty or weak
	Tool           string
	AgentID        string
	ConversationID string
	Results        int
	TopScore       *float64
	TopRaw         *float64
}

// Question is what was done about one of a datapack's questions: a row of questions.
type Question struct {
	Key             string
	AnsweredAt      *time.Time
	DismissedAt     *time.Time
	DismissedBy     string // the agent's conversation; "" when a person did it
	DismissedMemory string
	TaughtAt        *time.Time
	TaughtText      string
	TaughtMemory    string
	TaughtBy        string // as DismissedBy
	OpenedAt        *time.Time
}

// QuestionMark is something done about a question: dismissed, taught, or opened again (restored).
// By is the conversation of the agent that did it, "" for a person.
type QuestionMark struct {
	State  string // dismissed, taught or open
	At     time.Time
	By     string
	Text   string // taught: what was saved
	Memory string // taught: the memory saved; dismissed: the one that already said it
}

// AddAsk records a poor search, making its question if it's the first ask.
func (s *Store) AddAsk(ctx context.Context, a Ask) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO questions(datapack_id, query_key) VALUES (?, ?) "+
		"ON CONFLICT DO NOTHING", a.DatapackID, a.Key); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO question_asks(datapack_id, query_key, at, query, quality, tool, "+
		"agent_id, conversation_id, results, top_score, top_raw) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		a.DatapackID, a.Key, a.At.UTC().Format(timeLayout), a.Query, a.Quality, a.Tool, a.AgentID,
		a.ConversationID, a.Results, a.TopScore, a.TopRaw); err != nil {
		return err
	}
	return tx.Commit()
}

// AnswerQuestion records that a search of the question found something relevant, when the datapack
// has that question; there's nothing to record otherwise.
func (s *Store) AnswerQuestion(ctx context.Context, datapack, key string, at time.Time) error {
	_, err := s.w.ExecContext(ctx, "UPDATE questions SET answered_at = ? WHERE datapack_id = ? AND query_key = ?",
		at.UTC().Format(timeLayout), datapack, key)
	return err
}

// Asks is a datapack's poor searches, latest first (ties by id, latest first).
func (s *Store) Asks(ctx context.Context, datapack string) ([]Ask, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT query_key, at, query, quality, tool, agent_id, conversation_id, "+
		"results, top_score, top_raw FROM question_asks WHERE datapack_id = ? ORDER BY at DESC, id DESC", datapack)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Ask{}
	for rows.Next() {
		a := Ask{DatapackID: datapack}
		var at string
		var top, raw sql.NullFloat64
		if err = rows.Scan(&a.Key, &at, &a.Query, &a.Quality, &a.Tool, &a.AgentID, &a.ConversationID, &a.Results,
			&top, &raw); err != nil {
			return nil, err
		}
		if a.At, err = time.Parse(timeLayout, at); err != nil {
			return nil, err
		}
		if top.Valid {
			a.TopScore = &top.Float64
		}
		if raw.Valid {
			a.TopRaw = &raw.Float64
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const questionCols = "query_key, answered_at, dismissed_at, dismissed_by, dismissed_memory, taught_at, " +
	"taught_text, taught_memory, taught_by, opened_at"

func scanQuestion(sc interface{ Scan(...any) error }) (Question, error) {
	var q Question
	var answered, dismissed, dismissedBy, dismissedMemory, taught, taughtText, taughtMemory, taughtBy,
		opened sql.NullString
	if err := sc.Scan(&q.Key, &answered, &dismissed, &dismissedBy, &dismissedMemory, &taught, &taughtText,
		&taughtMemory, &taughtBy, &opened); err != nil {
		return Question{}, err
	}
	var err error
	for _, f := range []struct {
		v   sql.NullString
		dst **time.Time
	}{{answered, &q.AnsweredAt}, {dismissed, &q.DismissedAt}, {taught, &q.TaughtAt}, {opened, &q.OpenedAt}} {
		if !f.v.Valid {
			continue
		}
		var t time.Time
		if t, err = time.Parse(timeLayout, f.v.String); err != nil {
			return Question{}, err
		}
		*f.dst = &t
	}
	q.DismissedBy, q.DismissedMemory = dismissedBy.String, dismissedMemory.String
	q.TaughtText, q.TaughtMemory, q.TaughtBy = taughtText.String, taughtMemory.String, taughtBy.String
	return q, nil
}

// Questions is what was done about each of a datapack's questions.
func (s *Store) Questions(ctx context.Context, datapack string) ([]Question, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+questionCols+" FROM questions WHERE datapack_id = ?", datapack)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Question{}
	for rows.Next() {
		q, err := scanQuestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// MarkQuestion records something done about a question, or ErrNotFound when the datapack has no
// such question. A person's own dismissal and teach are kept apart too (person_*_at), so an
// agent's later mark doesn't take them out of the usage stats; a restore clears a person's
// dismissal only when theirs was the one it restores. A restore keeps what was taught.
func (s *Store) MarkQuestion(ctx context.Context, datapack, key string, m QuestionMark) error {
	at := m.At.UTC().Format(timeLayout)
	by := sql.NullString{String: m.By, Valid: m.By != ""}
	var res sql.Result
	var err error
	switch m.State {
	case "dismissed":
		res, err = s.w.ExecContext(ctx, "UPDATE questions SET dismissed_at = ?, dismissed_by = ?, dismissed_memory = ?, "+
			"person_dismissed_at = CASE WHEN ? IS NULL THEN ? ELSE person_dismissed_at END "+
			"WHERE datapack_id = ? AND query_key = ?",
			at, by, sql.NullString{String: m.Memory, Valid: m.Memory != ""}, by, at, datapack, key)
	case "taught":
		res, err = s.w.ExecContext(ctx, "UPDATE questions SET taught_at = ?, taught_text = ?, taught_memory = ?, "+
			"taught_by = ?, person_taught_at = CASE WHEN ? IS NULL THEN ? ELSE person_taught_at END "+
			"WHERE datapack_id = ? AND query_key = ?",
			at, m.Text, sql.NullString{String: m.Memory, Valid: m.Memory != ""}, by, by, at, datapack, key)
	case "open":
		// Values on the right are the row's before the update: the restored dismissal's.
		res, err = s.w.ExecContext(ctx, "UPDATE questions SET dismissed_at = NULL, dismissed_by = NULL, "+
			"dismissed_memory = NULL, opened_at = ?, "+
			"person_dismissed_at = CASE WHEN dismissed_by IS NULL THEN NULL ELSE person_dismissed_at END "+
			"WHERE datapack_id = ? AND query_key = ?", at, datapack, key)
	default:
		return errors.New("unknown question state " + m.State)
	}
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

// DeleteAsks deletes one conversation's asks in a datapack made before a time, and the questions
// left with no asks and nothing done about them, and says how many asks it deleted.
func (s *Store) DeleteAsks(ctx context.Context, datapack, conversation string, before time.Time) (int64, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "DELETE FROM question_asks WHERE datapack_id = ? AND conversation_id = ? AND at < ?",
		datapack, conversation, before.UTC().Format(timeLayout))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	// A question someone dismissed or taught stays, with its mark, until pruning takes it.
	const unmarked = "datapack_id = ? AND dismissed_at IS NULL AND taught_at IS NULL AND "
	if err = deleteUnasked(ctx, tx, unmarked, datapack); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// PruneAsks deletes the asks made before a time, and the questions left with none, and says how
// many asks it deleted.
func (s *Store) PruneAsks(ctx context.Context, before time.Time) (int64, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "DELETE FROM question_asks WHERE at < ?", before.UTC().Format(timeLayout))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err = deleteUnasked(ctx, tx, "", ""); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// deleteUnasked deletes the questions with no asks left (and, with where, only those it picks): with
// nothing to show, they're off the list. A pruned question's person_*_at go with it, as the call
// log's days do.
func deleteUnasked(ctx context.Context, tx *sql.Tx, where, datapack string) error {
	args := []any{}
	if datapack != "" {
		args = append(args, datapack)
	}
	// G202: where is one of the constants above, never input.
	_, err := tx.ExecContext(ctx, "DELETE FROM questions WHERE "+where+ //nolint:gosec
		"NOT EXISTS (SELECT 1 FROM question_asks a WHERE a.datapack_id = questions.datapack_id "+
		"AND a.query_key = questions.query_key)", args...)
	return err
}

// QuestionDay is one UTC day's poor searches by quality and people's marks, for the usage stats.
type QuestionDay struct {
	Day       string // YYYY-MM-DD
	Empty     int
	Weak      int
	Dismissed int
	Taught    int
}

// QuestionDays counts the poor searches since a time and people's latest dismissals and teaches
// made since, by UTC day, oldest first, on this Meko's own datapacks only: a linked Meko Cloud's,
// whose questions are kept here too, aren't this Meko's usage. Never a query.
func (s *Store) QuestionDays(ctx context.Context, since time.Time) ([]QuestionDay, error) {
	t := since.UTC().Format(timeLayout)
	const local = "datapack_id IN (SELECT id FROM datapacks) "
	rows, err := s.r.QueryContext(ctx, "SELECT day, sum(empty), sum(weak), sum(dismissed), sum(taught) FROM ("+
		"SELECT substr(at, 1, 10) AS day, quality = 'empty' AS empty, quality = 'weak' AS weak, 0 AS dismissed, "+
		"0 AS taught FROM question_asks WHERE at >= ? AND "+local+
		"UNION ALL SELECT substr(person_dismissed_at, 1, 10), 0, 0, 1, 0 FROM questions "+
		"WHERE person_dismissed_at >= ? AND "+local+
		"UNION ALL SELECT substr(person_taught_at, 1, 10), 0, 0, 0, 1 FROM questions WHERE person_taught_at >= ? AND "+
		local+") GROUP BY day ORDER BY day", t, t, t)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QuestionDay{}
	for rows.Next() {
		var d QuestionDay
		if err = rows.Scan(&d.Day, &d.Empty, &d.Weak, &d.Dismissed, &d.Taught); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
