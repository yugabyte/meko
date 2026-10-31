package store

import (
	"context"
	"encoding/json"
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
	// Location is where the call ran: "" for this Meko, LocationCloud for one /mcp sent on to the
	// linked Meko Cloud. Calls and the other reads return only this Meko's.
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
		"tokens_sent, tokens_saved, tokens_if_raw, input_tokens, output_tokens, steps, location) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		c.At.UTC().Format(timeLayout), c.Tool, c.DatapackID, c.AgentID, c.ConversationID, c.Credential, c.OK,
		c.ErrorCode, c.Ms, c.MemoryN, c.KBN, c.ConversationN, c.ScoreHigh, c.ScoreMid, c.ScoreLow, c.TokensSent,
		c.TokensSaved, c.TokensIfRaw, c.InputTokens, c.OutputTokens, string(js), c.Location)
	return err
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

const selectCalls = "SELECT id, at, tool, datapack_id, agent_id, conversation_id, credential, location, ok, " +
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
			&c.OK,
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
