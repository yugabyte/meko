package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

var testModel = Embedder{Provider: "test", Model: "unit-3d", Dims: 3}

// openMem is a migrated store with vector tables 3 wide and two datapacks, a and b.
func openMem(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureVectors(ctx, testModel); err != nil {
		t.Fatal(err)
	}
	for _, dp := range []string{"a", "b"} {
		if _, err := s.w.Exec("INSERT INTO datapacks(id, name, created_at, updated_at) VALUES (?, ?, '', '')",
			dp, "dp-"+dp); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func unit(v ...float32) []float32 {
	var n float64
	for _, f := range v {
		n += float64(f * f)
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(n))
	}
	return v
}

func insert(t *testing.T, s *Store, k Kind, dp, id string, vec []float32, p map[string]any) {
	t.Helper()
	if err := s.InsertVectors(context.Background(), k, dp, []string{id}, [][]float32{vec},
		[]map[string]any{p}); err != nil {
		t.Fatal(err)
	}
}

func ids(rs []Record) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return strings.Join(out, ",")
}

func TestEnsureVectors(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SearchVectors(ctx, Memories, "a", unit(1, 0, 0), 1, nil); !errors.Is(err, ErrNoVectors) {
		t.Fatalf("search before the first hello: %v", err)
	}
	if _, ok, _ := s.Embedder(ctx); ok {
		t.Fatal("an embedder is recorded before the first hello")
	}
	for i := 0; i < 2; i++ { // the same model again is a no-op
		if err := s.EnsureVectors(ctx, testModel); err != nil {
			t.Fatal(err)
		}
	}
	if e, ok, err := s.Embedder(ctx); err != nil || !ok || e != testModel {
		t.Fatalf("recorded %+v %v %v", e, ok, err)
	}
	other := Embedder{Provider: "test", Model: "other", Dims: 3}
	var changed *ModelChangedError
	if err := s.EnsureVectors(ctx, other); !errors.As(err, &changed) || changed.Have != testModel {
		t.Fatalf("a different model: %v", err)
	}
	if err := s.EnsureVectors(ctx, Embedder{Model: "x"}); err == nil {
		t.Fatal("a model with no dimension was accepted")
	}
	// A store opened later on the same file finds the tables.
	s2, err := Open(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if d, err := s2.vectorDims(ctx); err != nil || d != 3 {
		t.Fatalf("dims %d, %v", d, err)
	}
}

func TestMemoryPayloadRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	insert(t, s, Memories, "a", "m1", unit(1, 0, 0), map[string]any{
		"data": "Sessions survive restarts", "textLemmatized": "session surviv restart", "hash": "h1",
		"user_id": "u", "agent_id": "ag", "meko_conversation_id": "c1", "role": "user",
		"createdAt": "2026-10-17T10:00:00.000Z", "updatedAt": "2026-10-17T10:00:00.000Z",
		"created_at":   "2026-09-01", // a backdated memory: wins over createdAt
		"attributedTo": "user", "custom": map[string]any{"n": 1.0},
	})
	r, err := s.GetVector(ctx, Memories, "a", "m1")
	if err != nil || r == nil {
		t.Fatalf("get: %v %v", r, err)
	}
	want := map[string]any{
		"data": "Sessions survive restarts", "textLemmatized": "session surviv restart", "hash": "h1",
		"user_id": "u", "agent_id": "ag", "meko_conversation_id": "c1", "role": "user",
		"createdAt": "2026-09-01T00:00:00.000Z", "updatedAt": "2026-10-17T10:00:00.000Z",
		"attributedTo": "user", "custom": map[string]any{"n": 1.0},
	}
	got, _ := json.Marshal(r.Payload)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Errorf("payload\n got %s\nwant %s", got, exp)
	}
	if r, _ := s.GetVector(ctx, Memories, "b", "m1"); r != nil {
		t.Error("another datapack's memory was found")
	}
	if err := s.InsertVectors(ctx, Memories, "b", []string{"m1"}, [][]float32{unit(1, 0, 0)},
		[]map[string]any{{"data": "x"}}); !errors.Is(err, ErrOtherDatapack) {
		t.Errorf("reusing another datapack's id: %v", err)
	}
	if err := s.InsertVectors(ctx, Memories, "nope", []string{"m9"}, [][]float32{unit(1, 0, 0)},
		[]map[string]any{{"data": "x"}}); !errors.Is(err, ErrNoDatapack) {
		t.Errorf("an unknown datapack: %v", err)
	}
	if err := s.InsertVectors(ctx, Memories, "a", []string{"m9"}, [][]float32{{1, 0}},
		[]map[string]any{{"data": "x"}}); err == nil {
		t.Error("a vector of the wrong width was accepted")
	}
}

