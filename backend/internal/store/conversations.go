package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Conversation is a row of conversations. DatapackID is "" for a conversation in no datapack.
type Conversation struct {
	ID           string
	DatapackID   string
	AgentID      string
	SessionID    string
	RunID        string
	Title        string
	Metadata     map[string]any
	CreatedAt    time.Time
	UpdatedAt    time.Time
	MessageCount int
}

// Message is one turn of a conversation.
type Message struct {
	ID             string
	ConversationID string
	Input          string
	Output         string
	Reasoning      string
	Plan           []string
	Metadata       map[string]any
	CreatedAt      time.Time
	// QueueExtraction queues a new turn for memory extraction, in the same transaction that adds it.
	QueueExtraction bool
}

// nullable is a datapack id as SQL: "" is NULL.
func nullable(datapack string) any {
	if datapack == "" {
		return nil
	}
	return datapack
}

const conversationCols = "c.id, COALESCE(c.datapack_id, ''), c.agent_id, c.session_id, c.run_id, c.title, " +
	"c.metadata, c.created_at, c.updated_at, (SELECT count(*) FROM messages m WHERE m.conversation_rowid = c.rowid)"

// datapackMatch selects conversations of one datapack, or of none for "".
const datapackMatch = "COALESCE(c.datapack_id, '') = ?"

func scanConversation(row interface{ Scan(...any) error }) (Conversation, error) {
	var c Conversation
	var md, created, updated string
	if err := row.Scan(&c.ID, &c.DatapackID, &c.AgentID, &c.SessionID, &c.RunID, &c.Title, &md, &created,
		&updated, &c.MessageCount); err != nil {
		return Conversation{}, err
	}
	return parseConversation(c, md, created, updated)
}

// parseConversation fills in a scanned conversation's metadata and times from their stored text.
func parseConversation(c Conversation, md, created, updated string) (Conversation, error) {
	if err := json.Unmarshal([]byte(md), &c.Metadata); err != nil {
		return Conversation{}, err
	}
	var err error
	if c.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
		return Conversation{}, err
	}
	if c.UpdatedAt, err = time.Parse(timeLayout, updated); err != nil {
		return Conversation{}, err
	}
	return c, nil
}

