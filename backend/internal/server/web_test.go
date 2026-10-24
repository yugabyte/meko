package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/extproxy"
	"github.com/yugabyte/meko/backend/internal/login"
	"github.com/yugabyte/meko/backend/internal/store"
)

// fakeExtension listens on a unix socket like AgentK: it echoes what reached it, and a WebSocket
// upgrade gets 101 and an echo of what's written on it.
func fakeExtension(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(shortTemp(t), "web.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			c, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = rw.Flush()
			_, _ = io.Copy(c, rw)
			return
		}
		//nolint:gosec // G705: a test server's plain-text reply
		_, _ = fmt.Fprintf(w, "path=%s host=%s cookie=%q xff=%q", r.URL.Path, r.Host, r.Header.Get("Cookie"),
			r.Header.Get("X-Forwarded-For"))
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(handler)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// shortTemp is a temporary folder with a short path: unix sockets have a length limit.
func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startWeb runs a web listener with access, one extension (agentk) on sock, and the sign-in; each
// of with changes its Web first.
func startWeb(t *testing.T, access, sock string, with ...func(*Web)) (string, *login.Service) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "meko.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logins := login.New(st)
	ext := extproxy.New("agentk", "AgentK", func() string { return sock }, slog.New(slog.DiscardHandler))
	// As serve wires it (webListener.RevokeEnded): a session that ends closes its WebSockets.
	logins.OnRevoke(func() { ext.Revoke(func(h string) bool { return logins.Live(context.Background(), h) }) })
	web := &Web{Access: access, Extensions: func() []*extproxy.Proxy { return []*extproxy.Proxy{ext} }}
	for _, f := range with {
		f(web)
	}
	s := New(Options{Info: Info{Version: "test"}, Port: tcpPort(t, ln), Logger: slog.New(slog.DiscardHandler),
		Login: logins,
		UI:    http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ui") }),
		Web:   web})
	probe(s)
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return fmt.Sprintf("http://127.0.0.1:%d", tcpPort(t, ln)), logins
}