func TestUpdateKeepsCreatedAt(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	insert(t, s, Memories, "a", "m1", unit(1, 0, 0), map[string]any{
		"data": "old", "user_id": "u", "createdAt": "2026-10-01T00:00:00.000Z",
	})
	err := s.UpdateVector(ctx, Memories, "a", "m1", unit(0, 1, 0), map[string]any{"data": "new", "user_id": "u"})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetVector(ctx, Memories, "a", "m1")
	if r.Payload["data"] != "new" || r.Payload["createdAt"] != "2026-10-01T00:00:00.000Z" {
		t.Errorf("after update: %v", r.Payload)
	}
	updated, _ := r.Payload["updatedAt"].(string)
	if ts, perr := time.Parse(jsTime, updated); perr != nil || time.Since(ts) > time.Minute {
		t.Errorf("updatedAt %v %v", r.Payload["updatedAt"], perr)
	}
	res, err := s.SearchVectors(ctx, Memories, "a", unit(0, 1, 0), 1, nil)
	if err != nil || len(res) != 1 || *res[0].Score < 0.999 {
		t.Errorf("the new vector isn't searched: %v %v", res, err)
	}
	if hits, _ := s.KeywordSearch(ctx, "a", "old", 5, nil); len(hits) != 0 {
		t.Errorf("the old text is still in the keyword index: %v", hits)
	}
	err = s.UpdateVector(ctx, Memories, "b", "m1", unit(0, 1, 0), map[string]any{"data": "x"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("update in another datapack: %v", err)
	}
}

func TestSearchVectors(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	insert(t, s, Memories, "a", "near", unit(1, 0.1, 0), map[string]any{"data": "n", "user_id": "u", "role": "user"})
	insert(t, s, Memories, "a", "far", unit(0, 1, 0), map[string]any{"data": "f", "user_id": "u", "role": "user"})
	insert(t, s, Memories, "a", "other-user", unit(1, 0, 0), map[string]any{"data": "o", "user_id": "v"})
	insert(t, s, Memories, "a", "assistant", unit(1, 0.05, 0), map[string]any{"data": "s", "user_id": "u",
		"role": "assistant"})
	insert(t, s, Memories, "b", "other-dp", unit(1, 0, 0), map[string]any{"data": "d", "user_id": "u"})

	q := unit(1, 0, 0)
	res, err := s.SearchVectors(ctx, Memories, "a", q, 10, Filter{"user_id": "u"})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res); got != "assistant,near,far" {
		t.Errorf("pushed-down filter: %s", got)
	}
	if s0 := *res[0].Score; s0 < 0.99 || s0 > 1.0001 {
		t.Errorf("similarity %f", s0)
	}
	if s2 := *res[2].Score; math.Abs(s2) > 0.001 {
		t.Errorf("an orthogonal vector's similarity is %f, want 0", s2)
	}
	res, _ = s.SearchVectors(ctx, Memories, "a", q, 10, Filter{"user_id": "u", "role": "user"})
	if got := ids(res); got != "near,far" {
		t.Errorf("filter after the scan: %s", got)
	}
	res, _ = s.SearchVectors(ctx, Memories, "a", q, 1, Filter{"OR": []any{
		map[string]any{"user_id": "v"}, map[string]any{"role": "assistant"}}})
	if got := ids(res); got != "other-user" {
		t.Errorf("OR, top 1: %s", got)
	}
}

