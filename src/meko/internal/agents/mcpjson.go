package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// mcpJSON is an agent whose MCP servers are the mcpServers of a JSON file (Kiro CLI's, Cursor's),
// which Meko edits as text: neither CLI's mcp add takes headers. title names the agent in errors.
type mcpJSON struct {
	config func() (string, error)
	title  string
}

// readFile reads the file and its mcpServers; no file, or a JSON null, is an empty one.
func (j mcpJSON) readFile() (path string, doc, servers map[string]json.RawMessage, err error) {
	if path, err = j.config(); err != nil {
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

func (j mcpJSON) read(Target) (*server, []string, error) {
	_, _, servers, err := j.readFile()
	if err != nil {
		return nil, nil, err
	}
	return entryOf(servers[ServerName]), nil, nil
}

// connect writes Meko into the file, as text. An old meko entry keeps its other fields (autoApprove,
// timeout), with its url and Authorization header set and disabled dropped; the file's other servers
// and keys stay as they were. The result is read back before it's written.
func (j mcpJSON) connect(_ context.Context, _ string, t Target) ([]string, error) {
	path, err := j.config()
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
		return nil, fmt.Errorf("%s has a meko server Meko can't replace: remove it and connect %s again", path, j.title)
	}
	return nil, writeConfigFile(path, out)
}

// disconnect takes mcpServers.meko out of the file as text, so the rest of it stays as it was.
func (j mcpJSON) disconnect(context.Context, string, Target) ([]string, error) {
	path, err := j.config()
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
