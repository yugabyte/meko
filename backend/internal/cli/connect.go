package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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
// and how Meko adds itself to it, which returns what it did.
type connectAgent struct {
	name, title string
	connect     func(a mcpAccess) (string, error)
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
	did, err := g.connect(a)
	if err != nil {
		// The token goes only where it's meant to: never in an error an agent's CLI echoed.
		return fail(stderr, fmt.Errorf("connecting %s: %s. Run meko connect for the URL and token to add by hand",
			g.title, strings.ReplaceAll(err.Error(), a.token, "<token>")))
	}
	fmt.Fprintf(stdout, "Connected %s to Meko: %s\n", g.title, did)
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
func runAgent(name string, args ...string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s isn't on PATH", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
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

// connectClaude adds Meko at Claude Code's user scope, in ~/.claude.json, which every project
// reads. claude mcp add refuses a name the scope already has, so a meko server there is removed
// first; the removal fails when there's none.
func connectClaude(a mcpAccess) (string, error) {
	rmErr := runAgent("claude", "mcp", "remove", "--scope", "user", mcpName)
	if err := runAgent("claude", "mcp", "add", "--scope", "user", "--transport", "http", mcpName, a.url,
		"--header", "Authorization: Bearer "+a.token); err != nil {
		return "", err
	}
	did := "added the MCP server meko at " + a.url + " for all your projects (user scope)"
	if rmErr == nil {
		did += ", replacing the one it had"
	}
	return did, nil
}

// connectOpenCode adds Meko to OpenCode's global config. opencode mcp add replaces a server of the
// same name, and writes the config in the shape its own version reads.
func connectOpenCode(a mcpAccess) (string, error) {
	if err := runAgent("opencode", "mcp", "add", mcpName, "--url", a.url,
		"--header", "Authorization=Bearer "+a.token, "--global"); err != nil {
		return "", err
	}
	return "added the MCP server meko at " + a.url + " to its global config, for all your projects", nil
}

// connectKiro writes Meko into Kiro CLI's global ~/.kiro/settings/mcp.json, which every agent of
// Kiro's reads: kiro-cli mcp add takes no headers (and needs a login). The file's other servers
// and keys stay.
func connectKiro(a mcpAccess) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".kiro", "settings", "mcp.json")
	doc := map[string]json.RawMessage{}
	b, err := readConfigFile(path)
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return "", fmt.Errorf("can't read %s: %w", path, err)
		}
	}
	servers := map[string]json.RawMessage{}
	if raw := doc["mcpServers"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return "", fmt.Errorf("can't read %s's mcpServers: %w", path, err)
		}
	}
	var old struct {
		URL string `json:"url"`
	}
	if raw, ok := servers[mcpName]; ok {
		_ = json.Unmarshal(raw, &old)
	}
	servers[mcpName], _ = json.Marshal(map[string]any{"url": a.url,
		"headers": map[string]string{"Authorization": "Bearer " + a.token}})
	doc["mcpServers"], _ = json.Marshal(servers)
	out, _ := json.MarshalIndent(doc, "", "  ")
	if err := writeConfigFile(path, append(out, '\n')); err != nil {
		return "", err
	}
	return "wrote the MCP server meko at " + a.url + " to " + path + replacing(old.URL, a.url), nil
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
// added at the end, and the result is read back before it's written.
func connectCodex(a mcpAccess) (string, error) {
	dir := os.Getenv("CODEX_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".codex")
	}
	path := filepath.Join(dir, "config.toml")
	b, err := readConfigFile(path)
	if err != nil {
		return "", err
	}
	var before codexServers
	if _, derr := toml.Decode(string(b), &before); derr != nil {
		return "", fmt.Errorf("can't read %s: %w", path, derr)
	}
	var kept strings.Builder
	in := false
	for _, line := range strings.SplitAfter(string(b), "\n") {
		if name, ok := tomlTable(line); ok {
			in = name == "mcp_servers."+mcpName || strings.HasPrefix(name, "mcp_servers."+mcpName+".")
		}
		if !in {
			kept.WriteString(line)
		}
	}
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
		return "", fmt.Errorf("%s has a meko server meko connect can't replace: remove it (codex mcp remove meko) "+
			"and run meko connect codex again", path)
	}
	if err := writeConfigFile(path, []byte(text)); err != nil {
		return "", err
	}
	return "wrote the MCP server meko at " + a.url + " to " + path + replacing(before.MCPServers[mcpName].URL, a.url),
		nil
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
