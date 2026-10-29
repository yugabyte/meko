package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/agents"
	"github.com/yugabyte/meko/backend/internal/cloudlink"
	"github.com/yugabyte/meko/backend/internal/paths"
)

// fakeAgents puts fake agent commands first on PATH (claude, codex and kiro-cli unless named). Each
// logs its arguments beside itself; failing makes claude's mcp add fail.
// claude mcp remove --scope user fails until an add has run, as the real one does when the scope has
// no meko server.
func fakeAgents(t *testing.T, failing bool, names ...string) {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	added := filepath.Join(bin, "added")
	fails := ""
	if failing {
		fails = "[ \"$2\" = add ] && { echo \"refused: $*\" >&2; exit 1; }\n"
	}
	if len(names) == 0 {
		names = []string{"claude", "codex", "kiro-cli"}
	}
	for _, name := range names {
		body := ""
		if name == "claude" {
			body = "[ \"$2\" = remove ] && [ \"$4\" = user ] && [ ! -e " + added + " ] && " +
				"{ echo 'No MCP server found' >&2; exit 1; }\n" + fails + "[ \"$2\" = add ] && touch " + added + "\n"
		}
		script := "#!/bin/sh\necho \"" + name + " $*\" >> " + log + "\n" + body + "exit 0\n"
		//nolint:gosec // G306: a test script
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
}

// writeFile writes a test's file, making its folder.
func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // G703: the test's own file
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil { //nolint:gosec // G703: the test's own file
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: the test's own file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// kiroFile is Kiro CLI's settings/mcp.json in the test's home.
func kiroFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("HOME"), ".kiro", "settings", "mcp.json")
}

// kiroAt writes Kiro CLI's meko server at url with tok.
func kiroAt(t *testing.T, url, tok string) {
	t.Helper()
	writeFile(t, kiroFile(t), `{"mcpServers": {"meko": {"url": "`+url+`", `+
		`"headers": {"Authorization": "Bearer `+tok+`"}}}}`)
}

// meko agent groups the agent commands: alone it lists them (exit 2), help names each, the plural is
// a typo of it, and meko connect is gone, with no pointer to it.
func TestAgentCommands(t *testing.T) {
	code, out, _ := run("agent")
	want(t, "agent alone", code, exitUsage, out, "Usage: meko agent list|connect|disconnect [<agent>] [flags]")
	for _, sub := range []string{"list", "connect", "disconnect"} {
		want(t, "agent alone", code, exitUsage, out, "\n  "+sub+" ")
		subCode, help, _ := run("agent", sub, "-h")
		want(t, "agent "+sub+" -h", subCode, 0, help, "Usage: meko agent "+sub)
		want(t, "agent "+sub+" -h", subCode, 0, help, "More: "+docsURL+"#agent-"+sub)
	}
	code, help, _ := run("help", "agent")
	if code != exitOK || help != out {
		t.Errorf("meko help agent: exit %d, %q", code, help)
	}
	_, help, _ = run("agent", "list", "-h")
	want(t, "agent list -h", 0, 0, help, "--json")
	_, usage, _ := run()
	want(t, "meko --help", 0, 0, usage, "\nConnect\n  agent list ")
	if strings.Contains(usage, "  connect ") {
		t.Errorf("meko --help lists connect:\n%s", usage)
	}
	code, _, errs := run("agents", "list")
	want(t, "agents list", code, exitUsage, errs, "Did you mean meko agent list?")
	code, _, errs = run("agent", "frob")
	want(t, "agent frob", code, exitUsage, errs, `meko agent: unknown command "frob"`)
	for _, args := range [][]string{{"connect"}, {"connect", "kiro"}} {
		code, out, errs = run(args...)
		if code != exitUsage || out != "" || !strings.HasPrefix(errs, `meko: unknown command "connect"`+"\n") ||
			strings.Contains(errs, "Did you mean") {
			t.Errorf("meko %s: exit %d, %q %q", strings.Join(args, " "), code, out, errs)
		}
	}
	code, _, errs = run("agent", "connect", "cursor", "--data-dir", t.TempDir())
	want(t, "agent connect cursor", code, exitUsage, errs,
		`meko agent connect: unknown agent "cursor" (claude, codex, kiro, opencode)`)
	code, _, errs = run("agent", "disconnect", "--data-dir", t.TempDir())
	want(t, "agent disconnect alone", code, exitUsage, errs,
		"meko agent disconnect: name the agent (claude, codex, kiro, opencode)")
}

