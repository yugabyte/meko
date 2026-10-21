package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Kind is the table a mem0 collection maps to. The engine's collection "<datapack id>" holds
// memories, and "<datapack id>_entities" the entities mem0 links them to.
type Kind int

// Kinds.
const (
	Memories Kind = iota
	Entities
)

// Record is a row as mem0's VectorStore sees it: an id, mem0's payload and, from a search, a score
// (cosine similarity for vector search, BM25 for keyword search; higher is better for both).
type Record struct {
	ID      string         `json:"id"`
	Payload map[string]any `json:"payload"`
	Score   *float64       `json:"score,omitempty"`
}

// Errors the memory methods return.
var (
	ErrNotFound      = errors.New("not found")
	ErrNoDatapack    = errors.New("no such datapack")
	ErrOtherDatapack = errors.New("the id belongs to another datapack")
	ErrBadFilter     = errors.New("bad filter")
	ErrBadVector     = errors.New("bad vector")
)

// Limits on what one call reads.
const (
	defaultListLimit = 100   // mem0's own stores list 100 when no limit is given
	maxListLimit     = 10000 // mem0 lists up to 10000 entities when it links them
	maxKNN           = 4096  // sqlite-vec's largest k
	overFetch        = 10    // KNN candidates per wanted result when a filter runs after the scan
)

// jsTime is how times go back to the engine: what JavaScript's Date.toISOString writes.
const jsTime = "2006-01-02T15:04:05.000Z07:00"

type colKind int

const (
	textCol colKind = iota
	timeCol
	jsonCol
)

// column is a SQL column filled from mem0's payload. keys are the payload keys read into it (the
// first present wins, and all are taken out of the JSON payload); out is the key it's written
// back as.
type column struct {
	name string
	keys []string
	out  string
	kind colKind
}

// table maps one Kind to SQL. Every table also has datapack_id, payload (the rest of mem0's
// payload, as JSON), created_at and updated_at.
type table struct {
	name, vec string
	cols      []column
	sql       tableSQL
}

// tableSQL is a table's fixed statements, built once from its names (never from input).
type tableSQL struct {
	insert, update, get, delete, vecDelete, vecInsert string
}

func init() {
	for _, t := range tables {
		names := t.colNames()
		sets := make([]string, len(names))
		for i, n := range names {
			sets[i] = n + " = ?"
			if n == "created_at" {
				sets[i] = "created_at = COALESCE(?, created_at)"
			}
		}
		t.sql = tableSQL{
			insert: "INSERT INTO " + t.name + "(id, datapack_id, " + strings.Join(names, ", ") + ") VALUES (?, ?" +
				strings.Repeat(", ?", len(names)) + ") RETURNING rowid",
			update:    "UPDATE " + t.name + " SET " + strings.Join(sets, ", ") + " WHERE rowid = ?",
			get:       "SELECT " + t.selectList() + " FROM " + t.name + " t WHERE t.id = ? AND t.datapack_id = ?",
			delete:    "DELETE FROM " + t.name + " WHERE id = ? AND datapack_id = ?",
			vecDelete: "DELETE FROM " + t.vec + " WHERE rowid = ?",
			vecInsert: "INSERT INTO " + t.vec + "(rowid, datapack_id, " + strings.Join(pushdown, ", ") +
				", embedding) VALUES (?, ?, ?, ?, ?, ?)",
		}
	}
}

// The vec0 metadata columns, which a KNN can filter on inside the scan. Both tables have them.
var pushdown = []string{"user_id", "agent_id", "conversation_id"}

