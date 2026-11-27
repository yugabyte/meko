package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/extproxy"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/tlscert"
)

// signInCloud is Meko Cloud's half of signing in: allow issues a code as its /meko-signin page does
// on Allow; its exchange route checks the key, the verifier and return_to, and uses the code up.
type signInCloud struct {
	mu    sync.Mutex
	codes map[string]signInCode
}

type signInCode struct {
	challenge, returnTo string
	owner               bool
}

func newSignInCloud(t *testing.T) (*signInCloud, string) {
	t.Helper()
	f := &signInCloud{codes: map[string]signInCode{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/auth/meko-signin/exchange" || r.Header.Get("Authorization") != "Bearer "+cloudKey {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		c, ok := f.codes[in["code"]]
		delete(f.codes, in["code"])
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(in["code_verifier"]))
		if !ok || c.returnTo != in["return_to"] || c.challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"INVALID_SIGNIN_CODE","message":"invalid"}}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":{"same_account":%t,"email":"alice@example.com"}}`, c.owner)
	}))
	t.Cleanup(ts.Close)
	return f, ts.URL + "/api/v1"
}

// allow is the person clicking Allow on the page start's reply sends them to; it returns the code
// and state Meko Cloud sends the browser back with.
func (f *signInCloud) allow(t *testing.T, startReply string, owner bool) (code, state, returnTo string) {
	t.Helper()
	var env struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(startReply), &env); err != nil {
		t.Fatalf("start: %s", startReply)
	}
	u, err := url.Parse(env.Data.URL)
	if err != nil || u.Host != "web.example.com" || u.Path != "/meko-signin" {
		t.Fatalf("sign-in page %q", env.Data.URL)
	}
	q := u.Query()
	code = fmt.Sprintf("code-%d", time.Now().UnixNano())
	f.mu.Lock()
	f.codes[code] = signInCode{challenge: q.Get("code_challenge"), returnTo: q.Get("return_to"), owner: owner}
	f.mu.Unlock()
	return code, q.Get("state"), q.Get("return_to")
}

