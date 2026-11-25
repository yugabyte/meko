package agents

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// codexServers is the part of Codex's config.toml Meko reads.
type codexServers struct {
	MCPServers map[string]struct {
		URL         string            `toml:"url"`
		HTTPHeaders map[string]string `toml:"http_headers"`
		Enabled     *bool             `toml:"enabled"`
	} `toml:"mcp_servers"`
}

// codexConfig is Codex's config.toml ($CODEX_HOME, or ~/.codex), and what it holds.
func codexConfig() (path string, text []byte, servers codexServers, err error) {
	dir, err := homeDir("CODEX_HOME", ".codex")
	if err != nil {
		return "", nil, servers, err
	}
	path = filepath.Join(dir, "config.toml")
	if text, err = readConfigFile(path); err != nil {
		return path, nil, servers, err
	}
	if _, derr := toml.Decode(string(text), &servers); derr != nil {
		return path, text, servers, fmt.Errorf("can't read %s: %w", path, derr)
	}
	return path, text, servers, nil
}

func readCodex(Target) (*server, []string, error) {
	_, _, cfg, err := codexConfig()
	if err != nil {
		return nil, nil, err
	}
	s, ok := cfg.MCPServers[ServerName]
	if !ok || s.Enabled != nil && !*s.Enabled { // one turned off is one Codex doesn't use
		return nil, nil, nil
	}
	return &server{url: s.URL, auth: s.HTTPHeaders["Authorization"]}, nil, nil
}

// connectCodex writes Meko into Codex's config.toml: codex mcp add takes no headers, only the name
// of an environment variable holding a bearer token, which the user's shell would then have to set.
// The file is edited as text so the user's comments and layout stay (withoutCodexMeko), a new
// [mcp_servers.meko] table is added at the end, and the result is read back before it's written.
func connectCodex(_ context.Context, _ string, t Target) ([]string, error) {
	path, b, _, err := codexConfig()
	if err != nil {
		return nil, err
	}
	text := strings.TrimRight(withoutCodexMeko(string(b)), "\n")
	if text != "" {
		text += "\n\n"
	}
	text += fmt.Sprintf("[mcp_servers.%s]\nurl = %s\nhttp_headers = { Authorization = %s }\n",
		ServerName, strconv.Quote(t.URL), strconv.Quote("Bearer "+t.Token))
	var after codexServers
	_, err = toml.Decode(text, &after)
	s := after.MCPServers[ServerName]
	if err != nil || s.URL != t.URL || s.HTTPHeaders["Authorization"] != "Bearer "+t.Token {
		return nil, fmt.Errorf("%s has a meko server Meko can't replace: remove it (codex mcp remove meko) "+
			"and connect Codex again", path)
	}
	return nil, writeConfigFile(path, []byte(text))
}

// disconnectCodex takes the [mcp_servers.meko] table out of Codex's config.toml, keeping the rest of
// the file as it was.
func disconnectCodex(context.Context, string, Target) ([]string, error) {
	path, b, _, err := codexConfig()
	if err != nil {
		return nil, err
	}
	text := strings.TrimRight(withoutCodexMeko(string(b)), "\n")
	if text != "" {
		text += "\n"
	}
	var after codexServers
	if _, err := toml.Decode(text, &after); err != nil || codexHas(after) {
		return nil, fmt.Errorf("%s has a meko server Meko can't remove: run codex mcp remove meko", path)
	}
	return nil, writeConfigFile(path, []byte(text))
}

// withoutCodexMeko is a config.toml's text without its [mcp_servers.meko] table (as codex mcp add
// writes it) and that table's subtables. The comment and blank lines right before the next table's
// header are that table's, so they stay. A meko set some other way (a dotted key) stays too.
func withoutCodexMeko(b string) string {
	var kept, pending strings.Builder // pending: a dropped table's comment and blank lines, since its last key
	in := false
	for _, line := range strings.SplitAfter(b, "\n") {
		if name, ok := tomlTable(line); ok {
			was := in
			in = name == "mcp_servers."+ServerName || strings.HasPrefix(name, "mcp_servers."+ServerName+".")
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
	return kept.String()
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

// codexHas says whether a config has a meko server.
func codexHas(c codexServers) bool {
	_, ok := c.MCPServers[ServerName]
	return ok
}
