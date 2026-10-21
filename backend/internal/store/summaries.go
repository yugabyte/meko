package store

import (
	"context"
	"database/sql"
	"time"
)

// ConversationSummary is a conversation as meko_ui's list shows it: when it started and was last
// active, its first prompt, and how many turns and memories it has.
type ConversationSummary struct {
	Conversation
	FirstMessage   string
	LastActivityAt time.Time
	MemoriesAdded  int // memories saved in it, promoted ones included
}

// conversationActivity is a conversation's last activity: its newest turn, or its last update.
const conversationActivity = "max(c.updated_at, COALESCE((SELECT max(m.created_at) FROM messages m " +
	"WHERE m.conversation_rowid = c.rowid), ''))"

// ConversationSummaries is a page of the datapack's conversations, most recently active first,
// optionally only those whose title or turns contain query (any case), and whether there are more.
func (s *Store) ConversationSummaries(ctx context.Context, datapack, query string, limit, offset int) (
	[]ConversationSummary, bool, error) {
	return s.conversationSummaries(ctx, datapack, "", query, limit, offset)
}

// ConversationSummaryOf is one conversation's summary, or ErrNotFound.
func (s *Store) ConversationSummaryOf(ctx context.Context, datapack, id string) (ConversationSummary, error) {
	out, _, err := s.conversationSummaries(ctx, datapack, id, "", 1, 0)
	if err == nil && len(out) == 0 {
		err = ErrNotFound
	}
	if err != nil {
		return ConversationSummary{}, err
	}
	return out[0], nil
}

func (s *Store) conversationSummaries(ctx context.Context, datapack, id, query string, limit, offset int) (
	[]ConversationSummary, bool, error) {
	limit = min(max(limit, 1), 1000)
	q := "SELECT " + conversationCols + ", " +
		"COALESCE((SELECT m.input FROM messages m WHERE m.conversation_rowid = c.rowid ORDER BY m.rowid LIMIT 1), ''), " +
		conversationActivity + ", " +
		"(SELECT count(*) FROM memories x WHERE x.datapack_id = c.datapack_id AND x.conversation_id = c.id) + " +
		"(SELECT count(*) FROM promoted_memories p WHERE p.datapack_id = c.datapack_id AND p.conversation_id = c.id) " +
		"FROM conversations c WHERE " + datapackMatch + " AND (? = '' OR c.id = ?) AND " +
		"(? = '' OR instr(lower(c.title), lower(?)) > 0 OR " +
		"EXISTS (SELECT 1 FROM messages m WHERE m.conversation_rowid = c.rowid AND " +
		"(instr(lower(m.input), lower(?)) > 0 OR instr(lower(m.output), lower(?)) > 0))) " +
		"ORDER BY " + conversationActivity + " DESC, c.rowid DESC LIMIT ? OFFSET ?"
	rows, err := s.r.QueryContext(ctx, q, datapack, id, id, query, query, query, query, limit+1, max(offset, 0))
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []ConversationSummary{}
	for rows.Next() {
		var cs ConversationSummary
		var md, created, updated, activity string
		if err = rows.Scan(&cs.ID, &cs.DatapackID, &cs.AgentID, &cs.SessionID, &cs.RunID, &cs.Title, &md, &created,
			&updated, &cs.MessageCount, &cs.FirstMessage, &activity, &cs.MemoriesAdded); err != nil {
			return nil, false, err
		}
		if cs.Conversation, err = parseConversation(cs.Conversation, md, created, updated); err != nil {
			return nil, false, err
		}
		if cs.LastActivityAt, err = time.Parse(timeLayout, activity); err != nil {
			return nil, false, err
		}
		out = append(out, cs)
	}
	if len(out) > limit {
		return out[:limit], true, rows.Err()
	}
	return out, false, rows.Err()
}

// CountConversations counts the datapack's conversations.
func (s *Store) CountConversations(ctx context.Context, datapack string) (int, error) {
	var n int
	err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM conversations WHERE datapack_id = ?", datapack).Scan(&n)
	return n, err
}

// SavedMemory is a memory saved in a conversation, still a memory or since promoted.
type SavedMemory struct {
	ID        string
	Text      string
	CreatedAt time.Time
	Promoted  bool
}

