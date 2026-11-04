package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Call is one row of the call log: what a tool call did, never what it carried.
type Call struct {
	ID             int64 // set when read back
	At             time.Time
	Tool           string
	DatapackID     string
	AgentID        string
	ConversationID string
	Credential     string
	// Client is the X-Meko-Client header the call came with: "agentk" for AgentK's own calls, else "".
	Client string
	// Location is where the call ran: "" for this Meko, LocationCloud for one /mcp sent on to the
	// linked Meko Cloud. Calls and ConversationCalls, which metrics read, return only this Meko's;
	// ListCalls and CallByID, the Calls page's, return both.
	Location      string
	OK            bool
	ErrorCode     string
	Ms            int64
	MemoryN       int
	KBN           int
	ConversationN int
	ScoreHigh     int
	ScoreMid      int
	ScoreLow      int
	TokensSent    int
	TokensSaved   int
	TokensIfRaw   int // context_search's baseline: the whole knowledge base's tokens (0 before it was kept)
	InputTokens   int
	OutputTokens  int
	Steps         []Step
	// HasContents says the call's contents are kept (call_contents); set by ListCalls and CallByID.
	HasContents bool
}

// CallContents is what a call sent and got back, kept only while Settings' "Keep call contents" is on.
type CallContents struct {
	Args  string // JSON
	Reply string
	Cut   bool // either was cut to fit
}

// Step is one timed part of a call, such as a search of one source.
type Step struct {
	Step string `json:"step"`
	Ms   int64  `json:"ms"`
	N    int    `json:"n"`
	// IDs is what a memory search returned, so a turn's trace can show what the agent saw. Calls
	// recorded before it was added have none.
	IDs []string `json:"ids,omitempty"`
}

// LocationCloud marks a call /mcp sent on to the linked Meko Cloud.
const LocationCloud = "cloud"

// AddCall records a call, with its contents when they're given, in one transaction.
func (s *Store) AddCall(ctx context.Context, c Call, contents *CallContents) error {
	steps := c.Steps
	if steps == nil {
		steps = []Step{}
	}
	js, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, "INSERT INTO call_log(at, tool, datapack_id, agent_id, conversation_id, "+
		"credential, ok, error_code, ms, memory_n, kb_n, conversation_n, score_high, score_mid, score_low, "+
		"tokens_sent, tokens_saved, tokens_if_raw, input_tokens, output_tokens, steps, location, client) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		c.At.UTC().Format(timeLayout), c.Tool, c.DatapackID, c.AgentID, c.ConversationID, c.Credential, c.OK,
		c.ErrorCode, c.Ms, c.MemoryN, c.KBN, c.ConversationN, c.ScoreHigh, c.ScoreMid, c.ScoreLow, c.TokensSent,
		c.TokensSaved, c.TokensIfRaw, c.InputTokens, c.OutputTokens, string(js), c.Location, c.Client)
	if err != nil {
		return err
	}
	if contents != nil {
		id, ierr := res.LastInsertId()
		if ierr != nil {
			return ierr
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO call_contents(call_id, args, reply, cut) VALUES (?, ?, ?, ?)",
			id, contents.Args, contents.Reply, contents.Cut); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CallFilter picks calls for ListCalls; an empty field doesn't filter.
type CallFilter struct {
	AgentID        string
	ConversationID string
	DatapackID     string
	Tool           string
	Client         string
	Failed         bool  // only calls that failed
	Before         int64 // only calls with a smaller id (the page before); 0 for the newest
}

// ListCalls is up to limit calls, newest first, of this Meko and the ones it sent on to Meko Cloud,
// with whether each one's contents are kept; more says older ones match too.
func (s *Store) ListCalls(ctx context.Context, f CallFilter, limit int) (calls []Call, more bool, err error) {
	q := selectCalls + "WHERE 1 = 1"
	var args []any
	for _, w := range []struct {
		col, v string
	}{{"agent_id", f.AgentID}, {"conversation_id", f.ConversationID}, {"datapack_id", f.DatapackID},
		{"tool", f.Tool}, {"client", f.Client}} {
		if w.v != "" {
			q += " AND " + w.col + " = ?"
			args = append(args, w.v)
		}
	}
	if f.Failed {
		q += " AND ok = 0"
	}
	if f.Before > 0 {
		q += " AND id < ?"
		args = append(args, f.Before)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)
	if calls, err = s.calls(ctx, q, args...); err != nil {
		return nil, false, err
	}
	if len(calls) > limit {
		calls, more = calls[:limit], true
	}
	if len(calls) == 0 {
		return calls, more, nil
	}
	// Which of the page's calls have contents: one query over the page's id range.
	rows, err := s.r.QueryContext(ctx, "SELECT call_id FROM call_contents WHERE call_id BETWEEN ? AND ?",
		calls[len(calls)-1].ID, calls[0].ID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	kept := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, false, err
		}
		kept[id] = true
	}
	for i := range calls {
		calls[i].HasContents = kept[calls[i].ID]
	}
	return calls, more, rows.Err()
}

