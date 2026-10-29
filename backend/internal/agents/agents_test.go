package agents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// The tests never read or change this computer's own agents: no system folders, no login shell, and
// every config folder an environment variable could point at is the test's.
func TestMain(m *testing.M) {
	SystemFolders = nil
	LoginShell = ""
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "KIRO_HOME", "XDG_CONFIG_HOME"} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

const (
	token    = "test-token-0123456789"
	cloudKey = "mko_tkn_cloud_0123456789wxyz"
	localURL = "http://127.0.0.1:7778/mcp"
	cloudURL = "https://cloud.example.com/mcp"
	otherURL = "https://meko.example.com/mcp"
)

var (
	local = Target{Kind: KindLocal, URL: localURL, Token: token}
	cloud = Target{Kind: KindCloud, URL: cloudURL, Token: cloudKey}
)

// realPath is dir with its symlinks resolved, as the fake claude logs the folder it ran in (pwd -P):
// on macOS a test's temp folder is under /var, a symlink to /private/var.
func realPath(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeAgents gives a test its own home and a PATH of fake agent commands (all four unless named),
// first, then /usr/bin:/bin. claude and opencode log their arguments, a line a run, to the returned
// log (claude's local-scope remove adds the folder it ran in). claude mcp remove --scope user fails
// until a claude mcp add has run (or an "added" file is made beside the log), as the real one does
// when the scope has no meko server; failing makes every add fail, echoing its arguments.
func fakeAgents(t *testing.T, failing bool, names ...string) (home, log string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "KIRO_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
	}
	bin := t.TempDir()
	log = filepath.Join(bin, "log")
	added := filepath.Join(bin, "added")
	fails := ""
	if failing {
		fails = "[ \"$2\" = add ] && { echo \"refused: $*\" >&2; exit 1; }\n"
	}
	scripts := map[string]string{
		"claude": "[ \"$4\" = local ] && echo \"  in $(pwd -P)\" >> " + log + "\n" +
			"[ \"$2\" = remove ] && [ \"$4\" = user ] && [ ! -e " + added + " ] && " +
			"{ echo 'No MCP server found' >&2; exit 1; }\n" +
			fails + "[ \"$2\" = add ] && touch " + added + "\n",
		"opencode": fails,
		"codex":    "",
		"kiro-cli": "",
	}
	if len(names) == 0 {
		names = []string{"claude", "opencode", "codex", "kiro-cli"}
	}
	for _, name := range names {
		script := "#!/bin/sh\necho \"" + name + " $*\" >> " + log + "\n" + scripts[name] + "exit 0\n"
		//nolint:gosec // G306: a test script
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	return home, log
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: the test's own file
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	return string(b)
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: the test's own file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func manager(t Target) *Manager {
	return &Manager{Target: func() (Target, error) { return t, nil }}
}

// entry is an MCP server entry with a header, as Claude Code and Kiro CLI write them.
func entry(url, tok string) string {
	return `{"type": "http", "url": "` + url + `", "headers": {"Authorization": "Bearer ` + tok + `"}}`
}

// rows is a list's agents by name.
func rows(t *testing.T, m *Manager) map[string]Row {
	t.Helper()
	l, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Row{}
	for _, r := range l.Agents {
		out[r.Name] = r
	}
	return out
}

func states(t *testing.T, m *Manager) map[string]string {
	t.Helper()
	out := map[string]string{}
	for n, r := range rows(t, m) {
		out[n] = r.State
	}
	return out
}

func wantStates(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %v, want %v", what, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s is %q, want %q (%v)", what, k, got[k], v, got)
		}
	}
}