// meko agent list shows the agents installed and whether each reaches Meko, and where they connect;
// --json is GET /api/v1/agents's data.
func TestAgentList(t *testing.T) {
	dir, port := setup(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	code, out, errs := run("agent", "list", "--data-dir", dir)
	if code != 0 || errs != "" || out != "No agents found on this computer: Meko works with Claude Code, Codex, Kiro CLI "+
		"and OpenCode\n" {
		t.Fatalf("none installed: %d %q %q", code, out, errs)
	}
	fakeAgents(t, false)
	_, tok, _ := run("token", "--data-dir", dir)
	tok = strings.TrimSpace(tok)
	home := os.Getenv("HOME")
	kiroAt(t, "http://127.0.0.1:7778/mcp", tok)
	writeFile(t, filepath.Join(home, ".codex", "config.toml"),
		"[mcp_servers.meko]\nurl = \"https://meko.example.com/mcp\"\n")
	writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"meko": {"url": "http://localhost:7790/mcp"}}}`)
	code, out, errs = run("agent", "list", "--data-dir", dir)
	if w := "Agent         Meko\n" +
		"Claude Code   needs reconnecting   meko agent connect claude\n" +
		"Codex         elsewhere: https://meko.example.com/mcp\n" +
		"Kiro CLI      connected\n" +
		"Agents connect to http://127.0.0.1:7778/mcp (Meko on this computer)\n"; code != 0 || errs != "" || out != w {
		t.Fatalf("agent list: %d\n%s%s\nwant:\n%s", code, out, errs, w)
	}
	writeFile(t, filepath.Join(home, ".claude.json"), `{}`)
	_, out, _ = run("agent", "list", "--data-dir", dir)
	want(t, "agent list", 0, 0, out, "Claude Code   not connected   meko agent connect claude\n")

	code, out, errs = run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	code, js, _ := run("agent", "list", "--data-dir", dir, "--json")
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+port+"/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var reply struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &reply); err != nil {
		t.Fatal(err)
	}
	if code != 0 || strings.TrimSpace(js) != string(reply.Data) || strings.Contains(js, tok) ||
		!strings.Contains(js, `"target":{"kind":"local","url":"http://127.0.0.1:`+port+`/mcp"}`) {
		t.Fatalf("--json:\n%s\nthe route:\n%s", js, reply.Data)
	}
}

// meko agent connect connects an agent to Meko in a line, and alone prints what any MCP client
// needs; disconnect takes it out again. Neither needs a server.
func TestAgentConnect(t *testing.T) {
	dir, _ := setup(t)
	fakeAgents(t, false, "claude", "kiro-cli")
	_, tok, _ := run("token", "--data-dir", dir)
	tok = strings.TrimSpace(tok)
	code, out, errs := run("agent", "connect", "--data-dir", dir)
	want(t, "agent connect alone", code, 0, out+errs, "Add Meko to any MCP client as a Streamable HTTP server:\n")
	for _, s := range []string{"http://127.0.0.1:7778/mcp\n", "Authorization: Bearer " + tok + "\n", tok + "\n",
		"  meko agent connect claude  Claude Code\n  meko agent connect kiro    Kiro CLI\n",
		"Meko isn't running: start it with meko start"} {
		want(t, "agent connect alone", code, 0, out, s)
	}
	if strings.Contains(out, "codex") || strings.Contains(out, "opencode") {
		t.Errorf("agent connect lists agents that aren't installed:\n%s", out)
	}

	code, out, errs = run("agent", "connect", "--data-dir", dir, "kiro")
	if code != 0 || errs != "" || out != "Connected Kiro CLI to Meko\n"+
		"Its new sessions have Meko's tools; ones already running need a restart\n"+
		"Meko isn't running: start it with meko start\n" {
		t.Fatalf("agent connect kiro: %d %q %q", code, out, errs)
	}
	if !strings.Contains(readFile(t, kiroFile(t)), "Bearer "+tok) {
		t.Errorf("mcp.json: %s", readFile(t, kiroFile(t)))
	}
	code, out, errs = run("agent", "connect", "--data-dir", dir, "codex")
	if code != 1 || out != "" || errs != "meko agent connect: Codex isn't installed\n" {
		t.Errorf("agent connect codex: %d %q %q", code, out, errs)
	}
	code, out, errs = run("agent", "disconnect", "--data-dir", dir, "kiro")
	if code != 0 || errs != "" || out != "Disconnected Kiro CLI from Meko\n" {
		t.Errorf("agent disconnect kiro: %d %q %q", code, out, errs)
	}
	code, out, _ = run("agent", "disconnect", "--data-dir", dir, "kiro")
	want(t, "agent disconnect kiro again", code, 0, out, "Kiro CLI isn't connected to Meko\n")
	kiroAt(t, "https://meko.example.com/mcp", "elsewhere")
	code, out, _ = run("agent", "disconnect", "--data-dir", dir, "kiro")
	if code != 0 || out != "Kiro CLI's meko server points at https://meko.example.com/mcp, not Meko: left as it is\n" {
		t.Errorf("agent disconnect an elsewhere: %d %q", code, out)
	}
	code, out, _ = run("agent", "connect", "--data-dir", dir, "kiro")
	want(t, "agent connect over an elsewhere", code, 0, out,
		"Connected Kiro CLI to Meko\nIt replaced the meko server at https://meko.example.com/mcp\n")

	// A failing claude says so without the token, and how to connect by hand.
	fakeAgents(t, true, "claude")
	code, out, errs = run("agent", "connect", "--data-dir", dir, "claude")
	want(t, "agent connect claude when it fails", code, 1, errs, "meko agent connect: Couldn't connect Claude Code: "+
		"claude mcp failed: refused: mcp add --scope user --transport http meko http://127.0.0.1:7778/mcp "+
		"--header Authorization: Bearer <token>. Run meko agent connect for the URL to add it by hand\n")
	if strings.Contains(out+errs, tok) {
		t.Errorf("the error shows the token: %s", out+errs)
	}

	// While Meko Cloud is linked, agents connect there, and the help names its URL but never its key.
	cloud := fakeCloud(t)
	fakeAgents(t, false, "kiro-cli")
	withStdin(t, testCloudKey+"\n")
	code, out, errs = run("cloud_link", "--data-dir", dir, "--url", cloud+"/mcp", "--web-url", cloud)
	want(t, "cloud_link", code, 0, out+errs, "Linked to")
	code, out, _ = run("agent", "connect", "--data-dir", dir)
	want(t, "agent connect alone, linked", code, 0, out, "  URL      "+cloud+"/mcp\n")
	want(t, "agent connect alone, linked", code, 0, out, "the key is in Settings › Meko Cloud")
	if strings.Contains(out, testCloudKey) || strings.Contains(out, tok) || strings.Contains(out, "isn't running") {
		t.Errorf("agent connect alone, linked:\n%s", out)
	}
	_, out, _ = run("agent", "list", "--data-dir", dir)
	want(t, "agent list, linked", 0, 0, out, "Kiro CLI   connected\nAgents connect to Meko Cloud ("+cloud+"/mcp)\n")
}

// A link change moves the agents connected to Meko to the new target, with the server stopped and
// through it: linking moves them to Meko Cloud and leaves one elsewhere, a new key rewrites them,
// unlinking moves them back with no key left in their configs, and a failed move is reported while
// the link stays.
func TestCloudLinkMovesAgents(t *testing.T) {
	dir, port := setup(t)
	cloud := fakeCloud(t)
	fakeAgents(t, false, "codex", "kiro-cli")
	home := os.Getenv("HOME")
	_, tok, _ := run("token", "--data-dir", dir)
	tok = strings.TrimSpace(tok)
	code, out, errs := run("agent", "connect", "--data-dir", dir, "kiro")
	want(t, "agent connect kiro", code, 0, out+errs, "Connected Kiro CLI to Meko")
	codex := filepath.Join(home, ".codex", "config.toml")
	elsewhere := "[mcp_servers.meko]\nurl = \"https://meko.example.com/mcp\"\n"
	writeFile(t, codex, elsewhere)

	withStdin(t, testCloudKey+"\n")
	code, out, errs = run("cloud_link", "--data-dir", dir, "--url", cloud+"/mcp", "--web-url", cloud)
	if code != 0 || errs != "" || out != "Linked to "+cloud+"/mcp (key …wxyz)\n"+
		"Moved Kiro CLI to Meko Cloud; their sessions already running need a restart\n" {
		t.Fatalf("cloud_link with no server: %d %q %q", code, out, errs)
	}
	if k := readFile(t, kiroFile(t)); !strings.Contains(k, cloud+"/mcp") || !strings.Contains(k, "Bearer "+testCloudKey) {
		t.Errorf("mcp.json after linking: %s", k)
	}
	if readFile(t, codex) != elsewhere {
		t.Errorf("codex's config changed: %s", readFile(t, codex))
	}

	// Through the server: a new key rewrites Kiro CLI's, and the reply says what moved.
	code, out, errs = run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	withStdin(t, testCloudKey2+"\n")
	code, out, errs = run("cloud_link", "--data-dir", dir)
	want(t, "a new key", code, 0, out+errs, "Moved Kiro CLI to Meko Cloud")
	if k := readFile(t, kiroFile(t)); !strings.Contains(k, "Bearer "+testCloudKey2) {
		t.Errorf("mcp.json after a new key: %s", k)
	}

	// Unlinking moves it back to this Meko, and no config keeps the key.
	code, out, errs = run("cloud_link", "--data-dir", dir, "--off")
	if code != 0 || out != "Meko Cloud is unlinked: extensions use this Meko\n"+
		"Moved Kiro CLI to Meko on this computer; their sessions already running need a restart\n" {
		t.Fatalf("cloud_link --off: %d %q %q", code, out, errs)
	}
	if k := readFile(t, kiroFile(t)); strings.Contains(k, "mko_tkn") ||
		!strings.Contains(k, "http://127.0.0.1:"+port+"/mcp") || !strings.Contains(k, "Bearer "+tok) {
		t.Errorf("mcp.json after unlinking: %s", k)
	}

	// A move that fails is reported, and the link is kept: the agent shows as needing reconnecting.
	// (With the server stopped: the running one has its own PATH.)
	code, out, errs = run("stop", "--data-dir", dir, "--keep-extensions")
	want(t, "stop", code, 0, out+errs, "stopped")
	fakeAgents(t, true, "claude", "kiro-cli")
	writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"meko": {"url": "http://127.0.0.1:`+port+`/mcp", `+
		`"headers": {"Authorization": "Bearer `+tok+`"}}}}`)
	withStdin(t, testCloudKey+"\n")
	code, out, errs = run("cloud_link", "--data-dir", dir)
	want(t, "a failed move", code, 0, out+errs,
		"Moved Kiro CLI to Meko Cloud; their sessions already running need a restart\n"+
			"Couldn't move Claude Code: claude mcp failed: refused: mcp add --scope user --transport http meko "+
			cloud+"/mcp "+
			"--header Authorization: Bearer <token>; run meko agent connect claude\n")
	if strings.Contains(out+errs, testCloudKey) {
		t.Errorf("the output shows the key: %s", out+errs)
	}
	if c := readCloudToken(t, dir); !c.Set || c.APIKey != testCloudKey {
		t.Errorf("the link after a failed move: %+v", c)
	}
	_, out, _ = run("agent", "list", "--data-dir", dir)
	want(t, "agent list after a failed move", 0, 0, out, "Claude Code   needs reconnecting   meko agent connect claude\n")
}

