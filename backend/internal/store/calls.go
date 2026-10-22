package store

import (
	"context"
	"encoding/json"
	"time"
)

// Call is one row of the call log: what a tool call did, never what it carried.
type Call struct {
	At             time.Time
	Tool           string
	DatapackID     string
	AgentID        string
	ConversationID string
	Credential     string
	OK             bool
	ErrorCode      string
	Ms             int64
	MemoryN        int
	KBN            int
	ConversationN  int
	ScoreHigh      int
	ScoreMid       int
	ScoreLow       int
	TokensSent     int
	TokensSaved    int
	InputTokens    int
	OutputTokens   int
	Steps          []Step
}

// Step is one timed part of a call, such as a search of one source.
type Step struct {
	Step string `json:"step"`
	Ms   int64  `json:"ms"`
	N    int    `json:"n"`
}

// AddCall records a call.
func (s *Store) AddCall(ctx context.Context, c Call) error {
	steps := c.Steps
	if steps == nil {
		steps = []Step{}
	}
	js, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	_, err = s.w.ExecContext(ctx, "INSERT INTO call_log(at, tool, datapack_id, agent_id, conversation_id, "+
		"credential, ok, error_code, ms, memory_n, kb_n, conversation_n, score_high, score_mid, score_low, "+
		"tokens_sent, tokens_saved, input_tokens, output_tokens, steps) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		c.At.UTC().Format(timeLayout), c.Tool, c.DatapackID, c.AgentID, c.ConversationID, c.Credential, c.OK,
		c.ErrorCode, c.Ms, c.MemoryN, c.KBN, c.ConversationN, c.ScoreHigh, c.ScoreMid, c.ScoreLow, c.TokensSent,
		c.TokensSaved, c.InputTokens, c.OutputTokens, string(js))
	return err
}

// Calls is the datapack's calls since a time, oldest first ("" datapack: every call).
func (s *Store) Calls(ctx context.Context, datapack string, since time.Time) ([]Call, error) {
	q := "SELECT at, tool, datapack_id, agent_id, conversation_id, credential, ok, error_code, ms, memory_n, " +
		"kb_n, conversation_n, score_high, score_mid, score_low, tokens_sent, tokens_saved, input_tokens, " +
		"output_tokens, steps FROM call_log WHERE at >= ? AND (? = '' OR datapack_id = ?) ORDER BY at, id"
	rows, err := s.r.QueryContext(ctx, q, since.UTC().Format(timeLayout), datapack, datapack)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Call{}
	for rows.Next() {
		var c Call
		var at, steps string
		if err = rows.Scan(&at, &c.Tool, &c.DatapackID, &c.AgentID, &c.ConversationID, &c.Credential, &c.OK,
			&c.ErrorCode, &c.Ms, &c.MemoryN, &c.KBN, &c.ConversationN, &c.ScoreHigh, &c.ScoreMid, &c.ScoreLow,
			&c.TokensSent, &c.TokensSaved, &c.InputTokens, &c.OutputTokens, &steps); err != nil {
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