// Each agent's state comes from its own config: connected at the target with its token, stale at one
// of Meko's own otherwise, elsewhere at any other URL, none without one. While Meko Cloud is linked
// the target is Meko Cloud, and an entry at this Meko is stale. An agent that isn't installed isn't
// listed.
func TestListStates(t *testing.T) {
	home, _ := fakeAgents(t, false)
	l, err := manager(local).List()
	if err != nil {
		t.Fatal(err)
	}
	if l.Target != (TargetInfo{Kind: KindLocal, URL: localURL}) || len(l.Agents) != 4 {
		t.Fatalf("list %+v", l)
	}
	for i, n := range []string{"claude", "codex", "kiro", "opencode"} {
		r := l.Agents[i]
		if r.Name != n || r.State != StateNone || r.URL != nil || r.Notes == nil || !strings.HasPrefix(r.Command, "/") {
			t.Errorf("row %d: %+v", i, r)
		}
	}

	write(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"meko": `+entry(localURL, token)+`}}`)
	write(t, filepath.Join(home, ".kiro", "settings", "mcp.json"),
		`{"mcpServers": {"meko": `+entry("http://localhost:7790/mcp", "old")+`}}`)
	write(t, filepath.Join(home, ".codex", "config.toml"),
		"[mcp_servers.meko]\nurl = \""+otherURL+"\"\nhttp_headers = { Authorization = \"Bearer x\" }\n")
	write(t, filepath.Join(home, ".config", "opencode", "opencode.json"),
		`{"mcp": {"servers": {"meko": {"type": "remote", "url": "`+cloudURL+`", "headers": {"Authorization": "Bearer `+
			cloudKey+`"}}}}}`)
	wantStates(t, "unlinked", states(t, manager(local)),
		map[string]string{"claude": StateConnected, "kiro": StateStale, "codex": StateElsewhere, "opencode": StateElsewhere})
	wantStates(t, "linked", states(t, manager(cloud)),
		map[string]string{"claude": StateStale, "kiro": StateStale, "codex": StateElsewhere, "opencode": StateConnected})
	r := rows(t, manager(cloud))["opencode"]
	if r.URL == nil || *r.URL != cloudURL {
		t.Errorf("opencode's url: %+v", r)
	}
	b, _ := json.Marshal(rows(t, manager(cloud)))
	if strings.Contains(string(b), token) || strings.Contains(string(b), cloudKey) {
		t.Errorf("a row holds a token: %s", b)
	}

	// No token, or the wrong one, at the target is stale too.
	write(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"meko": {"type": "http", "url": "`+localURL+`"}}}`)
	if s := states(t, manager(local))["claude"]; s != StateStale {
		t.Errorf("claude without a token: %s", s)
	}

	// Only installed agents are listed, with their command found in the usual folders too.
	_, _ = fakeAgents(t, false, "codex")
	h, _ := os.UserHomeDir()
	write(t, filepath.Join(h, ".local", "bin", "kiro-cli"), "#!/bin/sh\n")
	//nolint:gosec // G302: a test script
	if err := os.Chmod(filepath.Join(h, ".local", "bin", "kiro-cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	l, _ = manager(local).List()
	if len(l.Agents) != 2 || l.Agents[0].Name != "codex" || l.Agents[1].Command != "~/.local/bin/kiro-cli" {
		t.Errorf("installed: %+v", l.Agents)
	}
	_, _ = fakeAgents(t, false, "codex")
	if _, err := manager(local).Connect(context.Background(), "kiro"); !isCode(err, CodeNotInstalled) ||
		err.Error() != "Kiro CLI isn't installed" {
		t.Errorf("connect kiro, not installed: %v", err)
	}
	if _, err := manager(local).Disconnect(context.Background(), "cursor"); !isCode(err, CodeUnknown) {
		t.Errorf("an unknown agent: %v", err)
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	if l, _ := manager(local).List(); len(l.Agents) != 0 || l.Agents == nil {
		t.Errorf("none installed: %+v", l)
	}
}

// loginShell makes the test's login shell a fake called name, which counts its runs in the returned
// file and runs its -c script (the last argument) with PATH set to path, after a profile's chatter.
func loginShell(t *testing.T, name, path string) (runs string) {
	t.Helper()
	dir := t.TempDir()
	runs = filepath.Join(dir, "runs")
	write(t, filepath.Join(dir, name), "#!/bin/sh\necho run >> "+runs+"\necho 'Welcome back'\n"+
		"for a; do script=$a; done\nPATH="+path+"; export PATH\neval \"$script\"\necho bye\n")
	//nolint:gosec // G302: a test script
	if err := os.Chmod(filepath.Join(dir, name), 0o700); err != nil {
		t.Fatal(err)
	}
	old, oldTTL := LoginShell, LoginPathTTL
	LoginShell = filepath.Join(dir, name)
	t.Cleanup(func() { LoginShell, LoginPathTTL = old, oldTTL })
	return runs
}

// refreshed waits for the login shell's PATH being read in the background.
func refreshed(t *testing.T) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		loginPathCache.Lock()
		busy := loginPathCache.refreshing
		loginPathCache.Unlock()
		if !busy {
			return
		}
		if time.Now().After(end) {
			t.Fatal("the login shell's PATH is still being read")
		}
	}
}

// An agent the server's PATH and the usual folders lack, such as Claude Code installed with npm under
// nvm, is found on the user's login shell's PATH, and its CLI runs with that PATH: its node is in
// another of its folders. The shell runs only when an agent isn't found otherwise, and once a
// LoginPathTTL, in the background, the list going on with the PATH it had.
func TestFindOnTheLoginShellsPath(t *testing.T) {
	_, log := fakeAgents(t, false, "codex")
	nvm := filepath.Join(t.TempDir(), "nvm", "versions", "node", "v22", "bin")
	tools := filepath.Join(t.TempDir(), "homebrew", "bin")
	write(t, filepath.Join(nvm, "claude"), "#!/bin/sh\ncommand -v node >/dev/null || "+
		"{ echo 'env: node: No such file or directory' >&2; exit 127; }\necho \"claude $*\" >> "+log+"\n")
	write(t, filepath.Join(tools, "node"), "#!/bin/sh\n")
	for _, p := range []string{filepath.Join(nvm, "claude"), filepath.Join(tools, "node")} {
		//nolint:gosec // G302: a test script
		if err := os.Chmod(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runs := loginShell(t, "zsh", "relative/bin:"+nvm+":"+tools+":/usr/bin:/bin")
	LoginPathTTL = time.Hour
	l, _ := manager(local).List()
	if len(l.Agents) != 2 || l.Agents[0].Name != "claude" || l.Agents[0].Command != filepath.Join(nvm, "claude") ||
		l.Agents[1].Name != "codex" {
		t.Fatalf("installed: %+v", l.Agents)
	}
	if _, err := manager(local).Connect(context.Background(), "claude"); err != nil ||
		!strings.Contains(readLog(t, log), "claude mcp add") {
		t.Errorf("connect claude: %v", err)
	}
	if n := strings.Count(readLog(t, runs), "run"); n != 1 {
		t.Errorf("the login shell ran %d times, want once", n)
	}
	LoginPathTTL = 0
	if p, ok := Catalog[0].Find(); !ok || p != filepath.Join(nvm, "claude") {
		t.Errorf("after the TTL: %q, %v", p, ok)
	}
	refreshed(t)
	if n := strings.Count(readLog(t, runs), "run"); n != 1 {
		t.Errorf("after the TTL, the login shell ran %d times, want once", n)
	}

	// An agent on PATH never runs the shell, nor gets its PATH.
	LoginPathTTL = time.Hour
	if _, ok := Catalog[1].Find(); !ok || readLog(t, runs) != "" {
		t.Errorf("codex on PATH ran the login shell")
	}
	if p := loginPathOf(filepath.Join(filepath.Dir(log), "codex")); p != nil {
		t.Errorf("codex's PATH: %v", p)
	}

	// Other shells give way to sh's login profile.
	_ = loginShell(t, "tcsh", "/nowhere")
	h, _ := os.UserHomeDir()
	write(t, filepath.Join(h, ".profile"), "PATH="+nvm+":$PATH\n")
	if p, ok := Catalog[0].Find(); !ok || p != filepath.Join(nvm, "claude") {
		t.Errorf("through sh's profile: %q, %v", p, ok)
	}
}

// Only a service looks on the login shell's PATH: Meko started from the user's shell has it already.
func TestLoginShellOnlyForAService(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("MEKO_SERVICE", "")
	if s := defaultShell(); s != "" {
		t.Errorf("outside a service: %q", s)
	}
	t.Setenv("MEKO_SERVICE", "launchd")
	if s := defaultShell(); s != "/bin/zsh" {
		t.Errorf("under launchd: %q", s)
	}
	t.Setenv("SHELL", "")
	if s := defaultShell(); s != "/bin/sh" {
		t.Errorf("with no SHELL: %q", s)
	}
}

// A login shell that fails, prints no PATH or hangs finds nothing, and one that hangs is killed, its
// children too, at LoginPathTimeout.
func TestLoginShellThatFails(t *testing.T) {
	_, _ = fakeAgents(t, false, "codex")
	old := LoginPathTimeout
	LoginPathTimeout = 300 * time.Millisecond
	t.Cleanup(func() { LoginPathTimeout = old })
	pids := filepath.Join(t.TempDir(), "pids")
	for _, script := range []string{"exit 1", "echo /usr/bin", "sleep 30 & echo $! > " + pids + "; exec sleep 30"} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "bash"), "#!/bin/sh\n"+script+"\n")
		//nolint:gosec // G302: a test script
		if err := os.Chmod(filepath.Join(dir, "bash"), 0o700); err != nil {
			t.Fatal(err)
		}
		oldShell := LoginShell
		LoginShell = filepath.Join(dir, "bash")
		start := time.Now()
		p, ok := Catalog[0].Find()
		LoginShell = oldShell
		if ok || time.Since(start) > 5*time.Second {
			t.Errorf("%q: found %q in %s", script, p, time.Since(start))
		}
	}
	pid, err := strconv.Atoi(strings.TrimSpace(read(t, pids)))
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the hanging shell's child is still running")
		}
	}
}

func isCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// CLAUDE_CONFIG_DIR, CODEX_HOME, KIRO_HOME and XDG_CONFIG_HOME move the agents' configs, as each agent
// (and AgentK) reads them.
func TestConfigFolders(t *testing.T) {
	fakeAgents(t, false)
	dirs := map[string]string{}
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "KIRO_HOME", "XDG_CONFIG_HOME"} {
		dirs[k] = t.TempDir()
		t.Setenv(k, dirs[k])
	}
	write(t, filepath.Join(dirs["CLAUDE_CONFIG_DIR"], ".claude.json"),
		`{"mcpServers": {"meko": `+entry(localURL, token)+`}}`)
	write(t, filepath.Join(dirs["KIRO_HOME"], "settings", "mcp.json"),
		`{"mcpServers": {"meko": `+entry(localURL, token)+`}}`)
	write(t, filepath.Join(dirs["CODEX_HOME"], "config.toml"),
		"[mcp_servers.meko]\nurl = \""+localURL+"\"\nhttp_headers = { Authorization = \"Bearer "+token+"\" }\n")
	// OpenCode's later file wins, and JSONC's comments and trailing commas are read.
	write(t, filepath.Join(dirs["XDG_CONFIG_HOME"], "opencode", "config.json"),
		`{"mcp": {"meko": {"type": "remote", "url": "`+otherURL+`"}}}`)
	write(t, filepath.Join(dirs["XDG_CONFIG_HOME"], "opencode", "opencode.jsonc"),
		"// mine\n{\"mcp\": {\"meko\": {\"type\": \"remote\", \"url\": \""+localURL+"\", /* the token */ "+
			"\"headers\": {\"Authorization\": \"Bearer "+token+"\",},},},}\n")
	wantStates(t, "moved configs", states(t, manager(local)),
		map[string]string{"claude": StateConnected, "kiro": StateConnected, "codex": StateConnected,
			"opencode": StateConnected})
}

// Claude Code: the meko server goes at user scope through claude mcp, replacing one there, with the
// token in a header; a failing claude says so without the token.
func TestConnectClaude(t *testing.T) {
	_, log := fakeAgents(t, false)
	m := manager(local)
	add := "claude mcp add --scope user --transport http meko " + localURL + " --header Authorization: Bearer " +
		token + "\n"
	r, err := m.Connect(context.Background(), "claude")
	if err != nil || !r.Changed || len(r.Notes) != 0 || r.Agent.Name != "claude" {
		t.Fatalf("connect claude: %+v %v", r, err)
	}
	if got := readLog(t, log); got != "claude mcp remove --scope user meko\n"+add {
		t.Errorf("claude ran:\n%s", got)
	}

	_, log = fakeAgents(t, true)
	_, err = m.Connect(context.Background(), "claude")
	if !isCode(err, CodeFailed) || err.Error() != "Couldn't connect Claude Code: claude mcp failed: refused: mcp add "+
		"--scope user --transport http meko "+localURL+" --header Authorization: Bearer <token>" {
		t.Errorf("a failing claude: %v", err)
	}
	if !strings.Contains(readLog(t, log), "claude mcp add") {
		t.Errorf("claude wasn't run")
	}
}

// Claude Code takes a folder's own meko (local scope) over the user scope's: one for this computer's
// Meko is removed there, through claude in that folder, and one for another server stays, with a
// note, which the list shows too. An add that fails puts the user scope's old meko back.
func TestConnectClaudeFolders(t *testing.T) {
	home, log := fakeAgents(t, false)
	mine, other, bare := t.TempDir(), t.TempDir(), t.TempDir()
	old := `{"type":"http","url":"` + otherURL + `","headers":{"Authorization":"Bearer elsewhere"}}`
	cfg := `{"mcpServers": {"meko": ` + old + `}, "projects": {` +
		`"` + mine + `": {"mcpServers": {"meko": {"type": "http", "url": "` + localURL + `"}}}, ` +
		`"` + other + `": {"mcpServers": {"meko": {"type": "http", "url": "` + otherURL + `"}}}, ` +
		`"` + bare + `": {"mcpServers": {"git": {"command": "git-mcp"}}}}}`
	write(t, filepath.Join(home, ".claude.json"), cfg)
	note := "In " + other + ", Claude Code uses a meko server of its own (" + otherURL + "): " +
		"claude mcp remove --scope local meko there removes it"
	if r := rows(t, manager(local))["claude"]; len(r.Notes) != 2 || r.Notes[1] != note && r.Notes[0] != note {
		t.Errorf("the list's notes: %q", r.Notes)
	}
	r, err := manager(local).Connect(context.Background(), "claude")
	if err != nil || len(r.Notes) != 2 || r.Notes[0] != "It replaced the meko server at "+otherURL || r.Notes[1] != note {
		t.Fatalf("connect claude: %q %v", r.Notes, err)
	}
	ran := "claude mcp remove --scope local meko\n  in " + realPath(t, mine) + "\n"
	if got := readLog(t, log); !strings.HasSuffix(got, ran) ||
		strings.Count(got, "--scope local") != 1 {
		t.Errorf("claude ran:\n%s", got)
	}

	// CLAUDE_CONFIG_DIR moves the file; a failing add puts the old meko back.
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	_, log = fakeAgents(t, true)
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	write(t, filepath.Join(filepath.Dir(log), "added"), "")
	_, err = manager(local).Connect(context.Background(), "claude")
	if err == nil || !strings.Contains(err.Error(), "(the meko server it had is back)") {
		t.Fatalf("a failing add: %v", err)
	}
	if got := readLog(t, log); !strings.Contains(got, "claude mcp add-json --scope user meko "+old+"\n") ||
		strings.Contains(got, "--scope local") {
		t.Errorf("claude ran:\n%s", got)
	}
}

// OpenCode: opencode mcp add, to its global config, with the header as name=value.
func TestConnectOpenCode(t *testing.T) {
	_, log := fakeAgents(t, false)
	if _, err := manager(cloud).Connect(context.Background(), "opencode"); err != nil {
		t.Fatal(err)
	}
	if got, w := readLog(t, log), "opencode mcp add meko --url "+cloudURL+" --header Authorization=Bearer "+cloudKey+
		" --global\n"; got != w {
		t.Errorf("opencode ran %q, want %q", got, w)
	}
}

type kiroServer struct {
	URL     string            `json:"url"`
	Command string            `json:"command"`
	Headers map[string]string `json:"headers"`
}

type kiroDoc struct {
	MCPServers map[string]kiroServer `json:"mcpServers"`
	Other      string                `json:"other"`
}

func readKiroDoc(t *testing.T, path string) kiroDoc {
	t.Helper()
	var doc kiroDoc
	if err := json.Unmarshal([]byte(read(t, path)), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// Kiro CLI: settings/mcp.json gets the meko server, made 0600 when new, and an existing file keeps its
// other servers and keys. Disconnect takes it out as text, keeping the rest as it was.
func TestKiro(t *testing.T) {
	home, _ := fakeAgents(t, false)
	path := filepath.Join(home, ".kiro", "settings", "mcp.json")
	m := manager(local)
	r, err := m.Connect(context.Background(), "kiro")
	if err != nil || r.Agent.State != StateConnected || len(r.Notes) != 0 {
		t.Fatalf("connect kiro: %+v %v", r, err)
	}
	if fi, serr := os.Stat(path); serr != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mcp.json: %v %v", fi, serr)
	}
	if s := readKiroDoc(t, path).MCPServers["meko"]; s.URL != localURL || s.Headers["Authorization"] != "Bearer "+token {
		t.Errorf("meko server %+v", s)
	}

	write(t, path, `{"other": "kept", "mcpServers": {"git": {"command": "git-mcp"}, `+
		`"meko": {"url": "`+otherURL+`", "headers": {"Authorization": "Bearer elsewhere"}}}}`)
	r, err = m.Connect(context.Background(), "kiro")
	if err != nil || len(r.Notes) != 1 || r.Notes[0] != "It replaced the meko server at "+otherURL {
		t.Fatalf("connect over another: %+v %v", r, err)
	}
	if doc := readKiroDoc(t, path); doc.Other != "kept" || doc.MCPServers["git"].Command != "git-mcp" ||
		doc.MCPServers["meko"].Headers["Authorization"] != "Bearer "+token {
		t.Errorf("mcp.json %+v", doc)
	}

	write(t, path, "{not json")
	if _, err = m.Connect(context.Background(), "kiro"); !isCode(err, CodeFailed) ||
		!strings.Contains(err.Error(), "can't read "+path) {
		t.Errorf("a broken file: %v", err)
	}
	if r := rows(t, m)["kiro"]; r.State != StateNone || len(r.Notes) != 1 || !strings.HasPrefix(r.Notes[0],
		"Meko can't read Kiro CLI's settings: can't read "+path) {
		t.Errorf("a broken file's row: %+v", r)
	}
	write(t, path, "null\n")
	if _, err = m.Connect(context.Background(), "kiro"); err != nil {
		t.Errorf("over null: %v", err)
	}

	keep := "{\n  \"other\": \"kept\",\n  \"mcpServers\": {\n    \"git\": {\"command\": \"git-mcp\"},\n" +
		"    \"meko\": " + entry(localURL, token) + "\n  }\n}\n"
	write(t, path, keep)
	r, err = m.Disconnect(context.Background(), "kiro")
	if err != nil || !r.Changed || r.Agent.State != StateNone {
		t.Fatalf("disconnect kiro: %+v %v", r, err)
	}
	if got, w := read(t, path), "{\n  \"other\": \"kept\",\n  \"mcpServers\": {\n"+
		"    \"git\": {\"command\": \"git-mcp\"}\n  }\n}\n"; got != w {
		t.Errorf("mcp.json:\n%s\nwant:\n%s", got, w)
	}
	// Again: nothing to take out.
	r, err = m.Disconnect(context.Background(), "kiro")
	if err != nil || r.Changed || len(r.Notes) != 0 {
		t.Errorf("disconnect with none: %+v %v", r, err)
	}
}

// Codex: config.toml in CODEX_HOME gets an [mcp_servers.meko] table with the header; the old one and
// its subtables go, and the rest of the file stays as it was, comments too. Disconnect takes the
// table out the same way.
func TestCodex(t *testing.T) {
	fakeAgents(t, false)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "config.toml")
	old := `# my settings
model = "o3"

[mcp_servers.meko]
url = "http://localhost:7790/mcp"

[mcp_servers.meko.env_http_headers]
X-Old = "OLD"

# GitHub's server
[mcp_servers.git] # kept
command = "git-mcp"
`
	write(t, path, old)
	m := manager(local)
	r, err := m.Connect(context.Background(), "codex")
	if err != nil || len(r.Notes) != 0 || r.Agent.State != StateConnected {
		t.Fatalf("connect codex: %+v %v", r, err)
	}
	w := `# my settings
model = "o3"

# GitHub's server
[mcp_servers.git] # kept
command = "git-mcp"

[mcp_servers.meko]
url = "http://127.0.0.1:7778/mcp"
http_headers = { Authorization = "Bearer ` + token + `" }
`
	if got := read(t, path); got != w {
		t.Errorf("config.toml:\n%s\nwant:\n%s", got, w)
	}
	r, err = m.Disconnect(context.Background(), "codex")
	if err != nil || !r.Changed || r.Agent.State != StateNone {
		t.Fatalf("disconnect codex: %+v %v", r, err)
	}
	w = "# my settings\nmodel = \"o3\"\n\n# GitHub's server\n[mcp_servers.git] # kept\ncommand = \"git-mcp\"\n"
	if got := read(t, path); got != w {
		t.Errorf("config.toml after disconnect:\n%s\nwant:\n%s", got, w)
	}
	var cfg codexServers
	if _, err := toml.Decode(read(t, path), &cfg); err != nil || codexHas(cfg) {
		t.Errorf("%v %+v", err, cfg)
	}

	// meko set some other way than its own table can't be replaced or removed, and the file isn't touched.
	odd := "mcp_servers.meko.url = \"" + localURL + "\"\n"
	write(t, path, odd)
	if _, err := m.Connect(context.Background(), "codex"); !isCode(err, CodeFailed) ||
		!strings.Contains(err.Error(), "has a meko server Meko can't replace") {
		t.Errorf("connect over a dotted key: %v", err)
	}
	if _, err := m.Disconnect(context.Background(), "codex"); !isCode(err, CodeFailed) ||
		!strings.Contains(err.Error(), "has a meko server Meko can't remove: run codex mcp remove meko") {
		t.Errorf("disconnect a dotted key: %v", err)
	}
	if got := read(t, path); got != odd {
		t.Errorf("config.toml changed: %s", got)
	}

	// With no file, it's made, 0600.
	t.Setenv("CODEX_HOME", filepath.Join(home, "new"))
	if _, err := m.Connect(context.Background(), "codex"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(home, "new", "config.toml")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config.toml: %v %v", fi, err)
	}
}

// Disconnect removes only a meko server that's one of Meko's own: Claude Code's through claude mcp
// (with a folder's own on this computer), OpenCode's out of its global config as text, comments and
// all. One pointing elsewhere is left as it is, with a note.
func TestDisconnect(t *testing.T) {
	home, log := fakeAgents(t, false)
	write(t, filepath.Join(filepath.Dir(log), "added"), "") // claude has a meko server at user scope
	mine := t.TempDir()
	write(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"meko": `+entry(cloudURL, cloudKey)+`}, `+
		`"projects": {"`+mine+`": {"mcpServers": {"meko": {"url": "`+localURL+`"}}}}}`)
	r, err := manager(cloud).Disconnect(context.Background(), "claude")
	if err != nil || !r.Changed || len(r.Notes) != 0 {
		t.Fatalf("disconnect claude: %+v %v", r, err)
	}
	if got := readLog(t, log); got != "claude mcp remove --scope user meko\nclaude mcp remove --scope local meko\n  in "+
		realPath(t, mine)+"\n" {
		t.Errorf("claude ran:\n%s", got)
	}
	// Unlinked, the same entry at Meko Cloud is elsewhere: it stays.
	r, err = manager(local).Disconnect(context.Background(), "claude")
	if err != nil || r.Changed || len(r.Notes) != 1 ||
		r.Notes[0] != "Claude Code's meko server points at "+cloudURL+", not Meko: left as it is" {
		t.Errorf("disconnect an elsewhere: %+v %v", r, err)
	}
	if got := readLog(t, log); got != "" {
		t.Errorf("claude ran:\n%s", got)
	}

	path := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	text := "{\n  // my servers\n  \"mcp\": {\n    \"servers\": {\n      \"git\": {\"type\": \"local\"}, // kept\n" +
		"      \"meko\": {\"type\": \"remote\", \"url\": \"" + localURL + "\"},\n    },\n" +
		"    \"meko\": {\"type\": \"remote\", \"url\": \"http://localhost:7790/mcp\"}\n  },\n}\n"
	write(t, path, text)
	r, err = manager(local).Disconnect(context.Background(), "opencode")
	if err != nil || !r.Changed || r.Agent.State != StateNone {
		t.Fatalf("disconnect opencode: %+v %v", r, err)
	}
	if got, w := read(t, path), "{\n  // my servers\n  \"mcp\": {\n    \"servers\": {\n"+
		"      \"git\": {\"type\": \"local\"}, // kept\n    }\n  },\n}\n"; got != w {
		t.Errorf("opencode.jsonc:\n%s\nwant:\n%s", got, w)
	}
	if got := readLog(t, log); got != "" {
		t.Errorf("opencode ran: %s", got)
	}
	write(t, path, `{"mcp": {"servers": {"meko": {"url": "`+otherURL+`"}}}}`)
	if r, err := manager(local).Disconnect(context.Background(), "opencode"); err != nil || r.Changed ||
		read(t, path) != `{"mcp": {"servers": {"meko": {"url": "`+otherURL+`"}}}}` {
		t.Errorf("disconnect an elsewhere opencode: %+v %v", r, err)
	}
}

// removeMember keeps everything but the member, and a document without it unchanged.
func TestRemoveMember(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"a": 1, "meko": {"x": [1, {"y": "}"}]}, "b": 2}`, `{"a": 1, "b": 2}`},
		{`{"a": 1, "meko": 2}`, `{"a": 1}`},
		{`{"meko": 2}`, `{}`},
		{"{\n  \"meko\": 2\n}\n", "{\n}\n"},
		{"{\n  \"a\": 1,\n  \"meko\": 2 // old\n}\n", "{\n  \"a\": 1 // old\n}\n"},
		{"{\"a\": \"meko\", \"b\": {\"meko\": 1}}", "{\"a\": \"meko\", \"b\": {\"meko\": 1}}"},
	} {
		out, _, err := removeMember([]byte(c.in), nil)
		if err != nil || string(out) != c.want {
			t.Errorf("removeMember(%q) = %q, %v; want %q", c.in, out, err, c.want)
		}
	}
	if _, ok, err := removeMember([]byte(`{"mcpServers": {"git": {}}}`), []string{"mcpServers"}); ok || err != nil {
		t.Errorf("no member: %v %v", ok, err)
	}
	if _, _, err := removeMember([]byte(`{"mcpServers": `), []string{"mcpServers"}); err == nil {
		t.Error("a broken document")
	}
}

// A link change moves every agent at Meko's old target, or at one of its own, to the new one; one
// elsewhere is left, and one that fails is reported, without either target's token or key. Unlinking
// moves them back, which takes the key out of their configs.
func TestMove(t *testing.T) {
	home, _ := fakeAgents(t, true)
	kiro := filepath.Join(home, ".kiro", "settings", "mcp.json")
	codex := filepath.Join(home, ".codex", "config.toml")
	write(t, kiro, `{"mcpServers": {"meko": `+entry(localURL, token)+`}}`)
	write(t, codex, "[mcp_servers.meko]\nurl = \""+otherURL+"\"\n")
	write(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"meko": `+entry("http://localhost:7790/mcp", "x")+`}}`)
	m := &Manager{Lock: filepath.Join(t.TempDir(), "agents.lock")}
	mv := m.Move(context.Background(), local, cloud)
	if len(mv.Moved) != 1 || mv.Moved[0] != (Moved{"kiro", "Kiro CLI"}) ||
		len(mv.Left) != 1 || mv.Left[0] != (Left{"codex", "Codex", otherURL}) ||
		len(mv.Failed) != 1 || mv.Failed[0].Name != "claude" || !strings.Contains(mv.Failed[0].Error, "refused") {
		t.Fatalf("move: %+v", mv)
	}
	b, _ := json.Marshal(mv)
	if strings.Contains(string(b), token) || strings.Contains(string(b), cloudKey) {
		t.Errorf("the move's report holds a token: %s", b)
	}
	if s := readKiroDoc(t, kiro).MCPServers["meko"]; s.URL != cloudURL ||
		s.Headers["Authorization"] != "Bearer "+cloudKey {
		t.Errorf("kiro: %+v", s)
	}
	wantStates(t, "after the move", states(t, manager(cloud)),
		map[string]string{"claude": StateStale, "kiro": StateConnected, "codex": StateElsewhere, "opencode": StateNone})

	// A new key rewrites the agents on Meko Cloud; one already at the target isn't touched.
	newKey := cloud
	newKey.Token = "mko_tkn_cloud_new_0123456789"
	mv = m.Move(context.Background(), cloud, newKey)
	if len(mv.Moved) != 1 || mv.Moved[0].Name != "kiro" {
		t.Fatalf("new key: %+v", mv)
	}
	if mv = m.Move(context.Background(), newKey, newKey); len(mv.Moved) != 0 {
		t.Errorf("no change moved %+v", mv)
	}

	// Unlinking moves it back, and no config keeps a key.
	write(t, filepath.Join(home, ".claude.json"), `{}`)
	mv = m.Move(context.Background(), newKey, local)
	if len(mv.Moved) != 1 || len(mv.Failed) != 0 {
		t.Fatalf("unlink: %+v", mv)
	}
	if strings.Contains(read(t, kiro), "mko_tkn") || !strings.Contains(read(t, kiro), "Bearer "+token) {
		t.Errorf("kiro after unlinking: %s", read(t, kiro))
	}
}