var tables = map[Kind]*table{
	Memories: {name: "memories", vec: "memories_vec", cols: []column{
		{name: "user_id", keys: []string{"user_id"}, out: "user_id"},
		{name: "agent_id", keys: []string{"agent_id"}, out: "agent_id"},
		{name: "conversation_id", keys: []string{"meko_conversation_id", "run_id"}, out: "meko_conversation_id"},
		{name: "role", keys: []string{"role"}, out: "role"},
		{name: "actor_id", keys: []string{"actor_id"}, out: "actor_id"},
		{name: "text", keys: []string{"data"}, out: "data"},
		{name: "text_lemmatized", keys: []string{"textLemmatized", "text_lemmatized"}, out: "textLemmatized"},
		{name: "hash", keys: []string{"hash"}, out: "hash"},
		// A caller's metadata.created_at (a backdated memory) wins over mem0's own createdAt.
		{name: "created_at", keys: []string{"created_at", "createdAt"}, out: "createdAt", kind: timeCol},
		{name: "updated_at", keys: []string{"updatedAt", "updated_at"}, out: "updatedAt", kind: timeCol},
	}},
	Entities: {name: "entities", vec: "entities_vec", cols: []column{
		{name: "user_id", keys: []string{"user_id"}, out: "user_id"},
		{name: "agent_id", keys: []string{"agent_id"}, out: "agent_id"},
		{name: "conversation_id", keys: []string{"meko_conversation_id", "run_id"}, out: "meko_conversation_id"},
		{name: "text", keys: []string{"data"}, out: "data"},
		{name: "entity_type", keys: []string{"entityType"}, out: "entityType"},
		{name: "linked_memory_ids", keys: []string{"linkedMemoryIds"}, out: "linkedMemoryIds", kind: jsonCol},
	}},
}

// colNames is every column a write sets and a read returns, in order: the mapped ones, then
// payload, created_at and updated_at unless mapped.
func (t *table) colNames() []string {
	names := make([]string, 0, len(t.cols)+3)
	has := map[string]bool{}
	for _, c := range t.cols {
		names = append(names, c.name)
		has[c.name] = true
	}
	names = append(names, "payload")
	for _, n := range []string{"created_at", "updated_at"} {
		if !has[n] {
			names = append(names, n)
		}
	}
	return names
}

// selectList is colNames, after the id, for a query that names the table t.
func (t *table) selectList() string {
	names := t.colNames()
	for i, n := range names {
		names[i] = "t." + n
	}
	return "t.id, " + strings.Join(names, ", ")
}

// colExpr is the SQL a filter on a payload key tests, in a query that names the table t: its
// column, or the key in the JSON payload.
func (t *table) colExpr(key string) (string, []any) {
	for _, c := range t.cols {
		for _, k := range c.keys {
			if k == key {
				return "t." + c.name, nil
			}
		}
	}
	return "json_extract(t.payload, ?)", []any{`$."` + strings.ReplaceAll(key, `"`, `""`) + `"`}
}

// row is a payload split into column values (nil for a time that wasn't given) and the rest.
type row struct {
	vals  map[string]any
	extra string
}

// split maps a payload onto t's columns. Absent text columns are ” and an absent JSON column is
// an empty list; a time that doesn't parse stays in the JSON payload.
func (t *table) split(p map[string]any) (row, error) {
	rest := make(map[string]any, len(p))
	for k, v := range p {
		rest[k] = v
	}
	r := row{vals: map[string]any{}}
	for _, c := range t.cols {
		var val any
		switch c.kind {
		case textCol:
			val = ""
		case jsonCol:
			val = "[]"
		}
		found := false
		for _, k := range c.keys {
			v, ok := rest[k]
			if !ok || v == nil {
				delete(rest, k)
				continue
			}
			if found {
				delete(rest, k) // a later spelling of a key already taken
				continue
			}
			switch c.kind {
			case textCol:
				if s, isStr := v.(string); isStr {
					val = s
				} else {
					val = fmt.Sprint(v)
				}
			case jsonCol:
				b, err := json.Marshal(v)
				if err != nil {
					return row{}, err
				}
				val = string(b)
			case timeCol:
				ts, ok := parseTime(v)
				if !ok {
					continue // stays in the payload
				}
				val = ts.UTC().Format(timeLayout)
			}
			found = true
			delete(rest, k)
		}
		r.vals[c.name] = val
	}
	b, err := json.Marshal(rest)
	if err != nil {
		return row{}, err
	}
	r.extra = string(b)
	return r, nil
}

