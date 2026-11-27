package cloudlink

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSignInCloud is Meko Cloud's half of sign-in to a linked Meko: allow issues a code the way
// its /meko-signin page does when the person clicks Allow, and the exchange route checks the key,
// the verifier against the challenge and return_to, and uses the code up whatever happens.
type fakeSignInCloud struct {
	mu        sync.Mutex
	codes     map[string]issuedCode
	exchanges int
	status    int // when set, the exchange answers this with no envelope (a Meko Cloud without it)
}

type issuedCode struct {
	challenge, returnTo string
	owner               bool // issued to the account that owns the key
}

func newFakeSignInCloud(t *testing.T) (*fakeSignInCloud, *httptest.Server) {
	t.Helper()
	f := &fakeSignInCloud{codes: map[string]issuedCode{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/meko-signin/exchange" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "UNAUTHORIZED"}})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.exchanges++
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		var body struct{ Code, CodeVerifier, ReturnTo string }
		var raw map[string]string
		_ = json.NewDecoder(r.Body).Decode(&raw)
		body.Code, body.CodeVerifier, body.ReturnTo = raw["code"], raw["code_verifier"], raw["return_to"]
		c, ok := f.codes[body.Code]
		delete(f.codes, body.Code)
		sum := sha256.Sum256([]byte(body.CodeVerifier))
		if !ok || c.returnTo != body.ReturnTo || c.challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{
				"code": "INVALID_SIGNIN_CODE", "message": "the sign-in code is invalid or has expired"}})
			return
		}
		data := map[string]any{"same_account": c.owner}
		if c.owner {
			data["email"] = "alice@example.com"
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data})
	}))
	t.Cleanup(ts.Close)
	return f, ts
}

// allow plays the person clicking Allow on the page startURL opens: it checks the page's address
// and returns the code and state the browser comes back with.
func (f *fakeSignInCloud) allow(t *testing.T, startURL string, owner bool) (code, state string) {
	t.Helper()
	u, err := url.Parse(startURL)
	if err != nil || u.Scheme+"://"+u.Host+u.Path != "https://web.example.com/meko-signin" {
		t.Fatalf("sign-in page %q, %v", startURL, err)
	}
	q := u.Query()
	if len(q.Get("code_challenge")) != 43 || q.Get("state") == "" || q.Get("return_to") == "" {
		t.Fatalf("query %v", q)
	}
	code = randomString(32)
	f.mu.Lock()
	f.codes[code] = issuedCode{challenge: q.Get("code_challenge"), returnTo: q.Get("return_to"), owner: owner}
	f.mu.Unlock()
	return code, q.Get("state")
}

func signInService(t *testing.T) (*Service, *fakeSignInCloud) {
	t.Helper()
	f, ts := newFakeSignInCloud(t)
	s, _ := newService(t)
	if _, err := s.Link(context.Background(), Update{URL: ptr("https://meko.example.com/mcp"),
		WebURL: ptr("https://web.example.com"), APIURL: ptr(ts.URL + "/api/v1"), Key: ptr(key)}); err != nil {
		t.Fatal(err)
	}
	return s, f
}

const callback = "https://my-meko.example:7777/signin/callback"

// br is the starting browser's nonce hash, as the server passes it.
const br = "browser-1"

func TestBrowserSignIn(t *testing.T) {
	ctx := context.Background()
	s, f := signInService(t)

	start, err := s.StartBrowserSignIn(ctx, callback, "/ext/kode/", br)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(start, key) || mustQuery(t, start).Get("return_to") != callback {
		t.Fatalf("start URL %q", start)
	}
	code, state := f.allow(t, start, true)
	got, err := s.FinishBrowserSignIn(ctx, code, state, br)
	if err != nil || !got.SameAccount || got.Email != "alice@example.com" || got.Next != "/ext/kode/" {
		t.Fatalf("owner: %+v, %v", got, err)
	}
	// The same code and state again: the sign-in is used up.
	if _, err = s.FinishBrowserSignIn(ctx, code, state, br); !errors.Is(err, ErrSignInExpired) {
		t.Fatalf("reused state: %v", err)
	}

	// Someone signed in to another account: no, and no email.
	start, _ = s.StartBrowserSignIn(ctx, callback, "/", br)
	code, state = f.allow(t, start, false)
	got, err = s.FinishBrowserSignIn(ctx, code, state, br)
	if err != nil || got.SameAccount || got.Email != "" {
		t.Fatalf("another account: %+v, %v", got, err)
	}
}

