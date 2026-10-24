package cli

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/auth"
	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/tlscert"
)

// An AgentK from before Meko served it hands over at Meko's start: its port and remote access go
// into config.toml, its self-signed certificate and its login come along, and the web listener then
// serves it at /ext/agentk/ over HTTPS with that login. A second start changes nothing.
func TestHandoff(t *testing.T) {
	dir, port := setup(t)
	webPort := os.Getenv("MEKO_WEB_PORT")

	// AgentK's side: its socket, its own certificate, and its login.
	agentk := shortDir(t)
	sock := filepath.Join(agentk, "web.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	echo := func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "agentk %s", r.URL.Path) //nolint:gosec // G705: a test server's plain-text reply
	}
	ext := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(echo)}
	go func() { _ = ext.Serve(ln) }()
	t.Cleanup(func() { _ = ext.Close() })
	pair, err := tlscert.SelfSigned(filepath.Join(agentk, "certs"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("a long password")
	handoff, _ := json.Marshal(extensions.Handoff{Socket: sock, Legacy: &extensions.Legacy{Bind: "0.0.0.0",
		Port: atoi(t, webPort), TLS: &extensions.LegacyTLS{Cert: filepath.Join(agentk, "certs", tlscert.CertName),
			Key: filepath.Join(agentk, "certs", tlscert.KeyName), SelfSigned: true},
		Owner: &extensions.LegacyOwner{Username: "ana", PWScrypt: hash}}})
	cmd := filepath.Join(agentk, "agentk")
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = _handoff ] && { printf '%%s\\n' '%s'; exit 0; }\nexit 0\n", handoff)
	if err = os.WriteFile(cmd, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	reg, _ := extensions.Load(dir)
	reg.Entries["agentk"] = extensions.Entry{Command: cmd, InstalledByMeko: true}
	if err = reg.Save(); err != nil {
		t.Fatal(err)
	}

	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	waitFor(t, "the web listener", func() bool {
		_, status, _ := run("status", "--data-dir", dir)
		return strings.Contains(status, "Certificate:   self-signed, SHA-256 "+pair.Fingerprint)
	})
	c, err := config.Load(paths.New(dir).Config, func(string) string { return "" }, config.Overrides{})
	if err != nil || c.Web.Access != config.AccessRemote || c.Web.Port != atoi(t, webPort) {
		t.Fatalf("config after the handoff: %+v %v", c.Web, err)
	}

	//nolint:gosec // G402: the test's own self-signed certificate
	insecure := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Timeout: 5 * time.Second, Transport: insecure,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "https://127.0.0.1:" + webPort
	resp, err := client.Get(base + "/ext/agentk/hello") //nolint:gosec // G704: the test's own server
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("AgentK without signing in: %d", resp.StatusCode)
	}
	resp, err = client.Post(base+"/api/v1/auth/login", "application/json", //nolint:gosec // G704: as above
		strings.NewReader(`{"username":"ana","password":"a long password"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var session *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == "meko_session" {
			session = ck
		}
	}
	if resp.StatusCode != 200 || session == nil || !session.Secure {
		t.Fatalf("AgentK's login: %d %+v", resp.StatusCode, session)
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/ext/agentk/hello", nil) //nolint:gosec // G704: as above
	req.AddCookie(session)
	resp, err = client.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "agentk /hello" {
		t.Fatalf("AgentK through Meko: %d %q", resp.StatusCode, b)
	}

	// Again: nothing is added twice.
	code, out, errs = run("restart", "--data-dir", dir, "--port", port)
	want(t, "restart", code, 0, out+errs, "is running")
	raw, _ := os.ReadFile(paths.New(dir).Config)
	if n := strings.Count(string(raw), "[web]"); n != 1 {
		t.Errorf("config.toml has %d [web] tables:\n%s", n, raw)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("%s didn't come up in time", what)
}