// CallByID is one call, with its contents when they're kept (nil when not); ok is false when there's
// no such call.
func (s *Store) CallByID(ctx context.Context, id int64) (c Call, contents *CallContents, ok bool, err error) {
	cs, err := s.calls(ctx, selectCalls+"WHERE id = ?", id)
	if err != nil || len(cs) == 0 {
		return Call{}, nil, false, err
	}
	var cc CallContents
	err = s.r.QueryRowContext(ctx, "SELECT args, reply, cut FROM call_contents WHERE call_id = ?", id).
		Scan(&cc.Args, &cc.Reply, &cc.Cut)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return cs[0], nil, true, nil
	case err != nil:
		return Call{}, nil, false, err
	}
	cs[0].HasContents = true
	return cs[0], &cc, true, nil
}

// CallContentsSize is how many calls' contents are kept and their bytes.
func (s *Store) CallContentsSize(ctx context.Context) (calls, bytes int64, err error) {
	err = s.r.QueryRowContext(ctx,
		"SELECT COUNT(*), COALESCE(SUM(LENGTH(CAST(args AS BLOB)) + LENGTH(CAST(reply AS BLOB))), 0) FROM call_contents").
		Scan(&calls, &bytes)
	return calls, bytes, err
}

// ClearCallContents deletes every call's contents (the calls stay) and says how many it deleted.
func (s *Store) ClearCallContents(ctx context.Context) (int64, error) {
	res, err := s.w.ExecContext(ctx, "DELETE FROM call_contents")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneCallContents deletes the contents of calls made before a time and says how many it deleted.
func (s *Store) PruneCallContents(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.w.ExecContext(ctx,
		"DELETE FROM call_contents WHERE call_id IN (SELECT id FROM call_log WHERE at < ?)",
		before.UTC().Format(timeLayout))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Calls is this Meko's calls in a datapack since a time, oldest first ("" datapack: every call).
func (s *Store) Calls(ctx context.Context, datapack string, since time.Time) ([]Call, error) {
	return s.calls(ctx, selectCalls+"WHERE location = '' AND at >= ? AND (? = '' OR datapack_id = ?) ORDER BY at, id",
		since.UTC().Format(timeLayout), datapack, datapack)
}

// ConversationCalls is the calls made in one of a datapack's conversations since a time, oldest
// first, with the conversation's calls that failed before their datapack was known (datapack_id ”).
func (s *Store) ConversationCalls(ctx context.Context, datapack, conversation string, since time.Time) ([]Call,
	error) {
	return s.calls(ctx, selectCalls+"WHERE location = '' AND conversation_id = ? AND "+
		"(datapack_id = ? OR datapack_id = '') "+
		"AND at >= ? ORDER BY at, id", conversation, datapack, since.UTC().Format(timeLayout))
}

// CloudCalls is the calls /mcp sent on to the linked Meko Cloud since a time, oldest first.
func (s *Store) CloudCalls(ctx context.Context, since time.Time) ([]Call, error) {
	return s.calls(ctx, selectCalls+"WHERE location = ? AND at >= ? ORDER BY at, id", LocationCloud,
		since.UTC().Format(timeLayout))
}

const selectCalls = "SELECT id, at, tool, datapack_id, agent_id, conversation_id, credential, location, client, ok, " +
	"error_code, ms, " +
	"memory_n, kb_n, conversation_n, score_high, score_mid, score_low, tokens_sent, tokens_saved, tokens_if_raw, " +
	"input_tokens, " +
	"output_tokens, steps FROM call_log "

func (s *Store) calls(ctx context.Context, q string, args ...any) ([]Call, error) {
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Call{}
	for rows.Next() {
		var c Call
		var at, steps string
		if err = rows.Scan(&c.ID, &at, &c.Tool, &c.DatapackID, &c.AgentID, &c.ConversationID, &c.Credential, &c.Location,
			&c.Client, &c.OK,
			&c.ErrorCode, &c.Ms, &c.MemoryN, &c.KBN, &c.ConversationN, &c.ScoreHigh, &c.ScoreMid, &c.ScoreLow,
			&c.TokensSent, &c.TokensSaved, &c.TokensIfRaw, &c.InputTokens, &c.OutputTokens, &steps); err != nil {
			return nil, err
		}
		if c.At, err = time.Parse(timeLayout, at); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(steps), &c.Steps); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneCalls deletes the calls made before a time and says how many it deleted.
func (s *Store) PruneCalls(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.w.ExecContext(ctx, "DELETE FROM call_log WHERE at < ?", before.UTC().Format(timeLayout))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
