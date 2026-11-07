package server

import (
	"context"
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
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/extproxy"
	"github.com/yugabyte/meko/src/meko/internal/secrets"
)

const cloudKey = "mko_tkn_cloud_0123456789wxyz"

// cloudRoute is the Meko Cloud link's route.
const cloudRoute = "/api/v1/settings/meko-cloud"

func cloudService(t *testing.T) *cloudlink.Service {
	t.Helper()
	dir := t.TempDir()
	return &cloudlink.Service{
		Store:    cloudlink.Store{File: filepath.Join(dir, "cloud.toml"), Secrets: secrets.NewFiles(dir)},
		Check:    func(context.Context, string, string) error { return nil },
		CheckAPI: func(context.Context, string) error { return nil },
	}
}

func send(t *testing.T, method, url, payload string, header map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	code, b, _ := body(t, req)
	return code, b
}

// The link's routes take the admin token (meko cloud-link), never an agent's client token, which
// can't read it or point it elsewhere.
func TestCloudRoutesWithTokens(t *testing.T) {
	ln, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Info: Info{Version: "test"}, Port: tcpPort(t, ln), Token: token, ClientToken: clientToken,
		Logger: slog.New(slog.DiscardHandler), Cloud: cloudService(t)})
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	base := fmt.Sprintf("http://127.0.0.1:%d", tcpPort(t, ln))
	admin := map[string]string{"Authorization": "Bearer " + token}

	code, b := send(t, http.MethodPut, base+cloudRoute, `{"api_key":"`+cloudKey+`"}`,
		map[string]string{"Authorization": "Bearer " + clientToken})
	if code != 403 {
		t.Fatalf("PUT with the client token: %d %s", code, b)
	}
	code, b = send(t, http.MethodPut, base+cloudRoute, `{"api_key":"two words"}`, admin)
	if code != 400 || !strings.Contains(b, "VALIDATION_ERROR") || !strings.Contains(b, "no spaces") {
		t.Fatalf("a refused change: %d %s", code, b)
	}
	code, b = send(t, http.MethodPut, base+cloudRoute, `{"api_key":"`+cloudKey+`"}`, admin)
	if code != 200 || !strings.Contains(b, `"linked":true`) || !strings.Contains(b, `"key_hint":"wxyz"`) ||
		strings.Contains(b, cloudKey) {
		t.Fatalf("PUT with the admin token: %d %s", code, b)
	}
}

