package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/yugabyte/meko/backend/internal/mcp"
)

// runList lists what Meko offers. mcp-tools is the only list so far: the tools /mcp serves, read
// from their definitions, so it needs no running server.
func runList(what string, asJSON bool, stdout, stderr io.Writer) int {
	if what != "mcp-tools" {
		fmt.Fprintf(stderr, "meko list: say what to list: mcp-tools\n")
		return exitUsage
	}
	tools := mcp.Tools()
	if asJSON {
		type row struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			Summary string `json:"summary"`
		}
		out := make([]row, len(tools))
		for i, t := range tools {
			out[i] = row{t.Name, t.Title, t.Summary}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, t := range tools {
		fmt.Fprintf(w, "%s\t%s\n", t.Name, t.Summary)
	}
	if err := w.Flush(); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}