// Connect, disconnect and move hold one lock across processes: a second waits for the first.
func TestLockWaits(t *testing.T) {
	fakeAgents(t, false)
	m := manager(local)
	m.Lock = filepath.Join(t.TempDir(), "run", "agents.lock")
	unlock, err := m.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := m.Connect(context.Background(), "kiro")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("connect didn't wait for the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("connect didn't take the lock once it was free")
	}
	old := LockWait
	LockWait = 100 * time.Millisecond
	t.Cleanup(func() { LockWait = old })
	unlock, _ = m.lock(context.Background())
	defer unlock()
	if _, err := m.Disconnect(context.Background(), "kiro"); err == nil || !strings.Contains(err.Error(), "try again") {
		t.Errorf("a lock held too long: %v", err)
	}
}

// Connect reads the target under the lock, and Move the agents' states: a connect waiting on a link
// change's move writes the target after the change, and a move sees what a connect before it wrote.
func TestTargetAndStatesReadUnderTheLock(t *testing.T) {
	home, _ := fakeAgents(t, false)
	kiro := filepath.Join(home, ".kiro", "settings", "mcp.json")
	var mu sync.Mutex
	target := local
	m := &Manager{Lock: filepath.Join(t.TempDir(), "agents.lock"), Target: func() (Target, error) {
		mu.Lock()
		defer mu.Unlock()
		return target, nil
	}}
	unlock, err := m.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := m.Connect(context.Background(), "kiro")
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	target = cloud // the link changed while the connect waited
	mu.Unlock()
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s := readKiroDoc(t, kiro).MCPServers["meko"]; s.URL != cloudURL {
		t.Fatalf("the connect wrote the old target: %+v", s)
	}

	write(t, kiro, `{}`)
	unlock, _ = m.lock(context.Background())
	moved := make(chan Moves, 1)
	go func() { moved <- m.Move(context.Background(), local, cloud) }()
	time.Sleep(200 * time.Millisecond)
	write(t, kiro, `{"mcpServers": {"meko": `+entry(localURL, token)+`}}`) // a connect that held the lock
	unlock()
	if mv := <-moved; len(mv.Moved) != 1 || mv.Moved[0].Name != "kiro" {
		t.Fatalf("the move read the states before the lock: %+v", mv)
	}
}

