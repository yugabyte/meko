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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/secrets"
)

// The fake Meko Cloud's sign-in credentials: each a sentinel no error or reply may hold.
const (
	oauthCode   = "code-sentinel-123"
	accessToken = "access-sentinel-456"
	mintedKey   = "mko_tkn_minted0123456789"
)

// fakeOAuth is Meko Cloud's sign-in as mcp.mekodata.ai serves it: an MCP endpoint that answers 401
// with WWW-Authenticate, the protected-resource and authorization-server metadata, client
// registration, /authorize (which signs in at once and sends the browser back with a code), /token
// and /api-keys.
type fakeOAuth struct {
	*httptest.Server
	mu         sync.Mutex
	registered int
	challenge  string // the authorize request's code_challenge
	verifier   string // the token request's code_verifier
	keyAuth    string // the api-keys request's Authorization
	keyName    string
	minted     int    // keys made: the first is mintedKey, then mintedKey2, mintedKey3…
	keyStatus  int    // what /api-keys answers; 0 is 201 with a key
	keyBody    string // its body, when keyStatus is set
	noOAuth    bool   // no authorization-server metadata
	authServer string // the authorization server the resource metadata names; "" is this one
	metaTo     string // where the authorization-server metadata redirects; "" serves it
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	t.Helper()
	f := &fakeOAuth{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", resource_metadata="`+f.URL+
			`/.well-known/oauth-protected-resource/mcp", scope="openid email profile"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		as := f.authServer
		if as == "" {
			as = f.URL + "/"
		}
		writeJSON(w, 200, map[string]any{"resource": f.URL + "/mcp", "authorization_servers": []string{as}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		if f.noOAuth {
			http.NotFound(w, r)
			return
		}
		if f.metaTo != "" {
			http.Redirect(w, r, f.metaTo, http.StatusFound)
			return
		}
		writeJSON(w, 200, map[string]any{"authorization_endpoint": f.URL + "/authorize",
			"token_endpoint": f.URL + "/token", "registration_endpoint": f.URL + "/register"})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.registered++
		f.mu.Unlock()
		writeJSON(w, 201, map[string]any{"client_id": "client-1", "redirect_uris": req.RedirectURIs})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.challenge = q.Get("code_challenge")
		f.mu.Unlock()
		back, _ := url.Parse(q.Get("redirect_uri"))
		back.RawQuery = url.Values{"code": {oauthCode}, "state": {q.Get("state")}}.Encode()
		//nolint:gosec // G710: a fake Meko Cloud, sending the browser back as Meko Cloud does
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.verifier = r.PostForm.Get("code_verifier")
		f.mu.Unlock()
		if r.PostForm.Get("code") != oauthCode || r.PostForm.Get("client_id") != "client-1" {
			writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": accessToken, "token_type": "Bearer"})
	})
	mux.HandleFunc("/api-keys", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.keyAuth, f.keyName = r.Header.Get("Authorization"), req.Name
		status, body := f.keyStatus, f.keyBody
		f.minted++
		key := mintedKey
		if f.minted > 1 {
			key += strconv.Itoa(f.minted)
		}
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		writeJSON(w, 201, map[string]any{"api_key": key, "id": "key-1", "name": req.Name,
			"expires_at": "2027-02-01T00:00:00Z"})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// browse follows the authorize URL as a browser would, up to the redirect back, and returns the
// address Meko Cloud sent it to.
func browse(t *testing.T, authorizeURL string) string {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Get(authorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.Header.Get("Location")
}

// noSecrets fails if an error or reply holds a sign-in credential.
func noSecrets(t *testing.T, what, text string) {
	t.Helper()
	for _, s := range []string{oauthCode, accessToken, mintedKey} {
		if strings.Contains(text, s) {
			t.Errorf("%s holds %q: %s", what, s, text)
		}
	}
}

func TestSignInFlow(t *testing.T) {
	f := newFakeOAuth(t)
	ctx := context.Background()
	const redirect = "http://127.0.0.1:1/callback"
	flow, err := StartFlow(ctx, nil, f.URL+"/mcp", redirect, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, flow.AuthorizeURL)
	if q.Get("client_id") != "client-1" || q.Get("redirect_uri") != redirect || q.Get("code_challenge_method") != "S256" ||
		q.Get("scope") != "openid email profile" || q.Get("resource") != f.URL+"/mcp" || len(q.Get("state")) < 32 {
		t.Fatalf("authorize URL: %s", flow.AuthorizeURL)
	}
	code, state, err := Callback(browse(t, flow.AuthorizeURL))
	if err != nil || code != oauthCode {
		t.Fatalf("Callback = %q, %q, %v", code, state, err)
	}
	m, err := flow.Finish(ctx, code, state, "Meko on test")
	if err != nil {
		t.Fatal(err)
	}
	if m.Key != mintedKey || m.Name != "Meko on test" || f.keyName != "Meko on test" ||
		f.keyAuth != "Bearer "+accessToken {
		t.Fatalf("minted %+v; api-keys got %q, %q", m, f.keyName, f.keyAuth)
	}
	sum := sha256.Sum256([]byte(f.verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
		t.Fatalf("the verifier %q doesn't match the challenge %q", f.verifier, f.challenge)
	}

	// The same state can't be swapped for another.
	if _, err = flow.Finish(ctx, code, "forged", "x"); err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Fatalf("a forged state: %v", err)
	}
}

func mustQuery(t *testing.T, u string) url.Values {
	t.Helper()
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	return p.Query()
}

func TestSignInRefusals(t *testing.T) {
	ctx := context.Background()
	finish := func(t *testing.T, f *fakeOAuth) error {
		t.Helper()
		flow, err := StartFlow(ctx, nil, f.URL+"/mcp", "http://127.0.0.1:1/callback", nil)
		if err != nil {
			return err
		}
		code, state, err := Callback(browse(t, flow.AuthorizeURL))
		if err != nil {
			return err
		}
		_, err = flow.Finish(ctx, code, state, "n")
		return err
	}
	for _, tc := range []struct {
		name, want string
		set        func(*fakeOAuth)
	}{
		{"no OAuth", "doesn't offer sign-in", func(f *fakeOAuth) { f.noOAuth = true }},
		{"no api-keys", "can't make API keys from a sign-in", func(f *fakeOAuth) { f.keyStatus = 404 }},
		{"too many keys", "as many API keys as it can", func(f *fakeOAuth) { f.keyStatus = 429 }},
		{"no account", "has no Meko Cloud account", func(f *fakeOAuth) {
			f.keyStatus, f.keyBody = 403, `{"error":{"code":"account_not_provisioned","message":"no"}}`
		}},
		{"failure", "didn't make an API key (HTTP 500): Cloud is down", func(f *fakeOAuth) {
			f.keyStatus, f.keyBody = 500, `{"error":{"code":"internal","message":"Cloud is down."}}`
		}},
		{"not a key", "didn't make an API key (HTTP 201)", func(f *fakeOAuth) {
			f.keyStatus, f.keyBody = 201, `{"api_key":"`+accessToken+`"}`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOAuth(t)
			tc.set(f)
			err := finish(t, f)
			var refused *Error
			if !errors.As(err, &refused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			noSecrets(t, "the error", err.Error())
		})
	}

	if _, err := StartFlow(ctx, nil, "http://meko.example.com/mcp", "http://127.0.0.1:1/callback", nil); err == nil ||
		!strings.Contains(err.Error(), "needs https") {
		t.Fatalf("plain http off this computer: %v", err)
	}

	// The code and verifier never go over plain http to another computer: not to an authorization
	// server the metadata names, nor where a redirect leads.
	f := newFakeOAuth(t)
	f.authServer = "http://auth.example.com"
	if _, err := StartFlow(ctx, nil, f.URL+"/mcp", "http://127.0.0.1:1/callback", nil); err == nil ||
		!strings.Contains(err.Error(), "doesn't offer sign-in") {
		t.Fatalf("a plain-http authorization server: %v", err)
	}
	f = newFakeOAuth(t)
	f.metaTo = "http://auth.example.com/.well-known/oauth-authorization-server"
	if _, err := StartFlow(ctx, nil, f.URL+"/mcp", "http://127.0.0.1:1/callback", nil); err == nil ||
		!strings.Contains(err.Error(), "redirected to an address that isn't https") {
		t.Fatalf("a redirect to plain http: %v", err)
	}
}

func TestCallback(t *testing.T) {
	for _, tc := range []struct{ address, code, want string }{
		{"http://127.0.0.1:5/callback?code=c&state=s", "c", ""},
		{" http://127.0.0.1:5/callback?state=s&code=c \n", "c", ""},
		{"http://127.0.0.1:5/callback?error=access_denied&error_description=User+denied+access.", "",
			"Meko Cloud didn't sign you in: User denied access. Nothing was saved."},
		{"http://127.0.0.1:5/callback?error=access_denied", "", "didn't sign you in: access_denied."},
		{"http://127.0.0.1:5/callback?state=s", "", "has no sign-in code"},
		{"hello", "", "isn't the address Meko Cloud sent your browser to"},
	} {
		code, _, err := Callback(tc.address)
		if code != tc.code || (tc.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Callback(%q) = %q, %v; want %q, %q", tc.address, code, err, tc.code, tc.want)
		}
	}
}

func TestKeyNameAndURL(t *testing.T) {
	n := KeyName(time.Date(2026, 11, 3, 4, 5, 6, 0, time.UTC))
	if !strings.HasPrefix(n, "Meko on ") || !strings.HasSuffix(n, " 2026-11-03 04:05:06Z") || len(n) > 100 {
		t.Errorf("KeyName = %q", n)
	}
	for in, want := range map[string]string{
		DefaultURL:                         "https://mcp.mekodata.ai/api-keys",
		"https://meko.example.com/x/mcp/":  "https://meko.example.com/x/api-keys",
		"http://127.0.0.1:7777/mcp":        "http://127.0.0.1:7777/api-keys",
		"https://meko.example.com/ignored": "https://meko.example.com/ignored/api-keys",
	} {
		if got := APIKeysURL(in); got != want {
			t.Errorf("APIKeysURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The web UI's sign-in: StartSignIn, the browser at Meko Cloud, then FinishSignIn, which links with
// the minted key as a typed one. A sign-in is used once, and another replaces it.
func TestServiceSignIn(t *testing.T) {
	f := newFakeOAuth(t)
	s, calls := newService(t)
	ctx := context.Background()
	const redirect = "http://127.0.0.1:7777/settings/meko-cloud/callback"
	u := Update{URL: ptr(f.URL + "/mcp"), WebURL: ptr(f.URL)}

	to, err := s.StartSignIn(ctx, u, redirect)
	if err != nil {
		t.Fatal(err)
	}
	code, state, err := Callback(browse(t, to))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishSignIn(ctx, code, "forged"); err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Fatalf("a forged state: %v", err)
	}
	st, err := s.FinishSignIn(ctx, code, state)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Linked || st.EffectiveURL != f.URL+"/mcp" || *st.KeyHint != "6789" {
		t.Fatalf("linked: %+v", st)
	}
	noSecrets(t, "the status", toJSON(t, st))
	if got, _ := s.Store.Load(); got.Link.Key != mintedKey {
		t.Fatalf("saved key %q", got.Link.Key)
	}
	if strings.Join(*calls, ",") != "check "+f.URL+"/mcp,api "+f.URL+"/api/v1,changed" {
		t.Fatalf("calls %v", *calls)
	}
	if _, err = s.FinishSignIn(ctx, code, state); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("a sign-in used twice: %v", err)
	}

	// Signing in again reuses the client registered for this server and redirect URI, and a new
	// sign-in replaces the one before it.
	first, err := s.StartSignIn(ctx, Update{}, redirect)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.StartSignIn(ctx, Update{}, redirect)
	if err != nil {
		t.Fatal(err)
	}
	if f.registered != 1 {
		t.Fatalf("registered %d clients", f.registered)
	}
	code, state, _ = Callback(browse(t, first))
	if _, err = s.FinishSignIn(ctx, code, state); err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Fatalf("a replaced sign-in: %v", err)
	}

	// A key that fails the link's check is named, since Meko Cloud can't delete it; nothing is saved.
	s.Check = func(context.Context, string, string) error { return errors.New("401 Unauthorized") }
	code, state, _ = Callback(browse(t, second))
	_, err = s.FinishSignIn(ctx, code, state)
	if err == nil || !strings.Contains(err.Error(), "didn't accept that key") ||
		!strings.Contains(err.Error(), `The key Meko made, "Meko on `) {
		t.Fatalf("a failed link: %v", err)
	}
	noSecrets(t, "the error", err.Error())
	if got, _ := s.Store.Load(); got.Link.Key != mintedKey {
		t.Fatalf("a failed link changed the saved key to %q", got.Link.Key)
	}
}

// A sign-in links its key to the server it signed in to, even when the saved link moves elsewhere
// before it finishes.
func TestSignInPinsItsServer(t *testing.T) {
	f := newFakeOAuth(t)
	s, calls := newService(t)
	ctx := context.Background()
	if _, err := s.Link(ctx, Update{URL: ptr(f.URL + "/mcp"), WebURL: ptr(f.URL), Key: ptr(key)}); err != nil {
		t.Fatal(err)
	}
	to, err := s.StartSignIn(ctx, Update{}, "http://127.0.0.1:7777/settings/meko-cloud/callback")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Link(ctx, Update{URL: ptr("https://other.example.com/mcp"), WebURL: ptr("https://other.example.com"),
		Key: ptr(key)}); err != nil {
		t.Fatal(err)
	}
	*calls = nil
	code, state, _ := Callback(browse(t, to))
	st, err := s.FinishSignIn(ctx, code, state)
	if err != nil {
		t.Fatal(err)
	}
	if st.EffectiveURL != f.URL+"/mcp" || *st.EffectiveWebURL != f.URL {
		t.Fatalf("linked to %+v", st)
	}
	for _, c := range *calls {
		if strings.Contains(c, "other.example.com") {
			t.Fatalf("the key went to the other server: %v", *calls)
		}
	}
}

// failingSecrets is a secret store that can't save.
type failingSecrets struct{ secrets.Store }

func (failingSecrets) Set(string, string) error {
	return errors.New("the keychain at /private/path is locked")
}

// A link that fails for a reason that isn't a refusal is an internal error, which the route answers
// generically: it names the key left in the account for the log, not as a refusal's text.
func TestSignInInternalFailure(t *testing.T) {
	f := newFakeOAuth(t)
	s, _ := newService(t)
	s.Store.Secrets = failingSecrets{s.Store.Secrets}
	ctx := context.Background()
	to, err := s.StartSignIn(ctx, Update{URL: ptr(f.URL + "/mcp"), WebURL: ptr(f.URL)},
		"http://127.0.0.1:7777/settings/meko-cloud/callback")
	if err != nil {
		t.Fatal(err)
	}
	code, state, _ := Callback(browse(t, to))
	_, err = s.FinishSignIn(ctx, code, state)
	var refused *Error
	if err == nil || errors.As(err, &refused) || !strings.Contains(err.Error(), `the key Meko made, "Meko on `) {
		t.Fatalf("an internal failure: %v", err)
	}
	noSecrets(t, "the error", err.Error())
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
