package agents

import (
	"os"
	"path/filepath"
)

// cursor is Cursor, the editor and its CLI (cursor-agent), which share the global ~/.cursor/mcp.json.
var cursor = mcpJSON{config: cursorConfig, title: "Cursor"}

// cursorApp is where the macOS app keeps the editor's cursor command, under Applications: it isn't on
// PATH until the user installs it from the app.
const cursorApp = "Cursor.app/Contents/Resources/app/bin"

func cursorConfig() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cursor", "mcp.json"), nil
}
