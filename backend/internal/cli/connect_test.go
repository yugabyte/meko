package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// fakeAgents puts fake claude and opencode commands first on PATH. Each logs its arguments, a line
// a run, to the returned log. claude mcp remove fails until a claude mcp add has run, as the real
// one does when the scope has no meko server; failing makes every add fail, echoing its arguments.
func fakeAgents(t *testing.T, failing bool) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "log")
	added := filepath.Join(bin, "added")
	fails := ""
	if failing {
		fails = "[ \"$2\" = add ] && { echo \"refused: $*\" >&2; exit 1; }\n"
	}
	scripts := map[string]string{
		"claude": "[ \"$2\" = remove ] && [ ! -e " + added + " ] && { echo 'No MCP server found' >&2; exit 1; }\n" +
			fails + "[ \"$2\" = add ] && touch " + added + "\n",
		"opencode": fails,
	}
	for name, body := range scripts {
		script := "#!/bin/sh\necho \"" + name + " $*\" >> " + log + "\n" + body + "exit 0\n"
		p := filepath.Join(bin, name)
		if err := os.WriteFile(p, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test script
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	return log
}

// meko connect with no agent prints what any MCP client needs, and the agents it connects itself.
func TestConnectPrintsTheURLAndToken(t *testing.T) {
	dir, _ := setup(t)
	_, tok, _ := run("token", "--data-dir", dir)
	tok = strings.TrimSpace(tok)
	code, out, errs := run("connect", "--data-dir", dir)
	want(t, "connect", code, 0, out+errs, "Streamable HTTP")
	for _, s := range []string{"http://127.0.0.1:7778/mcp\n", "Authorization: Bearer " + tok + "\n", tok + "\n",
		"meko connect claude", "meko connect codex", "meko connect kiro", "meko connect opencode",
		"Meko isn't running: start it with meko start"} {
		want(t, "connect", code, 0, out, s)
	}
	code, _, errs = run("connect", "--data-dir", dir, "cursor")
	want(t, "connect to an unknown agent", code, exitUsage, errs, `unknown agent "cursor" (claude, codex, kiro, opencode)`)
	code, _, errs = run("token", "--data-dir", dir, "--mcp")
	want(t, "token --mcp is gone", code, exitUsage, errs, "flag provided but not defined: -mcp")
}

// Claude Code: the meko server goes at user scope, replacing one there, with the token in a header.
func TestConnectClaude(t *testing.T) {
	dir, _ := setup(t)
	log := fakeAgents(t, false)
	_, tok, _ := run("token", "--data-dir", dir)
	tok = strings.TrimSpace(tok)
	add := "claude mcp add --scope user --transport http meko http://127.0.0.1:7778/mcp --header Authorization: Bearer " +
		tok + "\n"
	code, out, errs := run("connect", "--data-dir", dir, "claude")
	want(t, "connect claude", code, 0, out+errs, "Connected Claude Code to Meko: added the MCP server meko at "+
		"http://127.0.0.1:7778/mcp for all your projects (user scope)\n")
	if got := readLog(t, log); got != "claude mcp remove --scope user meko\n"+add {
		t.Errorf("claude ran:\n%s", got)
	}
	code, out, errs = run("connect", "--data-dir", dir, "claude")
	want(t, "connect claude again", code, 0, out+errs, "(user scope), replacing the one it had\n")

	// A failing claude says so without the token, and how to connect by hand.
	log = fakeAgents(t, true)
	code, out, errs = run("connect", "--data-dir", dir, "claude")
	want(t, "connect claude when it fails", code, 1, out+errs,
		"connecting Claude Code: claude mcp failed: refused: mcp add --scope user --transport http meko "+
			"http://127.0.0.1:7778/mcp --header Authorization: Bearer <token>. Run meko connect for the URL and token")
	if strings.Contains(out+errs, tok) {
		t.Errorf("the error shows the token: %s", out+errs)
	}
	if !strings.Contains(readLog(t, log), "claude mcp add") {
		t.Errorf("claude wasn't run")
	}

	t.Setenv("PATH", "/usr/bin:/bin")
	code, _, errs = run("connect", "--data-dir", dir, "claude")
	want(t, "connect claude without claude", code, 1, errs, "connecting Claude Code: claude isn't on PATH")
}

// OpenCode: opencode mcp add, to its global config, with the header as name=value.
func TestConnectOpenCode(t *testing.T) {
	dir, _ := setup(t)
	log := fakeAgents(t, false)
	_, tok, _ := run("token", "--data-dir", dir)
	code, out, errs := run("connect", "--data-dir", dir, "opencode")
	want(t, "connect opencode", code, 0, out+errs, "Connected OpenCode to Meko")
	if got, w := readLog(t, log), "opencode mcp add meko --url http://127.0.0.1:7778/mcp --header Authorization=Bearer "+
		strings.TrimSpace(tok)+" --global\n"; got != w {
		t.Errorf("opencode ran %q, want %q", got, w)
	}
}

// Kiro CLI: ~/.kiro/settings/mcp.json gets the meko server, made 0600 when new, and an existing
// file keeps its other servers and keys.
func TestConnectKiro(t *testing.T) {
	dir, _ := setup(t)
	_, tok, _ := run("token", "--data-dir", dir)
	tok = strings.TrimSpace(tok)
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".kiro", "settings", "mcp.json")
	type server struct {
		URL     string            `json:"url"`
		Command string            `json:"command"`
		Headers map[string]string `json:"headers"`
	}
	read := func() (doc struct {
		MCPServers map[string]server `json:"mcpServers"`
		Other      string            `json:"other"`
	}) {
		t.Helper()
		b, err := os.ReadFile(path) //nolint:gosec // G304: the test's own file
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
		return doc
	}
	code, out, errs := run("connect", "--data-dir", dir, "kiro")
	want(t, "connect kiro", code, 0, out+errs, "Connected Kiro CLI to Meko: wrote the MCP server meko at "+
		"http://127.0.0.1:7778/mcp to "+path+"\n")
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mcp.json: %v %v", fi.Mode(), err)
	}
	s := read().MCPServers["meko"]
	if s.URL != "http://127.0.0.1:7778/mcp" || s.Headers["Authorization"] != "Bearer "+tok {
		t.Errorf("meko server %+v", s)
	}

	old := `{"other": "kept", "mcpServers": {"git": {"command": "git-mcp"}, ` +
		`"meko": {"url": "https://meko.example.com/mcp", "headers": {"Authorization": "Bearer cloud"}}}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs = run("connect", "--data-dir", dir, "kiro")
	want(t, "connect kiro over Meko Cloud", code, 0, out+errs, " (replacing the one at https://meko.example.com/mcp)\n")
	doc := read()
	if doc.Other != "kept" || doc.MCPServers["git"].Command != "git-mcp" ||
		doc.MCPServers["meko"].Headers["Authorization"] != "Bearer "+tok {
		t.Errorf("mcp.json %+v", doc)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs = run("connect", "--data-dir", dir, "kiro")
	want(t, "connect kiro with a broken file", code, 1, errs, "can't read "+path)
}

// Codex: config.toml in CODEX_HOME gets an [mcp_servers.meko] table with the header; the old one and
// its subtables go, and the rest of the file stays as it was, comments too.
func TestConnectCodex(t *testing.T) {
	dir, _ := setup(t)
	_, tok, _ := run("token", "--data-dir", dir)
	tok = strings.TrimSpace(tok)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "config.toml")
	old := `# my settings
