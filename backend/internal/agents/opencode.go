package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
)

// openCodeFiles are OpenCode's global config files, in the order it reads them (a later one's keys
// win), in $XDG_CONFIG_HOME/opencode or ~/.config/opencode. They may be JSONC.
var openCodeFiles = []string{"config.json", "opencode.json", "opencode.jsonc"}

// openCodePaths are where a config's meko server is: mcp.servers.meko (OpenCode 2), or mcp.meko
// (earlier versions).
var openCodePaths = [][]string{{"mcp", "servers"}, {"mcp"}}

func openCodeDir() (string, error) {
	dir, err := homeDir("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "opencode"), nil
}

// openCodeMeko is a config file's meko server at one of openCodePaths, by its index; nil with none.
func openCodeMeko(b []byte) (map[int]*server, error) {
	var doc struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	if err := json.Unmarshal(blankJSONC(b), &doc); err != nil {
		return nil, err
	}
	out := map[int]*server{}
	if raw, ok := doc.MCP["servers"]; ok {
		var servers map[string]json.RawMessage
		if json.Unmarshal(raw, &servers) == nil && servers[ServerName] != nil {
			out[0] = entryOf(servers[ServerName])
		}
	}
	if raw, ok := doc.MCP[ServerName]; ok {
		out[1] = entryOf(raw)
	}
	return out, nil
}

// readOpenCode reads the meko server OpenCode uses: the last file's that has one, mcp.servers.meko
// over mcp.meko.
func readOpenCode(Target) (*server, []string, error) {
	dir, err := openCodeDir()
	if err != nil {
		return nil, nil, err
	}
	var s *server
	for _, name := range openCodeFiles {
		path := filepath.Join(dir, name)
		b, err := readConfigFile(path)
		if err != nil {
			return nil, nil, err
		}
		if len(b) == 0 {
			continue
		}
		found, err := openCodeMeko(b)
		if err != nil {
			return nil, nil, fmt.Errorf("can't read %s: %w", path, err)
		}
		for i := range openCodePaths {
			if found[i] != nil {
				s = found[i]
				break
			}
		}
	}
	return s, nil, nil
}

// connectOpenCode adds Meko to OpenCode's global config. opencode mcp add replaces a server of the
// same name, and writes the config in the shape its own version reads.
func connectOpenCode(ctx context.Context, cmd string, t Target) ([]string, error) {
	return nil, runCLI(ctx, "", cmd, "mcp", "add", ServerName, "--url", t.URL,
		"--header", "Authorization=Bearer "+t.Token, "--global")
}

// disconnectOpenCode takes every meko server that's one of Meko's own out of OpenCode's global
// config files, as text, so the rest of each file (its comments too) stays: opencode mcp has no
// remove.
func disconnectOpenCode(_ context.Context, _ string, t Target) ([]string, error) {
	dir, err := openCodeDir()
	if err != nil {
		return nil, err
	}
	for _, name := range openCodeFiles {
		path := filepath.Join(dir, name)
		b, err := readConfigFile(path)
		if err != nil {
			return nil, err
		}
		if len(b) == 0 {
			continue
		}
		found, err := openCodeMeko(b)
		if err != nil {
			return nil, fmt.Errorf("can't read %s: %w", path, err)
		}
		changed := false
		for i, at := range openCodePaths {
			if s := found[i]; s == nil || !t.ours(s.url) {
				continue
			}
			out, ok, err := removeMember(b, at)
			if err != nil {
				return nil, fmt.Errorf("can't read %s: %w", path, err)
			}
			b, changed = out, changed || ok
		}
		if changed {
			if err := writeConfigFile(path, b); err != nil {
				return nil, err
			}
		}
	}
	return nil, nil
}