// A meko server that's turned off isn't one the agent uses: none, and connect turns it on.
func TestDisabledServers(t *testing.T) {
	home, _ := fakeAgents(t, false)
	kiro := filepath.Join(home, ".kiro", "settings", "mcp.json")
	write(t, kiro, `{"mcpServers": {"meko": {"url": "`+localURL+`", "headers": {"Authorization": "Bearer `+token+
		`"}, "disabled": true, "autoApprove": ["search"]}}}`)
	write(t, filepath.Join(home, ".codex", "config.toml"), "[mcp_servers.meko]\nurl = \""+localURL+
		"\"\nhttp_headers = { Authorization = \"Bearer "+token+"\" }\nenabled = false\n")
	write(t, filepath.Join(home, ".config", "opencode", "opencode.json"),
		`{"mcp": {"servers": {"meko": {"type": "remote", "url": "`+localURL+`", `+
			`"headers": {"Authorization": "Bearer `+token+`"}, "enabled": false}}}}`)
	write(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"meko": `+entry(localURL, token)+`}}`)
	wantStates(t, "turned off", states(t, manager(local)),
		map[string]string{"claude": StateConnected, "kiro": StateNone, "codex": StateNone, "opencode": StateNone})
	for _, n := range []string{"kiro", "codex"} {
		if _, err := manager(local).Connect(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	if got := read(t, kiro); strings.Contains(got, "disabled") || !strings.Contains(got, `"autoApprove": [`) {
		t.Errorf("mcp.json: %s", got)
	}
	wantStates(t, "connected again", states(t, manager(local)),
		map[string]string{"claude": StateConnected, "kiro": StateConnected, "codex": StateConnected, "opencode": StateNone})
}

// Claude Code expands ${VAR} and ${VAR:-default} in a header, from settings.json's env over the
// environment; a variable set in neither can't be checked, so a server at the target is connected.
func TestClaudeHeaderVariables(t *testing.T) {
	home, _ := fakeAgents(t, false)
	write(t, filepath.Join(home, ".claude", "settings.json"), `{"env": {"MEKO_TOKEN": "`+token+`"}}`)
	t.Setenv("MEKO_OTHER", "wrong")
	for header, want := range map[string]string{
		"Bearer ${MEKO_TOKEN}":                    StateConnected,
		"Bearer ${MEKO_UNSET_VAR}":                StateConnected,
		"Bearer ${MEKO_UNSET_VAR:-" + token + "}": StateConnected,
		"Bearer ${MEKO_UNSET_VAR:-wrong}":         StateStale,
		"Bearer ${MEKO_OTHER}":                    StateStale,
		"bearer  " + token:                        StateConnected,
	} {
		write(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"meko": {"url": "`+localURL+
			`", "headers": {"Authorization": "`+header+`"}}}}`)
		if got := states(t, manager(local))["claude"]; got != want {
			t.Errorf("%s: %s, want %s", header, got, want)
		}
	}
	// Only Claude Code expands them.
	write(t, filepath.Join(home, ".kiro", "settings", "mcp.json"), `{"mcpServers": {"meko": {"url": "`+localURL+
		`", "headers": {"Authorization": "Bearer ${MEKO_UNSET_VAR}"}}}}`)
	if got := states(t, manager(local))["kiro"]; got != StateStale {
		t.Errorf("kiro with a variable: %s", got)
	}
}

