package agents

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// claudeConfig is the part of Claude Code's ~/.claude.json Meko reads: the user scope's servers, and
// each folder's own (local scope), kept as written.
type claudeConfig struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
	Projects   map[string]struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	} `json:"projects"`
}

// readClaudeConfig reads Claude Code's ~/.claude.json, or .claude.json in CLAUDE_CONFIG_DIR. Meko
// only reads it: Claude Code's running sessions rewrite it, so changes go through claude mcp.
func readClaudeConfig() (claudeConfig, error) {
	var cfg claudeConfig
	dir, err := homeDir("CLAUDE_CONFIG_DIR")
	if err != nil {
		return cfg, err
	}
	path := filepath.Join(dir, ".claude.json")
	b, err := readConfigFile(path)
	if err != nil || len(b) == 0 {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("can't read %s: %w", path, err)
	}
	return cfg, nil
}

// mcpEntry is an MCP server entry as Claude Code, Kiro CLI and OpenCode write one: its URL, headers,
// and whether it's turned off (Kiro's disabled, OpenCode's enabled).
type mcpEntry struct {
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	Disabled bool              `json:"disabled"`
	Enabled  *bool             `json:"enabled"`
}

// entryOf reads an MCP server entry's url and Authorization header; nil with none, or for one that's
// turned off, which the agent doesn't use.
func entryOf(raw json.RawMessage) *server {
	if raw == nil {
		return nil
	}
	var e mcpEntry
	_ = json.Unmarshal(raw, &e)
	if e.Disabled || e.Enabled != nil && !*e.Enabled {
		return nil
	}
	s := &server{url: e.URL}
	for k, v := range e.Headers {
		if strings.EqualFold(k, "Authorization") {
			s.auth = v
		}
	}
	return s
}

// urlOf is an MCP server entry's url, on or off.
func urlOf(raw json.RawMessage) string {
	var e mcpEntry
	_ = json.Unmarshal(raw, &e)
	return e.URL
}

// claudeEnv is Claude Code's settings.json env (in CLAUDE_CONFIG_DIR, else ~/.claude), which its
// sessions apply over the environment.
func claudeEnv() map[string]any {
	dir, err := homeDir("CLAUDE_CONFIG_DIR", ".claude")
	if err != nil {
		return nil
	}
	b, _ := readConfigFile(filepath.Join(dir, "settings.json"))
	var s struct {
		Env map[string]any `json:"env"`
	}
	_ = json.Unmarshal(b, &s)
	return s.Env
}

// envVar matches ${VAR} and ${VAR:-default}.
var envVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandEnv expands ${VAR} and ${VAR:-default} as Claude Code does in an MCP server's headers: from
// settings.json's env over the environment. unresolved says a variable is set in neither and has no
// default: a session may still have it, from its login shell.
func expandEnv(v string, env map[string]any) (value string, unresolved bool) {
	value = envVar.ReplaceAllStringFunc(v, func(m string) string {
		sub := envVar.FindStringSubmatch(m)
		set, _ := env[sub[1]].(string)
		if set == "" {
			set = os.Getenv(sub[1])
		}
		if set != "" {
			return set
		}
		if !strings.Contains(m, ":-") {
			unresolved = true
		}
		return sub[2]
	})
	return value, unresolved
}

// claudeEntry is entryOf with the Authorization header expanded as Claude Code expands it.
func claudeEntry(raw json.RawMessage, env map[string]any) *server {
	s := entryOf(raw)
	if s != nil {
		s.auth, s.unresolved = expandEnv(s.auth, env)
	}
	return s
}

// readClaude reads the user scope's meko server, and notes the folders whose own (local scope) meko
// server isn't the target: Claude Code uses it there instead.
func readClaude(t Target) (*server, []string, error) {
	cfg, err := readClaudeConfig()
	if err != nil {
		return nil, nil, err
	}
	env := claudeEnv()
	var notes []string
	for _, dir := range slices.Sorted(maps.Keys(cfg.Projects)) {
		if s := claudeEntry(cfg.Projects[dir].MCPServers[ServerName], env); s != nil && t.state(s) != StateConnected {
			notes = append(notes, claudeFolderNote(dir, s.url))
		}
	}
	return claudeEntry(cfg.MCPServers[ServerName], env), notes, nil
}

func claudeFolderNote(dir, url string) string {
	return fmt.Sprintf("In %s, Claude Code uses a meko server of its own (%s): "+
		"claude mcp remove --scope local meko there removes it", dir, cmp.Or(shownURL(url), "no URL"))
}

// connectClaude adds Meko at Claude Code's user scope, which every project reads, through claude mcp.
//
// claude mcp add refuses a name the scope already has, so a meko server there is removed first, and
// put back (claude mcp add-json) if the add then fails. Claude Code takes a folder's own server
// (local scope) over the user scope's, so a folder's meko pointing at this computer (a claude mcp
// add with no --scope) is removed there too: it holds a token that a destroy leaves dead. A folder's
// meko pointing elsewhere is the user's choice and stays, with a note that Claude Code uses it there.
func connectClaude(ctx context.Context, cmd string, t Target) ([]string, error) {
	cfg, _ := readClaudeConfig() // unreadable: as if empty, and claude mcp says what's wrong
	rmErr := runCLI(ctx, "", cmd, "mcp", "remove", "--scope", "user", ServerName)
	if err := runCLI(ctx, "", cmd, "mcp", "add", "--scope", "user", "--transport", "http", ServerName, t.URL,
		"--header", "Authorization: Bearer "+t.Token); err != nil {
		old, had := cfg.MCPServers[ServerName]
		if rmErr != nil || !had {
			return nil, err
		}
		if rerr := runCLI(ctx, "", cmd, "mcp", "add-json", "--scope", "user", ServerName, string(old)); rerr != nil {
			return nil, fmt.Errorf("%w, and the meko server it had (%s) was removed", err,
				cmp.Or(shownURL(urlOf(old)), "no URL"))
		}
		return nil, fmt.Errorf("%w (the meko server it had is back)", err)
	}
	var notes []string
	for _, dir := range slices.Sorted(maps.Keys(cfg.Projects)) {
		raw, ok := cfg.Projects[dir].MCPServers[ServerName]
		if !ok {
			continue
		}
		if onThisComputer(urlOf(raw)) && runCLI(ctx, dir, cmd, "mcp", "remove", "--scope", "local", ServerName) == nil {
			continue
		}
		if e := claudeEntry(raw, claudeEnv()); e != nil && t.state(e) != StateConnected {
			notes = append(notes, claudeFolderNote(dir, e.url))
		}
	}
	return notes, nil
}

// disconnectClaude removes the user scope's meko server through claude mcp, and a folder's own on
// this computer, as connect does.
func disconnectClaude(ctx context.Context, cmd string, _ Target) ([]string, error) {
	cfg, _ := readClaudeConfig()
	if err := runCLI(ctx, "", cmd, "mcp", "remove", "--scope", "user", ServerName); err != nil {
		return nil, err
	}
	var notes []string
	for _, dir := range slices.Sorted(maps.Keys(cfg.Projects)) {
		raw, ok := cfg.Projects[dir].MCPServers[ServerName]
		if !ok || !onThisComputer(urlOf(raw)) {
			continue
		}
		if runCLI(ctx, dir, cmd, "mcp", "remove", "--scope", "local", ServerName) != nil {
			notes = append(notes, claudeFolderNote(dir, urlOf(raw)))
		}
	}
	return notes, nil
}