// ConversationMemories is every memory saved in a conversation, oldest first.
func (s *Store) ConversationMemories(ctx context.Context, datapack, conversationID string) ([]SavedMemory, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT id, text, created_at, 0 FROM memories WHERE datapack_id = ? AND "+
		"conversation_id = ? UNION ALL SELECT id, text, created_at, 1 FROM promoted_memories WHERE datapack_id = ? AND "+
		"conversation_id = ? ORDER BY 3, 1", datapack, conversationID, datapack, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SavedMemory{}
	for rows.Next() {
		var m SavedMemory
		var created string
		if err = rows.Scan(&m.ID, &m.Text, &created, &m.Promoted); err != nil {
			return nil, err
		}
		if m.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AgentSeen is an agent that wrote to a datapack (memories, conversations) or called it, and when
// it first and last did.
type AgentSeen struct {
	AgentID         string
	First, LastSeen time.Time
}

// DatapackAgents is every agent seen in a datapack, the most recently seen first.
func (s *Store) DatapackAgents(ctx context.Context, datapack string) ([]AgentSeen, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT agent_id, min(t), max(t) FROM ("+
		"SELECT agent_id, created_at t FROM memories WHERE datapack_id = ? "+
		"UNION ALL SELECT agent_id, created_at FROM conversations WHERE datapack_id = ? "+
		"UNION ALL SELECT agent_id, updated_at FROM conversations WHERE datapack_id = ? "+
		"UNION ALL SELECT agent_id, at FROM call_log WHERE datapack_id = ?) "+
		"WHERE agent_id != '' GROUP BY agent_id ORDER BY 3 DESC, 1", datapack, datapack, datapack, datapack)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentSeen{}
	for rows.Next() {
		var a AgentSeen
		var first, last string
		if err = rows.Scan(&a.AgentID, &first, &last); err != nil {
			return nil, err
		}
		if a.First, err = time.Parse(timeLayout, first); err != nil {
			return nil, err
		}
		if a.LastSeen, err = time.Parse(timeLayout, last); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DatapackSizes is how much a datapack holds: rows and bytes of text, as meko_ui's metrics show.
type DatapackSizes struct {
	Memories, MemoryBytes           int
	ConversationChunks, TurnBytes   int
	Conversations, Artifacts        int
	ArtifactBytes, PromotedMemories int
}

// SizeDatapack measures what a datapack holds.
func (s *Store) SizeDatapack(ctx context.Context, datapack string) (DatapackSizes, error) {
	var z DatapackSizes
	var memBytes, turnBytes, artBytes sql.NullInt64
	err := s.r.QueryRowContext(ctx, "SELECT "+
		"(SELECT count(*) FROM memories WHERE datapack_id = ?), "+
		"(SELECT sum(length(CAST(text AS BLOB))) FROM memories WHERE datapack_id = ?), "+
		"(SELECT count(*) FROM message_chunks k JOIN messages m ON m.rowid = k.message_rowid "+
		"JOIN conversations c ON c.rowid = m.conversation_rowid WHERE c.datapack_id = ?), "+
		"(SELECT sum(length(CAST(m.input AS BLOB)) + length(CAST(m.output AS BLOB))) FROM messages m "+
		"JOIN conversations c ON c.rowid = m.conversation_rowid WHERE c.datapack_id = ?), "+
		"(SELECT count(*) FROM conversations WHERE datapack_id = ?), "+
		"(SELECT count(*) FROM artifacts WHERE datapack_id = ?), "+
		"(SELECT sum(size_bytes) FROM artifacts WHERE datapack_id = ?), "+
		"(SELECT count(*) FROM promoted_memories WHERE datapack_id = ?)",
		datapack, datapack, datapack, datapack, datapack, datapack, datapack, datapack).Scan(&z.Memories, &memBytes,
		&z.ConversationChunks, &turnBytes, &z.Conversations, &z.Artifacts, &artBytes, &z.PromotedMemories)
	z.MemoryBytes, z.TurnBytes, z.ArtifactBytes = int(memBytes.Int64), int(turnBytes.Int64), int(artBytes.Int64)
	return z, err
}

// CountAll counts the install's datapacks and conversations, for the user's usage.
func (s *Store) CountAll(ctx context.Context) (datapacks, conversations int, err error) {
	err = s.r.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM datapacks), (SELECT count(*) FROM conversations)").
		Scan(&datapacks, &conversations)
	return datapacks, conversations, err
}
