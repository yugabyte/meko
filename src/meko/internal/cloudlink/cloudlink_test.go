package cloudlink

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/src/meko/internal/agents"
	"github.com/yugabyte/meko/src/meko/internal/secrets"
)

func TestWebURL(t *testing.T) {
	for _, tc := range []struct{ mcp, override, want string }{
		{DefaultURL, "", DefaultWebURL},
		{"https://mcp.mekodata.ai/other", "", DefaultWebURL},
		{"https://meko.example.com/mcp", "", ""},
		{"http://127.0.0.1:7778/mcp", "", ""},
		{"https://meko.example.com/mcp", " https://web.example.com/ ", "https://web.example.com"},
		{"not a url", "", ""},
	} {
		if got := WebURL(tc.mcp, tc.override); got != tc.want {
			t.Errorf("WebURL(%q, %q) = %q, want %q", tc.mcp, tc.override, got, tc.want)
		}
	}
	if got := (Link{URL: "https://meko.example.com/mcp", WebURL: "https://web.example.com"}).EffectiveAPIURL(); got !=
		"https://web.example.com/api/v1" {
		t.Errorf("EffectiveAPIURL = %q", got)
	}
}

const key = "mko_tkn_0123456789abcd"

func newService(t *testing.T) (*Service, *[]string) {
	t.Helper()
	dir := t.TempDir()
	var calls []string
	s := &Service{
		Store: Store{File: filepath.Join(dir, "cloud.toml"), Secrets: secrets.NewFiles(dir)},
		Check: func(_ context.Context, url, k string) error {
			calls = append(calls, "check "+url)
			if k == "bad" {
				return errors.New("401 Unauthorized")
			}
			return nil
		},
		CheckAPI: func(_ context.Context, api string) error {
			calls = append(calls, "api "+api)
			if strings.Contains(api, "notmeko") {
				return errors.New("it answered 404, not like Meko's API")
			}
			return nil
		},
		Changed: func(context.Context) { calls = append(calls, "changed") },
	}
	return s, &calls
}

func ptr(s string) *string { return &s }

func TestLinkAndUnlink(t *testing.T) {
	s, calls := newService(t)
	st, err := s.Status()
	if err != nil || st.Set || st.Linked || st.KeyHint != nil {
		t.Fatalf("a new Meko: %+v, %v", st, err)
	}
	st, err = s.Link(context.Background(), Update{Key: ptr(" " + key + " ")})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Linked || st.EffectiveURL != DefaultURL || *st.KeyHint != "abcd" ||
		*st.EffectiveAPIURL != DefaultWebURL+"/api/v1" {
		t.Fatalf("linked: %+v", st)
	}
	if strings.Join(*calls, ",") != "check "+DefaultURL+",changed" {
		t.Fatalf("calls %v", *calls)
	}
	b, _ := os.ReadFile(s.Store.File)
	if strings.Contains(string(b), key) {
		t.Fatalf("cloud.toml holds the key:\n%s", b)
	}
	got, err := s.Store.Load()
	if err != nil || got.Link.Key != key || !got.Linked {
		t.Fatalf("Load = %+v, %v", got, err)
	}

	// Saving the same again changes nothing and tells no one.
	*calls = nil
	if _, err = s.Link(context.Background(), Update{Key: ptr(""), URL: ptr(DefaultURL)}); err != nil || len(*calls) != 0 {
		t.Fatalf("no change: %v, calls %v", err, *calls)
	}

	st, err = s.Unlink(context.Background())
	if err != nil || !st.Set || st.Linked || st.KeyHint != nil {
		t.Fatalf("unlinked: %+v, %v", st, err)
	}
	if _, err = s.Store.Secrets.Get(SecretName); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("the key is still kept: %v", err)
	}
	_, err = s.Link(context.Background(), Update{})
	if err == nil || !strings.Contains(err.Error(), "Enter a Meko Cloud API key") {
		t.Fatalf("linking again without a key: %v", err)
	}
}

