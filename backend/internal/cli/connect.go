package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/yugabyte/meko/backend/internal/paths"
)

// meko connect adds Meko's MCP server to a coding agent, for every project of the user's: Streamable
// HTTP at the MCP URL with the client token in an Authorization header. Each agent's own way:
// Claude Code's and OpenCode's CLIs take the header, Kiro CLI's and Codex's don't, so Meko writes
// their config files itself. With no agent it prints the URL and token for any MCP client.

// mcpName is the server's name in every agent: AgentK recognizes Meko's tools by it (mcp__meko__*).
const mcpName = "meko"

// agentTimeout bounds one run of an agent's CLI.
const agentTimeout = 60 * time.Second

// connectAgent is an agent meko connect knows: its name on the command line, what people call it,
// and how Meko adds itself to it, which returns what it did and anything else the user should know.
type connectAgent struct {
	name, title string
	connect     func(a mcpAccess) (did string, notes []string, err error)
}

var connectAgents = []connectAgent{
	{name: "claude", title: "Claude Code", connect: connectClaude},
	{name: "codex", title: "Codex", connect: connectCodex},
	{name: "kiro", title: "Kiro CLI", connect: connectKiro},
	{name: "opencode", title: "OpenCode", connect: connectOpenCode},
}

func connectAgentNames() []string {
	names := make([]string, len(connectAgents))
	for i, g := range connectAgents {
		names[i] = g.name
	}
	return names
}

// runConnect adds Meko to the named agent, or with none prints how any MCP client connects.
func runConnect(l paths.Layout, agent string, stdout, stderr io.Writer) int {
	var g *connectAgent
	for i := range connectAgents {
		if connectAgents[i].name == agent {
			g = &connectAgents[i]
		}
	}
	if agent != "" && g == nil {
		fmt.Fprintf(stderr, "meko connect: unknown agent %q (%s)\n", agent, strings.Join(connectAgentNames(), ", "))
		return exitUsage
	}
	_, a, err := readMCPAccess(l)
	if err != nil {
		return fail(stderr, err)
	}
	if g == nil {
		printConnectHelp(a, stdout)
		return exitOK
	}
	did, notes, err := g.connect(a)
	if err != nil {
		// The token goes only where it's meant to: never in an error an agent's CLI echoed.
		return fail(stderr, fmt.Errorf("connecting %s: %s. Run meko connect for the URL and token to add by hand",
			g.title, strings.ReplaceAll(err.Error(), a.token, "<token>")))
	}
	fmt.Fprintf(stdout, "Connected %s to Meko: %s\n", g.title, did)
	for _, n := range notes {
		fmt.Fprintln(stdout, strings.ReplaceAll(n, a.token, "<token>"))
	}
	fmt.Fprintln(stdout, "Its new sessions have Meko's tools; ones already running need a restart")
	if !a.running {
		fmt.Fprintln(stdout, "Meko isn't running: start it with meko start")
	}
	return exitOK
}

// printConnectHelp prints what any MCP client needs, and the agents meko connect adds Meko to.
func printConnectHelp(a mcpAccess, w io.Writer) {
	fmt.Fprintln(w, "Add Meko to any MCP client as a Streamable HTTP server:")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintf(tw, "  Name\t%s (AgentK knows Meko's tools by it)\n", mcpName)
	fmt.Fprintf(tw, "  URL\t%s\n", a.url)
	fmt.Fprintf(tw, "  Header\tAuthorization: Bearer %s\n", a.token)
	fmt.Fprintf(tw, "  Token\t%s\n", a.token)
	_ = tw.Flush()
	fmt.Fprintln(w, "\nOr let Meko add itself to your agent, for all your projects:")
	tw = tabwriter.NewWriter(w, 0, 0, 4, ' ', 0)
	for _, g := range connectAgents {
		fmt.Fprintf(tw, "  meko connect %s\t%s\n", g.name, g.title)
	}
	_ = tw.Flush()
	if !a.running {
		fmt.Fprintln(w, "\nMeko isn't running: start it with meko start")
	}
}

// runAgent runs an agent's CLI with no stdin (so one that would ask, asks no one). Its error has the
// output's last line.
func runAgent(name string, args ...string) error { return runAgentIn("", name, args...) }

// runAgentIn is runAgent in the folder dir.
func runAgentIn(dir, name string, args ...string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s isn't on PATH", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	text := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	if text == "" {
		return fmt.Errorf("%s mcp failed: %w", name, err)
	}
	return fmt.Errorf("%s mcp failed: %s", name, text)
}

// claudeConfig is the part of Claude Code's ~/.claude.json meko connect reads: the user scope's meko
// server, and each folder's own (local scope), kept as written.
type claudeConfig struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
	Projects   map[string]struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	} `json:"projects"`
}