// A URL Meko reports never carries credentials, a query or a fragment: another server's key may be
// in them.
func TestReportedURLsHoldNoSecrets(t *testing.T) {
	home, _ := fakeAgents(t, false)
	secretURL := "https://user:pass@mcp.example.com/mcp?api_key=secret#frag" //nolint:gosec // G101: a made-up key
	write(t, filepath.Join(home, ".kiro", "settings", "mcp.json"), `{"mcpServers": {"meko": {"url": "`+secretURL+`"}}}`)
	write(t, filepath.Join(home, ".claude.json"), `{"projects": {"/work": {"mcpServers": {"meko": {"url": "`+
		secretURL+`"}}}}}`)
	l, _ := manager(local).List()
	r, err := manager(local).Disconnect(context.Background(), "kiro")
	if err != nil {
		t.Fatal(err)
	}
	mv := (&Manager{}).Move(context.Background(), local, cloud)
	b, _ := json.Marshal([]any{l, r, mv})
	got := string(b)
	if strings.Contains(got, "secret") || strings.Contains(got, "pass") || strings.Contains(got, "frag") ||
		!strings.Contains(got, `"https://mcp.example.com/mcp"`) {
		t.Errorf("reported: %s", b)
	}
	if got := shownURL("not a url"); got != "a URL Meko can't read" {
		t.Errorf("shownURL: %q", got)
	}
}

