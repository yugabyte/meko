package cloudlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeMCP is a Meko Cloud MCP server keeping sessions (as Cloud's may), with an echo tool, that takes
// key; it counts the sessions opened. Its handler can be swapped for a new one, which has none of
// the old one's sessions.
type fakeMCP struct {
	url      string
	sessions atomic.Int32
	h        atomic.Pointer[http.Handler]
}

func newFakeMCP(t *testing.T) *fakeMCP {
	t.Helper()
	f := &fakeMCP{}
	f.renew()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), `"method":"initialize"`) {
				f.sessions.Add(1)
			}
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		(*f.h.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	f.url = ts.URL + "/mcp"
	return f
}

// renew replaces the server, as a restart of Meko Cloud's does: the sessions it had are gone.
func (f *fakeMCP) renew() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "cloud", Version: "1"},
		&mcp.ServerOptions{Instructions: "Cloud's instructions."})
	srv.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}}, nil
		})
	var h http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	f.h.Store(&h)
}

func linkedTo(t *testing.T, url string) *Service {
	t.Helper()
	s, _ := newService(t)
	if _, err := s.Link(context.Background(), Update{URL: ptr(url), WebURL: ptr("https://web.example.com"),
		Key: ptr(key)}); err != nil {
		t.Fatal(err)
	}
	return s
}

func resultText(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	tc, _ := res.Content[0].(*mcp.TextContent)
	return tc.Text
}

// Forward sends the arguments as they came on one kept session, makes a new one when Meko Cloud no
// longer has it, and drops it on a link change.
func TestForward(t *testing.T) {
	f := newFakeMCP(t)
	s := linkedTo(t, f.url)
	ctx := context.Background()
	for range 3 {
		res, err := s.Forward(ctx, "echo", json.RawMessage(`{"a":1}`))
		if err != nil || resultText(res) != `{"a":1}` {
			t.Fatalf("forward: %v %v", res, err)
		}
	}
	if n := f.sessions.Load(); n != 1 {
		t.Fatalf("sessions after three calls: %d", n)
	}
	f.renew()
	if res, err := s.Forward(ctx, "echo", nil); err != nil || resultText(res) != `{}` {
		t.Fatalf("forward after Meko Cloud lost the session: %v %v", res, err)
	}
	if n := f.sessions.Load(); n != 2 {
		t.Fatalf("sessions after a lost one: %d", n)
	}
	if _, err := s.Link(ctx, Update{Key: ptr(key), WebURL: ptr("https://other.example.com")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forward(ctx, "echo", nil); err != nil {
		t.Fatal(err)
	}
	if n := f.sessions.Load(); n != 3 {
		t.Fatalf("sessions after a link change: %d", n)
	}
	if tl := s.CloudTools(ctx); tl == nil || len(tl.Tools) != 1 || tl.Tools[0].Name != "echo" ||
		tl.Instructions != "Cloud's instructions." {
		t.Fatalf("CloudTools: %+v", tl)
	}

	if _, err := s.Unlink(ctx); err != nil {
		t.Fatal(err)
	}
	var ce *CallError
	if _, err := s.Forward(ctx, "echo", nil); !errors.As(err, &ce) || ce.Code != CodeNotLinked {
		t.Fatalf("forward unlinked: %v", err)
	}
	if tl := s.CloudTools(ctx); tl != nil {
		t.Fatalf("CloudTools unlinked: %+v", tl)
	}
}

// A key Meko Cloud refuses is CodeKeyRefused, and the key is in no error.
func TestForwardKeyRefused(t *testing.T) {
	f := newFakeMCP(t)
	s := linkedTo(t, f.url)
	s.saved(State{Set: true, Linked: true, Link: Link{URL: f.url, Key: "mko_tkn_stale_0123456789"}})
	_, err := s.Forward(context.Background(), "echo", nil)
	var ce *CallError
	if !errors.As(err, &ce) || ce.Code != CodeKeyRefused || strings.Contains(err.Error(), "stale") {
		t.Fatalf("a refused key: %v", err)
	}
}

// Pass sends a REST request as it came, with the key, and answers with Meko Cloud's reply as it is,
// but for a 401.
func TestPass(t *testing.T) {
	var got string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, `{"error":{"code":"UNAUTHORIZED"}}`, http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		got = r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("Content-Type") + " " + string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"error":{"code":"TEAPOT","message":"short and stout"}}`))
	}))
	t.Cleanup(api.Close)
	s, _ := newService(t)
	s.saved(State{Set: true, Linked: true, Link: Link{URL: "https://mcp.example.com/mcp", APIURL: api.URL + "/api/v1",
		Key: key}})
	r, err := s.Pass(context.Background(), http.MethodPost, "/datapacks/d1/share?x=1", strings.NewReader(`{"a":1}`),
		"application/json")
	if err != nil || r.Status != http.StatusTeapot || !strings.Contains(string(r.Body), "TEAPOT") ||
		r.ContentType != "application/json" {
		t.Fatalf("pass: %+v %v", r, err)
	}
	if got != `POST /api/v1/datapacks/d1/share?x=1 application/json {"a":1}` {
		t.Errorf("Meko Cloud got %q", got)
	}
	s.saved(State{Set: true, Linked: true, Link: Link{URL: "https://mcp.example.com/mcp", APIURL: api.URL + "/api/v1",
		Key: "mko_tkn_stale_0123456789"}})
	var ce *CallError
	if _, err = s.Pass(context.Background(), http.MethodGet, "/datapacks", nil, ""); !errors.As(err, &ce) ||
		ce.Code != CodeKeyRefused {
		t.Fatalf("a refused key: %v", err)
	}
}

// PostUpload takes any 2xx as a file received: Meko's link answers 204, and an S3 POST can be told
// to answer 200 or 201.
func TestPostUploadTakesAny2xx(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusCreated, http.StatusOK} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil || !strings.Contains(string(body), "name=\"key\"\r\n\r\nk\r\n") {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			w.WriteHeader(status)
		}))
		err := PostUpload(t.Context(), ts.URL, map[string]string{"key": "k"}, "a.md", strings.NewReader("# a\n"))
		ts.Close()
		if err != nil {
			t.Errorf("%d: %v", status, err)
		}
	}
}
