package storeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/store"
)

const token = "internal-token"

var model = store.Embedder{Provider: "test", Model: "m", Dims: 2}

type api struct {
	t      *testing.T
	s      *Server
	dp     string
	hellos []store.Embedder
}

func newAPI(t *testing.T) *api {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "meko.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dp, err := st.EnsureDefaultDatapack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := &api{t: t, dp: dp}
	a.s = New(st, token, slog.New(slog.NewTextHandler(io.Discard, nil)), func(e store.Embedder) {
		a.hellos = append(a.hellos, e)
	})
	return a
}

// do sends a request with the token and decodes the envelope's data into out (if not nil).
func (a *api) do(method, path string, body any, out any) (int, *envelope.Error) {
	a.t.Helper()
	return a.doWith(token, method, path, body, out)
}

func (a *api) doWith(tok, method, path string, body any, out any) (int, *envelope.Error) {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	a.s.ServeHTTP(rec, req)
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *envelope.Error `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		a.t.Fatalf("%s %s: not an envelope: %s", method, path, rec.Body.String())
	}
	if out != nil && env.Error == nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			a.t.Fatalf("%s %s: %v in %s", method, path, err, env.Data)
		}
	}
	return rec.Code, env.Error
}

func (a *api) hello() {
	a.t.Helper()
	if code, e := a.do(http.MethodPost, "/v1/memory_engine/hello", model, nil); code != http.StatusOK {
		a.t.Fatalf("hello: %d %v", code, e)
	}
}

func TestNeedsTheToken(t *testing.T) {
	a := newAPI(t)
	for _, tok := range []string{"", "wrong"} {
		code, e := a.doWith(tok, http.MethodPost, "/v1/memory_engine/hello", model, nil)
		if code != http.StatusUnauthorized || e.Code != "UNAUTHORIZED" {
			t.Errorf("token %q: %d %v", tok, code, e)
		}
	}
}

func TestHello(t *testing.T) {
	a := newAPI(t)
	code, e := a.do(http.MethodPost, "/v1/collections/"+a.dp+"/search",
		map[string]any{"vector": []float32{1, 0}, "top_k": 1}, nil)
	if code != http.StatusServiceUnavailable || e.Code != "NO_VECTORS" {
		t.Errorf("search before the hello: %d %v", code, e)
	}
	a.hello()
	a.hello() // the same model again
	if len(a.hellos) != 2 || a.hellos[0] != model {
		t.Errorf("hellos heard: %v", a.hellos)
	}
	other := store.Embedder{Provider: "test", Model: "other", Dims: 2}
	code, e = a.do(http.MethodPost, "/v1/memory_engine/hello", other, nil)
	if code != http.StatusConflict || e.Code != "MODEL_CHANGED" {
		t.Errorf("another model: %d %v", code, e)
	}
	code, _ = a.do(http.MethodPost, "/v1/memory_engine/hello", store.Embedder{Model: "x"}, nil)
	if code != http.StatusBadRequest {
		t.Errorf("a hello with no dimension: %d", code)
	}
}

func TestVectorStoreRoutes(t *testing.T) {
	a := newAPI(t)
	a.hello()
	col := "/v1/collections/" + a.dp
	code, e := a.do(http.MethodPost, col+"/insert", map[string]any{
		"ids": []string{"m1", "m2", "m3"}, "vectors": [][]float32{{1, 0}, {0, 1}, {0, 1}},
		"payloads": []map[string]any{
			{"data": "rate limit is 500", "textLemmatized": "rate limit 500", "user_id": "u"},
			{"data": "bananas", "user_id": "u"},
			// Another user's, so "limit" is in under half the rows (keyword search drops the rest).
			{"data": "mangoes", "user_id": "v"},
		},
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("insert: %d %v", code, e)
	}

	var res struct {
		Results []store.Record `json:"results"`
		Total   int            `json:"total"`
	}
	a.do(http.MethodPost, col+"/search", map[string]any{"vector": []float32{1, 0}, "top_k": 1,
		"filters": map[string]any{"user_id": "u"}}, &res)
	if len(res.Results) != 1 || res.Results[0].ID != "m1" || *res.Results[0].Score < 0.99 {
		t.Errorf("search: %+v", res.Results)
	}
	a.do(http.MethodPost, col+"/keyword-search", map[string]any{"query": "limit", "top_k": 5}, &res)
	if len(res.Results) != 1 || res.Results[0].ID != "m1" || *res.Results[0].Score <= 0 {
		t.Errorf("keyword search: %+v", res.Results)
	}
	a.do(http.MethodPost, col+"/list", map[string]any{"filters": map[string]any{"user_id": "u"}, "top_k": 1}, &res)
	if len(res.Results) != 1 || res.Total != 2 {
		t.Errorf("list: %d rows, total %d", len(res.Results), res.Total)
	}

	var rec *store.Record
	a.do(http.MethodGet, col+"/vectors/m1", nil, &rec)
	if rec == nil || rec.Payload["data"] != "rate limit is 500" {
		t.Errorf("get: %+v", rec)
	}
	a.do(http.MethodGet, col+"/vectors/nope", nil, &rec)
	if rec != nil {
		t.Errorf("get of a missing id: %+v, want null", rec)
	}
	if code, _ = a.do(http.MethodPut, col+"/vectors/m1", map[string]any{"vector": []float32{0, 1},
		"payload": map[string]any{"data": "new"}}, nil); code != http.StatusOK {
		t.Errorf("update: %d", code)
	}
	if code, e = a.do(http.MethodPut, col+"/vectors/nope", map[string]any{"vector": []float32{0, 1},
		"payload": map[string]any{"data": "x"}}, nil); code != http.StatusNotFound {
		t.Errorf("update of a missing id: %d %v", code, e)
	}
	if code, _ = a.do(http.MethodDelete, col+"/vectors/m1", nil, nil); code != http.StatusOK {
		t.Errorf("delete: %d", code)
	}
	if code, e = a.do(http.MethodDelete, col, nil, nil); code != http.StatusForbidden {
		t.Errorf("deleteCol: %d %v", code, e)
	}

	// Refusals: the API's own checks, and the store's, whose reason the reply carries.
	for _, c := range []struct {
		path   string
		body   any
		want   int
		reason string // in the reply's message when the store refused it
	}{
		{col + "/search", map[string]any{"vector": []float32{1, 0, 0}, "top_k": 1}, http.StatusBadRequest,
			"bad vector: it has 3 dimensions"},
		{col + "/list", map[string]any{"filters": map[string]any{"OR": "x"}}, http.StatusBadRequest,
			"bad filter: OR needs a list"},
		{"/v1/collections/nope/insert", map[string]any{"ids": []string{"x"}, "vectors": [][]float32{{1, 0}},
			"payloads": []map[string]any{{"data": "x"}}}, http.StatusNotFound, ""},
		{col + "/insert", map[string]any{"ids": []string{"x"}, "vectors": [][]float32{},
			"payloads": []map[string]any{}}, http.StatusBadRequest, ""},
		{"/v1/collections/_entities/list", map[string]any{}, http.StatusBadRequest, ""},
	} {
		code, e := a.do(http.MethodPost, c.path, c.body, nil)
		if code != c.want || e == nil {
			t.Errorf("%s: %d %v, want %d", c.path, code, e, c.want)
			continue
		}
		if c.reason != "" && (e.Code != "BAD_REQUEST" || !strings.Contains(e.Message, c.reason)) {
			t.Errorf("%s: %s %q, want BAD_REQUEST with %q", c.path, e.Code, e.Message, c.reason)
		}
	}
}

func TestEntitiesCollection(t *testing.T) {
	a := newAPI(t)
	a.hello()
	col := "/v1/collections/" + a.dp + "_entities"
	a.do(http.MethodPost, col+"/insert", map[string]any{"ids": []string{"e1"}, "vectors": [][]float32{{1, 0}},
		"payloads": []map[string]any{{"data": "AgentK", "linkedMemoryIds": []string{"m1"}}}}, nil)
	var res struct {
		Results []store.Record `json:"results"`
	}
	a.do(http.MethodPost, col+"/search", map[string]any{"vector": []float32{1, 0}, "top_k": 5}, &res)
	if len(res.Results) != 1 || res.Results[0].ID != "e1" {
		t.Errorf("entity search: %+v", res.Results)
	}
	a.do(http.MethodPost, col+"/keyword-search", map[string]any{"query": "agentk"}, &res)
	if len(res.Results) != 0 {
		t.Errorf("keyword search on entities: %+v", res.Results)
	}
	var rec store.Record
	a.do(http.MethodGet, "/v1/collections/"+a.dp+"/vectors/e1", nil, &rec)
	if rec.ID != "" {
		t.Error("an entity was found among memories")
	}
}

func TestHistoryRoutes(t *testing.T) {
	a := newAPI(t)
	v := "text"
	if code, e := a.do(http.MethodPost, "/v1/history/"+a.dp, map[string]any{"entries": []store.HistoryEntry{
		{MemoryID: "m1", NewValue: &v, Action: "ADD", CreatedAt: "2026-10-17T00:00:00.000Z"},
	}}, nil); code != http.StatusOK {
		t.Fatalf("add history: %d %v", code, e)
	}
	var h []store.HistoryEntry
	a.do(http.MethodGet, "/v1/history/"+a.dp+"/m1", nil, &h)
	if len(h) != 1 || h[0].Action != "ADD" {
		t.Errorf("history: %+v", h)
	}
}

func TestListen(t *testing.T) {
	// A short folder: t.TempDir's paths can pass the 104-byte socket path limit on macOS.
	dir, err := os.MkdirTemp("/tmp", "meko-sa-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "store.sock")
	if err = os.WriteFile(path, []byte("left by a killed server"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Type() != os.ModeSocket || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want a socket with 0600", fi.Mode())
	}
}