// parseTime reads an RFC 3339 time or a plain date.
func parseTime(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02"} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

// scan reads a row selected with selectList (plus any extra destinations) into a Record.
func (t *table) scan(rows *sql.Rows, extra ...any) (Record, error) {
	names := t.colNames()
	vals := make([]string, len(names))
	var id string
	dest := make([]any, 0, 1+len(vals)+len(extra))
	dest = append(dest, &id)
	for i := range vals {
		dest = append(dest, &vals[i])
	}
	if err := rows.Scan(append(dest, extra...)...); err != nil {
		return Record{}, err
	}
	byName := map[string]string{}
	for i, n := range names {
		byName[n] = vals[i]
	}
	p := map[string]any{}
	if err := json.Unmarshal([]byte(byName["payload"]), &p); err != nil {
		return Record{}, fmt.Errorf("%s %s has a bad payload: %w", t.name, id, err)
	}
	for _, c := range t.cols {
		v := byName[c.name]
		switch {
		case c.kind == jsonCol:
			var j any
			if err := json.Unmarshal([]byte(v), &j); err != nil {
				return Record{}, fmt.Errorf("%s %s has bad %s: %w", t.name, id, c.name, err)
			}
			p[c.out] = j
		case c.kind == timeCol:
			if ts, err := time.Parse(timeLayout, v); err == nil {
				p[c.out] = ts.UTC().Format(jsTime)
			}
		case v != "" || c.name == "text":
			p[c.out] = v
		}
	}
	return Record{ID: id, Payload: p}, nil
}

// InsertVectors adds rows with their vectors, as mem0's VectorStore.insert does. An id that's
// already in the datapack is replaced, as mem0's own stores do; one in another datapack is refused.
func (s *Store) InsertVectors(ctx context.Context, k Kind, datapack string, ids []string, vectors [][]float32,
	payloads []map[string]any) error {
	if len(ids) != len(vectors) || len(ids) != len(payloads) {
		return fmt.Errorf("%d ids, %d vectors and %d payloads don't match", len(ids), len(vectors), len(payloads))
	}
	dims, err := s.vectorDims(ctx)
	if err != nil {
		return err
	}
	t := tables[k]
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := datapackExists(ctx, tx, datapack); err != nil {
		return err
	}
	now := time.Now().UTC().Format(timeLayout)
	for i, id := range ids {
		blob, err := vectorBlob(vectors[i], dims)
		if err != nil {
			return err
		}
		r, err := t.split(payloads[i])
		if err != nil {
			return err
		}
		var rowid int64
		var dp string
		err = tx.QueryRowContext(ctx, "SELECT rowid, datapack_id FROM "+t.name+" WHERE id = ?", id).Scan(&rowid, &dp)
		switch {
		case err == nil && dp != datapack:
			return ErrOtherDatapack
		case err == nil:
			if err = t.update(ctx, tx, rowid, r, now); err != nil {
				return err
			}
		case errors.Is(err, sql.ErrNoRows):
			if rowid, err = t.insert(ctx, tx, id, datapack, r, now); err != nil {
				return err
			}
		default:
			return err
		}
		if err := t.putVector(ctx, tx, rowid, datapack, r, blob); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (t *table) insert(ctx context.Context, tx *sql.Tx, id, datapack string, r row, now string) (int64, error) {
	args := []any{id, datapack}
	for _, n := range t.colNames() {
		switch v := r.vals[n]; {
		case n == "payload":
			args = append(args, r.extra)
		case v == nil: // a time that wasn't given (or a timestamp column mem0 doesn't send)
			args = append(args, now)
		default:
			args = append(args, v)
		}
	}
	var rowid int64
	err := tx.QueryRowContext(ctx, t.sql.insert, args...).Scan(&rowid)
	return rowid, err
}

// update replaces a row's columns from r. A created_at that wasn't given is kept; an updated_at
// that wasn't given is now.
func (t *table) update(ctx context.Context, tx *sql.Tx, rowid int64, r row, now string) error {
	var args []any
	for _, n := range t.colNames() {
		switch v := r.vals[n]; {
		case n == "payload":
			args = append(args, r.extra)
		case n == "created_at": // nil keeps the stored one (COALESCE)
			args = append(args, v)
		case v == nil:
			args = append(args, now)
		default:
			args = append(args, v)
		}
	}
	_, err := tx.ExecContext(ctx, t.sql.update, append(args, rowid)...)
	return err
}

func (t *table) putVector(ctx context.Context, tx *sql.Tx, rowid int64, datapack string, r row, blob []byte) error {
	if _, err := tx.ExecContext(ctx, t.sql.vecDelete, rowid); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, t.sql.vecInsert,
		rowid, datapack, r.vals["user_id"], r.vals["agent_id"], r.vals["conversation_id"], blob)
	return err
}

func datapackExists(ctx context.Context, q querier, datapack string) error {
	var one int
	err := q.QueryRowContext(ctx, "SELECT 1 FROM datapacks WHERE id = ?", datapack).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoDatapack
	}
	return err
}

// UpdateVector replaces a row's vector and payload, as mem0's VectorStore.update does.
func (s *Store) UpdateVector(ctx context.Context, k Kind, datapack, id string, vector []float32,
	payload map[string]any) error {
	dims, err := s.vectorDims(ctx)
	if err != nil {
		return err
	}
	blob, err := vectorBlob(vector, dims)
	if err != nil {
		return err
	}
	t := tables[k]
	r, err := t.split(payload)
	if err != nil {
		return err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rowid int64
	err = tx.QueryRowContext(ctx, "SELECT rowid FROM "+t.name+" WHERE id = ? AND datapack_id = ?", id, datapack).
		Scan(&rowid)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := t.update(ctx, tx, rowid, r, time.Now().UTC().Format(timeLayout)); err != nil {
		return err
	}
	if err := t.putVector(ctx, tx, rowid, datapack, r, blob); err != nil {
		return err
	}
	return tx.Commit()
}

// GetVector is one row of the datapack, or nil if it has no row with that id.
func (s *Store) GetVector(ctx context.Context, k Kind, datapack, id string) (*Record, error) {
	t := tables[k]
	rows, err := s.r.QueryContext(ctx, t.sql.get, id, datapack)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	rec, err := t.scan(rows)
	if err != nil {
		return nil, err
	}
	return &rec, rows.Err()
}

// DeleteVector deletes a row and its vector (and its keyword index entry). A missing id is no error.
func (s *Store) DeleteVector(ctx context.Context, k Kind, datapack, id string) error {
	_, err := s.w.ExecContext(ctx, tables[k].sql.delete, id, datapack)
	return err
}

// ListVectors is the datapack's rows that match f, oldest first (by created_at, which a backdated
// memory sets), up to limit (100 when it's 0), and how many match in all.
func (s *Store) ListVectors(ctx context.Context, k Kind, datapack string, f Filter, limit int) ([]Record, int, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxListLimit)
	t := tables[k]
	cond, args, err := filterSQL(f, t.colExpr)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrBadFilter, err)
	}
	where := " FROM " + t.name + " t WHERE t.datapack_id = ? AND " + cond
	all := append([]any{datapack}, args...)
	var total int
	if err = s.r.QueryRowContext(ctx, "SELECT count(*)"+where, all...).Scan(&total); err != nil {
		return nil, 0, err
	}
	// G202: fixed names, and filterSQL binds every value.
	q := "SELECT " + t.selectList() + where + " ORDER BY t.created_at, t.rowid LIMIT ?" //nolint:gosec
	rows, err := s.r.QueryContext(ctx, q, append(all, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		rec, err := t.scan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, rec)
	}
	return out, total, rows.Err()
}