// CreateConversation adds c unless the datapack already has a conversation with its id, and
// returns the stored one either way; created says which.
func (s *Store) CreateConversation(ctx context.Context, c Conversation) (Conversation, bool, error) {
	if c.Metadata == nil {
		c.Metadata = map[string]any{}
	}
	md, err := json.Marshal(c.Metadata)
	if err != nil {
		return Conversation{}, false, err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return Conversation{}, false, err
	}
	defer tx.Rollback()
	if c.DatapackID != "" {
		if err = datapackExists(ctx, tx, c.DatapackID); err != nil {
			return Conversation{}, false, err
		}
	}
	now := time.Now().UTC().Format(timeLayout)
	res, err := tx.ExecContext(ctx, "INSERT INTO conversations(id, datapack_id, agent_id, session_id, run_id, title, "+
		"metadata, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) "+
		"ON CONFLICT (COALESCE(datapack_id, ''), id) DO NOTHING",
		c.ID, nullable(c.DatapackID), c.AgentID, c.SessionID, c.RunID, c.Title, string(md), now, now)
	if err != nil {
		return Conversation{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Conversation{}, false, err
	}
	got, err := scanConversation(tx.QueryRowContext(ctx,
		"SELECT "+conversationCols+" FROM conversations c WHERE "+datapackMatch+" AND c.id = ?", c.DatapackID, c.ID))
	if err != nil {
		return Conversation{}, false, err
	}
	return got, n == 1, tx.Commit()
}

// GetConversation is one conversation of the datapack ("" for none), or ErrNotFound.
func (s *Store) GetConversation(ctx context.Context, datapack, id string) (Conversation, error) {
	c, err := scanConversation(s.r.QueryRowContext(ctx,
		"SELECT "+conversationCols+" FROM conversations c WHERE "+datapackMatch+" AND c.id = ?", datapack, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	return c, err
}

// ListConversations is the datapack's conversations, most recently updated first, optionally only
// one agent's, and whether there are more past limit.
func (s *Store) ListConversations(ctx context.Context, datapack, agentID string, limit, offset int) (
	[]Conversation, bool, error) {
	limit = min(max(limit, 1), 1000)
	q := "SELECT " + conversationCols + " FROM conversations c WHERE " + datapackMatch
	args := []any{datapack}
	if agentID != "" {
		q += " AND c.agent_id = ?"
		args = append(args, agentID)
	}
	rows, err := s.r.QueryContext(ctx, q+" ORDER BY c.updated_at DESC, c.rowid DESC LIMIT ? OFFSET ?",
		append(args, limit+1, max(offset, 0))...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []Conversation{}
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, c)
	}
	if len(out) > limit {
		return out[:limit], true, rows.Err()
	}
	return out, false, rows.Err()
}

// UpdateConversation sets a conversation's title and metadata (nil leaves them as they are).
func (s *Store) UpdateConversation(ctx context.Context, datapack, id string, title *string,
	metadata map[string]any) (Conversation, error) {
	var md *string
	if metadata != nil {
		b, err := json.Marshal(metadata)
		if err != nil {
			return Conversation{}, err
		}
		v := string(b)
		md = &v
	}
	res, err := s.w.ExecContext(ctx, "UPDATE conversations SET title = COALESCE(?, title), "+
		"metadata = COALESCE(?, metadata), updated_at = ? WHERE COALESCE(datapack_id, '') = ? AND id = ?",
		title, md, time.Now().UTC().Format(timeLayout), datapack, id)
	if err != nil {
		return Conversation{}, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err == nil {
			err = ErrNotFound
		}
		return Conversation{}, err
	}
	return s.GetConversation(ctx, datapack, id)
}

// DeleteConversation deletes a conversation with its turns and their chunks (and their vectors).
// Memories that name it stay. It's ErrNotFound if there's no such conversation.
func (s *Store) DeleteConversation(ctx context.Context, datapack, id string) error {
	res, err := s.w.ExecContext(ctx, "DELETE FROM conversations WHERE COALESCE(datapack_id, '') = ? AND id = ?",
		datapack, id)
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

// AddMessage adds a turn to a conversation, with the chunks to embed for conversation search
// (none if it isn't to be searchable). A turn with an id the conversation already has isn't added
// again: the stored one comes back, with created false.
func (s *Store) AddMessage(ctx context.Context, datapack string, m Message, chunks []string) (Message, bool, error) {
	if m.Plan == nil {
		m.Plan = []string{}
	}
	if m.Metadata == nil {
		m.Metadata = map[string]any{}
	}
	plan, err := json.Marshal(m.Plan)
	if err != nil {
		return Message{}, false, err
	}
	md, err := json.Marshal(m.Metadata)
	if err != nil {
		return Message{}, false, err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, false, err
	}
	defer tx.Rollback()
	var conv int64
	err = tx.QueryRowContext(ctx, "SELECT rowid FROM conversations WHERE COALESCE(datapack_id, '') = ? AND id = ?",
		datapack, m.ConversationID).Scan(&conv)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, ErrNotFound
	}
	if err != nil {
		return Message{}, false, err
	}
	now := time.Now().UTC()
	var msg int64
	err = tx.QueryRowContext(ctx, "INSERT INTO messages(id, conversation_rowid, input, output, reasoning, plan, "+
		"metadata, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (conversation_rowid, id) DO NOTHING "+
		"RETURNING rowid", m.ID, conv, m.Input, m.Output, m.Reasoning, string(plan), string(md),
		now.Format(timeLayout)).Scan(&msg)
	if errors.Is(err, sql.ErrNoRows) { // already there
		existing, qerr := messagesWhere(ctx, tx, "m.conversation_rowid = ? AND m.id = ?", conv, m.ID)
		if qerr != nil || len(existing) == 0 {
			return Message{}, false, qerr
		}
		return existing[0], false, tx.Commit()
	}
	if err != nil {
		return Message{}, false, err
	}
	for i, text := range chunks {
		if _, err := tx.ExecContext(ctx, "INSERT INTO message_chunks(message_rowid, chunk_index, text) VALUES (?, ?, ?)",
			msg, i, text); err != nil {
			return Message{}, false, err
		}
	}
	if m.QueueExtraction {
		if _, err := tx.ExecContext(ctx, "INSERT INTO extraction_queue(message_rowid, queued_at) VALUES (?, ?)",
			msg, now.Format(timeLayout)); err != nil {
			return Message{}, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE conversations SET updated_at = ? WHERE rowid = ?",
		now.Format(timeLayout), conv); err != nil {
		return Message{}, false, err
	}
	m.CreatedAt = now
	return m, true, tx.Commit()
}

const messageCols = "m.id, c.id, m.input, m.output, m.reasoning, m.plan, m.metadata, m.created_at"

func messagesWhere(ctx context.Context, q querier, where string, args ...any) ([]Message, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+messageCols+" FROM messages m JOIN conversations c "+
		"ON c.rowid = m.conversation_rowid WHERE "+where+" ORDER BY m.rowid", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var plan, md, created string
		if err = rows.Scan(&m.ID, &m.ConversationID, &m.Input, &m.Output, &m.Reasoning, &plan, &md, &created); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(plan), &m.Plan); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(md), &m.Metadata); err != nil {
			return nil, err
		}
		if m.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Messages is a conversation's turns, oldest first, from offset, up to limit.
func (s *Store) Messages(ctx context.Context, datapack, conversationID string, limit, offset int) ([]Message, error) {
	return messagesWhere(ctx, s.r, datapackMatch+" AND c.id = ? AND m.rowid IN (SELECT rowid FROM messages "+
		"WHERE conversation_rowid = c.rowid ORDER BY rowid LIMIT ? OFFSET ?)",
		datapack, conversationID, min(max(limit, 1), 1000), max(offset, 0))
}

// LatestMessages is a conversation's turns counted from the newest: up to limit of them, skipping
// the offset newest, returned oldest first.
func (s *Store) LatestMessages(ctx context.Context, datapack, conversationID string, limit, offset int) (
	[]Message, error) {
	return messagesWhere(ctx, s.r, datapackMatch+" AND c.id = ? AND m.rowid IN (SELECT rowid FROM messages "+
		"WHERE conversation_rowid = c.rowid ORDER BY rowid DESC LIMIT ? OFFSET ?)",
		datapack, conversationID, min(max(limit, 1), 1000), max(offset, 0))
}

// ConversationSearchOptOut says whether a datapack opted out of conversation search.
func (s *Store) ConversationSearchOptOut(ctx context.Context, datapack string) (bool, error) {
	var out bool
	err := s.r.QueryRowContext(ctx, "SELECT conversation_search_opt_out FROM datapacks WHERE id = ?", datapack).Scan(&out)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNoDatapack
	}
	return out, err
}

// SetConversationSearchOptOut opts a datapack out of conversation search, or back in.
func (s *Store) SetConversationSearchOptOut(ctx context.Context, datapack string, optOut bool) error {
	res, err := s.w.ExecContext(ctx, "UPDATE datapacks SET conversation_search_opt_out = ? WHERE id = ?", optOut, datapack)
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

// Chunk is a turn's window of text waiting for its vector.
type Chunk struct {
	Rowid int64
	Text  string
}

// PendingChunks is up to limit chunks that have no vector yet, oldest first.
func (s *Store) PendingChunks(ctx context.Context, limit int) ([]Chunk, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT rowid, text FROM message_chunks WHERE embedded = 0 ORDER BY rowid LIMIT ?",
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chunk{}
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.Rowid, &c.Text); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutChunkVectors stores chunks' vectors, making them searchable. A chunk deleted meanwhile is
// skipped, and one that can't be searched is deleted: its conversation is in no datapack, or its
// datapack opted out of conversation search since the turn arrived.
func (s *Store) PutChunkVectors(ctx context.Context, rowids []int64, vectors [][]float32) error {
	dims, err := s.vectorDims(ctx)
	if err != nil {
		return err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range rowids {
		blob, err := vectorBlob(vectors[i], dims)
		if err != nil {
			return err
		}
		var dp sql.NullString
		var conv string
		var optedOut bool
		err = tx.QueryRowContext(ctx, "SELECT c.datapack_id, c.id, COALESCE(d.conversation_search_opt_out, 0) "+
			"FROM message_chunks k JOIN messages m ON m.rowid = k.message_rowid "+
			"JOIN conversations c ON c.rowid = m.conversation_rowid LEFT JOIN datapacks d ON d.id = c.datapack_id "+
			"WHERE k.rowid = ? AND k.embedded = 0", id).Scan(&dp, &conv, &optedOut)
		if errors.Is(err, sql.ErrNoRows) { // deleted meanwhile
			continue
		}
		if err != nil {
			return err
		}
		if !dp.Valid || optedOut { // it can't be searched: drop the chunk
			if _, err := tx.ExecContext(ctx, "DELETE FROM message_chunks WHERE rowid = ?", id); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO message_chunks_vec(rowid, datapack_id, conversation_id, embedding) "+
			"VALUES (?, ?, ?, ?)", id, dp.String, conv, blob); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE message_chunks SET embedded = 1 WHERE rowid = ?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ChunkHit is a chunk near a conversation-search query.
type ChunkHit struct {
	ConversationID string
	MessageID      string
	Text           string
	Distance       float64 // cosine distance: 0 is the same direction
}

// SearchChunks is the datapack's k chunks nearest to query outside the conversation exclude
// ("" excludes none), nearest first.
func (s *Store) SearchChunks(ctx context.Context, datapack string, query []float32, k int, exclude string) (
	[]ChunkHit, error) {
	release, err := s.searchSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	dims, err := s.vectorDims(ctx)
	if err != nil {
		return nil, err
	}
	blob, err := vectorBlob(query, dims)
	if err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx, "WITH knn AS (SELECT rowid, distance FROM message_chunks_vec "+
		"WHERE embedding MATCH ? AND k = ? AND datapack_id = ? AND conversation_id != ?) "+
		"SELECT c.id, m.id, x.text, knn.distance FROM knn JOIN message_chunks x ON x.rowid = knn.rowid "+
		"JOIN messages m ON m.rowid = x.message_rowid JOIN conversations c ON c.rowid = m.conversation_rowid "+
		"ORDER BY knn.distance", blob, min(max(k, 1), maxKNN), datapack, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChunkHit{}
	for rows.Next() {
		var h ChunkHit
		if err := rows.Scan(&h.ConversationID, &h.MessageID, &h.Text, &h.Distance); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
