package cli

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/agents"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/paths"
)

// The commands whose output ends with what to run next. Each list leaves out what's already done
// on this computer (nextState), so a new user sees only the steps still ahead.
type nextFor int

const (
	nextInstall nextFor = iota // meko install
	nextStart                  // meko start
	nextUse                    // meko --help (while Meko runs), meko status and meko cloud-link
	nextConnect                // meko agent connect <agent>
)

// nextState is what's already set up, which nextSteps leaves out. A fact that couldn't be read
// counts as done, so a row is never offered for something that may already be in place.
type nextState struct {
	running    bool         // Meko's server runs (meko agent connect works without it)
	webOff     bool         // the web UI is turned off
	agent      agents.Agent // the agent to connect, or the one just connected
	connected  bool         // an installed agent reaches this Meko
	claude     bool         // Claude Code is installed: memory extraction runs through it
	extracting bool         // memory extraction is on
	linked     bool         // Meko Cloud is linked
	kode       bool         // Kode is installed for this Meko
	kodeRead   bool         // the extensions' registry could be read, so kode says which
	// kodeOther is this Meko's data folder when it isn't the one meko kode uses by default
	// (MEKO_HOME, or ~/.meko), so meko kode is given it: "" for the default.
	kodeOther string
	added     string // the extension install's picker just added, offered first
}

// nextSteps is the commands to run next, each with what it does, for the command that ends with it.
// The first is the one to run now (report.next shows it as the call to action): opening the
// extension install's picker just added (after meko ui start while the web UI is off, since its page
// needs it), or the web UI. Then it offers Meko's uses in order: in an
// agent, learning from conversations, sharing through Meko Cloud, and Kode; meko status comes
// last, and after install names meko uninstall too.
func nextSteps(s nextState, kind nextFor) [][2]string {
	var out [][2]string
	add := func(cmd, what string) { out = append(out, [2]string{cmd, what}) }
	switch {
	case s.added != "" && s.webOff: // its page needs the web UI
		add("meko ui start", "turn the web UI on, when you want it")
		add(openExtension(s.added, s.kodeOther))
	case s.added != "":
		add(openExtension(s.added, s.kodeOther))
	case kind == nextConnect:
		if !s.running {
			add("meko start", "start Meko")
		}
		add(s.agent.Command, "start a new "+s.agent.Title+" session, with Meko's tools (ones already running need a restart)")
	case kind == nextUse && !s.running: // meko cloud-link with Meko stopped
		add("meko start", "start Meko")
	case s.webOff:
		add("meko ui start", "turn the web UI on, when you want it")
	default:
		add("meko ui", "open the web UI")
	}
	if kind != nextConnect && !s.connected {
		add("meko agent connect "+s.agent.Name, "use Meko in "+s.agent.Title+" (or "+otherAgents(s.agent.Name)+")")
	}
	if !s.extracting && s.claude {
		add("meko memory-extractor use claude", "learn memories from your conversations, through Claude Code")
	}
	if !s.linked {
		add("meko cloud-link", "share with your team through Meko Cloud")
	}
	switch {
	case !s.kodeRead, s.added == "kode": // just added: opening it came first
	case s.kode:
		add(openExtension("kode", s.kodeOther))
	default:
		add("meko extension install kode", "add Kode, which runs coding agents from task to merged PR")
	}
	switch kind {
	case nextInstall:
		add("meko status", "check on Meko at any time · meko uninstall removes it")
	case nextStart:
		add("meko status", "check on Meko at any time")
	}
	return out
}

// openExtension is the command that opens extension name's own page, and what it does: Kode's
// meko kode ui, given this Meko's data folder when other names one; the others' from Meko's web UI,
// whose sidebar lists them.
func openExtension(name, other string) (string, string) {
	title := extensionTitle(name)
	switch {
	case name == "kode" && other != "":
		return "meko kode --data-dir " + shellQuote([]string{other}) + " ui", "open " + title
	case name == "kode":
		return "meko kode ui", "open " + title
	}
	return "meko ui", "open the web UI, then " + title + " in its sidebar"
}

// otherAgents names the agents other than name, for "(or codex, kiro, opencode, cursor)".
func otherAgents(name string) string {
	var names []string
	for _, n := range agents.Names() {
		if n != name {
			names = append(names, n)
		}
	}
	return strings.Join(names, ", ")
}

// nextWait bounds the one call readNextState makes to the server: help must stay quick.
const nextWait = 2 * time.Second

// readNextState reads what's set up on this computer for nextSteps. c is the running server's
// client, or nil when none runs; memory extraction's state is read from it. Whatever it can't read
// (the agents, memory extraction, the Meko Cloud link, the extensions' registry) counts as set up,
// so its step isn't offered.
func readNextState(l paths.Layout, c *client) nextState {
	s := nextState{running: c != nil, connected: true, extracting: true, linked: true}
	s.agent, _ = agents.Lookup("claude")
	if reg, err := extensions.Load(l.Root); err == nil {
		_, s.kode = reg.Entries["kode"]
		s.kodeRead = true
		if s.kode && !isDefaultDataDir(l.Root) {
			s.kodeOther = l.Root
		}
	}
	if m, _, _, err := agentManager(l); err == nil {
		if list, err := m.List(); err == nil {
			s.connected = false
			for _, r := range list.Agents {
				if r.State == agents.StateConnected {
					s.connected = true
				}
				if r.Name == "claude" {
					s.claude = true
				}
			}
			if !s.claude && len(list.Agents) > 0 {
				s.agent, _ = agents.Lookup(list.Agents[0].Name)
			}
		}
	}
	if c != nil {
		ctx, cancel := context.WithTimeout(context.Background(), nextWait)
		defer cancel()
		var st extractorStatus
		if err := c.get(ctx, "/api/v1/settings/memory-extractor", &st); err == nil {
			s.extracting = st.Enabled
		}
	}
	if cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{}); err == nil {
		s.webOff = !cfg.Web.Enabled
		if store, err := cloudStore(l, cfg); err == nil {
			if st, err := store.Load(); err == nil {
				s.linked = st.Linked
			}
		}
	}
	return s
}

// isDefaultDataDir reports whether root is the data folder Meko's commands use without --data-dir.
func isDefaultDataDir(root string) bool {
	d, err := paths.Resolve("")
	return err == nil && paths.SameDir(d, root)
}