// The extensions are told after the change is saved and the lock is released, so a slow one holds
// up neither the reply nor the next change.
func TestChangedRunsUnlocked(t *testing.T) {
	s, _ := newService(t)
	var locked []bool
	s.Changed = func(context.Context) {
		free := s.mu.TryLock()
		if free {
			s.mu.Unlock()
		}
		locked = append(locked, !free)
	}
	if _, err := s.Link(context.Background(), Update{Key: ptr(key)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Unlink(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(locked) != 2 || locked[0] || locked[1] {
		t.Fatalf("Changed ran with the lock held: %v", locked)
	}
}

// A saved change moves the agents from the link before it to the link after it, before the
// extensions hear of it, and the reply says what the move did; no change moves nothing.
func TestMovedFollowsAChange(t *testing.T) {
	s, calls := newService(t)
	s.Moved = func(_ context.Context, before, after State) agents.Moves {
		*calls = append(*calls, fmt.Sprintf("moved %t→%t %s", before.Linked, after.Linked, after.Link.Key))
		return agents.Moves{Moved: []agents.Moved{{Name: "kiro", Title: "Kiro CLI"}}}
	}
	st, err := s.Link(context.Background(), Update{Key: ptr(key)})
	if err != nil || st.Agents == nil || len(st.Agents.Moved) != 1 {
		t.Fatalf("link: %+v %v", st, err)
	}
	if _, err = s.Link(context.Background(), Update{Key: ptr("")}); err != nil {
		t.Fatal(err)
	}
	st, err = s.Unlink(context.Background())
	if err != nil || st.Agents == nil {
		t.Fatalf("unlink: %+v %v", st, err)
	}
	if got := strings.Join(*calls, ","); got != "check "+DefaultURL+",moved false→true "+key+",changed,"+
		"moved true→false ,changed" {
		t.Fatalf("calls %s", got)
	}
	if st, _ := s.Status(); st.Agents != nil {
		t.Errorf("a status says what a move did: %+v", st.Agents)
	}
}

// MoveAgents moves back the agents at the current link (its before is the link, with its key),
// holding the changes' lock, and does nothing while unlinked.
func TestMoveAgents(t *testing.T) {
	s, _ := newService(t)
	var calls []string
	s.Moved = func(_ context.Context, before, after State) agents.Moves {
		locked := !s.mu.TryLock()
		if !locked {
			s.mu.Unlock()
		}
		calls = append(calls, fmt.Sprintf("%t→%t %s locked=%t", before.Linked, after.Linked, after.Link.Key, locked))
		return agents.Moves{}
	}
	if m, err := s.MoveAgents(context.Background()); m != nil || err != nil || len(calls) != 0 {
		t.Fatalf("unlinked: %v %v %v", m, err, calls)
	}
	if _, err := s.Link(context.Background(), Update{Key: ptr(key)}); err != nil {
		t.Fatal(err)
	}
	calls = nil
	if m, err := s.MoveAgents(context.Background()); m == nil || err != nil {
		t.Fatalf("linked: %v %v", m, err)
	}
	if strings.Join(calls, ",") != "true→true "+key+" locked=true" {
		t.Fatalf("calls %v", calls)
	}
}

// A saved key goes only where it already goes, or to Meko Cloud, unless it's typed again; a typed
// web app or API URL must answer like Meko's API; a new key or server must take the check.
func TestLinkRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first Update // linked first, with key, unless empty
		next  Update
		err   string // "" when it saves
		calls string // the checks next runs
	}{
		{name: "a saved key to another server", next: Update{URL: ptr("https://evil.example.com/mcp")},
			err: "Enter the Meko API key again to use it with https://evil.example.com"},
		{name: "a saved key to another API", next: Update{APIURL: ptr("https://evil.example.com/api/v1")},
			err: "Enter the Meko API key again to use it with https://evil.example.com"},
		{name: "a typed key to another server",
			next:  Update{URL: ptr("https://meko.example.com/mcp/"), Key: ptr("other_key_0123456789")},
			calls: "check https://meko.example.com/mcp"},
		{name: "back to Meko Cloud with the saved key",
			first: Update{URL: ptr("https://meko.example.com/mcp"), Key: ptr(key)},
			next:  Update{URL: ptr("")}, calls: "check " + DefaultURL},
		{name: "a key the server refuses", next: Update{Key: ptr("bad")}, err: "didn't accept that key: 401 Unauthorized",
			calls: "check " + DefaultURL},
		{name: "a typed web app that isn't Meko's", first: Update{URL: ptr("https://meko.example.com/mcp"), Key: ptr(key)},
			next: Update{WebURL: ptr("https://meko.example.com/notmeko")}, err: "doesn't look like Meko's API",
			calls: "api https://meko.example.com/notmeko/api/v1"},
		{name: "a key with a space", next: Update{Key: ptr("two words")}, err: "no spaces or quotes"},
		{name: "not a URL", next: Update{URL: ptr("meko.example.com")}, err: "isn't a URL"},
		{name: "not http", next: Update{URL: ptr("ftp://meko.example.com")}, err: "must start with https:// or http://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, calls := newService(t)
			first := tc.first
			if first.Key == nil {
				first.Key = ptr(key)
			}
			if _, err := s.Link(context.Background(), first); err != nil {
				t.Fatal(err)
			}
			before, _ := s.Store.Load()
			*calls = nil
			_, err := s.Link(context.Background(), tc.next)
			switch {
			case tc.err == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Fatalf("err %v, want %q", err, tc.err)
			}
			var refused *Error
			if tc.err != "" && !errors.As(err, &refused) {
				t.Fatalf("not an *Error: %T", err)
			}
			checks := strings.TrimSuffix(strings.Join(*calls, ","), ",changed")
			if checks != tc.calls {
				t.Fatalf("checks %q, want %q", checks, tc.calls)
			}
			if after, _ := s.Store.Load(); tc.err != "" && after != before {
				t.Fatalf("a refused change saved: %+v", after)
			}
		})
	}
}

// CheckMCP sends the key to the MCP server and reports a refusal; CheckAPI tells Meko's envelope
// from another API's reply.
func TestChecks(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp":
			if r.Header.Get("Authorization") != "Bearer "+key {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		case "/api/v1/datapacks/incoming-shares":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":null,"error":{"code":"UNAUTHORIZED","message":"no"}}`))
		default:
			http.Error(w, `{"error":"nope"}`, http.StatusNotFound)
		}
	}))
	defer ts.Close()
	ctx := context.Background()
	if err := CheckMCP(ctx, ts.URL+"/mcp", key); err != nil {
		t.Fatalf("CheckMCP with the key: %v", err)
	}
	err := CheckMCP(ctx, ts.URL+"/mcp", "wrong_key_0123456789")
	if err == nil || strings.Contains(err.Error(), "wrong_key") {
		t.Fatalf("CheckMCP with a wrong key: %v", err)
	}
	if err := CheckAPI(ctx, ts.URL+"/api/v1/"); err != nil {
		t.Fatalf("CheckAPI on Meko's: %v", err)
	}
	if err := CheckAPI(ctx, ts.URL+"/other"); err == nil || !strings.Contains(err.Error(), "answered 404") {
		t.Fatalf("CheckAPI on another: %v", err)
	}
}