// SearchVectors is the datapack's topK rows nearest to query that match f, nearest first, scored
// by cosine similarity (1 - cosine distance), as mem0's VectorStore.search expects. Equality on
// user_id, agent_id or conversation_id runs inside the vec0 scan; any other filter runs on the
// scan's nearest candidates, overFetch per wanted result.
func (s *Store) SearchVectors(ctx context.Context, k Kind, datapack string, query []float32, topK int,
	f Filter) ([]Record, error) {
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
	topK = min(max(topK, 1), maxKNN)
	t := tables[k]
	knnWhere := "embedding MATCH ? AND k = ? AND datapack_id = ?"
	pushed, rest := splitPushdown(f)
	cand := topK
	if len(rest) > 0 {
		cand = min(topK*overFetch, maxKNN)
	}
	knnArgs := []any{blob, cand, datapack}
	for _, c := range pushdown {
		if v, ok := pushed[c]; ok {
			knnWhere += " AND " + c + " = ?"
			knnArgs = append(knnArgs, v)
		}
	}
	cond, args, err := filterSQL(rest, t.colExpr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadFilter, err)
	}
	// G202: fixed names, and filterSQL binds every value.
	q := "WITH knn AS (SELECT rowid, distance FROM " + t.vec + " WHERE " + knnWhere + ") " + //nolint:gosec
		"SELECT " + t.selectList() + ", knn.distance FROM knn JOIN " + t.name + " t ON t.rowid = knn.rowid " +
		"WHERE " + cond + " ORDER BY knn.distance LIMIT ?"
	rows, err := s.r.QueryContext(ctx, q, append(append(knnArgs, args...), topK)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var dist float64
		rec, err := t.scan(rows, &dist)
		if err != nil {
			return nil, err
		}
		sim := 1 - dist
		rec.Score = &sim
		out = append(out, rec)
	}
	return out, rows.Err()
}

