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

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/tlscert"
)

// An AgentK from before Meko served it hands over at Meko's start: its port and remote access go
// into web.toml and its self-signed certificate comes along, and the web listener then serves it at
// /ext/agentk/ over HTTPS with no login: its owner is ignored. A second start changes nothing.
func TestHandoff(t *testing.T) {
	dir, port := setup(t)
	webPort := os.Getenv("MEKO_WEB_PORT")

	// AgentK's side: its socket, its own certificate, and its old login.
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
	const oldHash = "scrypt$16384$8$1$c2FsdA$aGFzaA" //nolint:gosec // G101: a made-up hash, which Meko ignores
	handoff, _ := json.Marshal(extensions.Handoff{Socket: sock, Legacy: &extensions.Legacy{Bind: "0.0.0.0",
		Port: atoi(t, webPort), TLS: &extensions.LegacyTLS{Cert: filepath.Join(agentk, "certs", tlscert.CertName),
			Key: filepath.Join(agentk, "certs", tlscert.KeyName), SelfSigned: true},
		Owner: &extensions.LegacyOwner{Username: "ana", PWScrypt: oldHash}}})
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
		_, status, _ := run("status", "--data-dir", dir, "--verbose")
		return strings.Contains(status, "Certificate:    self-signed, SHA-256 "+pair.Fingerprint)
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
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "agentk /hello" {
		t.Fatalf("AgentK through Meko: %d %q", resp.StatusCode, b)
	}
	// There's no login to sign in to, AgentK's or any other.
	resp, err = client.Post(base+"/api/v1/auth/login", "application/json", //nolint:gosec // G704: as above
		strings.NewReader(`{"username":"ana","password":"a long password"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("the login route: %d", resp.StatusCode)
	}

	// What Meko chose is in web.toml, so config.toml stays readable by a release from before it.
	if raw, _ := os.ReadFile(paths.New(dir).Config); strings.Contains(string(raw), "[web]") {
		t.Errorf("config.toml got a [web] table:\n%s", raw)
	}
	// The handoff happens once: AgentK keeps reporting its old settings, and a web.toml the owner
	// removed (to go back to the defaults) doesn't come back.
	if err = os.Remove(config.WebFile(paths.New(dir).Config)); err != nil {
		t.Fatal(err)
	}
	code, out, errs = run("restart", "--data-dir", dir, "--port", port)
	want(t, "restart", code, 0, out+errs, "  ✓ Server          running at")
	if _, err = os.Stat(config.WebFile(paths.New(dir).Config)); !os.IsNotExist(err) {
		t.Errorf("web.toml came back after the first handoff: %v", err)
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

// Presenter is served like AgentK: registered, it names its socket at Meko's start, and the web
// listener serves it at /ext/presenter/ and lists it for the sidebar.
func TestPresenterBesideMeko(t *testing.T) {
	dir, port := setup(t)
	webPort := os.Getenv("MEKO_WEB_PORT")
	home := shortDir(t)
	sock := filepath.Join(home, "web.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	echo := func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "presenter %s", r.URL.Path) //nolint:gosec // G705: a test server's plain-text reply
	}
	ext := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(echo)}
	go func() { _ = ext.Serve(ln) }()
	t.Cleanup(func() { _ = ext.Close() })
	cmd := filepath.Join(home, "me_presenter")
	reply := fmt.Sprintf(`{"socket":"%s","legacy":null}`, sock)
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = _handoff ] && { printf '%%s\\n' '%s'; exit 0; }\nexit 0\n", reply)
	if err = os.WriteFile(cmd, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	reg, _ := extensions.Load(dir)
	reg.Entries["presenter"] = extensions.Entry{Command: cmd, InstalledByMeko: true}
	if err = reg.Save(); err != nil {
		t.Fatal(err)
	}

	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	base := "http://127.0.0.1:" + webPort
	client := &http.Client{Timeout: 5 * time.Second}
	get := func(path string) (int, string) {
		resp, err := client.Get(base + path) //nolint:gosec // G704: the test's own server
		if err != nil {
			return 0, err.Error()
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	waitFor(t, "the web listener", func() bool { c, _ := get("/healthz"); return c == 200 })
	if c, b := get("/ext/presenter/decks/x/map"); c != 200 || b != "presenter /decks/x/map" {
		t.Fatalf("Presenter through Meko: %d %q", c, b)
	}
	c, b := get("/api/v1/extensions")
	var list struct {
		Data []struct {
			Name, Title, Path    string
			Installed, Available bool
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(b), &list); c != 200 || err != nil {
		t.Fatalf("extensions: %d %s %v", c, b, err)
	}
	found := false
	for _, e := range list.Data {
		if e.Name == "presenter" {
			found = e.Title == "Presenter" && e.Path == "/ext/presenter/" && e.Installed && e.Available
		}
	}
	if !found {
		t.Fatalf("extensions: no running Presenter in %s", b)
	}
}
