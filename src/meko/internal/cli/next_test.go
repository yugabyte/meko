package cli

import (
	"reflect"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/agents"
)

// The "Next" lists offer only what isn't set up yet, in the same order everywhere, with meko status
// last where it's offered.
func TestNextSteps(t *testing.T) {
	claude, _ := agents.Lookup("claude")
	kiro, _ := agents.Lookup("kiro")
	fresh := nextState{running: true, agent: claude, claude: true, agentKRead: true}
	done := nextState{running: true, agent: claude, connected: true, claude: true, extracting: true, linked: true,
		agentK: true, agentKRead: true}
	ui := [2]string{"meko ui", "open the web UI"}
	connect := [2]string{"meko agent connect claude", "use Meko in Claude Code (or codex, kiro, opencode, cursor)"}
	learn := [2]string{"meko memory-extractor use claude", "learn memories from your conversations, through Claude Code"}
	share := [2]string{"meko cloud_link", "share with your team through Meko Cloud"}
	addAgentK := [2]string{"meko extension install agentk", "add AgentK, the agentic coder for ADLC"}
	openAgentK := [2]string{"meko ui /ext/agentk/", "open AgentK, the agentic coder for ADLC"}
	cli := [2]string{"meko-cli", "search and add to what Meko keeps"}
	status := [2]string{"meko status", "check on Meko at any time"}
	for _, c := range []struct {
		name string
		s    nextState
		kind nextFor
		want [][2]string
	}{
		{"install, nothing set up", fresh, nextInstall, [][2]string{ui, connect, learn, share, addAgentK, status}},
		{"install, everything set up", done, nextInstall, [][2]string{ui, openAgentK, status}},
		{"start", done, nextStart, [][2]string{ui, openAgentK, cli, status}},
		{"help", fresh, nextHelp, [][2]string{ui, connect, learn, share, addAgentK}},
		{"web UI off", nextState{webOff: true, agent: claude, connected: true, claude: true, extracting: true,
			linked: true, agentKRead: true}, nextInstall, [][2]string{
			{"meko ui start", "turn the web UI on, when you want it"}, addAgentK, status}},
		{"no Claude Code: connect another agent, and no extraction", nextState{agent: kiro, extracting: false,
			linked: true, agentK: true, agentKRead: true}, nextHelp, [][2]string{ui,
			{"meko agent connect kiro", "use Meko in Kiro CLI (or claude, codex, opencode, cursor)"}, openAgentK}},
		{"connect, Meko stopped", nextState{agent: kiro, claude: true, extracting: true, agentKRead: true}, nextConnect,
			[][2]string{{"meko start", "start Meko"},
				{"kiro-cli", "start a new Kiro CLI session, with Meko's tools (ones already running need a restart)"},
				share, addAgentK}},
		{"connect, the rest set up", done, nextConnect, [][2]string{
			{"claude", "start a new Claude Code session, with Meko's tools (ones already running need a restart)"},
			openAgentK}},
		{"the extensions' registry unread: no AgentK step", nextState{running: true, agent: claude, connected: true,
			extracting: true, linked: true}, nextHelp, [][2]string{ui}},
	} {
		if got := nextSteps(c.s, c.kind); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// An extension's status headline ("✓ AgentK 0.2 is running …") gives its row its mark and its state,
// without the mark and the title; any other first line is added to running or not running, and the
// exit code's mark stays.
func TestExtensionState(t *testing.T) {
	for _, c := range []struct {
		k                   stepKind
		state, line, format string
		wantK               stepKind
		want                string
	}{
		{stepOK, "running", "✓ AgentK 0.2 is running (pid 7, up 2h)", ", %s", stepOK, "0.2 is running (pid 7, up 2h)"},
		{stepFail, "not running", "✗ AgentK 0.2 isn't running", ", %s", stepFail, "0.2 isn't running"},
		// A server that holds its lease but doesn't answer exits 0: its row is a warning, not a ✓.
		{stepOK, "running", "! AgentK 0.2 is running (pid 7), but isn't answering", " (%s)", stepWarn,
			"0.2 is running (pid 7), but isn't answering"},
		{stepOK, "running", "AgentK 0.2", ", %s", stepOK, "running, AgentK 0.2"},
		{stepOK, "running", "AgentK 0.2", " (%s)", stepOK, "running (AgentK 0.2)"},
		{stepFail, "not running", "", ", %s", stepFail, "not running"},
	} {
		if k, got := extensionState("AgentK", c.k, c.state, c.line, c.format); k != c.wantK || got != c.want {
			t.Errorf("%q: got %v %q, want %v %q", c.line, k, got, c.wantK, c.want)
		}
	}
}
