package cloudlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeMCP is a Meko Cloud MCP server keeping sessions (as Cloud's may), with an echo tool, that takes
// key; it counts the sessions opened. Its handler can be swapped for a new one, which has none of
// the old one's sessions.
type fakeMCP struct {
	url      string
	sessions atomic.Int32
	h        atomic.Pointer[http.Handler]
	revoked  atomic.Bool // refuse the key, as Meko Cloud does once it's revoked
}

func newFakeMCP(t *testing.T) *fakeMCP {
	t.Helper()
	f := &fakeMCP{}
	f.renew()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key || f.revoked.Load() {
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
// but for a 401 and a 5xx.
func TestPass(t *testing.T) {
	var got string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, `{"error":{"code":"UNAUTHORIZED"}}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/datapacks/down":
			http.Error(w, "upstream gone", http.StatusServiceUnavailable)
			return
		case "/api/v1/datapacks/broken":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"INTERNAL","message":"database is busy"}}`))
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
	var ce *CallError
	for path, msg := range map[string]string{"/datapacks/down": "Meko Cloud answered 503 Service Unavailable.",
		"/datapacks/broken": "Meko Cloud: database is busy."} {
		if _, err = s.Pass(context.Background(), http.MethodGet, path, nil, ""); !errors.As(err, &ce) ||
			ce.Code != CodeCallFailed || ce.Status != 0 || ce.Error() != msg {
			t.Errorf("Meko Cloud's 5xx on %s: %v", path, err)
		}
	}
	s.saved(State{Set: true, Linked: true, Link: Link{URL: "https://mcp.example.com/mcp", APIURL: api.URL + "/api/v1",
		Key: "mko_tkn_stale_0123456789"}})
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

// A call or a list read that started under a link a change has since replaced can't connect it again:
// the session is the new link's, and a read in progress no longer holds up the new link's.
func TestLinkChangeStalesSessions(t *testing.T) {
	f := newFakeMCP(t)
	s := linkedTo(t, f.url)
	l, gen, err := s.current()
	if err != nil {
		t.Fatal(err)
	}
	s.remote.toolsMu.Lock()
	s.remote.reading = make(chan struct{})
	s.remote.toolsMu.Unlock()
	if _, err = s.Link(context.Background(), Update{Key: ptr(key), WebURL: ptr("https://other.example.com")}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.remote.session(context.Background(), l, gen); !errors.Is(err, errLinkChanged) {
		t.Fatalf("a session for the old link: %v", err)
	}
	s.remote.toolsMu.Lock()
	reading := s.remote.reading
	s.remote.toolsMu.Unlock()
	if reading != nil {
		t.Fatal("the old link's read still holds up the new one's")
	}
	if tl := s.CloudTools(context.Background()); tl == nil || len(tl.Tools) != 1 {
		t.Fatalf("the new link's tools: %+v", tl)
	}
}

// Pass takes the credentials out of a JSON reply, at any depth, keeping the rest as it was, and
// refuses a reply too large to pass whole.
func TestPassDropsCredentials(t *testing.T) {
	reply := `{"data":[{"datapack_id":"d1","datapack_name":"notes","count":12345678901234567890,` +
		`"datapack_ysql_connection_string":"postgresql://db.example.com/dp","db_access":{"user_name":"u","password":"p"},` +
		`"mcp_server":{"url":"https://mcp.example.com/mcp","api_key":"mko_tkn_x"}}]}`
	big := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if big {
			_, _ = w.Write(bytes.Repeat([]byte("x"), maxCallBytes+1))
			return
		}
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(api.Close)
	s, _ := newService(t)
	s.saved(State{Set: true, Linked: true, Link: Link{URL: "https://mcp.example.com/mcp", APIURL: api.URL + "/api/v1",
		Key: key}})
	r, err := s.Pass(context.Background(), http.MethodGet, "/datapacks", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	body := string(r.Body)
	for _, gone := range []string{"postgresql", "db_access", "password", "mko_tkn_x", "api_key"} {
		if strings.Contains(body, gone) {
			t.Errorf("the reply still holds %q: %s", gone, body)
		}
	}
	for _, kept := range []string{`"datapack_name":"notes"`, `"count":12345678901234567890`,
		`"url":"https://mcp.example.com/mcp"`} {
		if !strings.Contains(body, kept) {
			t.Errorf("the reply lost %s: %s", kept, body)
		}
	}
	big = true
	var ce *CallError
	if _, err = s.Pass(context.Background(), http.MethodGet, "/datapacks", nil, ""); !errors.As(err, &ce) ||
		ce.Code != CodeCallFailed {
		t.Fatalf("a reply over the limit: %v", err)
	}
}

// A key revoked under a session already open is CodeKeyRefused, and the session is dropped, so the
// call after the key works again connects anew.
func TestForwardKeyRevoked(t *testing.T) {
	f := newFakeMCP(t)
	s := linkedTo(t, f.url)
	ctx := context.Background()
	if _, err := s.Forward(ctx, "echo", nil); err != nil {
		t.Fatal(err)
	}
	f.revoked.Store(true)
	var ce *CallError
	if _, err := s.Forward(ctx, "echo", nil); !errors.As(err, &ce) || ce.Code != CodeKeyRefused {
		t.Fatalf("a revoked key: %v", err)
	}
	f.revoked.Store(false)
	if _, err := s.Forward(ctx, "echo", nil); err != nil {
		t.Fatalf("after the key works again: %v", err)
	}
	if n := f.sessions.Load(); n != 2 {
		t.Fatalf("sessions: %d", n)
	}
}

// A connect that hangs (Meko Cloud black-holed) holds up neither an unlink nor a read of the link.
func TestHungConnectDoesntHoldUpALinkChange(t *testing.T) {
	hang := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(hang); ts.Close() })
	s := linkedTo(t, ts.URL+"/mcp")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _, _ = s.Forward(ctx, "echo", nil) }()
	time.Sleep(200 * time.Millisecond) // the connect is under way
	done := make(chan error, 1)
	go func() { _, err := s.Unlink(context.Background()); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the unlink waited on the hung connect")
	}
	if linked, err := s.IsLinked(); err != nil || linked {
		t.Fatalf("after the unlink: %v %v", linked, err)
	}
}

// A save that fails partway drops the link as last read, so the next read takes it from the store.
func TestFailedSaveForgetsTheCachedLink(t *testing.T) {
	f := newFakeMCP(t)
	s := linkedTo(t, f.url)
	gen := s.gen
	if err := os.Remove(s.Store.File); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.Store.File, 0o700); err != nil { // cloud.toml can't be written now
		t.Fatal(err)
	}
	if _, err := s.Link(context.Background(), Update{Key: ptr("mko_tkn_other_0123456789")}); err == nil {
		t.Fatal("a link whose cloud.toml can't be written was saved")
	}
	s.stateMu.Lock()
	cached, newGen := s.state, s.gen
	s.stateMu.Unlock()
	if cached != nil || newGen == gen {
		t.Fatalf("after a failed save: state %+v, gen %d (was %d)", cached, newGen, gen)
	}
}

// Account is pinned: Kode works out the same hash for an older Meko's link (core.ts
// mekoCloudAccount), so the two must agree.
func TestAccount(t *testing.T) {
	if got := (Link{URL: "https://mcp.example.com/mcp", Key: "mko_tkn_x"}).Account(); got != "8adf4764ab826787" {
		t.Errorf("Account = %q", got)
	}
	if got := (Link{URL: "https://mcp.example.com/mcp"}).Account(); got != "" {
		t.Errorf("Account with no key = %q", got)
	}
}
