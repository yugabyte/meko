package store

import (
	"context"
	"sort"
)

// The sources a search map draws, as context_search names them.
const (
	MapMemory       = "memory"
	MapKnowledge    = "knowledge_base"
	MapConversation = "conversation"
)

// MapItem is one thing near a query, for the search map: a memory, a knowledge-base chunk (a
// promoted memory's included) or a conversation turn, with its vector.
type MapItem struct {
	Source         string
	ID             string // the memory's or chunk's id; a turn's message id
	Text           string
	ConversationID string
	Distance       float64 // cosine distance from the query
	Vector         []float32
}

// mapQueries reads a source's k nearest rows to the query, nearest first, with their vectors: id,
// text, conversation id, distance and embedding. Each binds the query, k and the datapack; memories' the
// user too.
var mapQueries = map[string]string{
	MapMemory: "WITH knn AS (SELECT rowid, distance, embedding FROM memories_vec WHERE embedding MATCH ? " +
		"AND k = ? AND datapack_id = ? AND user_id = ?) SELECT t.id, t.text, t.conversation_id, knn.distance, " +
		"knn.embedding FROM knn JOIN memories t ON t.rowid = knn.rowid ORDER BY knn.distance",
	MapKnowledge: "WITH knn AS (SELECT rowid, distance, embedding FROM kb_chunks_vec WHERE embedding MATCH ? " +
		"AND k = ? AND datapack_id = ?) SELECT c.id, c.text, '', knn.distance, knn.embedding FROM knn " +
		"JOIN kb_chunks c ON c.rowid = knn.rowid ORDER BY knn.distance",
	MapConversation: "WITH knn AS (SELECT rowid, distance, embedding FROM message_chunks_vec WHERE embedding " +
		"MATCH ? AND k = ? AND datapack_id = ?) SELECT m.id, x.text, c.id, knn.distance, knn.embedding " +
		"FROM knn JOIN message_chunks x ON x.rowid = knn.rowid JOIN messages m ON m.rowid = x.message_rowid " +
		"JOIN conversations c ON c.rowid = m.conversation_rowid ORDER BY knn.distance",
}

// NearestForMap is each source's n rows nearest to query, merged nearest first. A conversation turn
// is one item, at its nearest chunk, so a long turn isn't several dots; its n come from the nearest
// n*4 chunks, so they can be fewer than n when long turns fill those. A knowledge base with no
// chunks table yet has none.
func (s *Store) NearestForMap(ctx context.Context, datapack, userID string, query []float32, n int,
	sources []string) ([]MapItem, error) {
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
	var out []MapItem
	for _, src := range sources {
		q, ok := mapQueries[src]
		if !ok {
			continue
		}
		k := n
		if src == MapConversation {
			k = min(n*4, maxKNN) // turns have several chunks
		}
		args := []any{blob, k, datapack}
		if src == MapMemory {
			args = append(args, userID)
		}
		items, err := s.nearestOf(ctx, q, src, args)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Distance < out[j].Distance })
	return out, nil
}

func (s *Store) nearestOf(ctx context.Context, q, src string, args []any) ([]MapItem, error) {
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		if isNoSuchTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []MapItem
	seen := map[string]bool{}
	for rows.Next() {
		it := MapItem{Source: src}
		var emb []byte
		if err := rows.Scan(&it.ID, &it.Text, &it.ConversationID, &it.Distance, &emb); err != nil {
			return nil, err
		}
		key := it.ConversationID + "/" + it.ID
		if src == MapConversation && seen[key] {
			continue // a turn's nearer chunk came first
		}
		seen[key] = true
		it.Vector = decodeVector(emb)
		out = append(out, it)
	}
	return out, rows.Err()
}