// startSignInWeb runs a server with remote access and Meko Cloud sign-in, linked to a fake Meko
// Cloud, as serve runs it. It returns its https:// address, the same port over plain HTTP (this
// computer's), the store holding the sessions, and the fake Meko Cloud.
func startSignInWeb(t *testing.T) (base, local string, st *store.Store, cloud *signInCloud) {
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
	cloud, api := newSignInCloud(t)
	svc := cloudService(t)
	web, mcp := "https://web.example.com", "https://meko.example.com/mcp"
	key := cloudKey
	if _, err = svc.Link(ctx, cloudlink.Update{URL: &mcp, WebURL: &web, APIURL: &api, Key: &key}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := tcpPort(t, ln)
	base, local = fmt.Sprintf("https://127.0.0.1:%d", port), fmt.Sprintf("http://127.0.0.1:%d", port)
	ext := extproxy.New("kode", "Kode", func() string { return fakeExtension(t) }, slog.New(slog.DiscardHandler))
	s := New(Options{Info: Info{Version: "test"}, Port: port, Token: token, ClientToken: clientToken,
		Logger: slog.New(slog.DiscardHandler), Cloud: svc,
		UI: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ui") }),
		Web: &Web{Access: config.AccessRemote, SignIn: true, Address: base, Sessions: st,
			Extensions: func() []*extproxy.Proxy { return []*extproxy.Proxy{ext} }}})
	probe(s)
	pair, err := tlscert.SelfSigned(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tl := Sniff(ln, &tls.Config{Certificates: []tls.Certificate{pair.Cert}, MinVersion: tls.VersionTLS12})
	go func() { _ = s.Serve(tl) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return base, local, st, cloud
}

// signIn goes through the whole sign-in as the browser would, and returns finish's status, body and
// the session cookie it set ("" for none).
func signIn(t *testing.T, base string, cloud *signInCloud, owner bool) (int, string, string) {
	t.Helper()
	code, b := send(t, http.MethodPost, base+signInStartRoute, `{"next":"/ext/kode/"}`, nil)
	if code != 200 {
		t.Fatalf("start: %d %s", code, b)
	}
	c, state, returnTo := cloud.allow(t, b, owner)
	if returnTo != base+SignInCallbackPath {
		t.Fatalf("return_to %q, want this Meko's callback page", returnTo)
	}
	req, _ := http.NewRequest(http.MethodPost, base+signInFinishRoute,
		strings.NewReader(fmt.Sprintf(`{"code":%q,"state":%q}`, c, state)))
	req.Header.Set("Content-Type", "application/json")
	status, out, h := body(t, req)
	cookie := ""
	for _, ck := range (&http.Response{Header: h}).Cookies() {
		if ck.Name == SessionCookie {
			if !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" || ck.MaxAge <= 0 {
				t.Errorf("cookie %+v", ck)
			}
			cookie = ck.Value
		}
	}
	return status, out, cookie
}

func withCookie(cookie string) map[string]string {
	return map[string]string{"Cookie": SessionCookie + "=" + cookie}
}

func TestSignInRequired(t *testing.T) {
	base, local, _, _ := startSignInWeb(t)

	// Another computer with no session: refused with a 401 the web UI and Kode read as "sign in".
	code, env := get(t, base+"/api/v1/probe", nil, "")
	if code != 401 || env.Error == nil || env.Error.Code != "SIGNIN_REQUIRED" {
		t.Fatalf("no session: %d %+v", code, env)
	}
	code, env = get(t, base+authStatusRoute, nil, "")
	if m, _ := env.Data.(map[string]any); code != 200 || m["signed_in"] != false || m["signin"] != "cloud" {
		t.Fatalf("auth status: %d %+v", code, env)
	}
	// The web UI's pages load, so the sign-in page can.
	req, _ := http.NewRequest(http.MethodGet, base+"/signin/callback?code=x&state=y", nil)
	if got, b, _ := body(t, req); got != 200 || b != "ui" {
		t.Fatalf("callback page: %d %q", got, b)
	}
	// Opening an extension's page goes to sign-in and back; its scripts' calls get the 401.
	req, _ = http.NewRequest(http.MethodGet, base+"/ext/kode/p?x=1", nil)
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	if got, _, h := body(t, req); got != 303 || h.Get("Location") != "/signin?next=%2Fext%2Fkode%2Fp%3Fx%3D1" {
		t.Fatalf("extension page: %d %q", got, h.Get("Location"))
	}
	req, _ = http.NewRequest(http.MethodGet, base+"/ext/kode/api/state", nil)
	req.Header.Set("Sec-Fetch-Mode", "cors")
	if got, _, _ := body(t, req); got != 401 {
		t.Fatalf("extension call: %d", got)
	}
	// Another site's page still gets the 403.
	if code, _ = get(t, base+"/api/v1/probe", map[string]string{"Sec-Fetch-Site": "cross-site"}, ""); code != 403 {
		t.Fatalf("cross-site: %d", code)
	}
	// This computer needs no sign-in.
	code, env = get(t, local+"/api/v1/probe", nil, "")
	if code != 200 || env.Data != CredentialLocal {
		t.Fatalf("local: %d %+v", code, env)
	}
	if code, _ = send(t, http.MethodPost, local+signInStartRoute, `{}`, nil); code != 400 {
		t.Fatalf("start over plain HTTP: %d", code)
	}
}

func TestSignInFlow(t *testing.T) {
	base, _, _, cloud := startSignInWeb(t)

	code, b, cookie := signIn(t, base, cloud, true)
	if code != 200 || cookie == "" || !strings.Contains(b, `"next":"/ext/kode/"`) {
		t.Fatalf("finish: %d %s cookie %q", code, b, cookie)
	}
	code, env := get(t, base+"/api/v1/probe", withCookie(cookie), "")
	if code != 200 || env.Data != CredentialRemote {
		t.Fatalf("with the session: %d %+v", code, env)
	}
	code, env = get(t, base+authStatusRoute, withCookie(cookie), "")
	if m, _ := env.Data.(map[string]any); code != 200 || m["signed_in"] != true {
		t.Fatalf("auth status signed in: %+v", env)
	}
	if code, _ = get(t, base+"/api/v1/probe", withCookie("made-up"), ""); code != 401 {
		t.Fatalf("an unknown cookie: %d", code)
	}

	// Signing out ends the session and clears the cookie.
	req, _ := http.NewRequest(http.MethodPost, base+signOutRoute, nil)
	req.Header.Set("Cookie", SessionCookie+"="+cookie)
	got, _, h := body(t, req)
	if got != 200 || !strings.Contains(h.Get("Set-Cookie"), SessionCookie+"=;") {
		t.Fatalf("sign out: %d %q", got, h.Get("Set-Cookie"))
	}
	if code, _ = get(t, base+"/api/v1/probe", withCookie(cookie), ""); code != 401 {
		t.Fatalf("after signing out: %d", code)
	}
}

func TestSignInRefusals(t *testing.T) {
	base, _, st, cloud := startSignInWeb(t)

	// Someone signed in to another Meko Cloud account gets no session.
	code, b, cookie := signIn(t, base, cloud, false)
	if code != 403 || !strings.Contains(b, "NOT_THE_OWNER") || cookie != "" {
		t.Fatalf("another account: %d %s %q", code, b, cookie)
	}
	// A code used again: expired.
	code, b = send(t, http.MethodPost, base+signInFinishRoute, `{"code":"x","state":"unknown"}`, nil)
	if code != 400 || !strings.Contains(b, "SIGNIN_EXPIRED") {
		t.Fatalf("unknown state: %d %s", code, b)
	}
	// A request naming another host never gets a sign-in that comes back there.
	req, _ := http.NewRequest(http.MethodPost, base+signInStartRoute, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "evil.example"
	if got, out, _ := body(t, req); got != 400 || !strings.Contains(out, "WRONG_ADDRESS") ||
		!strings.Contains(out, base) {
		t.Fatalf("another host: %d %s", got, out)
	}
	// An expired session is refused.
	past := time.Now().Add(-time.Second)
	if err := st.CreateSession(context.Background(), tokenSHA256("old"), past, "", ""); err != nil {
		t.Fatal(err)
	}
	if code, _ = get(t, base+"/api/v1/probe", withCookie("old"), ""); code != 401 {
		t.Fatalf("expired session: %d", code)
	}
}

func TestSignInNoneIsUnchanged(t *testing.T) {
	base, _ := startWeb(t, config.AccessRemote, fakeExtension(t))
	code, env := get(t, base+"/api/v1/probe", nil, "")
	if code != 200 || env.Data != CredentialRemote {
		t.Fatalf("remote without sign-in: %d %+v", code, env)
	}
	code, env = get(t, base+authStatusRoute, nil, "")
	if m, _ := env.Data.(map[string]any); code != 200 || m["signin"] != "none" {
		t.Fatalf("auth status: %+v", env)
	}
	if code, _ = send(t, http.MethodPost, base+signInStartRoute, `{}`, nil); code != 404 {
		t.Fatalf("start without sign-in: %d", code)
	}
}

func TestSafeNextAndSameAddress(t *testing.T) {
	for in, want := range map[string]string{"/ext/kode/?a=1": "/ext/kode/?a=1", "": "/", "https://evil.example": "/",
		"//evil.example": "/", "/\\evil.example": "/", "ext": "/", "/a\nb": "/"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
	for _, tc := range []struct {
		host, address string
		want          bool
	}{
		{"my-meko.example:7777", "https://my-meko.example:7777", true},
		{"MY-MEKO.example:7777", "https://my-meko.example:7777", true},
		{"my-meko.example", "https://my-meko.example:443", true},
		{"my-meko.example:443", "https://my-meko.example", true},
		{"evil.example:7777", "https://my-meko.example:7777", false},
		{"my-meko.example:7778", "https://my-meko.example:7777", false},
		{"my-meko.example:7777", "", false},
	} {
		if got := sameAddress(tc.host, tc.address); got != tc.want {
			t.Errorf("sameAddress(%q, %q) = %t", tc.host, tc.address, got)
		}
	}
}
