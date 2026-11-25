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
	fresh := nextState{running: true, agent: claude, claude: true, kodeRead: true}
	done := nextState{running: true, agent: claude, connected: true, claude: true, extracting: true, linked: true,
		kode: true, kodeRead: true}
	ui := [2]string{"meko ui", "open the web UI"}
	connect := [2]string{"meko agent connect claude", "use Meko in Claude Code (or codex, kiro, opencode, cursor)"}
	learn := [2]string{"meko memory-extractor use claude", "learn memories from your conversations, through Claude Code"}
	share := [2]string{"meko cloud-link", "share with your team through Meko Cloud"}
	addAgentK := [2]string{"meko extension install kode", "add Kode, which runs coding agents from task to merged PR"}
	openAgentK := [2]string{"meko kode ui", "open Kode"}
	openPresent := [2]string{"meko ui", "open the web UI, then Present in its sidebar"}
	status := [2]string{"meko status", "check on Meko at any time"}
	statusInstall := [2]string{"meko status", "check on Meko at any time · meko uninstall removes it"}
	for _, c := range []struct {
		name string
		s    nextState
		kind nextFor
		want [][2]string
	}{
		{"install, nothing set up", fresh, nextInstall, [][2]string{ui, connect, learn, share, addAgentK, statusInstall}},
		{"install, everything set up", done, nextInstall, [][2]string{ui, openAgentK, statusInstall}},
		{"install, Kode just added in the picker: it comes first", func() nextState {
			s := fresh
			s.kode, s.added = true, "kode"
			return s
		}(), nextInstall, [][2]string{openAgentK, connect, learn, share, statusInstall}},
		{"install, Present just added", func() nextState {
			s := done
			s.added = "present"
			return s
		}(), nextInstall, [][2]string{openPresent, openAgentK, statusInstall}},
		{"install, Present just added, Kode not installed", func() nextState {
			s := fresh
			s.added = "present"
			return s
		}(), nextInstall, [][2]string{openPresent, connect, learn, share, addAgentK, statusInstall}},
		{"install --no-ui, Kode just added: the web UI it needs first", func() nextState {
			s := fresh
			s.webOff, s.kode, s.added = true, true, "kode"
			return s
		}(), nextInstall, [][2]string{{"meko ui start", "turn the web UI on, when you want it"}, openAgentK, connect,
			learn, share, statusInstall}},
		{"start", done, nextStart, [][2]string{ui, openAgentK, status}},
		{"help", fresh, nextUse, [][2]string{ui, connect, learn, share, addAgentK}},
		{"web UI off", nextState{webOff: true, agent: claude, connected: true, claude: true, extracting: true,
			linked: true, kodeRead: true}, nextInstall, [][2]string{
			{"meko ui start", "turn the web UI on, when you want it"}, addAgentK, statusInstall}},
		{"no Claude Code: connect another agent, and no extraction", nextState{running: true, agent: kiro,
			linked: true, kode: true, kodeRead: true}, nextUse, [][2]string{ui,
			{"meko agent connect kiro", "use Meko in Kiro CLI (or claude, codex, opencode, cursor)"}, openAgentK}},
		{"connect, Meko stopped", nextState{agent: kiro, claude: true, extracting: true, kodeRead: true}, nextConnect,
			[][2]string{{"meko start", "start Meko"},
				{"kiro-cli", "start a new Kiro CLI session, with Meko's tools (ones already running need a restart)"},
				share, addAgentK}},
		{"connect, the rest set up", done, nextConnect, [][2]string{
			{"claude", "start a new Claude Code session, with Meko's tools (ones already running need a restart)"},
			openAgentK}},
		{"cloud-link with Meko stopped: start it first", nextState{agent: claude, connected: true, claude: true,
			extracting: true, linked: true, kode: true, kodeRead: true}, nextUse, [][2]string{
			{"meko start", "start Meko"}, openAgentK}},
		{"another data folder than meko kode's", nextState{running: true, agent: claude, connected: true, claude: true,
			extracting: true, linked: true, kode: true, kodeRead: true, kodeOther: "/data/meko two"}, nextUse,
			[][2]string{ui, {"meko kode --data-dir '/data/meko two' ui", "open Kode"}}},
		{"the extensions' registry unread: no Kode step", nextState{running: true, agent: claude, connected: true,
			extracting: true, linked: true}, nextUse, [][2]string{ui}},
	} {
		if got := nextSteps(c.s, c.kind); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// An extension's status headline ("✓ Kode 0.2 is running …") gives its row its mark and its state,
// without the mark and the title; any other first line is added to running or not running, and the
// exit code's mark stays.
func TestExtensionState(t *testing.T) {
	for _, c := range []struct {
		k                   stepKind
		state, line, format string
		wantK               stepKind
		want                string
	}{
		{stepOK, "running", "✓ Kode 0.2 is running (pid 7, up 2h)", ", %s", stepOK, "0.2 is running (pid 7, up 2h)"},
		{stepFail, "not running", "✗ Kode 0.2 isn't running", ", %s", stepFail, "0.2 isn't running"},
		// A server that holds its lease but doesn't answer exits 0: its row is a warning, not a ✓.
		{stepOK, "running", "! Kode 0.2 is running (pid 7), but isn't answering", " (%s)", stepWarn,
			"0.2 is running (pid 7), but isn't answering"},
		{stepOK, "running", "Kode 0.2", ", %s", stepOK, "running, Kode 0.2"},
		{stepOK, "running", "Kode 0.2", " (%s)", stepOK, "running (Kode 0.2)"},
		{stepFail, "not running", "", ", %s", stepFail, "not running"},
	} {
		if k, got := extensionState("kode", c.k, c.state, c.line, c.format); k != c.wantK || got != c.want {
			t.Errorf("%q: got %v %q, want %v %q", c.line, k, got, c.wantK, c.want)
		}
	}
}

// An extension's row drops its title from the headline, and its old title, which a release from
// before the rename still prints (Present's Presenter, Kode's AgentK).
func TestExtensionStateOldTitle(t *testing.T) {
	for name, title := range map[string]string{"present": "Presenter", "kode": "AgentK"} {
		for _, line := range []string{"✓ " + extensionTitle(name) + " 0.2 is running (pid 7, up 2h)",
			"✓ " + title + " 0.2 is running (pid 7, up 2h)"} {
			k, got := extensionState(name, stepOK, "running", line, ", %s")
			if k != stepOK || got != "0.2 is running (pid 7, up 2h)" {
				t.Errorf("%q: got %v %q", line, k, got)
			}
		}
	}
}

// An interval is said in words, in the one unit that says it.
func TestEveryText(t *testing.T) {
	for secs, want := range map[int]string{300: "5 minutes", 60: "1 minute", 5400: "90 minutes", 3600: "1 hour",
		7200: "2 hours", 30: "30 seconds", 1: "1 second", 90: "1m30s"} {
		if got := everyText(secs); got != want {
			t.Errorf("everyText(%d) = %q, want %q", secs, got, want)
		}
	}
}

// meko kode runs the Kode of the data folder MEKO_HOME names, else ~/.meko's.
func TestIsDefaultDataDir(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	t.Setenv("MEKO_HOME", root)
	if !isDefaultDataDir(root) {
		t.Error("MEKO_HOME's data folder isn't the default")
	}
	if isDefaultDataDir(other) {
		t.Error("another data folder is the default")
	}
}
