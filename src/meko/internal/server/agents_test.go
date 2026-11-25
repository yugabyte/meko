package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/agents"
	"github.com/yugabyte/meko/src/meko/internal/config"
)

// agentsHome gives a test a home of its own with Kiro CLI installed (a fake kiro-cli first on PATH)
// and nothing else of this computer's: no other agent, no system folders, no login shell, no config
// folders.
func agentsHome(t *testing.T) string {
	t.Helper()
	old, oldApps, oldShell := agents.SystemFolders, agents.Applications, agents.LoginShell
	agents.SystemFolders, agents.Applications, agents.LoginShell = nil, "", ""
	t.Cleanup(func() { agents.SystemFolders, agents.Applications, agents.LoginShell = old, oldApps, oldShell })
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "KIRO_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
	}
	bin := t.TempDir()
	//nolint:gosec // G306: a test script
	if err := os.WriteFile(filepath.Join(bin, "kiro-cli"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	return home
}

// agentsServer runs a listener (the API listener, or the web listener with local access) with the
// agents' routes for target, and returns its URL.
func agentsServer(t *testing.T, web bool, target agents.Target) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Info: Info{Version: "test"}, Port: tcpPort(t, ln), Token: token, ClientToken: clientToken,
		Logger: slog.New(slog.DiscardHandler), Agents: &agents.Manager{Lock: filepath.Join(t.TempDir(), "agents.lock"),
			Target: func() (agents.Target, error) { return target, nil }}}
	if web {
		o.Token, o.ClientToken = "", ""
		o.Web = &Web{Access: config.AccessLocal}
	}
	s := New(o)
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return fmt.Sprintf("http://127.0.0.1:%d", tcpPort(t, ln))
}

// On the API listener, GET /api/v1/agents takes any token, and connect and disconnect only the admin
// token: a client token gets 403 OWNER_ONLY. No reply holds the token.
func TestAgentsOnTheAPIListener(t *testing.T) {
	home := agentsHome(t)
	local := agents.Target{Kind: agents.KindLocal, URL: "http://127.0.0.1:7777/mcp", Token: "agents-token-0123456789"}
	base := agentsServer(t, false, local)
	client := map[string]string{"Authorization": "Bearer " + clientToken}
	admin := map[string]string{"Authorization": "Bearer " + token}

	code, b := send(t, http.MethodGet, base+"/api/v1/agents", "", nil)
	if code != 401 {
		t.Fatalf("GET with no token: %d %s", code, b)
	}
	code, b = send(t, http.MethodGet, base+"/api/v1/agents", "", client)
	var list struct {
		Data agents.List `json:"data"`
	}
	if code != 200 || json.Unmarshal([]byte(b), &list) != nil || list.Data.Target.Kind != "local" ||
		len(list.Data.Agents) != 1 || list.Data.Agents[0].Name != "kiro" || list.Data.Agents[0].State != "none" ||
		!strings.Contains(b, `"url":null`) {
		t.Fatalf("GET with the client token: %d %s", code, b)
	}
	code, b = send(t, http.MethodPost, base+"/api/v1/agents/kiro/connect", "", client)
	if code != 403 || !strings.Contains(b, "OWNER_ONLY") {
		t.Fatalf("connect with the client token: %d %s", code, b)
	}
	code, b = send(t, http.MethodPost, base+"/api/v1/agents/kiro/connect", "", admin)
	if code != 200 || !strings.Contains(b, `"state":"connected"`) || !strings.Contains(b, `"notes":[]`) {
		t.Fatalf("connect with the admin token: %d %s", code, b)
	}
	path := filepath.Join(home, ".kiro", "settings", "mcp.json")
	mcp, _ := os.ReadFile(path) //nolint:gosec // G304: the test's own file
	if !strings.Contains(string(mcp), "Bearer "+local.Token) {
		t.Errorf("mcp.json: %s", mcp)
	}
	for _, c := range []struct {
		path, code string
		status     int
	}{
		{"/api/v1/agents/windsurf/connect", "UNKNOWN_AGENT", 404},
		{"/api/v1/agents/codex/connect", "AGENT_NOT_INSTALLED", 409},
		{"/api/v1/agents/opencode/disconnect", "AGENT_NOT_INSTALLED", 409},
	} {
		code, b = send(t, http.MethodPost, base+c.path, "", admin)
		if code != c.status || !strings.Contains(b, c.code) {
			t.Errorf("%s: %d %s", c.path, code, b)
		}
	}
	code, b = send(t, http.MethodPost, base+"/api/v1/agents/kiro/disconnect", "", client)
	if code != 403 {
		t.Fatalf("disconnect with the client token: %d %s", code, b)
	}
	code, b = send(t, http.MethodPost, base+"/api/v1/agents/kiro/disconnect", "", admin)
	if code != 200 || !strings.Contains(b, `"state":"none"`) {
		t.Fatalf("disconnect with the admin token: %d %s", code, b)
	}

	// A broken file fails the connect with 502, and no reply holds the token.
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, b = send(t, http.MethodPost, base+"/api/v1/agents/kiro/connect", "", admin)
	if code != 502 || !strings.Contains(b, "AGENT_CLI_FAILED") || strings.Contains(b, local.Token) {
		t.Fatalf("a failing connect: %d %s", code, b)
	}
}

// On the web listener the owner (local access here) lists, connects and disconnects. While Meko Cloud
// is linked the target is Meko Cloud, and an agent there with its key is connected; one pointing
// elsewhere is left as it is on disconnect.
func TestAgentsOnTheWebListener(t *testing.T) {
	home := agentsHome(t)
	cloud := agents.Target{Kind: agents.KindCloud, URL: "https://cloud.example.com/mcp", Token: cloudKey}
	base := agentsServer(t, true, cloud)
	code, b := send(t, http.MethodPost, base+"/api/v1/agents/kiro/connect", "", nil)
	if code != 200 || !strings.Contains(b, `"state":"connected"`) || strings.Contains(b, cloud.Token) {
		t.Fatalf("connect: %d %s", code, b)
	}
	code, b = send(t, http.MethodGet, base+"/api/v1/agents", "", nil)
	if code != 200 || !strings.Contains(b, `"target":{"kind":"cloud","url":"https://cloud.example.com/mcp"}`) ||
		!strings.Contains(b, `"url":"https://cloud.example.com/mcp"`) || strings.Contains(b, cloud.Token) {
		t.Fatalf("GET: %d %s", code, b)
	}
	path := filepath.Join(home, ".kiro", "settings", "mcp.json")
	other := `{"mcpServers": {"meko": {"url": "https://meko.example.com/mcp"}}}`
	if err := os.WriteFile(path, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	code, b = send(t, http.MethodPost, base+"/api/v1/agents/kiro/disconnect", "", nil)
	if code != 200 || !strings.Contains(b, "Kiro CLI's meko server points at https://meko.example.com/mcp, not Meko: "+
		"left as it is") {
		t.Fatalf("disconnect an elsewhere: %d %s", code, b)
	}
	if got, _ := os.ReadFile(path); string(got) != other { //nolint:gosec // G304: the test's file
		t.Errorf("mcp.json changed: %s", got)
	}
	code, _ = send(t, http.MethodPost, base+"/api/v1/agents/kiro/connect", "",
		map[string]string{"Origin": "http://example.com"})
	if code != 403 {
		t.Errorf("another site's page: %d", code)
	}
}