// claudeConfigPath is Claude Code's ~/.claude.json, or .claude.json in CLAUDE_CONFIG_DIR.
func claudeConfigPath() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// connectClaude adds Meko at Claude Code's user scope, which every project reads, through claude mcp.
// Meko only reads Claude's own file, which its running sessions rewrite.
//
// claude mcp add refuses a name the scope already has, so a meko server there is removed first, and
// put back (claude mcp add-json) if the add then fails. Claude Code takes a folder's own server
// (local scope) over the user scope's, so a folder's meko pointing at this computer (what the
// claude mcp add line meko token --mcp printed made, with no --scope) is removed there too: it
// holds the token from then, which a destroy leaves dead. A folder's meko pointing elsewhere (Meko
// Cloud) is the user's choice and stays, with a note that Claude Code uses it there.
func connectClaude(a mcpAccess) (string, []string, error) {
	var cfg claudeConfig
	if p, err := claudeConfigPath(); err == nil {
		if b, rerr := readConfigFile(p); rerr == nil && len(b) > 0 {
			_ = json.Unmarshal(b, &cfg) // unreadable: as if empty, and claude mcp says what's wrong
		}
	}
	rmErr := runAgent("claude", "mcp", "remove", "--scope", "user", mcpName)
	if err := runAgent("claude", "mcp", "add", "--scope", "user", "--transport", "http", mcpName, a.url,
		"--header", "Authorization: Bearer "+a.token); err != nil {
		old, had := cfg.MCPServers[mcpName]
		if rmErr != nil || !had {
			return "", nil, err
		}
		if rerr := runAgent("claude", "mcp", "add-json", "--scope", "user", mcpName, string(old)); rerr != nil {
			return "", nil, fmt.Errorf("%w, and the meko server it had (%s) was removed", err,
				cmp.Or(serverURL(old), "no URL"))
		}
		return "", nil, fmt.Errorf("%w (the meko server it had is back)", err)
	}
	did := "added the MCP server meko at " + a.url + " for all your projects (user scope)"
	if rmErr == nil {
		did += ", replacing the one it had"
	}
	var notes []string
	removed := 0
	for _, dir := range slices.Sorted(maps.Keys(cfg.Projects)) {
		old, ok := cfg.Projects[dir].MCPServers[mcpName]
		if !ok {
			continue
		}
		if onThisComputer(serverURL(old)) && runAgentIn(dir, "claude", "mcp", "remove", "--scope", "local", mcpName) == nil {
			removed++
			continue
		}
		notes = append(notes, fmt.Sprintf("Claude Code still uses the meko server of its own in %s (%s): "+
			"run claude mcp remove --scope local meko there to use this one", dir, cmp.Or(serverURL(old), "no URL")))
	}
	if removed > 0 {
		did += fmt.Sprintf(", and removed the meko server of its own %d folder(s) had for Meko on this computer "+
			"(local scope), which Claude Code would use there instead", removed)
	}
	return did, notes, nil
}

// serverURL is an MCP server entry's url, or "" when it has none.
func serverURL(entry json.RawMessage) string {
	var s struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(entry, &s)
	return s.URL
}

// onThisComputer says whether an MCP URL is on this computer's loopback, where Meko listens.
func onThisComputer(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || net.ParseIP(h).IsLoopback()
}

// connectOpenCode adds Meko to OpenCode's global config. opencode mcp add replaces a server of the
// same name, and writes the config in the shape its own version reads.
func connectOpenCode(a mcpAccess) (string, []string, error) {
	if err := runAgent("opencode", "mcp", "add", mcpName, "--url", a.url,
		"--header", "Authorization=Bearer "+a.token, "--global"); err != nil {
		return "", nil, err
	}
	return "added the MCP server meko at " + a.url + " to its global config, for all your projects", nil, nil
}

// connectKiro writes Meko into Kiro CLI's global settings/mcp.json in its home (KIRO_HOME, else
// ~/.kiro, as AgentK's kiroHome), which every agent of Kiro's reads: kiro-cli mcp add takes no
// headers (and needs a login). The file's other servers and keys stay.
func connectKiro(a mcpAccess) (string, []string, error) {
	home := os.Getenv("KIRO_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", nil, err
		}
		home = filepath.Join(h, ".kiro")
	}
	path := filepath.Join(home, "settings", "mcp.json")
	var doc map[string]json.RawMessage
	b, err := readConfigFile(path)
	if err != nil {
		return "", nil, err
	}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return "", nil, fmt.Errorf("can't read %s: %w", path, err)
		}
	}
	if doc == nil { // no file, or a JSON null
		doc = map[string]json.RawMessage{}
	}
	var servers map[string]json.RawMessage
	if raw := doc["mcpServers"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return "", nil, fmt.Errorf("can't read %s's mcpServers: %w", path, err)
		}
	}
	if servers == nil {
		servers = map[string]json.RawMessage{}
	}
	old := servers[mcpName]
	servers[mcpName], _ = json.Marshal(map[string]any{"url": a.url,
		"headers": map[string]string{"Authorization": "Bearer " + a.token}})
	doc["mcpServers"], _ = json.Marshal(servers)
	out, _ := json.MarshalIndent(doc, "", "  ")
	if err := writeConfigFile(path, append(out, '\n')); err != nil {
		return "", nil, err
	}
	return "wrote the MCP server meko at " + a.url + " to " + path + replacing(serverURL(old), a.url), nil, nil
}