// A Meko linked before it moved agents on a link change moves them to Meko Cloud at its first start,
// and only then.
func TestFirstStartMovesAgentsOnce(t *testing.T) {
	dir, port := setup(t)
	cloud := fakeCloud(t)
	fakeAgents(t, false, "kiro-cli")
	withStdin(t, testCloudKey+"\n")
	code, out, errs := run("cloud_link", "--data-dir", dir, "--url", cloud+"/mcp", "--web-url", cloud)
	want(t, "cloud_link", code, 0, out+errs, "Linked to")
	_, tok, _ := run("token", "--data-dir", dir)
	tok = strings.TrimSpace(tok)
	local := "http://127.0.0.1:" + port + "/mcp"
	kiroAt(t, local, tok)

	code, out, errs = run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(readFile(t, kiroFile(t)), "Bearer "+testCloudKey) {
		if time.Now().After(deadline) {
			t.Fatalf("the first start didn't move Kiro CLI: %s", readFile(t, kiroFile(t)))
		}
		time.Sleep(100 * time.Millisecond)
	}
	code, out, errs = run("stop", "--data-dir", dir, "--keep-extensions")
	want(t, "stop", code, 0, out+errs, "stopped")
	kiroAt(t, local, tok)
	code, out, errs = run("start", "--data-dir", dir, "--port", port)
	want(t, "start again", code, 0, out+errs, "is running")
	time.Sleep(time.Second)
	if k := readFile(t, kiroFile(t)); !strings.Contains(k, local) {
		t.Errorf("the second start moved Kiro CLI again: %s", k)
	}
	if _, err := os.Stat(agentsLock(paths.New(dir))); err != nil {
		t.Errorf("the move took no lock: %v", err)
	}
}

// meko cloud_link waits for as long as the server's link change can take: its checks and the
// agents' move.
func TestCloudLinkWaitsForTheMove(t *testing.T) {
	if cloudTimeout <= 2*cloudlink.CheckTimeout+agents.MoveTimeout {
		t.Errorf("cloudTimeout %s is shorter than a link change can take", cloudTimeout)
	}
}