func TestBrowserSignInRefusals(t *testing.T) {
	ctx := context.Background()
	s, f := signInService(t)

	// An unknown state never reaches Meko Cloud.
	if _, err := s.FinishBrowserSignIn(ctx, "code", "nobody's", br); !errors.Is(err, ErrSignInExpired) ||
		f.exchanges != 0 {
		t.Fatalf("unknown state: %v, %d exchanges", err, f.exchanges)
	}
	// A code Meko Cloud refuses (unknown, used, another sign-in's) is an expired sign-in.
	start, _ := s.StartBrowserSignIn(ctx, callback, "/", br)
	_, state := f.allow(t, start, true)
	if _, err := s.FinishBrowserSignIn(ctx, "not-the-code", state, br); !errors.Is(err, ErrSignInExpired) {
		t.Fatalf("wrong code: %v", err)
	}
	// One waiting past BrowserSignInWait has expired.
	start, _ = s.StartBrowserSignIn(ctx, callback, "/", br)
	code, state := f.allow(t, start, true)
	s.browserSignIns.mu.Lock()
	p := s.browserSignIns.pending[state]
	p.started = p.started.Add(-BrowserSignInWait - time.Second)
	s.browserSignIns.pending[state] = p
	s.browserSignIns.mu.Unlock()
	if _, err := s.FinishBrowserSignIn(ctx, code, state, br); !errors.Is(err, ErrSignInExpired) {
		t.Fatalf("expired: %v", err)
	}
}

func TestBrowserSignInsAreBounded(t *testing.T) {
	ctx := context.Background()
	s, f := signInService(t)
	first, _ := s.StartBrowserSignIn(ctx, callback, "/", br)
	code, state := f.allow(t, first, true)
	for range maxBrowserSignIns {
		if _, err := s.StartBrowserSignIn(ctx, callback, "/", br); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(s.browserSignIns.pending); n != maxBrowserSignIns {
		t.Fatalf("%d waiting, want %d", n, maxBrowserSignIns)
	}
	if _, err := s.FinishBrowserSignIn(ctx, code, state, br); !errors.Is(err, ErrSignInExpired) {
		t.Fatalf("the oldest should have been dropped: %v", err)
	}
}

func TestBrowserSignInUnsupported(t *testing.T) {
	ctx := context.Background()
	s, f := signInService(t)
	f.status = http.StatusNotFound
	start, _ := s.StartBrowserSignIn(ctx, callback, "/", br)
	code, state := f.allow(t, start, true)
	var failed *CallError
	_, err := s.FinishBrowserSignIn(ctx, code, state, br)
	if !errors.As(err, &failed) || failed.Code != CodeSignInUnsupported || failed.Status != http.StatusConflict {
		t.Fatalf("404 without an envelope: %v", err)
	}
	// Meko Cloud failing is not "unsupported".
	f.status = http.StatusInternalServerError
	start, _ = s.StartBrowserSignIn(ctx, callback, "/", br)
	code, state = f.allow(t, start, true)
	if _, err = s.FinishBrowserSignIn(ctx, code, state, br); !errors.As(err, &failed) || failed.Code != CodeCallFailed {
		t.Fatalf("500: %v", err)
	}
}

func TestBrowserSignInNeedsALink(t *testing.T) {
	s, _ := newService(t)
	var failed *CallError
	if _, err := s.StartBrowserSignIn(context.Background(), callback, "/", br); !errors.As(err, &failed) ||
		failed.Code != CodeNotLinked {
		t.Fatalf("unlinked: %v", err)
	}
}

// A finish from another browser than the one that started, with the right code and state (an
// address copied from the first), signs no one in, and uses the sign-in up.
func TestBrowserSignInIsTheStartingBrowsers(t *testing.T) {
	ctx := context.Background()
	s, f := signInService(t)
	start, _ := s.StartBrowserSignIn(ctx, callback, "/", br)
	code, state := f.allow(t, start, true)
	for _, other := range []string{"", "browser-2"} {
		if _, err := s.FinishBrowserSignIn(ctx, code, state, other); !errors.Is(err, ErrSignInExpired) {
			t.Fatalf("browser %q: %v", other, err)
		}
	}
	if f.exchanges != 0 {
		t.Fatalf("%d exchanges: the code should never reach Meko Cloud", f.exchanges)
	}
}

// A link change voids the sign-ins started before it, both waiting ones and one finished just
// before the change.
func TestBrowserSignInEndsWithTheLink(t *testing.T) {
	ctx := context.Background()
	s, f := signInService(t)
	start, _ := s.StartBrowserSignIn(ctx, callback, "/", br)
	code, state := f.allow(t, start, true)
	got, err := s.FinishBrowserSignIn(ctx, code, state, br)
	if err != nil || !s.BrowserSignInCurrent(got) {
		t.Fatalf("finish: %+v, %v", got, err)
	}
	start, _ = s.StartBrowserSignIn(ctx, callback, "/", br)
	code, state = f.allow(t, start, true)
	if _, err = s.Link(ctx, Update{WebURL: ptr("https://web2.example.com")}); err != nil {
		t.Fatal(err)
	}
	if s.BrowserSignInCurrent(got) {
		t.Fatal("a sign-in finished under the old link is still current")
	}
	if _, err = s.FinishBrowserSignIn(ctx, code, state, br); !errors.Is(err, ErrSignInExpired) {
		t.Fatalf("started under the old link: %v", err)
	}
}