// codexServers is the part of Codex's config.toml meko connect reads back.
type codexServers struct {
	MCPServers map[string]struct {
		URL         string            `toml:"url"`
		HTTPHeaders map[string]string `toml:"http_headers"`
	} `toml:"mcp_servers"`
}

// connectCodex writes Meko into Codex's config.toml ($CODEX_HOME, or ~/.codex): codex mcp add takes
// no headers, only the name of an environment variable holding a bearer token, which the user's
// shell would then have to set. The file is edited as text so the user's comments and layout stay:
// the [mcp_servers.meko] table (as codex mcp add writes it) and its subtables go, a new one is
// added at the end, and the result is read back before it's written. The comment and blank lines
// right before the next table's header are that table's, so they stay.
func connectCodex(a mcpAccess) (string, []string, error) {
	dir := os.Getenv("CODEX_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil, err
		}
		dir = filepath.Join(home, ".codex")
	}
	path := filepath.Join(dir, "config.toml")
	b, err := readConfigFile(path)
	if err != nil {
		return "", nil, err
	}
	var before codexServers
	if _, derr := toml.Decode(string(b), &before); derr != nil {
		return "", nil, fmt.Errorf("can't read %s: %w", path, derr)
	}
	var kept, pending strings.Builder // pending: a dropped table's comment and blank lines, since its last key
	in := false
	for _, line := range strings.SplitAfter(string(b), "\n") {
		if name, ok := tomlTable(line); ok {
			was := in
			in = name == "mcp_servers."+mcpName || strings.HasPrefix(name, "mcp_servers."+mcpName+".")
			if was && !in {
				keepComments(&kept, pending.String())
			}
			pending.Reset()
		}
		switch t := strings.TrimSpace(line); {
		case !in:
			kept.WriteString(line)
		case t == "" || strings.HasPrefix(t, "#"):
			pending.WriteString(line)
		default:
			pending.Reset()
		}
	}
	keepComments(&kept, pending.String())
	text := strings.TrimRight(kept.String(), "\n")
	if text != "" {
		text += "\n\n"
	}
	text += fmt.Sprintf("[mcp_servers.%s]\nurl = %s\nhttp_headers = { Authorization = %s }\n",
		mcpName, strconv.Quote(a.url), strconv.Quote("Bearer "+a.token))
	var after codexServers
	_, err = toml.Decode(text, &after)
	s := after.MCPServers[mcpName]
	if err != nil || s.URL != a.url || s.HTTPHeaders["Authorization"] != "Bearer "+a.token {
		return "", nil, fmt.Errorf("%s has a meko server meko connect can't replace: remove it (codex mcp remove meko) "+
			"and run meko connect codex again", path)
	}
	if err := writeConfigFile(path, []byte(text)); err != nil {
		return "", nil, err
	}
	return "wrote the MCP server meko at " + a.url + " to " + path + replacing(before.MCPServers[mcpName].URL, a.url),
		nil, nil
}

// keepComments adds a dropped table's trailing comment lines to kept, without a second blank line
// where kept already ends with one.
func keepComments(kept *strings.Builder, lines string) {
	if s := kept.String(); s == "" || strings.HasSuffix(s, "\n\n") {
		lines = strings.TrimLeft(lines, " \t\n")
	}
	kept.WriteString(lines)
}

// tomlTable returns the name of the table a line's header opens ([a.b] or [[a.b]]), without spaces
// or quotes, and whether the line is one.
func tomlTable(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") {
		return "", false
	}
	end := strings.Index(t, "]")
	if end < 0 {
		return "", false
	}
	return strings.NewReplacer("[", "", " ", "", "\t", "", `"`, "", "'", "").Replace(t[:end]), true
}

// replacing says what a file's meko server pointed at before, when that was somewhere else.
func replacing(old, now string) string {
	if old == "" || old == now {
		return ""
	}
	return " (replacing the one at " + old + ")"
}

// readConfigFile reads an agent's config file; one that isn't there yet reads as empty.
func readConfigFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the agent's own config file, in the user's home
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// writeConfigFile writes an agent's config file in place, keeping its mode and a symlink to it (a
// dotfiles repo's); a new one is 0600, since it holds the token.
func writeConfigFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // G703: the agent's config folder
		return err
	}
	return os.WriteFile(path, b, 0o600) //nolint:gosec // G703: the agent's own config file, in the user's home
}
