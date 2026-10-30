package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/yugabyte/meko/backend/internal/agents"
	"github.com/yugabyte/meko/backend/internal/cloudlink"
	"github.com/yugabyte/meko/backend/internal/paths"
)

// meko agent list|connect|disconnect: the agents on this computer and whether each reaches Meko,
// through internal/agents, in this process whether or not a server runs, holding the same lock as
// the server's routes. Agents connect to the linked Meko Cloud while one is linked, else to this
// Meko.

// groupAgent is the command whose subcommands work on agents: meko agent list|connect|disconnect.
const groupAgent = "agent"

// agentsLock is the lock file connect, disconnect and a link change's move hold.
func agentsLock(l paths.Layout) string { return filepath.Join(l.RunDir, "agents.lock") }

// agentTarget is where agents connect with the link in st: the linked Meko Cloud, else local.
func agentTarget(st cloudlink.State, local agents.Target) agents.Target {
	if st.Linked {
		return agents.Target{Kind: agents.KindCloud, URL: st.Link.MCPURL(), Token: st.Link.Key}
	}
	return local
}

// localTarget is this Meko's MCP URL and client token.
func localTarget(a mcpAccess) agents.Target {
	return agents.Target{Kind: agents.KindLocal, URL: a.url, Token: a.token}
}

// agentManager is the CLI's agents.Manager, its target read from the client token and the link
// each time it's asked (under the agents' lock, for a connect), and the target now.
func agentManager(l paths.Layout) (*agents.Manager, agents.Target, mcpAccess, error) {
	cfg, a, err := readMCPAccess(l)
	if err != nil {
		return nil, agents.Target{}, a, err
	}
	store, err := cloudStore(l, cfg)
	if err != nil {
		return nil, agents.Target{}, a, err
	}
	target := func() (agents.Target, error) {
		st, lerr := store.Load()
		return agentTarget(st, localTarget(a)), lerr
	}
	t, err := target()
	if err != nil {
		return nil, agents.Target{}, a, err
	}
	return &agents.Manager{Lock: agentsLock(l), Target: target}, t, a, nil
}

// runAgentList prints the installed agents and whether each reaches Meko; asJSON prints GET
// /api/v1/agents's data.
func runAgentList(l paths.Layout, asJSON bool, stdout, stderr io.Writer) int {
	m, _, _, err := agentManager(l)
	if err != nil {
		return fail(stderr, err)
	}
	list, err := m.List()
	if err != nil {
		return fail(stderr, err)
	}
	if asJSON {
		b, _ := json.Marshal(list)
		fmt.Fprintln(stdout, string(b))
		return exitOK
	}
	if len(list.Agents) == 0 {
		fmt.Fprintln(stdout, "No agents found on this computer: Meko works with "+catalogTitles())
		return exitOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "Agent\tMeko")
	var notes []string
	for _, r := range list.Agents {
		switch r.State {
		case agents.StateConnected:
			fmt.Fprintf(tw, "%s\tconnected\n", r.Title)
		case agents.StateStale:
			fmt.Fprintf(tw, "%s\tneeds reconnecting\tmeko agent connect %s\n", r.Title, r.Name)
		case agents.StateElsewhere:
			fmt.Fprintf(tw, "%s\telsewhere: %s\n", r.Title, *r.URL)
		default:
			fmt.Fprintf(tw, "%s\tnot connected\tmeko agent connect %s\n", r.Title, r.Name)
		}
		notes = append(notes, r.Notes...)
	}
	_ = tw.Flush()
	for _, n := range notes {
		fmt.Fprintln(stdout, n)
	}
	if list.Target.Kind == agents.KindCloud {
		fmt.Fprintf(stdout, "Agents connect to Meko Cloud (%s)\n", list.Target.URL)
	} else {
		fmt.Fprintf(stdout, "Agents connect to %s (Meko on this computer)\n", list.Target.URL)
	}
	return exitOK
}

// catalogTitles names every agent Meko knows: "Claude Code, Codex, Kiro CLI, OpenCode and Cursor".
func catalogTitles() string {
	t := make([]string, 0, len(agents.Catalog))
	for _, a := range agents.Catalog {
		t = append(t, a.Title)
	}
	return strings.Join(t[:len(t)-1], ", ") + " and " + t[len(t)-1]
}

// agentUsage says name isn't an agent Meko knows.
func agentUsage(cmd, name string, stderr io.Writer) int {
	if name == "" {
		fmt.Fprintf(stderr, "meko %s: name the agent (%s)\n", cmd, strings.Join(agents.Names(), ", "))
	} else {
		fmt.Fprintf(stderr, "meko %s: unknown agent %q (%s)\n", cmd, name, strings.Join(agents.Names(), ", "))
	}
	return exitUsage
}