// pushKeys are the payload keys whose string equality runs inside the vec0 scan, and the metadata
// column each tests: the same columns colExpr maps them to, so a filter matches the same rows on
// every path.
var pushKeys = map[string]string{
	"user_id": "user_id", "agent_id": "agent_id", "meko_conversation_id": "conversation_id", "run_id": "conversation_id",
}

// splitPushdown takes from f the string equalities on vec0's metadata columns.
func splitPushdown(f Filter) (pushed map[string]string, rest Filter) {
	pushed, rest = map[string]string{}, Filter{}
	for k, v := range f {
		col, ok := pushKeys[k]
		s, isStr := v.(string)
		_, dup := pushed[col]
		if ok && isStr && s != "*" && !dup {
			pushed[col] = s
			continue
		}
		rest[k] = v
	}
	return pushed, rest
}

// KeywordSearch is the datapack's topK memories matching any of query's terms (mem0 sends them
// lemmatized and space-separated) that match f, best first, scored by BM25 as mem0's keywordSearch
// expects: positive, higher is better (FTS5's bm25() is negative, so it's negated).
func (s *Store) KeywordSearch(ctx context.Context, datapack, query string, topK int, f Filter) ([]Record, error) {
	release, err := s.searchSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	terms, err := s.weighableTerms(ctx, queryTerms(query))
	if err != nil {
		return nil, err
	}
	match := ftsQuery(datapack, terms)
	if match == "" {
		return []Record{}, nil
	}
	topK = min(max(topK, 1), maxListLimit)
	t := tables[Memories]
	cond, args, err := filterSQL(f, t.colExpr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadFilter, err)
	}
	// The datapack column's weight is 0: matching it says nothing about relevance.
	// G202: fixed names, and filterSQL binds every value.
	q := "SELECT " + t.selectList() + ", -bm25(memories_fts, 0.0, 1.0) AS score FROM memories_fts " + //nolint:gosec
		"JOIN memories t ON t.rowid = memories_fts.rowid WHERE memories_fts MATCH ? AND " + cond +
		" ORDER BY score DESC LIMIT ?"
	rows, err := s.r.QueryContext(ctx, q, append(append([]any{match}, args...), topK)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var score float64
		rec, err := t.scan(rows, &score)
		if err != nil {
			return nil, err
		}
		rec.Score = &score
		out = append(out, rec)
	}
	return out, rows.Err()
}

// maxTerms caps the terms of one keyword query. AgentK's recall queries run to hundreds of words.
const maxTerms = 64

// queryTerms is a query's distinct words, lowercased, at most maxTerms, sorted.
func queryTerms(query string) []string {
	seen := map[string]bool{}
	var terms []string
	for _, w := range strings.Fields(strings.ToLower(query)) {
		if !seen[w] && strings.ContainsFunc(w, isWordRune) && len(terms) < maxTerms {
			seen[w] = true
			terms = append(terms, w)
		}
	}
	sort.Strings(terms)
	return terms
}