// Kiro CLI's mcp.json is edited as text: the other servers and keys keep their order and layout, an
// old meko entry its other fields, and a missing meko or mcpServers is added.
func TestKiroConnectKeepsTheFile(t *testing.T) {
	home, _ := fakeAgents(t, false)
	path := filepath.Join(home, ".kiro", "settings", "mcp.json")
	m := manager(local)
	for _, c := range []struct{ in, want string }{
		{"{\n  \"zeta\": 1,\n  \"mcpServers\": {\n    \"git\": {\"command\": \"git-mcp\", \"args\": [\"<a>\"]},\n" +
			"    \"meko\": {\"timeout\": 5, \"url\": \"x\", \"disabled\": true, \"autoApprove\": [\"search\"]}\n" +
			"  },\n  \"alpha\": 2\n}\n",
			"{\n  \"zeta\": 1,\n  \"mcpServers\": {\n    \"git\": {\"command\": \"git-mcp\", \"args\": [\"<a>\"]},\n" +
				"    \"meko\": {\n      \"timeout\": 5,\n      \"url\": \"" + localURL + "\",\n      \"autoApprove\": [\n" +
				"        \"search\"\n      ],\n      \"headers\": {\n        \"Authorization\": \"Bearer " + token + "\"\n" +
				"      }\n    }\n  },\n  \"alpha\": 2\n}\n"},
		{"{\n  \"mcpServers\": {\n    \"git\": {\"command\": \"git-mcp\"}\n  }\n}\n",
			"{\n  \"mcpServers\": {\n    \"git\": {\"command\": \"git-mcp\"},\n    \"meko\": {\n      \"url\": \"" + localURL +
				"\",\n      \"headers\": {\n        \"Authorization\": \"Bearer " + token + "\"\n      }\n    }\n  }\n}\n"},
		{`{"other": "kept"}`, `{"other": "kept", "mcpServers": {"meko":{"url":"` + localURL +
			`","headers":{"Authorization":"Bearer ` + token + `"}}}}`},
		{"", "{\n  \"mcpServers\": {\n    \"meko\": {\n      \"url\": \"" + localURL + "\",\n      \"headers\": {\n" +
			"        \"Authorization\": \"Bearer " + token + "\"\n      }\n    }\n  }\n}\n"},
	} {
		if c.in != "" {
			write(t, path, c.in)
		} else {
			_ = os.Remove(path)
		}
		if _, err := m.Connect(context.Background(), "kiro"); err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got := read(t, path); got != c.want {
			t.Errorf("from %q:\n%s\nwant:\n%s", c.in, got, c.want)
		}
	}
}