func body(t *testing.T, req *http.Request) (int, string, http.Header) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// With local access a request from this computer needs nothing, by AgentK's old rule; a proxy's
// headers, another site's page, a foreign Host or a Bearer token alone get nothing.
func TestWebLocalAccess(t *testing.T) {
	base, _ := startWeb(t, config.AccessLocal, fakeExtension(t))
	cases := []struct {
		name   string
		header map[string]string
		host   string
		want   int
	}{
		{name: "from this computer", want: 200},
		{name: "typed in the address bar", header: map[string]string{"Sec-Fetch-Site": "none"}, want: 200},
		{name: "its own page", header: map[string]string{"Origin": base, "Sec-Fetch-Site": "same-origin"}, want: 200},
		{name: "as localhost", host: "localhost:" + strings.Split(base, ":")[2], want: 200},
		{name: "through a proxy", header: map[string]string{"X-Forwarded-For": "10.0.0.9"}, want: 401},
		{name: "another site's page", header: map[string]string{"Sec-Fetch-Site": "cross-site"}, want: 401},
		{name: "another origin", header: map[string]string{"Origin": "http://example.com"}, want: 403},
		{name: "a foreign Host", host: "evil.example.com", want: 403},
		{name: "a Bearer token isn't a credential here",
			header: map[string]string{"Authorization": "Bearer x", "X-Forwarded-For": "10.0.0.9"}, want: 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, env := get(t, base+"/api/v1/probe", tc.header, tc.host)
			if code != tc.want {
				t.Fatalf("%d, want %d (%+v)", code, tc.want, env)
			}
			if code == 200 && env.Data != CredentialLocal {
				t.Errorf("credential %v, want local", env.Data)
			}
		})
	}
	code, env := get(t, base+authStatusRoute, nil, "")
	if m, _ := env.Data.(map[string]any); code != 200 || m["access"] != "local" || m["signed_in"] != true ||
		m["setup_required"] != false {
		t.Errorf("auth status: %d %+v", code, env)
	}
	for _, path := range []string{"/api/v1/status", "/api/v1/version", setupLinkRoute} {
		if code, _ := get(t, base+path, nil, ""); code != 404 {
			t.Errorf("%s on the web listener: %d, want 404", path, code)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/datapacks/x", nil)
	if code, b, _ := body(t, req); code != 200 || b != "ui" {
		t.Errorf("the UI: %d %q", code, b)
	}
}

// With remote access: a setup link makes the login, the session cookie is the credential, a page
// of an extension's without one goes to the sign-in, and the proxy passes the browser's Host on
// with neither Meko's cookie nor forwarding headers.
func TestWebRemoteSignInAndProxy(t *testing.T) {
	base, logins := startWeb(t, config.AccessRemote, fakeExtension(t))
	host := strings.TrimPrefix(base, "http://")
	code, env := get(t, base+authStatusRoute, nil, "")
	if m, _ := env.Data.(map[string]any); code != 200 || m["setup_required"] != true || m["signed_in"] != false {
		t.Fatalf("status before setup: %d %+v", code, env)
	}
	if code, _ = get(t, base+"/api/v1/probe", nil, ""); code != 401 {
		t.Fatalf("no credential: %d", code)
	}
	if code, _ = get(t, base+"/healthz", map[string]string{"Origin": base}, ""); code != 403 {
		t.Errorf("an http Origin on the HTTPS listener: %d, want 403", code)
	}

	token, err := logins.IssueSetupLink(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, base+authSetupRoute,
		strings.NewReader(`{"token":"`+token+`","username":"ana","password":"a long password"}`))
	req.Header.Set("Content-Type", "application/json")
	code, _, h := body(t, req)
	cookie := ""
	for _, c := range (&http.Response{Header: h}).Cookies() {
		if c.Name == SessionCookie {
			cookie = c.Value
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
				t.Errorf("cookie %+v", c)
			}
		}
	}
	if code != 200 || cookie == "" {
		t.Fatalf("setup: %d, cookie %q", code, cookie)
	}
	signed := map[string]string{"Cookie": SessionCookie + "=" + cookie}
	if code, env = get(t, base+"/api/v1/probe", signed, ""); code != 200 || env.Data != CredentialSession {
		t.Fatalf("signed in: %d %+v", code, env)
	}

	// An extension's page without a session goes to the sign-in, and comes back after it.
	req, _ = http.NewRequest(http.MethodGet, base+"/ext/agentk/p/42?x=1", nil)
	req.Header.Set("Accept", "text/html")
	if got, _, h := body(t, req); got != 302 || h.Get("Location") != "/signin?next=%2Fext%2Fagentk%2Fp%2F42%3Fx%3D1" {
		t.Errorf("page without a session: %d %s", got, h.Get("Location"))
	}
	req, _ = http.NewRequest(http.MethodGet, base+"/ext/agentk", nil)
	if got, _, h := body(t, req); got != 308 || h.Get("Location") != "/ext/agentk/" {
		t.Errorf("no trailing slash: %d %s", got, h.Get("Location"))
	}
	req, _ = http.NewRequest(http.MethodGet, base+"/ext/agentk/api/state", nil)
	req.Header.Set("Cookie", SessionCookie+"="+cookie+"; other=1")
	req.Header.Set("X-Forwarded-For", "10.0.0.9")
	code, b, _ := body(t, req)
	if want := fmt.Sprintf("path=/api/state host=%s cookie=\"\" xff=\"\"", host); code != 200 || b != want {
		t.Errorf("proxied: %d %q, want %q", code, b, want)
	}
	if code, _ = get(t, base+"/ext/nope/x", signed, ""); code != 404 {
		t.Errorf("an unknown extension: %d", code)
	}

	// A WebSocket through the proxy stays open while its session lasts, and closes at sign-out.
	ws := dialWebSocket(t, host, "/ext/agentk/ws/events", cookie)
	if _, err := ws.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(ws, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo: %q %v", buf, err)
	}
	req, _ = http.NewRequest(http.MethodPost, base+authLogoutRoute, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", SessionCookie+"="+cookie)
	if code, _, _ := body(t, req); code != 200 {
		t.Fatalf("logout: %d", code)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := ws.Read(buf); err == nil {
		t.Fatal("the WebSocket stayed open after sign-out")
	}
	if code, _ := get(t, base+"/api/v1/probe", signed, ""); code != 401 {
		t.Errorf("after sign-out: %d", code)
	}
}

// The proxy says so when the extension isn't there to answer.
func TestWebExtensionDown(t *testing.T) {
	base, _ := startWeb(t, config.AccessLocal, filepath.Join(shortTemp(t), "gone.sock"))
	req, _ := http.NewRequest(http.MethodGet, base+"/ext/agentk/", nil)
	req.Header.Set("Accept", "text/html")
	if code, b, _ := body(t, req); code != 502 || !strings.Contains(b, "AgentK isn't running") {
		t.Errorf("page: %d %q", code, b)
	}
	code, env := get(t, base+"/api/v1/extensions", nil, "")
	rows, _ := env.Data.([]any)
	if row, _ := rowNamed(rows, "agentk"); code != 200 || row == nil || row["installed"] != true || row["available"] != false {
		t.Errorf("extensions: %d %+v", code, env)
	}
}

func dialWebSocket(t *testing.T, host, path, cookie string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_, _ = fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nCookie: %s=%s\r\n\r\n",
		path, host, SessionCookie, cookie)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	return &bufConn{Conn: c, r: br}
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// rowNamed is the extension called name in a list of them, or nil.
func rowNamed(rows []any, name string) (map[string]any, bool) {
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok && m["name"] == name {
			return m, true
		}
	}
	return nil, false
}
