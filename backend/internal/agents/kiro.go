package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// kiroConfig is Kiro CLI's global settings/mcp.json, in its home (KIRO_HOME, else ~/.kiro, as
// AgentK reads it), which every agent of Kiro's reads.
func kiroConfig() (string, error) {
	home, err := homeDir("KIRO_HOME", ".kiro")
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "settings", "mcp.json"), nil
}

// readKiroFile reads mcp.json and its mcpServers; no file, or a JSON null, is an empty one.
func readKiroFile() (path string, doc, servers map[string]json.RawMessage, err error) {
	if path, err = kiroConfig(); err != nil {
		return "", nil, nil, err
	}
	b, err := readConfigFile(path)
	if err != nil {
		return path, nil, nil, err
	}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return path, nil, nil, fmt.Errorf("can't read %s: %w", path, err)
		}
	}
	if doc == nil {
		doc = map[string]json.RawMessage{}
	}
	if raw := doc["mcpServers"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return path, nil, nil, fmt.Errorf("can't read %s's mcpServers: %w", path, err)
		}
	}
	if servers == nil {
		servers = map[string]json.RawMessage{}
	}
	return path, doc, servers, nil
}

func readKiro(Target) (*server, []string, error) {
	_, _, servers, err := readKiroFile()
	if err != nil {
		return nil, nil, err
	}
	return entryOf(servers[ServerName]), nil, nil
}

// connectKiro writes Meko into Kiro CLI's mcp.json, as text: kiro-cli mcp add takes no headers (and
// needs a login). An old meko entry keeps its other fields (autoApprove, timeout), with its url and
// Authorization header set and disabled dropped; the file's other servers and keys stay as they
// were. The result is read back before it's written.
func connectKiro(_ context.Context, _ string, t Target) ([]string, error) {
	path, err := kiroConfig()
	if err != nil {
		return nil, err
	}
	b, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}
	var old, oldHeaders []byte
	clean := blankJSONC(b)
	if sp, lerr := locate(clean, []string{"mcpServers"}, ServerName); lerr == nil && sp.member {
		old = clean[sp.val:sp.end]
	}
	var drop []string
	for _, m := range members(old) {
		if string(m[0]) == `"headers"` {
			oldHeaders = m[1]
		}
	}
	for _, m := range members(oldHeaders) {
		var k string
		_ = json.Unmarshal(m[0], &k)
		if strings.EqualFold(k, "Authorization") && k != "Authorization" {
			drop = append(drop, k)
		}
	}
	headers := withMembers(oldHeaders, [][2]string{{"Authorization", jsonString("Bearer " + t.Token)}}, drop...)
	entry := withMembers(old, [][2]string{{"url", jsonString(t.URL)}, {"headers", string(headers)}}, "disabled")
	out, err := setMember(b, []string{"mcpServers"}, ServerName, entry)
	if err != nil {
		return nil, fmt.Errorf("can't read %s: %w", path, err)
	}
	var after struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(blankJSONC(out), &after); err != nil || t.state(entryOf(after.MCPServers[ServerName])) !=
		StateConnected {
		return nil, fmt.Errorf("%s has a meko server Meko can't replace: remove it and connect Kiro CLI again", path)
	}
	return nil, writeConfigFile(path, out)
}

// disconnectKiro takes mcpServers.meko out of mcp.json as text, so the rest of the file stays as it
// was.
func disconnectKiro(context.Context, string, Target) ([]string, error) {
	path, err := kiroConfig()
	if err != nil {
		return nil, err
	}
	b, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}
	out, ok, err := removeMember(b, []string{"mcpServers"})
	if err != nil {
		return nil, fmt.Errorf("can't read %s: %w", path, err)
	}
	if !ok {
		return nil, nil
	}
	return nil, writeConfigFile(path, out)
}