func TestKeywordSearch(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	insert(t, s, Memories, "a", "m1", unit(1, 0, 0), map[string]any{"data": "Rate limit is 500",
		"textLemmatized": "rate limit 500", "user_id": "u"})
	insert(t, s, Memories, "a", "m2", unit(1, 0, 0), map[string]any{"data": "Limits apply per key",
		"textLemmatized": "limit appli per key", "user_id": "u"})
	insert(t, s, Memories, "a", "m3", unit(1, 0, 0), map[string]any{"data": "no lemmas: MIN_MEMORY_SCORE",
		"user_id": "u"})
	insert(t, s, Memories, "b", "m4", unit(1, 0, 0), map[string]any{"data": "rate limit", "user_id": "u"})
	for i := 0; i < 4; i++ { // so no query term is in half the rows or more (those are dropped)
		insert(t, s, Memories, "b", fmt.Sprintf("filler%d", i), unit(1, 0, 0), map[string]any{"data": "unrelated"})
	}

	res, err := s.KeywordSearch(ctx, "a", "rate limit", 10, Filter{"user_id": "u"})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res); got != "m1,m2" {
		t.Errorf("hits %s", got)
	}
	if *res[0].Score <= *res[1].Score || *res[1].Score <= 0 {
		t.Errorf("scores %f %f: want positive, best first", *res[0].Score, *res[1].Score)
	}
	// Terms are quoted, so FTS5 syntax is just text; an empty query matches nothing.
	for _, q := range []string{`limit" OR "x`, "NEAR(limit", "a:limit", "AND", "*", ""} {
		if _, err := s.KeywordSearch(ctx, "a", q, 10, nil); err != nil {
			t.Errorf("query %q: %v", q, err)
		}
	}
	if res, _ := s.KeywordSearch(ctx, "a", "min_memory_score", 10, nil); ids(res) != "m3" {
		t.Errorf("text without lemmas isn't indexed: %s", ids(res))
	}
	if res, _ := s.KeywordSearch(ctx, "a", "rate", 10, Filter{"user_id": "v"}); len(res) != 0 {
		t.Errorf("filter ignored: %s", ids(res))
	}
}