// A browser reads and changes the link, and each running extension hears of a change on its socket.
func TestCloudRoutesForBrowsers(t *testing.T) {
	sock := filepath.Join(shortTemp(t), "web.sock")
	heard := make(chan string, 4)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	record := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		heard <- r.Method + " " + r.URL.Path + " " + string(b) + " origin=" + r.Header.Get("Origin")
	}
	ext := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(record)}
	go func() { _ = ext.Serve(ln) }()
	t.Cleanup(func() { _ = ext.Close() })

	cloud := cloudService(t)
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := extproxy.New("agentk", "AgentK", func() string { return sock }, slog.New(slog.DiscardHandler))
	cloud.Changed = func(ctx context.Context) { _ = p.Notify(ctx, "cloud") }
	s := New(Options{Info: Info{Version: "test"}, Port: tcpPort(t, ln2), Logger: slog.New(slog.DiscardHandler),
		Cloud: cloud, Web: &Web{Access: config.AccessLocal,
			Extensions: func() []*extproxy.Proxy { return []*extproxy.Proxy{p} }}})
	go func() { _ = s.Serve(ln2) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	web := fmt.Sprintf("http://127.0.0.1:%d", tcpPort(t, ln2))

	code, b := send(t, http.MethodGet, web+cloudRoute, "", nil)
	if code != 200 || !strings.Contains(b, `"set":false`) {
		t.Fatalf("GET: %d %s", code, b)
	}
	code, b = send(t, http.MethodPut, web+cloudRoute, `{"api_key":"`+cloudKey+`"}`, map[string]string{"Origin": web})
	if code != 200 || strings.Contains(b, cloudKey) {
		t.Fatalf("PUT: %d %s", code, b)
	}
	select {
	case got := <-heard:
		if got != `POST /_meko/changed {"what":"cloud"} origin=` {
			t.Fatalf("the extension heard %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the extension heard nothing")
	}
	code, b = send(t, http.MethodDelete, web+cloudRoute, "", map[string]string{"Origin": web})
	if code != 200 || !strings.Contains(b, `"linked":false`) {
		t.Fatalf("DELETE: %d %s", code, b)
	}
	code, _ = send(t, http.MethodDelete, web+cloudRoute, "", map[string]string{"Origin": "http://example.com"})
	if code != 403 {
		t.Fatalf("another site's page: %d", code)
	}
}

// fakeSignIn is Meko Cloud's browser sign-in, as small as Meko's checks allow: discovery, client
// registration, an /authorize that signs in at once, /token and /api-keys (docs/cloud.md).
func fakeSignIn(t *testing.T) string {
	t.Helper()
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"authorization_endpoint":%q,"token_endpoint":%q,"registration_endpoint":%q}`,
			base+"/authorize", base+"/token", base+"/register")
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"client_id":"client-1"}`)
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		//nolint:gosec // G710: a test fake of Meko Cloud, sending the browser back as Meko Cloud does
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=the-code&state="+url.QueryEscape(q.Get("state")),
			http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"the-token"}`)
	})
	mux.HandleFunc("/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"api_key":"`+cloudKey+`","name":"Meko on test"}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	base = ts.URL
	return base
}

// The web UI signs in to Meko Cloud on the web listener: the sign-in comes back to the UI's own
// callback page, served to Meko Cloud's redirect (another site's), whose POST finishes it. The API
// listener has no sign-in routes.
func TestCloudSignInRoutes(t *testing.T) {
	cloudURL := fakeSignIn(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Info: Info{Version: "test"}, Port: tcpPort(t, ln), Logger: slog.New(slog.DiscardHandler),
		Cloud: cloudService(t), Web: &Web{Access: config.AccessLocal},
		UI: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ui") })})
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	web := fmt.Sprintf("http://127.0.0.1:%d", tcpPort(t, ln))
	own := map[string]string{"Origin": web}

	code, b := send(t, http.MethodPost, web+cloudRoute+"/sign-in", `{"url":"`+cloudURL+`/mcp"}`,
		map[string]string{"Origin": "http://example.com"})
	if code != 403 {
		t.Fatalf("another site's page starts a sign-in: %d %s", code, b)
	}
	code, b = send(t, http.MethodPost, web+cloudRoute+"/sign-in", `{"url":"http://meko.example.com/mcp"}`, own)
	if code != 400 || !strings.Contains(b, "needs https") {
		t.Fatalf("a plain-http server: %d %s", code, b)
	}
	code, b = send(t, http.MethodPost, web+cloudRoute+"/sign-in", `{"url":"`+cloudURL+`/mcp","web_url":"`+cloudURL+`"}`,
		own)
	var started struct {
		Data struct {
			AuthorizeURL string `json:"authorize_url"`
		} `json:"data"`
	}
	if err = json.Unmarshal([]byte(b), &started); code != 200 || err != nil {
		t.Fatalf("start: %d %s", code, b)
	}
	authorize, _ := url.Parse(started.Data.AuthorizeURL)
	if got := authorize.Query().Get("redirect_uri"); got != web+"/settings/meko-cloud/callback" {
		t.Fatalf("redirect_uri %q", got)
	}

	// Meko Cloud sends the browser back: the page is served to another site's redirect.
	req, _ := http.NewRequest(http.MethodGet, started.Data.AuthorizeURL, nil)
	_, _, h := body(t, req)
	back, _ := url.Parse(h.Get("Location"))
	req, _ = http.NewRequest(http.MethodGet, back.String(), nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if code, b, _ = body(t, req); code != 200 || b != "ui" {
		t.Fatalf("the callback page: %d %q", code, b)
	}
	finish := `{"code":"the-code","state":"` + back.Query().Get("state") + `"}`
	code, b = send(t, http.MethodPost, web+cloudRoute+"/sign-in/finish", finish, own)
	if code != 200 || !strings.Contains(b, `"linked":true`) || !strings.Contains(b, `"key_hint":"wxyz"`) ||
		strings.Contains(b, cloudKey) || strings.Contains(b, "the-token") {
		t.Fatalf("finish: %d %s", code, b)
	}
	code, b = send(t, http.MethodPost, web+cloudRoute+"/sign-in/finish", finish, own)
	if code != 400 || !strings.Contains(b, "expired") {
		t.Fatalf("finishing twice: %d %s", code, b)
	}

	api, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	s2 := New(Options{Info: Info{Version: "test"}, Port: tcpPort(t, api), Token: token, ClientToken: clientToken,
		Logger: slog.New(slog.DiscardHandler), Cloud: cloudService(t)})
	go func() { _ = s2.Serve(api) }()
	t.Cleanup(func() { _ = s2.Shutdown(context.Background()) })
	code, _ = send(t, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d", tcpPort(t, api))+cloudRoute+"/sign-in", `{}`,
		map[string]string{"Authorization": "Bearer " + token})
	if code != 404 {
		t.Fatalf("sign-in on the API listener: %d", code)
	}
}