// weighableTerms drops the terms in half or more of all rows. FTS5's bm25() computes IDF as
// log((N-n+0.5)/(n+0.5)) and clamps it to 1e-6 when that's <= 0, which happens from n = N/2: such a
// term adds nothing to any score, but matched with OR it would still make nearly every row a match
// to score and sort (measured: 366 ms instead of 138 ms over 200k memories), and count a weightless
// match as a keyword hit in mem0's divisor. Rows are counted in text_lemmatized only, as bm25()
// counts them.
func (s *Store) weighableTerms(ctx context.Context, terms []string) ([]string, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	var rows int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM memories").Scan(&rows); err != nil {
		return nil, err
	}
	keep := terms[:0:0]
	for _, t := range terms {
		var docs int
		err := s.r.QueryRowContext(ctx,
			"SELECT doc FROM memories_fts_vocab WHERE term = ? AND col = 'text_lemmatized'", t).Scan(&docs)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if docs*2 < rows {
			keep = append(keep, t)
		}
	}
	return keep, nil
}

// ftsQuery is an FTS5 query for any of the terms within the datapack, each term quoted so FTS5
// syntax in it is just text: datapack_id:"<id>" AND text_lemmatized:("a" OR "b"). It's "" when
// there are no terms.
func ftsQuery(datapack string, terms []string) string {
	if len(terms) == 0 {
		return ""
	}
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = quote(t)
	}
	return "datapack_id:" + quote(datapack) + " AND text_lemmatized:(" + strings.Join(quoted, " OR ") + ")"
}

// isWordRune is what FTS5's unicode61 tokenizer keeps: a term of only punctuation matches nothing.
func isWordRune(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r > 127
}

// MemoryPage asks for a page of a user's memories, newest first, as Meko Cloud's memory list
// returns them.
type MemoryPage struct {
	DatapackID string
	UserID     string
	AgentID    string // "" for every agent's
	Scope      string // ScopeLearnings for learnings only; "" or ScopeMemories for all
	Query      string // a case-insensitive substring of the text; "" for all
	Limit      int
	Offset     int
}

// ListMemoryPage is a page of memories and how many match in all.
func (s *Store) ListMemoryPage(ctx context.Context, p MemoryPage) ([]Record, int, error) {
	t := tables[Memories]
	where := " FROM memories t WHERE t.datapack_id = ? AND t.user_id = ?"
	args := []any{p.DatapackID, p.UserID}
	if p.AgentID != "" {
		where += " AND t.agent_id = ?"
		args = append(args, p.AgentID)
	}
	if p.Scope == ScopeLearnings {
		where += " AND t.promotion_action = ''"
	}
	if p.Query != "" {
		where += ` AND instr(lower(t.text), lower(?)) > 0`
		args = append(args, p.Query)
	}
	var total int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*)"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	// G202: fixed names, and every value is bound.
	q := "SELECT " + t.selectList() + where + " ORDER BY t.created_at DESC, t.rowid DESC LIMIT ? OFFSET ?" //nolint:gosec
	rows, err := s.r.QueryContext(ctx, q, append(args, min(max(p.Limit, 1), maxListLimit), max(p.Offset, 0))...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		rec, err := t.scan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, rec)
	}
	return out, total, rows.Err()
}

// MemoryIDs is the ids of a user's memories in a datapack, optionally only one agent's or one
// conversation's.
func (s *Store) MemoryIDs(ctx context.Context, datapack, userID, agentID, conversationID string) ([]string, error) {
	q := "SELECT id FROM memories WHERE datapack_id = ? AND user_id = ?"
	args := []any{datapack, userID}
	if agentID != "" {
		q += " AND agent_id = ?"
		args = append(args, agentID)
	}
	if conversationID != "" {
		q += " AND conversation_id = ?"
		args = append(args, conversationID)
	}
	rows, err := s.r.QueryContext(ctx, q+" ORDER BY rowid", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
