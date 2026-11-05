package agents

import "path/filepath"

// kiro is Kiro CLI: kiro-cli mcp add takes no headers (and needs a login), so its file is edited.
var kiro = mcpJSON{config: kiroConfig, title: "Kiro CLI"}

// kiroConfig is Kiro CLI's global settings/mcp.json, in its home (KIRO_HOME, else ~/.kiro, as
// AgentK reads it), which every agent of Kiro's reads.
func kiroConfig() (string, error) {
	home, err := homeDir("KIRO_HOME", ".kiro")
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "settings", "mcp.json"), nil
}