// runAgentConnect connects the named agent to Meko's target; with none, it prints what any MCP
// client needs.
func runAgentConnect(l paths.Layout, name string, stdout, stderr io.Writer) int {
	if _, ok := agents.Lookup(name); name != "" && !ok {
		return agentUsage("agent connect", name, stderr)
	}
	m, t, a, err := agentManager(l)
	if err != nil {
		return fail(stderr, err)
	}
	if name == "" {
		printConnectHelp(t, a.running, stdout)
		return exitOK
	}
	r, err := m.Connect(context.Background(), name)
	if err != nil {
		var ae *agents.Error
		if errors.As(err, &ae) && ae.Code == agents.CodeFailed {
			fmt.Fprintf(stderr, "meko agent connect: %v. Run meko agent connect for the URL to add it by hand\n", err)
			return exitError
		}
		fmt.Fprintf(stderr, "meko agent connect: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "Connected %s to Meko\n", r.Agent.Title)
	for _, n := range r.Notes {
		fmt.Fprintln(stdout, n)
	}
	fmt.Fprintln(stdout, "Its new sessions have Meko's tools; ones already running need a restart")
	if t.Kind == agents.KindLocal && !a.running {
		fmt.Fprintln(stdout, "Meko isn't running: start it with meko start")
	}
	return exitOK
}

// runAgentDisconnect removes the named agent's meko server, when it's one of Meko's own.
func runAgentDisconnect(l paths.Layout, name string, stdout, stderr io.Writer) int {
	a, ok := agents.Lookup(name)
	if !ok {
		return agentUsage("agent disconnect", name, stderr)
	}
	m, _, _, err := agentManager(l)
	if err != nil {
		return fail(stderr, err)
	}
	r, err := m.Disconnect(context.Background(), name)
	if err != nil {
		fmt.Fprintf(stderr, "meko agent disconnect: %v\n", err)
		return exitError
	}
	switch {
	case r.Changed:
		fmt.Fprintf(stdout, "Disconnected %s from Meko\n", a.Title)
	case len(r.Notes) == 0:
		fmt.Fprintf(stdout, "%s isn't connected to Meko\n", a.Title)
	}
	for _, n := range r.Notes {
		fmt.Fprintln(stdout, n)
	}
	return exitOK
}

// printConnectHelp prints what any MCP client needs to reach the target, and the installed agents
// meko agent connect adds Meko to. While Meko Cloud is linked it names its URL, never its key.
func printConnectHelp(t agents.Target, running bool, w io.Writer) {
	fmt.Fprintln(w, "Add Meko to any MCP client as a Streamable HTTP server:")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintf(tw, "  Name\t%s (AgentK knows Meko's tools by it)\n", agents.ServerName)
	fmt.Fprintf(tw, "  URL\t%s\n", t.URL)
	if t.Kind == agents.KindCloud {
		fmt.Fprintf(tw, "  Header\tAuthorization: Bearer <key>\n")
		fmt.Fprintf(tw, "  Key\tyour Meko Cloud API key: the key is in Settings › Meko Cloud\n")
	} else {
		fmt.Fprintf(tw, "  Header\tAuthorization: Bearer %s\n", t.Token)
		fmt.Fprintf(tw, "  Token\t%s\n", t.Token)
	}
	_ = tw.Flush()
	var found []agents.Agent
	for _, a := range agents.Catalog {
		if _, ok := a.Find(); ok {
			found = append(found, a)
		}
	}
	if len(found) > 0 {
		fmt.Fprintln(w, "\nOr let Meko add itself to your agent, for all your projects:")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, a := range found {
			fmt.Fprintf(tw, "  meko agent connect %s\t%s\n", a.Name, a.Title)
		}
		_ = tw.Flush()
	}
	if t.Kind == agents.KindLocal && !running {
		fmt.Fprintln(w, "\nMeko isn't running: start it with meko start")
	}
}

// moveLines are meko cloud_link's lines on what a link change did to the agents: who moved, and
// who couldn't be.
func moveLines(m *agents.Moves, linked bool) []string {
	if m == nil {
		return nil
	}
	var lines []string
	if len(m.Moved) > 0 {
		var names []string
		for _, a := range m.Moved {
			names = append(names, a.Title)
		}
		to := "Meko Cloud"
		if !linked {
			to = "Meko on this computer"
		}
		lines = append(lines, fmt.Sprintf("Moved %s to %s; their sessions already running need a restart",
			joinAnd(names), to))
	}
	for _, f := range m.Failed {
		lines = append(lines, fmt.Sprintf("Couldn't move %s: %s; run meko agent connect %s", f.Title,
			strings.TrimSuffix(f.Error, "."), f.Name))
	}
	return lines
}

// joinAnd joins names as a sentence does: "a", "a and b", "a, b and c".
func joinAnd(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// cloudMover is the move of a link change the CLI makes itself, with the server stopped: from the
// link before to the link after, this Meko's side being local.
func cloudMover(l paths.Layout, local agents.Target) func(context.Context, cloudlink.State,
	cloudlink.State) agents.Moves {
	m := &agents.Manager{Lock: agentsLock(l)}
	return func(ctx context.Context, before, after cloudlink.State) agents.Moves {
		return m.Move(ctx, agentTarget(before, local), agentTarget(after, local))
	}
}