func TestListAndFilters(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	for i, p := range []map[string]any{
		{"data": "one", "user_id": "u", "n": 1.0, "tag": "Alpha"},
		{"data": "two", "user_id": "u", "n": 2.0, "tag": "beta"},
		{"data": "three", "user_id": "v", "n": 3.0},
	} {
		insert(t, s, Memories, "a", []string{"m1", "m2", "m3"}[i], unit(1, 0, 0), p)
	}
	cases := []struct {
		f    Filter
		want string
	}{
		{nil, "m1,m2,m3"},
		{Filter{"user_id": "*"}, "m1,m2,m3"},
		{Filter{"user_id": "u"}, "m1,m2"},
		{Filter{"user_id": []any{"v", "w"}}, "m3"},
		{Filter{"n": map[string]any{"gte": 2.0}}, "m2,m3"},
		{Filter{"n": map[string]any{"lt": 2.0}}, "m1"},
		{Filter{"n": map[string]any{"ne": 2.0}}, "m1,m3"},
		{Filter{"tag": map[string]any{"nin": []any{"beta"}}}, "m1,m3"},
		{Filter{"tag": map[string]any{"in": []any{}}}, ""},
		{Filter{"tag": map[string]any{"icontains": "ALP"}}, "m1"},
		{Filter{"data": map[string]any{"contains": "o"}}, "m1,m2"},
		{Filter{"tag": nil}, "m3"},
		{Filter{"$or": []any{map[string]any{"n": 1.0}, map[string]any{"n": 3.0}}}, "m1,m3"},
		{Filter{"AND": []any{map[string]any{"user_id": "u"}, map[string]any{"n": 2.0}}}, "m2"},
		{Filter{"NOT": []any{map[string]any{"user_id": "u"}}}, "m3"},
		{Filter{"OR": []any{}}, ""},
		{Filter{`we"ird`: "x"}, ""},
	}
	for _, c := range cases {
		res, total, err := s.ListVectors(ctx, Memories, "a", c.f, 0)
		if err != nil {
			t.Errorf("%v: %v", c.f, err)
			continue
		}
		if got := ids(res); got != c.want || total != len(res) {
			t.Errorf("%v: got %s (total %d), want %s", c.f, got, total, c.want)
		}
	}
	res, total, _ := s.ListVectors(ctx, Memories, "a", nil, 2)
	if len(res) != 2 || total != 3 {
		t.Errorf("limit 2: %d rows of %d", len(res), total)
	}
	for _, bad := range []Filter{{"OR": "x"}, {"n": map[string]any{"between": 1.0}}, {"n": map[string]any{}},
		{"OR": []any{}, "$or": []any{}}} {
		if _, _, err := s.ListVectors(ctx, Memories, "a", bad, 0); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}

func TestEntities(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	insert(t, s, Entities, "a", "e1", unit(1, 0, 0), map[string]any{"data": "Kode", "entityType": "PROPER",
		"linkedMemoryIds": []any{"m1", "m2"}, "user_id": "u"})
	r, err := s.GetVector(ctx, Entities, "a", "e1")
	if err != nil || r == nil {
		t.Fatal(r, err)
	}
	if got, _ := json.Marshal(r.Payload); string(got) !=
		`{"data":"Kode","entityType":"PROPER","linkedMemoryIds":["m1","m2"],"user_id":"u"}` {
		t.Errorf("entity payload %s", got)
	}
	res, err := s.SearchVectors(ctx, Entities, "a", unit(1, 0, 0), 5, Filter{"user_id": "u"})
	if err != nil || ids(res) != "e1" {
		t.Errorf("entity search %v %v", ids(res), err)
	}
	if res, _, _ := s.ListVectors(ctx, Entities, "a", Filter{"data": "Kode"}, 0); ids(res) != "e1" {
		t.Errorf("entity by text: %s", ids(res))
	}
}

// Every way a row goes takes its vector and keyword index entry along.
func TestDeletesTakeVectors(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	count := func(table string) int {
		var n int
		if err := s.r.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	insert(t, s, Memories, "a", "m1", unit(1, 0, 0), map[string]any{"data": "alpha"})
	insert(t, s, Memories, "a", "m2", unit(1, 0, 0), map[string]any{"data": "beta"})
	insert(t, s, Memories, "b", "m3", unit(1, 0, 0), map[string]any{"data": "gamma"})
	for i := 0; i < 3; i++ { // distinct words, so no term is in half the rows or more
		insert(t, s, Memories, "b", fmt.Sprintf("f%d", i), unit(1, 0, 0), map[string]any{"data": fmt.Sprintf("filler%d", i)})
	}
	insert(t, s, Entities, "a", "e1", unit(1, 0, 0), map[string]any{"data": "Alpha"})
	if err := s.DeleteVector(ctx, Memories, "b", "m1"); err != nil || count("memories_vec") != 6 {
		t.Fatalf("delete from the wrong datapack touched it: %v", err)
	}
	if err := s.DeleteVector(ctx, Memories, "a", "m1"); err != nil {
		t.Fatal(err)
	}
	if n := count("memories_vec"); n != 5 {
		t.Errorf("%d vectors after a delete, want 5", n)
	}
	if res, _ := s.KeywordSearch(ctx, "a", "alpha", 10, nil); len(res) != 0 {
		t.Errorf("a deleted memory's keyword hits: %s", ids(res))
	}
	if res, _ := s.KeywordSearch(ctx, "a", "beta", 10, nil); ids(res) != "m2" {
		t.Errorf("keyword hits after a delete: %s", ids(res))
	}
	if _, err := s.w.Exec("DELETE FROM datapacks WHERE id = 'a'"); err != nil {
		t.Fatal(err)
	}
	if m, e := count("memories_vec"), count("entities_vec"); m != 4 || e != 0 {
		t.Errorf("after deleting a datapack: %d memory and %d entity vectors, want 4 and 0", m, e)
	}
	if res, _ := s.KeywordSearch(ctx, "b", "gamma", 10, nil); ids(res) != "m3" {
		t.Errorf("the other datapack's keyword index: %s", ids(res))
	}
}

func TestHistory(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	newV, oldV := "new", "old"
	if err := s.AddHistory(ctx, "a", []HistoryEntry{
		{MemoryID: "m1", NewValue: &oldV, Action: "ADD", CreatedAt: "t1"},
		{MemoryID: "m1", PreviousValue: &oldV, NewValue: &newV, Action: "UPDATE", CreatedAt: "t1", UpdatedAt: &newV},
		{MemoryID: "m2", NewValue: &newV, Action: "ADD", CreatedAt: "t2"},
	}); err != nil {
		t.Fatal(err)
	}
	h, err := s.History(ctx, "a", "m1")
	if err != nil || len(h) != 2 || h[1].Action != "UPDATE" || *h[1].PreviousValue != "old" {
		t.Fatalf("history %+v %v", h, err)
	}
	if h, _ := s.History(ctx, "b", "m1"); len(h) != 0 {
		t.Error("history crossed datapacks")
	}
	if err := s.AddHistory(ctx, "nope", nil); !errors.Is(err, ErrNoDatapack) {
		t.Errorf("unknown datapack: %v", err)
	}
}

// A term in half or more of the rows weighs nothing in bm25(), so it's dropped from the query.
func TestKeywordSearchDropsCommonTerms(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	for i, text := range []string{"common alpha", "common beta", "common gamma", "delta"} {
		insert(t, s, Memories, "a", fmt.Sprintf("m%d", i), unit(1, 0, 0), map[string]any{"data": text})
	}
	if res, _ := s.KeywordSearch(ctx, "a", "common", 10, nil); len(res) != 0 {
		t.Errorf("a term in 3 of 4 rows matched: %s", ids(res))
	}
	if res, _ := s.KeywordSearch(ctx, "a", "common beta", 10, nil); ids(res) != "m1" {
		t.Errorf("with a rare term: %s", ids(res))
	}
	if got := queryTerms("B a  b \"x\" ?"); strings.Join(got, ",") != "\"x\",a,b" {
		t.Errorf("terms %q", got)
	}
}

// A term in exactly half the rows is dropped too: bm25()'s IDF is log(1) there, clamped to 1e-6.
func TestKeywordSearchDropsHalfTerms(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	for i, text := range []string{"half one", "half two", "three", "four"} {
		insert(t, s, Memories, "a", fmt.Sprintf("m%d", i), unit(1, 0, 0), map[string]any{"data": text})
	}
	if res, _ := s.KeywordSearch(ctx, "a", "half", 10, nil); len(res) != 0 {
		t.Errorf("a term in 2 of 4 rows matched: %s", ids(res))
	}
}

// Rows are counted in the text column only: a term equal to a piece of a big datapack's id (which
// unicode61 splits at the hyphens) still counts as rare when the text rarely has it.
func TestKeywordSearchIgnoresDatapackIDTokens(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	q := "INSERT INTO datapacks(id, name, created_at, updated_at) VALUES ('d1d97fc0-2026', 'big', '', '')"
	if _, err := s.w.Exec(q); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		insert(t, s, Memories, "d1d97fc0-2026", fmt.Sprintf("big%d", i), unit(1, 0, 0), map[string]any{"data": "unrelated"})
	}
	insert(t, s, Memories, "a", "m1", unit(1, 0, 0), map[string]any{"data": "the port is 2026"})
	if res, _ := s.KeywordSearch(ctx, "a", "2026", 10, nil); ids(res) != "m1" {
		t.Errorf("a term that's also a datapack id token: %s", ids(res))
	}
}

// A filter key matches the same rows whether it runs inside the vec0 scan or after it.
func TestFilterKeysMatchOnEveryPath(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	insert(t, s, Memories, "a", "m1", unit(1, 0, 0), map[string]any{"data": "x", "meko_conversation_id": "c1"})
	insert(t, s, Memories, "a", "m2", unit(1, 0.1, 0), map[string]any{"data": "x", "conversation_id": "c1"})
	for _, f := range []Filter{{"meko_conversation_id": "c1"}, {"run_id": "c1"}, {"conversation_id": "c1"}} {
		knn, _ := s.SearchVectors(ctx, Memories, "a", unit(1, 0, 0), 10, f)
		list, _, _ := s.ListVectors(ctx, Memories, "a", f, 0)
		withRest := Filter{"data": "x"}
		for k, v := range f {
			withRest[k] = v
		}
		after, _ := s.SearchVectors(ctx, Memories, "a", unit(1, 0, 0), 10, withRest)
		if ids(knn) != ids(list) || ids(knn) != ids(after) || len(knn) != 1 {
			t.Errorf("%v: knn %s, list %s, filtered after the scan %s", f, ids(knn), ids(list), ids(after))
		}
	}
}

// Lists go by created_at, so a backdated memory comes before newer ones it was added after.
func TestListOrderFollowsCreatedAt(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	insert(t, s, Memories, "a", "new", unit(1, 0, 0), map[string]any{"data": "x", "createdAt": "2026-10-17T00:00:00.000Z"})
	insert(t, s, Memories, "a", "old", unit(1, 0, 0), map[string]any{"data": "y", "created_at": "2026-09-01"})
	if res, _, _ := s.ListVectors(ctx, Memories, "a", nil, 0); ids(res) != "old,new" {
		t.Errorf("order %s", ids(res))
	}
}