model = "o3"

[mcp_servers.meko]
url = "https://meko.example.com/mcp"

[mcp_servers.meko.env_http_headers]
X-Old = "OLD"

[mcp_servers.git] # kept
command = "git-mcp"
`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("connect", "--data-dir", dir, "codex")
	want(t, "connect codex", code, 0, out+errs, "Connected Codex to Meko: wrote the MCP server meko at "+
		"http://127.0.0.1:7778/mcp to "+path+" (replacing the one at https://meko.example.com/mcp)\n")
	b, err := os.ReadFile(path) //nolint:gosec // G304: the test's own file
	if err != nil {
		t.Fatal(err)
	}
	w := `# my settings
model = "o3"

[mcp_servers.git] # kept
command = "git-mcp"

[mcp_servers.meko]
url = "http://127.0.0.1:7778/mcp"
http_headers = { Authorization = "Bearer ` + tok + `" }
`
	if string(b) != w {
		t.Errorf("config.toml:\n%s\nwant:\n%s", b, w)
	}
	var cfg codexServers
	if _, err := toml.Decode(string(b), &cfg); err != nil || cfg.MCPServers["git"].URL != "" {
		t.Errorf("%v %+v", err, cfg)
	}

	// meko set some other way than its own table can't be replaced, and the file isn't touched.
	odd := "mcp_servers.meko.url = \"https://meko.example.com/mcp\"\n"
	if err := os.WriteFile(path, []byte(odd), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs = run("connect", "--data-dir", dir, "codex")
	want(t, "connect codex over a dotted key", code, 1, errs, "has a meko server meko connect can't replace")
	if b, _ := os.ReadFile(path); string(b) != odd { //nolint:gosec // G304: the test's own file
		t.Errorf("config.toml changed: %s", b)
	}

	// With no file, it's made, 0600.
	t.Setenv("CODEX_HOME", filepath.Join(home, "new"))
	code, out, errs = run("connect", "--data-dir", dir, "codex")
	want(t, "connect codex with no config", code, 0, out+errs, "Connected Codex to Meko")
	if fi, err := os.Stat(filepath.Join(home, "new", "config.toml")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config.toml: %v %v", fi, err)
	}
}
